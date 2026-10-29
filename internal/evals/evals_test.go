package evals_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/evals"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func TestSingleScenarioTrajectoryGradesAllSixInvariants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	grade, err := evals.RunScenarioTrajectory(ctx, evals.TrajectorySpec{
		IncidentID:     storefront.IncidentStackedPromoNegativeSubtotal,
		HarnessMode:    "multi",
		ProvenanceMode: gateway.ProvenanceFixture,
	})
	if err != nil {
		t.Fatalf("RunScenarioTrajectory(INC-101, multi, FIXTURE) failed: %v", err)
	}

	if !grade.Passed {
		t.Fatalf("expected trajectory grade Passed=true, got false: %+v", grade)
	}
	if !grade.BaselineCIGreen {
		t.Errorf("invariant 1 failed: BaselineCIGreen = false")
	}
	if !grade.AlertmanagerWebhookTriggered {
		t.Errorf("invariant 2 failed: AlertmanagerWebhookTriggered = false")
	}
	if !grade.SelectiveBlastRadiusFanOut {
		t.Errorf("invariant 3 failed: SelectiveBlastRadiusFanOut = false")
	}
	if !grade.RedGreenRegressionTDD {
		t.Errorf("invariant 4 failed: RedGreenRegressionTDD = false")
	}
	if !grade.OnCommandExitAndSuspendReclaimed {
		t.Errorf("invariant 5 failed: OnCommandExitAndSuspendReclaimed = false")
	}
	if !grade.PreWarmedRolloutAndStorefrontHealed {
		t.Errorf("invariant 6 failed: PreWarmedRolloutAndStorefrontHealed = false")
	}
	if len(grade.ImplicatedServices) != 2 {
		t.Errorf("len(ImplicatedServices) = %d, want 2 (pricing-service, shipping-service)", len(grade.ImplicatedServices))
	}
	if grade.SpawnedChildCount != 2 {
		t.Errorf("SpawnedChildCount = %d, want 2", grade.SpawnedChildCount)
	}
	if grade.RolloutDurationMs <= 0 || grade.RolloutDurationMs >= 10000 {
		t.Errorf("RolloutDurationMs = %d, want in (0, 10000)", grade.RolloutDurationMs)
	}
	if grade.PostRolloutCheckoutStatus != 200 {
		t.Errorf("PostRolloutCheckoutStatus = %d, want 200", grade.PostRolloutCheckoutStatus)
	}
}

func TestFourScenariosAcrossAllHarnessesAndLiveCascade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	report, err := evals.RunEvaluationSuite(ctx, evals.SuiteConfig{
		IncludeLiveCascadeChecks: true,
		MaxConcurrency:           4,
	})
	if err != nil {
		t.Fatalf("RunEvaluationSuite failed: %v", err)
	}
	if !report.AllPassed {
		for _, tr := range report.Trajectories {
			if !tr.Passed {
				t.Errorf("trajectory (%s, harness=%s, provenance=%s) failed: %v", tr.IncidentID, tr.HarnessMode, tr.ProvenanceMode, tr.FailureReasons)
			}
		}
		t.Fatalf("expected AllPassed=true, got %d/%d passed", report.PassedTrajectories, report.TotalTrajectories)
	}

	// 4 Scenarios x 4 Harnesses in FIXTURE mode (16) + 4 LIVE Gemini Flash Cascade runs (antigravity, opencode, pi, multi) = 20
	if report.TotalTrajectories < 20 {
		t.Errorf("TotalTrajectories = %d, want >= 20 (16 4x4 matrix + 4 LIVE cascade checks)", report.TotalTrajectories)
	}

	for _, scID := range []string{"INC-101", "INC-102", "INC-103", "INC-104"} {
		if report.ScenarioCoverage[scID] < 4 {
			t.Errorf("ScenarioCoverage[%s] = %d, want >= 4 (all 4 harness configurations)", scID, report.ScenarioCoverage[scID])
		}
	}

	for _, h := range []string{"antigravity", "opencode", "pi", "multi"} {
		if report.HarnessCoverage[h] < 5 {
			t.Errorf("HarnessCoverage[%s] = %d, want >= 5 (4 scenarios + LIVE cascade check)", h, report.HarnessCoverage[h])
		}
	}

	for _, model := range gateway.DefaultFallbackCascade {
		if !report.CascadeModelsVerified[model] {
			t.Errorf("expected Gemini Flash cascade model %q to be verified in LIVE cascade runs, got %v", model, report.CascadeModelsVerified)
		}
	}
}

