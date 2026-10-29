package bugbash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TODO(security): Sandbox task workers are headless ephemeral CLI processes inside unprivileged gVisor
// sandboxes rather than browser-facing account apps; end-user password hashing, cookie/CSRF sessions,
// multipart file uploads, and SQL queries are intentionally absent here. Sandboxes authenticate to
// cluster-internal Model Gateway via placeholder tokens replaced upstream by GKE Workload Identity.

var validTaskSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SandboxChildOptions configures in-sandbox Child Task Red-Green TDD execution.
type SandboxChildOptions struct {
	ExecutionID              string
	RootTaskName             string
	Mission                  ChildTaskMission
	WorkspaceDir             string
	StorefrontGitURL         string
	ModelGatewayURL          string
	ObserverCallbackURL      string
	HarnessBinDir            string
	MaxAttempts              int
	OnStep                   func(step TDDStepEvent)
	OnEphemeralConfigCreated func(cfgDir string)
}

// ExecuteSandboxChildTask runs the Phase 2 in-sandbox Child Task lifecycle:
//  1. Idempotently short-circuits if /workspace/.ax-state/<task-name>.json already records completion.
//  2. Ensures /workspace is checked out from storefront.git on main.
//  3. Generates per-task harness CLI configs in an ephemeral temp directory outside /workspace.
//  4. Runs up to 2 live CLI attempts (antigravity, opencode run, pi --print --no-session) for REPRODUCE_RED
//     and PATCH_GREEN before deterministic fixture fallback.
//  5. Reports SourceProvenance="LIVE" iff both phases succeeded via a live model without fixture fallback.
//  6. Stages strictly services/<service> (excluding .ax-state/.ax), pushes HEAD:refs/heads/fix/<task-name>,
//     persists /workspace/.ax-state/<task-name>.json, and streams child.step + child.completed callbacks.
func ExecuteSandboxChildTask(ctx context.Context, opts SandboxChildOptions) (ChildTaskResult, error) {
	mission := opts.Mission
	if strings.TrimSpace(mission.TaskName) == "" {
		mission.TaskName = SanitizeTaskName(fmt.Sprintf("fixer-%s-%s", strings.ToLower(mission.IncidentID), mission.Service))
	} else {
		mission.TaskName = SanitizeTaskName(mission.TaskName)
	}
	if !validTaskSlugRe.MatchString(mission.TaskName) {
		return ChildTaskResult{}, fmt.Errorf("invalid child task name %q", mission.TaskName)
	}

	if !isAllowedStorefrontService(mission.Service) {
		return ChildTaskResult{}, fmt.Errorf("disallowed storefront service %q", mission.Service)
	}

	harness := strings.ToLower(strings.TrimSpace(mission.Harness))
	if harness == "" {
		harness = "antigravity"
	}
	switch harness {
	case "antigravity", "opencode", "pi":
	default:
		return ChildTaskResult{}, fmt.Errorf("unsupported coding harness %q", mission.Harness)
	}
	mission.Harness = harness

	if strings.TrimSpace(mission.RequestedModel) == "" {
		mission.RequestedModel = gateway.DefaultFallbackCascade[0]
	}

	wsDir := strings.TrimSpace(opts.WorkspaceDir)
	if wsDir == "" {
		wsDir = "/workspace"
	}
	absWS, err := filepath.Abs(wsDir)
	if err != nil {
		return ChildTaskResult{}, fmt.Errorf("resolving workspace path: %w", err)
	}
	wsDir = absWS

	// 1. Idempotent resume short-circuit when durable completion marker already exists.
	if existing, loadErr := LoadTaskStateSnapshot(wsDir, mission.TaskName); loadErr == nil {
		if !existing.CompletedAt.IsZero() && existing.ExitCode == 0 {
			return existing, nil
		}
	}

	sc, ok := storefront.GetScenario(strings.ToUpper(strings.TrimSpace(mission.IncidentID)))
	if !ok {
		return ChildTaskResult{}, fmt.Errorf("unknown incident scenario %q", mission.IncidentID)
	}
	detail, ok := sc.ServiceTraces[mission.Service]
	if !ok {
		return ChildTaskResult{}, fmt.Errorf("service %q is not implicated in incident %q", mission.Service, sc.ID)
	}
	mission.IncidentID = sc.ID
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
	if mission.ErrorMessage == "" {
		mission.ErrorMessage = detail.ErrorMessage
	}
	if mission.StackTrace == "" {
		mission.StackTrace = detail.StackTrace
	}

	execID := firstNonEmptyString(opts.ExecutionID, os.Getenv("AX_EXECUTION_ID"))
	rootTaskName := firstNonEmptyString(opts.RootTaskName, mission.ParentTask, os.Getenv("AX_ROOT_TASK_NAME"), os.Getenv("AX_PARENT_TASK"))
	mission.ParentTask = rootTaskName
	gitURL := firstNonEmptyString(opts.StorefrontGitURL, os.Getenv("STOREFRONT_GIT_URL"))
	gwBase := strings.TrimRight(firstNonEmptyString(opts.ModelGatewayURL, os.Getenv("MODEL_GATEWAY_URL")), "/")
	cbURL := strings.TrimSpace(firstNonEmptyString(opts.ObserverCallbackURL, os.Getenv("OBSERVER_CALLBACK_URL")))

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}

	// 2. Ensure Git workspace is initialized on main.
	if err := ensureGitWorkspaceCheckedOutMain(ctx, wsDir, gitURL); err != nil {
		return ChildTaskResult{}, fmt.Errorf("preparing child git workspace: %w", err)
	}
	_ = os.MkdirAll(filepath.Join(wsDir, AXStateDirName), 0o755)
	_ = os.MkdirAll(filepath.Join(wsDir, ".ax"), 0o755)

	// 3. Create ephemeral harness config directory strictly outside /workspace.
	cfgDir, err := createEphemeralHarnessConfigDir(wsDir, mission.TaskName, mission.Harness, mission.RequestedModel, gwBase)
	if err != nil {
		return ChildTaskResult{}, err
	}
	defer os.RemoveAll(cfgDir)
	if opts.OnEphemeralConfigCreated != nil {
		opts.OnEphemeralConfigCreated(cfgDir)
	}

	branchRef := "refs/heads/fix/" + mission.TaskName
	res := ChildTaskResult{
		TaskName:           mission.TaskName,
		ParentTask:         rootTaskName,
		IncidentID:         mission.IncidentID,
		Service:            mission.Service,
		Language:           mission.Language,
		Harness:            mission.Harness,
		RequestedModel:     mission.RequestedModel,
		ActiveModel:        mission.RequestedModel,
		SourceProvenance:   string(gateway.ProvenanceFixture),
		RegressionTestName: mission.RegressionTestName,
		BranchName:         branchRef,
	}

	emitStep := func(step TDDStepEvent) {
		res.TDDSteps = append(res.TDDSteps, step)
		if opts.OnStep != nil {
			opts.OnStep(step)
		}
		if cbURL != "" {
			stepCopy := step
			_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
				ExecutionID:    execID,
				RootTaskName:   rootTaskName,
				TaskName:       mission.TaskName,
				Service:        mission.Service,
				EventType:      ObserverEventChildStep,
				IncidentID:     mission.IncidentID,
				HarnessMode:    mission.Harness,
				RequestedModel: mission.RequestedModel,
				Step:           &stepCopy,
				Timestamp:      step.Timestamp,
			})
		}
	}

	relTestPath := filepath.ToSlash(filepath.Join("services", mission.Service, detail.TestFile))
	relSrcPath := filepath.ToSlash(filepath.Join("services", mission.Service, detail.SourceFile))
	absTestPath := filepath.Join(wsDir, "services", mission.Service, detail.TestFile)
	absSrcPath := filepath.Join(wsDir, "services", mission.Service, detail.SourceFile)

	baselineSrcBytes, _ := os.ReadFile(absSrcPath)
	baselineTestBytes, _ := os.ReadFile(absTestPath)

	var (
		redLiveSucceeded         bool
		greenLiveSucceeded       bool
		usedLocalFixtureFallback bool
		redAttemptsUsed          int
		greenAttemptsUsed        int
		redOut                   string
		greenOut                 string
	)

	// 4. Phase 1: REPRODUCE_RED (up to maxAttempts live CLI attempts)
	phase1Start := time.Now()
	if gwBase != "" {
		var prevDiag string
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			redAttemptsUsed = attempt
			_ = resetServiceToBaselineBytes(ctx, wsDir, mission.Service, absSrcPath, baselineSrcBytes, absTestPath, baselineTestBytes)

			redPrompt := buildSandboxRedPrompt(mission, detail, relTestPath, attempt, prevDiag)
			cliOut, cliErr := invokeSandboxHarnessCLI(ctx, wsDir, cfgDir, opts.HarnessBinDir, mission, gwBase, redPrompt)
			if cliOut != "" {
				_, _ = extractAndWriteServiceFileBlocks(wsDir, mission.Service, cliOut)
			}
			// Ensure baseline production source was NOT patched prematurely during RED phase!
			if len(baselineSrcBytes) > 0 {
				_ = os.WriteFile(absSrcPath, baselineSrcBytes, 0o644)
			}
			cleanStrayUntrackedOutsideService(wsDir, mission.Service)

			currentTestBytes, _ := os.ReadFile(absTestPath)
			testChanged := string(currentTestBytes) != string(baselineTestBytes)
			out, tErr := runServiceTestSuite(ctx, wsDir, mission.Service)
			redOut = out
			if cliErr == nil && testChanged && tErr != nil {
				redLiveSucceeded = true
				break
			}
			prevDiag = fmt.Sprintf("Attempt %d did not produce a failing regression test in %s (cliErr=%v testChanged=%v testSuiteErr=%v):\n%s",
				attempt, relTestPath, cliErr, testChanged, tErr, truncateOutput(out, 1200))
		}
	}

	if !redLiveSucceeded {
		usedLocalFixtureFallback = true
		_ = resetServiceToBaselineBytes(ctx, wsDir, mission.Service, absSrcPath, baselineSrcBytes, absTestPath, baselineTestBytes)
		if err := os.WriteFile(absTestPath, []byte(detail.RegressionTestFileContent), 0o644); err != nil {
			return res, fmt.Errorf("writing fallback regression test %s: %w", absTestPath, err)
		}
		out, tErr := runServiceTestSuite(ctx, wsDir, mission.Service)
		redOut = out
		if tErr == nil {
			return res, fmt.Errorf("expected regression test %s to fail (Red) on baseline %s, but suite passed:\n%s",
				detail.RegressionTestName, mission.Service, out)
		}
	}

	verifiedRedTestBytes, err := os.ReadFile(absTestPath)
	if err != nil {
		return res, fmt.Errorf("reading verified red test file %s: %w", absTestPath, err)
	}
	res.RedTestOutput = redOut
	res.RegressionTestDiff = CaptureWorkspaceGitDiff(wsDir, filepath.Join("services", mission.Service, detail.TestFile))

	emitStep(TDDStepEvent{
		Phase:      PhaseReproduceRed,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("[%s] Authored %s in %s and verified expected Red failure", mission.Harness, detail.RegressionTestName, detail.TestFile),
		Output:     redOut,
		DurationMs: time.Since(phase1Start).Milliseconds(),
		Timestamp:  time.Now().UTC(),
	})

	// 5. Phase 2: PATCH_GREEN (up to maxAttempts live CLI attempts)
	phase2Start := time.Now()
	if gwBase != "" {
		var prevDiag string
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			greenAttemptsUsed = attempt
			_ = resetServiceToBaselineBytes(ctx, wsDir, mission.Service, absSrcPath, baselineSrcBytes, absTestPath, verifiedRedTestBytes)

			greenPrompt := buildSandboxGreenPrompt(mission, detail, relSrcPath, relTestPath, attempt, prevDiag)
			cliOut, cliErr := invokeSandboxHarnessCLI(ctx, wsDir, cfgDir, opts.HarnessBinDir, mission, gwBase, greenPrompt)
			if cliOut != "" {
				_, _ = extractAndWriteServiceFileBlocks(wsDir, mission.Service, cliOut)
			}
			// Guardrail: preserve the verified Red regression test if the harness overwrote or deleted it.
			if curTest, rErr := os.ReadFile(absTestPath); rErr != nil || !strings.Contains(string(curTest), detail.RegressionTestName) {
				_ = os.WriteFile(absTestPath, verifiedRedTestBytes, 0o644)
			}
			cleanStrayUntrackedOutsideService(wsDir, mission.Service)

			out, tErr := runServiceTestSuite(ctx, wsDir, mission.Service)
			greenOut = out
			if cliErr == nil && tErr == nil {
				greenLiveSucceeded = true
				break
			}
			prevDiag = fmt.Sprintf("Attempt %d patch did not make test suite green (cliErr=%v testErr=%v):\n%s",
				attempt, cliErr, tErr, truncateOutput(out, 1200))
		}
	}

	if !greenLiveSucceeded {
		usedLocalFixtureFallback = true
		_ = resetServiceToBaselineBytes(ctx, wsDir, mission.Service, absSrcPath, baselineSrcBytes, absTestPath, verifiedRedTestBytes)
		if err := os.WriteFile(absSrcPath, []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
			return res, fmt.Errorf("writing fallback source patch %s: %w", absSrcPath, err)
		}
		out, tErr := runServiceTestSuite(ctx, wsDir, mission.Service)
		greenOut = out
		if tErr != nil {
			return res, fmt.Errorf("expected patched service %s tests to pass (Green), got %v:\n%s", mission.Service, tErr, out)
		}
	}

	res.GreenTestOutput = greenOut
	res.AttemptCount = maxInt(1, maxInt(redAttemptsUsed, greenAttemptsUsed))

	emitStep(TDDStepEvent{
		Phase:      PhasePatchGreen,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("[%s] Patched %s; %s and all existing tests passed (Green)", mission.Harness, detail.SourceFile, detail.RegressionTestName),
		Output:     greenOut,
		DurationMs: time.Since(phase2Start).Milliseconds(),
		Timestamp:  time.Now().UTC(),
	})

	// 6. Resolve honest SourceProvenance (LIVE strictly when both phases succeeded via a live model without fallback).
	prov, activeModel := resolveHonestChildProvenance(ctx, gwBase, mission.TaskName, mission.RequestedModel, redLiveSucceeded && greenLiveSucceeded && !usedLocalFixtureFallback)
	res.SourceProvenance = prov
	if activeModel != "" {
		res.ActiveModel = activeModel
	}

	// 7. Capture diff, commit ONLY services/<service> (never .ax-state/.ax), and push HEAD:refs/heads/fix/<task-name>.
	phase3Start := time.Now()
	res.CodeFixDiff = CaptureWorkspaceGitDiff(wsDir, filepath.Join("services", mission.Service, detail.SourceFile))
	res.UnifiedGitDiff = CaptureWorkspaceGitDiff(wsDir, filepath.Join("services", mission.Service))

	commitMsg := fmt.Sprintf("fix(%s): %s regression test and repair [%s]", mission.Service, strings.ToLower(mission.IncidentID), mission.Harness)
	commitSHA, err := stageCommitAndPushChildBranch(ctx, wsDir, mission.Service, branchRef, commitMsg)
	if err != nil {
		return res, err
	}
	res.CommitSHA = commitSHA
	res.CompletedAt = time.Now().UTC()
	res.ExitCode = 0

	emitStep(TDDStepEvent{
		Phase:      PhaseExtractDiff,
		Status:     "PASSED",
		Summary:    fmt.Sprintf("Committed %s (%d diff bytes) and pushed %s", commitSHA, len(res.UnifiedGitDiff), branchRef),
		Output:     res.UnifiedGitDiff,
		DurationMs: time.Since(phase3Start).Milliseconds(),
		Timestamp:  res.CompletedAt,
	})

	if err := SaveTaskStateSnapshot(wsDir, res); err != nil {
		return res, err
	}

	if cbURL != "" {
		resCopy := res
		_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
			ExecutionID:      execID,
			RootTaskName:     rootTaskName,
			TaskName:         mission.TaskName,
			Service:          mission.Service,
			EventType:        ObserverEventChildCompleted,
			IncidentID:       mission.IncidentID,
			HarnessMode:      mission.Harness,
			RequestedModel:   mission.RequestedModel,
			SourceProvenance: res.SourceProvenance,
			Child:            &resCopy,
			Timestamp:        res.CompletedAt,
		})
	}

	return res, nil
}

