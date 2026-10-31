package evals_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/evals"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func containsEmojiRune(s string) (rune, bool) {
	for _, r := range s {
		if (r >= 0x1F300 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF && unicode.IsSymbol(r) && r != 0x2600 && r != 0x263E) {
			if r >= 0x1F300 {
				return r, true
			}
		}
	}
	return 0, false
}

// TestSmokeSuite executes fast (<5s) hermetic smoke checks across all 3 decoupled surfaces,
// the Model Gateway fallback cascade, SSE streaming, Storefront checkout/incident lifecycle,
// and --dry-run deployment/warmup scripts.
func TestSmokeSuite(t *testing.T) {
	suiteStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sf, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("creating storefront runtime: %v", err)
	}
	sfSrv := httptest.NewServer(sf.Handler())
	defer sfSrv.Close()

	gwCfg := gateway.DefaultConfig()
	gwCfg.EnableFixtureFallback = true
	gw := gateway.NewServer(gwCfg, func(context.Context) (string, error) {
		return "", nil
	})
	gwSrv := httptest.NewServer(gw.Handler())
	defer gwSrv.Close()

	obsSrv, err := observer.NewServer(ctx, observer.Config{
		Storefront:          sf,
		StorefrontURL:       sfSrv.URL,
		StorefrontPublicURL: sfSrv.URL,
		ModelGateway:        gw,
		ModelGatewayURL:     gwSrv.URL,
	})
	if err != nil {
		t.Fatalf("creating observer server: %v", err)
	}
	defer obsSrv.Close()

	obsHTTP := httptest.NewServer(obsSrv.Handler())
	defer obsHTTP.Close()

	monHTTP := httptest.NewServer(obsSrv.Scraper().Handler())
	defer monHTTP.Close()

	client := &http.Client{Timeout: 4 * time.Second}

	t.Run("SurfaceHealthAndReadiness", func(t *testing.T) {
		endpoints := []struct {
			name string
			url  string
		}{
			{name: "storefront /healthz", url: sfSrv.URL + "/healthz"},
			{name: "storefront /readyz", url: sfSrv.URL + "/readyz"},
			{name: "model-gateway /healthz", url: gwSrv.URL + "/healthz"},
			{name: "model-gateway /readyz", url: gwSrv.URL + "/readyz"},
			{name: "ax-observer /healthz", url: obsHTTP.URL + "/healthz"},
			{name: "ax-observer /readyz", url: obsHTTP.URL + "/readyz"},
			{name: "monitoring /healthz", url: monHTTP.URL + "/healthz"},
			{name: "monitoring /monitoring", url: monHTTP.URL + "/monitoring"},
		}
		for _, ep := range endpoints {
			resp, err := client.Get(ep.url)
			if err != nil {
				t.Fatalf("%s request failed: %v", ep.name, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s status = %d, want 200 OK", ep.name, resp.StatusCode)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s X-Content-Type-Options = %q, want nosniff", ep.name, got)
			}
		}
	})

	t.Run("ObserverUIRoutesAndProjectorLightMode", func(t *testing.T) {
		for _, route := range []string{"/", "/workers", "/about"} {
			resp, err := client.Get(obsHTTP.URL + route)
			if err != nil {
				t.Fatalf("GET %s failed: %v", route, err)
			}
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200 OK", route, resp.StatusCode)
			}
			html := string(bodyBytes)
			if !strings.Contains(html, "theme-toggle") && !strings.Contains(html, "btn-theme-light") {
				t.Errorf("GET %s HTML missing Projector Light Mode toggle control", route)
			}
			if !strings.Contains(html, "<svg") {
				t.Errorf("GET %s HTML missing inline SVG icons", route)
			}
			if r, hasEmoji := containsEmojiRune(html); hasEmoji {
				t.Errorf("GET %s HTML contains forbidden emoji rune U+%04X", route, r)
			}
		}

		// Query-parameter routing must be rejected with HTTP 400
		badResp, err := client.Get(obsHTTP.URL + "/?view=workers")
		if err != nil {
			t.Fatalf("GET /?view=workers failed: %v", err)
		}
		_ = badResp.Body.Close()
		if badResp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /?view=workers status = %d, want 400 Bad Request", badResp.StatusCode)
		}
	})

	t.Run("AboutCatalog14ADRContract", func(t *testing.T) {
		resp, err := client.Get(obsHTTP.URL + "/api/about/catalog")
		if err != nil {
			t.Fatalf("GET /api/about/catalog failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/about/catalog status = %d, want 200", resp.StatusCode)
		}
		var cat observer.ArchitectureTopologyCatalog
		if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
			t.Fatalf("decoding ArchitectureTopologyCatalog: %v", err)
		}
		if len(cat.ADRs) != 14 {
			t.Errorf("len(ADRs) = %d, want 14 (ADR-0001..ADR-0014)", len(cat.ADRs))
		}
		if len(cat.Nodes) != 12 {
			t.Errorf("len(Nodes) = %d, want 12", len(cat.Nodes))
		}
		if len(cat.Edges) != 11 {
			t.Errorf("len(Edges) = %d, want 11", len(cat.Edges))
		}
		if len(cat.Services) != 5 {
			t.Errorf("len(Services) = %d, want 5", len(cat.Services))
		}
		if len(cat.Scenarios) != 4 {
			t.Errorf("len(Scenarios) = %d, want 4", len(cat.Scenarios))
		}
		if len(cat.Harnesses) != 3 {
			t.Errorf("len(Harnesses) = %d, want 3", len(cat.Harnesses))
		}
		if len(cat.CascadeModels) != 4 {
			t.Errorf("len(CascadeModels) = %d, want 4", len(cat.CascadeModels))
		}
		if cat.StatusStrip.StorefrontBaselineTag != "baseline-v1" {
			t.Errorf("StorefrontBaselineTag = %q, want baseline-v1", cat.StatusStrip.StorefrontBaselineTag)
		}
	})

	t.Run("WorkersAndTreeAndStateAPIs", func(t *testing.T) {
		wResp, err := client.Get(obsHTTP.URL + "/api/workers")
		if err != nil {
			t.Fatalf("GET /api/workers failed: %v", err)
		}
		defer wResp.Body.Close()
		if wResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/workers status = %d, want 200", wResp.StatusCode)
		}
		var workers observer.WorkersResponse
		if err := json.NewDecoder(wResp.Body).Decode(&workers); err != nil {
			t.Fatalf("decoding WorkersResponse: %v", err)
		}
		if len(workers.Workers) < 4 || workers.ReadyWarmSlots < 4 {
			t.Errorf("expected >= 4 ready warm slots on /api/workers, got workers=%d readyWarm=%d", len(workers.Workers), workers.ReadyWarmSlots)
		}

		tResp, err := client.Get(obsHTTP.URL + "/api/tree")
		if err != nil {
			t.Fatalf("GET /api/tree failed: %v", err)
		}
		defer tResp.Body.Close()
		if tResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/tree status = %d, want 200", tResp.StatusCode)
		}
		var treePayload map[string]any
		if err := json.NewDecoder(tResp.Body).Decode(&treePayload); err != nil {
			t.Fatalf("decoding /api/tree JSON: %v", err)
		}
		if _, ok := treePayload["execution_mode"]; !ok {
			t.Errorf("/api/tree missing execution_mode key: %+v", treePayload)
		}

		sResp, err := client.Get(obsHTTP.URL + "/api/state")
		if err != nil {
			t.Fatalf("GET /api/state failed: %v", err)
		}
		defer sResp.Body.Close()
		if sResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/state status = %d, want 200", sResp.StatusCode)
		}
		var snap observer.StateSnapshot
		if err := json.NewDecoder(sResp.Body).Decode(&snap); err != nil {
			t.Fatalf("decoding StateSnapshot: %v", err)
		}
		if len(snap.Services) != 5 {
			t.Errorf("len(StateSnapshot.Services) = %d, want 5", len(snap.Services))
		}
	})

	t.Run("SSEEventsStream", func(t *testing.T) {
		sseCtx, sseCancel := context.WithTimeout(ctx, 3*time.Second)
		defer sseCancel()

		req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, obsHTTP.URL+"/api/events", nil)
		if err != nil {
			t.Fatalf("creating SSE request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/events failed: %v", err)
		}
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("GET /api/events Content-Type = %q, want text/event-stream", ct)
		}

		scanner := bufio.NewScanner(resp.Body)
		foundEvent := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: state.snapshot") || strings.HasPrefix(line, "data: {") {
				foundEvent = true
				break
			}
		}
		if !foundEvent {
			t.Errorf("did not receive initial state.snapshot SSE frame from /api/events")
		}
	})

	t.Run("ModelGatewayCascade38To35ToFixture", func(t *testing.T) {
		// 1. Verify 3.8 (429) -> 3.7 (503) -> 3.6 (429) -> 3.5 (200 LIVE)
		upstream35 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer smoke-wi-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			for _, m := range []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash"} {
				if strings.Contains(r.URL.Path, "/models/"+m+":") {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"code":429,"message":"capacity exhausted"}}`))
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"healed via 3.5"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}`))
		}))
		defer upstream35.Close()

		cfgLive := gateway.DefaultConfig()
		cfgLive.AgentPlatformBaseURL = upstream35.URL
		cfgLive.EnableFixtureFallback = true
		gwLive := gateway.NewServer(cfgLive, gateway.StaticTokenFunc("smoke-wi-token"))
		gwLiveSrv := httptest.NewServer(gwLive.Handler())
		defer gwLiveSrv.Close()

		chatBody := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"smoke test cascade"}]}`)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwLiveSrv.URL+"/s/smoke-task-1/v1/chat/completions", bytes.NewReader(chatBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+gateway.SandboxTokenPlaceholder)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /s/smoke-task-1/v1/chat/completions failed: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cascade 3.8->3.5 status = %d, want 200", resp.StatusCode)
		}
		if gotModel := resp.Header.Get("X-AX-Active-Model"); gotModel != "gemini-3.5-flash" {
			t.Errorf("X-AX-Active-Model = %q, want gemini-3.5-flash", gotModel)
		}
		if gotProv := resp.Header.Get("X-AX-Source-Provenance"); gotProv != "LIVE" {
			t.Errorf("X-AX-Source-Provenance = %q, want LIVE", gotProv)
		}

		// 2. Verify full 3.8 -> 3.7 -> 3.6 -> 3.5 exhaustion falls back to deterministic FIXTURE
		upstreamAllDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"all tiers unavailable"}}`))
		}))
		defer upstreamAllDown.Close()

		cfgFix := gateway.DefaultConfig()
		cfgFix.AgentPlatformBaseURL = upstreamAllDown.URL
		cfgFix.EnableFixtureFallback = true
		gwFix := gateway.NewServer(cfgFix, gateway.StaticTokenFunc("smoke-wi-token"))
		gwFixSrv := httptest.NewServer(gwFix.Handler())
		defer gwFixSrv.Close()

		reqFix, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwFixSrv.URL+"/s/smoke-task-fix/v1/chat/completions", bytes.NewReader(chatBody))
		reqFix.Header.Set("Content-Type", "application/json")
		reqFix.Header.Set("Authorization", "Bearer "+gateway.SandboxTokenPlaceholder)
		respFix, err := client.Do(reqFix)
		if err != nil {
			t.Fatalf("fixture fallback request failed: %v", err)
		}
		_ = respFix.Body.Close()
		if respFix.StatusCode != http.StatusOK {
			t.Fatalf("fixture fallback status = %d, want 200", respFix.StatusCode)
		}
		if gotProv := respFix.Header.Get("X-AX-Source-Provenance"); gotProv != "FIXTURE" {
			t.Errorf("X-AX-Source-Provenance after 4-tier drop = %q, want FIXTURE", gotProv)
		}
	})

	t.Run("StorefrontHealthMetricsCheckoutAndIncidentLifecycle", func(t *testing.T) {
		mResp, err := client.Get(sfSrv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics failed: %v", err)
		}
		mBytes, _ := io.ReadAll(mResp.Body)
		_ = mResp.Body.Close()
		if !strings.Contains(string(mBytes), "storefront_http_requests_total") {
			t.Errorf("/metrics missing storefront_http_requests_total")
		}

		happyCart := []byte(`{"currency":"USD","items":[{"sku":"sku-cable-usbc","quantity":1}]}`)
		for _, ep := range []string{"/api/cart/checkout", "/api/checkout"} {
			cResp, err := client.Post(sfSrv.URL+ep, "application/json", bytes.NewReader(happyCart))
			if err != nil {
				t.Fatalf("POST %s failed: %v", ep, err)
			}
			var co storefront.CheckoutResponse
			_ = json.NewDecoder(cResp.Body).Decode(&co)
			_ = cResp.Body.Close()
			if cResp.StatusCode != http.StatusOK || co.Status != "CONFIRMED" {
				t.Fatalf("happy-path POST %s status=%d (%s), want 200 CONFIRMED", ep, cResp.StatusCode, co.Status)
			}
			if reqID := cResp.Header.Get("X-Storefront-Request-ID"); reqID == "" {
				t.Errorf("POST %s missing X-Storefront-Request-ID response header", ep)
			}
		}

		// Activate INC-101, verify 500 on checkout, then reset and verify 200 OK
		actResp, err := client.Post(sfSrv.URL+"/api/incidents/activate", "application/json", bytes.NewReader([]byte(`{"incident_id":"INC-101"}`)))
		if err != nil {
			t.Fatalf("POST /api/incidents/activate failed: %v", err)
		}
		_ = actResp.Body.Close()
		if actResp.StatusCode != http.StatusOK {
			t.Fatalf("activate INC-101 status = %d, want 200", actResp.StatusCode)
		}

		sc, _ := storefront.GetScenario("INC-101")
		trigCart, _ := json.Marshal(sc.TriggerCart)
		failResp, err := client.Post(sfSrv.URL+"/api/checkout", "application/json", bytes.NewReader(trigCart))
		if err != nil {
			t.Fatalf("POST /api/checkout (INC-101) failed: %v", err)
		}
		_ = failResp.Body.Close()
		if failResp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected HTTP 500 when INC-101 is active, got %d", failResp.StatusCode)
		}

		rstResp, err := client.Post(sfSrv.URL+"/api/incidents/reset", "application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("POST /api/incidents/reset failed: %v", err)
		}
		_ = rstResp.Body.Close()
		if rstResp.StatusCode != http.StatusOK {
			t.Fatalf("reset status = %d, want 200", rstResp.StatusCode)
		}

		healedResp, err := client.Post(sfSrv.URL+"/api/checkout", "application/json", bytes.NewReader(happyCart))
		if err != nil {
			t.Fatalf("post-reset checkout failed: %v", err)
		}
		_ = healedResp.Body.Close()
		if healedResp.StatusCode != http.StatusOK {
			t.Fatalf("post-reset checkout status = %d, want 200", healedResp.StatusCode)
		}
	})

	t.Run("HackScriptsDryRunSmoke", func(t *testing.T) {
		repoRoot, err := filepath.Abs("../..")
		if err != nil {
			t.Fatalf("resolving repoRoot: %v", err)
		}
		scripts := []struct {
			name string
			cmd  []string
			want string
		}{
			{
				name: "deploy-gke.sh --dry-run",
				cmd:  []string{"bash", "./hack/deploy-gke.sh", "--dry-run"},
				want: "[DRY-RUN] Manifest rendering and @sha256: digest verification succeeded.",
			},
			{
				name: "stage-warmup.sh --dry-run",
				cmd:  []string{"bash", "./hack/stage-warmup.sh", "--dry-run"},
				want: "[STAGE-WARMUP] Dry-run verification",
			},
			{
				name: "run-local.sh --dry-run",
				cmd:  []string{"bash", "./hack/run-local.sh", "--dry-run"},
				want: "[DRY-RUN] Local 3-surface launcher validated",
			},
			{
				name: "record-stage-walkthrough.sh --dry-run",
				cmd:  []string{"bash", "./hack/record-stage-walkthrough.sh", "--dry-run"},
				want: "[DRY-RUN] 6-Scene Stage Walkthrough recorder validated",
			},
			{
				name: "record-terminal-demo.py --dry-run",
				cmd:  []string{"python3", "./hack/record-terminal-demo.py", "--dry-run"},
				want: "[DRY-RUN] Terminal demo GIF recorder validated",
			},
		}
		for _, sc := range scripts {
			c := exec.CommandContext(ctx, sc.cmd[0], sc.cmd[1:]...)
			c.Dir = repoRoot
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", sc.name, err, string(out))
			}
			if !strings.Contains(string(out), sc.want) {
				t.Errorf("%s output = %q, want substring %q", sc.name, string(out), sc.want)
			}
		}
	})

	if elapsed := time.Since(suiteStart); elapsed > 5*time.Second {
		t.Errorf("TestSmokeSuite took %s, want < 5s", elapsed)
	}
}