func TestRepositoryHygieneAndManifestContract(t *testing.T) {
	hygiene, err := evals.VerifyRepositoryHygieneAndManifests("../..")
	if err != nil {
		t.Fatalf("VerifyRepositoryHygieneAndManifests failed: %v", err)
	}
	if !hygiene.Passed {
		t.Fatalf("expected hygiene & manifest contract Passed=true, got violations: %v", hygiene.Violations)
	}
	if len(hygiene.CheckedFiles) < 14 {
		t.Errorf("len(CheckedFiles) = %d, want >= 14 (including 03-gmp-podmonitoring.yaml, internal/observer/about_catalog.go, blog, tutorial, and demo recording scripts)", len(hygiene.CheckedFiles))
	}
	wantChecked := map[string]bool{
		"manifests/monitoring/03-gmp-podmonitoring.yaml":            false,
		"internal/observer/about_catalog.go":                        false,
		"docs/blog/self-spawning-bug-bash-tree-on-google-ax.md":     false,
		"docs/tutorial/build-a-self-spawning-bug-bash-tree.md":      false,
		"hack/record-stage-walkthrough.sh":                          false,
		"hack/record-terminal-demo.py":                              false,
	}
	for _, f := range hygiene.CheckedFiles {
		if _, ok := wantChecked[f]; ok {
			wantChecked[f] = true
		}
	}
	for f, seen := range wantChecked {
		if !seen {
			t.Errorf("expected CheckedFiles to contain %q, got %v", f, hygiene.CheckedFiles)
		}
	}
}

func TestVerifyMarkdownHumanizerAndHygiene(t *testing.T) {
	cleanMarkdown := "# Section 1: Why CI Passed and Production Still Returned HTTP 500\n\n" +
		"Every single-service unit test passes in baseline CI, yet a stacked coupon cart triggers an HTTP 500 response within 15s.\n\n" +
		"Use `kubectl --context=current get pods` or `--execute-task` with `gemini-3.8-flash -> gemini-3.7-flash`.\n\n" +
		"```go\n" +
		"// Code comment with em-dash \u2014 and --flag -> arrow inside fenced block is allowed\n" +
		"cmd := exec.Command(\"ax-task-runner\", \"--execute-task\", \"--role=child-fixer\")\n" +
		"```\n"

	if violations := evals.VerifyMarkdownHumanizerAndHygiene("docs/blog/clean.md", cleanMarkdown); len(violations) != 0 {
		t.Fatalf("expected zero violations on clean technical markdown, got: %v", violations)
	}

	cases := []struct {
		name        string
		markdown    string
		wantSubstr  string
	}{
		{
			name:       "em dash in prose",
			markdown:   "The Root Task isolates implicated services \u2014 skipping healthy ones.\n",
			wantSubstr: "dash",
		},
		{
			name:       "en dash in prose",
			markdown:   "The rollout finishes in 820ms \u2013 4200ms across the cluster.\n",
			wantSubstr: "dash",
		},
		{
			name:       "spaced double hyphen in prose",
			markdown:   "The supervisor swaps the process -- without dropping active connections.\n",
			wantSubstr: "dash",
		},
		{
			name:       "not X, but Y contrast",
			markdown:   "This is not a static retry loop, but a recursive task tree.\n",
			wantSubstr: "contrast",
		},
		{
			name:       "not just X, it's Y contrast",
			markdown:   "It is not just a container runner, it's a warm sandbox pool.\n",
			wantSubstr: "contrast",
		},
		{
			name:       "not only X, but Y contrast",
			markdown:   "The gateway provides not only authentication, but automatic model failover.\n",
			wantSubstr: "contrast",
		},
		{
			name:       "not merely X, but Y contrast",
			markdown:   "The supervisor is not merely a wrapper, but a PID-1 reverse proxy.\n",
			wantSubstr: "contrast",
		},
		{
			name:       "staged opener Let's dive in",
			markdown:   "Let's dive in and see how the Root Task works.\n",
			wantSubstr: "staged opener",
		},
		{
			name:       "staged opener Here's the thing",
			markdown:   "Here's the thing about microservice unit tests.\n",
			wantSubstr: "staged opener",
		},
		{
			name:       "staged opener Without further ado",
			markdown:   "Without further ado, here is the benchmark table.\n",
			wantSubstr: "staged opener",
		},
		{
			name:       "AI filler word delve",
			markdown:   "We will delve into the sandbox checkpointing implementation.\n",
			wantSubstr: "filler",
		},
		{
			name:       "AI filler word tapestry",
			markdown:   "The system weaves a rich tapestry of telemetry signals.\n",
			wantSubstr: "filler",
		},
		{
			name:       "AI filler word seamlessly",
			markdown:   "Child Tasks fail over seamlessly across model tiers.\n",
			wantSubstr: "filler",
		},
		{
			name:       "AI filler word pivotal",
			markdown:   "Warm sandboxes play a pivotal role in fast recovery.\n",
			wantSubstr: "filler",
		},
		{
			name:       "forbidden retired brand token",
			markdown:   "The proxy routes requests to " + "Vertex" + " AI endpoints.\n",
			wantSubstr: "forbidden",
		},
		{
			name:       "forbidden internal term",
			markdown:   "The gateway designs for " + "fluid" + " compute across regions.\n",
			wantSubstr: "forbidden",
		},
		{
			name:       "forbidden personal cluster or bucket identifier",
			markdown:   "Snapshots upload to gs://my-personal-bucket/snaps on " + "ate" + "-cluster.\n",
			wantSubstr: "forbidden",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evals.VerifyMarkdownHumanizerAndHygiene("docs/blog/test.md", tc.markdown)
			if len(v) == 0 {
				t.Fatalf("expected violation containing %q for %s, got none", tc.wantSubstr, tc.name)
			}
			matched := false
			for _, msg := range v {
				if strings.Contains(strings.ToLower(msg), strings.ToLower(tc.wantSubstr)) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("expected violation containing %q, got %v", tc.wantSubstr, v)
			}
		})
	}
}

func TestLiveGKEChaosSoakRunnerMockContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var (
		mu                sync.Mutex
		activeHarness     = "multi"
		activeIncidentID  string
		incidentHealed    bool
		firingAlertsCount int
		latestTree        *observer.ObserverTreeView
		trees             []*observer.ObserverTreeView
		baselineSHA       = "a1b2c3d4baselinev1"
		currentSHA        = baselineSHA
		forceFixture      bool
		loggingHeaders    http.Header
		monitoringHeaders http.Header
	)

	// Mock Storefront + Storefront-Git server bound strictly to 127.0.0.1
	storefrontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/healthz", "/readyz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/incidents/activate":
			var req struct {
				IncidentID string `json:"incident_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			activeIncidentID = strings.ToUpper(strings.TrimSpace(req.IncidentID))
			incidentHealed = false
			firingAlertsCount = 1
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"activated","incident_id":%q}`, activeIncidentID)
		case "/api/cart/checkout":
			w.Header().Set("Content-Type", "application/json")
			if activeIncidentID != "" && !incidentHealed {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = fmt.Fprintf(w, `{"status":"ERROR","incident_id":%q}`, activeIncidentID)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"CONFIRMED","order_id":"ord-live-200"}`))
		case "/api/incidents/reset":
			activeIncidentID = ""
			incidentHealed = true
			firingAlertsCount = 0
			currentSHA = baselineSHA
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"reset","baseline_tag":"baseline-v1","commit_sha":%q,"duration_ms":42}`, baselineSHA)
		case "/api/incidents/heal":
			incidentHealed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"healed","commit_sha":%q}`, currentSHA)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer storefrontSrv.Close()

	// Mock Prometheus server bound strictly to 127.0.0.1
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		firing := firingAlertsCount
		inc := activeIncidentID
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if firing > 0 && inc != "" {
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"alerts":[{"labels":{"alertname":"StorefrontHTTP500Spike","incident":%q},"state":"firing"}]}}`, inc)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"alerts":[]}}`))
	}))
	defer promSrv.Close()

	// Mock AX Observer server bound strictly to 127.0.0.1
	// Embeds stage-unsafe tokens in RootTaskName/logs to verify stage-safe report scrubbing!
	unsafeClusterToken := "gke-demo-cluster-default-pool-9999"
	unsafeProjectToken := "projects/demo-secret-project"
	unsafeNumberToken := "123456789012"
	observerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/config/harness":
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				var req struct {
					DefaultHarness string `json:"default_harness"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.DefaultHarness != "" {
					activeHarness = req.DefaultHarness
				}
			}
			_, _ = fmt.Fprintf(w, `{"status":"updated","default_harness":%q,"execution_mode":"LIVE_GKE"}`, activeHarness)
		case "/api/incidents/reset":
			var req struct {
				DefaultHarness string `json:"default_harness"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.DefaultHarness != "" {
				activeHarness = req.DefaultHarness
			}
			activeIncidentID = ""
			incidentHealed = true
			firingAlertsCount = 0
			currentSHA = baselineSHA
			_, _ = fmt.Fprintf(w, `{"status":"reset","default_harness":%q}`, activeHarness)
		case "/api/incidents/trigger":
			var req struct {
				IncidentID string `json:"incident_id"`
				Harness    string `json:"harness"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			incID := strings.ToUpper(strings.TrimSpace(req.IncidentID))
			sc, _ := storefront.GetScenario(incID)
			h := req.Harness
			if h == "" {
				h = activeHarness
			}
			children := make([]observer.ObserverChildTaskView, 0, len(sc.ImplicatedServices))
			rot := []string{"antigravity", "opencode", "pi"}
			prov := "LIVE"
			if forceFixture {
				prov = "FIXTURE"
			}
			for idx, svc := range sc.ImplicatedServices {
				childHarness := h
				if h == "multi" {
					childHarness = rot[idx%len(rot)]
				}
				children = append(children, observer.ObserverChildTaskView{
					TaskName:           fmt.Sprintf("fix-%s-%s", strings.ToLower(sc.ID), svc),
					ParentTask:         fmt.Sprintf("triage-%s-%s", strings.ToLower(sc.ID), unsafeClusterToken),
					IncidentID:         sc.ID,
					Service:            svc,
					Phase:              "Suspended",
					Harness:            childHarness,
					RequestedModel:     gateway.DefaultFallbackCascade[0],
					ActiveModel:        gateway.DefaultFallbackCascade[0],
					SourceProvenance:   prov,
					RegressionTestName: "TestRegression_" + sc.ID,
					TDDSteps: []bugbash.TDDStepEvent{
						{Phase: "REPRODUCE_RED", Status: "PASSED", Summary: "Failing regression test reproduced HTTP 500"},
						{Phase: "PATCH_GREEN", Status: "PASSED", Summary: "Patched service and verified all tests pass"},
					},
					RegressionTestDiff: fmt.Sprintf("diff --git a/services/%s/test b/services/%s/test\n+TestRegression_%s", svc, svc, sc.ID),
					CodeFixDiff:        fmt.Sprintf("diff --git a/services/%s/fix b/services/%s/fix\n+patched", svc, svc),
					UnifiedGitDiff:     fmt.Sprintf("diff --git a/services/%s/fix b/services/%s/fix\n+patched", svc, svc),
					RedTestOutput:      "FAIL: expected 200, got 500",
					GreenTestOutput:    "PASS: all tests passed",
					SlotReclamationMs:  48,
					Suspended:          true,
				})
			}
			incidentHealed = true
			currentSHA = "patched-" + strings.ToLower(sc.ID)
			tree := observer.ObserverTreeView{
				ExecutionID:   fmt.Sprintf("exec-%s-%s-%s", strings.ToLower(sc.ID), unsafeProjectToken, unsafeNumberToken),
				ExecutionMode: "LIVE_GKE",
				IncidentID:    sc.ID,
				Title:         sc.Title,
				Summary:       sc.Summary,
				WhyCIMissedIt: sc.WhyCIMissedIt,
				HarnessMode:   h,
				RequestedModel: gateway.DefaultFallbackCascade[0],
				SourceProvenance: "LIVE",
				Phase:         "HEALED",
				RootTask: observer.ObserverRootTaskView{
					TaskName:           fmt.Sprintf("triage-%s", strings.ToLower(sc.ID)),
					Phase:              "Suspended",
					ImplicatedServices: append([]string(nil), sc.ImplicatedServices...),
					UnaffectedServices: append([]string(nil), sc.UnaffectedServices...),
				},
				ImplicatedServices: append([]string(nil), sc.ImplicatedServices...),
				UnaffectedServices: append([]string(nil), sc.UnaffectedServices...),
				ChildTasks:         children,
				MergedGitCommitSHA: currentSHA,
				RolloutDurationMs:  1420,
				StorefrontHealed:   true,
			}
			latestTree = &tree
			trees = append(trees, &tree)
			_, _ = fmt.Fprintf(w, `{"status":"triggered","execution_id":%q}`, tree.ExecutionID)
		case "/api/state":
			snap := observer.StateSnapshot{
				ExecutionMode:  "LIVE_GKE",
				DefaultHarness: activeHarness,
				LatestTree:     latestTree,
				Trees:          trees,
			}
			snap.WorkerPool.TotalReclaimedSlots = 9
			snap.Gateway.ActiveModel = gateway.DefaultFallbackCascade[0]
			snap.Gateway.SourceProvenance = "LIVE"
			_ = json.NewEncoder(w).Encode(snap)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer observerSrv.Close()

	// Mock Google Cloud Logging v2 API & Cloud Monitoring v3 API bound strictly to 127.0.0.1
	cloudLoggingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		loggingHeaders = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[{"logName":"projects/redacted/logs/stdout","jsonPayload":{"incident_id":"INC-101","service":"pricing-service"}},{"logName":"projects/redacted/logs/stderr","jsonPayload":{"incident_id":"INC-104","service":"inventory-service"}}]}`))
	}))
	defer cloudLoggingSrv.Close()

	cloudMonitoringSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		monitoringHeaders = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"timeSeries":[{"metric":{"type":"prometheus.googleapis.com/storefront_http_requests_total/counter","labels":{"service":"storefront-web","status":"500"}},"points":[{"value":{"doubleValue":4}}]}]}`))
	}))
	defer cloudMonitoringSrv.Close()

	tmpReportPath := filepath.Join(t.TempDir(), "ax-live-gke-eval-report.json")
	report, err := evals.RunLiveGKEChaosSoak(ctx, evals.LiveGKEEvalConfig{
		ObserverURL:        observerSrv.URL,
		StorefrontURL:      storefrontSrv.URL,
		StorefrontGitURL:   storefrontSrv.URL,
		PrometheusURL:      promSrv.URL,
		ProjectID:          "demo-project-id",
		AccessToken:        "test-oauth2-bearer-token",
		CloudLoggingURL:    cloudLoggingSrv.URL,
		CloudMonitoringURL: cloudMonitoringSrv.URL,
		ReportPath:         tmpReportPath,
		TrajectoryTimeout:  10 * time.Second,
		AlertDrainTimeout:  5 * time.Second,
		DualExportTimeout:  5 * time.Second,
		PollInterval:       25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunLiveGKEChaosSoak failed: %v", err)
	}
	if !report.AllPassed {
		t.Fatalf("expected report.AllPassed=true, got false: %+v", report)
	}
	if report.MaxConcurrency != 1 {
		t.Errorf("MaxConcurrency = %d, want 1", report.MaxConcurrency)
	}
	if len(report.Trajectories) != 4 {
		t.Fatalf("len(Trajectories) = %d, want 4", len(report.Trajectories))
	}

	wantPairs := []struct {
		inc     string
		harness string
	}{
		{"INC-101", "antigravity"},
		{"INC-102", "opencode"},
		{"INC-103", "pi"},
		{"INC-104", "multi"},
	}
	for i, want := range wantPairs {
		tr := report.Trajectories[i]
		if tr.IncidentID != want.inc || tr.HarnessMode != want.harness {
			t.Errorf("trajectory[%d] = (%s, %s), want (%s, %s)", i, tr.IncidentID, tr.HarnessMode, want.inc, want.harness)
		}
		if !tr.Passed || !tr.AlertDrainedAfterReset || tr.SourceProvenance != "LIVE" {
			t.Errorf("trajectory[%d] failed invariants: %+v", i, tr)
		}
	}

	if !report.DualExport.Passed || !report.DualExport.CloudLoggingVerified || !report.DualExport.ManagedPrometheusVerified {
		t.Errorf("expected DualExport.Passed=true, got %+v", report.DualExport)
	}
	if got := loggingHeaders.Get("X-Goog-User-Project"); got != "demo-project-id" {
		t.Errorf("Cloud Logging X-Goog-User-Project = %q, want %q", got, "demo-project-id")
	}
	if got := monitoringHeaders.Get("X-Goog-User-Project"); got != "demo-project-id" {
		t.Errorf("Cloud Monitoring X-Goog-User-Project = %q, want %q", got, "demo-project-id")
	}

	// Verify report file permissions (0600) and stage-safe scrubbing
	st, err := os.Stat(tmpReportPath)
	if err != nil {
		t.Fatalf("stat report file %s: %v", tmpReportPath, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("report file permissions = %04o, want 0600", perm)
	}
	rawBytes, err := os.ReadFile(tmpReportPath)
	if err != nil {
		t.Fatalf("reading report file: %v", err)
	}
	rawReport := string(rawBytes)
	for _, forbidden := range []string{unsafeClusterToken, unsafeProjectToken, unsafeNumberToken} {
		if strings.Contains(strings.ToLower(rawReport), strings.ToLower(forbidden)) {
			t.Errorf("scrubbed report %s still contains forbidden token %q", tmpReportPath, forbidden)
		}
	}

	// Verify fail-closed rejection when any child task returns SourceProvenance == "FIXTURE"
	mu.Lock()
	forceFixture = true
	latestTree = nil
	trees = nil
	mu.Unlock()

	failedReport, err := evals.RunLiveGKEChaosSoak(ctx, evals.LiveGKEEvalConfig{
		ObserverURL:        observerSrv.URL,
		StorefrontURL:      storefrontSrv.URL,
		StorefrontGitURL:   storefrontSrv.URL,
		PrometheusURL:      promSrv.URL,
		ProjectID:          "demo-project-id",
		AccessToken:        "test-oauth2-bearer-token",
		CloudLoggingURL:    cloudLoggingSrv.URL,
		CloudMonitoringURL: cloudMonitoringSrv.URL,
		ReportPath:         filepath.Join(t.TempDir(), "failed-fixture-report.json"),
		TrajectoryTimeout:  10 * time.Second,
		AlertDrainTimeout:  5 * time.Second,
		DualExportTimeout:  5 * time.Second,
		PollInterval:       25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunLiveGKEChaosSoak(forceFixture) unexpected error: %v", err)
	}
	if failedReport.AllPassed || failedReport.PassedTrajectories != 0 {
		t.Fatalf("expected fail-closed AllPassed=false and PassedTrajectories=0 when SourceProvenance=FIXTURE, got %+v", failedReport)
	}
}

func TestLiveGKEChaosSoak(t *testing.T) {
	TestLiveGKEFourByFourChaosSoakAndDualExport(t)
}

func TestLiveGKEFourByFourChaosSoakAndDualExport(t *testing.T) {
	if os.Getenv("AX_LIVE_GKE_EVAL") != "1" {
		t.Skip("skipping live GKE 4x4 chaos soak and dual-export verification (set AX_LIVE_GKE_EVAL=1 or run `make eval-live-gke`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()

	report, err := evals.RunLiveGKEChaosSoak(ctx, evals.LiveGKEEvalConfig{
		ReportPath: evals.DefaultLiveGKEReportPath,
	})
	if err != nil {
		t.Fatalf("RunLiveGKEChaosSoak failed: %v", err)
	}
	if !report.AllPassed {
		for _, tr := range report.Trajectories {
			if !tr.Passed {
				t.Errorf("live trajectory (%s, harness=%s) failed: %v", tr.IncidentID, tr.HarnessMode, tr.FailureReasons)
			}
		}
		if !report.DualExport.Passed {
			t.Errorf("GCP dual-export verification failed: %v", report.DualExport.FailureReasons)
		}
		t.Fatalf("expected AllPassed=true, got %d/%d trajectories passed and DualExport.Passed=%v",
			report.PassedTrajectories, report.TotalTrajectories, report.DualExport.Passed)
	}
}




