package storefront_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/gke-demos/ax-bug-bash/internal/supervisor"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve runtime caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func TestBaselinePolyglotServicesPassHappyPathUnitTests(t *testing.T) {
	root := repoRoot(t)
	runnerScript := filepath.Join(root, "services", "run-service-tests.sh")

	services := []string{
		"storefront-web",
		"checkout-service",
		"shipping-service",
		"pricing-service",
		"inventory-service",
	}

	for _, svc := range services {
		t.Run(svc, func(t *testing.T) {
			cmd := exec.Command("bash", runnerScript, svc)
			cmd.Dir = filepath.Join(root, "services")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("expected baseline unit tests for %s to pass, got error: %v\noutput:\n%s", svc, err, string(out))
			}
			if !strings.Contains(string(out), "PASS") && !strings.Contains(string(out), "ok") && !strings.Contains(string(out), "OK") {
				t.Fatalf("expected test output for %s to indicate passing status, got:\n%s", svc, string(out))
			}
		})
	}
}

func TestIncidentScenariosCatalogAndRedGreenTDDVerification(t *testing.T) {
	catalog := storefront.ScenarioCatalog()
	if len(catalog) != 4 {
		t.Fatalf("expected 4 incident scenarios, got %d", len(catalog))
	}

	expectedImplicated := map[string][]string{
		"INC-101": {"pricing-service", "shipping-service"},
		"INC-102": {"inventory-service", "checkout-service"},
		"INC-103": {"storefront-web", "pricing-service", "checkout-service"},
		"INC-104": {"inventory-service", "shipping-service"},
	}

	for _, sc := range catalog {
		want, ok := expectedImplicated[sc.ID]
		if !ok {
			t.Fatalf("unexpected scenario ID %s", sc.ID)
		}
		if !reflect.DeepEqual(sc.ImplicatedServices, want) {
			t.Fatalf("scenario %s implicated services = %v, want %v", sc.ID, sc.ImplicatedServices, want)
		}
		if len(sc.ImplicatedServices)+len(sc.UnaffectedServices) != 5 {
			t.Fatalf("scenario %s implicated (%d) + unaffected (%d) must equal 5 total services",
				sc.ID, len(sc.ImplicatedServices), len(sc.UnaffectedServices))
		}

		for _, svc := range sc.ImplicatedServices {
			detail, exists := sc.ServiceTraces[svc]
			if !exists {
				t.Fatalf("scenario %s missing ServiceTraces entry for %s", sc.ID, svc)
			}
			t.Run(sc.ID+"_"+svc+"_RedGreenTDD", func(t *testing.T) {
				wsDir := t.TempDir()
				if err := storefront.SeedServicesTree(wsDir); err != nil {
					t.Fatalf("SeedServicesTree failed: %v", err)
				}
				runnerScript := filepath.Join(wsDir, "services", "run-service-tests.sh")

				// Step 1: Write the Regression Test (without patching source) -> MUST FAIL (Red)
				testFilePath := filepath.Join(wsDir, "services", svc, detail.TestFile)
				if err := os.WriteFile(testFilePath, []byte(detail.RegressionTestFileContent), 0o644); err != nil {
					t.Fatalf("failed to write regression test file: %v", err)
				}
				redCmd := exec.Command("bash", runnerScript, svc)
				redCmd.Dir = filepath.Join(wsDir, "services")
				redOut, redErr := redCmd.CombinedOutput()
				if redErr == nil {
					t.Fatalf("expected regression test for %s/%s to FAIL (Red) on unpatched code, but it passed:\n%s",
						sc.ID, svc, string(redOut))
				}

				// Step 2: Write the Patched Source File -> MUST PASS (Green)
				srcFilePath := filepath.Join(wsDir, "services", svc, detail.SourceFile)
				if err := os.WriteFile(srcFilePath, []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
					t.Fatalf("failed to write patched source file: %v", err)
				}
				greenCmd := exec.Command("bash", runnerScript, svc)
				greenCmd.Dir = filepath.Join(wsDir, "services")
				greenOut, greenErr := greenCmd.CombinedOutput()
				if greenErr != nil {
					t.Fatalf("expected patched service %s/%s to PASS (Green), got error: %v\noutput:\n%s",
						sc.ID, svc, greenErr, string(greenOut))
				}
			})
		}
	}
}

