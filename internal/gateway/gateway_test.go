package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
)

func jsonReader(s string) io.Reader {
	return strings.NewReader(s)
}

func TestHealthzAndConfigEndpoints(t *testing.T) {
	cfg := gateway.DefaultConfig()
	cfg.ProjectID = "ax-demo-project"
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-test-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Verify /healthz
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /healthz, got %d", resp.StatusCode)
	}

	// 2. Verify /api/gateway/config exposes the ordered Gemini Flash cascade and supported harnesses
	cfgResp, err := http.Get(ts.URL + "/api/gateway/config")
	if err != nil {
		t.Fatalf("GET /api/gateway/config failed: %v", err)
	}
	defer cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/gateway/config, got %d", cfgResp.StatusCode)
	}

	var gotCfg gateway.Config
	if err := json.NewDecoder(cfgResp.Body).Decode(&gotCfg); err != nil {
		t.Fatalf("decode config response: %v", err)
	}

	wantCascade := []string{
		"gemini-3.8-flash",
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
	}
	if !reflect.DeepEqual(gotCfg.FallbackCascade, wantCascade) {
		t.Errorf("FallbackCascade = %v, want %v", gotCfg.FallbackCascade, wantCascade)
	}

	wantHarnesses := []string{"antigravity", "opencode", "pi"}
	if !reflect.DeepEqual(gotCfg.SupportedHarnesses, wantHarnesses) {
		t.Errorf("SupportedHarnesses = %v, want %v", gotCfg.SupportedHarnesses, wantHarnesses)
	}
	if !gotCfg.ZeroSecretMode {
		t.Errorf("expected ZeroSecretMode to be true")
	}
}

