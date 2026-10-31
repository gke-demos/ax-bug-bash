package monitoring_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/monitoring"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func TestAlertmanagerWebhookEnrichesLokiLogsAndDispatchesRootTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sf, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}

	gw := gateway.NewServer(gateway.Config{}, nil)
	orch, err := bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
		Storefront:         sf,
		ModelGateway:       gw,
		DefaultHarnessMode: "multi",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}
	defer orch.Close()

	// Trigger INC-101 on Storefront and generate a real HTTP 500 checkout failure so Loki logs are populated.
	if _, err := sf.ActivateIncident(storefront.IncidentStackedPromoNegativeSubtotal); err != nil {
		t.Fatalf("ActivateIncident failed: %v", err)
	}

	sfServer := httptest.NewServer(sf.Handler())
	defer sfServer.Close()

	bridge := monitoring.NewAlertmanagerBridge(monitoring.AlertmanagerConfig{
		Storefront:    sf,
		StorefrontURL: sfServer.URL,
		Orchestrator:  orch,
	})

	webhookSrv := httptest.NewServer(bridge.Handler())
	defer webhookSrv.Close()

	payload := monitoring.AlertmanagerWebhookPayload{
		Version:  "4",
		GroupKey: "{}:{alertname=\"StorefrontHTTP500Spike\",incident=\"INC-101\"}",
		Status:   "firing",
		Receiver: "ax-observer-webhook",
		GroupLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  "INC-101",
		},
		CommonLabels: map[string]string{
			"alertname":  "StorefrontHTTP500Spike",
			"incident":   "INC-101",
			"severity":   "critical",
			"gcp_export": "managed-prometheus",
		},
		CommonAnnotations: map[string]string{
			"summary":     "Storefront HTTP 500 spike detected on pricing-service and shipping-service",
			"description": "Stacked promo codes SAVE20 + FLAT15 produced negative subtotal HTTP 500",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":  "StorefrontHTTP500Spike",
					"incident":   "INC-101",
					"service":    "pricing-service",
					"severity":   "critical",
					"gcp_export": "managed-prometheus",
				},
				Annotations: map[string]string{
					"summary": "pricing-service HTTP 500 NegativeSubtotalError",
				},
				StartsAt:    time.Now().UTC().Add(-2 * time.Second),
				Fingerprint: "fp-inc-101-pricing-shipping",
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	resp, err := http.Post(webhookSrv.URL+"/api/webhooks/alertmanager", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/webhooks/alertmanager failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d", resp.StatusCode)
	}

	var webhookResp monitoring.AlertmanagerWebhookResponse
	if err := json.NewDecoder(resp.Body).Decode(&webhookResp); err != nil {
		t.Fatalf("decoding webhook response failed: %v", err)
	}

	if webhookResp.Status != "dispatched" {
		t.Fatalf("expected status 'dispatched', got %q", webhookResp.Status)
	}
	if webhookResp.ExecutionID == "" || webhookResp.RootTaskName == "" {
		t.Fatalf("expected non-empty ExecutionID and RootTaskName, got %+v", webhookResp)
	}
	if len(webhookResp.Alert.LokiBreadcrumbs) < 2 {
		t.Fatalf("expected at least 2 Loki log breadcrumbs for INC-101, got %d", len(webhookResp.Alert.LokiBreadcrumbs))
	}
	if len(webhookResp.Alert.StackTraces) < 2 {
		t.Fatalf("expected at least 2 enriched stack traces for INC-101, got %d", len(webhookResp.Alert.StackTraces))
	}
	if !webhookResp.Alert.GCPExportCompatible {
		t.Fatalf("expected GCPExportCompatible=true on enriched alert")
	}

	// Verify Root Task only spawned Child Tasks for the 2 implicated services (pricing-service, shipping-service)
	execTree, ok := orch.GetExecution(webhookResp.ExecutionID)
	if !ok {
		t.Fatalf("expected execution %q in orchestrator", webhookResp.ExecutionID)
	}
	if len(execTree.ChildTasks) != 2 {
		t.Fatalf("expected 2 child tasks for INC-101, got %d", len(execTree.ChildTasks))
	}

	// Re-sending the exact same firing fingerprint while in-flight/active should deduplicate
	respDup, err := http.Post(webhookSrv.URL+"/api/webhooks/alertmanager", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("duplicate POST /api/webhooks/alertmanager failed: %v", err)
	}
	defer respDup.Body.Close()

	var dupResp monitoring.AlertmanagerWebhookResponse
	if err := json.NewDecoder(respDup.Body).Decode(&dupResp); err != nil {
		t.Fatalf("decoding duplicate webhook response failed: %v", err)
	}
	if !dupResp.Deduplicated {
		t.Fatalf("expected duplicate firing alert for same fingerprint to be deduplicated")
	}
	if len(orch.ListExecutions()) != 1 {
		t.Fatalf("expected exactly 1 execution after deduplication, got %d", len(orch.ListExecutions()))
	}
}