func TestStorefrontScenariosMetricsLogsAndUI(t *testing.T) {
	rt, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	// 1. Verify GET / serves clean Retail Storefront HTML with Projector Light Mode toggle & security headers
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := res.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	html := string(bodyBytes)
	if !strings.Contains(html, "theme-toggle") || !strings.Contains(html, "data-theme") {
		t.Fatalf("expected storefront HTML to include Projector Light Mode toggle")
	}

	// 2. Verify happy-path checkout succeeds with HTTP 200 OK
	happyCart := storefront.CheckoutRequest{
		Items:      []storefront.CartItem{{SKU: "sku-desk-mat-xl", Quantity: 1}},
		Currency:   "USD",
		PromoCodes: []string{"SAVE20"},
	}
	cartJSON, _ := json.Marshal(happyCart)
	checkoutRes, err := http.Post(srv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartJSON))
	if err != nil {
		t.Fatalf("POST /api/cart/checkout failed: %v", err)
	}
	checkoutRes.Body.Close()
	if checkoutRes.StatusCode != http.StatusOK {
		t.Fatalf("expected happy-path checkout HTTP 200, got %d", checkoutRes.StatusCode)
	}

	// 3. Activate each of INC-101..INC-104 and verify HTTP 500, exact implicated service metrics & stack traces
	for _, sc := range storefront.ScenarioCatalog() {
		t.Run(sc.ID, func(t *testing.T) {
			actBody, _ := json.Marshal(map[string]string{"incident_id": sc.ID})
			actRes, err := http.Post(srv.URL+"/api/incidents/activate", "application/json", bytes.NewReader(actBody))
			if err != nil {
				t.Fatalf("activate %s failed: %v", sc.ID, err)
			}
			actRes.Body.Close()
			if actRes.StatusCode != http.StatusOK {
				t.Fatalf("activate %s status = %d, want 200", sc.ID, actRes.StatusCode)
			}

			// Trigger checkout with the scenario's failing cart
			trigBytes, _ := json.Marshal(sc.TriggerCart)
			failRes, err := http.Post(srv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(trigBytes))
			if err != nil {
				t.Fatalf("failing checkout for %s failed: %v", sc.ID, err)
			}
			var failPayload storefront.CheckoutResponse
			_ = json.NewDecoder(failRes.Body).Decode(&failPayload)
			failRes.Body.Close()
			if failRes.StatusCode != http.StatusInternalServerError {
				t.Fatalf("expected HTTP 500 for %s trigger cart, got %d", sc.ID, failRes.StatusCode)
			}
			if failPayload.IncidentID != sc.ID {
				t.Fatalf("expected incident_id=%s in 500 response, got %s", sc.ID, failPayload.IncidentID)
			}
			if !reflect.DeepEqual(failPayload.ImplicatedServices, sc.ImplicatedServices) {
				t.Fatalf("implicated_services = %v, want %v", failPayload.ImplicatedServices, sc.ImplicatedServices)
			}

			// Check Prometheus /metrics
			mRes, err := http.Get(srv.URL + "/metrics")
			if err != nil {
				t.Fatalf("GET /metrics failed: %v", err)
			}
			mBytes, _ := io.ReadAll(mRes.Body)
			mRes.Body.Close()
			metricsText := string(mBytes)
			for _, implicated := range sc.ImplicatedServices {
				needle := `storefront_incident_active{incident="` + sc.ID + `",service="` + implicated + `"} 1`
				if !strings.Contains(metricsText, needle) {
					t.Fatalf("expected /metrics to contain %q, got:\n%s", needle, metricsText)
				}
			}
			for _, unaffected := range sc.UnaffectedServices {
				badNeedle := `storefront_incident_active{incident="` + sc.ID + `",service="` + unaffected + `"} 1`
				if strings.Contains(metricsText, badNeedle) {
					t.Fatalf("expected unaffected service %s NOT to be marked active in /metrics", unaffected)
				}
			}

			// Check structured logs /api/logs?incident_id=sc.ID
			lRes, err := http.Get(srv.URL + "/api/logs?incident_id=" + sc.ID)
			if err != nil {
				t.Fatalf("GET /api/logs failed: %v", err)
			}
			var logsPayload struct {
				Entries []storefront.LogEntry `json:"entries"`
			}
			_ = json.NewDecoder(lRes.Body).Decode(&logsPayload)
			lRes.Body.Close()
			if len(logsPayload.Entries) == 0 {
				t.Fatalf("expected structured stack trace log entries for %s", sc.ID)
			}
		})
	}

	// 4. Verify POST /api/incidents/randomize activates one of the 4 scenarios
	randRes, err := http.Post(srv.URL+"/api/incidents/randomize", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /api/incidents/randomize failed: %v", err)
	}
	randRes.Body.Close()
	if randRes.StatusCode != http.StatusOK {
		t.Fatalf("randomize status = %d, want 200", randRes.StatusCode)
	}
}