func buildSandboxRedPrompt(mission ChildTaskMission, detail storefront.ServiceIncidentDetail, relTestPath string, attempt int, prevDiag string) string {
	base := fmt.Sprintf(
		"PHASE=REPRODUCE_RED INCIDENT_ID=%s SERVICE=%s TARGET_FILE=%s\n"+
			"[%s/%s] Author failing regression test %s in %s reproducing HTTP 500 %s:\n%s\n"+
			"Do NOT modify %s yet. Write or emit the complete updated test file inside <file path=%q>...</file>.",
		mission.IncidentID,
		mission.Service,
		relTestPath,
		mission.IncidentID,
		mission.Service,
		detail.RegressionTestName,
		relTestPath,
		detail.ErrorMessage,
		detail.StackTrace,
		filepath.ToSlash(filepath.Join("services", mission.Service, detail.SourceFile)),
		relTestPath,
	)
	if attempt > 1 && prevDiag != "" {
		base += "\n\nPREVIOUS ATTEMPT FEEDBACK:\n" + prevDiag
	}
	return base
}

func buildSandboxGreenPrompt(mission ChildTaskMission, detail storefront.ServiceIncidentDetail, relSrcPath, relTestPath string, attempt int, prevDiag string) string {
	base := fmt.Sprintf(
		"PHASE=PATCH_GREEN INCIDENT_ID=%s SERVICE=%s TARGET_FILE=%s\n"+
			"[%s/%s] Patch %s so regression test %s in %s and all pre-existing unit tests pass.\n"+
			"Keep %s intact. Write or emit the complete patched source file inside <file path=%q>...</file>.",
		mission.IncidentID,
		mission.Service,
		relSrcPath,
		mission.IncidentID,
		mission.Service,
		relSrcPath,
		detail.RegressionTestName,
		relTestPath,
		relTestPath,
		relSrcPath,
	)
	if attempt > 1 && prevDiag != "" {
		base += "\n\nPREVIOUS ATTEMPT FEEDBACK:\n" + prevDiag
	}
	return base
}