func TestPrometheusScraperEvaluates500SpikeAndFiresAlertmanagerWebhook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sf, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}
	sfServer := httptest.NewServer(sf.Handler())
	defer sfServer.Close()

	gw := gateway.NewServer(gateway.Config{}, nil)
	orch, err := bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
		Storefront:         sf,
		ModelGateway:       gw,
		DefaultHarnessMode: "multi",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}
	defer orch.Close()

	bridge := monitoring.NewAlertmanagerBridge(monitoring.AlertmanagerConfig{
		Storefront:    sf,
		StorefrontURL: sfServer.URL,
		Orchestrator:  orch,
	})
	webhookSrv := httptest.NewServer(bridge.Handler())
	defer webhookSrv.Close()

	scraper := monitoring.NewScraper(monitoring.ScraperConfig{
		Storefront:         sf,
		StorefrontURL:      sfServer.URL,
		AlertmanagerBridge: bridge,
		WebhookURL:         webhookSrv.URL + "/api/webhooks/alertmanager",
		ScrapeInterval:     40 * time.Millisecond,
	})
	scraper.Start(ctx)
	defer scraper.Stop()

	monSrv := httptest.NewServer(scraper.Handler())
	defer monSrv.Close()

	// Trigger a real HTTP 500 on the Storefront via checkout (INC-102: Flash-Sale Bundle u32 Underflow)
	sc102, _ := storefront.GetScenario(storefront.IncidentFlashSaleBundleUnderflow)
	cartBytes, _ := json.Marshal(sc102.TriggerCart)
	checkoutResp, err := http.Post(sfServer.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		t.Fatalf("POST /api/cart/checkout failed: %v", err)
	}
	checkoutResp.Body.Close()
	if checkoutResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on unhealed INC-102 checkout, got %d", checkoutResp.StatusCode)
	}

	// Wait for the background scraper to detect the HTTP 500 spike and fire the Alertmanager webhook
	deadline := time.Now().Add(30 * time.Second)
	var latestExec *bugbash.BugBashTreeExecution
	for time.Now().Before(deadline) {
		if ex := orch.LatestExecution(); ex != nil && ex.IncidentID == storefront.IncidentFlashSaleBundleUnderflow && ex.Phase == "HEALED" {
			latestExec = ex
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if latestExec == nil {
		t.Fatalf("expected scraper to detect HTTP 500 spike and dispatch Root Task for INC-102 within deadline")
	}

	// Verify GET /api/monitoring/metrics exposes scraped 5-service golden signals and GCP dual-export status
	metricsResp, err := http.Get(monSrv.URL + "/api/monitoring/metrics")
	if err != nil {
		t.Fatalf("GET /api/monitoring/metrics failed: %v", err)
	}
	defer metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /api/monitoring/metrics, got %d", metricsResp.StatusCode)
	}
	var snap monitoring.MetricsSnapshot
	if err := json.NewDecoder(metricsResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding MetricsSnapshot failed: %v", err)
	}
	if snap.HTTP500ByService[storefront.ServiceInventory] < 1 || snap.HTTP500ByService[storefront.ServiceCheckout] < 1 {
		t.Fatalf("expected scraped HTTP 500 counts > 0 for inventory-service and checkout-service, got %+v", snap.HTTP500ByService)
	}
	if !snap.GCPManagedPrometheusExport || !snap.GCPCloudLoggingExport {
		t.Fatalf("expected GCPManagedPrometheusExport and GCPCloudLoggingExport to be true")
	}

	// Verify GET /monitoring serves the Grafana & Alertmanager Live Dashboard Preview UI
	uiResp, err := http.Get(monSrv.URL + "/monitoring")
	if err != nil {
		t.Fatalf("GET /monitoring failed: %v", err)
	}
	defer uiResp.Body.Close()
	if uiResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /monitoring, got %d", uiResp.StatusCode)
	}
}

