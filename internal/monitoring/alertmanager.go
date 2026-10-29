package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

const (
	// DefaultInClusterLokiURL is the default in-cluster Loki base URL when LOKI_BASE_URL or LOKI_URL is set to "default".
	DefaultInClusterLokiURL = "http://loki.ax-bug-bash.svc.cluster.local:3100"
	lokiQueryTimeout        = 2 * time.Second
)

// validHarnessModes enumerates the supported Coding Harness modes for runtime configuration.
var validHarnessModes = map[string]bool{
	"antigravity": true,
	"opencode":    true,
	"pi":          true,
	"multi":       true,
}

// StructuredErrorLog represents a structured JSON error log line emitted to stdout and Loki.
type StructuredErrorLog struct {
	Timestamp  string `json:"timestamp"`
	Level      string `json:"level"`
	RequestID  string `json:"request_id,omitempty"`
	IncidentID string `json:"incident_id"`
	Service    string `json:"service"`
	Language   string `json:"language"`
	SourceFile string `json:"source_file"`
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
}

// ToStorefrontLogEntry converts a StructuredErrorLog into a storefront.LogEntry.
func (e StructuredErrorLog) ToStorefrontLogEntry() storefront.LogEntry {
	return storefront.LogEntry{
		Timestamp:  e.Timestamp,
		Level:      e.Level,
		IncidentID: e.IncidentID,
		Service:    e.Service,
		Language:   e.Language,
		SourceFile: e.SourceFile,
		ErrorType:  e.ErrorType,
		Message:    e.Message,
		StackTrace: e.StackTrace,
	}
}

// FromStorefrontLogEntry converts a storefront.LogEntry into a StructuredErrorLog.
func FromStorefrontLogEntry(in storefront.LogEntry) StructuredErrorLog {
	return StructuredErrorLog{
		Timestamp:  in.Timestamp,
		Level:      in.Level,
		IncidentID: in.IncidentID,
		Service:    in.Service,
		Language:   in.Language,
		SourceFile: in.SourceFile,
		ErrorType:  in.ErrorType,
		Message:    in.Message,
		StackTrace: in.StackTrace,
	}
}

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

// EnrichedAlertRecord captures a consolidated Alertmanager incident alert enriched with Loki / Storefront
// structured error log breadcrumbs and linked to the Root AX Task execution that remediates it.
type EnrichedAlertRecord struct {
	Fingerprint          string                `json:"fingerprint"`
	AlertName            string                `json:"alert_name"`
	IncidentID           string                `json:"incident_id"`
	Status               string                `json:"status"`
	Severity             string                `json:"severity"`
	Services             []string              `json:"services"`
	BlastRadiusServices  []string              `json:"blast_radius_services,omitempty"`
	Summary              string                `json:"summary"`
	ErrorCode            string                `json:"error_code,omitempty"`
	ErrorMessage         string                `json:"error_message,omitempty"`
	StackTraces          []string              `json:"stack_traces,omitempty"`
	LokiBreadcrumbs      []storefront.LogEntry `json:"loki_breadcrumbs,omitempty"`
	EnrichedLogs         []StructuredErrorLog  `json:"enriched_logs,omitempty"`
	RequestIDs           []string              `json:"request_ids,omitempty"`
	Duplicate            bool                  `json:"duplicate,omitempty"`
	HarnessMode          string                `json:"harness_mode,omitempty"`
	GCPExportCompatible  bool                  `json:"gcp_export_compatible"`
	EvaluationWindow     string                `json:"evaluation_window"`
	TriggeredExecutionID string                `json:"triggered_execution_id,omitempty"`
	TriggeredRootTask    string                `json:"triggered_root_task,omitempty"`
	RolloutDurationMs    int64                 `json:"rollout_duration_ms,omitempty"`
	StartsAt             time.Time             `json:"starts_at"`
	ResolvedAt           time.Time             `json:"resolved_at,omitempty"`
}

// IncidentAlert is a type alias for EnrichedAlertRecord.
type IncidentAlert = EnrichedAlertRecord

