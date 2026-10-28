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
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
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

	// 2. Clone the in-cluster Git repository over HTTP and verify < 500ms latency
	cloneDir := filepath.Join(t.TempDir(), "workspace")
	start := time.Now()
	cloneCmd := exec.Command("git", "clone", srv.URL+"/git/storefront.git", cloneDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone over HTTP failed: %v\noutput:\n%s", err, string(out))
	}
	elapsed := time.Since(start)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("expected git clone over HTTP to complete in < 500ms, took %v", elapsed)
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


