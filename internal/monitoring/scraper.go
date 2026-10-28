package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// ScraperConfig configures the 5-second Prometheus scraper, StorefrontHTTP500Spike alert evaluator,
// and standalone Monitoring & Alerting HTTP surface (:3002).
type ScraperConfig struct {
	Storefront         *storefront.Runtime
	StorefrontURL      string
	AlertmanagerBridge *AlertmanagerBridge
	RolloutBridge      *RolloutBridge
	WebhookURL         string
	ScrapeInterval     time.Duration
	DisableAutoWebhook bool
	HTTPClient         *http.Client
}

// MetricsSnapshot represents the latest scraped Prometheus Golden Signals across the 5 polyglot microservices.
type MetricsSnapshot struct {
	ScrapeInterval             string              `json:"scrape_interval"`
	EvaluationWindow           string              `json:"evaluation_window"`
	ForDuration                string              `json:"for_duration"`
	HTTP200ByService           map[string]int      `json:"http_200_by_service"`
	HTTP500ByService           map[string]int      `json:"http_500_by_service"`
	ActiveIncidents            map[string][]string `json:"active_incidents"`
	TotalHTTP200               int                 `json:"total_http_200"`
	TotalHTTP500               int                 `json:"total_http_500"`
	AlertRuleName              string              `json:"alert_rule_name"`
	AlertRuleState             string              `json:"alert_rule_state"`
	GCPManagedPrometheusExport bool                `json:"gcp_managed_prometheus_export"`
	GCPCloudLoggingExport      bool                `json:"gcp_cloud_logging_export"`
	LastScrapeAt               time.Time           `json:"last_scrape_at"`
}

// Scraper runs the configurable 5-second Prometheus scrape and StorefrontHTTP500Spike evaluation loop (for: 0s)
// and serves the standalone Monitoring & Alerting Stack HTTP surface (:3002).
type Scraper struct {
	cfg        ScraperConfig
	httpClient *http.Client

	mu                 sync.RWMutex
	snapshot           MetricsSnapshot
	prev500ByService   map[string]int
	firedAt500Total    map[string]int
	dispatchedInFlight map[string]bool
	stopCh             chan struct{}
	stopOnce           sync.Once
	started            bool
}

var (
	promReqRe = regexp.MustCompile(`^storefront_http_requests_total\{status="([^"]+)",service="([^"]+)"\}\s+(\d+)`)
	promIncRe = regexp.MustCompile(`^storefront_incident_active\{incident="([^"]+)",service="([^"]+)"\}\s+(\d+)`)
)

// NewScraper creates a new Prometheus & Loki Scraper and Alert Evaluator.
func NewScraper(cfg ScraperConfig) *Scraper {
	if cfg.ScrapeInterval <= 0 {
		cfg.ScrapeInterval = 5 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	snap := MetricsSnapshot{
		ScrapeInterval:             cfg.ScrapeInterval.String(),
		EvaluationWindow:           "5s",
		ForDuration:                "0s",
		HTTP200ByService:           make(map[string]int),
		HTTP500ByService:           make(map[string]int),
		ActiveIncidents:            make(map[string][]string),
		AlertRuleName:              "StorefrontHTTP500Spike",
		AlertRuleState:             "inactive",
		GCPManagedPrometheusExport: true,
		GCPCloudLoggingExport:      true,
	}
	return &Scraper{
		cfg:                cfg,
		httpClient:         client,
		snapshot:           snap,
		prev500ByService:   make(map[string]int),
		firedAt500Total:    make(map[string]int),
		dispatchedInFlight: make(map[string]bool),
		stopCh:             make(chan struct{}),
	}
}

// Start launches the background Prometheus scrape & alert evaluation loop.
func (s *Scraper) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	interval := s.cfg.ScrapeInterval
	s.mu.Unlock()

	go func() {
		_, _ = s.ScrapeOnce(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				_, _ = s.ScrapeOnce(ctx)
			}
		}
	}()
}

