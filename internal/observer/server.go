package observer

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/monitoring"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

//go:embed static/*
var staticFS embed.FS

// Config configures the AX Observer HTTP server (:3000).
type Config struct {
	Storefront          *storefront.Runtime
	StorefrontURL       string
	StorefrontPublicURL string
	GrafanaPublicURL    string
	ModelGateway        *gateway.Server
	ModelGatewayURL     string
	Orchestrator        *bugbash.Orchestrator
	AlertmanagerBridge  *monitoring.AlertmanagerBridge
	RolloutBridge       *monitoring.RolloutBridge
	Scraper             *monitoring.Scraper
	HTTPClient          *http.Client
}

// ExternalSurfaceLinks holds browser-accessible URLs for the Retail Storefront (:3001) and Monitoring/Grafana (:3002).
type ExternalSurfaceLinks struct {
	StorefrontURL string `json:"storefront_url"`
	GrafanaURL    string `json:"grafana_url"`
}

// GatewayStatusSummary summarizes the Zero-Secret Agent Platform Model Gateway state.
type GatewayStatusSummary struct {
	ActiveModel        string                `json:"active_model"`
	FallbackCascade    []string              `json:"fallback_cascade"`
	SupportedHarnesses []string              `json:"supported_harnesses"`
	SourceProvenance   string                `json:"source_provenance"`
	ZeroSecretMode     bool                  `json:"zero_secret_mode"`
	Sessions           []gateway.TaskSession `json:"sessions"`
}

// PolyglotServiceHealth describes one of the 5 microservices in the polyglot Retail Storefront.
type PolyglotServiceHealth struct {
	Service      string `json:"service"`
	Language     string `json:"language"`
	Status       string `json:"status"` // "HEALTHY", "IMPLICATED", or "HEALED"
	Implicated   bool   `json:"implicated"`
	Healed       bool   `json:"healed"`
	HTTP200Count int    `json:"http_200_count"`
	HTTP500Count int    `json:"http_500_count"`
}

// ObserverRootTaskView enriches bugbash.RootTaskNode with workerIP and Task.spec.config for the AX Observer UI.
type ObserverRootTaskView struct {
	TaskName           string         `json:"task_name"`
	Phase              string         `json:"phase"`
	WorkerID           string         `json:"worker_id"`
	WorkerIP           string         `json:"worker_ip"`
	TaskConfig         map[string]any `json:"task_config"`
	ImplicatedServices []string       `json:"implicated_services"`
	UnaffectedServices []string       `json:"unaffected_services"`
	SpawnedChildren    []string       `json:"spawned_children"`
	StartedAt          time.Time      `json:"started_at"`
	SuspendedAt        time.Time      `json:"suspended_at,omitempty"`
}

// ObserverChildTaskView enriches bugbash.ChildTaskResult with Task.spec.config, workerIP, status.phase, and SlotReclamationMs.
type ObserverChildTaskView struct {
	TaskName           string                 `json:"task_name"`
	ParentTask         string                 `json:"parent_task"`
	IncidentID         string                 `json:"incident_id"`
	Service            string                 `json:"service"`
	Language           string                 `json:"language"`
	Phase              string                 `json:"phase"`
	WorkerID           string                 `json:"worker_id"`
	WorkerIP           string                 `json:"worker_ip"`
	Harness            string                 `json:"harness"`
	RequestedModel     string                 `json:"requested_model"`
	ActiveModel        string                 `json:"active_model"`
	SourceProvenance   string                 `json:"source_provenance"`
	TaskConfig         map[string]any         `json:"task_config"`
	FailingRequest     map[string]any         `json:"failing_request,omitempty"`
	RegressionTestName string                 `json:"regression_test_name"`
	TDDSteps           []bugbash.TDDStepEvent `json:"tdd_steps"`
	RegressionTestDiff string                 `json:"regression_test_diff"`
	CodeFixDiff        string                 `json:"code_fix_diff"`
	UnifiedGitDiff     string                 `json:"unified_git_diff"`
	RedTestOutput      string                 `json:"red_test_output"`
	GreenTestOutput    string                 `json:"green_test_output"`
	SlotReclamationMs  int64                  `json:"slot_reclamation_ms"`
	Suspended          bool                   `json:"suspended"`
	CompletedAt        time.Time              `json:"completed_at,omitempty"`
	SuspendedAt        time.Time              `json:"suspended_at,omitempty"`
}

