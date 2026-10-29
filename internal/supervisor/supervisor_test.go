package supervisor_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/gke-demos/ax-bug-bash/internal/supervisor"
)

func TestHotReloadSupervisorAtomicSwapMetricsContinuityAndBrokenBuildFallback(t *testing.T) {
	// 0. Reject unknown / unallowlisted service names
	if _, err := supervisor.New(supervisor.Config{
		Service:    "unknown-service;whoami",
		GitRepoURL: "http://127.0.0.1:1/git/storefront.git",
		WorkDir:    t.TempDir(),
	}); err == nil {
		t.Fatal("expected supervisor.New to reject unallowlisted service name")
	}

	// Loopback Loki push collector
	var (
		lokiMu       sync.Mutex
		lokiPayloads []string
	)
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/loki/api/v1/push" {
			b, _ := io.ReadAll(r.Body)
			lokiMu.Lock()
			lokiPayloads = append(lokiPayloads, string(b))
			lokiMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer lokiSrv.Close()

	// Canonical storefront-git server on 127.0.0.1
	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewGitServer failed: %v", err)
	}
	defer func() { _ = gitSrv.Close() }()
	gitHTTPSrv := httptest.NewServer(gitSrv.Handler())
	defer gitHTTPSrv.Close()

	baselineSHA := gitSrv.GitManager().BaselineCommitSHA()

	// Boot Supervisor for shipping-service (Go)
	sup, err := supervisor.New(supervisor.Config{
		Service:     storefront.ServiceShipping,
		GitRepoURL:  gitHTTPSrv.URL + "/git/storefront.git",
		WorkDir:     t.TempDir(),
		LokiPushURL: lokiSrv.URL + "/loki/api/v1/push",
	})
	if err != nil {
		t.Fatalf("supervisor.New(shipping-service) failed: %v", err)
	}
	defer func() { _ = sup.Close() }()

	supHTTPSrv := httptest.NewServer(sup.Handler())
	defer supHTTPSrv.Close()

	readMetrics := func() string {
		t.Helper()
		res, mErr := http.Get(supHTTPSrv.URL + "/metrics")
		if mErr != nil {
			t.Fatalf("GET /metrics failed: %v", mErr)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}

	// 1. Initial metrics series initialized at 0 + commit_sha + security headers + method allowlist
	m0 := readMetrics()
	for _, needle := range []string{
		`storefront_http_requests_total{status="200",service="shipping-service"} 0`,
		`storefront_http_requests_total{status="500",service="shipping-service"} 0`,
		`storefront_service_info{service="shipping-service",language="Go",commit_sha="` + baselineSHA + `"} 1`,
	} {
		if !strings.Contains(m0, needle) {
			t.Fatalf("initial /metrics missing %q:\n%s", needle, m0)
		}
	}

	delReq, _ := http.NewRequest(http.MethodDelete, supHTTPSrv.URL+"/api/shipping/quote", nil)
	delRes, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE request failed: %v", err)
	}
	_ = delRes.Body.Close()
	if delRes.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405", delRes.StatusCode)
	}
	if delRes.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options: nosniff header")
	}

	// 2. Happy-path shipping quote -> HTTP 200
	okRes, err := http.Post(
		supHTTPSrv.URL+"/api/shipping/quote",
		"application/json",
		bytes.NewReader([]byte(`{"subtotal_cents":4500,"weight_grams":450,"hazmat_class":"NONE"}`)),
	)
	if err != nil {
		t.Fatalf("happy-path POST /api/shipping/quote failed: %v", err)
	}
	var okBody struct {
		RateCents int `json:"rate_cents"`
	}
	_ = json.NewDecoder(okRes.Body).Decode(&okBody)
	_ = okRes.Body.Close()
	if okRes.StatusCode != http.StatusOK || okBody.RateCents != 799 {
		t.Fatalf("happy-path shipping = (%d, %d), want (200, 799)", okRes.StatusCode, okBody.RateCents)
	}

	// 3. INC-101 zero-subtotal shipping quote -> real HTTP 500 + Go stack trace + Loki push
	failReq, _ := http.NewRequest(
		http.MethodPost,
		supHTTPSrv.URL+"/api/shipping/quote",
		bytes.NewReader([]byte(`{"subtotal_cents":0,"weight_grams":120,"hazmat_class":"NONE"}`)),
	)
	failReq.Header.Set("Content-Type", "application/json")
	failReq.Header.Set("X-Storefront-Request-ID", "req-slice2-101")
	failRes, err := http.DefaultClient.Do(failReq)
	if err != nil {
		t.Fatalf("INC-101 POST /api/shipping/quote failed: %v", err)
	}
	var failBody struct {
		ErrorType  string `json:"error_type"`
		Message    string `json:"message"`
		StackTrace string `json:"stack_trace"`
		RequestID  string `json:"request_id"`
	}
	_ = json.NewDecoder(failRes.Body).Decode(&failBody)
	_ = failRes.Body.Close()
	if failRes.StatusCode != http.StatusInternalServerError {
		t.Fatalf("INC-101 shipping status = %d, want 500", failRes.StatusCode)
	}
	if !strings.Contains(failBody.ErrorType, "CarrierTariffBoundsError") || !strings.Contains(failBody.StackTrace, "goroutine") {
		t.Fatalf("expected CarrierTariffBoundsError and real Go goroutine stack trace, got type=%q trace=%q", failBody.ErrorType, failBody.StackTrace)
	}
	if failBody.RequestID != "req-slice2-101" {
		t.Fatalf("request_id = %q, want req-slice2-101", failBody.RequestID)
	}

	// Wait briefly for async Loki push
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lokiMu.Lock()
		n := len(lokiPayloads)
		lokiMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	lokiMu.Lock()
	joinedLoki := strings.Join(lokiPayloads, "\n")
	lokiMu.Unlock()
	if !strings.Contains(joinedLoki, "CarrierTariffBoundsError") || !strings.Contains(joinedLoki, "req-slice2-101") {
		t.Fatalf("expected async Loki push with CarrierTariffBoundsError and req-slice2-101, got: %s", joinedLoki)
	}

	mBeforeReload := readMetrics()
	if !strings.Contains(mBeforeReload, `storefront_http_requests_total{status="200",service="shipping-service"} 1`) ||
		!strings.Contains(mBeforeReload, `storefront_http_requests_total{status="500",service="shipping-service"} 1`) {
		t.Fatalf("unexpected metrics before reload:\n%s", mBeforeReload)
	}

	// 4. Push INC-101 shipping-service fix to storefront-git and trigger POST /internal/reload
	cloneDir := filepath.Join(t.TempDir(), "clone")
	if out, cErr := exec.Command("git", "clone", gitHTTPSrv.URL+"/git/storefront.git", cloneDir).CombinedOutput(); cErr != nil {
		t.Fatalf("git clone failed: %v\n%s", cErr, string(out))
	}
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cloneDir
		if out, gErr := cmd.CombinedOutput(); gErr != nil {
			t.Fatalf("git %v failed: %v\n%s", args, gErr, string(out))
		}
	}
	gitRun("config", "user.email", "fixer@ax-bug-bash.local")
	gitRun("config", "user.name", "AX Fixer")
	gitRun("config", "commit.gpgsign", "false")

	sc, _ := storefront.GetScenario("INC-101")
	shipFix := sc.ServiceTraces[storefront.ServiceShipping].PatchedSourceFileContent
	if err := os.WriteFile(filepath.Join(cloneDir, "services", "shipping-service", "shipping.go"), []byte(shipFix), 0o644); err != nil {
		t.Fatalf("writing shipping.go fix: %v", err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "fix(INC-101): allow zero-subtotal shipping quote")
	gitRun("push", "origin", "HEAD:refs/heads/main")
	fixSHA := gitSrv.GitManager().CurrentCommitSHA("refs/heads/main")

	reloadRes, err := http.Post(
		supHTTPSrv.URL+"/internal/reload",
		"application/json",
		bytes.NewReader([]byte(`{"service":"shipping-service","commit_sha":"`+fixSHA+`"}`)),
	)
	if err != nil {
		t.Fatalf("POST /internal/reload failed: %v", err)
	}
	reloadBody, _ := io.ReadAll(reloadRes.Body)
	_ = reloadRes.Body.Close()
	if reloadRes.StatusCode != http.StatusOK {
		t.Fatalf("POST /internal/reload status = %d, want 200: %s", reloadRes.StatusCode, string(reloadBody))
	}

	// Verify INC-101 zero-subtotal request now succeeds (HTTP 200) and /metrics counters were NOT reset!
	healedRes, err := http.Post(
		supHTTPSrv.URL+"/api/shipping/quote",
		"application/json",
		bytes.NewReader([]byte(`{"subtotal_cents":0,"weight_grams":120,"hazmat_class":"NONE"}`)),
	)
	if err != nil {
		t.Fatalf("post-reload POST /api/shipping/quote failed: %v", err)
	}
	_ = healedRes.Body.Close()
	if healedRes.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 after hot-reload, got %d", healedRes.StatusCode)
	}

	mAfterReload := readMetrics()
	for _, needle := range []string{
		`storefront_http_requests_total{status="200",service="shipping-service"} 2`,
		`storefront_http_requests_total{status="500",service="shipping-service"} 1`,
		`storefront_service_info{service="shipping-service",language="Go",commit_sha="` + fixSHA + `"} 1`,
		`storefront_hot_reload_duration_milliseconds{service="shipping-service"}`,
	} {
		if !strings.Contains(mAfterReload, needle) {
			t.Fatalf("post-reload /metrics missing %q:\n%s", needle, mAfterReload)
		}
	}

	// 5. Broken-build fallback protection: push invalid Go syntax and attempt reload; prior child stays live!
	if err := os.WriteFile(filepath.Join(cloneDir, "services", "shipping-service", "shipping.go"), []byte("package shipping\nfunc QuoteCarrierRate( BROKEN SYNTAX"), 0o644); err != nil {
		t.Fatalf("writing broken shipping.go: %v", err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "chore: broken build syntax")
	gitRun("push", "origin", "HEAD:refs/heads/fix/broken-build")
	brokenSHAOut, _ := exec.Command("git", "-C", cloneDir, "rev-parse", "HEAD").CombinedOutput()
	brokenSHA := strings.TrimSpace(string(brokenSHAOut))

	badReloadRes, err := http.Post(
		supHTTPSrv.URL+"/internal/reload",
		"application/json",
		bytes.NewReader([]byte(`{"service":"shipping-service","commit_sha":"`+brokenSHA+`"}`)),
	)
	if err != nil {
		t.Fatalf("broken reload HTTP call failed: %v", err)
	}
	_ = badReloadRes.Body.Close()
	if badReloadRes.StatusCode == http.StatusOK {
		t.Fatal("expected broken-build reload to fail with non-200 status")
	}

	stillLiveRes, err := http.Post(
		supHTTPSrv.URL+"/api/shipping/quote",
		"application/json",
		bytes.NewReader([]byte(`{"subtotal_cents":0,"weight_grams":120,"hazmat_class":"NONE"}`)),
	)
	if err != nil {
		t.Fatalf("shipping request after failed reload failed: %v", err)
	}
	_ = stillLiveRes.Body.Close()
	if stillLiveRes.StatusCode != http.StatusOK {
		t.Fatalf("expected prior healthy child to keep serving HTTP 200 after broken reload, got %d", stillLiveRes.StatusCode)
	}

	mAfterBadReload := readMetrics()
	if !strings.Contains(mAfterBadReload, `storefront_http_requests_total{status="200",service="shipping-service"} 3`) ||
		!strings.Contains(mAfterBadReload, `storefront_http_requests_total{status="500",service="shipping-service"} 1`) ||
		!strings.Contains(mAfterBadReload, `commit_sha="`+fixSHA+`"`) {
		t.Fatalf("broken reload corrupted metrics or commit_sha:\n%s", mAfterBadReload)
	}
}

