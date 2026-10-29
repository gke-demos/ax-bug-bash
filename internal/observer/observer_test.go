package observer_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/monitoring"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
)

func TestPathBasedRoutingAndCleanUI(t *testing.T) {
	ctx := context.Background()
	srv, err := observer.NewServer(ctx, observer.Config{})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, route := range []string{"/", "/workers", "/about"} {
		resp, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s failed: %v", route, err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", route, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", route, ct)
		}

		html := string(bodyBytes)
		for _, required := range []string{
			"AX Bug Bash Observer",
			"btn-theme-toggle",
			"btn-cmd-k",
			"cmd-k-modal",
			`href="/"`,
			`href="/workers"`,
			`href="/about"`,
			"INC-101",
			"INC-102",
			"INC-103",
			"INC-104",
			"antigravity",
			"opencode",
			"pi",
			`data-about-tab="topology"`,
			`data-about-tab="lifecycle"`,
			`data-about-tab="security"`,
			"about-topology-svg",
			"about-lifecycle-svg",
			"about-security-svg",
			"btn-launch-from-about",
		} {
			if !strings.Contains(html, required) {
				t.Errorf("GET %s missing expected UI element %q", route, required)
			}
		}
		if strings.Contains(html, "?view=") {
			t.Errorf("GET %s must use path-based routing, found ?view= query routing", route)
		}
	}

	// Verify static CSS and JS assets serve cleanly and contain zero innerHTML or emojis
	for _, asset := range []string{"/static/styles.css", "/static/app.js"} {
		resp, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatalf("GET %s failed: %v", asset, err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", asset, resp.StatusCode)
		}
		content := string(bodyBytes)
		if asset == "/static/app.js" && strings.Contains(content, "innerHTML") {
			t.Errorf("%s must not use innerHTML", asset)
		}
		if asset == "/static/styles.css" && !strings.Contains(content, `[data-theme="light"]`) {
			t.Errorf("%s must define [data-theme=\"light\"] projector mode tokens", asset)
		}
	}

	// Verify unknown paths return 404 Not Found
	for _, unknownPath := range []string{"/nonexistent", "/foo/bar", "/about/extra", "/workers/extra"} {
		resp, err := http.Get(ts.URL + unknownPath)
		if err != nil {
			t.Fatalf("GET %s failed: %v", unknownPath, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 Not Found", unknownPath, resp.StatusCode)
		}
	}

	// Verify query-parameter view routing (?view=...) is rejected with 400 Bad Request
	for _, badQuery := range []string{"/?view=about", "/workers?view=1", "/about?view=topology"} {
		resp, err := http.Get(ts.URL + badQuery)
		if err != nil {
			t.Fatalf("GET %s failed: %v", badQuery, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400 Bad Request", badQuery, resp.StatusCode)
		}
	}
}

func TestTriggerIncidentAndStateSnapshot(t *testing.T) {
	ctx := context.Background()
	srv, err := observer.NewServer(ctx, observer.Config{
		StorefrontPublicURL: "http://localhost:3001",
		GrafanaPublicURL:    "http://localhost:3002/monitoring",
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Trigger INC-101 in multi-harness parallel mode via POST /api/incidents/trigger
	triggerBody, _ := json.Marshal(observer.TriggerIncidentRequest{
		IncidentID: "INC-101",
		Harness:    "multi",
	})
	resp, err := http.Post(ts.URL+"/api/incidents/trigger", "application/json", bytes.NewReader(triggerBody))
	if err != nil {
		t.Fatalf("POST /api/incidents/trigger failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/incidents/trigger status = %d, want 200", resp.StatusCode)
	}

	// Fetch GET /api/state and verify Bug Bash Tree, Child Tasks, WorkerPool, and Rollout
	stateResp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state failed: %v", err)
	}
	var snap observer.StateSnapshot
	if err := json.NewDecoder(stateResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding StateSnapshot failed: %v", err)
	}
	_ = stateResp.Body.Close()

	if snap.ExternalLinks.StorefrontURL != "http://localhost:3001" {
		t.Errorf("StorefrontURL = %q, want http://localhost:3001", snap.ExternalLinks.StorefrontURL)
	}
	if snap.ExternalLinks.GrafanaURL != "http://localhost:3002/monitoring" {
		t.Errorf("GrafanaURL = %q, want http://localhost:3002/monitoring", snap.ExternalLinks.GrafanaURL)
	}
	if len(snap.Services) != 5 {
		t.Fatalf("len(Services) = %d, want 5 polyglot services", len(snap.Services))
	}
	if snap.LatestTree == nil {
		t.Fatalf("LatestTree is nil after triggering INC-101")
	}
	if snap.LatestTree.IncidentID != "INC-101" {
		t.Errorf("LatestTree.IncidentID = %q, want INC-101", snap.LatestTree.IncidentID)
	}
	if len(snap.LatestTree.ChildTasks) != 2 {
		t.Fatalf("len(ChildTasks) = %d, want 2 (pricing-service and shipping-service)", len(snap.LatestTree.ChildTasks))
	}

	seenHarnesses := make(map[string]bool)
	for _, ch := range snap.LatestTree.ChildTasks {
		seenHarnesses[ch.Harness] = true
		if ch.TaskConfig == nil || ch.TaskConfig["service"] != ch.Service {
			t.Errorf("child %s missing Task.spec.config service field: %+v", ch.TaskName, ch.TaskConfig)
		}
		if !strings.Contains(ch.RegressionTestDiff, ch.RegressionTestName) {
			t.Errorf("child %s RegressionTestDiff missing test %s", ch.TaskName, ch.RegressionTestName)
		}
		if ch.CodeFixDiff == "" {
			t.Errorf("child %s CodeFixDiff is empty", ch.TaskName)
		}
		if ch.RedTestOutput == "" || ch.GreenTestOutput == "" {
			t.Errorf("child %s missing Red/Green test outputs", ch.TaskName)
		}
		if len(ch.TDDSteps) != 4 {
			t.Errorf("child %s len(TDDSteps) = %d, want 4", ch.TaskName, len(ch.TDDSteps))
		}
		if ch.Phase != "Suspended" || !ch.Suspended {
			t.Errorf("child %s Phase = %q, Suspended = %v, want Suspended/true", ch.TaskName, ch.Phase, ch.Suspended)
		}
		if ch.SlotReclamationMs <= 0 {
			t.Errorf("child %s SlotReclamationMs = %d, want > 0", ch.TaskName, ch.SlotReclamationMs)
		}
	}
	if len(seenHarnesses) < 2 {
		t.Errorf("multi-harness mode expected distinct harnesses across sibling Child Tasks, got %v", seenHarnesses)
	}

	if len(snap.WorkerPool.Workers) != 4 {
		t.Errorf("len(WorkerPool.Workers) = %d, want 4 ateworker-gvisor nodes", len(snap.WorkerPool.Workers))
	}
	if snap.WorkerPool.TotalReclaimedSlots < 3 {
		t.Errorf("TotalReclaimedSlots = %d, want >= 3 (root + 2 children)", snap.WorkerPool.TotalReclaimedSlots)
	}
	if len(snap.VolumeSnapshots) < 2 {
		t.Errorf("len(VolumeSnapshots) = %d, want >= 2", len(snap.VolumeSnapshots))
	}
	if snap.LatestRollout == nil || !snap.LatestRollout.UnderTenSecondSLA || !snap.LatestRollout.StorefrontCheckoutVerified {
		t.Errorf("LatestRollout = %+v, want UnderTenSecondSLA=true and StorefrontCheckoutVerified=true", snap.LatestRollout)
	}

	// Verify POST /api/incidents/reset
	resetResp, err := http.Post(ts.URL+"/api/incidents/reset", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST /api/incidents/reset failed: %v", err)
	}
	_ = resetResp.Body.Close()
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/incidents/reset status = %d, want 200", resetResp.StatusCode)
	}

	// Verify RANDOM incident trigger with single harness ("opencode")
	rndBody, _ := json.Marshal(observer.TriggerIncidentRequest{
		IncidentID: "RANDOM",
		Harness:    "opencode",
	})
	rndResp, err := http.Post(ts.URL+"/api/incidents/trigger", "application/json", bytes.NewReader(rndBody))
	if err != nil {
		t.Fatalf("POST /api/incidents/trigger (RANDOM) failed: %v", err)
	}
	var rndPayload struct {
		Status     string                 `json:"status"`
		IncidentID string                 `json:"incident_id"`
		Harness    string                 `json:"harness"`
		State      observer.StateSnapshot `json:"state"`
	}
	if err := json.NewDecoder(rndResp.Body).Decode(&rndPayload); err != nil {
		t.Fatalf("decoding RANDOM trigger response failed: %v", err)
	}
	_ = rndResp.Body.Close()
	if rndResp.StatusCode != http.StatusOK || rndPayload.Status != "triggered" {
		t.Fatalf("RANDOM trigger status = %d (%q), want 200/triggered", rndResp.StatusCode, rndPayload.Status)
	}
	if rndPayload.State.LatestTree == nil {
		t.Fatalf("LatestTree nil after RANDOM trigger")
	}
	for _, ch := range rndPayload.State.LatestTree.ChildTasks {
		if ch.Harness != "opencode" {
			t.Errorf("expected child %s harness = opencode, got %q", ch.TaskName, ch.Harness)
		}
	}
}

func TestRealTimeSSEEventStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv, err := observer.NewServer(ctx, observer.Config{})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	sseResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events failed: %v", err)
	}
	defer sseResp.Body.Close()

	if ct := sseResp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("GET /api/events Content-Type = %q, want text/event-stream", ct)
	}

	eventsCh := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(sseResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				eventsCh <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()

	// Wait for initial state.snapshot event
	select {
	case ev := <-eventsCh:
		if ev != "state.snapshot" {
			t.Fatalf("first SSE event = %q, want state.snapshot", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for initial state.snapshot SSE event")
	}

	// Post Alertmanager v4 webhook for INC-104 (Lithium Hazmat Weight Tier)
	webhook := monitoring.AlertmanagerWebhookPayload{
		Version: "4",
		Status:  "firing",
		CommonLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  "INC-104",
			"harness":   "pi",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "StorefrontHTTP500Spike",
					"incident":  "INC-104",
					"harness":   "pi",
				},
				Fingerprint: "fp-sse-inc-104",
			},
		},
	}
	rawWebhook, _ := json.Marshal(webhook)
	wResp, err := http.Post(ts.URL+"/api/webhooks/alertmanager", "application/json", bytes.NewReader(rawWebhook))
	if err != nil {
		t.Fatalf("POST /api/webhooks/alertmanager failed: %v", err)
	}
	_ = wResp.Body.Close()
	if wResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/webhooks/alertmanager status = %d, want 200", wResp.StatusCode)
	}

	seenEvents := make(map[string]bool)
	deadline := time.After(30 * time.Second)
	for !(seenEvents["alert.fired"] && seenEvents["tree.updated"] && seenEvents["rollout.completed"]) {
		select {
		case ev := <-eventsCh:
			seenEvents[ev] = true
		case <-deadline:
			t.Fatalf("timed out waiting for SSE events; seen = %v", seenEvents)
		}
	}
}

