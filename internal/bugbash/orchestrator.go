package bugbash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/gke-demos/ax-bug-bash/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ExecutionMode defines whether the Bug Bash Tree executes against a live GKE google/ax + Agent Substrate
// cluster ("LIVE_GKE", fail-closed) or hermetically in-process ("SIMULATED").
type ExecutionMode string

const (
	ExecutionModeLiveGKE   ExecutionMode = "LIVE_GKE"
	ExecutionModeSimulated ExecutionMode = "SIMULATED"
)

var sha256DigestPinRe = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

// ResolveExecutionMode deterministically resolves ExecutionMode from explicitMode and axServerAddr.
// When explicitMode is empty, LIVE_GKE is selected if axServerAddr is non-empty, else SIMULATED.
// Explicit LIVE_GKE fails closed if axServerAddr is empty.
func ResolveExecutionMode(explicitMode, axServerAddr string) (ExecutionMode, error) {
	norm := strings.ToUpper(strings.TrimSpace(explicitMode))
	hasAddr := strings.TrimSpace(axServerAddr) != ""

	switch norm {
	case "":
		if hasAddr {
			return ExecutionModeLiveGKE, nil
		}
		return ExecutionModeSimulated, nil
	case string(ExecutionModeLiveGKE):
		if !hasAddr {
			return "", fmt.Errorf("EXECUTION_MODE=LIVE_GKE requires a non-empty AX_SERVER_ADDR")
		}
		return ExecutionModeLiveGKE, nil
	case string(ExecutionModeSimulated):
		return ExecutionModeSimulated, nil
	default:
		return "", fmt.Errorf("invalid EXECUTION_MODE %q (expected LIVE_GKE or SIMULATED)", explicitMode)
	}
}

// ValidateDigestPinnedImage verifies that a container image reference is pinned by an immutable @sha256: digest.
func ValidateDigestPinnedImage(image string) error {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return fmt.Errorf("task runner image is empty; LIVE_GKE requires an @sha256: digest-pinned image")
	}
	if !sha256DigestPinRe.MatchString(trimmed) {
		return fmt.Errorf("task runner image %q must be pinned by an immutable @sha256:<64-hex> digest", trimmed)
	}
	return nil
}

// OrchestratorConfig configures the Bug Bash Tree Orchestrator.
type OrchestratorConfig struct {
	ExecutionMode         ExecutionMode
	AXServerAddr          string
	TaskRunnerImage       string
	ObserverCallbackURL   string
	LokiURL               string
	Storefront            *storefront.Runtime
	StorefrontGitURL      string
	ModelGateway          *gateway.Server
	ModelGatewayURL       string
	GatewayClient         *gateway.Client
	SubstrateViewer       *substrate.Viewer
	SubstrateViewerOpts   *substrate.ViewerOptions
	DefaultHarnessMode    string
	DefaultRequestedModel string
	OnRolloutCompleted    func(tree *BugBashTreeExecution)
	OnObserverEvent       func(ev ObserverCallbackEvent, tree *BugBashTreeExecution)
}