// Stop halts the background scrape loop.
func (s *Scraper) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// Snapshot returns the most recent scraped MetricsSnapshot.
func (s *Scraper) Snapshot() MetricsSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMetricsSnapshot(s.snapshot)
}

// ScrapeOnce scrapes GET /metrics and GET /api/logs from the Retail Storefront, evaluates StorefrontHTTP500Spike
// (for: 0s), and fires a standard Alertmanager v4 webhook when an HTTP 500 spike is detected.
func (s *Scraper) ScrapeOnce(ctx context.Context) (MetricsSnapshot, error) {
	rawMetrics, err := s.fetchRawMetrics(ctx)
	if err != nil {
		return s.Snapshot(), err
	}

	http200 := make(map[string]int)
	http500 := make(map[string]int)
	activeIncidents := make(map[string][]string)
	total200 := 0
	total500 := 0

	for _, line := range strings.Split(rawMetrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := promReqRe.FindStringSubmatch(line); len(m) == 4 {
			status := m[1]
			svc := m[2]
			val, _ := strconv.Atoi(m[3])
			switch status {
			case "200":
				http200[svc] = val
				total200 += val
			case "500":
				http500[svc] = val
				total500 += val
			}
			continue
		}
		if m := promIncRe.FindStringSubmatch(line); len(m) == 4 {
			incID := m[1]
			svc := m[2]
			val, _ := strconv.Atoi(m[3])
			if val == 1 {
				activeIncidents[incID] = append(activeIncidents[incID], svc)
			}
		}
	}

	logs := s.fetchLogs(ctx)

	s.mu.Lock()
	var spikedServices []string
	for svc, count := range http500 {
		if count > s.prev500ByService[svc] {
			spikedServices = append(spikedServices, svc)
		}
		s.prev500ByService[svc] = count
	}

	// Determine if an incident is currently firing via either active gauge or new 500 log entry
	detectedIncidentID := ""
	for incID, svcs := range activeIncidents {
		if len(svcs) > 0 {
			detectedIncidentID = incID
			break
		}
	}
	if detectedIncidentID == "" && len(spikedServices) > 0 && len(logs) > 0 {
		detectedIncidentID = logs[len(logs)-1].IncidentID
	}

	ruleState := "inactive"
	shouldFire := false
	if detectedIncidentID != "" {
		lastFired500 := s.firedAt500Total[detectedIncidentID]
		if len(spikedServices) > 0 || (len(activeIncidents[detectedIncidentID]) > 0 && total500 > lastFired500) {
			ruleState = "firing"
			if !s.dispatchedInFlight[detectedIncidentID] {
				shouldFire = true
				s.dispatchedInFlight[detectedIncidentID] = true
				s.firedAt500Total[detectedIncidentID] = total500
			}
		} else if len(activeIncidents[detectedIncidentID]) > 0 {
			ruleState = "firing"
		}
	}

	now := time.Now().UTC()
	s.snapshot = MetricsSnapshot{
		ScrapeInterval:             s.cfg.ScrapeInterval.String(),
		EvaluationWindow:           "5s",
		ForDuration:                "0s",
		HTTP200ByService:           http200,
		HTTP500ByService:           http500,
		ActiveIncidents:            activeIncidents,
		TotalHTTP200:               total200,
		TotalHTTP500:               total500,
		AlertRuleName:              "StorefrontHTTP500Spike",
		AlertRuleState:             ruleState,
		GCPManagedPrometheusExport: true,
		GCPCloudLoggingExport:      true,
		LastScrapeAt:               now,
	}
	snapCopy := cloneMetricsSnapshot(s.snapshot)
	s.mu.Unlock()

	if shouldFire && detectedIncidentID != "" && !s.cfg.DisableAutoWebhook {
		s.fireAlertmanagerWebhook(ctx, detectedIncidentID, spikedServices, now)
		s.mu.Lock()
		s.dispatchedInFlight[detectedIncidentID] = false
		s.mu.Unlock()
	}

	return snapCopy, nil
}