func TestGeminiGenerateContentWithWorkloadIdentityAndSessionLedger(t *testing.T) {
	var upstreamAuthHeader string
	var upstreamAPIKeyHeader string
	var upstreamPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuthHeader = r.Header.Get("Authorization")
		upstreamAPIKeyHeader = r.Header.Get("x-goog-api-key")
		upstreamPath = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{"text": "Added failing regression test for INC-101 in pricing-service."}]
				}
			}],
			"usageMetadata": {
				"promptTokenCount": 120,
				"candidatesTokenCount": 45,
				"totalTokenCount": 165
			}
		}`))
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.ProjectID = "ax-demo-project"
	cfg.Location = "global"
	cfg.AgentPlatformBaseURL = upstream.URL

	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-real-gcp-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"Fix INC-101 negative subtotal bug"}]}]}`
	req, err := http.NewRequest(
		http.MethodPost,
		ts.URL+"/s/fixer-pricing-service/v1beta/models/gemini-3.8-flash:generateContent",
		nil,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Body = http.NoBody
	req, _ = http.NewRequest(
		http.MethodPost,
		ts.URL+"/s/fixer-pricing-service/v1beta/models/gemini-3.8-flash:generateContent",
		jsonReader(reqBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", gateway.SandboxTokenPlaceholder)
	req.Header.Set("X-AX-Harness", "antigravity")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST generateContent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if upstreamAuthHeader != "Bearer wi-real-gcp-token" {
		t.Errorf("upstream Authorization = %q, want %q", upstreamAuthHeader, "Bearer wi-real-gcp-token")
	}
	if upstreamAPIKeyHeader != "" {
		t.Errorf("expected sandbox dummy API key to be stripped before upstream, got %q", upstreamAPIKeyHeader)
	}
	wantPath := "/v1beta1/projects/ax-demo-project/locations/global/publishers/google/models/gemini-3.8-flash:generateContent"
	if upstreamPath != wantPath {
		t.Errorf("upstreamPath = %q, want %q", upstreamPath, wantPath)
	}
	if got := resp.Header.Get("X-AX-Source-Provenance"); got != string(gateway.ProvenanceLive) {
		t.Errorf("X-AX-Source-Provenance = %q, want %q", got, gateway.ProvenanceLive)
	}
	if got := resp.Header.Get("X-AX-Active-Model"); got != "gemini-3.8-flash" {
		t.Errorf("X-AX-Active-Model = %q, want gemini-3.8-flash", got)
	}

	// Verify session ledger via GET /api/gateway/sessions
	sessResp, err := http.Get(ts.URL + "/api/gateway/sessions")
	if err != nil {
		t.Fatalf("GET /api/gateway/sessions: %v", err)
	}
	defer sessResp.Body.Close()
	if sessResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /api/gateway/sessions, got %d", sessResp.StatusCode)
	}

	var ledger struct {
		Sessions []gateway.TaskSession `json:"sessions"`
	}
	if err := json.NewDecoder(sessResp.Body).Decode(&ledger); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(ledger.Sessions) != 1 {
		t.Fatalf("expected 1 session in ledger, got %d", len(ledger.Sessions))
	}
	sess := ledger.Sessions[0]
	if sess.TaskName != "fixer-pricing-service" {
		t.Errorf("TaskName = %q, want fixer-pricing-service", sess.TaskName)
	}
	if sess.Harness != "antigravity" {
		t.Errorf("Harness = %q, want antigravity", sess.Harness)
	}
	if sess.ActiveModel != "gemini-3.8-flash" {
		t.Errorf("ActiveModel = %q, want gemini-3.8-flash", sess.ActiveModel)
	}
	if sess.SourceProvenance != gateway.ProvenanceLive {
		t.Errorf("SourceProvenance = %q, want LIVE", sess.SourceProvenance)
	}
	if sess.TotalInputTokens != 120 || sess.TotalOutputTokens != 45 || sess.TotalTokens != 165 {
		t.Errorf("tokens = (%d, %d, %d), want (120, 45, 165)",
			sess.TotalInputTokens, sess.TotalOutputTokens, sess.TotalTokens)
	}
	if !sess.ZeroSecretVerified {
		t.Errorf("expected ZeroSecretVerified to be true")
	}
}

func TestGeminiFlashCascadeOrderAndFallback(t *testing.T) {
	var attemptedModels []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path ends with /models/{model}:generateContent
		idx := strings.Index(r.URL.Path, "/models/")
		tail := r.URL.Path[idx+len("/models/"):]
		model := strings.Split(tail, ":")[0]
		attemptedModels = append(attemptedModels, model)

		switch model {
		case "gemini-3.8-flash":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model gemini-3.8-flash not found"}}`))
		case "gemini-3.7-flash":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
		case "gemini-3.6-flash":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"backend unavailable"}}`))
		case "gemini-3.5-flash":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {
						"role": "model",
						"parts": [{"text": "Patched shipping-service negative weight guard."}]
					}
				}],
				"usageMetadata": {
					"promptTokenCount": 80,
					"candidatesTokenCount": 30,
					"totalTokenCount": 110
				}
			}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.AgentPlatformBaseURL = upstream.URL
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(
		ts.URL+"/s/fixer-shipping-service/v1beta/models/gemini-3.8-flash:generateContent",
		"application/json",
		jsonReader(`{"contents":[{"role":"user","parts":[{"text":"Fix INC-101 in shipping-service"}]}]}`),
	)
	if err != nil {
		t.Fatalf("POST generateContent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after cascade, got %d", resp.StatusCode)
	}

	wantOrder := []string{
		"gemini-3.8-flash",
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
	}
	if !reflect.DeepEqual(attemptedModels, wantOrder) {
		t.Fatalf("attemptedModels = %v, want exact cascade %v", attemptedModels, wantOrder)
	}

	if got := resp.Header.Get("X-AX-Active-Model"); got != "gemini-3.5-flash" {
		t.Errorf("X-AX-Active-Model = %q, want gemini-3.5-flash", got)
	}
	if got := resp.Header.Get("X-AX-Source-Provenance"); got != string(gateway.ProvenanceLive) {
		t.Errorf("X-AX-Source-Provenance = %q, want LIVE", got)
	}

	sess, ok := srv.GetSession("fixer-shipping-service")
	if !ok {
		t.Fatalf("expected session fixer-shipping-service to exist")
	}
	if sess.ActiveModel != "gemini-3.5-flash" {
		t.Errorf("sess.ActiveModel = %q, want gemini-3.5-flash", sess.ActiveModel)
	}
	if sess.CascadeFallbacks != 3 {
		t.Errorf("sess.CascadeFallbacks = %d, want 3", sess.CascadeFallbacks)
	}
	if len(sess.LastCascadeTrace) != 4 {
		t.Errorf("len(sess.LastCascadeTrace) = %d, want 4", len(sess.LastCascadeTrace))
	}
}

func TestGeminiCascadeExhaustionFallsBackToFixture(t *testing.T) {
	var attemptedModels []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := strings.Index(r.URL.Path, "/models/")
		tail := r.URL.Path[idx+len("/models/"):]
		model := strings.Split(tail, ":")[0]
		attemptedModels = append(attemptedModels, model)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"offline in CI"}}`))
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.AgentPlatformBaseURL = upstream.URL
	cfg.EnableFixtureFallback = true
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(
		ts.URL+"/s/triage-inc-103/v1beta/models/gemini-3.8-flash:generateContent",
		"application/json",
		jsonReader(`{"contents":[{"role":"user","parts":[{"text":"Triage INC-103 JPY rounding crash"}]}]}`),
	)
	if err != nil {
		t.Fatalf("POST generateContent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from FIXTURE fallback, got %d", resp.StatusCode)
	}

	wantOrder := []string{
		"gemini-3.8-flash",
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
	}
	if !reflect.DeepEqual(attemptedModels, wantOrder) {
		t.Fatalf("attemptedModels = %v, want %v", attemptedModels, wantOrder)
	}

	if got := resp.Header.Get("X-AX-Source-Provenance"); got != string(gateway.ProvenanceFixture) {
		t.Errorf("X-AX-Source-Provenance = %q, want FIXTURE", got)
	}
	if got := resp.Header.Get("X-AX-Active-Model"); got != "FIXTURE" {
		t.Errorf("X-AX-Active-Model = %q, want FIXTURE", got)
	}

	sess, ok := srv.GetSession("triage-inc-103")
	if !ok {
		t.Fatalf("expected session triage-inc-103")
	}
	if sess.SourceProvenance != gateway.ProvenanceFixture {
		t.Errorf("sess.SourceProvenance = %q, want FIXTURE", sess.SourceProvenance)
	}
	if sess.TotalTokens <= 0 {
		t.Errorf("expected positive fixture token count, got %d", sess.TotalTokens)
	}
}