func createEphemeralHarnessConfigDir(wsDir, taskName, harness, requestedModel, gwBase string) (string, error) {
	cfgDir, err := os.MkdirTemp("", "ax-harness-cfg-*")
	if err != nil {
		return "", fmt.Errorf("creating ephemeral harness config dir: %w", err)
	}
	absCfg, err := filepath.Abs(cfgDir)
	if err != nil {
		_ = os.RemoveAll(cfgDir)
		return "", err
	}
	if strings.HasPrefix(absCfg, wsDir+string(filepath.Separator)) || absCfg == wsDir {
		_ = os.RemoveAll(cfgDir)
		return "", fmt.Errorf("ephemeral config dir %q must reside outside workspace %q", absCfg, wsDir)
	}

	openAIBase := gwBase + "/s/" + taskName + "/v1"
	geminiBase := gwBase + "/s/" + taskName

	opencodeDir := filepath.Join(absCfg, "opencode")
	piDir := filepath.Join(absCfg, "pi-agent")
	agDir := filepath.Join(absCfg, "antigravity")
	for _, d := range []string{
		opencodeDir,
		piDir,
		agDir,
		filepath.Join(absCfg, "xdg-config"),
		filepath.Join(absCfg, "xdg-data"),
		filepath.Join(absCfg, "xdg-state"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			_ = os.RemoveAll(absCfg)
			return "", err
		}
	}

	opencodeModels := make(map[string]any, len(gateway.DefaultFallbackCascade))
	piModels := make([]map[string]any, 0, len(gateway.DefaultFallbackCascade))
	for _, m := range gateway.DefaultFallbackCascade {
		opencodeModels[m] = map[string]any{
			"name":      m,
			"tool_call": true,
		}
		piModels = append(piModels, map[string]any{
			"id":            m,
			"name":          m,
			"api":           "openai-completions",
			"contextWindow": 1048576,
			"maxTokens":     8192,
		})
	}

	opencodeCfg := map[string]any{
		"$schema":     "https://opencode.ai/config.json",
		"permission":  "allow",
		"model":       "agent-platform-gateway/" + requestedModel,
		"small_model": "agent-platform-gateway/gemini-3.5-flash",
		"provider": map[string]any{
			"agent-platform-gateway": map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Gemini Enterprise Agent Platform Model Gateway",
				"options": map[string]any{
					"baseURL": openAIBase,
					"apiKey":  gateway.SandboxTokenPlaceholder,
					"headers": map[string]string{
						"X-AX-Harness":   "opencode",
						"X-AX-Task-Name": taskName,
					},
				},
				"models": opencodeModels,
			},
		},
	}
	if raw, err := json.MarshalIndent(opencodeCfg, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opencodeDir, "opencode.json"), raw, 0o600)
	}

	piCfg := map[string]any{
		"defaultProvider": "agent-platform-gateway",
		"defaultModel":    requestedModel,
		"fallbackCascade": gateway.DefaultFallbackCascade,
		"providers": map[string]any{
			"agent-platform-gateway": map[string]any{
				"api":     "openai-completions",
				"type":    "openai-compatible",
				"baseUrl": openAIBase,
				"apiKey":  gateway.SandboxTokenPlaceholder,
				"headers": map[string]string{
					"X-AX-Harness":   "pi",
					"X-AX-Task-Name": taskName,
				},
				"models": piModels,
			},
		},
	}
	if raw, err := json.MarshalIndent(piCfg, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(piDir, "models.json"), raw, 0o600)
	}

	agCfg := map[string]any{
		"policy":                 "allow_all",
		"gateway_base_url":       gwBase,
		"google_gemini_base_url": geminiBase,
		"model":                  requestedModel,
		"harness":                harness,
	}
	if raw, err := json.MarshalIndent(agCfg, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(agDir, "config.json"), raw, 0o600)
	}

	return absCfg, nil
}