func TestPreWarmedIncrementalRolloutBridgeHealsStorefrontAndResolvesAlertUnder10s(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sf, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime failed: %v", err)
	}
	sfServer := httptest.NewServer(sf.Handler())
	defer sfServer.Close()

	gw := gateway.NewServer(gateway.Config{}, nil)
	orch, err := bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
		Storefront:         sf,
		ModelGateway:       gw,
		DefaultHarnessMode: "multi",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}
	defer orch.Close()

	bridge := monitoring.NewAlertmanagerBridge(monitoring.AlertmanagerConfig{
		Storefront:    sf,
		StorefrontURL: sfServer.URL,
		Orchestrator:  orch,
	})
	rb := monitoring.NewRolloutBridge(monitoring.RolloutBridgeConfig{
		Storefront:         sf,
		StorefrontURL:      sfServer.URL,
		AlertmanagerBridge: bridge,
	})
	bridge.SetRolloutBridge(rb)

	webhookSrv := httptest.NewServer(bridge.Handler())
	defer webhookSrv.Close()

	// Activate INC-103 (Zero-Decimal JPY Rounding across storefront-web, pricing-service, checkout-service)
	sc103, err := sf.ActivateIncident(storefront.IncidentZeroDecimalJPYRounding)
	if err != nil {
		t.Fatalf("ActivateIncident(INC-103) failed: %v", err)
	}

	// Verify Storefront checkout returns HTTP 500 before repair & rollout
	cartBytes, _ := json.Marshal(sc103.TriggerCart)
	preResp, err := http.Post(sfServer.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		t.Fatalf("pre-rollout checkout failed: %v", err)
	}
	preResp.Body.Close()
	if preResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 before rollout, got %d", preResp.StatusCode)
	}

	// Send firing Alertmanager v4 webhook for INC-103
	payload := monitoring.AlertmanagerWebhookPayload{
		Version:  "4",
		Status:   "firing",
		Receiver: "ax-observer-webhook",
		CommonLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  storefront.IncidentZeroDecimalJPYRounding,
			"severity":  "critical",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "StorefrontHTTP500Spike",
					"incident":  storefront.IncidentZeroDecimalJPYRounding,
					"service":   storefront.ServiceStorefrontWeb,
					"severity":  "critical",
				},
				StartsAt:    time.Now().UTC(),
				Fingerprint: "fp-inc-103-jpy",
			},
		},
	}
	rawPayload, _ := json.Marshal(payload)
	whResp, err := http.Post(webhookSrv.URL+"/api/webhooks/alertmanager", "application/json", bytes.NewReader(rawPayload))
	if err != nil {
		t.Fatalf("POST /api/webhooks/alertmanager failed: %v", err)
	}
	defer whResp.Body.Close()

	var result monitoring.AlertmanagerWebhookResponse
	if err := json.NewDecoder(whResp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding webhook response failed: %v", err)
	}

	// Alert should be resolved after the <10s incremental rollout bridge completes
	if result.Alert.Status != "resolved" {
		t.Fatalf("expected alert status to transition to 'resolved' after rollout, got %q", result.Alert.Status)
	}

	report := rb.LatestRollout()
	if report == nil {
		t.Fatalf("expected non-nil RolloutReport from LatestRollout()")
	}
	if report.IncidentID != storefront.IncidentZeroDecimalJPYRounding {
		t.Fatalf("expected rollout IncidentID=%q, got %q", storefront.IncidentZeroDecimalJPYRounding, report.IncidentID)
	}
	if report.TotalDurationMs <= 0 || report.TotalDurationMs >= 10000 {
		t.Fatalf("expected rollout TotalDurationMs in (0, 10000), got %d ms", report.TotalDurationMs)
	}
	if !report.StorefrontCheckoutVerified {
		t.Fatalf("expected StorefrontCheckoutVerified=true on RolloutReport")
	}
	if len(report.UpdatedServices) != 3 {
		t.Fatalf("expected only the 3 implicated services rolled out for INC-103, got %d (%+v)", len(report.UpdatedServices), report.UpdatedServices)
	}
	for _, svcRollout := range report.UpdatedServices {
		if !svcRollout.CacheWarmHit || svcRollout.BuildCache == "" || svcRollout.Status != "ROLLED_OUT" {
			t.Fatalf("expected warm build cache hit and ROLLED_OUT status for %s, got %+v", svcRollout.Service, svcRollout)
		}
	}
	if len(report.SkippedServices) != 2 {
		t.Fatalf("expected 2 unaffected services skipped during incremental rollout, got %v", report.SkippedServices)
	}

	// Verify live Retail Storefront now returns HTTP 200 OK for the exact INC-103 JPY checkout cart
	postResp, err := http.Post(sfServer.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		t.Fatalf("post-rollout checkout failed: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK after incremental rollout, got %d", postResp.StatusCode)
	}

	// Verify RolloutBridge rejects RolloutTreeFix if the git workspace is not actually patched
	unpatchedSF, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime for unpatched check failed: %v", err)
	}
	if _, err := unpatchedSF.ActivateIncident(storefront.IncidentStackedPromoNegativeSubtotal); err != nil {
		t.Fatalf("ActivateIncident failed: %v", err)
	}
	unpatchedRB := monitoring.NewRolloutBridge(monitoring.RolloutBridgeConfig{
		Storefront: unpatchedSF,
	})
	if _, err := unpatchedRB.RolloutTreeFix(ctx, &bugbash.BugBashTreeExecution{
		ExecutionID:        "tree-unpatched",
		IncidentID:         storefront.IncidentStackedPromoNegativeSubtotal,
		ImplicatedServices: []string{storefront.ServicePricing, storefront.ServiceShipping},
	}); err == nil {
		t.Fatalf("expected RolloutTreeFix to fail when git workspace is unpatched, got nil error")
	}
}