func TestOpenAIChatCompletionsBidirectionalToolCallsAndCascade(t *testing.T) {
	var attemptedModels []string
	var capturedGeminiReq map[string]any

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := strings.Index(r.URL.Path, "/models/")
		tail := r.URL.Path[idx+len("/models/"):]
		model := strings.Split(tail, ":")[0]
		attemptedModels = append(attemptedModels, model)

		if model == "gemini-3.8-flash" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"gemini-3.8-flash not available"}}`))
			return
		}

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedGeminiReq)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "Writing failing regression test for INC-102 u32 underflow."},
						{
							"functionCall": {
								"name": "write_file",
								"args": {
									"path": "services/inventory-service/tests/regression_inc102.rs",
									"content": "#[test]\nfn test_bundle_underflow() { assert!(false); }"
								}
							}
						}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 210,
				"candidatesTokenCount": 65,
				"totalTokenCount": 275
			}
		}`))
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.AgentPlatformBaseURL = upstream.URL
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	openAIReq := `{
		"model": "gemini-3.8-flash",
		"messages": [
			{"role": "system", "content": "You are OpenCode running inside an AX Child Task sandbox."},
			{"role": "user", "content": "Fix INC-102 in inventory-service"},
			{
				"role": "assistant",
				"content": "Inspecting lib.rs",
				"tool_calls": [{
					"id": "call_read_1",
					"type": "function",
					"function": {
						"name": "read_file",
						"arguments": "{\"path\":\"services/inventory-service/src/lib.rs\"}"
					}
				}]
			},
			{
				"role": "tool",
				"tool_call_id": "call_read_1",
				"name": "read_file",
				"content": "pub fn reserve_stock(qty: u32, bundle: u32) -> u32 { qty - bundle }"
			}
		],
		"tools": [{
			"type": "function",
			"function": {
				"name": "write_file",
				"description": "Write file in workspace",
				"parameters": {
					"type": "object",
					"properties": {
						"path": {"type": "string"},
						"content": {"type": "string"}
					},
					"required": ["path", "content"]
				}
			}
		}]
	}`

	req, err := http.NewRequest(
		http.MethodPost,
		ts.URL+"/s/fixer-inventory-service-opencode/v1/chat/completions",
		jsonReader(openAIReq),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gateway.SandboxTokenPlaceholder)
	req.Header.Set("X-AX-Harness", "opencode")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	wantAttempted := []string{"gemini-3.8-flash", "gemini-3.7-flash"}
	if !reflect.DeepEqual(attemptedModels, wantAttempted) {
		t.Errorf("attemptedModels = %v, want %v", attemptedModels, wantAttempted)
	}

	// Verify request translation to Gemini format
	if capturedGeminiReq["systemInstruction"] == nil {
		t.Errorf("expected systemInstruction in translated Gemini request")
	}
	if capturedGeminiReq["tools"] == nil {
		t.Errorf("expected tools in translated Gemini request")
	}
	contents, _ := capturedGeminiReq["contents"].([]any)
	if len(contents) != 3 {
		t.Errorf("expected 3 contents entries (user, model tool_call, user functionResponse), got %d", len(contents))
	}

	// Verify response translation back to OpenAI format
	var openAIResp struct {
		ID               string                   `json:"id"`
		Object           string                   `json:"object"`
		Model            string                   `json:"model"`
		SourceProvenance gateway.SourceProvenance `json:"source_provenance"`
		Choices          []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&openAIResp); err != nil {
		t.Fatalf("decode OpenAI response: %v", err)
	}

	if openAIResp.Object != "chat.completion" {
		t.Errorf("Object = %q, want chat.completion", openAIResp.Object)
	}
	if openAIResp.Model != "gemini-3.7-flash" {
		t.Errorf("Model = %q, want gemini-3.7-flash", openAIResp.Model)
	}
	if openAIResp.SourceProvenance != gateway.ProvenanceLive {
		t.Errorf("SourceProvenance = %q, want LIVE", openAIResp.SourceProvenance)
	}
	if len(openAIResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(openAIResp.Choices))
	}
	choice := openAIResp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != "write_file" {
		t.Errorf("ToolCall function name = %q, want write_file", tc.Function.Name)
	}
	if !strings.Contains(tc.Function.Arguments, "regression_inc102.rs") {
		t.Errorf("ToolCall arguments = %q, want substring regression_inc102.rs", tc.Function.Arguments)
	}
	if openAIResp.Usage.PromptTokens != 210 || openAIResp.Usage.CompletionTokens != 65 || openAIResp.Usage.TotalTokens != 275 {
		t.Errorf("Usage = %+v, want (210, 65, 275)", openAIResp.Usage)
	}

	// Verify session ledger via GET /api/gateway/sessions/fixer-inventory-service-opencode
	sessResp, err := http.Get(ts.URL + "/api/gateway/sessions/fixer-inventory-service-opencode")
	if err != nil {
		t.Fatalf("GET session by task: %v", err)
	}
	defer sessResp.Body.Close()
	var sess gateway.TaskSession
	if err := json.NewDecoder(sessResp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.Harness != "opencode" {
		t.Errorf("sess.Harness = %q, want opencode", sess.Harness)
	}
	if sess.ActiveModel != "gemini-3.7-flash" {
		t.Errorf("sess.ActiveModel = %q, want gemini-3.7-flash", sess.ActiveModel)
	}
}