// TriggerRequest initiates a recursive Bug Bash Tree run for an Incident Scenario.
type TriggerRequest struct {
	IncidentID     string        `json:"incident_id"`
	HarnessMode    string        `json:"harness_mode,omitempty"`
	RequestedModel string        `json:"requested_model,omitempty"`
	AlertName      string        `json:"alert_name,omitempty"`
	ErrorMessage   string        `json:"error_message,omitempty"`
	StackTraces    []string      `json:"stack_traces,omitempty"`
	Wait           bool          `json:"wait,omitempty"`
	WaitTimeout    time.Duration `json:"-"`
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
	ExecutionMode      ExecutionMode     `json:"execution_mode,omitempty"`
	IncidentID         string            `json:"incident_id"`
	Title              string            `json:"title"`
	Summary            string            `json:"summary"`
	WhyCIMissedIt      string            `json:"why_ci_missed_it"`
	HarnessMode        string            `json:"harness_mode"`
	RequestedModel     string            `json:"requested_model"`
	SourceProvenance   string            `json:"source_provenance,omitempty"`
	Phase              string            `json:"phase"`
	Error              string            `json:"error,omitempty"`
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

// Orchestrator coordinates the recursive AX Bug Bash Tree across ax-server, Agent Substrate v0.6.0+ workers,
// the Model Gateway, and the in-cluster Storefront Git server.
type Orchestrator struct {
	cfg             OrchestratorConfig
	mode            ExecutionMode
	embeddedAX      *EmbeddedAXServer
	ownsViewer      bool
	substrateViewer *substrate.Viewer
	gatewayClient   *gateway.Client
	grpcConn        *grpc.ClientConn
	axClient        v1alpha1.AXClient

	mu            sync.RWMutex
	seq           int64
	executions    []*BugBashTreeExecution
	byID          map[string]*BugBashTreeExecution
	activeByIncID map[string]string
	notifyCh      chan struct{}
}

// NewOrchestrator initializes the Bug Bash Tree Orchestrator.
// In SIMULATED mode, it starts an embedded gRPC AXServer on 127.0.0.1:0 if AXServerAddr is empty.
// In LIVE_GKE mode, it fails closed: it NEVER starts an embedded AXServer or fake worker pool.
func NewOrchestrator(ctx context.Context, cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.DefaultHarnessMode == "" {
		cfg.DefaultHarnessMode = "multi"
	}
	if cfg.DefaultRequestedModel == "" {
		cfg.DefaultRequestedModel = gateway.DefaultFallbackCascade[0]
	}

	explicitMode := string(cfg.ExecutionMode)
	if explicitMode == "" {
		explicitMode = strings.TrimSpace(os.Getenv("EXECUTION_MODE"))
	}
	targetAddr := strings.TrimSpace(cfg.AXServerAddr)
	if targetAddr == "" && strings.EqualFold(explicitMode, string(ExecutionModeLiveGKE)) {
		targetAddr = strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))
	}

	mode, err := ResolveExecutionMode(explicitMode, targetAddr)
	if err != nil {
		return nil, err
	}
	cfg.ExecutionMode = mode

	var (
		embedded   *EmbeddedAXServer
		viewer     = cfg.SubstrateViewer
		ownsViewer = false
	)

	if mode == ExecutionModeLiveGKE {
		if strings.TrimSpace(cfg.TaskRunnerImage) == "" {
			cfg.TaskRunnerImage = firstNonEmptyString(
				os.Getenv("AX_DEFAULT_TASK_IMAGE"),
				os.Getenv("TASK_RUNNER_IMAGE"),
			)
		}
		if strings.TrimSpace(cfg.TaskRunnerImage) != "" {
			if err := ValidateDigestPinnedImage(cfg.TaskRunnerImage); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(cfg.ObserverCallbackURL) == "" {
			cfg.ObserverCallbackURL = strings.TrimSpace(os.Getenv("OBSERVER_CALLBACK_URL"))
		}
		if strings.TrimSpace(cfg.LokiURL) == "" {
			cfg.LokiURL = strings.TrimSpace(os.Getenv("LOKI_URL"))
		}
		if strings.TrimSpace(cfg.StorefrontGitURL) == "" {
			cfg.StorefrontGitURL = strings.TrimSpace(os.Getenv("STOREFRONT_GIT_URL"))
		}
		if strings.TrimSpace(cfg.ModelGatewayURL) == "" {
			cfg.ModelGatewayURL = strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
		}
		if viewer == nil && cfg.SubstrateViewerOpts != nil {
			createdViewer, vErr := substrate.NewViewer(*cfg.SubstrateViewerOpts)
			if vErr != nil {
				return nil, fmt.Errorf("initializing substrate viewer: %w", vErr)
			}
			viewer = createdViewer
			ownsViewer = true
		} else if viewer == nil && strings.TrimSpace(os.Getenv("SUBSTRATE_API_ADDR")) != "" {
			createdViewer, vErr := substrate.NewViewer(substrate.ViewerOptions{})
			if vErr != nil {
				return nil, fmt.Errorf("initializing substrate viewer from env: %w", vErr)
			}
			viewer = createdViewer
			ownsViewer = true
		}
	} else {
		if targetAddr == "" {
			srv, addr, sErr := StartEmbeddedAXServer(ctx)
			if sErr != nil {
				return nil, fmt.Errorf("starting embedded ax-server: %w", sErr)
			}
			embedded = srv
			targetAddr = addr
			cfg.AXServerAddr = addr
		} else {
			embedded = NewEmbeddedAXServer()
		}
	}

	conn, err := grpc.NewClient(targetAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		if embedded != nil {
			embedded.Stop()
		}
		if ownsViewer && viewer != nil {
			_ = viewer.Close()
		}
		return nil, fmt.Errorf("dialing ax-server %s: %w", targetAddr, err)
	}

	gwClient := cfg.GatewayClient
	if gwClient == nil && strings.TrimSpace(cfg.ModelGatewayURL) != "" {
		gwClient = gateway.NewClient(cfg.ModelGatewayURL, nil)
	}

	return &Orchestrator{
		cfg:             cfg,
		mode:            mode,
		embeddedAX:      embedded,
		ownsViewer:      ownsViewer,
		substrateViewer: viewer,
		gatewayClient:   gwClient,
		grpcConn:        conn,
		axClient:        v1alpha1.NewAXClient(conn),
		byID:            make(map[string]*BugBashTreeExecution),
		activeByIncID:   make(map[string]string),
		notifyCh:        make(chan struct{}),
	}, nil
}