func (s *Scraper) fireAlertmanagerWebhook(ctx context.Context, incidentID string, spikedServices []string, startsAt time.Time) {
	sc, _ := storefront.GetScenario(incidentID)
	services := spikedServices
	if len(services) == 0 && len(sc.ImplicatedServices) > 0 {
		services = sc.ImplicatedServices
	}
	primarySvc := "storefront-web"
	if len(services) > 0 {
		primarySvc = services[0]
	}

	fingerprint := fmt.Sprintf("fp-storefronthttp500spike-%s-%d", strings.ToLower(incidentID), startsAt.UnixNano())
	payload := AlertmanagerWebhookPayload{
		Version:  "4",
		GroupKey: fmt.Sprintf("{}:{alertname=\"StorefrontHTTP500Spike\",incident=%q}", incidentID),
		Status:   "firing",
		Receiver: "ax-observer-webhook",
		GroupLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  incidentID,
		},
		CommonLabels: map[string]string{
			"alertname":  "StorefrontHTTP500Spike",
			"incident":   incidentID,
			"service":    primarySvc,
			"severity":   "critical",
			"gcp_export": "managed-prometheus",
		},
		CommonAnnotations: map[string]string{
			"summary":     sc.Title,
			"description": sc.ErrorMessage,
			"error_code":  sc.ErrorCode,
		},
		Alerts: []AlertmanagerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":  "StorefrontHTTP500Spike",
					"incident":   incidentID,
					"service":    primarySvc,
					"services":   strings.Join(services, ","),
					"severity":   "critical",
					"gcp_export": "managed-prometheus",
				},
				Annotations: map[string]string{
					"summary":     sc.Title,
					"description": sc.ErrorMessage,
					"error_code":  sc.ErrorCode,
				},
				StartsAt:    startsAt,
				Fingerprint: fingerprint,
			},
		},
	}

	if strings.TrimSpace(s.cfg.WebhookURL) != "" {
		raw, err := json.Marshal(payload)
		if err == nil {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(raw))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := s.httpClient.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					return
				}
			}
		}
	}

	if s.cfg.AlertmanagerBridge != nil {
		_, _ = s.cfg.AlertmanagerBridge.ProcessWebhook(ctx, payload)
	}
}

func (s *Scraper) fetchRawMetrics(ctx context.Context) (string, error) {
	if strings.TrimSpace(s.cfg.StorefrontURL) != "" {
		u := strings.TrimRight(strings.TrimSpace(s.cfg.StorefrontURL), "/") + "/metrics"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			resp, err := s.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					b, err := io.ReadAll(resp.Body)
					if err == nil {
						return string(b), nil
					}
				}
			}
		}
	}
	if s.cfg.Storefront != nil {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		s.cfg.Storefront.Handler().ServeHTTP(rec, req)
		return rec.Body.String(), nil
	}
	return "", fmt.Errorf("no StorefrontURL or Storefront runtime configured")
}