func TestMonitoringKubernetesManifestsAndGrafanaDashboardContract(t *testing.T) {
	manifestDir := filepath.Join("..", "..", "manifests", "monitoring")

	promRaw, err := os.ReadFile(filepath.Join(manifestDir, "01-prometheus-alertmanager.yaml"))
	if err != nil {
		t.Fatalf("reading 01-prometheus-alertmanager.yaml failed: %v", err)
	}
	promYAML := string(promRaw)
	requiredPromTokens := []string{
		"name: monitoring",
		"scrape_interval: 5s",
		"evaluation_interval: 5s",
		"alert: StorefrontServiceHttp500Spike",
		"StorefrontHTTP500Spike",
		`increase(storefront_http_requests_total{status="500"}[15s]) > 0`,
		"for: 0s",
		"storefront_http_requests_total",
		"group_by: ['alertname']",
		"group_wait: 1s",
		"http://ax-observer.ax-bug-bash.svc.cluster.local:8080/api/webhooks/alertmanager",
		"http://ax-observer.ax-bug-bash.svc.cluster.local:3000/api/webhooks/alertmanager",
		"monitoring.googleapis.com/v1",
		"kind: PodMonitoring",
	}
	for _, svc := range storefront.AllServices() {
		requiredPromTokens = append(requiredPromTokens, svc)
	}
	for _, tok := range requiredPromTokens {
		if !strings.Contains(promYAML, tok) {
			t.Fatalf("expected 01-prometheus-alertmanager.yaml to contain %q", tok)
		}
	}

	lokiGrafanaRaw, err := os.ReadFile(filepath.Join(manifestDir, "02-loki-grafana.yaml"))
	if err != nil {
		t.Fatalf("reading 02-loki-grafana.yaml failed: %v", err)
	}
	lokiGrafanaYAML := string(lokiGrafanaRaw)
	requiredGrafanaTokens := []string{
		"Storefront Golden Signals & 500 Error Logs",
		"AX Bug Bash — Polyglot Storefront & Rollouts",
		"ax-bug-bash-storefront",
		"storefront-golden-signals.json",
		"storefront_http_requests_total",
		"storefront_service_reloads_total",
		"StorefrontServiceHttp500Spike",
		`{app="storefront"} |= "HTTP 500"`,
		"GF_AUTH_ANONYMOUS_ENABLED",
		"GF_AUTH_ANONYMOUS_ORG_ROLE",
		"GF_SERVER_HTTP_PORT",
		"uid: prometheus",
		"uid: loki",
		"logging.googleapis.com",
		"port: 3002",
	}
	for _, tok := range requiredGrafanaTokens {
		if !strings.Contains(lokiGrafanaYAML, tok) {
			t.Fatalf("expected 02-loki-grafana.yaml to contain %q", tok)
		}
	}

	gmpRaw, err := os.ReadFile(filepath.Join(manifestDir, "03-gmp-podmonitoring.yaml"))
	if err != nil {
		t.Fatalf("reading 03-gmp-podmonitoring.yaml failed: %v", err)
	}
	gmpYAML := string(gmpRaw)
	requiredGMPTokens := []string{
		"monitoring.googleapis.com/v1",
		"kind: PodMonitoring",
		"namespace: ax-bug-bash",
		"interval: 10s",
		"http-web",
		"http-checkout",
		"http-pricing",
		"http-inventory",
		"http-shipping",
	}
	for _, tok := range requiredGMPTokens {
		if !strings.Contains(gmpYAML, tok) {
			t.Fatalf("expected 03-gmp-podmonitoring.yaml to contain %q", tok)
		}
	}
}