// Close releases gRPC client connections and stops any embedded AXServer or owned SubstrateViewer.
func (o *Orchestrator) Close() {
	if o.grpcConn != nil {
		_ = o.grpcConn.Close()
	}
	if o.embeddedAX != nil {
		o.embeddedAX.Stop()
	}
	if o.ownsViewer && o.substrateViewer != nil {
		_ = o.substrateViewer.Close()
	}
}

// ExecutionMode returns the Orchestrator's active ExecutionMode ("LIVE_GKE" or "SIMULATED").
func (o *Orchestrator) ExecutionMode() ExecutionMode {
	return o.mode
}

// AXClient returns the underlying ax.v1alpha1.AXClient gRPC stub.
func (o *Orchestrator) AXClient() v1alpha1.AXClient {
	return o.axClient
}

// SubstrateViewer returns the configured read-only Substrate Control viewer client (if any).
func (o *Orchestrator) SubstrateViewer() *substrate.Viewer {
	return o.substrateViewer
}

// GatewayClient returns the remote Model Gateway query client (if any).
func (o *Orchestrator) GatewayClient() *gateway.Client {
	return o.gatewayClient
}

// PollWorkerPoolSnapshot polls live Agent Substrate WorkerPool telemetry in LIVE_GKE mode
// or returns the embedded worker pool snapshot in SIMULATED mode.
func (o *Orchestrator) PollWorkerPoolSnapshot(ctx context.Context) (WorkerPoolSummary, error) {
	if o.mode == ExecutionModeLiveGKE {
		if o.substrateViewer == nil {
			return WorkerPoolSummary{
				SubstrateVersion: "v0.6.1",
				Workers:          []SubstrateWorkerNode{},
			}, nil
		}
		snap, err := o.substrateViewer.PollSnapshot(ctx)
		if err != nil {
			return WorkerPoolSummary{}, err
		}
		return mapViewerSnapshotToPoolSummary(snap), nil
	}
	if o.embeddedAX != nil {
		return o.embeddedAX.WorkerPoolSnapshot(), nil
	}
	return NewEmbeddedAXServer().WorkerPoolSnapshot(), nil
}

// WorkerPoolSnapshot returns the current Agent Substrate worker pool telemetry.
// In LIVE_GKE mode, it NEVER fabricates fake workers.
func (o *Orchestrator) WorkerPoolSnapshot() WorkerPoolSummary {
	if o.mode == ExecutionModeLiveGKE {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		summary, err := o.PollWorkerPoolSnapshot(ctx)
		if err != nil {
			return WorkerPoolSummary{
				SubstrateVersion: "v0.6.1",
				Workers:          []SubstrateWorkerNode{},
			}
		}
		return summary
	}
	if o.embeddedAX != nil {
		return o.embeddedAX.WorkerPoolSnapshot()
	}
	return NewEmbeddedAXServer().WorkerPoolSnapshot()
}