func (s *Scraper) fetchLogs(ctx context.Context) []storefront.LogEntry {
	if strings.TrimSpace(s.cfg.StorefrontURL) != "" {
		u := strings.TrimRight(strings.TrimSpace(s.cfg.StorefrontURL), "/") + "/api/logs"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			resp, err := s.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var body struct {
						Entries []storefront.LogEntry `json:"entries"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
						return body.Entries
					}
				}
			}
		}
	}
	if s.cfg.Storefront != nil {
		return s.cfg.Storefront.Logs("")
	}
	return nil
}

// Handler returns the standalone Monitoring & Alerting HTTP surface (:3002).
func (s *Scraper) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/api/monitoring/metrics", s.handleMetricsSnapshot)
	mux.HandleFunc("/api/monitoring/alerts", s.handleAlertsSnapshot)
	mux.HandleFunc("/api/monitoring/rollouts", s.handleRolloutsSnapshot)
	mux.HandleFunc("/api/monitoring/dashboard", s.handleDashboardJSON)
	mux.HandleFunc("/api/webhooks/alertmanager", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AlertmanagerBridge != nil {
			s.cfg.AlertmanagerBridge.HandleWebhook(w, r)
			return
		}
		http.Error(w, "AlertmanagerBridge not configured", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/monitoring/dashboard.js", s.handleMonitoringScript)
	mux.HandleFunc("/monitoring", s.handleMonitoringUI)
	mux.HandleFunc("/", s.handleMonitoringUI)
	return httputil.WithSecurityHeaders(mux)
}

func (s *Scraper) handleMetricsSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.Snapshot()
	if snap.LastScrapeAt.IsZero() {
		snap, _ = s.ScrapeOnce(r.Context())
	}
	httputil.WriteJSON(w, http.StatusOK, snap)
}

func (s *Scraper) handleAlertsSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.AlertmanagerBridge != nil {
		s.cfg.AlertmanagerBridge.HandleListAlerts(w, r)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"alerts":        []EnrichedAlertRecord{},
		"scrape_window": "5s (for: 0s)",
	})
}

func (s *Scraper) handleRolloutsSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.RolloutBridge != nil {
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"rollouts":       s.cfg.RolloutBridge.ListRollouts(),
			"latest_rollout": s.cfg.RolloutBridge.LatestRollout(),
		})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"rollouts": []RolloutReport{},
	})
}

func (s *Scraper) handleDashboardJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(StorefrontGoldenSignalsDashboardJSON()))
}

func (s *Scraper) handleMonitoringUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/monitoring" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(monitoringDashboardHTML))
}

func (s *Scraper) handleMonitoringScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(monitoringDashboardJS))
}

// StorefrontGoldenSignalsDashboardJSON returns the pre-provisioned Grafana dashboard JSON
// for "Storefront Golden Signals & 500 Error Logs".
func StorefrontGoldenSignalsDashboardJSON() string {
	return `{
  "uid": "storefront-golden-signals",
  "title": "Storefront Golden Signals & 500 Error Logs",
  "tags": ["ax-bug-bash", "prometheus", "loki", "alertmanager", "gcp-dual-export"],
  "timezone": "utc",
  "schemaVersion": 39,
  "version": 1,
  "refresh": "5s",
  "panels": [
    {
      "id": 1,
      "title": "Polyglot Storefront HTTP 500 Error Spike (5s Scrape / for: 0s)",
      "type": "timeseries",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        {
          "expr": "sum by (service) (increase(storefront_http_requests_total{status=\"500\"}[5s]))",
          "legendFormat": "{{service}} HTTP 500"
        }
      ]
    },
    {
      "id": 2,
      "title": "Polyglot Storefront HTTP 200 Throughput by Microservice",
      "type": "timeseries",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        {
          "expr": "sum by (service) (rate(storefront_http_requests_total{status=\"200\"}[30s]))",
          "legendFormat": "{{service}} HTTP 200"
        }
      ]
    },
    {
      "id": 3,
      "title": "Active Incident Blast Radius by Microservice",
      "type": "stat",
      "datasource": { "type": "prometheus", "uid": "prometheus" },
      "targets": [
        {
          "expr": "storefront_incident_active == 1",
          "legendFormat": "{{incident}} -> {{service}}"
        }
      ]
    },
    {
      "id": 4,
      "title": "Loki & Google Cloud Logging Structured 500 Error Stack Traces",
      "type": "logs",
      "datasource": { "type": "loki", "uid": "loki" },
      "targets": [
        {
          "expr": "{app=\"retail-storefront\"} |= \"ERROR\""
        }
      ]
    }
  ]
}`
}

func cloneMetricsSnapshot(in MetricsSnapshot) MetricsSnapshot {
	out := in
	out.HTTP200ByService = make(map[string]int, len(in.HTTP200ByService))
	for k, v := range in.HTTP200ByService {
		out.HTTP200ByService[k] = v
	}
	out.HTTP500ByService = make(map[string]int, len(in.HTTP500ByService))
	for k, v := range in.HTTP500ByService {
		out.HTTP500ByService[k] = v
	}
	out.ActiveIncidents = make(map[string][]string, len(in.ActiveIncidents))
	for k, v := range in.ActiveIncidents {
		out.ActiveIncidents[k] = append([]string(nil), v...)
	}
	return out
}

const monitoringDashboardHTML = `<!DOCTYPE html>
<html lang="en" data-theme="dark">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Storefront Golden Signals &amp; Alertmanager (:3002)</title>
  <style>
    :root[data-theme="dark"] {
      --bg: #0b0f19;
      --surface: #111827;
      --border: #1f2937;
      --text: #f9fafb;
      --muted: #9ca3af;
      --accent: #38bdf8;
      --green: #10b981;
      --red: #ef4444;
    }
    :root[data-theme="light"] {
      --bg: #f8fafc;
      --surface: #ffffff;
      --border: #cbd5e1;
      --text: #0f172a;
      --muted: #475569;
      --accent: #0284c7;
      --green: #059669;
      --red: #dc2626;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 12px 24px;
      background: var(--surface);
      border-bottom: 1px solid var(--border);
    }
    .brand { display: flex; align-items: center; gap: 10px; font-weight: 700; font-size: 15px; }
    .badges { display: flex; align-items: center; gap: 10px; font-size: 12px; color: var(--muted); }
    .badge { padding: 4px 8px; border-radius: 6px; border: 1px solid var(--border); }
    .theme-toggle {
      display: inline-flex;
      align-items: center;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--bg);
      padding: 2px;
      cursor: pointer;
    }
    .theme-toggle button {
      background: transparent;
      border: none;
      color: var(--muted);
      padding: 6px;
      border-radius: 6px;
      cursor: pointer;
      display: inline-flex;
    }
    .theme-toggle button.active {
      background: var(--surface);
      color: var(--accent);
    }
    main { max-width: 1280px; margin: 20px auto; padding: 0 24px; display: grid; gap: 16px; }
    .card { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 16px; }
    .card h2 { margin: 0 0 12px; font-size: 14px; text-transform: uppercase; letter-spacing: 0.04em; color: var(--muted); }
    .grid-2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"></polyline></svg>
      <span>Storefront Golden Signals &amp; 500 Error Logs (Prometheus + Alertmanager + Loki + Grafana)</span>
    </div>
    <div class="badges">
      <span class="badge">Scrape: 5s (for: 0s)</span>
      <span class="badge">GCP Managed Prometheus + Cloud Logging Dual-Export</span>
      <div class="theme-toggle" id="theme-toggle" role="group" aria-label="Projector Light Mode Toggle">
        <button type="button" id="btn-dark" class="active" aria-label="Dark Mode">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path></svg>
        </button>
        <button type="button" id="btn-light" aria-label="Projector Light Mode">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="5"></circle><line x1="12" y1="1" x2="12" y2="3"></line><line x1="12" y1="21" x2="12" y2="23"></line><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line><line x1="1" y1="12" x2="3" y2="12"></line><line x1="21" y1="12" x2="23" y2="12"></line><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line></svg>
        </button>
      </div>
    </div>
  </header>
  <main>
    <div class="grid-2">
      <section class="card">
        <h2>Prometheus 5-Service Golden Signals (storefront_http_requests_total)</h2>
        <div id="metrics-container">Loading 5s Prometheus scrape metrics...</div>
      </section>
      <section class="card">
        <h2>Alertmanager v4 Webhook Status (StorefrontHTTP500Spike)</h2>
        <div id="alerts-container">No firing alerts</div>
      </section>
    </div>
    <section class="card">
      <h2>Pre-Warmed Incremental Rollouts (&lt; 10s Target)</h2>
      <div id="rollouts-container">No rollouts recorded yet</div>
    </section>
  </main>
  <script src="/monitoring/dashboard.js"></script>