func invokeSandboxHarnessCLI(
	ctx context.Context,
	wsDir string,
	cfgDir string,
	explicitBinDir string,
	mission ChildTaskMission,
	gwBase string,
	prompt string,
) (string, error) {
	binPath, args, err := resolveHarnessCLICommand(mission.Harness, explicitBinDir, prompt)
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(callCtx, binPath, args...)
	cmd.Dir = wsDir

	env := httputil.EnsureGoBinOnPath(os.Environ())
	overrides := map[string]string{
		"WORKSPACE_DIR":          wsDir,
		"AX_TASK_NAME":           mission.TaskName,
		"SESSION_ID":             mission.TaskName,
		"GATEWAY_BASE_URL":       gwBase,
		"GOOGLE_GEMINI_BASE_URL": gwBase + "/s/" + mission.TaskName,
		"OPENAI_BASE_URL":        gwBase + "/s/" + mission.TaskName + "/v1",
		"AGENT_MODEL":            mission.RequestedModel,
		"GEMINI_API_KEY":         gateway.SandboxTokenPlaceholder,
		"OPENAI_API_KEY":         gateway.SandboxTokenPlaceholder,
		"OPENCODE_CONFIG":        filepath.Join(cfgDir, "opencode", "opencode.json"),
		"OPENCODE_CONFIG_DIR":    filepath.Join(cfgDir, "opencode"),
		"PI_CODING_AGENT_DIR":    filepath.Join(cfgDir, "pi-agent"),
		"ANTIGRAVITY_CONFIG_DIR": filepath.Join(cfgDir, "antigravity"),
		"XDG_CONFIG_HOME":        filepath.Join(cfgDir, "xdg-config"),
		"XDG_DATA_HOME":          filepath.Join(cfgDir, "xdg-data"),
		"XDG_STATE_HOME":         filepath.Join(cfgDir, "xdg-state"),
	}
	for k, v := range overrides {
		env = setEnvKeyValue(env, k, v)
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	return string(out), err
}

func resolveHarnessCLICommand(harness, explicitBinDir, prompt string) (string, []string, error) {
	var searchDirs []string
	if strings.TrimSpace(explicitBinDir) != "" {
		searchDirs = append(searchDirs, strings.TrimSpace(explicitBinDir))
	}
	if envDir := strings.TrimSpace(os.Getenv("AX_HARNESS_BIN_DIR")); envDir != "" {
		searchDirs = append(searchDirs, envDir)
	}
	searchDirs = append(searchDirs, "/usr/local/bin")
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		repoBin := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "harnesses", "bin"))
		searchDirs = append(searchDirs, repoBin)
	}

	preferNative := os.Getenv("AX_PREFER_NATIVE_HARNESS_CLI") == "1"

	findCandidate := func(names ...string) string {
		for _, name := range names {
			for _, dir := range searchDirs {
				cand := filepath.Join(dir, name)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand
				}
			}
			if found, err := exec.LookPath(name); err == nil {
				return found
			}
		}
		return ""
	}

	switch harness {
	case "antigravity":
		bin := findCandidate("antigravity")
		if bin == "" {
			return "", nil, fmt.Errorf("antigravity CLI not found in search paths")
		}
		return bin, []string{"--print", prompt}, nil

	case "opencode":
		var bin string
		if preferNative {
			bin = findCandidate("opencode", "opencode-harness")
		} else {
			bin = findCandidate("opencode-harness", "opencode")
		}
		if bin == "" {
			return "", nil, fmt.Errorf("opencode CLI not found in search paths")
		}
		return bin, []string{"run", prompt}, nil

	case "pi":
		var bin string
		if preferNative {
			bin = findCandidate("pi", "pi-harness")
		} else {
			bin = findCandidate("pi-harness", "pi")
		}
		if bin == "" {
			return "", nil, fmt.Errorf("pi CLI not found in search paths")
		}
		return bin, []string{"--print", "--no-session", prompt}, nil

	default:
		return "", nil, fmt.Errorf("unsupported harness %q", harness)
	}
}

func resolveHonestChildProvenance(ctx context.Context, gwBase, taskName, requestedModel string, bothPhasesLiveViaCLI bool) (string, string) {
	if !bothPhasesLiveViaCLI || gwBase == "" {
		return string(gateway.ProvenanceFixture), requestedModel
	}
	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := gateway.NewClient(gwBase, nil)
	snap, err := client.QuerySessions(qCtx)
	if err != nil {
		return string(gateway.ProvenanceFixture), requestedModel
	}

	var matchedSess *gateway.TaskSession
	for i := range snap.Sessions {
		if snap.Sessions[i].TaskName == taskName {
			matchedSess = &snap.Sessions[i]
			break
		}
	}
	if matchedSess == nil {
		return string(gateway.ProvenanceFixture), requestedModel
	}
	activeModel := matchedSess.ActiveModel
	if activeModel == "" {
		activeModel = requestedModel
	}
	if matchedSess.SourceProvenance != gateway.ProvenanceLive {
		return string(gateway.ProvenanceFixture), activeModel
	}
	sawTaskCall := false
	for _, call := range snap.Calls {
		if call.TaskName == taskName {
			sawTaskCall = true
			if call.SourceProvenance != gateway.ProvenanceLive {
				return string(gateway.ProvenanceFixture), activeModel
			}
		}
	}
	if !sawTaskCall {
		return string(gateway.ProvenanceFixture), activeModel
	}
	return string(gateway.ProvenanceLive), activeModel
}