func TestOpenAIChatCompletionsStreamingAndFixtureFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.AgentPlatformBaseURL = upstream.URL
	cfg.EnableFixtureFallback = true
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	reqBody := `{
		"model": "gemini-3.8-flash",
		"stream": true,
		"messages": [{"role": "user", "content": "Fix INC-104 in shipping-service with pi"}]
	}`
	req, _ := http.NewRequest(
		http.MethodPost,
		ts.URL+"/s/fixer-shipping-service-pi/v1/chat/completions",
		jsonReader(reqBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AX-Harness", "pi")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST streaming chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, "chat.completion.chunk") || !strings.Contains(text, "[DONE]") {
		t.Errorf("expected SSE stream with chat.completion.chunk and [DONE], got:\n%s", text)
	}

	sess, ok := srv.GetSession("fixer-shipping-service-pi")
	if !ok {
		t.Fatalf("expected session fixer-shipping-service-pi")
	}
	if sess.Harness != "pi" {
		t.Errorf("sess.Harness = %q, want pi", sess.Harness)
	}
	if sess.SourceProvenance != gateway.ProvenanceFixture {
		t.Errorf("sess.SourceProvenance = %q, want FIXTURE", sess.SourceProvenance)
	}
}

func TestOpenAIModelsListAndHeaderScopedRoutes(t *testing.T) {
	cfg := gateway.DefaultConfig()
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/s/fixer-checkout-service/v1/models")
	if err != nil {
		t.Fatalf("GET /s/fixer-checkout-service/v1/models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /v1/models, got %d", resp.StatusCode)
	}

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("Object = %q, want list", list.Object)
	}
	if len(list.Data) != 4 || list.Data[0].ID != "gemini-3.8-flash" || list.Data[3].ID != "gemini-3.5-flash" {
		t.Errorf("unexpected models list: %+v", list.Data)
	}
}