</body>
</html>`

const monitoringDashboardJS = `(function () {
  var btnDark = document.getElementById('btn-dark');
  var btnLight = document.getElementById('btn-light');
  function setTheme(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    if (btnDark && btnLight) {
      btnDark.className = theme === 'dark' ? 'active' : '';
      btnLight.className = theme === 'light' ? 'active' : '';
    }
  }
  if (btnDark) btnDark.addEventListener('click', function () { setTheme('dark'); });
  if (btnLight) btnLight.addEventListener('click', function () { setTheme('light'); });

  function renderMetrics(data) {
    var el = document.getElementById('metrics-container');
    if (!el || !data) return;
    el.replaceChildren();
    var summary = document.createElement('div');
    summary.textContent = 'Rule: ' + data.alert_rule_name + ' (' + data.alert_rule_state + ') | Total 200s: ' + data.total_http_200 + ' | Total 500s: ' + data.total_http_500;
    el.appendChild(summary);
    if (data.http_500_by_service) {
      var list = document.createElement('div');
      list.style.marginTop = '8px';
      list.style.fontSize = '12px';
      Object.keys(data.http_500_by_service).forEach(function (svc) {
        var row = document.createElement('div');
        var count200 = (data.http_200_by_service && data.http_200_by_service[svc]) || 0;
        var count500 = data.http_500_by_service[svc] || 0;
        row.textContent = svc + ' -> HTTP 200: ' + count200 + ' | HTTP 500: ' + count500;
        list.appendChild(row);
      });
      el.appendChild(list);
    }
  }

  function renderAlerts(data) {
    var el = document.getElementById('alerts-container');
    if (!el || !data) return;
    el.replaceChildren();
    var alerts = Array.isArray(data.alerts) ? data.alerts : [];
    if (alerts.length === 0) {
      var empty = document.createElement('div');
      empty.textContent = 'No firing alerts (' + (data.scrape_window || '5s') + ')';
      el.appendChild(empty);
      return;
    }
    alerts.slice(0, 5).forEach(function (alert) {
      var item = document.createElement('div');
      item.style.marginBottom = '8px';
      item.style.fontSize = '12px';
      var svcs = Array.isArray(alert.services) ? alert.services.join(', ') : '';
      var header = document.createElement('div');
      header.textContent = '[' + (alert.status || '').toUpperCase() + '] ' + (alert.alert_name || 'StorefrontHTTP500Spike') + ' (' + (alert.incident_id || '') + ') -> ' + svcs;
      item.appendChild(header);
      if (alert.triggered_root_task) {
        var taskLine = document.createElement('div');
        taskLine.textContent = 'Root Task: ' + alert.triggered_root_task + ' | Breadcrumbs: ' + ((alert.loki_breadcrumbs && alert.loki_breadcrumbs.length) || 0);
        item.appendChild(taskLine);
      }
      el.appendChild(item);
    });
  }

  function renderRollouts(data) {
    var el = document.getElementById('rollouts-container');
    if (!el || !data) return;
    el.replaceChildren();
    var rollouts = Array.isArray(data.rollouts) ? data.rollouts : [];
    if (rollouts.length === 0) {
      var empty = document.createElement('div');
      empty.textContent = 'No rollouts recorded yet';
      el.appendChild(empty);
      return;
    }
    rollouts.slice(0, 5).forEach(function (ro) {
      var item = document.createElement('div');
      item.style.marginBottom = '8px';
      item.style.fontSize = '12px';
      var updatedNames = Array.isArray(ro.updated_services) ? ro.updated_services.map(function (u) { return u.service; }).join(', ') : '';
      var skippedNames = Array.isArray(ro.skipped_services) ? ro.skipped_services.join(', ') : '';
      item.textContent = '[' + (ro.incident_id || '') + '] Commit ' + (ro.commit_sha || 'HEAD') + ' | Updated: ' + updatedNames + ' | Skipped: ' + skippedNames + ' | Duration: ' + (ro.total_duration_ms || 0) + 'ms (<10s SLA: ' + Boolean(ro.under_ten_second_sla) + ')';
      el.appendChild(item);
    });
  }

  function refresh() {
    fetch('/api/monitoring/metrics').then(function (r) { return r.json(); }).then(renderMetrics).catch(function () {});
    fetch('/api/monitoring/alerts').then(function (r) { return r.json(); }).then(renderAlerts).catch(function () {});
    fetch('/api/monitoring/rollouts').then(function (r) { return r.json(); }).then(renderRollouts).catch(function () {});
  }
  refresh();
  setInterval(refresh, 2000);
})();`
