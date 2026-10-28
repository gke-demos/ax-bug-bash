package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAIChatCompletionRequest represents an OpenAI-compatible /v1/chat/completions request from opencode or pi.
type OpenAIChatCompletionRequest struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Messages            []OpenAIMessage `json:"messages"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
}

// OpenAIMessage represents a message in an OpenAI chat conversation.
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAIToolCall represents an assistant tool invocation in OpenAI wire format.
type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

// OpenAIFunctionCall holds the function name and JSON-encoded arguments string.
type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAITool represents a tool definition in OpenAI wire format.
type OpenAITool struct {
	Type     string            `json:"type"`
	Function OpenAIFunctionDef `json:"function"`
}

// OpenAIFunctionDef describes a callable function schema.
type OpenAIFunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request, taskName string, bodyBytes []byte) {
	var oaiReq OpenAIChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &oaiReq); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("decode OpenAI request: %v", err))
		return
	}

	requestedModel := strings.TrimSpace(oaiReq.Model)
	if requestedModel == "" && len(s.cfg.FallbackCascade) > 0 {
		requestedModel = s.cfg.FallbackCascade[0]
	}
	harness := detectHarness(r, taskName, "opencode")

	geminiReqBytes, err := OpenAIToGeminiRequest(oaiReq)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "translation_error", err.Error())
		return
	}

	start := time.Now()
	result, err := s.ExecuteGeminiCascade(r.Context(), taskName, requestedModel, "generateContent", "", geminiReqBytes)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		status := result.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.recordCall(CallRecord{
			TaskName:         taskName,
			Harness:          harness,
			Endpoint:         "openai",
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

	outBytes, contentType, err := GeminiToOpenAIResponse(taskName, result, oaiReq.Stream)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "response_translation_error", err.Error())
		return
	}

	s.recordCall(CallRecord{
		TaskName:         taskName,
		Harness:          harness,
		Endpoint:         "openai",
		RequestedModel:   requestedModel,
		ActiveModel:      result.ActiveModel,
		CascadeAttempts:  result.Attempts,
		SourceProvenance: result.SourceProvenance,
		InputTokens:      result.InputTokens,
		OutputTokens:     result.OutputTokens,
		TotalTokens:      result.InputTokens + result.OutputTokens,
		DurationMs:       dur,
		StatusCode:       http.StatusOK,
	})

	setGatewayHeaders(w, taskName, result)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(outBytes)
}

// OpenAIToGeminiRequest translates an OpenAI /v1/chat/completions payload into a Gemini generateContent payload,
// including system instructions, multi-turn messages, tool calls, tool responses, and function declarations.
func OpenAIToGeminiRequest(req OpenAIChatCompletionRequest) ([]byte, error) {
	payload := make(map[string]any)

	var sysTexts []string
	var contents []map[string]any
	callIDToName := make(map[string]string)

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		text := extractOpenAIMessageText(msg.Content)

		switch role {
		case "system", "developer":
			if text != "" {
				sysTexts = append(sysTexts, text)
			}

		case "user":
			parts := []map[string]any{
				{"text": text},
			}
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": parts,
			})

		case "assistant":
			var parts []map[string]any
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" && tc.Function.Name != "" {
					callIDToName[tc.ID] = tc.Function.Name
				}
				var argsMap map[string]any
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
				}
				if argsMap == nil {
					argsMap = map[string]any{}
				}
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": tc.Function.Name,
						"args": argsMap,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{
					"role":  "model",
					"parts": parts,
				})
			}

		case "tool":
			fnName := msg.Name
			if fnName == "" && msg.ToolCallID != "" {
				fnName = callIDToName[msg.ToolCallID]
			}
			if fnName == "" {
				fnName = "tool_result"
			}
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []map[string]any{
					{
						"functionResponse": map[string]any{
							"name": fnName,
							"response": map[string]any{
								"result":       text,
								"tool_call_id": msg.ToolCallID,
							},
						},
					},
				},
			})
		}
	}

	if len(sysTexts) > 0 {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{
				{"text": strings.Join(sysTexts, "\n\n")},
			},
		}
	}
	if len(contents) == 0 {
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": []map[string]any{{"text": ""}},
		})
	}
	payload["contents"] = contents

	if len(req.Tools) > 0 {
		var decls []map[string]any
		for _, t := range req.Tools {
			if t.Function.Name == "" {
				continue
			}
			decl := map[string]any{
				"name": t.Function.Name,
			}
			if t.Function.Description != "" {
				decl["description"] = t.Function.Description
			}
			if t.Function.Parameters != nil {
				decl["parameters"] = t.Function.Parameters
			}
			decls = append(decls, decl)
		}
		if len(decls) > 0 {
			payload["tools"] = []map[string]any{
				{"functionDeclarations": decls},
			}
		}
	}

	genCfg := make(map[string]any)
	if req.Temperature != nil {
		genCfg["temperature"] = *req.Temperature
	}
	maxTok := req.MaxCompletionTokens
	if maxTok <= 0 {
		maxTok = req.MaxTokens
	}
	if maxTok > 0 {
		genCfg["maxOutputTokens"] = maxTok
	}
	if len(genCfg) > 0 {
		payload["generationConfig"] = genCfg
	}

	return json.Marshal(payload)
}

// GeminiToOpenAIResponse translates a Gemini generateContent response into an OpenAI chat.completion JSON or SSE stream.
func GeminiToOpenAIResponse(taskName string, result CascadeResult, isStream bool) ([]byte, string, error) {
	var geminiPayload struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text,omitempty"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	_ = json.Unmarshal(result.Body, &geminiPayload)

	var textParts []string
	var toolCalls []OpenAIToolCall

	if len(geminiPayload.Candidates) > 0 {
		for idx, p := range geminiPayload.Candidates[0].Content.Parts {
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
			if p.FunctionCall != nil && p.FunctionCall.Name != "" {
				argsJSON, _ := json.Marshal(p.FunctionCall.Args)
				if len(argsJSON) == 0 {
					argsJSON = []byte("{}")
				}
				toolCalls = append(toolCalls, OpenAIToolCall{
					ID:   fmt.Sprintf("call_%s_%d", taskName, idx+1),
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      p.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}
	}

	combinedText := strings.Join(textParts, "\n")
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	created := time.Now().Unix()
	completionID := fmt.Sprintf("chatcmpl-%s-%d", taskName, created)

	if isStream {
		delta := map[string]any{
			"role":    "assistant",
			"content": combinedText,
		}
		if len(toolCalls) > 0 {
			delta["tool_calls"] = toolCalls
		}
		chunk := map[string]any{
			"id":                completionID,
			"object":            "chat.completion.chunk",
			"created":           created,
			"model":             result.ActiveModel,
			"source_provenance": result.SourceProvenance,
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     result.InputTokens,
				"completion_tokens": result.OutputTokens,
				"total_tokens":      result.InputTokens + result.OutputTokens,
			},
		}
		chunkBytes, err := json.Marshal(chunk)
		if err != nil {
			return nil, "", err
		}
		sse := fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", string(chunkBytes))
		return []byte(sse), "text/event-stream", nil
	}

	msgObj := map[string]any{
		"role":    "assistant",
		"content": combinedText,
	}
	if len(toolCalls) > 0 {
		msgObj["tool_calls"] = toolCalls
	}

	respObj := map[string]any{
		"id":                completionID,
		"object":            "chat.completion",
		"created":           created,
		"model":             result.ActiveModel,
		"source_provenance": result.SourceProvenance,
		"cascade_attempts":  result.Attempts,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msgObj,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     result.InputTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      result.InputTokens + result.OutputTokens,
		},
	}

	out, err := json.Marshal(respObj)
	if err != nil {
		return nil, "", err
	}
	return out, "application/json", nil
}

func extractOpenAIMessageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