func TestMultiHarnessPackagingAndCLIWrappers(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	// 1. Verify Dockerfile.task-runner and config files
	dockerfileBytes, err := os.ReadFile(filepath.Join(repoRoot, "harnesses", "Dockerfile.task-runner"))
	if err != nil {
		t.Fatalf("read Dockerfile.task-runner: %v", err)
	}
	dockerfile := string(dockerfileBytes)
	for _, needle := range []string{
		"google-antigravity",
		"opencode-ai",
		"@mariozechner/pi-coding-agent",
		"GOCACHE",
		"CARGO_HOME",
		"/usr/local/bin/ax-task-runner",
		"ax-sandbox-token",
	} {
		if !strings.Contains(dockerfile, needle) {
			t.Errorf("Dockerfile.task-runner missing expected token %q", needle)
		}
	}

	opencodeCfgBytes, err := os.ReadFile(filepath.Join(repoRoot, "harnesses", "config", "opencode.json"))
	if err != nil {
		t.Fatalf("read harnesses/config/opencode.json: %v", err)
	}
	var opencodeCfg map[string]any
	if err := json.Unmarshal(opencodeCfgBytes, &opencodeCfg); err != nil {
		t.Fatalf("invalid JSON in opencode.json: %v", err)
	}

	piCfgBytes, err := os.ReadFile(filepath.Join(repoRoot, "harnesses", "config", "pi-models.json"))
	if err != nil {
		t.Fatalf("read harnesses/config/pi-models.json: %v", err)
	}
	var piCfg map[string]any
	if err := json.Unmarshal(piCfgBytes, &piCfg); err != nil {
		t.Fatalf("invalid JSON in pi-models.json: %v", err)
	}

	// 2. Verify all 3 CLI harness binaries execute against Model Gateway with zero secrets
	cfg := gateway.DefaultConfig()
	cfg.AgentPlatformBaseURL = "http://127.0.0.1:1"
	cfg.EnableFixtureFallback = true
	gw := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(gw.Handler())
	defer ts.Close()

	workDir := t.TempDir()
	for _, tc := range []struct {
		bin      string
		taskName string
		harness  string
	}{
		{bin: "antigravity", taskName: "fixer-pricing-antigravity", harness: "antigravity"},
		{bin: "opencode-harness", taskName: "fixer-inventory-opencode", harness: "opencode"},
		{bin: "pi-harness", taskName: "fixer-shipping-pi", harness: "pi"},
	} {
		scriptPath := filepath.Join(repoRoot, "harnesses", "bin", tc.bin)
		cmd := exec.Command(scriptPath, "-p", "Write regression test and fix for "+tc.taskName)
		cmd.Env = append(os.Environ(),
			"WORKSPACE_DIR="+workDir,
			"AX_TASK_NAME="+tc.taskName,
			"GATEWAY_BASE_URL="+ts.URL,
			"AGENT_MODEL=gemini-3.8-flash",
			"GEMINI_API_KEY="+gateway.SandboxTokenPlaceholder,
			"OPENAI_API_KEY="+gateway.SandboxTokenPlaceholder,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\noutput:\n%s", tc.bin, err, string(out))
		}
		if len(strings.TrimSpace(string(out))) == 0 {
			t.Errorf("expected non-empty output from %s", tc.bin)
		}

		sess, ok := gw.GetSession(tc.taskName)
		if !ok {
			t.Fatalf("expected gateway session %q to be recorded by %s", tc.taskName, tc.bin)
		}
		if sess.Harness != tc.harness {
			t.Errorf("%s recorded harness = %q, want %q", tc.bin, sess.Harness, tc.harness)
		}
	}
}