// ObserverTreeView represents an active or historical Bug Bash Tree execution formatted for the AX Observer UI.
type ObserverTreeView struct {
	ExecutionID        string                  `json:"execution_id"`
	IncidentID         string                  `json:"incident_id"`
	Title              string                  `json:"title"`
	Summary            string                  `json:"summary"`
	WhyCIMissedIt      string                  `json:"why_ci_missed_it"`
	HarnessMode        string                  `json:"harness_mode"`
	RequestedModel     string                  `json:"requested_model"`
	Phase              string                  `json:"phase"`
	RootTask           ObserverRootTaskView    `json:"root_task"`
	ImplicatedServices []string                `json:"implicated_services"`
	UnaffectedServices []string                `json:"unaffected_services"`
	ChildTasks         []ObserverChildTaskView `json:"child_tasks"`
	MergedGitCommitSHA string                  `json:"merged_git_commit_sha,omitempty"`
	MergedUnifiedDiff  string                  `json:"merged_unified_diff,omitempty"`
	RolloutDurationMs  int64                   `json:"rollout_duration_ms"`
	StorefrontHealed   bool                    `json:"storefront_healed"`
	StartedAt          time.Time               `json:"started_at"`
	CompletedAt        time.Time               `json:"completed_at,omitempty"`
}

// VolumeSnapshotRecord represents a suspended task volume snapshot on the Cloud Storage Volume Snapshot Shelf.
type VolumeSnapshotRecord struct {
	TaskName         string    `json:"task_name"`
	Service          string    `json:"service"`
	IncidentID       string    `json:"incident_id"`
	WorkerID         string    `json:"worker_id"`
	SnapshotFidelity string    `json:"snapshot_fidelity"`
	SnapshotPath     string    `json:"snapshot_path"`
	ReclaimedMs      int64     `json:"reclaimed_ms"`
	SuspendedAt      time.Time `json:"suspended_at"`
}

// StateSnapshot is the unified response returned by GET /api/state and streamed over GET /api/events.
type StateSnapshot struct {
	ExternalLinks   ExternalSurfaceLinks             `json:"external_links"`
	Gateway         GatewayStatusSummary             `json:"gateway"`
	Services        []PolyglotServiceHealth          `json:"services"`
	Scenarios       []storefront.IncidentScenario    `json:"scenarios"`
	Alerts          []monitoring.EnrichedAlertRecord `json:"alerts"`
	LatestAlert     *monitoring.EnrichedAlertRecord  `json:"latest_alert,omitempty"`
	LatestTree      *ObserverTreeView                `json:"latest_tree,omitempty"`
	Trees           []*ObserverTreeView              `json:"trees"`
	WorkerPool      bugbash.WorkerPoolSummary        `json:"worker_pool"`
	VolumeSnapshots []VolumeSnapshotRecord           `json:"volume_snapshots"`
	Rollouts        []monitoring.RolloutReport       `json:"rollouts"`
	LatestRollout   *monitoring.RolloutReport        `json:"latest_rollout,omitempty"`
	Timestamp       time.Time                        `json:"timestamp"`
}

// TriggerIncidentRequest is the payload sent to POST /api/incidents/trigger from the ⌘K modal.
type TriggerIncidentRequest struct {
	IncidentID string `json:"incident_id"`
	Harness    string `json:"harness"`
	Model      string `json:"model,omitempty"`
}

// Server is the AX Observer HTTP server (:3000).
type Server struct {
	cfg                Config
	storefront         *storefront.Runtime
	gateway            *gateway.Server
	embeddedGatewaySrv *http.Server
	embeddedGatewayLis net.Listener
	orchestrator       *bugbash.Orchestrator
	ownsOrchestrator   bool
	alertBridge        *monitoring.AlertmanagerBridge
	rolloutBridge      *monitoring.RolloutBridge
	scraper            *monitoring.Scraper
	sse                *SSEBroker
	httpClient         *http.Client

	mu sync.RWMutex
}