func ensureGitWorkspaceCheckedOutMain(ctx context.Context, wsDir, gitURL string) error {
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return err
	}
	gitDir := filepath.Join(wsDir, ".git")
	servicesDir := filepath.Join(wsDir, "services")
	_, hasGit := os.Stat(gitDir)
	_, hasServices := os.Stat(servicesDir)

	if hasGit == nil && hasServices == nil {
		if strings.TrimSpace(gitURL) != "" && !hasGitOriginRemote(wsDir) {
			_ = runGitCmd(ctx, wsDir, "remote", "add", "origin", gitURL)
		}
		return nil
	}

	if strings.TrimSpace(gitURL) == "" {
		return fmt.Errorf("workspace %s is missing services/ and StorefrontGitURL is empty", wsDir)
	}

	if hasGit != nil {
		if err := runGitCmd(ctx, wsDir, "init"); err != nil {
			return err
		}
	}
	if err := runGitCmd(ctx, wsDir, "remote", "add", "origin", gitURL); err != nil {
		if err2 := runGitCmd(ctx, wsDir, "remote", "set-url", "origin", gitURL); err2 != nil {
			return err2
		}
	}
	if err := runGitCmd(ctx, wsDir, "fetch", "origin", "main"); err != nil {
		return err
	}
	if err := runGitCmd(ctx, wsDir, "checkout", "-B", "main", "FETCH_HEAD"); err != nil {
		return err
	}
	return nil
}

