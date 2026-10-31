package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
)

// SourceProvenance indicates whether a model response came from live Gemini Enterprise Agent Platform or deterministic fixture synthesis.
type SourceProvenance string

const (
	ProvenanceLive    SourceProvenance = "LIVE"
	ProvenanceFixture SourceProvenance = "FIXTURE"

	// SandboxTokenPlaceholder is the dummy token passed inside task sandboxes so real cloud secrets never enter the sandbox.
	SandboxTokenPlaceholder = "ax-sandbox-token"
)

// DefaultFallbackCascade defines the canonical ADR-0007 ordered Gemini Flash fallback cascade.
var DefaultFallbackCascade = []string{
	"gemini-3.8-flash",
	"gemini-3.7-flash",
	"gemini-3.6-flash",
	"gemini-3.5-flash",
}

// SupportedHarnesses lists the three CLI coding harnesses bundled in Dockerfile.task-runner.
var SupportedHarnesses = []string{
	"antigravity",
	"opencode",
	"pi",
}

// Config holds runtime settings for the zero-secret Gemini Enterprise Agent Platform Model Gateway.
type Config struct {
	ListenAddr            string   `json:"listen_addr"`
	ProjectID             string   `json:"project_id"`
	Location              string   `json:"location"`
	AgentPlatformBaseURL  string   `json:"agent_platform_base_url"`
	FallbackCascade       []string `json:"fallback_cascade"`
	EnableFixtureFallback bool     `json:"enable_fixture_fallback"`
	ZeroSecretMode        bool     `json:"zero_secret_mode"`
	SupportedHarnesses    []string `json:"supported_harnesses"`
}

// DefaultConfig returns sensible defaults for the Model Gateway.
func DefaultConfig() Config {
	enableFixture := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ENABLE_FIXTURE_FALLBACK"))); v == "false" || v == "0" {
		enableFixture = false
	}
	cascade := append([]string(nil), DefaultFallbackCascade...)
	if raw := strings.TrimSpace(os.Getenv("GEMINI_FALLBACK_CASCADE")); raw != "" {
		parts := strings.Split(raw, ",")
		var custom []string
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				custom = append(custom, trimmed)
			}
		}
		if len(custom) > 0 {
			cascade = custom
		}
	}

	projectID := httputil.EnvOr("PROJECT_ID", httputil.EnvOr("GOOGLE_CLOUD_PROJECT", "ax-bug-bash"))
	return Config{
		ListenAddr:            httputil.EnvOr("LISTEN_ADDR", ":8080"),
		ProjectID:             projectID,
		Location:              httputil.EnvOr("AGENT_PLATFORM_LOCATION", "global"),
		AgentPlatformBaseURL:  httputil.EnvOr("AGENT_PLATFORM_BASE_URL", "https://aiplatform.googleapis.com"),
		FallbackCascade:       cascade,
		EnableFixtureFallback: enableFixture,
		ZeroSecretMode:        true,
		SupportedHarnesses:    append([]string(nil), SupportedHarnesses...),
	}
}

// TokenFunc returns a Bearer OAuth2 access token for authenticating to Gemini Enterprise Agent Platform.
type TokenFunc func(ctx context.Context) (string, error)

// StaticTokenFunc returns a constant token (used in tests or when GOOGLE_OAUTH_ACCESS_TOKEN is fixed).
func StaticTokenFunc(token string) TokenFunc {
	return func(context.Context) (string, error) {
		return token, nil
	}
}