// TestEndToEndIntegrationSuite executes the complete hermetic E2E integration and 20-trajectory
// evaluation matrix, callback lifecycle verification, and repository hygiene/manifest contracts.
func TestEndToEndIntegrationSuite(t *testing.T) {
	t.Run("TwentyTrajectoryMatrixAndLiveCascade", func(t *testing.T) {
		TestFourScenariosAcrossAllHarnessesAndLiveCascade(t)
	})

	t.Run("ObserverCallbackAndRolloutE2ELifecycle", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sf, err := storefront.NewRuntime(storefront.Config{GitWorkDir: t.TempDir()})
		if err != nil {
			t.Fatalf("creating storefront runtime: %v", err)
		}
		sfSrv := httptest.NewServer(sf.Handler())
		defer sfSrv.Close()

		obsSrv, err := observer.NewServer(ctx, observer.Config{
			Storefront:          sf,
			StorefrontURL:       sfSrv.URL,
			StorefrontPublicURL: sfSrv.URL,
		})
		if err != nil {
			t.Fatalf("creating observer server: %v", err)
		}
		defer obsSrv.Close()

		obsHTTP := httptest.NewServer(obsSrv.Handler())
		defer obsHTTP.Close()

		events := []bugbash.ObserverCallbackEvent{
			{
				EventType:          "root_triage_started",
				ExecutionID:        "exec-e2e-inc-104",
				IncidentID:         "INC-104",
				RootTaskName:       "triage-inc-104",
				HarnessMode:        "multi",
				RequestedModel:     "gemini-3.8-flash",
				ImplicatedServices: []string{"inventory-service", "shipping-service"},
				UnaffectedServices: []string{"storefront-web", "checkout-service", "pricing-service"},
			},
			{
				EventType:      "child_fixer_spawned",
				ExecutionID:    "exec-e2e-inc-104",
				IncidentID:     "INC-104",
				RootTaskName:   "triage-inc-104",
				TaskName:       "fix-inc-104-inventory-service",
				Service:        "inventory-service",
				HarnessMode:    "antigravity",
				RequestedModel: "gemini-3.8-flash",
			},
			{
				EventType:        "child_fixer_completed",
				ExecutionID:      "exec-e2e-inc-104",
				IncidentID:       "INC-104",
				RootTaskName:     "triage-inc-104",
				TaskName:         "fix-inc-104-inventory-service",
				Service:          "inventory-service",
				HarnessMode:      "antigravity",
				RequestedModel:   "gemini-3.8-flash",
				SourceProvenance: "LIVE",
			},
			{
				EventType:         "root_merge_completed",
				ExecutionID:       "exec-e2e-inc-104",
				IncidentID:        "INC-104",
				RootTaskName:      "triage-inc-104",
				MergedCommitSHA:   "e2e104healedsha",
				RolloutDurationMs: 920,
			},
		}

		for _, ev := range events {
			raw, _ := json.Marshal(ev)
			resp, err := http.Post(obsHTTP.URL+"/api/internal/observer-events", "application/json", bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("posting callback %s: %v", ev.EventType, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("callback %s status = %d, want 200", ev.EventType, resp.StatusCode)
			}
		}

		treeResp, err := http.Get(obsHTTP.URL + "/api/tree")
		if err != nil {
			t.Fatalf("GET /api/tree failed: %v", err)
		}
		var treeData map[string]any
		_ = json.NewDecoder(treeResp.Body).Decode(&treeData)
		_ = treeResp.Body.Close()
		if gotID := treeData["incident_id"]; gotID != "INC-104" {
			t.Errorf("/api/tree incident_id = %v, want INC-104", gotID)
		}
	})

	t.Run("RepositoryHygieneAndManifestContract", func(t *testing.T) {
		TestRepositoryHygieneAndManifestContract(t)
	})
}

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