func resetServiceToBaselineBytes(ctx context.Context, wsDir, service, absSrcPath string, srcBytes []byte, absTestPath string, testBytes []byte) error {
	_ = runGitCmd(ctx, wsDir, "checkout", "HEAD", "--", filepath.Join("services", service))
	_ = runGitCmd(ctx, wsDir, "clean", "-fd", "--", filepath.Join("services", service))
	if len(srcBytes) > 0 {
		if err := os.WriteFile(absSrcPath, srcBytes, 0o644); err != nil {
			return err
		}
	}
	if len(testBytes) > 0 {
		if err := os.WriteFile(absTestPath, testBytes, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func extractAndWriteServiceFileBlocks(wsDir, service, responseText string) (map[string]bool, error) {
	servicePrefix := "services/" + service + "/"
	written, err := extractAndWriteFileBlocks(wsDir, responseText)
	for rel := range written {
		if !strings.HasPrefix(rel, servicePrefix) {
			_ = os.Remove(filepath.Join(wsDir, filepath.FromSlash(rel)))
			delete(written, rel)
		}
	}
	return written, err
}

func cleanStrayUntrackedOutsideService(wsDir, service string) {
	entries, err := os.ReadDir(wsDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		switch name {
		case ".git", "services", AXStateDirName, ".ax", "go.mod", "go.sum", "README.md":
			continue
		default:
			_ = os.RemoveAll(filepath.Join(wsDir, name))
		}
	}
	svcEntries, err := os.ReadDir(filepath.Join(wsDir, "services"))
	if err != nil {
		return
	}
	for _, ent := range svcEntries {
		if ent.Name() != service && ent.Name() != "run-service-tests.sh" {
			_ = exec.Command("git", "-C", wsDir, "checkout", "HEAD", "--", filepath.Join("services", ent.Name())).Run()
			_ = exec.Command("git", "-C", wsDir, "clean", "-fd", "--", filepath.Join("services", ent.Name())).Run()
		}
	}
}

func stageCommitAndPushChildBranch(ctx context.Context, wsDir, service, branchRef, commitMsg string) (string, error) {
	if !strings.HasPrefix(branchRef, "refs/heads/fix/") || strings.Contains(branchRef, "..") || strings.ContainsAny(branchRef, " \t\r\n") {
		return "", fmt.Errorf("invalid child fix branch ref %q", branchRef)
	}
	serviceRel := filepath.ToSlash(filepath.Join("services", service))
	_ = runGitCmd(ctx, wsDir, "reset")
	if err := runGitCmd(ctx, wsDir, "add", "--", serviceRel); err != nil {
		return "", err
	}

	stagedCmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only")
	stagedCmd.Dir = wsDir
	stagedOut, err := stagedCmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspecting staged files: %w", err)
	}
	stagedLines := strings.Split(strings.TrimSpace(string(stagedOut)), "\n")
	if len(stagedLines) == 0 || (len(stagedLines) == 1 && stagedLines[0] == "") {
		return "", fmt.Errorf("no staged changes under %s to commit", serviceRel)
	}
	allowedPrefix := serviceRel + "/"
	for _, f := range stagedLines {
		trimmed := strings.TrimSpace(f)
		if !strings.HasPrefix(trimmed, allowedPrefix) {
			return "", fmt.Errorf("refusing to commit file %q outside %s", trimmed, allowedPrefix)
		}
	}

	if err := runGitCmd(
		ctx,
		wsDir,
		"-c", "user.name=AX Child Fixer",
		"-c", "user.email=ax-child-fixer@ax-bug-bash.local",
		"commit", "-m", commitMsg,
	); err != nil {
		return "", err
	}

	shaCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	shaCmd.Dir = wsDir
	shaOut, err := shaCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	commitSHA := strings.TrimSpace(string(shaOut))

	if hasGitOriginRemote(wsDir) {
		if err := runGitCmd(ctx, wsDir, "push", "--force", "origin", "HEAD:"+branchRef); err != nil {
			return "", fmt.Errorf("pushing %s: %w", branchRef, err)
		}
	}
	return commitSHA, nil
}

func runGitCmd(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func setEnvKeyValue(env []string, key, val string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

func truncateOutput(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...[truncated]"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SandboxRootOptions configures in-sandbox Root Task triage, Child Task fan-out, and verified branch merging.
type SandboxRootOptions struct {
	ExecutionID             string
	RootTaskName            string
	Atespace                string
	WorkspaceName           string
	WorkspaceDir            string
	Alert                   AlertPayload
	AXServerAddr            string
	AXClient                v1alpha1.AXClient
	TaskRunnerImage         string
	ChildCommand            []string
	StorefrontGitURL        string
	ModelGatewayURL         string
	ObserverCallbackURL     string
	LokiURL                 string
	TreeDeadline            time.Duration
	PollInterval            time.Duration
	RaceHarnessesPerService bool
}

type spawnedChildCandidate struct {
	taskName  string
	service   string
	harness   string
	mission   ChildTaskMission
	branchRef string
	terminal  bool
	failed    bool
	verified  bool
	failErr   string
}

// ExecuteSandboxRootTask runs the Phase 2 zero-trust in-sandbox Root Task lifecycle:
//  1. Idempotently short-circuits if durable root completion state already exists.
//  2. Clones/syncs storefront.git on main, enriches traces from Loki if configured, and runs AnalyzeIncidentTraces.
//  3. Streams root.triaged and spawns digest-pinned Child Tasks over ax.v1alpha1.AX gRPC (including parallel multi races).
//  4. Polls GetTask within a 120s deadline and verifies `git ls-remote --exit-code origin refs/heads/fix/<task-name>`
//     before merging (emitting root.failed without pushing main if any service fails, times out, or omits its branch).
//  5. Merges verified child branches, runs touched-service tests, pushes HEAD:main, and streams root.merged.
func ExecuteSandboxRootTask(ctx context.Context, opts SandboxRootOptions) (*BugBashTreeExecution, error) {
	startedAt := time.Now().UTC()
	rootTaskName := SanitizeTaskName(firstNonEmptyString(opts.RootTaskName, os.Getenv("AX_ROOT_TASK_NAME"), os.Getenv("AX_TASK_NAME"), "triage-root"))
	execID := firstNonEmptyString(opts.ExecutionID, os.Getenv("AX_EXECUTION_ID"), "tree-"+rootTaskName)
	atespace := firstNonEmptyString(opts.Atespace, "default")
	wsName := firstNonEmptyString(opts.WorkspaceName, DefaultWorkspaceName)
	wsDir := firstNonEmptyString(opts.WorkspaceDir, "/workspace")
	if absWS, err := filepath.Abs(wsDir); err == nil {
		wsDir = absWS
	}

	gitURL := firstNonEmptyString(opts.StorefrontGitURL, os.Getenv("STOREFRONT_GIT_URL"))
	gwURL := firstNonEmptyString(opts.ModelGatewayURL, os.Getenv("MODEL_GATEWAY_URL"))
	cbURL := strings.TrimSpace(firstNonEmptyString(opts.ObserverCallbackURL, os.Getenv("OBSERVER_CALLBACK_URL")))
	lokiURL := strings.TrimSpace(firstNonEmptyString(opts.LokiURL, os.Getenv("LOKI_URL")))
	axAddr := strings.TrimSpace(firstNonEmptyString(opts.AXServerAddr, os.Getenv("AX_SERVER_ADDR")))
	taskImage := strings.TrimSpace(firstNonEmptyString(opts.TaskRunnerImage, os.Getenv("AX_DEFAULT_TASK_IMAGE"), os.Getenv("TASK_RUNNER_IMAGE")))

	// 1. Idempotent short-circuit if durable root state already reached HEALED.
	rootStateFile := filepath.Join(wsDir, AXStateDirName, "root-"+rootTaskName+".json")
	if raw, err := os.ReadFile(rootStateFile); err == nil {
		var saved BugBashTreeExecution
		if json.Unmarshal(raw, &saved) == nil && (saved.Phase == "HEALED" || saved.Phase == "DEPLOYED") {
			return &saved, nil
		}
	}

	emitRootFailed := func(incidentID string, cause error) (*BugBashTreeExecution, error) {
		if cbURL != "" {
			_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
				ExecutionID:  execID,
				RootTaskName: rootTaskName,
				EventType:    ObserverEventRootFailed,
				IncidentID:   incidentID,
				Error:        cause.Error(),
				Timestamp:    time.Now().UTC(),
			})
		}
		return nil, cause
	}

	if taskImage != "" {
		if err := ValidateDigestPinnedImage(taskImage); err != nil {
			return emitRootFailed(opts.Alert.IncidentID, err)
		}
	}
	if gitURL == "" {
		return emitRootFailed(opts.Alert.IncidentID, fmt.Errorf("ExecuteSandboxRootTask requires non-empty StorefrontGitURL"))
	}

	// 2. Clone/sync storefront.git on main.
	if err := ensureGitWorkspaceCheckedOutMain(ctx, wsDir, gitURL); err != nil {
		return emitRootFailed(opts.Alert.IncidentID, fmt.Errorf("cloning storefront.git in root task: %w", err))
	}
	if hasGitOriginRemote(wsDir) {
		_ = runGitCmd(ctx, wsDir, "fetch", "origin", "main")
		_ = runGitCmd(ctx, wsDir, "checkout", "-B", "main", "FETCH_HEAD")
	}
	_ = os.MkdirAll(filepath.Join(wsDir, AXStateDirName), 0o755)
	_ = os.MkdirAll(filepath.Join(wsDir, ".ax"), 0o755)

	// 3. Enrich stack traces from Loki if configured and missing, then run AnalyzeIncidentTraces.
	alert := opts.Alert
	if len(alert.StackTraces) == 0 && lokiURL != "" {
		if lokiTraces := queryLokiErrorTraces(ctx, lokiURL); len(lokiTraces) > 0 {
			alert.StackTraces = append(alert.StackTraces, lokiTraces...)
		}
	}

	plan, err := AnalyzeIncidentTraces(alert, wsDir)
	if err != nil {
		return emitRootFailed(alert.IncidentID, fmt.Errorf("root AnalyzeIncidentTraces failed: %w", err))
	}

	if cbURL != "" {
		_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
			ExecutionID:        execID,
			RootTaskName:       rootTaskName,
			EventType:          ObserverEventRootTriaged,
			IncidentID:         plan.IncidentID,
			HarnessMode:        plan.HarnessMode,
			RequestedModel:     plan.RequestedModel,
			ImplicatedServices: plan.ImplicatedServices,
			UnaffectedServices: plan.UnaffectedServices,
			Timestamp:          time.Now().UTC(),
		})
	}

	// 4. Connect to ax-server gRPC and spawn digest-pinned Child Tasks.
	axClient := opts.AXClient
	if axClient == nil {
		if axAddr == "" {
			return emitRootFailed(plan.IncidentID, fmt.Errorf("ExecuteSandboxRootTask requires AXServerAddr or AXClient"))
		}
		conn, dErr := grpc.NewClient(axAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if dErr != nil {
			return emitRootFailed(plan.IncidentID, fmt.Errorf("dialing ax-server %s: %w", axAddr, dErr))
		}
		defer conn.Close()
		axClient = v1alpha1.NewAXClient(conn)
	}

	racePerService := opts.RaceHarnessesPerService ||
		strings.EqualFold(alert.HarnessMode, "multi-race") ||
		os.Getenv("AX_MULTI_RACE_HARNESSES") == "true"

	childCmd := opts.ChildCommand
	if len(childCmd) == 0 && taskImage != "" {
		childCmd = []string{"/usr/local/bin/ax-task-runner"}
	}

	extraEnv := []*v1alpha1.EnvVar{
		{Name: "AX_EXECUTION_ID", Value: execID},
		{Name: "AX_ROOT_TASK_NAME", Value: rootTaskName},
		{Name: "STOREFRONT_GIT_URL", Value: gitURL},
	}
	if axAddr != "" {
		extraEnv = append(extraEnv, &v1alpha1.EnvVar{Name: "AX_SERVER_ADDR", Value: axAddr})
	}
	if gwURL != "" {
		extraEnv = append(extraEnv, &v1alpha1.EnvVar{Name: "MODEL_GATEWAY_URL", Value: gwURL})
	}
	if cbURL != "" {
		extraEnv = append(extraEnv, &v1alpha1.EnvVar{Name: "OBSERVER_CALLBACK_URL", Value: cbURL})
	}

	var missionsToSpawn []ChildTaskMission
	suffix := rootSuffixToken(rootTaskName)
	for _, baseSpec := range plan.ChildSpecs {
		if racePerService {
			for _, h := range gateway.SupportedHarnesses {
				racer := baseSpec
				racer.Harness = h
				racer.ParentTask = rootTaskName
				racer.TaskName = SanitizeTaskName(fmt.Sprintf("%s-%s-%s", baseSpec.TaskName, h[:2], suffix))
				missionsToSpawn = append(missionsToSpawn, racer)
			}
		} else {
			single := baseSpec
			single.ParentTask = rootTaskName
			single.TaskName = SanitizeTaskName(fmt.Sprintf("%s-%s", baseSpec.TaskName, suffix))
			missionsToSpawn = append(missionsToSpawn, single)
		}
	}

	spawnPlan := plan
	spawnPlan.ChildSpecs = missionsToSpawn
	spawnedTasks, err := SpawnChildTasksForTriage(ctx, axClient, rootTaskName, spawnPlan, SpawnChildOptions{
		Atespace:        atespace,
		WorkspaceName:   wsName,
		WorkspacePath:   "/workspace",
		TaskRunnerImage: taskImage,
		Command:         childCmd,
		ExtraEnv:        extraEnv,
	})
	if err != nil {
		return emitRootFailed(plan.IncidentID, fmt.Errorf("spawning child tasks from root: %w", err))
	}

	candidatesByService := make(map[string][]*spawnedChildCandidate, len(plan.ImplicatedServices))
	var spawnedNames []string
	for i, m := range missionsToSpawn {
		tName := spawnedTasks[i].GetMetadata().GetName()
		m.TaskName = tName
		cand := &spawnedChildCandidate{
			taskName:  tName,
			service:   m.Service,
			harness:   m.Harness,
			mission:   m,
			branchRef: "refs/heads/fix/" + tName,
		}
		candidatesByService[m.Service] = append(candidatesByService[m.Service], cand)
		spawnedNames = append(spawnedNames, tName)

		if cbURL != "" {
			childStub := ChildTaskResult{
				TaskName:           tName,
				ParentTask:         rootTaskName,
				IncidentID:         plan.IncidentID,
				Service:            m.Service,
				Language:           m.Language,
				Harness:            m.Harness,
				RequestedModel:     m.RequestedModel,
				RegressionTestName: m.RegressionTestName,
				BranchName:         cand.branchRef,
			}
			_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
				ExecutionID:    execID,
				RootTaskName:   rootTaskName,
				TaskName:       tName,
				Service:        m.Service,
				EventType:      ObserverEventChildSpawned,
				IncidentID:     plan.IncidentID,
				HarnessMode:    m.Harness,
				RequestedModel: m.RequestedModel,
				Child:          &childStub,
				Timestamp:      time.Now().UTC(),
			})
		}
	}

	// 5. Enforce 120s tree deadline and verify `git ls-remote --exit-code origin refs/heads/fix/<task-name>`.
	deadline := opts.TreeDeadline
	if deadline <= 0 {
		deadline = 120 * time.Second
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, deadline)
	defer waitCancel()

	winnersByService := make(map[string]*spawnedChildCandidate, len(plan.ImplicatedServices))
	for len(winnersByService) < len(plan.ImplicatedServices) {
		for _, svc := range plan.ImplicatedServices {
			if _, alreadyWon := winnersByService[svc]; alreadyWon {
				continue
			}
			cands := candidatesByService[svc]
			allFailed := true
			var lastFailReason string

			for _, cand := range cands {
				if cand.terminal {
					if cand.failErr != "" {
						lastFailReason = cand.failErr
					}
					continue
				}
				allFailed = false

				gotTask, gErr := axClient.GetTask(waitCtx, &v1alpha1.GetTaskRequest{
					Atespace: atespace,
					Name:     cand.taskName,
				})
				if gErr != nil {
					continue
				}
				phase := gotTask.GetStatus().GetPhase()
				switch phase {
				case v1alpha1.PhaseFailed, v1alpha1.PhaseTerminating:
					cand.terminal = true
					cand.failed = true
					cand.failErr = fmt.Sprintf("child task %s (%s) entered phase %s", cand.taskName, svc, phase)
					lastFailReason = cand.failErr

				case v1alpha1.PhaseSuspended, "Succeeded":
					cand.terminal = true
					if vErr := verifyRemoteFixBranch(waitCtx, gitURL, cand.branchRef); vErr != nil {
						cand.failed = true
						cand.failErr = fmt.Sprintf("child task %s (%s) reached %s without verified remote branch %s: %v",
							cand.taskName, svc, phase, cand.branchRef, vErr)
						lastFailReason = cand.failErr
						continue
					}
					cand.verified = true
					winnersByService[svc] = cand

					// Suspend any remaining parallel racers for this service immediately to reclaim worker slots!
					for _, sibling := range cands {
						if sibling.taskName != cand.taskName && !sibling.terminal {
							sibling.terminal = true
							_, _ = axClient.SuspendTask(waitCtx, &v1alpha1.SuspendTaskRequest{
								Atespace: atespace,
								Name:     sibling.taskName,
							})
						}
					}
					break
				}
			}

			if _, won := winnersByService[svc]; !won {
				stillPending := false
				for _, cand := range cands {
					if !cand.terminal {
						stillPending = true
						break
					}
				}
				if !stillPending || allFailed {
					if lastFailReason == "" {
						lastFailReason = fmt.Sprintf("all child candidates for %s failed branch verification", svc)
					}
					// Suspend any other active child tasks across other services before failing closed
					suspendRemainingCandidates(ctx, axClient, atespace, candidatesByService)
					return emitRootFailed(plan.IncidentID, fmt.Errorf("%s", lastFailReason))
				}
			}
		}

		if len(winnersByService) >= len(plan.ImplicatedServices) {
			break
		}

		select {
		case <-waitCtx.Done():
			suspendRemainingCandidates(context.Background(), axClient, atespace, candidatesByService)
			return emitRootFailed(plan.IncidentID, fmt.Errorf("root task deadline (%s) exceeded waiting for verified refs/heads/fix/* child branches: %w", deadline, waitCtx.Err()))
		case <-time.After(pollInterval):
		}
	}

	// 6. Fetch and merge verified refs/heads/fix/<task-name> branches into main, run tests, and push HEAD:main.
	rolloutStart := time.Now()
	if err := runGitCmd(ctx, wsDir, "fetch", "origin", "main"); err != nil {
		return emitRootFailed(plan.IncidentID, fmt.Errorf("fetching origin/main before merge: %w", err))
	}
	if err := runGitCmd(ctx, wsDir, "checkout", "-B", "main", "FETCH_HEAD"); err != nil {
		return emitRootFailed(plan.IncidentID, fmt.Errorf("checking out clean main before merge: %w", err))
	}

	winningChildren := make([]ChildTaskResult, 0, len(plan.ImplicatedServices))
	for _, svc := range plan.ImplicatedServices {
		winner := winnersByService[svc]
		if err := runGitCmd(ctx, wsDir, "fetch", "origin", winner.branchRef); err != nil {
			return emitRootFailed(plan.IncidentID, fmt.Errorf("fetching verified branch %s: %w", winner.branchRef, err))
		}
		mergeMsg := fmt.Sprintf("merge(%s): integrate verified child branch %s", svc, winner.branchRef)
		if err := runGitCmd(
			ctx,
			wsDir,
			"-c", "user.name=AX Root Triage Agent",
			"-c", "user.email=ax-root-triage@ax-bug-bash.local",
			"merge", "--no-ff", "-m", mergeMsg, "FETCH_HEAD",
		); err != nil {
			_ = runGitCmd(ctx, wsDir, "merge", "--abort")
			return emitRootFailed(plan.IncidentID, fmt.Errorf("merging %s failed with conflict: %w", winner.branchRef, err))
		}

		winningChildren = append(winningChildren, ChildTaskResult{
			TaskName:           winner.taskName,
			ParentTask:         rootTaskName,
			IncidentID:         plan.IncidentID,
			Service:            svc,
			Language:           winner.mission.Language,
			Harness:            winner.harness,
			RequestedModel:     winner.mission.RequestedModel,
			RegressionTestName: winner.mission.RegressionTestName,
			BranchName:         winner.branchRef,
			Suspended:          true,
		})
	}

	// Ensure zero .ax-state or .ax markers entered the merged Git tree.
	treeCmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", "HEAD")
	treeCmd.Dir = wsDir
	if treeOut, tErr := treeCmd.Output(); tErr == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(treeOut)), "\n") {
			if strings.HasPrefix(f, AXStateDirName+"/") || strings.HasPrefix(f, ".ax/") {
				return emitRootFailed(plan.IncidentID, fmt.Errorf("refusing to push main: state marker %q found in merged tree", f))
			}
		}
	}

	// Run cross-service verification tests on every implicated service before pushing main!
	for _, svc := range plan.ImplicatedServices {
		if out, tErr := runServiceTestSuite(ctx, wsDir, svc); tErr != nil {
			return emitRootFailed(plan.IncidentID, fmt.Errorf("merged verification test suite failed for %s: %w\n%s", svc, tErr, truncateOutput(out, 1200)))
		}
	}

	diffCmd := exec.CommandContext(ctx, "git", "diff", "origin/main..HEAD", "--", "services")
	diffCmd.Dir = wsDir
	diffBytes, _ := diffCmd.Output()
	mergedDiff := string(diffBytes)

	if err := runGitCmd(ctx, wsDir, "push", "origin", "HEAD:main"); err != nil {
		return emitRootFailed(plan.IncidentID, fmt.Errorf("pushing merged fix HEAD:main failed: %w", err))
	}

	shaCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	shaCmd.Dir = wsDir
	shaOut, _ := shaCmd.Output()
	mergedSHA := strings.TrimSpace(string(shaOut))
	if mergedSHA == "" {
		mergedSHA = "merged"
	}

	completedAt := time.Now().UTC()
	rolloutMs := time.Since(rolloutStart).Milliseconds()
	if rolloutMs <= 0 {
		rolloutMs = 1
	}

	tree := &BugBashTreeExecution{
		ExecutionID:        execID,
		ExecutionMode:      ExecutionModeLiveGKE,
		IncidentID:         plan.IncidentID,
		Title:              plan.Title,
		Summary:            plan.Summary,
		WhyCIMissedIt:      plan.WhyCIMissedIt,
		HarnessMode:        plan.HarnessMode,
		RequestedModel:     plan.RequestedModel,
		Phase:              "HEALED",
		ImplicatedServices: plan.ImplicatedServices,
		UnaffectedServices: plan.UnaffectedServices,
		ChildTasks:         winningChildren,
		MergedGitCommitSHA: mergedSHA,
		MergedUnifiedDiff:  mergedDiff,
		RolloutDurationMs:  rolloutMs,
		StorefrontHealed:   true,
		StartedAt:          startedAt,
		CompletedAt:        completedAt,
		RootTask: RootTaskNode{
			TaskName:           rootTaskName,
			Phase:              v1alpha1.PhaseSuspended,
			ImplicatedServices: plan.ImplicatedServices,
			UnaffectedServices: plan.UnaffectedServices,
			SpawnedChildren:    spawnedNames,
			StartedAt:          startedAt,
			SuspendedAt:        completedAt,
		},
	}

	if rawTree, mErr := json.MarshalIndent(tree, "", "  "); mErr == nil {
		_ = os.WriteFile(rootStateFile, rawTree, 0o644)
	}

	if cbURL != "" {
		_ = PostObserverEvent(ctx, cbURL, ObserverCallbackEvent{
			ExecutionID:       execID,
			RootTaskName:      rootTaskName,
			EventType:         ObserverEventRootMerged,
			IncidentID:        plan.IncidentID,
			HarnessMode:       plan.HarnessMode,
			RequestedModel:    plan.RequestedModel,
			MergedCommitSHA:   mergedSHA,
			MergedUnifiedDiff: mergedDiff,
			RolloutDurationMs: rolloutMs,
			StorefrontHealed:  true,
			Timestamp:         completedAt,
		})
	}

	return tree, nil
}