func TestConsolidatedMultiServiceAlertmanagerWebhookDeduplicationAndReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var triggered []bugbash.TriggerRequest
	receiver := monitoring.NewAlertmanagerReceiver(monitoring.AlertmanagerConfig{
		HarnessMode: "antigravity",
		OnIncident: func(_ context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error) {
			triggered = append(triggered, req)
			return &bugbash.BugBashTreeExecution{
				ExecutionID:        "exec-" + strings.ToLower(req.IncidentID),
				IncidentID:         req.IncidentID,
				HarnessMode:        req.HarnessMode,
				ImplicatedServices: []string{storefront.ServiceStorefrontWeb, storefront.ServicePricing, storefront.ServiceCheckout},
				RootTask: bugbash.RootTaskNode{
					TaskName: "bugbash-root-" + strings.ToLower(req.IncidentID),
				},
			}, nil
		},
	})

	// Test runtime default_harness getter/setter
	if err := receiver.SetDefaultHarness("opencode"); err != nil {
		t.Fatalf("SetDefaultHarness(opencode) failed: %v", err)
	}
	if got := receiver.GetDefaultHarness(); got != "opencode" {
		t.Fatalf("GetDefaultHarness() = %q, want opencode", got)
	}
	if err := receiver.SetDefaultHarness("invalid-harness"); err == nil {
		t.Fatalf("expected SetDefaultHarness to reject unknown harness")
	}

	// Send a 3-service consolidated Alertmanager webhook (without incident_id label, as Prometheus fires on pure HTTP 500 metrics)
	payload3 := monitoring.AlertmanagerWebhookPayload{
		Version:  "4",
		GroupKey: `{}:{alertname="StorefrontServiceHttp500Spike"}`,
		Status:   "firing",
		Receiver: "ax-observer-webhook",
		GroupLabels: map[string]string{
			"alertname": "StorefrontServiceHttp500Spike",
		},
		CommonLabels: map[string]string{
			"alertname": "StorefrontServiceHttp500Spike",
			"severity":  "critical",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": "inventory-service", "severity": "critical"},
				StartsAt:    time.Now().UTC().Add(-3 * time.Second),
				Fingerprint: "fp-svc-inventory",
			},
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": "shipping-service", "severity": "critical"},
				StartsAt:    time.Now().UTC().Add(-2 * time.Second),
				Fingerprint: "fp-svc-shipping",
			},
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": "storefront-web", "severity": "critical"},
				StartsAt:    time.Now().UTC().Add(-1 * time.Second),
				Fingerprint: "fp-svc-web",
			},
		},
	}

	resp1, err := receiver.ProcessWebhook(ctx, payload3)
	if err != nil {
		t.Fatalf("ProcessWebhook failed: %v", err)
	}
	if resp1.Deduplicated || resp1.Duplicate {
		t.Fatalf("expected first consolidated webhook not to be duplicate, got %+v", resp1)
	}
	if resp1.Alert.IncidentID != storefront.IncidentLithiumHazmatWeightTier {
		t.Fatalf("expected 3-service hazmat blast radius to infer INC-104, got %q", resp1.Alert.IncidentID)
	}
	if len(resp1.Alert.BlastRadiusServices) != 3 {
		t.Fatalf("expected 3 BlastRadiusServices, got %v", resp1.Alert.BlastRadiusServices)
	}
	if len(resp1.Alert.EnrichedLogs) < 2 {
		t.Fatalf("expected EnrichedLogs populated for faulted services, got %d", len(resp1.Alert.EnrichedLogs))
	}
	if len(triggered) != 1 || triggered[0].HarnessMode != "opencode" {
		t.Fatalf("expected 1 Root Task triggered with runtime default harness 'opencode', got %+v", triggered)
	}

	// Second webhook while active must deduplicate (Duplicate: true, Deduplicated: true)
	resp2, err := receiver.ProcessWebhook(ctx, payload3)
	if err != nil {
		t.Fatalf("second ProcessWebhook failed: %v", err)
	}
	if !resp2.Deduplicated || !resp2.Duplicate || !resp2.Alert.Duplicate {
		t.Fatalf("expected second webhook to mark Duplicate=true and Deduplicated=true, got %+v", resp2)
	}
	if len(triggered) != 1 {
		t.Fatalf("expected Root Task not to re-trigger on duplicate webhook, count=%d", len(triggered))
	}

	// ResetDeduplication allows the same incident to fire cleanly again on the next chaos soak run
	receiver.ResetDeduplication()
	if err := receiver.SetDefaultHarness("pi"); err != nil {
		t.Fatalf("SetDefaultHarness(pi) failed: %v", err)
	}
	resp3, err := receiver.ProcessWebhook(ctx, payload3)
	if err != nil {
		t.Fatalf("post-reset ProcessWebhook failed: %v", err)
	}
	if resp3.Deduplicated || resp3.Duplicate {
		t.Fatalf("expected post-reset webhook to dispatch fresh Root Task, got %+v", resp3)
	}
	if len(triggered) != 2 || triggered[1].HarnessMode != "pi" {
		t.Fatalf("expected second Root Task dispatch with harness 'pi' after ResetDeduplication, got %+v", triggered)
	}
}

