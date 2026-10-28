package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// AlertmanagerWebhookPayload represents the standard Prometheus Alertmanager v4 webhook JSON payload.
type AlertmanagerWebhookPayload struct {
	Version           string              `json:"version"`
	GroupKey          string              `json:"groupKey"`
	TruncatedAlerts   int                 `json:"truncatedAlerts,omitempty"`
	Status            string              `json:"status"`
	Receiver          string              `json:"receiver"`
	GroupLabels       map[string]string   `json:"groupLabels"`
	CommonLabels      map[string]string   `json:"commonLabels"`
	CommonAnnotations map[string]string   `json:"commonAnnotations"`
	ExternalURL       string              `json:"externalURL,omitempty"`
	Alerts            []AlertmanagerAlert `json:"alerts"`
}

// AlertmanagerAlert represents an individual firing or resolved alert inside an Alertmanager v4 webhook.
type AlertmanagerAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
	Fingerprint  string            `json:"fingerprint"`
}

// EnrichedAlertRecord captures an Alertmanager alert enriched with Loki / Storefront structured error log breadcrumbs
// and linked to the Root AX Task execution that remediates it.
type EnrichedAlertRecord struct {
	Fingerprint          string                `json:"fingerprint"`
	AlertName            string                `json:"alert_name"`
	IncidentID           string                `json:"incident_id"`
	Status               string                `json:"status"`
	Severity             string                `json:"severity"`
	Services             []string              `json:"services"`
	Summary              string                `json:"summary"`
	ErrorCode            string                `json:"error_code,omitempty"`
	ErrorMessage         string                `json:"error_message,omitempty"`
	StackTraces          []string              `json:"stack_traces,omitempty"`
	LokiBreadcrumbs      []storefront.LogEntry `json:"loki_breadcrumbs,omitempty"`
	GCPExportCompatible  bool                  `json:"gcp_export_compatible"`
	EvaluationWindow     string                `json:"evaluation_window"`
	TriggeredExecutionID string                `json:"triggered_execution_id,omitempty"`
	TriggeredRootTask    string                `json:"triggered_root_task,omitempty"`
	RolloutDurationMs    int64                 `json:"rollout_duration_ms,omitempty"`
	StartsAt             time.Time             `json:"starts_at"`
	ResolvedAt           time.Time             `json:"resolved_at,omitempty"`
}

// AlertmanagerWebhookResponse is returned by POST /api/webhooks/alertmanager.
type AlertmanagerWebhookResponse struct {
	Status       string              `json:"status"`
	Deduplicated bool                `json:"deduplicated"`
	ExecutionID  string              `json:"execution_id,omitempty"`
	RootTaskName string              `json:"root_task_name,omitempty"`
	Alert        EnrichedAlertRecord `json:"alert"`
}

// AlertmanagerConfig configures the AlertmanagerBridge.
type AlertmanagerConfig struct {
	Storefront     *storefront.Runtime
	StorefrontURL  string
	Orchestrator   *bugbash.Orchestrator
	RolloutBridge  *RolloutBridge
	HarnessMode    string
	RequestedModel string
	HTTPClient     *http.Client
	OnIncident     func(ctx context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error)
	OnAlertUpdated func(alert EnrichedAlertRecord)
}

// AlertmanagerBridge receives upstream Alertmanager v4 webhooks, enriches firing alerts with Loki / Storefront
// structured error logs, deduplicates in-flight alerts by fingerprint, and dispatches the Root AX Task.
type AlertmanagerBridge struct {
	cfg        AlertmanagerConfig
	httpClient *http.Client

	mu             sync.RWMutex
	alerts         []EnrichedAlertRecord
	byFingerprint  map[string]int
	activeByFp     map[string]string
	activeByIncID  map[string]string
	rolloutHookSet bool
}

// NewAlertmanagerBridge creates a new AlertmanagerBridge.
func NewAlertmanagerBridge(cfg AlertmanagerConfig) *AlertmanagerBridge {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &AlertmanagerBridge{
		cfg:           cfg,
		httpClient:    client,
		byFingerprint: make(map[string]int),
		activeByFp:    make(map[string]string),
		activeByIncID: make(map[string]string),
	}
}

// SetRolloutBridge attaches a RolloutBridge to automatically roll out verified fixes when the Root Task finishes.
func (b *AlertmanagerBridge) SetRolloutBridge(rb *RolloutBridge) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.RolloutBridge = rb
}

// Handler returns an HTTP handler serving POST /api/webhooks/alertmanager and GET /api/monitoring/alerts.
func (b *AlertmanagerBridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/webhooks/alertmanager", b.HandleWebhook)
	mux.HandleFunc("/api/monitoring/alerts", b.HandleListAlerts)
	return httputil.WithSecurityHeaders(mux)
}