func verifyRemoteFixBranch(ctx context.Context, gitURL, branchRef string) error {
	if !strings.HasPrefix(branchRef, "refs/heads/fix/") || strings.Contains(branchRef, "..") {
		return fmt.Errorf("invalid branch ref %q", branchRef)
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", gitURL, branchRef)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote --exit-code %s %s: %w (%s)", gitURL, branchRef, err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("remote branch %s returned empty ref", branchRef)
	}
	return nil
}

func suspendRemainingCandidates(ctx context.Context, axClient v1alpha1.AXClient, atespace string, byService map[string][]*spawnedChildCandidate) {
	if axClient == nil {
		return
	}
	sCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, cands := range byService {
		for _, c := range cands {
			if !c.terminal {
				c.terminal = true
				_, _ = axClient.SuspendTask(sCtx, &v1alpha1.SuspendTaskRequest{
					Atespace: atespace,
					Name:     c.taskName,
				})
			}
		}
	}
}

func rootSuffixToken(rootTaskName string) string {
	parts := strings.Split(rootTaskName, "-")
	if len(parts) > 0 && parts[len(parts)-1] != "" {
		last := parts[len(parts)-1]
		if len(last) > 8 {
			last = last[:8]
		}
		return last
	}
	return "1"
}

func queryLokiErrorTraces(ctx context.Context, lokiURL string) []string {
	qCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	base := strings.TrimRight(strings.TrimSpace(lokiURL), "/")
	u, err := url.Parse(base + "/loki/api/v1/query_range")
	if err != nil {
		return nil
	}
	q := u.Query()
	q.Set("query", `{app=~".+"} |= "500"`)
	q.Set("limit", "50")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(qCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil
	}
	var payload struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	var traces []string
	for _, stream := range payload.Data.Result {
		for _, entry := range stream.Values {
			if len(entry) >= 2 && strings.TrimSpace(entry[1]) != "" {
				traces = append(traces, entry[1])
			}
		}
	}
	return traces
}