func TestGeminiCascadeFallsBackOn401And403AndRemoteSessionClientQueriesGateway(t *testing.T) {
	if !gateway.ShouldFallback(http.StatusUnauthorized, nil) {
		t.Fatalf("expected ShouldFallback(401, nil) to be true")
	}
	if !gateway.ShouldFallback(http.StatusForbidden, nil) {
		t.Fatalf("expected ShouldFallback(403, nil) to be true")
	}
	if gateway.ShouldFallback(http.StatusBadRequest, nil) {
		t.Fatalf("expected ShouldFallback(400, nil) to be false")
	}
	if gateway.ShouldFallback(http.StatusOK, nil) {
		t.Fatalf("expected ShouldFallback(200, nil) to be false")
	}

	var attemptedModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := strings.Index(r.URL.Path, "/models/")
		tail := r.URL.Path[idx+len("/models/"):]
		model := strings.Split(tail, ":")[0]
		attemptedModels = append(attemptedModels, model)

		switch model {
		case "gemini-3.8-flash":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"preview allowlist required"}}`))
		case "gemini-3.7-flash":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"transient token mismatch"}}`))
		case "gemini-3.6-flash":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {
						"role": "model",
						"parts": [{"text": "Patched checkout-service via gemini-3.6-flash."}]
					}
				}],
				"usageMetadata": {
					"promptTokenCount": 95,
					"candidatesTokenCount": 40,
					"totalTokenCount": 135
				}
			}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	cfg := gateway.DefaultConfig()
	cfg.ProjectID = "ax-demo-project"
	cfg.AgentPlatformBaseURL = upstream.URL
	srv := gateway.NewServer(cfg, gateway.StaticTokenFunc("wi-token"))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(
		ts.URL+"/s/fixer-auth-cascade/v1beta/models/gemini-3.8-flash:generateContent",
		"application/json",
		jsonReader(`{"contents":[{"role":"user","parts":[{"text":"Fix INC-102 checkout-service"}]}]}`),
	)
	if err != nil {
		t.Fatalf("POST generateContent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after 403/401 cascade fallback, got %d", resp.StatusCode)
	}

	wantOrder := []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash"}
	if !reflect.DeepEqual(attemptedModels, wantOrder) {
		t.Fatalf("attemptedModels = %v, want %v", attemptedModels, wantOrder)
	}
	if got := resp.Header.Get("X-AX-Active-Model"); got != "gemini-3.6-flash" {
		t.Errorf("X-AX-Active-Model = %q, want gemini-3.6-flash", got)
	}
	if got := resp.Header.Get("X-AX-Source-Provenance"); got != string(gateway.ProvenanceLive) {
		t.Errorf("X-AX-Source-Provenance = %q, want LIVE", got)
	}

	// Verify remote session query client against GET /api/gateway/sessions and /api/gateway/sessions/{task}
	client := gateway.NewClient(ts.URL, nil)
	ctx := context.Background()
	remoteCfg, err := client.GetConfig(ctx)
	if err != nil {
		t.Fatalf("client.GetConfig failed: %v", err)
	}
	if !reflect.DeepEqual(remoteCfg.FallbackCascade, gateway.DefaultFallbackCascade) {
		t.Errorf("remoteCfg.FallbackCascade = %v, want %v", remoteCfg.FallbackCascade, gateway.DefaultFallbackCascade)
	}

	snap, err := client.QuerySessions(ctx)
	if err != nil {
		t.Fatalf("client.QuerySessions failed: %v", err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].TaskName != "fixer-auth-cascade" {
		t.Fatalf("unexpected sessions snapshot: %+v", snap.Sessions)
	}
	if len(snap.Calls) != 1 || snap.Calls[0].ActiveModel != "gemini-3.6-flash" {
		t.Fatalf("unexpected calls snapshot: %+v", snap.Calls)
	}

	sessionsList, err := client.ListSessions(ctx)
	if err != nil || len(sessionsList) != 1 {
		t.Fatalf("client.ListSessions failed: err=%v sessions=%+v", err, sessionsList)
	}

	sess, found, err := client.GetSession(ctx, "fixer-auth-cascade")
	if err != nil {
		t.Fatalf("client.GetSession failed: %v", err)
	}
	if !found {
		t.Fatalf("expected session fixer-auth-cascade to be found")
	}
	if sess.ActiveModel != "gemini-3.6-flash" || sess.SourceProvenance != gateway.ProvenanceLive || sess.CascadeFallbacks != 2 {
		t.Errorf("unexpected session details: %+v", sess)
	}

	_, foundMissing, err := client.GetSession(ctx, "nonexistent-task")
	if err != nil {
		t.Fatalf("client.GetSession(nonexistent) returned error: %v", err)
	}
	if foundMissing {
		t.Errorf("expected nonexistent-task found=false")
	}
}