// HandleListAlerts handles GET /api/monitoring/alerts.
func (b *AlertmanagerBridge) HandleListAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"alerts":        b.ListAlerts(),
		"latest_alert":  b.LatestAlert(),
		"scrape_window": "5s (for: 0s)",
	})
}

// HandleWebhook handles POST /api/webhooks/alertmanager.
func (b *AlertmanagerBridge) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload AlertmanagerWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Alertmanager v4 JSON payload"})
		return
	}
	if len(payload.Alerts) == 0 && payload.Status == "" {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "alerts list cannot be empty"})
		return
	}

	resp, err := b.ProcessWebhook(r.Context(), payload)
	if err != nil {
		httputil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// ProcessWebhook enriches an AlertmanagerWebhookPayload with Loki logs, deduplicates in-flight alerts,
// and dispatches the Root Task on google/ax when firing.
func (b *AlertmanagerBridge) ProcessWebhook(ctx context.Context, payload AlertmanagerWebhookPayload) (AlertmanagerWebhookResponse, error) {
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	if status == "" && len(payload.Alerts) > 0 {
		status = strings.ToLower(strings.TrimSpace(payload.Alerts[0].Status))
	}
	if status == "" {
		status = "firing"
	}

	alertItem := AlertmanagerAlert{Status: status}
	if len(payload.Alerts) > 0 {
		alertItem = payload.Alerts[0]
		if alertItem.Status == "" {
			alertItem.Status = status
		}
	}

	alertName := firstNonEmpty(
		alertItem.Labels["alertname"],
		payload.CommonLabels["alertname"],
		payload.GroupLabels["alertname"],
		"StorefrontHTTP500Spike",
	)
	severity := firstNonEmpty(
		alertItem.Labels["severity"],
		payload.CommonLabels["severity"],
		"critical",
	)
	incidentID := strings.ToUpper(firstNonEmpty(
		alertItem.Labels["incident"],
		alertItem.Labels["incident_id"],
		payload.CommonLabels["incident"],
		payload.CommonLabels["incident_id"],
		payload.GroupLabels["incident"],
	))
	if incidentID == "" && b.cfg.Storefront != nil {
		if active := b.cfg.Storefront.ActiveScenario(); active != nil {
			incidentID = active.ID
		}
	}

	breadcrumbs := b.fetchLokiBreadcrumbs(ctx, incidentID)
	if incidentID == "" && len(breadcrumbs) > 0 {
		incidentID = breadcrumbs[len(breadcrumbs)-1].IncidentID
	}
	if incidentID == "" {
		incidentID = storefront.IncidentStackedPromoNegativeSubtotal
	}

	sc, hasScenario := storefront.GetScenario(incidentID)
	fingerprint := strings.TrimSpace(alertItem.Fingerprint)
	if fingerprint == "" {
		fingerprint = fmt.Sprintf("fp-%s-%s", strings.ToLower(alertName), strings.ToLower(incidentID))
	}

	// Collect implicated services and stack traces from Loki breadcrumbs + scenario metadata
	var services []string
	var stackTraces []string
	seenSvc := make(map[string]bool)
	seenTrace := make(map[string]bool)

	for _, entry := range breadcrumbs {
		if entry.IncidentID != "" && !strings.EqualFold(entry.IncidentID, incidentID) {
			continue
		}
		if entry.Service != "" && !seenSvc[entry.Service] {
			seenSvc[entry.Service] = true
			services = append(services, entry.Service)
		}
		if entry.StackTrace != "" && !seenTrace[entry.StackTrace] {
			seenTrace[entry.StackTrace] = true
			stackTraces = append(stackTraces, entry.StackTrace)
		}
	}

	if hasScenario {
		if len(services) == 0 {
			services = append(services, sc.ImplicatedServices...)
		}
		if len(stackTraces) == 0 {
			for _, svc := range sc.ImplicatedServices {
				if d, ok := sc.ServiceTraces[svc]; ok && d.StackTrace != "" && !seenTrace[d.StackTrace] {
					seenTrace[d.StackTrace] = true
					stackTraces = append(stackTraces, d.StackTrace)
				}
			}
		}
		if len(breadcrumbs) == 0 {
			nowStr := time.Now().UTC().Format(time.RFC3339Nano)
			for _, svc := range sc.ImplicatedServices {
				if d, ok := sc.ServiceTraces[svc]; ok {
					breadcrumbs = append(breadcrumbs, storefront.LogEntry{
						Timestamp:  nowStr,
						Level:      "ERROR",
						IncidentID: sc.ID,
						Service:    svc,
						Language:   d.Language,
						SourceFile: d.SourceFile,
						ErrorType:  d.ErrorType,
						Message:    d.ErrorMessage,
						StackTrace: d.StackTrace,
					})
				}
			}
		}
	}

	summary := firstNonEmpty(
		alertItem.Annotations["summary"],
		payload.CommonAnnotations["summary"],
		sc.Summary,
		fmt.Sprintf("%s firing on %s", alertName, incidentID),
	)
	errCode := firstNonEmpty(
		alertItem.Annotations["error_code"],
		payload.CommonAnnotations["error_code"],
		sc.ErrorCode,
	)
	errMsg := firstNonEmpty(
		alertItem.Annotations["description"],
		payload.CommonAnnotations["description"],
		sc.ErrorMessage,
	)

	startsAt := alertItem.StartsAt
	if startsAt.IsZero() {
		startsAt = time.Now().UTC()
	}

	if status == "resolved" {
		b.ResolveIncidentAlerts(incidentID, 0)
		b.mu.RLock()
		var resolvedRec EnrichedAlertRecord
		if idx, ok := b.byFingerprint[fingerprint]; ok && idx < len(b.alerts) {
			resolvedRec = b.alerts[idx]
		} else {
			resolvedRec = EnrichedAlertRecord{
				Fingerprint:         fingerprint,
				AlertName:           alertName,
				IncidentID:          incidentID,
				Status:              "resolved",
				Severity:            severity,
				Services:            services,
				Summary:             summary,
				ErrorCode:           errCode,
				ErrorMessage:        errMsg,
				StackTraces:         stackTraces,
				LokiBreadcrumbs:     breadcrumbs,
				GCPExportCompatible: true,
				EvaluationWindow:    "5s (for: 0s)",
				StartsAt:            startsAt,
				ResolvedAt:          time.Now().UTC(),
			}
		}
		b.mu.RUnlock()
		return AlertmanagerWebhookResponse{
			Status: "resolved",
			Alert:  resolvedRec,
		}, nil
	}

	// Deduplicate in-flight firing alerts per fingerprint or active incident ID
	b.mu.Lock()
	if existingExecID, ok := b.activeByFp[fingerprint]; ok && existingExecID != "" {
		idx := b.byFingerprint[fingerprint]
		existingAlert := b.alerts[idx]
		b.mu.Unlock()
		return AlertmanagerWebhookResponse{
			Status:       "deduplicated",
			Deduplicated: true,
			ExecutionID:  existingExecID,
			RootTaskName: existingAlert.TriggeredRootTask,
			Alert:        existingAlert,
		}, nil
	}
	if existingExecID, ok := b.activeByIncID[incidentID]; ok && existingExecID != "" {
		for _, rec := range b.alerts {
			if rec.TriggeredExecutionID == existingExecID {
				b.mu.Unlock()
				return AlertmanagerWebhookResponse{
					Status:       "deduplicated",
					Deduplicated: true,
					ExecutionID:  existingExecID,
					RootTaskName: rec.TriggeredRootTask,
					Alert:        rec,
				}, nil
			}
		}
	}

	record := EnrichedAlertRecord{
		Fingerprint:         fingerprint,
		AlertName:           alertName,
		IncidentID:          incidentID,
		Status:              "firing",
		Severity:            severity,
		Services:            services,
		Summary:             summary,
		ErrorCode:           errCode,
		ErrorMessage:        errMsg,
		StackTraces:         stackTraces,
		LokiBreadcrumbs:     breadcrumbs,
		GCPExportCompatible: true,
		EvaluationWindow:    "5s (for: 0s)",
		StartsAt:            startsAt,
	}
	idx, exists := b.byFingerprint[fingerprint]
	if exists {
		b.alerts[idx] = record
	} else {
		idx = len(b.alerts)
		b.alerts = append(b.alerts, record)
		b.byFingerprint[fingerprint] = idx
	}
	b.activeByFp[fingerprint] = "pending"
	b.activeByIncID[incidentID] = "pending"
	b.mu.Unlock()

	harnessMode := firstNonEmpty(
		alertItem.Labels["harness"],
		payload.CommonLabels["harness"],
		b.cfg.HarnessMode,
		"multi",
	)
	requestedModel := firstNonEmpty(
		alertItem.Labels["model"],
		payload.CommonLabels["model"],
		b.cfg.RequestedModel,
	)

	treeExec, err := b.HandleIncident(ctx, bugbash.TriggerRequest{
		IncidentID:     incidentID,
		HarnessMode:    harnessMode,
		RequestedModel: requestedModel,
		AlertName:      alertName,
		ErrorMessage:   errMsg,
		StackTraces:    stackTraces,
	})
	if err != nil {
		b.mu.Lock()
		delete(b.activeByFp, fingerprint)
		delete(b.activeByIncID, incidentID)
		b.mu.Unlock()
		return AlertmanagerWebhookResponse{}, err
	}

	b.mu.Lock()
	if treeExec != nil {
		record.TriggeredExecutionID = treeExec.ExecutionID
		record.TriggeredRootTask = treeExec.RootTask.TaskName
		record.RolloutDurationMs = treeExec.RolloutDurationMs
		b.activeByFp[fingerprint] = treeExec.ExecutionID
		b.activeByIncID[incidentID] = treeExec.ExecutionID
	}
	b.alerts[idx] = record
	rb := b.cfg.RolloutBridge
	onUpdate := b.cfg.OnAlertUpdated
	b.mu.Unlock()

	if rb != nil && treeExec != nil {
		if report, err := rb.RolloutTreeFix(ctx, treeExec); err == nil {
			b.ResolveIncidentAlerts(incidentID, report.TotalDurationMs)
			b.mu.RLock()
			record = b.alerts[idx]
			b.mu.RUnlock()
		}
	}

	if onUpdate != nil {
		onUpdate(record)
	}

	return AlertmanagerWebhookResponse{
		Status:       "dispatched",
		Deduplicated: false,
		ExecutionID:  record.TriggeredExecutionID,
		RootTaskName: record.TriggeredRootTask,
		Alert:        record,
	}, nil
}

// HandleIncident dispatches the Root Task on bugbash.Orchestrator (or the configured OnIncident hook).
func (b *AlertmanagerBridge) HandleIncident(ctx context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error) {
	if b.cfg.OnIncident != nil {
		return b.cfg.OnIncident(ctx, req)
	}
	if b.cfg.Orchestrator != nil {
		return b.cfg.Orchestrator.TriggerIncidentBugBash(ctx, req)
	}
	return nil, nil
}

// ResolveIncidentAlerts transitions firing alerts for incidentID (or all firing alerts if empty) to resolved.
func (b *AlertmanagerBridge) ResolveIncidentAlerts(incidentID string, rolloutDurationMs int64) {
	b.mu.Lock()
	var updated []EnrichedAlertRecord
	now := time.Now().UTC()
	for i := range b.alerts {
		if incidentID == "" || strings.EqualFold(b.alerts[i].IncidentID, incidentID) {
			b.alerts[i].Status = "resolved"
			b.alerts[i].ResolvedAt = now
			if rolloutDurationMs > 0 {
				b.alerts[i].RolloutDurationMs = rolloutDurationMs
			}
			delete(b.activeByFp, b.alerts[i].Fingerprint)
			delete(b.activeByIncID, b.alerts[i].IncidentID)
			updated = append(updated, b.alerts[i])
		}
	}
	onUpdate := b.cfg.OnAlertUpdated
	b.mu.Unlock()

	if onUpdate != nil {
		for _, rec := range updated {
			onUpdate(rec)
		}
	}
}

// ListAlerts returns all recorded EnrichedAlertRecords in reverse chronological order.
func (b *AlertmanagerBridge) ListAlerts() []EnrichedAlertRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]EnrichedAlertRecord, 0, len(b.alerts))
	for i := len(b.alerts) - 1; i >= 0; i-- {
		out = append(out, cloneAlertRecord(b.alerts[i]))
	}
	return out
}

