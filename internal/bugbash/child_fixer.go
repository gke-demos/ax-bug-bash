package bugbash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

const (
	PhaseReproduceRed = "REPRODUCE_RED"
	PhasePatchGreen   = "PATCH_GREEN"
	PhaseExtractDiff  = "EXTRACT_DIFF"
	PhaseAutoSuspend  = "AUTO_SUSPEND"

	AXStateDirName = ".ax-state"
)

// TDDStepEvent represents a single observable phase in a Child Task's Red-Green TDD lifecycle.
type TDDStepEvent struct {
	Phase      string    `json:"phase"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary"`
	Output     string    `json:"output,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// ChildTaskResult captures the complete TDD remediation output and volume-persisted state of a Child Task.
type ChildTaskResult struct {
	TaskName           string         `json:"task_name"`
	ParentTask         string         `json:"parent_task"`
	IncidentID         string         `json:"incident_id"`
	Service            string         `json:"service"`
	Language           string         `json:"language"`
	Harness            string         `json:"harness"`
	RequestedModel     string         `json:"requested_model"`
	ActiveModel        string         `json:"active_model"`
	SourceProvenance   string         `json:"source_provenance"`
	RegressionTestName string         `json:"regression_test_name"`
	RedTestOutput      string         `json:"red_test_output"`
	GreenTestOutput    string         `json:"green_test_output"`
	RegressionTestDiff string         `json:"regression_test_diff"`
	CodeFixDiff        string         `json:"code_fix_diff"`
	UnifiedGitDiff     string         `json:"unified_git_diff"`
	TDDSteps           []TDDStepEvent `json:"tdd_steps"`
	ExitCode           int            `json:"exit_code"`
	Suspended          bool           `json:"suspended"`
	WorkerID           string         `json:"worker_id,omitempty"`
	CompletedAt        time.Time      `json:"completed_at,omitempty"`
	SuspendedAt        time.Time      `json:"suspended_at,omitempty"`
}

// ChildFixerConfig configures a Child Task's Red-Green TDD execution.
type ChildFixerConfig struct {
	Mission         ChildTaskMission
	WorkspaceDir    string
	ModelGatewayURL string
	OnStep          func(step TDDStepEvent)
}

// SaveTaskStateSnapshot persists the ChildTaskResult under <workspaceDir>/.ax-state/<taskName>.json
// so SNAPSHOT_FIDELITY_VOLUMES preserves state across suspend/resume (ADR-0006 Issue #518).
func SaveTaskStateSnapshot(workspaceDir string, res ChildTaskResult) error {
	if strings.TrimSpace(workspaceDir) == "" {
		return fmt.Errorf("workspaceDir is required")
	}
	taskName := SanitizeTaskName(res.TaskName)
	stateDir := filepath.Join(workspaceDir, AXStateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating .ax-state directory: %w", err)
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling task state snapshot: %w", err)
	}
	dst := filepath.Join(stateDir, taskName+".json")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing task state snapshot %s: %w", dst, err)
	}
	return nil
}

// LoadTaskStateSnapshot reads <workspaceDir>/.ax-state/<taskName>.json.
func LoadTaskStateSnapshot(workspaceDir, taskName string) (ChildTaskResult, error) {
	src := filepath.Join(workspaceDir, AXStateDirName, SanitizeTaskName(taskName)+".json")
	data, err := os.ReadFile(src)
	if err != nil {
		return ChildTaskResult{}, fmt.Errorf("reading task state snapshot %s: %w", src, err)
	}
	var out ChildTaskResult
	if err := json.Unmarshal(data, &out); err != nil {
		return ChildTaskResult{}, fmt.Errorf("unmarshaling task state snapshot %s: %w", src, err)
	}
	return out, nil
}

// ExecuteChildFixerTDD runs the 3 in-process TDD phases (REPRODUCE_RED -> PATCH_GREEN -> EXTRACT_DIFF)
// for a single implicated microservice and persists state to /workspace/.ax-state/<task-name>.json.
func ExecuteChildFixerTDD(ctx context.Context, cfg ChildFixerConfig) (ChildTaskResult, error) {
	mission := cfg.Mission
	if mission.TaskName == "" {
		mission.TaskName = SanitizeTaskName(fmt.Sprintf("fixer-%s-%s", strings.ToLower(mission.IncidentID), mission.Service))
	}
	if mission.Harness == "" {
		mission.Harness = "antigravity"
	}
	if mission.RequestedModel == "" {
		mission.RequestedModel = gateway.DefaultFallbackCascade[0]
	}

	sc, ok := storefront.GetScenario(mission.IncidentID)
	if !ok {
		return ChildTaskResult{}, fmt.Errorf("unknown incident scenario %q", mission.IncidentID)
	}
	detail, ok := sc.ServiceTraces[mission.Service]
	if !ok {
		return ChildTaskResult{}, fmt.Errorf("service %q is not implicated in incident %q", mission.Service, mission.IncidentID)
	}

	if mission.Language == "" {
		mission.Language = detail.Language
	}
	if mission.SourceFile == "" {
		mission.SourceFile = detail.SourceFile
	}
	if mission.TestFile == "" {
		mission.TestFile = detail.TestFile
	}
	if mission.RegressionTestName == "" {
		mission.RegressionTestName = detail.RegressionTestName
	}

	res := ChildTaskResult{
		TaskName:           mission.TaskName,
		ParentTask:         mission.ParentTask,
		IncidentID:         mission.IncidentID,
		Service:            mission.Service,
		Language:           mission.Language,
		Harness:            mission.Harness,
		RequestedModel:     mission.RequestedModel,
		ActiveModel:        mission.RequestedModel,
		SourceProvenance:   string(gateway.ProvenanceFixture),
		RegressionTestName: mission.RegressionTestName,
	}

	emitStep := func(step TDDStepEvent) {
		res.TDDSteps = append(res.TDDSteps, step)
		_ = SaveTaskStateSnapshot(cfg.WorkspaceDir, res)
		if cfg.OnStep != nil {
			cfg.OnStep(step)
		}
	}

	// Phase 1: REPRODUCE_RED — invoke Coding Harness through Model Gateway, write new Regression Test, verify it fails (Red).
	phase1Start := time.Now()
	relTestPath := filepath.ToSlash(filepath.Join("services", mission.Service, detail.TestFile))
	redPrompt := fmt.Sprintf(
		"PHASE=REPRODUCE_RED INCIDENT_ID=%s SERVICE=%s TARGET_FILE=%s\n[%s/%s] Author failing regression test %s in %s reproducing HTTP 500 %s:\n%s\nEmit the updated file inside <file path=%q>...</file>.",
		mission.IncidentID,
		mission.Service,
		relTestPath,
		mission.IncidentID,
		mission.Service,
		detail.RegressionTestName,
		detail.TestFile,
		detail.ErrorMessage,
		detail.StackTrace,
		relTestPath,
	)
	wroteRedFromGateway := false
	if cfg.ModelGatewayURL != "" {
		replyText, activeModel, prov, err := InvokeCodingHarnessPrompt(ctx, cfg.ModelGatewayURL, mission.TaskName, mission.Harness, mission.RequestedModel, redPrompt)
		if err == nil {
			if activeModel != "" {
				res.ActiveModel = activeModel
			}
			if prov != "" {
				res.SourceProvenance = prov
			}
			if written, wErr := extractAndWriteFileBlocks(cfg.WorkspaceDir, replyText); wErr == nil && written[relTestPath] {
				wroteRedFromGateway = true
			}
		}
	}

	if !wroteRedFromGateway {
		testFilePath := filepath.Join(cfg.WorkspaceDir, "services", mission.Service, detail.TestFile)
		if err := os.WriteFile(testFilePath, []byte(detail.RegressionTestFileContent), 0o644); err != nil {
			return res, fmt.Errorf("writing regression test %s: %w", testFilePath, err)
		}
	}

	redOut, redErr := runServiceTestSuite(ctx, cfg.WorkspaceDir, mission.Service)
	res.RedTestOutput = redOut
	if redErr == nil {
		return res, fmt.Errorf("expected regression test %s to fail (Red) before patching %s, but test suite passed:\n%s", detail.RegressionTestName, mission.Service, redOut)
	}
	res.RegressionTestDiff = CaptureWorkspaceGitDiff(cfg.WorkspaceDir, filepath.Join("services", mission.Service, detail.TestFile))

	emitStep(TDDStepEvent{
		Phase:      PhaseReproduceRed,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("[%s] Authored %s in %s and verified expected Red failure", mission.Harness, detail.RegressionTestName, detail.TestFile),
		Output:     redOut,
		DurationMs: time.Since(phase1Start).Milliseconds(),
		Timestamp:  time.Now().UTC(),
	})

	// Phase 2: PATCH_GREEN — invoke Coding Harness through Model Gateway, patch service source code, verify all tests pass (Green).
	phase2Start := time.Now()
	relSrcPath := filepath.ToSlash(filepath.Join("services", mission.Service, detail.SourceFile))
	greenPrompt := fmt.Sprintf(
		"PHASE=PATCH_GREEN INCIDENT_ID=%s SERVICE=%s TARGET_FILE=%s\n[%s/%s] Patch %s so regression test %s and all pre-existing unit tests pass.\nEmit the patched file inside <file path=%q>...</file>.",
		mission.IncidentID,
		mission.Service,
		relSrcPath,
		mission.IncidentID,
		mission.Service,
		detail.SourceFile,
		detail.RegressionTestName,
		relSrcPath,
	)
	wroteGreenFromGateway := false
	if cfg.ModelGatewayURL != "" {
		replyText, activeModel, prov, err := InvokeCodingHarnessPrompt(ctx, cfg.ModelGatewayURL, mission.TaskName, mission.Harness, mission.RequestedModel, greenPrompt)
		if err == nil {
			if activeModel != "" {
				res.ActiveModel = activeModel
			}
			if prov != "" {
				res.SourceProvenance = prov
			}
			if written, wErr := extractAndWriteFileBlocks(cfg.WorkspaceDir, replyText); wErr == nil && written[relSrcPath] {
				wroteGreenFromGateway = true
			}
		}
	}

	if !wroteGreenFromGateway {
		srcFilePath := filepath.Join(cfg.WorkspaceDir, "services", mission.Service, detail.SourceFile)
		if err := os.WriteFile(srcFilePath, []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
			return res, fmt.Errorf("writing patched source %s: %w", srcFilePath, err)
		}
	}

	greenOut, greenErr := runServiceTestSuite(ctx, cfg.WorkspaceDir, mission.Service)
	res.GreenTestOutput = greenOut
	if greenErr != nil {
		return res, fmt.Errorf("expected patched service %s tests to pass (Green), got error %v:\n%s", mission.Service, greenErr, greenOut)
	}

	emitStep(TDDStepEvent{
		Phase:      PhasePatchGreen,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("[%s] Patched %s; %s and all existing tests passed (Green)", mission.Harness, detail.SourceFile, detail.RegressionTestName),
		Output:     greenOut,
		DurationMs: time.Since(phase2Start).Milliseconds(),
		Timestamp:  time.Now().UTC(),
	})

	// Phase 3: EXTRACT_DIFF — extract unified git diff and persist /workspace/.ax-state/<task-name>.json.
	phase3Start := time.Now()
	res.CodeFixDiff = CaptureWorkspaceGitDiff(cfg.WorkspaceDir, filepath.Join("services", mission.Service, detail.SourceFile))
	res.UnifiedGitDiff = CaptureWorkspaceGitDiff(cfg.WorkspaceDir, filepath.Join("services", mission.Service))
	if res.UnifiedGitDiff == "" {
		res.UnifiedGitDiff = CaptureWorkspaceGitDiff(cfg.WorkspaceDir, "")
	}
	res.CompletedAt = time.Now().UTC()

	emitStep(TDDStepEvent{
		Phase:      PhaseExtractDiff,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("Captured unified git diff (%d bytes) and saved /workspace/.ax-state/%s.json", len(res.UnifiedGitDiff), res.TaskName),
		Output:     res.UnifiedGitDiff,
		DurationMs: time.Since(phase3Start).Milliseconds(),
		Timestamp:  res.CompletedAt,
	})

	if err := SaveTaskStateSnapshot(cfg.WorkspaceDir, res); err != nil {
		return res, err
	}
	return res, nil
}

// InvokeCodingHarnessPrompt sends a prompt through the zero-secret Model Gateway using the wire protocol
// corresponding to the selected Coding Harness ("antigravity" -> Gemini REST, "opencode"/"pi" -> OpenAI /v1/chat/completions).
func InvokeCodingHarnessPrompt(
	ctx context.Context,
	gatewayURL string,
	taskName string,
	harness string,
	requestedModel string,
	prompt string,
) (replyText string, activeModel string, provenance string, err error) {
	base := strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
	if base == "" {
		return "", requestedModel, string(gateway.ProvenanceFixture), nil
	}
	if requestedModel == "" {
		requestedModel = gateway.DefaultFallbackCascade[0]
	}
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness == "" {
		harness = "antigravity"
	}

	client := &http.Client{Timeout: 20 * time.Second}

	if harness == "antigravity" {
		endpoint := fmt.Sprintf("%s/s/%s/v1beta/models/%s:generateContent", base, taskName, requestedModel)
		reqBody, _ := json.Marshal(map[string]any{
			"contents": []map[string]any{
				{
					"role": "user",
					"parts": []map[string]any{
						{"text": prompt},
					},
				},
			},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return "", "", "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+gateway.SandboxTokenPlaceholder)
		req.Header.Set("X-AX-Task-Name", taskName)
		req.Header.Set("X-AX-Harness", harness)

		resp, err := client.Do(req)
		if err != nil {
			return "", "", "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			return "", "", "", fmt.Errorf("model gateway status %d: %s", resp.StatusCode, string(body))
		}
		return extractGeminiResponseText(body), resp.Header.Get("X-AX-Active-Model"), resp.Header.Get("X-AX-Source-Provenance"), nil
	}

	// OpenCode ("opencode") and Pi ("pi") use the OpenAI-compatible /v1/chat/completions endpoint
	endpoint := fmt.Sprintf("%s/s/%s/v1/chat/completions", base, taskName)
	reqBody, _ := json.Marshal(gateway.OpenAIChatCompletionRequest{
		Model: requestedModel,
		Messages: []gateway.OpenAIMessage{
			{Role: "system", Content: "You are an autonomous SRE coding agent running inside an Agent Substrate v0.6.0 sandbox."},
			{Role: "user", Content: prompt},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gateway.SandboxTokenPlaceholder)
	req.Header.Set("X-AX-Task-Name", taskName)
	req.Header.Set("X-AX-Harness", harness)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", "", "", fmt.Errorf("model gateway status %d: %s", resp.StatusCode, string(body))
	}
	return extractOpenAIResponseText(body), resp.Header.Get("X-AX-Active-Model"), resp.Header.Get("X-AX-Source-Provenance"), nil
}

func extractGeminiResponseText(body []byte) string {
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Candidates) > 0 {
		var sb strings.Builder
		for _, p := range parsed.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return string(body)
}

func extractOpenAIResponseText(body []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Choices) > 0 {
		if parsed.Choices[0].Message.Content != "" {
			return parsed.Choices[0].Message.Content
		}
	}
	return string(body)
}

// extractAndWriteFileBlocks parses <file path="services/...">...</file> blocks from a model response
// and writes them safely inside workDir, returning the set of relative slash paths written.
func extractAndWriteFileBlocks(workDir, responseText string) (map[string]bool, error) {
	written := make(map[string]bool)
	absRoot, err := filepath.Abs(workDir)
	if err != nil {
		return written, err
	}
	remaining := responseText
	for {
		openIdx := strings.Index(remaining, `<file path="`)
		if openIdx < 0 {
			break
		}
		afterOpen := remaining[openIdx+len(`<file path="`):]
		quoteEnd := strings.Index(afterOpen, `">`)
		if quoteEnd < 0 {
			break
		}
		relPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(afterOpen[:quoteEnd])))
		bodyStart := afterOpen[quoteEnd+2:]
		closeIdx := strings.Index(bodyStart, "</file>")
		if closeIdx < 0 {
			break
		}
		content := strings.TrimPrefix(bodyStart[:closeIdx], "\n")
		remaining = bodyStart[closeIdx+len("</file>"):]

		if strings.HasPrefix(relPath, "../") || strings.HasPrefix(relPath, "/") || relPath == "." || relPath == "" {
			continue
		}
		dst := filepath.Join(absRoot, filepath.FromSlash(relPath))
		if !strings.HasPrefix(dst, absRoot+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return written, err
		}
		written[relPath] = true
	}
	return written, nil
}

// CaptureWorkspaceGitDiff runs `git diff` inside workspaceDir (optionally scoped to relPath).
func CaptureWorkspaceGitDiff(workspaceDir, relPath string) string {
	args := []string{"diff"}
	if strings.TrimSpace(relPath) != "" {
		args = append(args, "--", relPath)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = workspaceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func runServiceTestSuite(ctx context.Context, workspaceDir, service string) (string, error) {
	scriptPath := filepath.Join(workspaceDir, "services", "run-service-tests.sh")
	cmd := exec.CommandContext(ctx, "bash", scriptPath, service)
	cmd.Dir = workspaceDir
	cmd.Env = httputil.EnsureGoBinOnPath(os.Environ())
	out, err := cmd.CombinedOutput()
	return string(out), err
}