// AlertmanagerWebhookResponse is returned by POST /api/webhooks/alertmanager.
type AlertmanagerWebhookResponse struct {
	Status       string              `json:"status"`
	Deduplicated bool                `json:"deduplicated"`
	Duplicate    bool                `json:"duplicate"`
	ExecutionID  string              `json:"execution_id,omitempty"`
	TreeID       string              `json:"tree_id,omitempty"`
	RootTaskName string              `json:"root_task_name,omitempty"`
	Alert        EnrichedAlertRecord `json:"alert"`
}

// AlertmanagerConfig configures the AlertmanagerBridge / AlertmanagerReceiver.
type AlertmanagerConfig struct {
	Storefront     *storefront.Runtime
	StorefrontURL  string
	LokiURL        string
	LokiBaseURL    string
	LokiPushURL    string
	Orchestrator   *bugbash.Orchestrator
	RolloutBridge  *RolloutBridge
	HarnessMode    string
	RequestedModel string
	HTTPClient     *http.Client
	OnIncident     func(ctx context.Context, req bugbash.TriggerRequest) (*bugbash.BugBashTreeExecution, error)
	OnAlertUpdated func(alert EnrichedAlertRecord)
}

// AlertmanagerOption configures an AlertmanagerBridge / AlertmanagerReceiver.
type AlertmanagerOption func(*AlertmanagerBridge)

// WithLokiURL sets the Loki base URL used to query /loki/api/v1/query_range during alert enrichment.
func WithLokiURL(lokiURL string) AlertmanagerOption {
	return func(b *AlertmanagerBridge) {
		b.SetLokiURL(lokiURL)
	}
}

// AlertmanagerBridge receives upstream Alertmanager v4 webhooks, consolidates multi-service alerts,
// enriches firing alerts with Loki / Storefront structured error logs, deduplicates in-flight alerts,
// and dispatches the Root AX Task.
type AlertmanagerBridge struct {
	cfg        AlertmanagerConfig
	httpClient *http.Client

	mu             sync.RWMutex
	lokiBaseURL    string
	lokiPushURL    string
	defaultHarness string
	requestedModel string
	alerts         []EnrichedAlertRecord
	byFingerprint  map[string]int
	activeByFp     map[string]string
	activeByIncID  map[string]string
}

// AlertmanagerReceiver is a type alias for AlertmanagerBridge.
type AlertmanagerReceiver = AlertmanagerBridge

// NewAlertmanagerBridge creates a new AlertmanagerBridge.
func NewAlertmanagerBridge(cfg AlertmanagerConfig, opts ...AlertmanagerOption) *AlertmanagerBridge {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	lokiBase := firstNonEmpty(cfg.LokiURL, cfg.LokiBaseURL, os.Getenv("LOKI_URL"), os.Getenv("LOKI_BASE_URL"))
	lokiPush := firstNonEmpty(cfg.LokiPushURL, os.Getenv("LOKI_PUSH_URL"))
	harness := strings.ToLower(firstNonEmpty(cfg.HarnessMode, "multi"))
	if !validHarnessModes[harness] {
		harness = "multi"
	}

	b := &AlertmanagerBridge{
		cfg:            cfg,
		httpClient:     client,
		lokiBaseURL:    strings.TrimRight(lokiBase, "/"),
		lokiPushURL:    strings.TrimSpace(lokiPush),
		defaultHarness: harness,
		requestedModel: strings.TrimSpace(cfg.RequestedModel),
		byFingerprint:  make(map[string]int),
		activeByFp:     make(map[string]string),
		activeByIncID:  make(map[string]string),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

// NewAlertmanagerReceiver creates a new AlertmanagerReceiver (alias for NewAlertmanagerBridge).
func NewAlertmanagerReceiver(cfg AlertmanagerConfig, opts ...AlertmanagerOption) *AlertmanagerBridge {
	return NewAlertmanagerBridge(cfg, opts...)
}

// SetLokiURL updates the Loki base URL used for /loki/api/v1/query_range enrichment.
func (b *AlertmanagerBridge) SetLokiURL(lokiURL string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lokiBaseURL = strings.TrimRight(strings.TrimSpace(lokiURL), "/")
}

// LokiURL returns the configured Loki base URL.
func (b *AlertmanagerBridge) LokiURL() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lokiBaseURL
}

// SetDefaultHarness sets the thread-safe runtime default Coding Harness mode ("antigravity" | "opencode" | "pi" | "multi").
func (b *AlertmanagerBridge) SetDefaultHarness(harness string) error {
	norm := strings.ToLower(strings.TrimSpace(harness))
	if !validHarnessModes[norm] {
		return fmt.Errorf("unsupported harness mode %q (must be antigravity, opencode, pi, or multi)", harness)
	}
	b.mu.Lock()
	b.defaultHarness = norm
	b.cfg.HarnessMode = norm
	b.mu.Unlock()
	return nil
}

// GetDefaultHarness returns the current runtime default Coding Harness mode.
func (b *AlertmanagerBridge) GetDefaultHarness() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.defaultHarness == "" {
		return "multi"
	}
	return b.defaultHarness
}