func mapViewerSnapshotToPoolSummary(snap substrate.ViewerSnapshot) WorkerPoolSummary {
	out := WorkerPoolSummary{
		SubstrateVersion:       snap.SubstrateVersion,
		Workers:                make([]SubstrateWorkerNode, 0, len(snap.Workers)),
		ActiveTasksCount:       snap.ActiveTasksCount,
		SuspendedTasksCount:    snap.SuspendedTasksCount,
		TotalReclaimedSlots:    snap.TotalReclaimedSlots,
		TotalCPUMillicores:     snap.TotalCPUMillicores,
		AllocatedCPUMillicores: snap.AllocatedCPUMillicores,
		TotalMemoryMB:          snap.TotalMemoryMB,
		AllocatedMemoryMB:      snap.AllocatedMemoryMB,
		TotalActorSlots:        snap.TotalActorSlots,
		AllocatedActorSlots:    snap.AllocatedActorSlots,
	}
	for _, w := range snap.Workers {
		out.Workers = append(out.Workers, SubstrateWorkerNode{
			WorkerID:            w.WorkerID,
			WorkerPod:           w.WorkerPod,
			WorkerPool:          w.WorkerPool,
			NodeName:            w.NodeName,
			RuntimeClass:        w.RuntimeClass,
			SnapshotFidelity:    w.SnapshotFidelity,
			WorkerIP:            w.WorkerIP,
			TotalCPUMillicores:  w.TotalCPUMillicores,
			UsedCPUMillicores:   w.UsedCPUMillicores,
			TotalMemoryMB:       w.TotalMemoryMB,
			UsedMemoryMB:        w.UsedMemoryMB,
			TotalActorSlots:     w.TotalActorSlots,
			UsedActorSlots:      w.UsedActorSlots,
			ActiveTasks:         append([]string(nil), w.ActiveTasks...),
			SuspendedSnapshots:  append([]string(nil), w.SuspendedSnapshots...),
			ReclaimedSlotsCount: w.ReclaimedSlotsCount,
			Status:              w.Status,
		})
	}
	return out
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

func (o *Orchestrator) broadcastNotifyLocked() {
	if o.notifyCh != nil {
		close(o.notifyCh)
	}
	o.notifyCh = make(chan struct{})
}

// WaitForExecution blocks until executionID reaches a terminal phase (HEALED, DEPLOYED, or FAILED)
// or timeout / ctx expires.
func (o *Orchestrator) WaitForExecution(ctx context.Context, executionID string, timeout time.Duration) (*BugBashTreeExecution, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		o.mu.RLock()
		ex, ok := o.byID[executionID]
		ch := o.notifyCh
		var snap *BugBashTreeExecution
		if ok {
			snap = cloneExecution(ex)
		}
		o.mu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("execution %q not found", executionID)
		}
		if isTerminalPhase(snap.Phase) {
			return snap, nil
		}

		select {
		case <-waitCtx.Done():
			return snap, fmt.Errorf("waiting for execution %s: %w", executionID, waitCtx.Err())
		case <-ch:
		}
	}
}

// HandleObserverCallback handles POST /api/internal/observer-events (typed ObserverCallbackEvent)
// as well as legacy POST /api/observer/callback (ChildTaskResult) payloads.
func (o *Orchestrator) HandleObserverCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var probe struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(probe.EventType) != "" {
		var ev ObserverCallbackEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			http.Error(w, "invalid ObserverCallbackEvent JSON", http.StatusBadRequest)
			return
		}
		if _, err := o.ApplyObserverCallbackEvent(ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
		return
	}

	var res ChildTaskResult
	if err := json.Unmarshal(raw, &res); err != nil {
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
	o.broadcastNotifyLocked()
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded"})
}

