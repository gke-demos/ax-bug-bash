package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	shipping "github.com/gke-demos/ax-bug-bash/services/shipping-service"
)

type quoteRequest struct {
	SubtotalCents int    `json:"subtotal_cents"`
	WeightGrams   int    `json:"weight_grams"`
	HazmatClass   string `json:"hazmat_class"`
}

type structuredLogEntry struct {
	Timestamp  string `json:"timestamp"`
	Level      string `json:"level"`
	RequestID  string `json:"request_id,omitempty"`
	IncidentID string `json:"incident_id,omitempty"`
	Service    string `json:"service"`
	Language   string `json:"language"`
	SourceFile string `json:"source_file"`
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
}

func main() {
	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if addr == "" {
		port := strings.TrimSpace(os.Getenv("PORT"))
		if port == "" {
			port = "8082"
		}
		addr = "127.0.0.1:" + port
	}
	lokiPushURL := strings.TrimSpace(os.Getenv("LOKI_PUSH_URL"))
	lokiClient := &http.Client{Timeout: 2 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "shipping-service"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "service": "shipping-service"})
	})
	mux.HandleFunc("/api/shipping/quote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		reqID := strings.TrimSpace(r.Header.Get("X-Storefront-Request-ID"))
		scenarioHdr := strings.TrimSpace(r.Header.Get("X-Incident-Scenario"))

		defer func() {
			if rec := recover(); rec != nil {
				msg := fmt.Sprintf("panic in QuoteCarrierRate: %v", rec)
				stack := fmt.Sprintf("%s\n%s", msg, string(debug.Stack()))
				emitFailure(w, lokiClient, lokiPushURL, reqID, scenarioHdr, "ShippingRuntimePanic", msg, stack)
			}
		}()

		var req quoteRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
			return
		}

		rateCents, err := shipping.QuoteCarrierRate(req.SubtotalCents, req.WeightGrams, req.HazmatClass)
		if err != nil {
			errMsg := err.Error()
			errType := classifyShippingError(errMsg)
			incidentID := scenarioHdr
			if incidentID == "" {
				incidentID = inferShippingIncident(errType)
			}
			stack := fmt.Sprintf("%s (services/shipping-service/shipping.go:QuoteCarrierRate)\n%s", errMsg, string(debug.Stack()))
			emitFailure(w, lokiClient, lokiPushURL, reqID, incidentID, errType, errMsg, stack)
			return
		}

		if reqID != "" {
			w.Header().Set("X-Storefront-Request-ID", reqID)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service":    "shipping-service",
			"rate_cents": rateCents,
			"request_id": reqID,
		})
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           withSecurityMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("shipping-service error: %v", err)
	}
}

func classifyShippingError(msg string) string {
	switch {
	case strings.Contains(msg, "CarrierTariffBoundsError"):
		return "CarrierTariffBoundsError"
	case strings.Contains(msg, "HazmatWeightTierOverflowError"):
		return "HazmatWeightTierOverflowError"
	default:
		return "ShippingDomainError"
	}
}

func inferShippingIncident(errType string) string {
	switch errType {
	case "CarrierTariffBoundsError":
		return "INC-101"
	case "HazmatWeightTierOverflowError":
		return "INC-104"
	default:
		return ""
	}
}

func emitFailure(
	w http.ResponseWriter,
	lokiClient *http.Client,
	lokiPushURL, reqID, incidentID, errType, msg, stack string,
) {
	now := time.Now().UTC()
	entry := structuredLogEntry{
		Timestamp:  now.Format(time.RFC3339Nano),
		Level:      "ERROR",
		RequestID:  reqID,
		IncidentID: incidentID,
		Service:    "shipping-service",
		Language:   "Go",
		SourceFile: "shipping.go",
		ErrorType:  errType,
		Message:    msg,
		StackTrace: stack,
	}
	lineBytes, _ := json.Marshal(entry)
	_, _ = os.Stdout.Write(append(lineBytes, '\n'))

	if lokiPushURL != "" {
		lineStr := string(lineBytes)
		nsStr := strconv.FormatInt(now.UnixNano(), 10)
		go pushToLoki(lokiClient, lokiPushURL, incidentID, nsStr, lineStr)
	}

	if reqID != "" {
		w.Header().Set("X-Storefront-Request-ID", reqID)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"status":      "FAILED",
		"service":     "shipping-service",
		"language":    "Go",
		"source_file": "shipping.go",
		"incident_id": incidentID,
		"error_type":  errType,
		"message":     msg,
		"stack_trace": stack,
		"request_id":  reqID,
	})
}

func pushToLoki(client *http.Client, lokiPushURL, incidentID, tsNano, logLine string) {
	streamLabels := map[string]string{
		"app":      "storefront",
		"service":  "shipping-service",
		"language": "Go",
		"level":    "ERROR",
	}
	if incidentID != "" {
		streamLabels["incident_id"] = incidentID
	}
	payload := map[string]any{
		"streams": []map[string]any{
			{
				"stream": streamLabels,
				"values": [][]string{{tsNano, logLine}},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, lokiPushURL, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func withSecurityMiddleware(next http.Handler) http.Handler {
	// TODO(security): Internal cluster microservice behind polyglot-supervisor; network policy isolates direct pod traffic.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions:
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
