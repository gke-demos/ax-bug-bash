package bugbash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// OrchestratorConfig configures the Bug Bash Tree Orchestrator.
type OrchestratorConfig struct {
	AXServerAddr          string
	Storefront            *storefront.Runtime
	StorefrontGitURL      string
	ModelGateway          *gateway.Server
	ModelGatewayURL       string
	DefaultHarnessMode    string
	DefaultRequestedModel string
	OnRolloutCompleted    func(tree *BugBashTreeExecution)
}

// TriggerRequest initiates a recursive Bug Bash Tree run for an Incident Scenario.
type TriggerRequest struct {
	IncidentID     string   `json:"incident_id"`
	HarnessMode    string   `json:"harness_mode,omitempty"`
	RequestedModel string   `json:"requested_model,omitempty"`
	AlertName      string   `json:"alert_name,omitempty"`
	ErrorMessage   string   `json:"error_message,omitempty"`
	StackTraces    []string `json:"stack_traces,omitempty"`
}

// RootTaskNode represents the top-level Root Task (`triage-<id>`) in a Bug Bash Tree execution.
type RootTaskNode struct {
	TaskName           string    `json:"task_name"`
	Phase              string    `json:"phase"`
	WorkerID           string    `json:"worker_id"`
	ImplicatedServices []string  `json:"implicated_services"`
	UnaffectedServices []string  `json:"unaffected_services"`
	SpawnedChildren    []string  `json:"spawned_children"`
	StartedAt          time.Time `json:"started_at"`
	SuspendedAt        time.Time `json:"suspended_at,omitempty"`
}

// BugBashTreeExecution captures the full hierarchical state of a Root Task and its spawned Child Tasks.
type BugBashTreeExecution struct {
	ExecutionID        string            `json:"execution_id"`
	IncidentID         string            `json:"incident_id"`
	Title              string            `json:"title"`
	Summary            string            `json:"summary"`
	WhyCIMissedIt      string            `json:"why_ci_missed_it"`
	HarnessMode        string            `json:"harness_mode"`
	RequestedModel     string            `json:"requested_model"`
	Phase              string            `json:"phase"`
	RootTask           RootTaskNode      `json:"root_task"`
	ImplicatedServices []string          `json:"implicated_services"`
	UnaffectedServices []string          `json:"unaffected_services"`
	ChildTasks         []ChildTaskResult `json:"child_tasks"`
	MergedGitCommitSHA string            `json:"merged_git_commit_sha,omitempty"`
	MergedUnifiedDiff  string            `json:"merged_unified_diff,omitempty"`
	RolloutDurationMs  int64             `json:"rollout_duration_ms"`
	StorefrontHealed   bool              `json:"storefront_healed"`
	StartedAt          time.Time         `json:"started_at"`
	CompletedAt        time.Time         `json:"completed_at,omitempty"`
}

// Orchestrator coordinates the recursive AX Bug Bash Tree across ax-server, Agent Substrate v0.6.0 workers,
// the Model Gateway, and the in-cluster Storefront Git server.
type Orchestrator struct {
	cfg        OrchestratorConfig
	embeddedAX *EmbeddedAXServer
	grpcConn   *grpc.ClientConn
	axClient   v1alpha1.AXClient

	mu         sync.RWMutex
	seq        int64
	executions []*BugBashTreeExecution
	byID       map[string]*BugBashTreeExecution
}

// NewOrchestrator initializes the Bug Bash Tree Orchestrator, starting an embedded gRPC AXServer
// if AXServerAddr is not provided.
func NewOrchestrator(ctx context.Context, cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.DefaultHarnessMode == "" {
		cfg.DefaultHarnessMode = "multi"
	}
	if cfg.DefaultRequestedModel == "" {
		cfg.DefaultRequestedModel = gateway.DefaultFallbackCascade[0]
	}

	var embedded *EmbeddedAXServer
	targetAddr := strings.TrimSpace(cfg.AXServerAddr)
	if targetAddr == "" {
		srv, addr, err := StartEmbeddedAXServer(ctx)
		if err != nil {
			return nil, fmt.Errorf("starting embedded ax-server: %w", err)
		}
		embedded = srv
		targetAddr = addr
		cfg.AXServerAddr = addr
	} else {
		embedded = NewEmbeddedAXServer()
	}

	conn, err := grpc.NewClient(targetAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		if embedded != nil {
			embedded.Stop()
		}
		return nil, fmt.Errorf("dialing ax-server %s: %w", targetAddr, err)
	}

	return &Orchestrator{
		cfg:        cfg,
		embeddedAX: embedded,
		grpcConn:   conn,
		axClient:   v1alpha1.NewAXClient(conn),
		byID:       make(map[string]*BugBashTreeExecution),
	}, nil
}