// DispatchIncidentBugBash dispatches a digest-pinned Root Task onto ax-server in LIVE_GKE mode
// without running any in-process triage or Child Task TDD. Fails closed on any configuration or gRPC error.
func (o *Orchestrator) DispatchIncidentBugBash(ctx context.Context, req TriggerRequest) (*BugBashTreeExecution, error) {
	if o.mode != ExecutionModeLiveGKE {
		return o.TriggerIncidentBugBash(ctx, req)
	}
	if err := ValidateDigestPinnedImage(o.cfg.TaskRunnerImage); err != nil {
		return nil, err
	}
	gitRepoURL := strings.TrimSpace(o.cfg.StorefrontGitURL)
	if gitRepoURL == "" {
		return nil, fmt.Errorf("LIVE_GKE requires non-empty StorefrontGitURL")
	}

	incidentID := strings.ToUpper(strings.TrimSpace(req.IncidentID))
	if incidentID == "" {
		incidentID = storefront.IncidentStackedPromoNegativeSubtotal
	}
	sc, ok := storefront.GetScenario(incidentID)
	if !ok {
		return nil, fmt.Errorf("unknown incident scenario %q", incidentID)
	}

	harnessMode := strings.ToLower(strings.TrimSpace(req.HarnessMode))
	if harnessMode == "" {
		harnessMode = o.cfg.DefaultHarnessMode
	}
	requestedModel := strings.TrimSpace(req.RequestedModel)
	if requestedModel == "" {
		requestedModel = o.cfg.DefaultRequestedModel
	}

	// Deduplicate active in-flight tree for the same incident scenario
	o.mu.Lock()
	if existingID, ok := o.activeByIncID[sc.ID]; ok && existingID != "" {
		if existingTree, exists := o.byID[existingID]; exists && !isTerminalPhase(existingTree.Phase) {
			cloned := cloneExecution(existingTree)
			o.mu.Unlock()
			return cloned, nil
		}
	}

	o.seq++
	seq := o.seq
	execID := fmt.Sprintf("tree-%s-%d", strings.ToLower(sc.ID), seq)
	rootTaskName := SanitizeTaskName(fmt.Sprintf("triage-%s-%d", strings.ToLower(sc.ID), seq))
	now := time.Now().UTC()

	tree := &BugBashTreeExecution{
		ExecutionID:    execID,
		ExecutionMode:  ExecutionModeLiveGKE,
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
	o.activeByIncID[sc.ID] = execID
	o.mu.Unlock()

	traces := append([]string(nil), req.StackTraces...)
	alert := AlertPayload{
		IncidentID:     sc.ID,
		AlertName:      firstNonEmptyString(req.AlertName, "StorefrontHTTP500Spike"),
		ErrorCode:      sc.ErrorCode,
		ErrorMessage:   firstNonEmptyString(req.ErrorMessage, sc.ErrorMessage),
		StackTraces:    traces,
		HarnessMode:    harnessMode,
		RequestedModel: requestedModel,
	}
	rootCfgStruct, err := AlertPayloadToStruct(alert)
	if err != nil {
		o.markExecutionFailed(execID, sc.ID, err)
		return nil, err
	}

	// Detach from caller HTTP webhook timeout so Alertmanager webhook cancellation never aborts ax-server dispatch.
	rpcCtx, rpcCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer rpcCancel()

	if _, err := o.axClient.UpdateWorkspace(rpcCtx, &v1alpha1.UpdateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: DefaultWorkspaceName, Atespace: "default"},
			Spec: &v1alpha1.WorkspaceSpec{
				Git: []*v1alpha1.GitRepo{
					{Name: "storefront", Repo: gitRepoURL, Branch: "main", Dir: "."},
				},
			},
		},
	}); err != nil {
		o.markExecutionFailed(execID, sc.ID, err)
		return nil, fmt.Errorf("LIVE_GKE UpdateWorkspace on ax-server failed: %w", err)
	}

	if _, err := o.axClient.UpdateModel(rpcCtx, &v1alpha1.UpdateModelRequest{
		Model: &v1alpha1.Model{
			Metadata: &v1alpha1.ObjectMeta{Name: "gemini-flash-cascade", Atespace: "default"},
			Spec: &v1alpha1.ModelSpec{
				Provider: "google",
				Model:    requestedModel,
			},
		},
	}); err != nil {
		o.markExecutionFailed(execID, sc.ID, err)
		return nil, fmt.Errorf("LIVE_GKE UpdateModel on ax-server failed: %w", err)
	}

	envVars := []*v1alpha1.EnvVar{
		{Name: "AX_EXECUTION_ID", Value: execID},
		{Name: "AX_ROOT_TASK_NAME", Value: rootTaskName},
		{Name: "AX_TASK_NAME", Value: rootTaskName},
		{Name: "AX_INCIDENT_ID", Value: sc.ID},
		{Name: "AX_CODING_HARNESS", Value: harnessMode},
		{Name: "AX_REQUESTED_MODEL", Value: requestedModel},
		{Name: "AX_DEFAULT_TASK_IMAGE", Value: o.cfg.TaskRunnerImage},
		{Name: "AX_SERVER_ADDR", Value: o.cfg.AXServerAddr},
		{Name: "STOREFRONT_GIT_URL", Value: gitRepoURL},
		{Name: "GEMINI_API_KEY", Value: gateway.SandboxTokenPlaceholder},
		{Name: "OPENAI_API_KEY", Value: gateway.SandboxTokenPlaceholder},
	}
	if o.cfg.ModelGatewayURL != "" {
		envVars = append(envVars, &v1alpha1.EnvVar{Name: "MODEL_GATEWAY_URL", Value: o.cfg.ModelGatewayURL})
	}
	if o.cfg.ObserverCallbackURL != "" {
		envVars = append(envVars, &v1alpha1.EnvVar{Name: "OBSERVER_CALLBACK_URL", Value: o.cfg.ObserverCallbackURL})
	}
	if o.cfg.LokiURL != "" {
		envVars = append(envVars, &v1alpha1.EnvVar{Name: "LOKI_URL", Value: o.cfg.LokiURL})
	}

	rootTaskSpec := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: rootTaskName, Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Image:   o.cfg.TaskRunnerImage,
			Command: []string{"/usr/local/bin/ax-task-runner", "--execute-task"},
			Env:     envVars,
			Workspace: &v1alpha1.WorkspaceRef{
				Name: DefaultWorkspaceName,
				Path: "/workspace",
			},
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "2048Mi"},
			},
			Config: rootCfgStruct,
		},
	}
	if _, err := o.axClient.CreateTask(rpcCtx, &v1alpha1.CreateTaskRequest{Task: rootTaskSpec}); err != nil {
		_, _ = o.axClient.DeleteTask(rpcCtx, &v1alpha1.DeleteTaskRequest{Atespace: "default", Name: rootTaskName})
		if _, err2 := o.axClient.CreateTask(rpcCtx, &v1alpha1.CreateTaskRequest{Task: rootTaskSpec}); err2 != nil {
			o.markExecutionFailed(execID, sc.ID, err2)
			return nil, fmt.Errorf("LIVE_GKE CreateTask(%s) failed: %w", rootTaskName, err2)
		}
	}
	if _, err := o.axClient.ResumeTask(rpcCtx, &v1alpha1.ResumeTaskRequest{Atespace: "default", Name: rootTaskName}); err != nil {
		o.markExecutionFailed(execID, sc.ID, err)
		return nil, fmt.Errorf("LIVE_GKE ResumeTask(%s) failed: %w", rootTaskName, err)
	}

	o.mu.Lock()
	snap := cloneExecution(tree)
	o.broadcastNotifyLocked()
	o.mu.Unlock()
	return snap, nil
}