func TestLiveObserverCallbackEventsAndSSE(t *testing.T) {
	ctx := context.Background()
	srv, err := observer.NewServer(ctx, observer.Config{})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	events := []bugbash.ObserverCallbackEvent{
		{
			ExecutionID:        "tree-live-inc-103-1",
			RootTaskName:       "triage-inc-103-1",
			IncidentID:         "INC-103",
			EventType:          "root_triage_started",
			HarnessMode:        "antigravity",
			RequestedModel:     "gemini-3.8-flash",
			ImplicatedServices: []string{"inventory-service", "checkout-service"},
			UnaffectedServices: []string{"storefront-web", "pricing-service", "shipping-service"},
		},
		{
			ExecutionID:  "tree-live-inc-103-1",
			RootTaskName: "triage-inc-103-1",
			IncidentID:   "INC-103",
			EventType:    "child_fixer_spawned",
			Child: &bugbash.ChildTaskResult{
				TaskName:       "child-inc-103-inventory-service",
				ParentTask:     "triage-inc-103-1",
				IncidentID:     "INC-103",
				Service:        "inventory-service",
				Language:       "Rust",
				Harness:        "antigravity",
				RequestedModel: "gemini-3.8-flash",
			},
		},
		{
			ExecutionID:  "tree-live-inc-103-1",
			RootTaskName: "triage-inc-103-1",
			TaskName:     "child-inc-103-inventory-service",
			Service:      "inventory-service",
			IncidentID:   "INC-103",
			EventType:    "child_fixer_attempt",
			Step: &bugbash.TDDStepEvent{
				Phase:      "REPRODUCE_RED",
				Status:     "COMPLETED",
				Summary:    "Reproducer test failed (Red) as expected",
				DurationMs: 18,
			},
		},
		{
			ExecutionID:  "tree-live-inc-103-1",
			RootTaskName: "triage-inc-103-1",
			IncidentID:   "INC-103",
			EventType:    "child_fixer_completed",
			Child: &bugbash.ChildTaskResult{
				TaskName:           "child-inc-103-inventory-service",
				ParentTask:         "triage-inc-103-1",
				IncidentID:         "INC-103",
				Service:            "inventory-service",
				Language:           "Rust",
				Harness:            "antigravity",
				RequestedModel:     "gemini-3.8-flash",
				ActiveModel:        "gemini-3.8-flash",
				SourceProvenance:   "LIVE",
				RegressionTestName: "test_bundle_component_shortage_returns_err",
				RegressionTestDiff: "+fn test_bundle_component_shortage_returns_err() {}",
				CodeFixDiff:        "+if available < required { return Err(...); }",
				RedTestOutput:      "FAILED test_bundle_component_shortage_returns_err",
				GreenTestOutput:    "test result: ok. 4 passed; 0 failed",
				Suspended:          true,
			},
		},
		{
			ExecutionID:       "tree-live-inc-103-1",
			RootTaskName:      "triage-inc-103-1",
			IncidentID:        "INC-103",
			EventType:         "root_merge_completed",
			SourceProvenance:  "LIVE",
			MergedCommitSHA:   "deadbeefcafe1234",
			MergedUnifiedDiff: "diff --git a/services/inventory-service/src/lib.rs b/services/inventory-service/src/lib.rs",
			RolloutDurationMs: 620,
			StorefrontHealed:  true,
		},
	}

	for i, ev := range events {
		endpoint := "/api/observer/events"
		if i%2 == 1 {
			endpoint = "/api/internal/observer-events"
		}
		raw, _ := json.Marshal(ev)
		resp, err := http.Post(ts.URL+endpoint, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("POST %s (step %d) failed: %v", endpoint, i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s (step %d) status = %d, want 200", endpoint, i, resp.StatusCode)
		}
	}

	stateResp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state failed: %v", err)
	}
	var snap observer.StateSnapshot
	if err := json.NewDecoder(stateResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding StateSnapshot failed: %v", err)
	}
	_ = stateResp.Body.Close()

	if snap.LatestTree == nil {
		t.Fatalf("LatestTree is nil after posting callback events")
	}
	if snap.LatestTree.ExecutionID != "tree-live-inc-103-1" {
		t.Errorf("LatestTree.ExecutionID = %q, want tree-live-inc-103-1", snap.LatestTree.ExecutionID)
	}
	if snap.LatestTree.Phase != "HEALED" {
		t.Errorf("LatestTree.Phase = %q, want HEALED", snap.LatestTree.Phase)
	}
	if snap.LatestTree.MergedGitCommitSHA != "deadbeefcafe1234" {
		t.Errorf("LatestTree.MergedGitCommitSHA = %q, want deadbeefcafe1234", snap.LatestTree.MergedGitCommitSHA)
	}
	if len(snap.LatestTree.ChildTasks) != 1 || snap.LatestTree.ChildTasks[0].SourceProvenance != "LIVE" {
		t.Errorf("LatestTree.ChildTasks = %+v, want 1 child with SourceProvenance=LIVE", snap.LatestTree.ChildTasks)
	}
}

