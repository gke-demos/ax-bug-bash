package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
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
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
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

var (
	fixturePhaseRe    = regexp.MustCompile(`\bPHASE=(REPRODUCE_RED|PATCH_GREEN)\b`)
	fixtureIncidentRe = regexp.MustCompile(`\bINCIDENT_ID=([A-Za-z0-9-]+)\b`)
	fixtureServiceRe  = regexp.MustCompile(`\bSERVICE=([a-z0-9-]+)\b`)
)

// SynthesizeFixtureText generates deterministic model output for offline/CI test runs.
// When the prompt contains structured TDD context (PHASE=REPRODUCE_RED or PHASE=PATCH_GREEN,
// INCIDENT_ID=<id>, and SERVICE=<svc>), it emits a real <file path="...">...</file> block
// containing the scenario's regression test or patched source file so the harness file-extractor
// genuinely parses and writes the model's <file> block.
func SynthesizeFixtureText(taskName, promptText string) string {
	phaseMatch := fixturePhaseRe.FindStringSubmatch(promptText)
	incMatch := fixtureIncidentRe.FindStringSubmatch(promptText)
	svcMatch := fixtureServiceRe.FindStringSubmatch(promptText)

	if len(phaseMatch) == 2 && len(incMatch) == 2 && len(svcMatch) == 2 {
		phase := phaseMatch[1]
		incidentID := strings.ToUpper(incMatch[1])
		service := svcMatch[1]
		if sc, ok := storefront.GetScenario(incidentID); ok {
			if detail, ok := sc.ServiceTraces[service]; ok {
				switch phase {
				case "REPRODUCE_RED":
					relPath := filepath.ToSlash(filepath.Join("services", service, detail.TestFile))
					return fmt.Sprintf(
						"[FIXTURE:%s] Authored failing regression test %s for %s/%s:\n<file path=%q>\n%s</file>",
						taskName,
						detail.RegressionTestName,
						incidentID,
						service,
						relPath,
						detail.RegressionTestFileContent,
					)
				case "PATCH_GREEN":
					relPath := filepath.ToSlash(filepath.Join("services", service, detail.SourceFile))
					return fmt.Sprintf(
						"[FIXTURE:%s] Patched %s for %s/%s so %s passes:\n<file path=%q>\n%s</file>",
						taskName,
						detail.SourceFile,
						incidentID,
						service,
						detail.RegressionTestName,
						relPath,
						detail.PatchedSourceFileContent,
					)
				}
			}
		}
	}

	promptSummary := strings.TrimSpace(promptText)
	if len(promptSummary) > 96 {
		promptSummary = promptSummary[:96]
	}
	if promptSummary == "" {
		promptSummary = "incident-remediation"
	}
	return fmt.Sprintf(
		"[FIXTURE:%s] Deterministic Gemini Enterprise Agent Platform synthesis for %s",
		taskName,
		promptSummary,
	)
}

// SynthesizeFixtureGeminiResult constructs a deterministic Gemini generateContent response for offline/CI test runs.
func SynthesizeFixtureGeminiResult(taskName, action string, geminiReqBody []byte, attempts []CascadeAttempt) CascadeResult {
	promptText := extractPromptText(geminiReqBody)
	replyText := SynthesizeFixtureText(taskName, promptText)

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

func extractPromptText(geminiReqBody []byte) string {
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