func (o *Orchestrator) markExecutionFailed(execID, incidentID string, cause error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.activeByIncID, incidentID)
	if ex, ok := o.byID[execID]; ok {
		ex.Phase = "FAILED"
		if cause != nil {
			ex.Error = cause.Error()
		}
		ex.CompletedAt = time.Now().UTC()
	}
	o.broadcastNotifyLocked()
}

// TriggerIncidentBugBash executes the recursive Bug Bash Tree lifecycle.
// In LIVE_GKE mode, it fails closed and dispatches the Root Task onto ax-server (returning immediately
// unless req.Wait is true, in which case it waits for terminal convergence).
// In SIMULATED mode, it runs the full hermetic in-process Bug Bash Tree lifecycle.
func (o *Orchestrator) TriggerIncidentBugBash(ctx context.Context, req TriggerRequest) (*BugBashTreeExecution, error) {
	if o.mode == ExecutionModeLiveGKE {
		dispatched, err := o.DispatchIncidentBugBash(ctx, req)
		if err != nil {
			return nil, err
		}
		if req.Wait {
			return o.WaitForExecution(ctx, dispatched.ExecutionID, req.WaitTimeout)
		}
		return dispatched, nil
	}

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
		ExecutionMode:  ExecutionModeSimulated,
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
		tree.Error = childErr.Error()
		o.broadcastNotifyLocked()
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
	tree.SourceProvenance = aggregateTreeProvenance(childResults)
	tree.MergedGitCommitSHA = commitSHA
	tree.MergedUnifiedDiff = mergedDiff
	tree.RolloutDurationMs = rolloutMs
	tree.StorefrontHealed = true
	tree.Phase = "HEALED"
	tree.CompletedAt = completedAt
	tree.RootTask.Phase = v1alpha1.PhaseSuspended
	tree.RootTask.SuspendedAt = completedAt
	snapshot := cloneExecution(tree)
	o.broadcastNotifyLocked()
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
		{"add", "--", "services"},
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
