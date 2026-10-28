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
		"alert: StorefrontHTTP500Spike",
		"for: 0s",
		"storefront_http_requests_total",
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
		"storefront-golden-signals.json",
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
}