var (
	suiteMatrixOnce   sync.Once
	suiteMatrixReport evals.SuiteReport
	suiteMatrixErr    error
)

func getTwentyTrajectorySuiteReport(t *testing.T) evals.SuiteReport {
	t.Helper()
	suiteMatrixOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		suiteMatrixReport, suiteMatrixErr = evals.RunEvaluationSuite(ctx, evals.SuiteConfig{
			IncludeLiveCascadeChecks: true,
			MaxConcurrency:           4,
		})
	})
	if suiteMatrixErr != nil {
		t.Fatalf("RunEvaluationSuite failed: %v", suiteMatrixErr)
	}
	return suiteMatrixReport
}

func TestFourScenariosAcrossAllHarnessesAndLiveCascade(t *testing.T) {
	report := getTwentyTrajectorySuiteReport(t)
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
	if len(hygiene.CheckedFiles) < 20 {
		t.Errorf("len(CheckedFiles) = %d, want >= 20 (including manifests, Dockerfiles, harness configs, scripts, and docs)", len(hygiene.CheckedFiles))
	}
	wantChecked := map[string]bool{
		"manifests/monitoring/03-gmp-podmonitoring.yaml":        false,
		"deploy/ax-observer/Dockerfile":                         false,
		"deploy/model-gateway/Dockerfile":                       false,
		"harnesses/Dockerfile.task-runner":                      false,
		"internal/httputil/httputil.go":                         false,
		"internal/observer/about_catalog.go":                    false,
		"services/run-service-tests.sh":                         false,
		"docs/blog/self-spawning-bug-bash-tree-on-google-ax.md": false,
		"docs/tutorial/build-a-self-spawning-bug-bash-tree.md":  false,
		"hack/record-stage-walkthrough.sh":                      false,
		"hack/record-terminal-demo.py":                          false,
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

func TestMarkdownHumanizerAndStageSafeProse(t *testing.T) {
	TestVerifyMarkdownHumanizerAndHygiene(t)
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
		name       string
		markdown   string
		wantSubstr string
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
			name:       "dramatic_one_line_closer",
			markdown:   "The supervisor swaps the process.\n\nThat is the real win.\n",
			wantSubstr: "closer",
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