func TestPolyglotBackendServicesStandaloneHTTP500StackTracesAndHotReload(t *testing.T) {
	var (
		lokiMu       sync.Mutex
		lokiPayloads []string
	)
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/loki/api/v1/push" {
			b, _ := io.ReadAll(r.Body)
			lokiMu.Lock()
			lokiPayloads = append(lokiPayloads, string(b))
			lokiMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer lokiSrv.Close()

	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewGitServer failed: %v", err)
	}
	defer func() { _ = gitSrv.Close() }()
	gitHTTPSrv := httptest.NewServer(gitSrv.Handler())
	defer gitHTTPSrv.Close()

	bootSvc := func(svc string) *httptest.Server {
		t.Helper()
		sup, sErr := supervisor.New(supervisor.Config{
			Service:     svc,
			GitRepoURL:  gitHTTPSrv.URL + "/git/storefront.git",
			WorkDir:     t.TempDir(),
			LokiPushURL: lokiSrv.URL + "/loki/api/v1/push",
		})
		if sErr != nil {
			t.Fatalf("supervisor.New(%s) failed: %v", svc, sErr)
		}
		t.Cleanup(func() { _ = sup.Close() })
		srv := httptest.NewServer(sup.Handler())
		t.Cleanup(srv.Close)
		return srv
	}

	checkoutSrv := bootSvc(storefront.ServiceCheckout)
	pricingSrv := bootSvc(storefront.ServicePricing)
	inventorySrv := bootSvc(storefront.ServiceInventory)

	postJSON := func(targetURL, reqID, payload string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader([]byte(payload)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Storefront-Request-ID", reqID)
		res, pErr := http.DefaultClient.Do(req)
		if pErr != nil {
			t.Fatalf("POST %s failed: %v", targetURL, pErr)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	// 1. Happy path across checkout-service (Go), pricing-service (Python), inventory-service (Rust)
	if code, _ := postJSON(checkoutSrv.URL+"/api/checkout/capture", "req-ok-chk", `{"available_stock":10,"requested_qty":2,"bonus_bundle_units":0,"amount_minor":5299,"currency":"USD"}`); code != http.StatusOK {
		t.Fatalf("checkout-service happy path status = %d, want 200", code)
	}
	if code, _ := postJSON(pricingSrv.URL+"/api/pricing/quote", "req-ok-prc", `{"subtotal_cents":4500,"promo_codes":["SAVE20"],"currency":"USD"}`); code != http.StatusOK {
		t.Fatalf("pricing-service happy path status = %d, want 200", code)
	}
	if code, _ := postJSON(inventorySrv.URL+"/api/inventory/reserve", "req-ok-inv", `{"available":10,"requested":2,"bonus_bundle_units":0,"weight_grams":450,"hazmat_class":"NONE"}`); code != http.StatusOK {
		t.Fatalf("inventory-service happy path status = %d, want 200", code)
	}

	// 2. Real domain HTTP 500s + language-native stack traces
	// checkout-service: INC-102 FlashBundleUnderflowError & INC-103 PaymentCapturePrecisionError
	chkCode, chkErr := postJSON(checkoutSrv.URL+"/api/checkout/capture", "req-500-chk-102", `{"available_stock":2,"requested_qty":2,"bonus_bundle_units":2,"amount_minor":58000,"currency":"USD"}`)
	if chkCode != http.StatusInternalServerError || !strings.Contains(chkErr["stack_trace"].(string), "FlashBundleUnderflowError") || !strings.Contains(chkErr["stack_trace"].(string), "goroutine") {
		t.Fatalf("checkout-service INC-102 expected 500 + Go trace, got %d: %v", chkCode, chkErr)
	}
	chkCode103, chkErr103 := postJSON(checkoutSrv.URL+"/api/checkout/capture", "req-500-chk-103", `{"available_stock":10,"requested_qty":1,"bonus_bundle_units":0,"amount_minor":4476.15,"currency":"JPY"}`)
	if chkCode103 != http.StatusInternalServerError || !strings.Contains(chkErr103["stack_trace"].(string), "PaymentCapturePrecisionError") {
		t.Fatalf("checkout-service INC-103 expected 500 + PaymentCapturePrecisionError, got %d: %v", chkCode103, chkErr103)
	}

	// pricing-service: INC-101 NegativeSubtotalError & INC-103 CurrencyPrecisionMismatchError with Python Traceback
	prcCode, prcErr := postJSON(pricingSrv.URL+"/api/pricing/quote", "req-500-prc-101", `{"subtotal_cents":1600,"promo_codes":["SAVE20","FLAT15"],"currency":"USD"}`)
	if prcCode != http.StatusInternalServerError || !strings.Contains(prcErr["stack_trace"].(string), "Traceback (most recent call last)") || !strings.Contains(prcErr["stack_trace"].(string), "apply_stacked_promos") {
		t.Fatalf("pricing-service INC-101 expected 500 + Python Traceback, got %d: %v", prcCode, prcErr)
	}
	prcCode103, prcErr103 := postJSON(pricingSrv.URL+"/api/pricing/quote", "req-500-prc-103", `{"subtotal_cents":2900,"promo_codes":[],"currency":"JPY"}`)
	if prcCode103 != http.StatusInternalServerError || !strings.Contains(prcErr103["stack_trace"].(string), "CurrencyPrecisionMismatchError") {
		t.Fatalf("pricing-service INC-103 expected 500 + CurrencyPrecisionMismatchError, got %d: %v", prcCode103, prcErr103)
	}

	// inventory-service: INC-102 RustU32UnderflowPanic & INC-104 UnhandledHazmatClassError
	invCode, invErr := postJSON(inventorySrv.URL+"/api/inventory/reserve", "req-500-inv-102", `{"available":2,"requested":2,"bonus_bundle_units":2,"weight_grams":1960,"hazmat_class":"NONE"}`)
	if invCode != http.StatusInternalServerError || !strings.Contains(invErr["stack_trace"].(string), "RustU32UnderflowPanic") || !strings.Contains(invErr["stack_trace"].(string), "reserve_bundle_stock") {
		t.Fatalf("inventory-service INC-102 expected 500 + RustU32UnderflowPanic trace, got %d: %v", invCode, invErr)
	}
	invCode104, invErr104 := postJSON(inventorySrv.URL+"/api/inventory/reserve", "req-500-inv-104", `{"available":18,"requested":1,"bonus_bundle_units":0,"weight_grams":3400,"hazmat_class":"UN3480-LITHIUM"}`)
	if invCode104 != http.StatusInternalServerError || !strings.Contains(invErr104["stack_trace"].(string), "UnhandledHazmatClassError") {
		t.Fatalf("inventory-service INC-104 expected 500 + UnhandledHazmatClassError trace, got %d: %v", invCode104, invErr104)
	}

	// Verify Loki pushes arrived from all 3 services
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lokiMu.Lock()
		joined := strings.Join(lokiPayloads, "\n")
		lokiMu.Unlock()
		if strings.Contains(joined, "req-500-chk-102") &&
			strings.Contains(joined, "req-500-prc-101") &&
			strings.Contains(joined, "req-500-inv-102") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	lokiMu.Lock()
	allLoki := strings.Join(lokiPayloads, "\n")
	lokiMu.Unlock()
	for _, reqID := range []string{"req-500-chk-102", "req-500-prc-101", "req-500-inv-102"} {
		if !strings.Contains(allLoki, reqID) {
			t.Fatalf("expected Loki push containing %q, got:\n%s", reqID, allLoki)
		}
	}
}