// SetRequestedModel sets the thread-safe runtime requested Gemini model.
func (b *AlertmanagerBridge) SetRequestedModel(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requestedModel = strings.TrimSpace(model)
	b.cfg.RequestedModel = b.requestedModel
}

// GetRequestedModel returns the current runtime requested Gemini model.
func (b *AlertmanagerBridge) GetRequestedModel() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.requestedModel
}

// ResetDeduplication clears active fingerprint and incident deduplication state so subsequent chaos soak runs
// or reset incidents can re-trigger cleanly.
func (b *AlertmanagerBridge) ResetDeduplication() {
	b.ResolveIncidentAlerts("", 0)
	b.mu.Lock()
	b.activeByFp = make(map[string]string)
	b.activeByIncID = make(map[string]string)
	b.mu.Unlock()
}

// SetRolloutBridge attaches a RolloutBridge to automatically roll out verified fixes when the Root Task finishes.
func (b *AlertmanagerBridge) SetRolloutBridge(rb *RolloutBridge) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.RolloutBridge = rb
}

// Handler returns an HTTP handler serving POST /api/webhooks/alertmanager, GET /api/monitoring/alerts, and /api/config/harness.
// TODO(security): Authentication and rate limiting on internal webhook routes are enforced by cluster-internal NetworkPolicy and EgressPolicy.
func (b *AlertmanagerBridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/webhooks/alertmanager", b.HandleWebhook)
	mux.HandleFunc("/api/monitoring/alerts", b.HandleListAlerts)
	mux.HandleFunc("/api/config/harness", b.HandleHarnessConfig)
	return httputil.WithSecurityHeaders(mux)
}