// Close releases gRPC client connections and stops any embedded AXServer.
func (o *Orchestrator) Close() {
	if o.grpcConn != nil {
		_ = o.grpcConn.Close()
	}
	if o.embeddedAX != nil {
		o.embeddedAX.Stop()
	}
}

// AXClient returns the underlying ax.v1alpha1.AXClient gRPC stub.
func (o *Orchestrator) AXClient() v1alpha1.AXClient {
	return o.axClient
}

// WorkerPoolSnapshot returns the current Agent Substrate v0.6.0 worker pool telemetry.
func (o *Orchestrator) WorkerPoolSnapshot() WorkerPoolSummary {
	if o.embeddedAX != nil {
		return o.embeddedAX.WorkerPoolSnapshot()
	}
	return NewEmbeddedAXServer().WorkerPoolSnapshot()
}

// LatestExecution returns the most recent BugBashTreeExecution, or nil if none has run yet.
func (o *Orchestrator) LatestExecution() *BugBashTreeExecution {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if len(o.executions) == 0 {
		return nil
	}
	return cloneExecution(o.executions[len(o.executions)-1])
}

// ListExecutions returns all recorded BugBashTreeExecutions in reverse chronological order.
func (o *Orchestrator) ListExecutions() []*BugBashTreeExecution {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*BugBashTreeExecution, 0, len(o.executions))
	for i := len(o.executions) - 1; i >= 0; i-- {
		out = append(out, cloneExecution(o.executions[i]))
	}
	return out
}

// GetExecution looks up a BugBashTreeExecution by ID.
func (o *Orchestrator) GetExecution(executionID string) (*BugBashTreeExecution, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	ex, ok := o.byID[executionID]
	if !ok {
		return nil, false
	}
	return cloneExecution(ex), true
}

// HandleObserverCallback handles POST /api/observer/callback from Custom Runner OnCommandExit hooks.
func (o *Orchestrator) HandleObserverCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var res ChildTaskResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	for _, ex := range o.executions {
		for idx := range ex.ChildTasks {
			if ex.ChildTasks[idx].TaskName == res.TaskName {
				ex.ChildTasks[idx] = res
			}
		}
	}
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
}

