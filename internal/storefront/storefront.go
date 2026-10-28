package storefront

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
)

//go:embed static/*
var staticFS embed.FS

// Config configures a StorefrontRuntime instance.
type Config struct {
	GitWorkDir     string
	OnIncidentHook func(sc IncidentScenario, logs []LogEntry)
}

// CheckoutResponse is returned by POST /api/cart/checkout.
type CheckoutResponse struct {
	OrderID            string                           `json:"order_id,omitempty"`
	Status             string                           `json:"status"`
	Currency           string                           `json:"currency,omitempty"`
	ChargedMinorUnits  int64                            `json:"charged_minor_units,omitempty"`
	ShippingCents      int                              `json:"shipping_cents,omitempty"`
	IncidentID         string                           `json:"incident_id,omitempty"`
	ErrorCode          string                           `json:"error_code,omitempty"`
	Error              string                           `json:"error,omitempty"`
	ImplicatedServices []string                         `json:"implicated_services,omitempty"`
	UnaffectedServices []string                         `json:"unaffected_services,omitempty"`
	StackTraces        []string                         `json:"stack_traces,omitempty"`
	ServiceTraces      map[string]ServiceIncidentDetail `json:"service_traces,omitempty"`
}

// LogEntry represents a structured Loki-compatible error log entry emitted by an implicated service.
type LogEntry struct {
	Timestamp  string `json:"timestamp"`
	Level      string `json:"level"`
	IncidentID string `json:"incident_id"`
	Service    string `json:"service"`
	Language   string `json:"language"`
	SourceFile string `json:"source_file"`
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
}

// Runtime is the standalone 5-service Retail Storefront HTTP server, incident engine, and Git workspace host.
type Runtime struct {
	mu                sync.RWMutex
	cfg               Config
	gitMgr            *GitWorkspaceManager
	activeIncidentID  string
	healedServices    map[string]map[string]bool
	http200ByService  map[string]int
	http500ByService  map[string]int
	incident500Counts map[string]map[string]int
	logs              []LogEntry
	orderSeq          int64
}

// NewRuntime initializes the Retail Storefront runtime and seeds the in-cluster Git repository.
func NewRuntime(cfg Config) (*Runtime, error) {
	rt := &Runtime{
		cfg:               cfg,
		healedServices:    make(map[string]map[string]bool),
		http200ByService:  make(map[string]int),
		http500ByService:  make(map[string]int),
		incident500Counts: make(map[string]map[string]int),
	}
	for _, svc := range AllServices() {
		rt.http200ByService[svc] = 1
	}

	gitMgr, err := NewGitWorkspaceManager(cfg.GitWorkDir, rt.onGitPush)
	if err != nil {
		return nil, err
	}
	rt.gitMgr = gitMgr
	return rt, nil
}

// GitManager returns the underlying GitWorkspaceManager.
func (rt *Runtime) GitManager() *GitWorkspaceManager {
	return rt.gitMgr
}

func (rt *Runtime) onGitPush(repoDir string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	for _, sc := range ScenarioCatalog() {
		if rt.healedServices[sc.ID] == nil {
			rt.healedServices[sc.ID] = make(map[string]bool)
		}
		allFixed := true
		for _, svc := range sc.ImplicatedServices {
			if IsServicePatchedForIncident(repoDir, sc.ID, svc) {
				rt.healedServices[sc.ID][svc] = true
			}
			if !rt.healedServices[sc.ID][svc] {
				allFixed = false
			}
		}
		if allFixed && rt.activeIncidentID == sc.ID {
			rt.activeIncidentID = ""
		}
	}
}

// ActivateIncident activates one of INC-101..INC-104 and records initial 500 telemetry.
func (rt *Runtime) ActivateIncident(incidentID string) (IncidentScenario, error) {
	sc, ok := GetScenario(incidentID)
	if !ok {
		return IncidentScenario{}, fmt.Errorf("unknown incident scenario: %s", incidentID)
	}

	rt.mu.Lock()
	rt.activeIncidentID = sc.ID
	rt.healedServices[sc.ID] = make(map[string]bool)
	newLogs := rt.recordIncidentFailureLocked(sc)
	hook := rt.cfg.OnIncidentHook
	rt.mu.Unlock()

	if hook != nil {
		hook(sc, newLogs)
	}
	return sc, nil
}