// HandleHarnessConfig handles GET and POST /api/config/harness for runtime default_harness inspection and updates.
func (b *AlertmanagerBridge) HandleHarnessConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"default_harness": b.GetDefaultHarness(),
			"requested_model": b.GetRequestedModel(),
		})
	case http.MethodPost:
		var req struct {
			Harness string `json:"harness"`
			Model   string `json:"model,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
			return
		}
		if err := b.SetDefaultHarness(req.Harness); err != nil {
			httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(req.Model) != "" {
			b.SetRequestedModel(req.Model)
		}
		httputil.WriteJSON(w, http.StatusOK, map[string]any{
			"status":          "updated",
			"default_harness": b.GetDefaultHarness(),
			"requested_model": b.GetRequestedModel(),
		})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// HandleListAlerts handles GET /api/monitoring/alerts.
func (b *AlertmanagerBridge) HandleListAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"alerts":          b.ListAlerts(),
		"latest_alert":    b.LatestAlert(),
		"default_harness": b.GetDefaultHarness(),
		"scrape_window":   "5s / [15s] (for: 0s)",
	})
}

// HandleWebhook handles POST /api/webhooks/alertmanager.
func (b *AlertmanagerBridge) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload AlertmanagerWebhookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
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

// PushLokiErrorLog pushes a StructuredErrorLog entry to Loki (/loki/api/v1/push) with
// {app="retail-storefront", service="<svc>", level="ERROR"} stream labels.
func (b *AlertmanagerBridge) PushLokiErrorLog(ctx context.Context, entry StructuredErrorLog) error {
	b.mu.RLock()
	pushURL := b.lokiPushURL
	baseURL := b.lokiBaseURL
	b.mu.RUnlock()

	if pushURL == "" && baseURL != "" {
		pushURL = baseURL + "/loki/api/v1/push"
	}
	if pushURL == "" {
		return nil
	}

	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if entry.Level == "" {
		entry.Level = "ERROR"
	}
	lineBytes, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	tsNano := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	lokiPayload := map[string]any{
		"streams": []map[string]any{
			{
				"stream": map[string]string{
					"app":         "retail-storefront",
					"service":     entry.Service,
					"level":       entry.Level,
					"incident_id": entry.IncidentID,
				},
				"values": [][]string{
					{tsNano, string(lineBytes)},
				},
			},
		},
	}
	bodyBytes, err := json.Marshal(lokiPayload)
	if err != nil {
		return err
	}

	pushCtx, cancel := context.WithTimeout(ctx, lokiQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pushCtx, http.MethodPost, pushURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("loki push returned status %d", resp.StatusCode)
	}
	return nil
}

// ProcessWebhook consolidates multi-service alerts in an AlertmanagerWebhookPayload, enriches with Loki logs,
// infers IncidentID when omitted by Prometheus, deduplicates in-flight alerts, and dispatches the Root Task.
func (b *AlertmanagerBridge) ProcessWebhook(ctx context.Context, payload AlertmanagerWebhookPayload) (AlertmanagerWebhookResponse, error) {
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	if status == "" && len(payload.Alerts) > 0 {
		for _, a := range payload.Alerts {
			if strings.EqualFold(strings.TrimSpace(a.Status), "firing") {
				status = "firing"
				break
			}
		}
		if status == "" {
			status = strings.ToLower(strings.TrimSpace(payload.Alerts[0].Status))
		}
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

	// Consolidate labels, services, fingerprints, and request_ids across all Alerts[] in the payload
	var alertServices []string
	seenAlertSvc := make(map[string]bool)
	addService := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			svc := strings.TrimSpace(part)
			if svc != "" && !seenAlertSvc[svc] {
				seenAlertSvc[svc] = true
				alertServices = append(alertServices, svc)
			}
		}
	}

	var rawFingerprints []string
	var requestIDs []string
	seenReqID := make(map[string]bool)
	var labelIncidentID string
	var earliestStart time.Time

	for _, a := range payload.Alerts {
		addService(a.Labels["service"])
		addService(a.Labels["services"])
		if fp := strings.TrimSpace(a.Fingerprint); fp != "" {
			rawFingerprints = append(rawFingerprints, fp)
		}
		if reqID := firstNonEmpty(a.Labels["request_id"], a.Annotations["request_id"]); reqID != "" && !seenReqID[reqID] {
			seenReqID[reqID] = true
			requestIDs = append(requestIDs, reqID)
		}
		if labelIncidentID == "" {
			cand := strings.ToUpper(firstNonEmpty(a.Labels["incident"], a.Labels["incident_id"]))
			if cand != "" && cand != "UNKNOWN" {
				labelIncidentID = cand
			}
		}
		if !a.StartsAt.IsZero() && (earliestStart.IsZero() || a.StartsAt.Before(earliestStart)) {
			earliestStart = a.StartsAt
		}
	}
	addService(payload.CommonLabels["service"])
	addService(payload.CommonLabels["services"])
	addService(payload.GroupLabels["service"])

	alertName := firstNonEmpty(
		alertItem.Labels["alertname"],
		payload.CommonLabels["alertname"],
		payload.GroupLabels["alertname"],
		"StorefrontServiceHttp500Spike",
	)
	severity := firstNonEmpty(
		alertItem.Labels["severity"],
		payload.CommonLabels["severity"],
		"critical",
	)

	incidentID := labelIncidentID
	if incidentID == "" {
		cand := strings.ToUpper(firstNonEmpty(
			payload.CommonLabels["incident"],
			payload.CommonLabels["incident_id"],
			payload.GroupLabels["incident"],
			payload.GroupLabels["incident_id"],
		))
		if cand != "" && cand != "UNKNOWN" {
			incidentID = cand
		}
	}

	// Fetch enriched logs from Loki /loki/api/v1/query_range (strict 2s timeout) or Storefront fallback
	enrichedLogs := b.fetchEnrichedLogs(ctx, incidentID)

	// Infer IncidentID from enriched Loki logs if not explicitly set on Alertmanager labels
	if incidentID == "" {
		incidentID = inferIncidentIDFromLogs(enrichedLogs)
	}

	// Collect all faulted services from Alertmanager labels + Loki logs
	var services []string
	seenSvc := make(map[string]bool)
	for _, svc := range alertServices {
		if !seenSvc[svc] {
			seenSvc[svc] = true
			services = append(services, svc)
		}
	}
	for _, entry := range enrichedLogs {
		if entry.IncidentID != "" && incidentID != "" && !strings.EqualFold(entry.IncidentID, incidentID) {
			continue
		}
		if entry.Service != "" && !seenSvc[entry.Service] {
			seenSvc[entry.Service] = true
			services = append(services, entry.Service)
		}
		if entry.RequestID != "" && !seenReqID[entry.RequestID] {
			seenReqID[entry.RequestID] = true
			requestIDs = append(requestIDs, entry.RequestID)
		}
	}

	// Infer IncidentID from faulted BlastRadiusServices signature if still unknown
	if incidentID == "" && len(services) > 0 {
		incidentID = inferIncidentIDFromServices(services)
	}
	if incidentID == "" && b.cfg.Storefront != nil {
		if active := b.cfg.Storefront.ActiveScenario(); active != nil {
			incidentID = active.ID
		}
	}
	if incidentID == "" {
		incidentID = storefront.IncidentStackedPromoNegativeSubtotal
	}

	sc, hasScenario := storefront.GetScenario(incidentID)

	// Build deterministic consolidated fingerprint
	var fingerprint string
	if len(rawFingerprints) == 1 {
		fingerprint = rawFingerprints[0]
	} else if len(rawFingerprints) > 1 {
		sorted := append([]string(nil), rawFingerprints...)
		sort.Strings(sorted)
		fingerprint = fmt.Sprintf("fp-consolidated-%s-%s", strings.ToLower(incidentID), strings.Join(sorted, "-"))
	} else {
		fingerprint = fmt.Sprintf("fp-%s-%s", strings.ToLower(alertName), strings.ToLower(incidentID))
	}

	// Filter enrichedLogs and collect stack traces for the resolved incident
	var filteredLogs []StructuredErrorLog
	var breadcrumbs []storefront.LogEntry
	var stackTraces []string
	seenTrace := make(map[string]bool)

	for _, entry := range enrichedLogs {
		if entry.IncidentID != "" && !strings.EqualFold(entry.IncidentID, incidentID) {
			continue
		}
		if entry.IncidentID == "" {
			entry.IncidentID = incidentID
		}
		filteredLogs = append(filteredLogs, entry)
		breadcrumbs = append(breadcrumbs, entry.ToStorefrontLogEntry())
		if entry.StackTrace != "" && !seenTrace[entry.StackTrace] {
			seenTrace[entry.StackTrace] = true
			stackTraces = append(stackTraces, entry.StackTrace)
		}
	}

	if hasScenario {
		if len(services) == 0 || (len(payload.Alerts) <= 1 && len(services) < len(sc.ImplicatedServices)) {
			for _, svc := range sc.ImplicatedServices {
				if !seenSvc[svc] {
					seenSvc[svc] = true
					services = append(services, svc)
				}
			}
		}
		if len(stackTraces) == 0 {
			for _, svc := range sc.ImplicatedServices {
				if d, ok := sc.ServiceTraces[svc]; ok && d.StackTrace != "" && !seenTrace[d.StackTrace] {
					seenTrace[d.StackTrace] = true
					stackTraces = append(stackTraces, d.StackTrace)
				}
			}
		}
		if len(filteredLogs) == 0 {
			nowStr := time.Now().UTC().Format(time.RFC3339Nano)
			targetSvcs := services
			if len(targetSvcs) == 0 {
				targetSvcs = sc.ImplicatedServices
			}
			for _, svc := range targetSvcs {
				if d, ok := sc.ServiceTraces[svc]; ok {
					entry := StructuredErrorLog{
						Timestamp:  nowStr,
						Level:      "ERROR",
						IncidentID: sc.ID,
						Service:    svc,
						Language:   d.Language,
						SourceFile: d.SourceFile,
						ErrorType:  d.ErrorType,
						Message:    d.ErrorMessage,
						StackTrace: d.StackTrace,
					}
					filteredLogs = append(filteredLogs, entry)
					breadcrumbs = append(breadcrumbs, entry.ToStorefrontLogEntry())
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

	startsAt := earliestStart
	if startsAt.IsZero() {
		startsAt = time.Now().UTC()
	}

	if status == "resolved" {
		b.ResolveIncidentAlerts(incidentID, 0)
		b.mu.RLock()
		var resolvedRec EnrichedAlertRecord
		if idx, ok := b.byFingerprint[fingerprint]; ok && idx < len(b.alerts) {
			resolvedRec = cloneAlertRecord(b.alerts[idx])
		} else {
			resolvedRec = EnrichedAlertRecord{
				Fingerprint:         fingerprint,
				AlertName:           alertName,
				IncidentID:          incidentID,
				Status:              "resolved",
				Severity:            severity,
				Services:            services,
				BlastRadiusServices: append([]string(nil), services...),
				Summary:             summary,
				ErrorCode:           errCode,
				ErrorMessage:        errMsg,
				StackTraces:         stackTraces,
				LokiBreadcrumbs:     breadcrumbs,
				EnrichedLogs:        filteredLogs,
				RequestIDs:          requestIDs,
				GCPExportCompatible: true,
				EvaluationWindow:    "5s / [15s] (for: 0s)",
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
		existingAlert := cloneAlertRecord(b.alerts[idx])
		existingAlert.Duplicate = true
		b.mu.Unlock()
		return AlertmanagerWebhookResponse{
			Status:       "deduplicated",
			Deduplicated: true,
			Duplicate:    true,
			ExecutionID:  existingExecID,
			TreeID:       existingExecID,
			RootTaskName: existingAlert.TriggeredRootTask,
			Alert:        existingAlert,
		}, nil
	}
	if existingExecID, ok := b.activeByIncID[incidentID]; ok && existingExecID != "" {
		for _, rec := range b.alerts {
			if rec.TriggeredExecutionID == existingExecID || strings.EqualFold(rec.IncidentID, incidentID) {
				existingAlert := cloneAlertRecord(rec)
				existingAlert.Duplicate = true
				b.mu.Unlock()
				return AlertmanagerWebhookResponse{
					Status:       "deduplicated",
					Deduplicated: true,
					Duplicate:    true,
					ExecutionID:  existingExecID,
					TreeID:       existingExecID,
					RootTaskName: existingAlert.TriggeredRootTask,
					Alert:        existingAlert,
				}, nil
			}
		}
	}

	harnessMode := strings.ToLower(firstNonEmpty(
		alertItem.Labels["harness"],
		payload.CommonLabels["harness"],
		b.defaultHarness,
		b.cfg.HarnessMode,
		"multi",
	))
	if !validHarnessModes[harnessMode] {
		harnessMode = "multi"
	}
	requestedModel := firstNonEmpty(
		alertItem.Labels["model"],
		payload.CommonLabels["model"],
		b.requestedModel,
		b.cfg.RequestedModel,
	)

	record := EnrichedAlertRecord{
		Fingerprint:         fingerprint,
		AlertName:           alertName,
		IncidentID:          incidentID,
		Status:              "firing",
		Severity:            severity,
		Services:            services,
		BlastRadiusServices: append([]string(nil), services...),
		Summary:             summary,
		ErrorCode:           errCode,
		ErrorMessage:        errMsg,
		StackTraces:         stackTraces,
		LokiBreadcrumbs:     breadcrumbs,
		EnrichedLogs:        filteredLogs,
		RequestIDs:          requestIDs,
		HarnessMode:         harnessMode,
		GCPExportCompatible: true,
		EvaluationWindow:    "5s / [15s] (for: 0s)",
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
		if treeExec.ExecutionID != "" {
			b.activeByFp[fingerprint] = treeExec.ExecutionID
			b.activeByIncID[incidentID] = treeExec.ExecutionID
		}
	}
	b.alerts[idx] = record
	rb := b.cfg.RolloutBridge
	onUpdate := b.cfg.OnAlertUpdated
	b.mu.Unlock()

	if rb != nil && treeExec != nil {
		if report, err := rb.RolloutTreeFix(ctx, treeExec); err == nil {
			b.ResolveIncidentAlerts(incidentID, report.TotalDurationMs)
			b.mu.RLock()
			record = cloneAlertRecord(b.alerts[idx])
			b.mu.RUnlock()
		}
	}

	if onUpdate != nil {
		onUpdate(record)
	}

	return AlertmanagerWebhookResponse{
		Status:       "dispatched",
		Deduplicated: false,
		Duplicate:    false,
		ExecutionID:  record.TriggeredExecutionID,
		TreeID:       record.TriggeredExecutionID,
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
			updated = append(updated, cloneAlertRecord(b.alerts[i]))
		}
	}
	if incidentID != "" {
		delete(b.activeByIncID, strings.ToUpper(strings.TrimSpace(incidentID)))
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

func (b *AlertmanagerBridge) fetchEnrichedLogs(ctx context.Context, incidentID string) []StructuredErrorLog {
	// 1. Try querying Loki /loki/api/v1/query_range if LokiURL is configured (strict 2s timeout)
	lokiBase := b.LokiURL()
	if lokiBase != "" {
		if entries := b.queryLokiRange(ctx, lokiBase); len(entries) > 0 {
			return entries
		}
	}

	// 2. Try querying GET /api/logs over HTTP if StorefrontURL is configured
	if strings.TrimSpace(b.cfg.StorefrontURL) != "" {
		u := strings.TrimRight(strings.TrimSpace(b.cfg.StorefrontURL), "/") + "/api/logs"
		if incidentID != "" && incidentID != "UNKNOWN" {
			u += "?incident_id=" + url.QueryEscape(incidentID)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			resp, err := b.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var body struct {
						Entries []StructuredErrorLog `json:"entries"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && len(body.Entries) > 0 {
						return body.Entries
					}
				}
			}
		}
	}

	// 3. Fall back to in-process StorefrontRuntime.Logs(...)
	if b.cfg.Storefront != nil {
		raw := b.cfg.Storefront.Logs(incidentID)
		if len(raw) > 0 {
			out := make([]StructuredErrorLog, 0, len(raw))
			for _, item := range raw {
				out = append(out, FromStorefrontLogEntry(item))
			}
			return out
		}
	}
	return nil
}