func TestRuntimeHarnessConfigAndResetForwarding(t *testing.T) {
	ctx := context.Background()

	var resetHitCount int
	mockStorefront := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/incidents/reset", "/api/incidents/heal":
			resetHitCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"reset"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"entries":[]}`))
		}
	}))
	defer mockStorefront.Close()

	srv, err := observer.NewServer(ctx, observer.Config{
		StorefrontURL: mockStorefront.URL,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Update default_harness via POST /api/config/harness
	for _, validHarness := range []string{"antigravity", "opencode", "pi", "multi"} {
		body, _ := json.Marshal(map[string]string{"default_harness": validHarness})
		resp, err := http.Post(ts.URL+"/api/config/harness", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST /api/config/harness (%s) failed: %v", validHarness, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/config/harness (%s) status = %d, want 200", validHarness, resp.StatusCode)
		}
	}

	// Reject invalid harness name with 400 Bad Request
	badResp, err := http.Post(ts.URL+"/api/config/harness", "application/json", bytes.NewReader([]byte(`{"default_harness":"not-a-harness"}`)))
	if err != nil {
		t.Fatalf("POST /api/config/harness (invalid) failed: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /api/config/harness (invalid) status = %d, want 400", badResp.StatusCode)
	}

	// Verify POST /api/incidents/reset forwards to StorefrontURL and updates default_harness when provided
	resetBody := []byte(`{"default_harness":"pi"}`)
	resetResp, err := http.Post(ts.URL+"/api/incidents/reset", "application/json", bytes.NewReader(resetBody))
	if err != nil {
		t.Fatalf("POST /api/incidents/reset failed: %v", err)
	}
	_ = resetResp.Body.Close()
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/incidents/reset status = %d, want 200", resetResp.StatusCode)
	}
	if resetHitCount == 0 {
		t.Errorf("expected POST /api/incidents/reset to forward to remote StorefrontURL, got 0 hits")
	}

	getCfgResp, err := http.Get(ts.URL + "/api/config/harness")
	if err != nil {
		t.Fatalf("GET /api/config/harness failed: %v", err)
	}
	var cfgPayload struct {
		DefaultHarness string `json:"default_harness"`
	}
	_ = json.NewDecoder(getCfgResp.Body).Decode(&cfgPayload)
	_ = getCfgResp.Body.Close()
	if cfgPayload.DefaultHarness != "pi" {
		t.Errorf("GET /api/config/harness = %q, want pi", cfgPayload.DefaultHarness)
	}
}

func TestLiveGKEAsyncWebhookAndSyncWaitMode(t *testing.T) {
	ctx := context.Background()
	embeddedAX, axAddr, err := bugbash.StartEmbeddedAXServer(ctx)
	if err != nil {
		t.Fatalf("StartEmbeddedAXServer failed: %v", err)
	}
	defer embeddedAX.Stop()

	orch, err := bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
		ExecutionMode:    bugbash.ExecutionModeLiveGKE,
		AXServerAddr:     axAddr,
		TaskRunnerImage:  "us-central1-docker.pkg.dev/project/repo/ax-bug-bash-runner@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		StorefrontGitURL: "http://127.0.0.1:3001/git/storefront.git",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator(LIVE_GKE) failed: %v", err)
	}
	defer orch.Close()

	srv, err := observer.NewServer(ctx, observer.Config{
		Orchestrator: orch,
	})
	if err != nil {
		t.Fatalf("NewServer(LIVE_GKE) failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Default Alertmanager webhook in LIVE_GKE mode returns 202 Accepted immediately
	webhook := monitoring.AlertmanagerWebhookPayload{
		Version: "4",
		Status:  "firing",
		CommonLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  "INC-101",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontHTTP500Spike", "incident": "INC-101"},
				Fingerprint: "fp-live-inc-101",
			},
		},
	}
	rawWebhook, _ := json.Marshal(webhook)
	wResp, err := http.Post(ts.URL+"/api/webhooks/alertmanager", "application/json", bytes.NewReader(rawWebhook))
	if err != nil {
		t.Fatalf("POST /api/webhooks/alertmanager failed: %v", err)
	}
	var wBody monitoring.AlertmanagerWebhookResponse
	_ = json.NewDecoder(wResp.Body).Decode(&wBody)
	_ = wResp.Body.Close()

	if wResp.StatusCode != http.StatusAccepted {
		t.Fatalf("LIVE_GKE POST /api/webhooks/alertmanager status = %d, want 202 Accepted", wResp.StatusCode)
	}
	if wBody.ExecutionID == "" {
		t.Fatalf("expected non-empty ExecutionID in 202 Accepted response")
	}

	// Complete INC-101 via callback so INC-102 can be tested with ?wait=true
	completeEv := bugbash.ObserverCallbackEvent{
		ExecutionID:      wBody.ExecutionID,
		RootTaskName:     wBody.RootTaskName,
		IncidentID:       "INC-101",
		EventType:        "root_merge_completed",
		SourceProvenance: "LIVE",
		MergedCommitSHA:  "1111222233334444",
		StorefrontHealed: true,
	}
	rawComplete, _ := json.Marshal(completeEv)
	cResp, err := http.Post(ts.URL+"/api/observer/events", "application/json", bytes.NewReader(rawComplete))
	if err != nil {
		t.Fatalf("POST /api/observer/events failed: %v", err)
	}
	_ = cResp.Body.Close()

	// 2. ?wait=true on POST /api/incidents/trigger blocks until callback marks tree HEALED and returns 200 OK
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			sResp, err := http.Get(ts.URL + "/api/state")
			if err == nil {
				var st observer.StateSnapshot
				_ = json.NewDecoder(sResp.Body).Decode(&st)
				_ = sResp.Body.Close()
				if st.LatestTree != nil && st.LatestTree.IncidentID == "INC-102" && st.LatestTree.Phase != "HEALED" {
					mergeEv := bugbash.ObserverCallbackEvent{
						ExecutionID:      st.LatestTree.ExecutionID,
						RootTaskName:     st.LatestTree.RootTask.TaskName,
						IncidentID:       "INC-102",
						EventType:        "root_merge_completed",
						SourceProvenance: "LIVE",
						MergedCommitSHA:  "aaaabbbbccccdddd",
						StorefrontHealed: true,
					}
					rawMerge, _ := json.Marshal(mergeEv)
					resp, err := http.Post(ts.URL+"/api/observer/events", "application/json", bytes.NewReader(rawMerge))
					if err == nil {
						_ = resp.Body.Close()
					}
					return
				}
			}
			time.Sleep(40 * time.Millisecond)
		}
	}()

	trigBody, _ := json.Marshal(observer.TriggerIncidentRequest{
		IncidentID: "INC-102",
		Harness:    "opencode",
	})
	trigResp, err := http.Post(ts.URL+"/api/incidents/trigger?wait=true", "application/json", bytes.NewReader(trigBody))
	if err != nil {
		t.Fatalf("POST /api/incidents/trigger?wait=true failed: %v", err)
	}
	var trigResult struct {
		Status string                 `json:"status"`
		State  observer.StateSnapshot `json:"state"`
	}
	_ = json.NewDecoder(trigResp.Body).Decode(&trigResult)
	_ = trigResp.Body.Close()

	if trigResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/incidents/trigger?wait=true status = %d, want 200 OK", trigResp.StatusCode)
	}
	if trigResult.State.LatestTree == nil || trigResult.State.LatestTree.Phase != "HEALED" {
		t.Errorf("expected LatestTree.Phase == HEALED after ?wait=true, got %+v", trigResult.State.LatestTree)
	}
}

func TestLiveWorkersAndRemoteGatewayClientTelemetry(t *testing.T) {
	ctx := context.Background()

	gw := gateway.NewServer(gateway.DefaultConfig(), gateway.StaticTokenFunc("test-token"))
	gwTS := httptest.NewServer(gw.Handler())
	defer gwTS.Close()

	chatResp, err := http.Post(
		gwTS.URL+"/s/child-live-pricing/antigravity/v1/chat/completions",
		"application/json",
		bytes.NewReader([]byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"fix INC-101"}]}`)),
	)
	if err != nil {
		t.Fatalf("POST gateway chat completions failed: %v", err)
	}
	_ = chatResp.Body.Close()

	srv, err := observer.NewServer(ctx, observer.Config{
		ModelGatewayURL: gwTS.URL,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wResp, err := http.Get(ts.URL + "/api/workers")
	if err != nil {
		t.Fatalf("GET /api/workers failed: %v", err)
	}
	var wPayload struct {
		ExecutionMode string                        `json:"execution_mode"`
		Workers       []bugbash.SubstrateWorkerNode `json:"workers"`
	}
	if err := json.NewDecoder(wResp.Body).Decode(&wPayload); err != nil {
		t.Fatalf("decoding GET /api/workers failed: %v", err)
	}
	_ = wResp.Body.Close()
	if wResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/workers status = %d, want 200", wResp.StatusCode)
	}
	if len(wPayload.Workers) != 4 {
		t.Errorf("len(wPayload.Workers) = %d, want 4", len(wPayload.Workers))
	}

	sResp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state failed: %v", err)
	}
	var snap observer.StateSnapshot
	_ = json.NewDecoder(sResp.Body).Decode(&snap)
	_ = sResp.Body.Close()
	if len(snap.Gateway.Sessions) == 0 {
		t.Errorf("expected remote ModelGatewayURL sessions in StateSnapshot.Gateway.Sessions, got 0")
	}
}

