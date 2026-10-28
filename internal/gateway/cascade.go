package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CascadeAttempt records a single upstream candidate model attempt inside the fallback cascade.
type CascadeAttempt struct {
	Model      string `json:"model"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// CascadeResult holds the winning Gemini response payload, active model, provenance, and attempt trace.
type CascadeResult struct {
	ActiveModel      string           `json:"active_model"`
	SourceProvenance SourceProvenance `json:"source_provenance"`
	StatusCode       int              `json:"status_code"`
	ContentType      string           `json:"content_type"`
	Body             []byte           `json:"-"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	Attempts         []CascadeAttempt `json:"attempts"`
}

// BuildCascadeOrder returns the ordered slice of Gemini models to try for the given requested model.
// Per ADR-0007, the default cascade is gemini-3.8-flash -> gemini-3.7-flash -> gemini-3.6-flash -> gemini-3.5-flash.
func BuildCascadeOrder(requested string, configured []string) []string {
	if len(configured) == 0 {
		configured = DefaultFallbackCascade
	}
	cleaned := strings.TrimSpace(requested)
	cleaned = strings.TrimPrefix(cleaned, "google/")
	cleaned = strings.TrimPrefix(cleaned, "models/")

	for idx, m := range configured {
		if strings.EqualFold(cleaned, m) {
			return append([]string(nil), configured[idx:]...)
		}
	}
	return append([]string(nil), configured...)
}

// ShouldFallback returns true if the upstream HTTP status or transport error should trigger a retry on the next model in the cascade.
func ShouldFallback(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	switch statusCode {
	case http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return statusCode >= 500
	}
}

// ExecuteGeminiCascade executes a Gemini generateContent / streamGenerateContent request across the ordered fallback cascade,
// falling back to deterministic FIXTURE synthesis if all upstream models fail and EnableFixtureFallback is true.
func (s *Server) ExecuteGeminiCascade(
	ctx context.Context,
	taskName string,
	requestedModel string,
	action string,
	rawQuery string,
	geminiReqBody []byte,
) (CascadeResult, error) {
	if action == "" {
		action = "generateContent"
	}
	candidates := BuildCascadeOrder(requestedModel, s.cfg.FallbackCascade)
	var attempts []CascadeAttempt

	tok, tokErr := s.tokenFn(ctx)
	if tokErr != nil {
		// Record token acquisition failure against primary candidate before fixture fallback.
		if len(candidates) > 0 {
			attempts = append(attempts, CascadeAttempt{
				Model: candidates[0],
				Error: tokErr.Error(),
			})
		}
		if s.cfg.EnableFixtureFallback {
			return SynthesizeFixtureGeminiResult(taskName, action, geminiReqBody, attempts), nil
		}
		return CascadeResult{Attempts: attempts}, fmt.Errorf("workload identity token error: %w", tokErr)
	}

	base := strings.TrimRight(s.cfg.AgentPlatformBaseURL, "/")
	if base == "" {
		base = "https://aiplatform.googleapis.com"
	}

	for _, candidate := range candidates {
		attemptStart := time.Now()
		targetURL := fmt.Sprintf(
			"%s/v1beta1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			base,
			s.cfg.ProjectID,
			s.cfg.Location,
			candidate,
			action,
		)
		if rawQuery != "" {
			targetURL += "?" + rawQuery
		}

		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(geminiReqBody))
		if err != nil {
			attempts = append(attempts, CascadeAttempt{
				Model:      candidate,
				Error:      err.Error(),
				DurationMs: time.Since(attemptStart).Milliseconds(),
			})
			continue
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+tok)

		resp, err := s.httpClient.Do(upReq)
		dur := time.Since(attemptStart).Milliseconds()
		if err != nil {
			attempts = append(attempts, CascadeAttempt{
				Model:      candidate,
				Error:      err.Error(),
				DurationMs: dur,
			})
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			attempts = append(attempts, CascadeAttempt{
				Model:      candidate,
				StatusCode: resp.StatusCode,
				Error:      readErr.Error(),
				DurationMs: dur,
			})
			continue
		}

		attempts = append(attempts, CascadeAttempt{
			Model:      candidate,
			StatusCode: resp.StatusCode,
			DurationMs: dur,
		})

		if !ShouldFallback(resp.StatusCode, nil) {
			inTok, outTok := extractGeminiTokenUsage(respBody)
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			return CascadeResult{
				ActiveModel:      candidate,
				SourceProvenance: ProvenanceLive,
				StatusCode:       resp.StatusCode,
				ContentType:      contentType,
				Body:             respBody,
				InputTokens:      inTok,
				OutputTokens:     outTok,
				Attempts:         attempts,
			}, nil
		}
	}

	if s.cfg.EnableFixtureFallback {
		return SynthesizeFixtureGeminiResult(taskName, action, geminiReqBody, attempts), nil
	}

	lastStatus := http.StatusBadGateway
	if len(attempts) > 0 && attempts[len(attempts)-1].StatusCode > 0 {
		lastStatus = attempts[len(attempts)-1].StatusCode
	}
	return CascadeResult{
		StatusCode: lastStatus,
		Attempts:   attempts,
	}, fmt.Errorf("all models in Gemini Flash fallback cascade failed (%d attempts)", len(attempts))
}