func (b *AlertmanagerBridge) queryLokiRange(ctx context.Context, lokiBase string) []StructuredErrorLog {
	lokiCtx, cancel := context.WithTimeout(ctx, lokiQueryTimeout)
	defer cancel()

	queries := []string{
		`{app="storefront"} |= "HTTP 500"`,
		`{app="retail-storefront"} |= "HTTP 500"`,
		`{namespace="ax-bug-bash"} |= "HTTP 500"`,
	}

	now := time.Now().UTC()
	startNano := strconv.FormatInt(now.Add(-60*time.Second).UnixNano(), 10)
	endNano := strconv.FormatInt(now.Add(5*time.Second).UnixNano(), 10)

	client := &http.Client{Timeout: lokiQueryTimeout}
	for _, logQL := range queries {
		if lokiCtx.Err() != nil {
			break
		}
		params := url.Values{}
		params.Set("query", logQL)
		params.Set("limit", "100")
		params.Set("start", startNano)
		params.Set("end", endNano)
		params.Set("direction", "backward")

		endpoint := strings.TrimRight(lokiBase, "/") + "/loki/api/v1/query_range?" + params.Encode()
		req, err := http.NewRequestWithContext(lokiCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			if lokiCtx.Err() != nil {
				break
			}
			continue
		}
		var lokiResp struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Stream map[string]string `json:"stream"`
					Values [][]string        `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&lokiResp)
		_ = resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}

		var entries []StructuredErrorLog
		for _, stream := range lokiResp.Data.Result {
			for _, valPair := range stream.Values {
				if len(valPair) < 2 {
					continue
				}
				rawLine := strings.TrimSpace(valPair[1])
				if rawLine == "" {
					continue
				}
				var parsed StructuredErrorLog
				if err := json.Unmarshal([]byte(rawLine), &parsed); err != nil {
					parsed = StructuredErrorLog{
						Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
						Level:      firstNonEmpty(stream.Stream["level"], "ERROR"),
						IncidentID: strings.ToUpper(stream.Stream["incident_id"]),
						Service:    stream.Stream["service"],
						Language:   storefront.ServiceLanguage(stream.Stream["service"]),
						Message:    rawLine,
						StackTrace: rawLine,
					}
				} else {
					if parsed.Service == "" {
						parsed.Service = stream.Stream["service"]
					}
					if parsed.IncidentID == "" {
						parsed.IncidentID = strings.ToUpper(stream.Stream["incident_id"])
					}
					if parsed.Level == "" {
						parsed.Level = firstNonEmpty(stream.Stream["level"], "ERROR")
					}
					if parsed.Language == "" && parsed.Service != "" {
						parsed.Language = storefront.ServiceLanguage(parsed.Service)
					}
				}
				entries = append(entries, parsed)
			}
		}
		if len(entries) > 0 {
			return entries
		}
	}
	return nil
}

func inferIncidentIDFromLogs(logs []StructuredErrorLog) string {
	for i := len(logs) - 1; i >= 0; i-- {
		cand := strings.ToUpper(strings.TrimSpace(logs[i].IncidentID))
		if cand != "" && cand != "UNKNOWN" {
			if _, ok := storefront.GetScenario(cand); ok {
				return cand
			}
		}
	}
	for _, l := range logs {
		combined := l.ErrorType + " " + l.Message + " " + l.StackTrace
		switch {
		case strings.Contains(combined, "NegativeSubtotalError") || strings.Contains(combined, "CarrierTariffBoundsError") || strings.Contains(combined, "SAVE20"):
			return storefront.IncidentStackedPromoNegativeSubtotal
		case strings.Contains(combined, "RustU32UnderflowPanic") || strings.Contains(combined, "FlashBundleUnderflowError") || strings.Contains(combined, "reserve_bundle_stock"):
			return storefront.IncidentFlashSaleBundleUnderflow
		case strings.Contains(combined, "CurrencyPrecisionMismatchError") || strings.Contains(combined, "PaymentCapturePrecisionError") || strings.Contains(combined, "StorefrontCurrencyPrecisionError") || strings.Contains(combined, "4476.15"):
			return storefront.IncidentZeroDecimalJPYRounding
		case strings.Contains(combined, "UnhandledHazmatClassError") || strings.Contains(combined, "HazmatWeightTierOverflowError") || strings.Contains(combined, "UN3480-LITHIUM"):
			return storefront.IncidentLithiumHazmatWeightTier
		}
	}
	return ""
}

func inferIncidentIDFromServices(services []string) string {
	has := make(map[string]bool, len(services))
	for _, s := range services {
		has[strings.TrimSpace(s)] = true
	}
	switch {
	case has[storefront.ServicePricing] && has[storefront.ServiceShipping] && !has[storefront.ServiceInventory]:
		return storefront.IncidentStackedPromoNegativeSubtotal
	case has[storefront.ServiceInventory] && has[storefront.ServiceCheckout] && !has[storefront.ServiceShipping]:
		return storefront.IncidentFlashSaleBundleUnderflow
	case has[storefront.ServicePricing] && has[storefront.ServiceCheckout] && !has[storefront.ServiceShipping] && !has[storefront.ServiceInventory]:
		return storefront.IncidentZeroDecimalJPYRounding
	case has[storefront.ServiceInventory] && has[storefront.ServiceShipping]:
		return storefront.IncidentLithiumHazmatWeightTier
	case len(has) >= 3:
		return storefront.IncidentLithiumHazmatWeightTier
	default:
		return ""
	}
}

func cloneAlertRecord(in EnrichedAlertRecord) EnrichedAlertRecord {
	out := in
	out.Services = append([]string(nil), in.Services...)
	out.BlastRadiusServices = append([]string(nil), in.BlastRadiusServices...)
	out.StackTraces = append([]string(nil), in.StackTraces...)
	out.LokiBreadcrumbs = append([]storefront.LogEntry(nil), in.LokiBreadcrumbs...)
	out.EnrichedLogs = append([]StructuredErrorLog(nil), in.EnrichedLogs...)
	out.RequestIDs = append([]string(nil), in.RequestIDs...)
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