// RandomizeIncident picks one of the 4 Incident Scenarios using crypto/rand and activates it.
func (rt *Runtime) RandomizeIncident() (IncidentScenario, error) {
	catalog := ScenarioCatalog()
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(catalog))))
	if err != nil {
		return IncidentScenario{}, err
	}
	return rt.ActivateIncident(catalog[n.Int64()].ID)
}

// HealIncident marks all implicated services for an incident (or all incidents if empty) as healed.
func (rt *Runtime) HealIncident(incidentID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	for _, sc := range ScenarioCatalog() {
		if incidentID != "" && sc.ID != incidentID {
			continue
		}
		if rt.healedServices[sc.ID] == nil {
			rt.healedServices[sc.ID] = make(map[string]bool)
		}
		for _, svc := range sc.ImplicatedServices {
			rt.healedServices[sc.ID][svc] = true
		}
		if rt.activeIncidentID == sc.ID {
			rt.activeIncidentID = ""
		}
	}
}

// MarkServiceHealed marks a single microservice healed for a specific incident scenario.
func (rt *Runtime) MarkServiceHealed(incidentID, service string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.healedServices[incidentID] == nil {
		rt.healedServices[incidentID] = make(map[string]bool)
	}
	rt.healedServices[incidentID][service] = true

	if sc, ok := GetScenario(incidentID); ok {
		allHealed := true
		for _, svc := range sc.ImplicatedServices {
			if !rt.healedServices[incidentID][svc] {
				allHealed = false
				break
			}
		}
		if allHealed && rt.activeIncidentID == incidentID {
			rt.activeIncidentID = ""
		}
	}
}

// ActiveScenario returns the currently active IncidentScenario, or nil if healthy.
func (rt *Runtime) ActiveScenario() *IncidentScenario {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	if rt.activeIncidentID == "" {
		return nil
	}
	sc, ok := GetScenario(rt.activeIncidentID)
	if !ok {
		return nil
	}
	return &sc
}

// Logs returns structured error log entries, optionally filtered by incidentID.
func (rt *Runtime) Logs(incidentID string) []LogEntry {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var out []LogEntry
	for _, entry := range rt.logs {
		if incidentID == "" || entry.IncidentID == incidentID {
			out = append(out, entry)
		}
	}
	return out
}

func (rt *Runtime) recordIncidentFailureLocked(sc IncidentScenario) []LogEntry {
	if rt.incident500Counts[sc.ID] == nil {
		rt.incident500Counts[sc.ID] = make(map[string]int)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var batch []LogEntry
	for _, svc := range sc.ImplicatedServices {
		if rt.healedServices[sc.ID] != nil && rt.healedServices[sc.ID][svc] {
			continue
		}
		rt.http500ByService[svc]++
		rt.incident500Counts[sc.ID][svc]++
		detail := sc.ServiceTraces[svc]
		entry := LogEntry{
			Timestamp:  now,
			Level:      "ERROR",
			IncidentID: sc.ID,
			Service:    svc,
			Language:   detail.Language,
			SourceFile: detail.SourceFile,
			ErrorType:  detail.ErrorType,
			Message:    detail.ErrorMessage,
			StackTrace: detail.StackTrace,
		}
		rt.logs = append(rt.logs, entry)
		batch = append(batch, entry)
	}
	return batch
}

// Handler returns the HTTP handler for the Retail Storefront (:3001).
func (rt *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	mux.Handle("/git/", rt.gitMgr)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/api/products", rt.handleProducts)
	mux.HandleFunc("/api/cart/checkout", rt.handleCheckout)
	mux.HandleFunc("/api/incidents", rt.handleIncidents)
	mux.HandleFunc("/api/incidents/activate", rt.handleActivateIncident)
	mux.HandleFunc("/api/incidents/randomize", rt.handleRandomizeIncident)
	mux.HandleFunc("/api/incidents/heal", rt.handleHealIncident)
	mux.HandleFunc("/api/logs", rt.handleLogs)
	mux.HandleFunc("/metrics", rt.handleMetrics)
	mux.HandleFunc("/", rt.handleIndex)

	return httputil.WithSecurityHeaders(mux)
}

func (rt *Runtime) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
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

func (rt *Runtime) handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"products": ProductCatalog(),
		"services": AllServices(),
	})
}