func TestLokiQueryRangeEnrichmentAndIncidentInferenceWithoutLabels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var capturedQuery string
	var pushedStreams int
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/push":
			pushedStreams++
			w.WriteHeader(http.StatusNoContent)
			return
		case "/loki/api/v1/query_range":
			capturedQuery = r.URL.Query().Get("query")
			line1, _ := json.Marshal(monitoring.StructuredErrorLog{
				Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
				Level:      "ERROR",
				RequestID:  "req-jpy-4476",
				IncidentID: storefront.IncidentZeroDecimalJPYRounding,
				Service:    storefront.ServicePricing,
				Language:   "Python / FastAPI",
				SourceFile: "pricing.py",
				ErrorType:  "CurrencyPrecisionMismatchError",
				Message:    "HTTP 500: CurrencyPrecisionMismatchError: ISO-4217 zero-decimal currency JPY cannot have fractional minor units: 4476.15",
				StackTrace: "File \"pricing.py\", line 42, in convert_currency_minor_units\nValueError: HTTP 500: CurrencyPrecisionMismatchError",
			})
			line2, _ := json.Marshal(monitoring.StructuredErrorLog{
				Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
				Level:      "ERROR",
				RequestID:  "req-jpy-4476",
				IncidentID: storefront.IncidentZeroDecimalJPYRounding,
				Service:    storefront.ServiceCheckout,
				Language:   "Go",
				SourceFile: "checkout.go",
				ErrorType:  "PaymentCapturePrecisionError",
				Message:    "HTTP 500: PaymentCapturePrecisionError: ISO-4217 zero-decimal currency JPY rejected fractional minor unit 4476.15",
				StackTrace: "checkout.CaptureCurrencyPayment(...) /workspace/services/checkout-service/checkout.go:32",
			})
			resp := map[string]any{
				"status": "success",
				"data": map[string]any{
					"resultType": "streams",
					"result": []map[string]any{
						{
							"stream": map[string]string{
								"app":     "storefront",
								"service": "pricing-service",
								"level":   "ERROR",
							},
							"values": [][]string{
								{
									"1730178000000000000",
									string(line1),
								},
							},
						},
						{
							"stream": map[string]string{
								"app":     "storefront",
								"service": "checkout-service",
								"level":   "ERROR",
							},
							"values": [][]string{
								{
									"1730178000000000001",
									string(line2),
								},
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer lokiSrv.Close()

	var dispatchedReq bugbash.TriggerRequest
	receiver := monitoring.NewAlertmanagerReceiver(
		monitoring.AlertmanagerConfig{
			LokiPushURL: lokiSrv.URL + "/loki/api/v1/push",
			OnIncident: func(_ context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error) {
				dispatchedReq = req
				return &bugbash.BugBashTreeExecution{
					ExecutionID: "exec-loki-103",
					IncidentID:  req.IncidentID,
					RootTask:    bugbash.RootTaskNode{TaskName: "root-loki-103"},
				}, nil
			},
		},
		monitoring.WithLokiURL(lokiSrv.URL),
	)

	// Verify async/direct Loki push helper
	if err := receiver.PushLokiErrorLog(ctx, monitoring.StructuredErrorLog{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Level:      "ERROR",
		RequestID:  "req-jpy-4476",
		IncidentID: storefront.IncidentZeroDecimalJPYRounding,
		Service:    storefront.ServicePricing,
		Message:    "HTTP 500: CurrencyPrecisionMismatchError",
	}); err != nil {
		t.Fatalf("PushLokiErrorLog failed: %v", err)
	}
	if pushedStreams != 1 {
		t.Fatalf("expected 1 pushed stream to /loki/api/v1/push, got %d", pushedStreams)
	}

	// Send Alertmanager webhook with UNKNOWN incident_id and 2 alerts (pricing-service + checkout-service)
	whPayload := monitoring.AlertmanagerWebhookPayload{
		Version: "4",
		Status:  "firing",
		GroupLabels: map[string]string{
			"alertname": "StorefrontServiceHttp500Spike",
		},
		CommonLabels: map[string]string{
			"alertname":   "StorefrontServiceHttp500Spike",
			"incident_id": "UNKNOWN",
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": "pricing-service"},
				Fingerprint: "fp-pricing-500",
			},
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": "checkout-service"},
				Fingerprint: "fp-checkout-500",
			},
		},
	}

	res, err := receiver.ProcessWebhook(ctx, whPayload)
	if err != nil {
		t.Fatalf("ProcessWebhook failed: %v", err)
	}
	if !strings.Contains(capturedQuery, `HTTP 500`) {
		t.Fatalf("expected Loki LogQL query to filter for HTTP 500, got %q", capturedQuery)
	}
	if res.Alert.IncidentID != storefront.IncidentZeroDecimalJPYRounding {
		t.Fatalf("expected IncidentID inferred from Loki logs to be INC-103, got %q", res.Alert.IncidentID)
	}
	if dispatchedReq.IncidentID != storefront.IncidentZeroDecimalJPYRounding {
		t.Fatalf("expected TriggerRequest IncidentID=INC-103, got %q", dispatchedReq.IncidentID)
	}
	if len(res.Alert.EnrichedLogs) != 2 {
		t.Fatalf("expected 2 EnrichedLogs from Loki query_range, got %d", len(res.Alert.EnrichedLogs))
	}
	if res.Alert.EnrichedLogs[0].RequestID != "req-jpy-4476" {
		t.Fatalf("expected RequestID 'req-jpy-4476' preserved in EnrichedLogs, got %q", res.Alert.EnrichedLogs[0].RequestID)
	}
}

func TestLokiTimeoutResilienceAndBlastRadiusSignatureInference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Slow Loki server that sleeps longer than the strict 2s timeout
	slowLoki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(4 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer slowLoki.Close()

	receiver := monitoring.NewAlertmanagerReceiver(
		monitoring.AlertmanagerConfig{
			OnIncident: func(_ context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error) {
				return &bugbash.BugBashTreeExecution{
					ExecutionID: "exec-timeout-" + strings.ToLower(req.IncidentID),
					IncidentID:  req.IncidentID,
					RootTask:    bugbash.RootTaskNode{TaskName: "root-timeout-" + strings.ToLower(req.IncidentID)},
				}, nil
			},
		},
		monitoring.WithLokiURL(slowLoki.URL),
	)

	cases := []struct {
		name         string
		services     []string
		wantIncident string
	}{
		{
			name:         "pricing+shipping infers INC-101",
			services:     []string{"pricing-service", "shipping-service"},
			wantIncident: storefront.IncidentStackedPromoNegativeSubtotal,
		},
		{
			name:         "inventory+checkout infers INC-102",
			services:     []string{"inventory-service", "checkout-service"},
			wantIncident: storefront.IncidentFlashSaleBundleUnderflow,
		},
		{
			name:         "pricing+checkout infers INC-103",
			services:     []string{"pricing-service", "checkout-service"},
			wantIncident: storefront.IncidentZeroDecimalJPYRounding,
		},
		{
			name:         "3 services infers INC-104",
			services:     []string{"inventory-service", "shipping-service", "storefront-web"},
			wantIncident: storefront.IncidentLithiumHazmatWeightTier,
		},
	}

	for i, tc := range cases {
		receiver.ResetDeduplication()
		var alerts []monitoring.AlertmanagerAlert
		for _, svc := range tc.services {
			alerts = append(alerts, monitoring.AlertmanagerAlert{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "StorefrontServiceHttp500Spike", "service": svc},
				Fingerprint: "fp-" + svc,
			})
		}

		// Only test the actual 2s slow Loki timeout on the first case so the test suite stays fast
		if i == 1 {
			receiver.SetLokiURL("")
		}
		start := time.Now()
		res, err := receiver.ProcessWebhook(ctx, monitoring.AlertmanagerWebhookPayload{
			Version: "4",
			Status:  "firing",
			Alerts:  alerts,
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s: ProcessWebhook failed: %v", tc.name, err)
		}
		if i == 0 && elapsed > 3*time.Second {
			t.Fatalf("%s: expected strict 2s Loki timeout, but took %v", tc.name, elapsed)
		}
		if res.Alert.IncidentID != tc.wantIncident {
			t.Fatalf("%s: got IncidentID=%q, want %q", tc.name, res.Alert.IncidentID, tc.wantIncident)
		}
	}
}