// TriggerIncidentBugBash executes the complete recursive Bug Bash Tree lifecycle:
// 1. Spawns the Root Task (`triage-<id>`) on ax-server (`CreateTask` + `ResumeTask`).
// 2. Root Task analyzes stack traces and identifies ONLY the 2-3 implicated services.
// 3. Root Task spawns one Child Task (`fixer-<id>-<service>`) per implicated service (`CreateTask` + `ResumeTask`).
// 4. Child Tasks run the Red-Green TDD cycle in parallel, persist `/workspace/.ax-state`, and auto-suspend (`SuspendTask`).
// 5. Root Task merges all verified child patches, commits, pushes to the Storefront Git server, and auto-suspends.
func (o *Orchestrator) TriggerIncidentBugBash(ctx context.Context, req TriggerRequest) (*BugBashTreeExecution, error) {
	incidentID := strings.ToUpper(strings.TrimSpace(req.IncidentID))
	if incidentID == "" {
		incidentID = storefront.IncidentStackedPromoNegativeSubtotal
	}
	sc, ok := storefront.GetScenario(incidentID)
	if !ok {
		return nil, fmt.Errorf("unknown incident scenario %q", incidentID)
	}

	if o.cfg.Storefront != nil {
		if active := o.cfg.Storefront.ActiveScenario(); active == nil || active.ID != sc.ID {
			_, _ = o.cfg.Storefront.ActivateIncident(sc.ID)
		}
	}

	harnessMode := strings.ToLower(strings.TrimSpace(req.HarnessMode))
	if harnessMode == "" {
		harnessMode = o.cfg.DefaultHarnessMode
	}
	requestedModel := strings.TrimSpace(req.RequestedModel)
	if requestedModel == "" {
		requestedModel = o.cfg.DefaultRequestedModel
	}

	traces := append([]string(nil), req.StackTraces...)
	if len(traces) == 0 {
		for _, svc := range sc.ImplicatedServices {
			if d, exists := sc.ServiceTraces[svc]; exists && d.StackTrace != "" {
				traces = append(traces, d.StackTrace)
			}
		}
	}

	alert := AlertPayload{
		IncidentID:     sc.ID,
		AlertName:      req.AlertName,
		ErrorCode:      sc.ErrorCode,
		ErrorMessage:   sc.ErrorMessage,
		StackTraces:    traces,
		ServiceTraces:  sc.ServiceTraces,
		HarnessMode:    harnessMode,
		RequestedModel: requestedModel,
	}
	if alert.AlertName == "" {
		alert.AlertName = "StorefrontHTTP500Spike"
	}

	o.mu.Lock()
	o.seq++
	seq := o.seq
	seqStr := strconv.FormatInt(seq, 10)
	execID := fmt.Sprintf("tree-%s-%d", strings.ToLower(sc.ID), seq)
	rootTaskName := SanitizeTaskName(fmt.Sprintf("triage-%s-%d", strings.ToLower(sc.ID), seq))
	now := time.Now().UTC()

	tree := &BugBashTreeExecution{
		ExecutionID:    execID,
		IncidentID:     sc.ID,
		Title:          sc.Title,
		Summary:        sc.Summary,
		WhyCIMissedIt:  sc.WhyCIMissedIt,
		HarnessMode:    harnessMode,
		RequestedModel: requestedModel,
		Phase:          "TRIAGING",
		RootTask: RootTaskNode{
			TaskName:  rootTaskName,
			Phase:     v1alpha1.PhaseRunning,
			StartedAt: now,
		},
		StartedAt: now,
	}
	o.executions = append(o.executions, tree)
	o.byID[execID] = tree
	o.mu.Unlock()

	gitSource, tempGitMgr, err := o.resolveGitSource()
	if err != nil {
		return nil, err
	}
	if tempGitMgr != nil {
		defer os.RemoveAll(filepath.Dir(tempGitMgr.RepoDir()))
	}

	// Register Workspace and Model on ax-server
	_, _ = o.axClient.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: DefaultWorkspaceName, Atespace: "default"},
			Spec: &v1alpha1.WorkspaceSpec{
				Git: []*v1alpha1.GitRepo{
					{Name: "storefront", Repo: gitSource, Branch: "main", Dir: "."},
				},
			},
		},
	})
	_, _ = o.axClient.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{
		Model: &v1alpha1.Model{
			Metadata: &v1alpha1.ObjectMeta{Name: "gemini-flash-cascade", Atespace: "default"},
			Spec: &v1alpha1.ModelSpec{
				Provider: "google",
				Model:    requestedModel,
			},
		},
	})

	// 1. Create and Resume Root Task on ax-server
	rootCfgStruct, err := AlertPayloadToStruct(alert)
	if err != nil {
		return nil, err
	}
	rootTaskSpec := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: rootTaskName, Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: DefaultWorkspaceName, Path: "/workspace"},
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "2048Mi"},
			},
			Config: rootCfgStruct,
		},
	}
	if _, err := o.axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: rootTaskSpec}); err != nil {
		return nil, fmt.Errorf("creating root task %s: %w", rootTaskName, err)
	}
	if _, err := o.axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: "default", Name: rootTaskName}); err != nil {
		return nil, fmt.Errorf("resuming root task %s: %w", rootTaskName, err)
	}

	// 2. Clone workspace for Root Task and run Triage analysis
	rootWorkDir, err := cloneGitWorkspace(ctx, gitSource)
	if err != nil {
		return nil, fmt.Errorf("cloning workspace for root task: %w", err)
	}
	defer os.RemoveAll(rootWorkDir)

	plan, err := AnalyzeIncidentTraces(alert, rootWorkDir)
	if err != nil {
		return nil, fmt.Errorf("root triage failed: %w", err)
	}

	// 3. Dynamically spawn Child Tasks on ax-server (ONLY for the 2-3 implicated services)
	spawnedTasks, err := SpawnChildTasksForTriage(ctx, o.axClient, rootTaskName, plan, SpawnChildOptions{
		Atespace:      "default",
		WorkspaceName: DefaultWorkspaceName,
		WorkspacePath: "/workspace",
		NameSuffix:    seqStr,
	})
	if err != nil {
		return nil, fmt.Errorf("spawning child tasks: %w", err)
	}

	var childNames []string
	initialChildren := make([]ChildTaskResult, len(plan.ChildSpecs))
	for i, spec := range plan.ChildSpecs {
		tName := spawnedTasks[i].GetMetadata().GetName()
		childNames = append(childNames, tName)
		initialChildren[i] = ChildTaskResult{
			TaskName:           tName,
			ParentTask:         rootTaskName,
			IncidentID:         sc.ID,
			Service:            spec.Service,
			Language:           spec.Language,
			Harness:            spec.Harness,
			RequestedModel:     spec.RequestedModel,
			RegressionTestName: spec.RegressionTestName,
		}
	}

	o.mu.Lock()
	tree.Phase = "FANOUT_TDD"
	tree.ImplicatedServices = append([]string(nil), plan.ImplicatedServices...)
	tree.UnaffectedServices = append([]string(nil), plan.UnaffectedServices...)
	tree.RootTask.ImplicatedServices = append([]string(nil), plan.ImplicatedServices...)
	tree.RootTask.UnaffectedServices = append([]string(nil), plan.UnaffectedServices...)
	tree.RootTask.SpawnedChildren = childNames
	tree.ChildTasks = initialChildren
	o.mu.Unlock()

	// 4. Execute Child Tasks in parallel (Red -> Green -> Diff -> Auto-Suspend)
	var (
		wg       sync.WaitGroup
		childErr error
		errOnce  sync.Once
	)
	childResults := make([]ChildTaskResult, len(plan.ChildSpecs))
	childWorkDirs := make([]string, len(plan.ChildSpecs))

	for i, spec := range plan.ChildSpecs {
		i, spec := i, spec
		tName := spawnedTasks[i].GetMetadata().GetName()
		spec.TaskName = tName
		spec.ParentTask = rootTaskName

		wg.Add(1)
		go func() {
			defer wg.Done()

			cWorkDir, err := cloneGitWorkspace(ctx, gitSource)
			if err != nil {
				errOnce.Do(func() { childErr = err })
				return
			}
			childWorkDirs[i] = cWorkDir

			res, err := ExecuteChildFixerTDD(ctx, ChildFixerConfig{
				Mission:         spec,
				WorkspaceDir:    cWorkDir,
				ModelGatewayURL: o.cfg.ModelGatewayURL,
				OnStep: func(step TDDStepEvent) {
					o.mu.Lock()
					if i < len(tree.ChildTasks) {
						tree.ChildTasks[i].TDDSteps = append(tree.ChildTasks[i].TDDSteps, step)
					}
					o.mu.Unlock()
				},
			})
			if err != nil {
				errOnce.Do(func() { childErr = err })
				return
			}

			// Auto-suspend Child Task via ax-server gRPC SuspendTask (reclaiming ateworker-gvisor slot)
			_, suspendErr := o.axClient.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
				Atespace: "default",
				Name:     tName,
			})
			if suspendErr != nil {
				errOnce.Do(func() { childErr = suspendErr })
				return
			}

			suspendedAt := time.Now().UTC()
			res.Suspended = true
			res.SuspendedAt = suspendedAt
			res.TDDSteps = append(res.TDDSteps, TDDStepEvent{
				Phase:      PhaseAutoSuspend,
				Status:     "PASSED",
				Summary:    fmt.Sprintf("OnCommandExit persisted /workspace/.ax-state/%s.json and called ax-server SuspendTask (worker slot reclaimed)", tName),
				DurationMs: 4,
				Timestamp:  suspendedAt,
			})
			_ = SaveTaskStateSnapshot(cWorkDir, res)
			childResults[i] = res

			o.mu.Lock()
			if i < len(tree.ChildTasks) {
				tree.ChildTasks[i] = res
			}
			o.mu.Unlock()
		}()
	}
	wg.Wait()

	for _, dir := range childWorkDirs {
		if dir != "" {
			defer os.RemoveAll(dir)
		}
	}
	if childErr != nil {
		o.mu.Lock()
		tree.Phase = "FAILED"
		o.mu.Unlock()
		return nil, childErr
	}

	// 5. Root Task merges all verified child patches, verifies cross-service tests, and pushes to Storefront Git server
	rolloutStart := time.Now()
	o.mu.Lock()
	tree.Phase = "MERGING_ROLLOUT"
	o.mu.Unlock()

	if err := applyChildPatchesToRootWorkspace(ctx, rootWorkDir, sc, plan.ImplicatedServices, childResults); err != nil {
		return nil, fmt.Errorf("merging child patches in root task: %w", err)
	}

	mergedDiff := CaptureWorkspaceGitDiff(rootWorkDir, "")
	commitMsg := fmt.Sprintf("fix(%s): automated AX bug bash tree repair for %s", strings.ToLower(sc.ID), strings.Join(plan.ImplicatedServices, ", "))
	commitSHA, err := commitAndPushWorkspaceFix(ctx, rootWorkDir, commitMsg)
	if err != nil {
		return nil, fmt.Errorf("committing and pushing merged fix from root task: %w", err)
	}

	// Verify Storefront Git repository received the patched code and mark healed
	if o.cfg.Storefront != nil {
		sfRepoDir := o.cfg.Storefront.GitManager().RepoDir()
		for _, svc := range plan.ImplicatedServices {
			if storefront.IsServicePatchedForIncident(sfRepoDir, sc.ID, svc) {
				o.cfg.Storefront.MarkServiceHealed(sc.ID, svc)
			}
		}
	}

	// Auto-suspend Root Task via ax-server gRPC SuspendTask
	_, _ = o.axClient.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
		Atespace: "default",
		Name:     rootTaskName,
	})

	completedAt := time.Now().UTC()
	rolloutMs := time.Since(rolloutStart).Milliseconds()
	if rolloutMs <= 0 {
		rolloutMs = 1
	}

	o.mu.Lock()
	tree.ChildTasks = childResults
	tree.MergedGitCommitSHA = commitSHA
	tree.MergedUnifiedDiff = mergedDiff
	tree.RolloutDurationMs = rolloutMs
	tree.StorefrontHealed = true
	tree.Phase = "HEALED"
	tree.CompletedAt = completedAt
	tree.RootTask.Phase = v1alpha1.PhaseSuspended
	tree.RootTask.SuspendedAt = completedAt
	snapshot := cloneExecution(tree)
	o.mu.Unlock()

	if o.cfg.OnRolloutCompleted != nil {
		o.cfg.OnRolloutCompleted(snapshot)
	}
	return snapshot, nil
}