// SynthesizeFixtureGeminiResult constructs a deterministic Gemini generateContent response for offline/CI test runs.
func SynthesizeFixtureGeminiResult(taskName, action string, geminiReqBody []byte, attempts []CascadeAttempt) CascadeResult {
	promptSummary := extractPromptSummary(geminiReqBody)
	replyText := fmt.Sprintf(
		"[FIXTURE:%s] Deterministic Gemini Enterprise Agent Platform synthesis for %s",
		taskName,
		promptSummary,
	)

	inTok := int64(len(geminiReqBody)/4) + 16
	outTok := int64(len(replyText)/4) + 12

	payload := map[string]any{
		"modelVersion": "FIXTURE",
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"role": "model",
					"parts": []map[string]any{
						{"text": replyText},
					},
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     inTok,
			"candidatesTokenCount": outTok,
			"totalTokenCount":      inTok + outTok,
		},
	}

	raw, _ := json.Marshal(payload)
	contentType := "application/json"
	if strings.EqualFold(action, "streamGenerateContent") {
		raw = []byte("data: " + string(raw) + "\n\n")
		contentType = "text/event-stream"
	}

	return CascadeResult{
		ActiveModel:      string(ProvenanceFixture),
		SourceProvenance: ProvenanceFixture,
		StatusCode:       http.StatusOK,
		ContentType:      contentType,
		Body:             raw,
		InputTokens:      inTok,
		OutputTokens:     outTok,
		Attempts:         attempts,
	}
}

func extractPromptSummary(geminiReqBody []byte) string {
	var parsed struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(geminiReqBody, &parsed); err == nil {
		for i := len(parsed.Contents) - 1; i >= 0; i-- {
			for _, p := range parsed.Contents[i].Parts {
				if t := strings.TrimSpace(p.Text); t != "" {
					if len(t) > 96 {
						return t[:96]
					}
					return t
				}
			}
		}
	}
	return "incident-remediation"
}

func extractGeminiTokenUsage(body []byte) (inputTokens, outputTokens int64) {
	var single struct {
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &single); err == nil && single.UsageMetadata != nil {
		return single.UsageMetadata.PromptTokenCount, single.UsageMetadata.CandidatesTokenCount
	}

	// Also support SSE lines ("data: {...}")
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if strings.HasPrefix(line, "{") {
			if err := json.Unmarshal([]byte(line), &single); err == nil && single.UsageMetadata != nil {
				if single.UsageMetadata.PromptTokenCount > 0 {
					inputTokens = single.UsageMetadata.PromptTokenCount
				}
				if single.UsageMetadata.CandidatesTokenCount > 0 {
					outputTokens = single.UsageMetadata.CandidatesTokenCount
				}
			}
		}
	}
	return inputTokens, outputTokens
}