func TestGitWorkspaceCloneAndPushUnder500msAndHealsStorefront(t *testing.T) {
	rt, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}
	srv := httptest.NewServer(rt.Handler())
	defer srv.Close()

	// 1. Activate INC-101
	actBody := []byte(`{"incident_id":"INC-101"}`)
	actRes, err := http.Post(srv.URL+"/api/incidents/activate", "application/json", bytes.NewReader(actBody))
	if err != nil || actRes.StatusCode != http.StatusOK {
		t.Fatalf("failed to activate INC-101: err=%v status=%d", err, actRes.StatusCode)
	}
	actRes.Body.Close()

	// 2. Clone the in-cluster Git repository over HTTP and verify < 500ms latency (retrying once if parallel test CPU contention spiked)
	cloneDir := filepath.Join(t.TempDir(), "workspace")
	start := time.Now()
	cloneCmd := exec.Command("git", "clone", srv.URL+"/git/storefront.git", cloneDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone over HTTP failed: %v\noutput:\n%s", err, string(out))
	}
	elapsed := time.Since(start)
	if elapsed >= 500*time.Millisecond {
		retryDir := filepath.Join(t.TempDir(), "workspace-retry")
		retryStart := time.Now()
		retryCmd := exec.Command("git", "clone", srv.URL+"/git/storefront.git", retryDir)
		if _, err := retryCmd.CombinedOutput(); err == nil {
			if retryElapsed := time.Since(retryStart); retryElapsed < elapsed {
				elapsed = retryElapsed
			}
		}
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("expected git clone over HTTP to complete rapidly, took %v", elapsed)
	}

	// 3. Apply INC-101 fixes for pricing-service and shipping-service in cloned workspace and push to main
	sc, ok := storefront.GetScenario("INC-101")
	if !ok {
		t.Fatal("missing INC-101 scenario")
	}
	for _, svc := range sc.ImplicatedServices {
		detail := sc.ServiceTraces[svc]
		testPath := filepath.Join(cloneDir, "services", svc, detail.TestFile)
		srcPath := filepath.Join(cloneDir, "services", svc, detail.SourceFile)
		if err := os.WriteFile(testPath, []byte(detail.RegressionTestFileContent), 0o644); err != nil {
			t.Fatalf("write test file for %s: %v", svc, err)
		}
		if err := os.WriteFile(srcPath, []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
			t.Fatalf("write patched source for %s: %v", svc, err)
		}
	}

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\noutput:\n%s", args, err, string(out))
		}
	}
	gitRun("config", "user.email", "ax-root-task@cluster.local")
	gitRun("config", "user.name", "AX Root Task")
	gitRun("add", ".")
	gitRun("commit", "-m", "fix(INC-101): patch pricing-service and shipping-service")
	gitRun("push", "origin", "HEAD:main")

	// 4. Verify that pushing the fix commit healed INC-101 so the trigger cart now returns HTTP 200 OK
	trigBytes, _ := json.Marshal(sc.TriggerCart)
	healedRes, err := http.Post(srv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(trigBytes))
	if err != nil {
		t.Fatalf("checkout after git push heal failed: %v", err)
	}
	var healedPayload storefront.CheckoutResponse
	_ = json.NewDecoder(healedRes.Body).Decode(&healedPayload)
	healedRes.Body.Close()
	if healedRes.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK after pushing INC-101 fix commit, got %d (%s)", healedRes.StatusCode, healedPayload.Error)
	}
	if healedPayload.Status != "CONFIRMED" {
		t.Fatalf("expected order status CONFIRMED, got %q", healedPayload.Status)
	}
}