func (rt *Runtime) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios":       ScenarioCatalog(),
		"active_scenario": rt.ActiveScenario(),
	})
}

func (rt *Runtime) handleActivateIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IncidentID string `json:"incident_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IncidentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incident_id is required"})
		return
	}
	sc, err := rt.ActivateIncident(req.IncidentID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "activated",
		"active_scenario": sc,
	})
}

func (rt *Runtime) handleRandomizeIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	sc, err := rt.RandomizeIncident()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "randomized",
		"active_scenario": sc,
	})
}

func (rt *Runtime) handleHealIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IncidentID string `json:"incident_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	rt.HealIncident(req.IncidentID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "healed",
		"active_scenario": rt.ActiveScenario(),
	})
}

func (rt *Runtime) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	incidentID := r.URL.Query().Get("incident_id")
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": rt.Logs(incidentID),
	})
}

func (rt *Runtime) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CheckoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, CheckoutResponse{
			Status: "INVALID_REQUEST",
			Error:  "invalid JSON payload",
		})
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	rt.mu.Lock()
	matchedScenario := rt.detectTriggeredScenarioLocked(req)
	if matchedScenario != nil && !rt.isScenarioFullyHealedLocked(matchedScenario.ID) {
		rt.recordIncidentFailureLocked(*matchedScenario)
		var failingSvcs []string
		var traces []string
		svcDetails := make(map[string]ServiceIncidentDetail)
		for _, svc := range matchedScenario.ImplicatedServices {
			if rt.healedServices[matchedScenario.ID] != nil && rt.healedServices[matchedScenario.ID][svc] {
				continue
			}
			failingSvcs = append(failingSvcs, svc)
			detail := matchedScenario.ServiceTraces[svc]
			traces = append(traces, detail.StackTrace)
			svcDetails[svc] = detail
		}
		rt.mu.Unlock()

		writeJSON(w, http.StatusInternalServerError, CheckoutResponse{
			Status:             "FAILED",
			IncidentID:         matchedScenario.ID,
			ErrorCode:          matchedScenario.ErrorCode,
			Error:              matchedScenario.ErrorMessage,
			ImplicatedServices: failingSvcs,
			UnaffectedServices: matchedScenario.UnaffectedServices,
			StackTraces:        traces,
			ServiceTraces:      svcDetails,
		})
		return
	}

	for _, svc := range AllServices() {
		rt.http200ByService[svc]++
	}
	rt.orderSeq++
	orderID := fmt.Sprintf("ORD-%d", 1000+rt.orderSeq)
	rt.mu.Unlock()

	subtotalCents, shippingCents, chargedMinor := computeHealedOrderTotals(req)
	_ = subtotalCents
	writeJSON(w, http.StatusOK, CheckoutResponse{
		OrderID:           orderID,
		Status:            "CONFIRMED",
		Currency:          strings.ToUpper(req.Currency),
		ChargedMinorUnits: chargedMinor,
		ShippingCents:     shippingCents,
	})
}

func (rt *Runtime) detectTriggeredScenarioLocked(req CheckoutRequest) *IncidentScenario {
	// Check if cart directly matches one of the 4 edge-case scenarios
	hasSave20 := false
	hasFlat15 := false
	for _, p := range req.PromoCodes {
		switch strings.ToUpper(strings.TrimSpace(p)) {
		case "SAVE20":
			hasSave20 = true
		case "FLAT15":
			hasFlat15 = true
		}
	}
	productsBySKU := make(map[string]Product)
	for _, p := range ProductCatalog() {
		productsBySKU[p.SKU] = p
	}

	subtotal := 0
	hasBundleOverflow := false
	hasLithiumHazmat := false
	for _, item := range req.Items {
		prod, exists := productsBySKU[item.SKU]
		if !exists {
			continue
		}
		subtotal += prod.PriceUSDCents * item.Quantity
		if (req.FlashBundle || prod.FlashBundleBonusUnits > 0) && item.Quantity+prod.FlashBundleBonusUnits > prod.AvailableStock {
			hasBundleOverflow = true
		}
		if prod.HazmatClass == "UN3480-LITHIUM" && prod.WeightGrams > 1000 {
			hasLithiumHazmat = true
		}
	}

	if hasSave20 && hasFlat15 && int(math.Round(float64(subtotal)*0.80))-1500 <= 0 {
		sc, _ := GetScenario(IncidentStackedPromoNegativeSubtotal)
		return &sc
	}
	if hasBundleOverflow {
		sc, _ := GetScenario(IncidentFlashSaleBundleUnderflow)
		return &sc
	}
	if strings.EqualFold(req.Currency, "JPY") {
		sc, _ := GetScenario(IncidentZeroDecimalJPYRounding)
		return &sc
	}
	if hasLithiumHazmat {
		sc, _ := GetScenario(IncidentLithiumHazmatWeightTier)
		return &sc
	}

	if rt.activeIncidentID != "" {
		if sc, ok := GetScenario(rt.activeIncidentID); ok {
			return &sc
		}
	}
	return nil
}