// NewServer initializes the AX Observer server and wires default in-memory components when omitted.
func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.StorefrontPublicURL) == "" {
		cfg.StorefrontPublicURL = httputil.EnvOr("STOREFRONT_PUBLIC_URL", "http://localhost:3001")
	}
	if strings.TrimSpace(cfg.GrafanaPublicURL) == "" {
		cfg.GrafanaPublicURL = httputil.EnvOr("GRAFANA_PUBLIC_URL", "http://localhost:3002/monitoring")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	sf := cfg.Storefront
	if sf == nil && strings.TrimSpace(cfg.StorefrontURL) == "" {
		createdSF, err := storefront.NewRuntime(storefront.Config{})
		if err != nil {
			return nil, fmt.Errorf("initializing storefront runtime: %w", err)
		}
		sf = createdSF
	}

	gw := cfg.ModelGateway
	if gw == nil {
		gw = gateway.NewServer(gateway.DefaultConfig(), nil)
	}

	var (
		embeddedGWSrv *http.Server
		embeddedGWLis net.Listener
	)
	gwURL := strings.TrimSpace(cfg.ModelGatewayURL)
	if gwURL == "" {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("starting embedded model-gateway listener: %w", err)
		}
		embeddedGWLis = lis
		embeddedGWSrv = &http.Server{Handler: gw.Handler()}
		go func() {
			_ = embeddedGWSrv.Serve(lis)
		}()
		gwURL = "http://" + lis.Addr().String()
	}

	s := &Server{
		cfg:                cfg,
		storefront:         sf,
		gateway:            gw,
		embeddedGatewaySrv: embeddedGWSrv,
		embeddedGatewayLis: embeddedGWLis,
		httpClient:         client,
	}
	s.sse = NewSSEBroker(func() any {
		return s.BuildStateSnapshot(context.Background())
	})

	orch := cfg.Orchestrator
	ownsOrch := false
	if orch == nil {
		createdOrch, err := bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
			Storefront:      sf,
			ModelGateway:    gw,
			ModelGatewayURL: gwURL,
			OnRolloutCompleted: func(tree *bugbash.BugBashTreeExecution) {
				s.sse.Broadcast("tree.updated", tree)
				s.sse.Broadcast("worker.updated", s.workerPoolSnapshot())
			},
		})
		if err != nil {
			if embeddedGWLis != nil {
				_ = embeddedGWLis.Close()
			}
			return nil, fmt.Errorf("creating bugbash orchestrator: %w", err)
		}
		orch = createdOrch
		ownsOrch = true
	}
	s.orchestrator = orch
	s.ownsOrchestrator = ownsOrch

	rb := cfg.RolloutBridge
	ab := cfg.AlertmanagerBridge
	if ab == nil {
		ab = monitoring.NewAlertmanagerBridge(monitoring.AlertmanagerConfig{
			Storefront:    sf,
			StorefrontURL: cfg.StorefrontURL,
			Orchestrator:  orch,
			HTTPClient:    client,
			OnAlertUpdated: func(alert monitoring.EnrichedAlertRecord) {
				s.sse.Broadcast("alert.fired", alert)
			},
		})
	}
	if rb == nil {
		rb = monitoring.NewRolloutBridge(monitoring.RolloutBridgeConfig{
			Storefront:         sf,
			StorefrontURL:      cfg.StorefrontURL,
			AlertmanagerBridge: ab,
			HTTPClient:         client,
		})
	}
	ab.SetRolloutBridge(rb)
	s.alertBridge = ab
	s.rolloutBridge = rb

	scr := cfg.Scraper
	if scr == nil {
		scr = monitoring.NewScraper(monitoring.ScraperConfig{
			Storefront:         sf,
			StorefrontURL:      cfg.StorefrontURL,
			AlertmanagerBridge: ab,
			RolloutBridge:      rb,
			ScrapeInterval:     5 * time.Second,
			DisableAutoWebhook: true,
			HTTPClient:         client,
		})
	}
	s.scraper = scr

	return s, nil
}

// Close releases background listeners and embedded servers.
func (s *Server) Close() {
	if s.scraper != nil {
		s.scraper.Stop()
	}
	if s.ownsOrchestrator && s.orchestrator != nil {
		s.orchestrator.Close()
	}
	if s.embeddedGatewaySrv != nil {
		_ = s.embeddedGatewaySrv.Close()
	}
	if s.embeddedGatewayLis != nil {
		_ = s.embeddedGatewayLis.Close()
	}
}

// Storefront returns the underlying Storefront runtime (if running in-process).
func (s *Server) Storefront() *storefront.Runtime {
	return s.storefront
}

// Scraper returns the underlying Prometheus/Alertmanager/Loki Scraper.
func (s *Server) Scraper() *monitoring.Scraper {
	return s.scraper
}

// ModelGateway returns the underlying Agent Platform Model Gateway server.
func (s *Server) ModelGateway() *gateway.Server {
	return s.gateway
}

// Orchestrator returns the underlying Bug Bash Tree Orchestrator.
func (s *Server) Orchestrator() *bugbash.Orchestrator {
	return s.orchestrator
}