// DefaultWorkloadIdentityTokenFunc caches GCP access tokens from the GKE Workload Identity metadata server
// (or falls back to GOOGLE_OAUTH_ACCESS_TOKEN / gcloud auth application-default print-access-token for local runs).
func DefaultWorkloadIdentityTokenFunc() TokenFunc {
	var (
		mu      sync.Mutex
		cached  string
		expires time.Time
		client  = &http.Client{Timeout: 3 * time.Second}
	)
	return func(ctx context.Context) (string, error) {
		if envTok := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN")); envTok != "" {
			return envTok, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if cached != "" && time.Now().Before(expires) {
			return cached, nil
		}

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
			nil,
		)
		if err == nil {
			req.Header.Set("Metadata-Flavor", "Google")
			if resp, err := client.Do(req); err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var payload struct {
						AccessToken string `json:"access_token"`
						ExpiresIn   int    `json:"expires_in"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.AccessToken != "" {
						cached = payload.AccessToken
						ttl := time.Duration(payload.ExpiresIn)*time.Second - 60*time.Second
						if ttl < 30*time.Second {
							ttl = 30 * time.Second
						}
						expires = time.Now().Add(ttl)
						return cached, nil
					}
				}
			}
		}

		if out, err := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token").Output(); err == nil {
			if tok := strings.TrimSpace(string(out)); tok != "" {
				cached = tok
				expires = time.Now().Add(5 * time.Minute)
				return cached, nil
			}
		}
		out, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
		if err != nil {
			return "", fmt.Errorf("fetch GCP token from Workload Identity metadata server or gcloud: %w", err)
		}
		cached = strings.TrimSpace(string(out))
		expires = time.Now().Add(5 * time.Minute)
		return cached, nil
	}
}

// CallRecord represents a single metered request through the Model Gateway.
type CallRecord struct {
	ID               string           `json:"id"`
	TaskName         string           `json:"task_name"`
	Harness          string           `json:"harness"`
	Endpoint         string           `json:"endpoint"`
	RequestedModel   string           `json:"requested_model"`
	ActiveModel      string           `json:"active_model"`
	CascadeAttempts  []CascadeAttempt `json:"cascade_attempts,omitempty"`
	SourceProvenance SourceProvenance `json:"source_provenance"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	DurationMs       int64            `json:"duration_ms"`
	StatusCode       int              `json:"status_code"`
	Timestamp        time.Time        `json:"timestamp"`
}

// TaskSession holds live cumulative token, request, model, and provenance metrics for an AX Task session.
type TaskSession struct {
	TaskName           string           `json:"task_name"`
	Harness            string           `json:"harness"`
	RequestedModel     string           `json:"requested_model"`
	ActiveModel        string           `json:"active_model"`
	SourceProvenance   SourceProvenance `json:"source_provenance"`
	RequestCount       int64            `json:"request_count"`
	TotalInputTokens   int64            `json:"total_input_tokens"`
	TotalOutputTokens  int64            `json:"total_output_tokens"`
	TotalTokens        int64            `json:"total_tokens"`
	CascadeFallbacks   int64            `json:"cascade_fallbacks"`
	LastCascadeTrace   []CascadeAttempt `json:"last_cascade_trace,omitempty"`
	ZeroSecretVerified bool             `json:"zero_secret_verified"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// Server wraps the Model Gateway proxy and telemetry HTTP endpoints.
type Server struct {
	cfg        Config
	tokenFn    TokenFunc
	httpClient *http.Client

	mu       sync.RWMutex
	sessions map[string]*TaskSession
	calls    []CallRecord
	nextSeq  int64
}

// NewServer constructs a Model Gateway Server.
func NewServer(cfg Config, tokenFn TokenFunc) *Server {
	if len(cfg.FallbackCascade) == 0 {
		cfg.FallbackCascade = append([]string(nil), DefaultFallbackCascade...)
	}
	if len(cfg.SupportedHarnesses) == 0 {
		cfg.SupportedHarnesses = append([]string(nil), SupportedHarnesses...)
	}
	if tokenFn == nil {
		tokenFn = DefaultWorkloadIdentityTokenFunc()
	}
	return &Server{
		cfg:     cfg,
		tokenFn: tokenFn,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		sessions: make(map[string]*TaskSession),
	}
}

// Config returns the Server's active configuration.
func (s *Server) Config() Config {
	return s.cfg
}

// Handler returns the top-level http.Handler for the Model Gateway.
// TODO(security): End-user login/cookies/file-uploads/SQL are intentionally absent; Model Gateway authenticates
// strictly via GKE Workload Identity to Gemini Enterprise Agent Platform and accepts only cluster-internal sandbox traffic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleHealthz)
	mux.HandleFunc("/api/gateway/config", s.handleConfig)
	mux.HandleFunc("/api/gateway/sessions", s.handleSessions)
	mux.HandleFunc("/api/gateway/sessions/", s.handleSessionByTask)
	mux.HandleFunc("/v1/models", s.handleModelsList)
	mux.HandleFunc("/v1/chat/completions", s.handleTopLevelRoute)
	mux.HandleFunc("/v1beta/models/", s.handleTopLevelRoute)
	mux.HandleFunc("/s/", s.handleSessionRoute)
	return httputil.WithSecurityHeaders(mux)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "ok",
		"platform": "Gemini Enterprise Agent Platform",
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.cfg)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": s.ListSessions(),
			"calls":    s.RecentCalls(),
		})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var meta TaskSession
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil || strings.TrimSpace(meta.TaskName) == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "valid task_name is required")
			return
		}
		updated := s.UpsertSession(meta)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(updated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSessionByTask(w http.ResponseWriter, r *http.Request) {
	taskName := strings.TrimPrefix(r.URL.Path, "/api/gateway/sessions/")
	if taskName == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "missing task_name")
		return
	}
	sess, ok := s.GetSession(taskName)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", "task session not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sess)
}

func (s *Server) handleSessionRoute(w http.ResponseWriter, r *http.Request) {
	taskName, restPath := parseTaskSessionPath(r.URL.Path)
	if taskName == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "missing task_name in /s/{task_name}/...")
		return
	}
	s.dispatchSessionRequest(w, r, taskName, restPath)
}

func (s *Server) handleTopLevelRoute(w http.ResponseWriter, r *http.Request) {
	taskName := strings.TrimSpace(r.Header.Get("X-AX-Task-Name"))
	if taskName == "" {
		taskName = strings.TrimSpace(r.Header.Get("X-Task-Name"))
	}
	if taskName == "" {
		taskName = "default-task"
	}
	s.dispatchSessionRequest(w, r, taskName, r.URL.Path)
}

func (s *Server) handleModelsList(w http.ResponseWriter, _ *http.Request) {
	var data []map[string]any
	for _, m := range s.cfg.FallbackCascade {
		data = append(data, map[string]any{
			"id":       m,
			"object":   "model",
			"owned_by": "gemini-enterprise-agent-platform",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) dispatchSessionRequest(w http.ResponseWriter, r *http.Request, taskName, restPath string) {
	if strings.HasSuffix(restPath, "/v1/models") || restPath == "/models" {
		s.handleModelsList(w, r)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read_error", err.Error())
		return
	}
	_ = r.Body.Close()

	switch {
	case strings.Contains(restPath, "/chat/completions"):
		s.handleOpenAIChatCompletions(w, r, taskName, bodyBytes)
	case strings.Contains(restPath, "/models/"):
		s.handleGeminiGenerate(w, r, taskName, restPath, bodyBytes)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown_route", fmt.Sprintf("unsupported session route %q", restPath))
	}
}

func (s *Server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, taskName, restPath string, bodyBytes []byte) {
	requestedModel, action := parseGeminiModelAndAction(restPath)
	if requestedModel == "" && len(s.cfg.FallbackCascade) > 0 {
		requestedModel = s.cfg.FallbackCascade[0]
	}
	harness := detectHarness(r, taskName, "antigravity")

	start := time.Now()
	result, err := s.ExecuteGeminiCascade(r.Context(), taskName, requestedModel, action, r.URL.RawQuery, bodyBytes)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		status := result.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.recordCall(CallRecord{
			TaskName:         taskName,
			Harness:          harness,
			Endpoint:         "gemini",
			RequestedModel:   requestedModel,
			ActiveModel:      result.ActiveModel,
			CascadeAttempts:  result.Attempts,
			SourceProvenance: result.SourceProvenance,
			DurationMs:       dur,
			StatusCode:       status,
		})
		writeJSONError(w, status, "upstream_cascade_error", err.Error())
		return
	}

	s.recordCall(CallRecord{
		TaskName:         taskName,
		Harness:          harness,
		Endpoint:         "gemini",
		RequestedModel:   requestedModel,
		ActiveModel:      result.ActiveModel,
		CascadeAttempts:  result.Attempts,
		SourceProvenance: result.SourceProvenance,
		InputTokens:      result.InputTokens,
		OutputTokens:     result.OutputTokens,
		TotalTokens:      result.InputTokens + result.OutputTokens,
		DurationMs:       dur,
		StatusCode:       result.StatusCode,
	})

	setGatewayHeaders(w, taskName, result)
	w.Header().Set("Content-Type", result.ContentType)
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

func setGatewayHeaders(w http.ResponseWriter, taskName string, result CascadeResult) {
	w.Header().Set("X-AX-Task-Name", taskName)
	w.Header().Set("X-AX-Active-Model", result.ActiveModel)
	w.Header().Set("X-AX-Source-Provenance", string(result.SourceProvenance))
	w.Header().Set("X-AX-Cascade-Attempts", strconv.Itoa(len(result.Attempts)))
}

// UpsertSession creates or updates metadata for an AX Task session.
func (s *Server) UpsertSession(meta TaskSession) TaskSession {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.ensureSessionLocked(meta.TaskName)
	if meta.Harness != "" {
		cur.Harness = meta.Harness
	}
	if meta.RequestedModel != "" {
		cur.RequestedModel = meta.RequestedModel
	}
	if meta.ActiveModel != "" {
		cur.ActiveModel = meta.ActiveModel
	}
	if meta.SourceProvenance != "" {
		cur.SourceProvenance = meta.SourceProvenance
	}
	cur.ZeroSecretVerified = true
	cur.UpdatedAt = time.Now().UTC()
	cp := *cur
	cp.LastCascadeTrace = append([]CascadeAttempt(nil), cur.LastCascadeTrace...)
	return cp
}

// GetSession returns the current stats for a single AX Task session.
func (s *Server) GetSession(taskName string) (TaskSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, ok := s.sessions[taskName]
	if !ok {
		return TaskSession{}, false
	}
	cp := *cur
	cp.LastCascadeTrace = append([]CascadeAttempt(nil), cur.LastCascadeTrace...)
	return cp, true
}

// ListSessions returns all known AX Task sessions sorted by TaskName.
func (s *Server) ListSessions() []TaskSession {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]TaskSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		cp := *sess
		cp.LastCascadeTrace = append([]CascadeAttempt(nil), sess.LastCascadeTrace...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TaskName < out[j].TaskName
	})
	return out
}

// RecentCalls returns the recorded gateway calls in chronological order.
func (s *Server) RecentCalls() []CallRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CallRecord, len(s.calls))
	for i, c := range s.calls {
		cp := c
		cp.CascadeAttempts = append([]CascadeAttempt(nil), c.CascadeAttempts...)
		out[i] = cp
	}
	return out
}

func (s *Server) recordCall(rec CallRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextSeq++
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("call-%d", s.nextSeq)
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}

	sess := s.ensureSessionLocked(rec.TaskName)
	if rec.Harness != "" {
		sess.Harness = rec.Harness
	}
	if rec.RequestedModel != "" {
		sess.RequestedModel = rec.RequestedModel
	}
	if rec.ActiveModel != "" {
		sess.ActiveModel = rec.ActiveModel
	}
	if rec.SourceProvenance != "" {
		sess.SourceProvenance = rec.SourceProvenance
	}
	sess.RequestCount++
	sess.TotalInputTokens += rec.InputTokens
	sess.TotalOutputTokens += rec.OutputTokens
	sess.TotalTokens = sess.TotalInputTokens + sess.TotalOutputTokens
	if len(rec.CascadeAttempts) > 1 {
		sess.CascadeFallbacks += int64(len(rec.CascadeAttempts) - 1)
	}
	sess.LastCascadeTrace = append([]CascadeAttempt(nil), rec.CascadeAttempts...)
	sess.ZeroSecretVerified = true
	sess.UpdatedAt = rec.Timestamp

	s.calls = append(s.calls, rec)
}

func (s *Server) ensureSessionLocked(taskName string) *TaskSession {
	if cur, ok := s.sessions[taskName]; ok {
		return cur
	}
	defaultModel := "gemini-3.8-flash"
	if len(s.cfg.FallbackCascade) > 0 {
		defaultModel = s.cfg.FallbackCascade[0]
	}
	cur := &TaskSession{
		TaskName:           taskName,
		Harness:            "antigravity",
		RequestedModel:     defaultModel,
		ActiveModel:        defaultModel,
		SourceProvenance:   ProvenanceLive,
		ZeroSecretVerified: true,
		UpdatedAt:          time.Now().UTC(),
	}
	s.sessions[taskName] = cur
	return cur
}

func parseTaskSessionPath(urlPath string) (taskName, restPath string) {
	if !strings.HasPrefix(urlPath, "/s/") {
		return "", urlPath
	}
	trimmed := strings.TrimPrefix(urlPath, "/s/")
	slash := strings.IndexByte(trimmed, '/')
	if slash <= 0 {
		return trimmed, "/"
	}
	return trimmed[:slash], trimmed[slash:]
}

func parseGeminiModelAndAction(restPath string) (model, action string) {
	action = "generateContent"
	idx := strings.Index(restPath, "/models/")
	if idx < 0 {
		return "", action
	}
	tail := restPath[idx+len("/models/"):]
	if colon := strings.IndexByte(tail, ':'); colon >= 0 {
		return tail[:colon], tail[colon+1:]
	}
	return tail, action
}

func detectHarness(r *http.Request, taskName, defaultHarness string) string {
	if h := strings.TrimSpace(r.Header.Get("X-AX-Harness")); h != "" {
		return strings.ToLower(h)
	}
	lowerTask := strings.ToLower(taskName)
	for _, known := range SupportedHarnesses {
		if strings.Contains(lowerTask, known) {
			return known
		}
	}
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	for _, known := range SupportedHarnesses {
		if strings.Contains(ua, known) {
			return known
		}
	}
	return defaultHarness
}

func writeJSONError(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}