// LatestAlert returns the most recent EnrichedAlertRecord, or nil if none exists.
func (b *AlertmanagerBridge) LatestAlert() *EnrichedAlertRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.alerts) == 0 {
		return nil
	}
	rec := cloneAlertRecord(b.alerts[len(b.alerts)-1])
	return &rec
}

func (b *AlertmanagerBridge) fetchLokiBreadcrumbs(ctx context.Context, incidentID string) []storefront.LogEntry {
	// 1. Try querying GET /api/logs over HTTP if StorefrontURL is configured
	if strings.TrimSpace(b.cfg.StorefrontURL) != "" {
		u := strings.TrimRight(strings.TrimSpace(b.cfg.StorefrontURL), "/") + "/api/logs"
		if incidentID != "" {
			u += "?incident_id=" + url.QueryEscape(incidentID)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			resp, err := b.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var body struct {
						Entries []storefront.LogEntry `json:"entries"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && len(body.Entries) > 0 {
						return body.Entries
					}
				}
			}
		}
	}
	// 2. Fall back to in-process StorefrontRuntime.Logs(...)
	if b.cfg.Storefront != nil {
		return b.cfg.Storefront.Logs(incidentID)
	}
	return nil
}

func cloneAlertRecord(in EnrichedAlertRecord) EnrichedAlertRecord {
	out := in
	out.Services = append([]string(nil), in.Services...)
	out.StackTraces = append([]string(nil), in.StackTraces...)
	out.LokiBreadcrumbs = append([]storefront.LogEntry(nil), in.LokiBreadcrumbs...)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