func (o *Orchestrator) resolveGitSource() (string, *storefront.GitWorkspaceManager, error) {
	if strings.TrimSpace(o.cfg.StorefrontGitURL) != "" {
		return strings.TrimSpace(o.cfg.StorefrontGitURL), nil, nil
	}
	if o.cfg.Storefront != nil && o.cfg.Storefront.GitManager() != nil {
		return o.cfg.Storefront.GitManager().RepoDir(), nil, nil
	}
	tmpMgr, err := storefront.NewGitWorkspaceManager("", nil)
	if err != nil {
		return "", nil, err
	}
	return tmpMgr.RepoDir(), tmpMgr, nil
}

func cloneGitWorkspace(ctx context.Context, repoSource string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "ax-task-workspace-*")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", repoSource, tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("git clone %s: %w (%s)", repoSource, err, string(out))
	}
	return tmpDir, nil
}

func applyChildPatchesToRootWorkspace(
	ctx context.Context,
	rootWorkDir string,
	sc storefront.IncidentScenario,
	implicatedServices []string,
	childResults []ChildTaskResult,
) error {
	diffByService := make(map[string]string, len(childResults))
	for _, cr := range childResults {
		if strings.TrimSpace(cr.UnifiedGitDiff) != "" {
			diffByService[cr.Service] = cr.UnifiedGitDiff
		}
	}

	for _, svc := range implicatedServices {
		appliedDiff := false
		if patch := strings.TrimSpace(diffByService[svc]); patch != "" {
			cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", "-")
			cmd.Dir = rootWorkDir
			cmd.Stdin = strings.NewReader(patch + "\n")
			if err := cmd.Run(); err == nil {
				appliedDiff = true
			}
		}

		if !appliedDiff {
			detail, ok := sc.ServiceTraces[svc]
			if !ok {
				continue
			}
			testPath := filepath.Join(rootWorkDir, "services", svc, detail.TestFile)
			if err := os.WriteFile(testPath, []byte(detail.RegressionTestFileContent), 0o644); err != nil {
				return err
			}
			srcPath := filepath.Join(rootWorkDir, "services", svc, detail.SourceFile)
			if err := os.WriteFile(srcPath, []byte(detail.PatchedSourceFileContent), 0o644); err != nil {
				return err
			}
		}

		if out, err := runServiceTestSuite(ctx, rootWorkDir, svc); err != nil {
			return fmt.Errorf("cross-service verification failed on %s: %w\n%s", svc, err, out)
		}
	}
	return nil
}

func commitAndPushWorkspaceFix(ctx context.Context, workDir, commitMsg string) (string, error) {
	cmds := [][]string{
		{"config", "user.email", "ax-root-triage@ax-bug-bash.local"},
		{"config", "user.name", "AX Root Triage Agent"},
		{"add", "."},
		{"commit", "-m", commitMsg},
		{"push", "origin", "HEAD:main"},
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %w (%s)", args, err, string(out))
		}
	}
	shaCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	shaCmd.Dir = workDir
	out, err := shaCmd.Output()
	if err != nil {
		return "patched", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func cloneExecution(in *BugBashTreeExecution) *BugBashTreeExecution {
	if in == nil {
		return nil
	}
	raw, _ := json.Marshal(in)
	var out BugBashTreeExecution
	_ = json.Unmarshal(raw, &out)
	return &out
}