// Handler returns the HTTP handler for the AX Observer (:3000).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("/api/state", s.handleGetState)
	mux.Handle("/api/events", s.sse)
	mux.HandleFunc("/api/webhooks/alertmanager", s.handleAlertmanagerWebhook)
	mux.HandleFunc("/api/incidents/trigger", s.handleTriggerIncident)
	mux.HandleFunc("/api/incidents/reset", s.handleResetIncidents)
	mux.HandleFunc("/api/observer/callback", s.handleObserverCallback)

	// Path-based SPA routes (/, /workers, /about)
	mux.HandleFunc("/workers", s.handleSPAIndex)
	mux.HandleFunc("/about", s.handleSPAIndex)
	mux.HandleFunc("/", s.handleSPAIndex)

	return httputil.WithSecurityHeaders(mux)
}

func (s *Server) handleSPAIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/", "/workers", "/about":
	default:
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "missing index.html", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.BuildStateSnapshot(r.Context())
	httputil.WriteJSON(w, http.StatusOK, snap)
}

func (s *Server) handleAlertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload monitoring.AlertmanagerWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Alertmanager v4 JSON payload"})
		return
	}
	if len(payload.Alerts) == 0 && payload.Status == "" {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "alerts list cannot be empty"})
		return
	}

	resp, err := s.alertBridge.ProcessWebhook(r.Context(), payload)
	if err != nil {
		httputil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	snap := s.BuildStateSnapshot(r.Context())
	s.sse.Broadcast("alert.fired", resp.Alert)
	if snap.LatestTree != nil {
		s.sse.Broadcast("tree.updated", snap.LatestTree)
	}
	if snap.LatestRollout != nil {
		s.sse.Broadcast("rollout.completed", snap.LatestRollout)
	}
	s.sse.Broadcast("worker.updated", snap.WorkerPool)
	s.sse.Broadcast("state.snapshot", snap)

	httputil.WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTriggerIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req TriggerIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}

	incID := strings.ToUpper(strings.TrimSpace(req.IncidentID))
	if incID == "" || incID == "RANDOM" {
		if s.storefront != nil {
			sc, err := s.storefront.RandomizeIncident()
			if err != nil {
				httputil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			incID = sc.ID
		} else {
			incID = storefront.IncidentStackedPromoNegativeSubtotal
		}
	}

	sc, ok := storefront.GetScenario(incID)
	if !ok {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown incident_id %q", incID)})
		return
	}

	harnessMode := strings.ToLower(strings.TrimSpace(req.Harness))
	if harnessMode == "" {
		harnessMode = "multi"
	}
	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		requestedModel = gateway.DefaultFallbackCascade[0]
	}

	// 1. Activate the incident on the Storefront and execute a failing checkout so a real HTTP 500 occurs
	s.activateAndCheckoutStorefront(r.Context(), sc)

	// 2. Dispatch Alertmanager v4 webhook with the chosen Coding Harness mode
	now := time.Now().UTC()
	primarySvc := "storefront-web"
	if len(sc.ImplicatedServices) > 0 {
		primarySvc = sc.ImplicatedServices[0]
	}
	webhookPayload := monitoring.AlertmanagerWebhookPayload{
		Version:  "4",
		GroupKey: fmt.Sprintf("{}:{alertname=\"StorefrontHTTP500Spike\",incident=%q}", sc.ID),
		Status:   "firing",
		Receiver: "ax-observer-webhook",
		GroupLabels: map[string]string{
			"alertname": "StorefrontHTTP500Spike",
			"incident":  sc.ID,
		},
		CommonLabels: map[string]string{
			"alertname":  "StorefrontHTTP500Spike",
			"incident":   sc.ID,
			"service":    primarySvc,
			"severity":   "critical",
			"harness":    harnessMode,
			"model":      requestedModel,
			"gcp_export": "managed-prometheus",
		},
		CommonAnnotations: map[string]string{
			"summary":     sc.Title,
			"description": sc.ErrorMessage,
			"error_code":  sc.ErrorCode,
		},
		Alerts: []monitoring.AlertmanagerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":  "StorefrontHTTP500Spike",
					"incident":   sc.ID,
					"service":    primarySvc,
					"services":   strings.Join(sc.ImplicatedServices, ","),
					"severity":   "critical",
					"harness":    harnessMode,
					"model":      requestedModel,
					"gcp_export": "managed-prometheus",
				},
				Annotations: map[string]string{
					"summary":     sc.Title,
					"description": sc.ErrorMessage,
					"error_code":  sc.ErrorCode,
				},
				StartsAt:    now,
				Fingerprint: fmt.Sprintf("fp-cmdk-%s-%d", strings.ToLower(sc.ID), now.UnixNano()),
			},
		},
	}

	resp, err := s.alertBridge.ProcessWebhook(r.Context(), webhookPayload)
	if err != nil {
		httputil.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 3. Refresh Prometheus scraper metrics snapshot
	if s.scraper != nil {
		_, _ = s.scraper.ScrapeOnce(r.Context())
	}

	snap := s.BuildStateSnapshot(r.Context())
	s.sse.Broadcast("alert.fired", resp.Alert)
	if snap.LatestTree != nil {
		for _, child := range snap.LatestTree.ChildTasks {
			for _, st := range child.TDDSteps {
				s.sse.Broadcast("child.tdd_step", st)
			}
		}
		s.sse.Broadcast("tree.updated", snap.LatestTree)
	}
	if snap.LatestRollout != nil {
		s.sse.Broadcast("rollout.completed", snap.LatestRollout)
	}
	s.sse.Broadcast("worker.updated", snap.WorkerPool)
	s.sse.Broadcast("state.snapshot", snap)

	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":         "triggered",
		"incident_id":    sc.ID,
		"harness":        harnessMode,
		"execution_id":   resp.ExecutionID,
		"root_task_name": resp.RootTaskName,
		"webhook":        resp,
		"state":          snap,
	})
}