func (rt *Runtime) isScenarioFullyHealedLocked(incidentID string) bool {
	sc, ok := GetScenario(incidentID)
	if !ok {
		return true
	}
	healedMap := rt.healedServices[incidentID]
	if healedMap == nil {
		return false
	}
	for _, svc := range sc.ImplicatedServices {
		if !healedMap[svc] {
			return false
		}
	}
	return true
}

func computeHealedOrderTotals(req CheckoutRequest) (int, int, int64) {
	productsBySKU := make(map[string]Product)
	for _, p := range ProductCatalog() {
		productsBySKU[p.SKU] = p
	}
	subtotal := 0
	weight := 0
	hazmat := "NONE"
	for _, item := range req.Items {
		if p, ok := productsBySKU[item.SKU]; ok {
			subtotal += p.PriceUSDCents * item.Quantity
			weight += p.WeightGrams * item.Quantity
			if p.HazmatClass != "" && p.HazmatClass != "NONE" {
				hazmat = p.HazmatClass
			}
		}
	}
	for _, code := range req.PromoCodes {
		switch strings.ToUpper(strings.TrimSpace(code)) {
		case "SAVE20":
			subtotal = int(math.Round(float64(subtotal) * 0.80))
		case "FLAT15":
			subtotal -= 1500
		}
	}
	if subtotal < 0 {
		subtotal = 0
	}
	shipping := 799
	if hazmat == "UN3480-LITHIUM" {
		shipping = 3499
	}
	totalUSDCents := subtotal + shipping
	switch strings.ToUpper(req.Currency) {
	case "EUR":
		return subtotal, shipping, int64(math.Round(float64(totalUSDCents) * 0.92))
	case "JPY":
		return subtotal, shipping, int64(math.Round((float64(totalUSDCents) / 100.0) * 154.35))
	default:
		return subtotal, shipping, int64(totalUSDCents)
	}
}

func (rt *Runtime) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("# HELP storefront_http_requests_total Total HTTP requests processed by storefront microservice and status.\n")
	sb.WriteString("# TYPE storefront_http_requests_total counter\n")
	for _, svc := range AllServices() {
		sb.WriteString(fmt.Sprintf("storefront_http_requests_total{status=\"200\",service=%q} %d\n", svc, rt.http200ByService[svc]))
		sb.WriteString(fmt.Sprintf("storefront_http_requests_total{status=\"500\",service=%q} %d\n", svc, rt.http500ByService[svc]))
	}

	sb.WriteString("# HELP storefront_incident_active Whether a specific incident scenario is currently active on a microservice (1 = active, 0 = healthy).\n")
	sb.WriteString("# TYPE storefront_incident_active gauge\n")
	for _, sc := range ScenarioCatalog() {
		implicatedSet := make(map[string]bool)
		for _, svc := range sc.ImplicatedServices {
			implicatedSet[svc] = true
		}
		for _, svc := range AllServices() {
			val := 0
			if rt.activeIncidentID == sc.ID && implicatedSet[svc] && (rt.healedServices[sc.ID] == nil || !rt.healedServices[sc.ID][svc]) {
				val = 1
			}
			sb.WriteString(fmt.Sprintf("storefront_incident_active{incident=%q,service=%q} %d\n", sc.ID, svc, val))
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	httputil.WriteJSON(w, status, payload)
}