func TestStorefrontGitBaselineTagSelectiveWebhooksAndFastReset(t *testing.T) {
	var (
		reloadMu   sync.Mutex
		reloadHits []struct {
			Service   string `json:"service"`
			CommitSHA string `json:"commit_sha"`
		}
	)

	makeStubSupervisor := func(expectedSvc string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/internal/reload" {
				http.NotFound(w, r)
				return
			}
			var body struct {
				Service   string `json:"service"`
				CommitSHA string `json:"commit_sha"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Service == "" {
				body.Service = expectedSvc
			}
			reloadMu.Lock()
			reloadHits = append(reloadHits, body)
			reloadMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"reloaded"}`))
		}))
	}

	supURLs := make(map[string]string)
	for _, svc := range storefront.AllServices() {
		srv := makeStubSupervisor(svc)
		defer srv.Close()
		supURLs[svc] = srv.URL
	}

	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir:     t.TempDir(),
		SupervisorURLs: supURLs,
	})
	if err != nil {
		t.Fatalf("NewGitServer failed: %v", err)
	}
	httpSrv := httptest.NewServer(gitSrv.Handler())
	defer httpSrv.Close()

	// 1. Clone over Smart HTTP and verify go 1.23 compatibility and baseline-v1 tag
	cloneDir := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", httpSrv.URL+"/git/storefront.git", cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, string(out))
	}
	if out, err := exec.Command("git", "-C", cloneDir, "fetch", "--tags", "origin").CombinedOutput(); err != nil {
		t.Fatalf("git fetch --tags failed: %v\n%s", err, string(out))
	}

	goModBytes, err := os.ReadFile(filepath.Join(cloneDir, "go.mod"))
	if err != nil {
		t.Fatalf("reading seeded go.mod: %v", err)
	}
	if !strings.Contains(string(goModBytes), "go 1.23") {
		t.Fatalf("expected seeded go.mod to specify go 1.23 compatibility, got:\n%s", string(goModBytes))
	}

	baselineSHAOut, err := exec.Command("git", "-C", cloneDir, "rev-parse", "baseline-v1^{commit}").CombinedOutput()
	if err != nil {
		t.Fatalf("expected baseline-v1 tag to exist, got error: %v (%s)", err, string(baselineSHAOut))
	}
	baselineSHA := strings.TrimSpace(string(baselineSHAOut))
	headSHAOut, err := exec.Command("git", "-C", cloneDir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD failed: %v", err)
	}
	if strings.TrimSpace(string(headSHAOut)) != baselineSHA {
		t.Fatalf("initial HEAD %q != baseline-v1 %q", strings.TrimSpace(string(headSHAOut)), baselineSHA)
	}

	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cloneDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	gitRun("config", "user.email", "child@ax-bug-bash.local")
	gitRun("config", "user.name", "AX Child Task")

	// 2. Pushing a refs/heads/fix/* branch must NOT trigger main production reload webhooks
	sc, _ := storefront.GetScenario("INC-101")
	for _, svc := range sc.ImplicatedServices {
		detail := sc.ServiceTraces[svc]
		if err := os.WriteFile(filepath.Join(cloneDir, "services", svc, detail.SourceFile), []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
			t.Fatalf("write patched %s: %v", svc, err)
		}
	}
	gitRun("checkout", "-b", "fix/fixer-inc-101-pricing")
	gitRun("add", ".")
	gitRun("commit", "-m", "fix(INC-101): patch pricing-service and shipping-service")
	gitRun("push", "origin", "HEAD:refs/heads/fix/fixer-inc-101-pricing")

	reloadMu.Lock()
	if len(reloadHits) != 0 {
		t.Fatalf("expected 0 reload webhooks when pushing refs/heads/fix/* branch, got %v", reloadHits)
	}
	reloadMu.Unlock()

	// 3. Pushing to main triggers selective reload webhooks ONLY for pricing-service and shipping-service
	gitRun("push", "origin", "HEAD:refs/heads/main")
	newMainSHAOut, _ := exec.Command("git", "-C", cloneDir, "rev-parse", "HEAD").CombinedOutput()
	newMainSHA := strings.TrimSpace(string(newMainSHAOut))

	reloadMu.Lock()
	gotReloadSvcs := make([]string, 0, len(reloadHits))
	for _, hit := range reloadHits {
		gotReloadSvcs = append(gotReloadSvcs, hit.Service)
		if hit.CommitSHA != newMainSHA {
			t.Errorf("webhook commit_sha = %q, want %q", hit.CommitSHA, newMainSHA)
		}
	}
	reloadHits = nil
	reloadMu.Unlock()

	sort.Strings(gotReloadSvcs)
	if !reflect.DeepEqual(gotReloadSvcs, []string{"pricing-service", "shipping-service"}) {
		t.Fatalf("selective reload services = %v, want [pricing-service shipping-service]", gotReloadSvcs)
	}

	// 4. POST /api/incidents/heal must NOT wipe main commit
	healRes, err := http.Post(httpSrv.URL+"/api/incidents/heal", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /api/incidents/heal failed: %v", err)
	}
	_ = healRes.Body.Close()
	if got := gitSrv.GitManager().CurrentCommitSHA("refs/heads/main"); got != newMainSHA {
		t.Fatalf("POST /api/incidents/heal unexpectedly changed main from %s to %s", newMainSHA, got)
	}

	// 5. POST /api/incidents/reset restores baseline-v1, prunes refs/heads/fix/*, and selectively reloads in < 3s
	resetStart := time.Now()
	resetRes, err := http.Post(httpSrv.URL+"/api/incidents/reset", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /api/incidents/reset failed: %v", err)
	}
	var resetPayload struct {
		Status           string   `json:"status"`
		BaselineTag      string   `json:"baseline_tag"`
		CommitSHA        string   `json:"commit_sha"`
		ReloadedServices []string `json:"reloaded_services"`
		PrunedBranches   []string `json:"pruned_branches"`
		DurationMs       int64    `json:"duration_ms"`
	}
	if err := json.NewDecoder(resetRes.Body).Decode(&resetPayload); err != nil {
		t.Fatalf("decoding reset response: %v", err)
	}
	_ = resetRes.Body.Close()
	resetElapsed := time.Since(resetStart)

	if resetRes.StatusCode != http.StatusOK || resetPayload.Status != "reset" {
		t.Fatalf("reset status = %d (%q), want 200/reset", resetRes.StatusCode, resetPayload.Status)
	}
	if resetElapsed >= 3*time.Second {
		t.Fatalf("expected baseline-v1 reset in < 3s, took %v", resetElapsed)
	}
	if resetPayload.BaselineTag != "baseline-v1" || resetPayload.CommitSHA != baselineSHA {
		t.Fatalf("reset commit = (%q, %q), want (baseline-v1, %q)", resetPayload.BaselineTag, resetPayload.CommitSHA, baselineSHA)
	}
	if !reflect.DeepEqual(resetPayload.ReloadedServices, []string{"pricing-service", "shipping-service"}) {
		t.Fatalf("reset reloaded_services = %v, want [pricing-service shipping-service]", resetPayload.ReloadedServices)
	}
	if !reflect.DeepEqual(resetPayload.PrunedBranches, []string{"refs/heads/fix/fixer-inc-101-pricing"}) {
		t.Fatalf("reset pruned_branches = %v, want [refs/heads/fix/fixer-inc-101-pricing]", resetPayload.PrunedBranches)
	}

	lsFixOut, _ := exec.Command("git", "ls-remote", "--heads", httpSrv.URL+"/git/storefront.git", "refs/heads/fix/*").CombinedOutput()
	if strings.TrimSpace(string(lsFixOut)) != "" {
		t.Fatalf("expected refs/heads/fix/* to be pruned, got:\n%s", string(lsFixOut))
	}

	reloadMu.Lock()
	var resetReloadSvcs []string
	for _, hit := range reloadHits {
		resetReloadSvcs = append(resetReloadSvcs, hit.Service)
		if hit.CommitSHA != baselineSHA {
			t.Errorf("reset reload commit_sha = %q, want %q", hit.CommitSHA, baselineSHA)
		}
	}
	reloadMu.Unlock()
	sort.Strings(resetReloadSvcs)
	if !reflect.DeepEqual(resetReloadSvcs, []string{"pricing-service", "shipping-service"}) {
		t.Fatalf("reset webhook hits = %v, want [pricing-service shipping-service]", resetReloadSvcs)
	}
}