func (s *Server) handleResetIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.storefront != nil {
		s.storefront.HealIncident("")
	} else if strings.TrimSpace(s.cfg.StorefrontURL) != "" {
		healURL := strings.TrimRight(strings.TrimSpace(s.cfg.StorefrontURL), "/") + "/api/incidents/heal"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, healURL, bytes.NewReader([]byte(`{}`)))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := s.httpClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	if s.alertBridge != nil {
		s.alertBridge.ResolveIncidentAlerts("", 0)
	}
	snap := s.BuildStateSnapshot(r.Context())
	s.sse.Broadcast("state.snapshot", snap)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "reset",
		"state":  snap,
	})
}

func (s *Server) handleObserverCallback(w http.ResponseWriter, r *http.Request) {
	if s.orchestrator != nil {
		s.orchestrator.HandleObserverCallback(w, r)
		snap := s.BuildStateSnapshot(r.Context())
		s.sse.Broadcast("tree.updated", snap.LatestTree)
		s.sse.Broadcast("state.snapshot", snap)
		return
	}
	http.Error(w, "orchestrator not configured", http.StatusServiceUnavailable)
}

func (s *Server) activateAndCheckoutStorefront(ctx context.Context, sc storefront.IncidentScenario) {
	cartRaw, _ := json.Marshal(sc.TriggerCart)
	if s.storefront != nil {
		if s.storefront.GitManager() != nil {
			_ = s.storefront.GitManager().ResetToBaseline()
		}
		_, _ = s.storefront.ActivateIncident(sc.ID)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cart/checkout", bytes.NewReader(cartRaw))
		req.Header.Set("Content-Type", "application/json")
		s.storefront.Handler().ServeHTTP(rec, req)
		return
	}
	if strings.TrimSpace(s.cfg.StorefrontURL) != "" {
		base := strings.TrimRight(strings.TrimSpace(s.cfg.StorefrontURL), "/")
		actBody, _ := json.Marshal(map[string]string{"incident_id": sc.ID})
		if req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/incidents/activate", bytes.NewReader(actBody)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := s.httpClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		if req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/cart/checkout", bytes.NewReader(cartRaw)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := s.httpClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// BuildStateSnapshot constructs the unified StateSnapshot for GET /api/state and SSE broadcasts.
func (s *Server) BuildStateSnapshot(ctx context.Context) StateSnapshot {
	pool := s.workerPoolSnapshot()

	// Map task_name -> (workerID, workerIP, phase, configMap) from ax-server ListTasks
	taskMetaByName := make(map[string]struct {
		workerID string
		workerIP string
		phase    string
		config   map[string]any
	})

	if s.orchestrator != nil && s.orchestrator.AXClient() != nil {
		if resp, err := s.orchestrator.AXClient().ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"}); err == nil {
			for _, t := range resp.GetTasks() {
				name := t.GetMetadata().GetName()
				wIP := t.GetStatus().GetWorkerIp()
				wID := workerIDForIP(pool, wIP)
				var cfgMap map[string]any
				if t.GetSpec().GetConfig() != nil {
					cfgMap = t.GetSpec().GetConfig().AsMap()
				}
				taskMetaByName[name] = struct {
					workerID string
					workerIP string
					phase    string
					config   map[string]any
				}{
					workerID: wID,
					workerIP: wIP,
					phase:    t.GetStatus().GetPhase(),
					config:   cfgMap,
				}
			}
		}
	}

	var trees []*ObserverTreeView
	var volumeSnapshots []VolumeSnapshotRecord
	if s.orchestrator != nil {
		for _, ex := range s.orchestrator.ListExecutions() {
			tv, snaps := s.convertTreeExecution(ex, taskMetaByName)
			trees = append(trees, tv)
			volumeSnapshots = append(volumeSnapshots, snaps...)
		}
	}
	var latestTree *ObserverTreeView
	if len(trees) > 0 {
		latestTree = trees[0]
	}

	var alerts []monitoring.EnrichedAlertRecord
	var latestAlert *monitoring.EnrichedAlertRecord
	if s.alertBridge != nil {
		alerts = s.alertBridge.ListAlerts()
		latestAlert = s.alertBridge.LatestAlert()
	}

	var rollouts []monitoring.RolloutReport
	var latestRollout *monitoring.RolloutReport
	if s.rolloutBridge != nil {
		rollouts = s.rolloutBridge.ListRollouts()
		latestRollout = s.rolloutBridge.LatestRollout()
	}

	services := s.buildServiceHealthList(ctx, latestTree, latestAlert)
	gwSummary := s.buildGatewaySummary(latestTree)

	return StateSnapshot{
		ExternalLinks: ExternalSurfaceLinks{
			StorefrontURL: s.cfg.StorefrontPublicURL,
			GrafanaURL:    s.cfg.GrafanaPublicURL,
		},
		Gateway:         gwSummary,
		Services:        services,
		Scenarios:       storefront.ScenarioCatalog(),
		Alerts:          alerts,
		LatestAlert:     latestAlert,
		LatestTree:      latestTree,
		Trees:           trees,
		WorkerPool:      pool,
		VolumeSnapshots: volumeSnapshots,
		Rollouts:        rollouts,
		LatestRollout:   latestRollout,
		Timestamp:       time.Now().UTC(),
	}
}

func (s *Server) convertTreeExecution(
	ex *bugbash.BugBashTreeExecution,
	taskMeta map[string]struct {
		workerID string
		workerIP string
		phase    string
		config   map[string]any
	},
) (*ObserverTreeView, []VolumeSnapshotRecord) {
	rootMeta, hasRoot := taskMeta[ex.RootTask.TaskName]
	rootWorkerID := ex.RootTask.WorkerID
	if rootWorkerID == "" && hasRoot && rootMeta.workerID != "" {
		rootWorkerID = rootMeta.workerID
	}
	if rootWorkerID == "" {
		rootWorkerID = "ateworker-gvisor-01"
	}
	rootWorkerIP := rootMeta.workerIP
	if rootWorkerIP == "" {
		rootWorkerIP = "10.244.1.11"
	}
	rootPhase := ex.RootTask.Phase
	if rootPhase == "" && hasRoot && rootMeta.phase != "" {
		rootPhase = rootMeta.phase
	}
	rootCfg := rootMeta.config
	if rootCfg == nil {
		rootCfg = map[string]any{
			"role":            "root-triage",
			"incident_id":     ex.IncidentID,
			"harness_mode":    ex.HarnessMode,
			"requested_model": ex.RequestedModel,
		}
	}

	sc, _ := storefront.GetScenario(ex.IncidentID)
	var childViews []ObserverChildTaskView
	var snaps []VolumeSnapshotRecord

	for idx, ch := range ex.ChildTasks {
		cMeta, hasChild := taskMeta[ch.TaskName]
		wID := ch.WorkerID
		if wID == "" && hasChild && cMeta.workerID != "" {
			wID = cMeta.workerID
		}
		if wID == "" {
			wID = fmt.Sprintf("ateworker-gvisor-0%d", ((idx+1)%4)+1)
		}
		wIP := cMeta.workerIP
		if wIP == "" {
			wIP = fmt.Sprintf("10.244.1.%d", 11+((idx+1)%4))
		}
		cPhase := v1alpha1.PhaseRunning
		if ch.Suspended {
			cPhase = v1alpha1.PhaseSuspended
		} else if hasChild && cMeta.phase != "" {
			cPhase = cMeta.phase
		}
		cCfg := cMeta.config
		failingReq := bugbash.CheckoutRequestToMap(sc.TriggerCart)
		if cCfg == nil {
			detail := sc.ServiceTraces[ch.Service]
			cCfg = map[string]any{
				"role":                 "child-fixer",
				"task_name":            ch.TaskName,
				"parent_task":          ch.ParentTask,
				"incident_id":          ch.IncidentID,
				"service":              ch.Service,
				"language":             ch.Language,
				"harness":              ch.Harness,
				"model":                ch.RequestedModel,
				"source_file":          detail.SourceFile,
				"test_file":            detail.TestFile,
				"error_type":           detail.ErrorType,
				"error_message":        detail.ErrorMessage,
				"stack_trace":          detail.StackTrace,
				"regression_test_name": ch.RegressionTestName,
				"failing_request":      failingReq,
			}
		} else if fr, ok := cCfg["failing_request"].(map[string]any); ok && len(fr) > 0 {
			failingReq = fr
		}

		reclaimMs := int64(4)
		for _, st := range ch.TDDSteps {
			if st.Phase == bugbash.PhaseAutoSuspend && st.DurationMs > 0 {
				reclaimMs = st.DurationMs
			}
		}

		activeModel := ch.ActiveModel
		if activeModel == "" {
			activeModel = ch.RequestedModel
		}
		prov := ch.SourceProvenance
		if prov == "" {
			prov = string(gateway.ProvenanceFixture)
		}

		childViews = append(childViews, ObserverChildTaskView{
			TaskName:           ch.TaskName,
			ParentTask:         ch.ParentTask,
			IncidentID:         ch.IncidentID,
			Service:            ch.Service,
			Language:           ch.Language,
			Phase:              cPhase,
			WorkerID:           wID,
			WorkerIP:           wIP,
			Harness:            ch.Harness,
			RequestedModel:     ch.RequestedModel,
			ActiveModel:        activeModel,
			SourceProvenance:   prov,
			TaskConfig:         cCfg,
			FailingRequest:     failingReq,
			RegressionTestName: ch.RegressionTestName,
			TDDSteps:           append([]bugbash.TDDStepEvent(nil), ch.TDDSteps...),
			RegressionTestDiff: ch.RegressionTestDiff,
			CodeFixDiff:        ch.CodeFixDiff,
			UnifiedGitDiff:     ch.UnifiedGitDiff,
			RedTestOutput:      ch.RedTestOutput,
			GreenTestOutput:    ch.GreenTestOutput,
			SlotReclamationMs:  reclaimMs,
			Suspended:          ch.Suspended,
			CompletedAt:        ch.CompletedAt,
			SuspendedAt:        ch.SuspendedAt,
		})

		if ch.Suspended {
			snaps = append(snaps, VolumeSnapshotRecord{
				TaskName:         ch.TaskName,
				Service:          ch.Service,
				IncidentID:       ch.IncidentID,
				WorkerID:         wID,
				SnapshotFidelity: "SNAPSHOT_FIDELITY_VOLUMES",
				SnapshotPath:     fmt.Sprintf("/workspace/.ax-state/%s.json", ch.TaskName),
				ReclaimedMs:      reclaimMs,
				SuspendedAt:      ch.SuspendedAt,
			})
		}
	}

	return &ObserverTreeView{
		ExecutionID: ex.ExecutionID,
		IncidentID:  ex.IncidentID,
		Title:       ex.Title,
		Summary:     ex.Summary,
		WhyCIMissedIt: ex.WhyCIMissedIt,
		HarnessMode:    ex.HarnessMode,
		RequestedModel: ex.RequestedModel,
		Phase:          ex.Phase,
		RootTask: ObserverRootTaskView{
			TaskName:           ex.RootTask.TaskName,
			Phase:              rootPhase,
			WorkerID:           rootWorkerID,
			WorkerIP:           rootWorkerIP,
			TaskConfig:         rootCfg,
			ImplicatedServices: append([]string(nil), ex.RootTask.ImplicatedServices...),
			UnaffectedServices: append([]string(nil), ex.RootTask.UnaffectedServices...),
			SpawnedChildren:    append([]string(nil), ex.RootTask.SpawnedChildren...),
			StartedAt:          ex.RootTask.StartedAt,
			SuspendedAt:        ex.RootTask.SuspendedAt,
		},
		ImplicatedServices: append([]string(nil), ex.ImplicatedServices...),
		UnaffectedServices: append([]string(nil), ex.UnaffectedServices...),
		ChildTasks:         childViews,
		MergedGitCommitSHA: ex.MergedGitCommitSHA,
		MergedUnifiedDiff:  ex.MergedUnifiedDiff,
		RolloutDurationMs:  ex.RolloutDurationMs,
		StorefrontHealed:   ex.StorefrontHealed,
		StartedAt:          ex.StartedAt,
		CompletedAt:        ex.CompletedAt,
	}, snaps
}

func (s *Server) buildServiceHealthList(
	ctx context.Context,
	latestTree *ObserverTreeView,
	latestAlert *monitoring.EnrichedAlertRecord,
) []PolyglotServiceHealth {
	var metricsSnap monitoring.MetricsSnapshot
	if s.scraper != nil {
		metricsSnap = s.scraper.Snapshot()
		if metricsSnap.LastScrapeAt.IsZero() {
			metricsSnap, _ = s.scraper.ScrapeOnce(ctx)
		}
	}

	implicatedSet := make(map[string]bool)
	healed := false
	if latestTree != nil {
		for _, svc := range latestTree.ImplicatedServices {
			implicatedSet[svc] = true
		}
		healed = latestTree.StorefrontHealed
	} else if latestAlert != nil {
		for _, svc := range latestAlert.Services {
			implicatedSet[svc] = true
		}
		healed = latestAlert.Status == "resolved"
	}

	if s.storefront != nil {
		if active := s.storefront.ActiveScenario(); active != nil {
			healed = false
			for _, svc := range active.ImplicatedServices {
				implicatedSet[svc] = true
			}
		}
	}

	all := storefront.AllServices()
	out := make([]PolyglotServiceHealth, 0, len(all))
	for _, svc := range all {
		isImp := implicatedSet[svc]
		status := "HEALTHY"
		if isImp {
			if healed {
				status = "HEALED"
			} else {
				status = "IMPLICATED"
			}
		}
		c200 := metricsSnap.HTTP200ByService[svc]
		if c200 == 0 {
			c200 = 1
		}
		c500 := metricsSnap.HTTP500ByService[svc]
		out = append(out, PolyglotServiceHealth{
			Service:      svc,
			Language:     storefront.ServiceLanguage(svc),
			Status:       status,
			Implicated:   isImp,
			Healed:       isImp && healed,
			HTTP200Count: c200,
			HTTP500Count: c500,
		})
	}
	return out
}

func (s *Server) buildGatewaySummary(latestTree *ObserverTreeView) GatewayStatusSummary {
	cfg := gateway.DefaultConfig()
	var sessions []gateway.TaskSession
	if s.gateway != nil {
		cfg = s.gateway.Config()
		sessions = s.gateway.ListSessions()
	}
	activeModel := gateway.DefaultFallbackCascade[0]
	if len(cfg.FallbackCascade) > 0 {
		activeModel = cfg.FallbackCascade[0]
	}
	prov := string(gateway.ProvenanceFixture)
	if len(sessions) > 0 {
		last := sessions[len(sessions)-1]
		if last.ActiveModel != "" {
			activeModel = last.ActiveModel
		}
		if last.SourceProvenance != "" {
			prov = string(last.SourceProvenance)
		}
	} else if latestTree != nil && len(latestTree.ChildTasks) > 0 {
		c := latestTree.ChildTasks[0]
		if c.ActiveModel != "" {
			activeModel = c.ActiveModel
		}
		if c.SourceProvenance != "" {
			prov = c.SourceProvenance
		}
	}
	return GatewayStatusSummary{
		ActiveModel:        activeModel,
		FallbackCascade:    append([]string(nil), cfg.FallbackCascade...),
		SupportedHarnesses: append([]string(nil), cfg.SupportedHarnesses...),
		SourceProvenance:   prov,
		ZeroSecretMode:     cfg.ZeroSecretMode,
		Sessions:           sessions,
	}
}

func (s *Server) workerPoolSnapshot() bugbash.WorkerPoolSummary {
	if s.orchestrator != nil {
		return s.orchestrator.WorkerPoolSnapshot()
	}
	return bugbash.NewEmbeddedAXServer().WorkerPoolSnapshot()
}

func workerIDForIP(pool bugbash.WorkerPoolSummary, ip string) string {
	for _, w := range pool.Workers {
		if w.WorkerIP == ip {
			return w.WorkerID
		}
	}
	return ""
}