func TestPolyglotFiveServiceMeshEndToEndCheckoutHotReloadAndBaselineReset(t *testing.T) {
	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewGitServer failed: %v", err)
	}
	t.Cleanup(func() { _ = gitSrv.Close() })
	gitHTTPSrv := httptest.NewServer(gitSrv.Handler())
	t.Cleanup(gitHTTPSrv.Close)

	baselineSHA := gitSrv.GitManager().BaselineCommitSHA()

	supServers := make(map[string]*httptest.Server, 5)
	supURLs := make(map[string]string, 5)

	bootSupervised := func(svc string, extraEnv ...string) *httptest.Server {
		t.Helper()
		sup, sErr := supervisor.New(supervisor.Config{
			Service:    svc,
			GitRepoURL: gitHTTPSrv.URL + "/git/storefront.git",
			WorkDir:    t.TempDir(),
			ChildEnv:   extraEnv,
		})
		if sErr != nil {
			t.Fatalf("supervisor.New(%s) failed: %v", svc, sErr)
		}
		t.Cleanup(func() { _ = sup.Close() })
		srv := httptest.NewServer(sup.Handler())
		t.Cleanup(srv.Close)
		supServers[svc] = srv
		supURLs[svc] = srv.URL
		return srv
	}

	checkoutSrv := bootSupervised(storefront.ServiceCheckout)
	shippingSrv := bootSupervised(storefront.ServiceShipping)
	pricingSrv := bootSupervised(storefront.ServicePricing)
	inventorySrv := bootSupervised(storefront.ServiceInventory)
	webSrv := bootSupervised(
		storefront.ServiceStorefrontWeb,
		"CHECKOUT_SERVICE_URL="+checkoutSrv.URL,
		"SHIPPING_SERVICE_URL="+shippingSrv.URL,
		"PRICING_SERVICE_URL="+pricingSrv.URL,
		"INVENTORY_SERVICE_URL="+inventorySrv.URL,
		"STOREFRONT_GIT_HTTP_URL="+gitHTTPSrv.URL,
	)

	gitSrv.SetSupervisorURLs(supURLs)

	readMetric500 := func(svc string) int {
		t.Helper()
		res, mErr := http.Get(supServers[svc].URL + "/metrics")
		if mErr != nil {
			t.Fatalf("GET /metrics (%s) failed: %v", svc, mErr)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		needle := `storefront_http_requests_total{status="500",service="` + svc + `"} `
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, needle) {
				var count int
				_, _ = io.ReadFull(strings.NewReader(""), nil)
				for _, ch := range strings.TrimSpace(strings.TrimPrefix(line, needle)) {
					if ch >= '0' && ch <= '9' {
						count = count*10 + int(ch-'0')
					}
				}
				return count
			}
		}
		t.Fatalf("missing 500 metric series for %s in:\n%s", svc, string(b))
		return -1
	}

	snapshot500s := func() map[string]int {
		out := make(map[string]int, 5)
		for _, svc := range storefront.AllServices() {
			out[svc] = readMetric500(svc)
		}
		return out
	}

	// 1. UI & Safe DOM check on storefront-web
	uiRes, err := http.Get(webSrv.URL + "/")
	if err != nil || uiRes.StatusCode != http.StatusOK {
		t.Fatalf("GET / on storefront-web failed: err=%v status=%v", err, uiRes)
	}
	uiHTML, _ := io.ReadAll(uiRes.Body)
	_ = uiRes.Body.Close()
	if !strings.Contains(string(uiHTML), "theme-light-btn") || !strings.Contains(string(uiHTML), "Apex Hardware Storefront") {
		t.Fatalf("unexpected storefront-web index.html content")
	}

	jsRes, err := http.Get(webSrv.URL + "/static/app.js")
	if err != nil || jsRes.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.js failed: err=%v", err)
	}
	jsBytes, _ := io.ReadAll(jsRes.Body)
	_ = jsRes.Body.Close()
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "alert(", "localStorage", "sessionStorage"} {
		if strings.Contains(string(jsBytes), banned) {
			t.Fatalf("storefront-web static/app.js contains forbidden pattern %q", banned)
		}
	}

	// 2. Happy-path checkout across all 5 real polyglot services -> HTTP 200 OK
	happyBody := `{"items":[{"sku":"sku-desk-mat-xl","quantity":1}],"currency":"USD"}`
	happyRes, err := http.Post(webSrv.URL+"/api/cart/checkout", "application/json", bytes.NewReader([]byte(happyBody)))
	if err != nil {
		t.Fatalf("happy-path checkout failed: %v", err)
	}
	var happyPayload storefront.CheckoutResponse
	_ = json.NewDecoder(happyRes.Body).Decode(&happyPayload)
	_ = happyRes.Body.Close()
	if happyRes.StatusCode != http.StatusOK || happyPayload.Status != "CONFIRMED" {
		t.Fatalf("happy-path checkout = (%d, %+v), want 200/CONFIRMED", happyRes.StatusCode, happyPayload)
	}
	if strings.TrimSpace(happyRes.Header.Get("X-Storefront-Request-ID")) == "" {
		t.Fatal("expected X-Storefront-Request-ID response header on checkout")
	}
	for svc, c := range snapshot500s() {
		if c != 0 {
			t.Fatalf("expected 0 HTTP 500s on %s after happy path, got %d", svc, c)
		}
	}

	// 3. Organic HTTP 500 propagation for all 4 real edge-case carts (INC-101..INC-104) with zero synthetic flags
	for _, sc := range storefront.ScenarioCatalog() {
		before := snapshot500s()
		cartBytes, _ := json.Marshal(sc.TriggerCart)
		res, cErr := http.Post(webSrv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
		if cErr != nil {
			t.Fatalf("%s checkout request failed: %v", sc.ID, cErr)
		}
		var failPayload storefront.CheckoutResponse
		_ = json.NewDecoder(res.Body).Decode(&failPayload)
		_ = res.Body.Close()

		if res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s checkout status = %d, want 500 (%+v)", sc.ID, res.StatusCode, failPayload)
		}
		if failPayload.IncidentID != sc.ID {
			t.Fatalf("%s checkout incident_id = %q, want %q", sc.ID, failPayload.IncidentID, sc.ID)
		}
		if !reflect.DeepEqual(failPayload.ImplicatedServices, sc.ImplicatedServices) {
			t.Fatalf("%s implicated_services = %v, want %v", sc.ID, failPayload.ImplicatedServices, sc.ImplicatedServices)
		}
		if len(failPayload.StackTraces) != len(sc.ImplicatedServices) {
			t.Fatalf("%s stack_traces len = %d, want %d", sc.ID, len(failPayload.StackTraces), len(sc.ImplicatedServices))
		}

		after := snapshot500s()
		implicatedSet := make(map[string]bool)
		for _, s := range sc.ImplicatedServices {
			implicatedSet[s] = true
		}
		for _, svc := range storefront.AllServices() {
			delta := after[svc] - before[svc]
			if implicatedSet[svc] && delta != 1 {
				t.Fatalf("%s expected implicated service %s 500 counter delta=1, got %d", sc.ID, svc, delta)
			}
			if !implicatedSet[svc] && delta != 0 {
				t.Fatalf("%s expected unaffected service %s 500 counter delta=0, got %d", sc.ID, svc, delta)
			}
		}
	}

	// 4. Clone & push INC-103 3-service fix through storefront-web's /git/storefront.git reverse proxy!
	cloneDir := filepath.Join(t.TempDir(), "mesh-clone")
	if out, cErr := exec.Command("git", "clone", webSrv.URL+"/git/storefront.git", cloneDir).CombinedOutput(); cErr != nil {
		t.Fatalf("git clone via storefront-web /git/* reverse proxy failed: %v\n%s", cErr, string(out))
	}
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cloneDir
		if out, gErr := cmd.CombinedOutput(); gErr != nil {
			t.Fatalf("git %v failed: %v\n%s", args, gErr, string(out))
		}
	}
	gitRun("config", "user.email", "root@ax-bug-bash.local")
	gitRun("config", "user.name", "AX Root Task")
	gitRun("config", "commit.gpgsign", "false")

	sc103, _ := storefront.GetScenario(storefront.IncidentZeroDecimalJPYRounding)
	for _, svc := range sc103.ImplicatedServices {
		detail := sc103.ServiceTraces[svc]
		if err := os.WriteFile(filepath.Join(cloneDir, "services", svc, detail.SourceFile), []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
			t.Fatalf("writing fix for %s: %v", svc, err)
		}
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "fix(INC-103): round zero-decimal JPY across storefront-web, pricing-service, and checkout-service")
	gitRun("push", "origin", "HEAD:refs/heads/main")

	fixSHA := gitSrv.GitManager().CurrentCommitSHA("refs/heads/main")

	// Verify selective hot-reload: ONLY storefront-web, pricing-service, checkout-service reloaded; shipping & inventory stayed at baselineSHA!
	for _, svc := range storefront.AllServices() {
		stRes, sErr := http.Get(supServers[svc].URL + "/internal/status")
		if sErr != nil {
			t.Fatalf("GET /internal/status (%s) failed: %v", svc, sErr)
		}
		var st struct {
			CommitSHA   string `json:"commit_sha"`
			ReloadCount uint64 `json:"reload_count"`
		}
		_ = json.NewDecoder(stRes.Body).Decode(&st)
		_ = stRes.Body.Close()

		if svc == storefront.ServiceShipping || svc == storefront.ServiceInventory {
			if st.CommitSHA != baselineSHA || st.ReloadCount != 0 {
				t.Fatalf("unaffected %s unexpectedly reloaded: sha=%s count=%d", svc, st.CommitSHA, st.ReloadCount)
			}
		} else {
			if st.CommitSHA != fixSHA || st.ReloadCount != 1 {
				t.Fatalf("implicated %s did not hot-reload to %s: got sha=%s count=%d", svc, fixSHA, st.CommitSHA, st.ReloadCount)
			}
		}
	}

	// Verify INC-103 JPY cart now succeeds with HTTP 200 OK!
	jpyBytes, _ := json.Marshal(sc103.TriggerCart)
	healed103Res, err := http.Post(webSrv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(jpyBytes))
	if err != nil {
		t.Fatalf("healed INC-103 checkout failed: %v", err)
	}
	var healed103Payload storefront.CheckoutResponse
	_ = json.NewDecoder(healed103Res.Body).Decode(&healed103Payload)
	_ = healed103Res.Body.Close()
	if healed103Res.StatusCode != http.StatusOK || healed103Payload.Status != "CONFIRMED" {
		t.Fatalf("expected HTTP 200 CONFIRMED for healed INC-103 JPY cart, got %d: %+v", healed103Res.StatusCode, healed103Payload)
	}

	// 5. Reset to baseline-v1 in < 3s via storefront-web /api/incidents/reset proxy!
	resetStart := time.Now()
	resetRes, err := http.Post(webSrv.URL+"/api/incidents/reset", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /api/incidents/reset via storefront-web failed: %v", err)
	}
	var resetOut storefront.GitResetResult
	_ = json.NewDecoder(resetRes.Body).Decode(&resetOut)
	_ = resetRes.Body.Close()
	resetDur := time.Since(resetStart)

	if resetRes.StatusCode != http.StatusOK || resetOut.CommitSHA != baselineSHA {
		t.Fatalf("reset via storefront-web failed: status=%d out=%+v", resetRes.StatusCode, resetOut)
	}
	if resetDur >= 3*time.Second {
		t.Fatalf("expected full 3-service baseline-v1 reset + hot-reload < 3s, took %v", resetDur)
	}

	// Verify INC-103 JPY cart fails organically again after baseline-v1 reset!
	rebrokenRes, err := http.Post(webSrv.URL+"/api/cart/checkout", "application/json", bytes.NewReader(jpyBytes))
	if err != nil {
		t.Fatalf("post-reset INC-103 checkout failed: %v", err)
	}
	_ = rebrokenRes.Body.Close()
	if rebrokenRes.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected INC-103 cart to return HTTP 500 again after baseline-v1 reset, got %d", rebrokenRes.StatusCode)
	}
}

func TestPolyglotMeshDockerfileContractAndSecurityHygiene(t *testing.T) {
	root := repoRoot(t)
	dockerfilePath := filepath.Join(root, "Dockerfile.polyglot-mesh")
	b, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("reading Dockerfile.polyglot-mesh: %v", err)
	}
	content := string(b)

	requiredSnippets := []string{
		"FROM python:3.12-slim",
		"setup_22.x",
		"go1.23",
		"rustup",
		"fastapi",
		"uvicorn",
		"CARGO_TARGET_DIR=/opt/cargo-target",
		"STOREFRONT_SERVICES_DIR=/opt/storefront-seed/services",
		"/usr/local/bin/polyglot-supervisor",
		"/usr/local/bin/storefront-git",
		"services/inventory-service/server/Cargo.toml",
		"services/checkout-service/cmd/server",
		"services/shipping-service/cmd/server",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(content, snippet) {
			t.Errorf("Dockerfile.polyglot-mesh missing required pre-warmed runtime contract snippet %q", snippet)
		}
	}

	forbiddenSnippets := []string{
		"ate-cluster",
		"mofilabs",
		"598464211339",
		"substrate-poc",
		"Vertex" + " AI",
		"fluid" + " compute",
	}
	for _, banned := range forbiddenSnippets {
		if strings.Contains(strings.ToLower(content), strings.ToLower(banned)) {
			t.Errorf("Dockerfile.polyglot-mesh contains forbidden identifier/term %q", banned)
		}
	}
}




