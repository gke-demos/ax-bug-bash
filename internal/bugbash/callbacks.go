package bugbash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// TDDStepRecord aliases TDDStepEvent so ObserverCallbackEvent consumers can reference either name.
type TDDStepRecord = TDDStepEvent

// Canonical ObserverCallbackEvent event_type values streamed by Root and Child sandboxes.
const (
	ObserverEventRootStarted    = "root.started"
	ObserverEventRootTriaged    = "root.triaged"
	ObserverEventChildSpawned   = "child.spawned"
	ObserverEventChildStep      = "child.step"
	ObserverEventChildCompleted = "child.completed"
	ObserverEventRootMerged     = "root.merged"
	ObserverEventRootFailed     = "root.failed"
)

// ObserverCallbackEvent is the typed telemetry event streamed from unprivileged Root and Child Task
// sandboxes to AX Observer (/api/internal/observer-events or /api/observer/callback).
type ObserverCallbackEvent struct {
	ExecutionID        string           `json:"execution_id"`
	RootTaskName       string           `json:"root_task_name"`
	TaskName           string           `json:"task_name,omitempty"`
	Service            string           `json:"service,omitempty"`
	EventType          string           `json:"event_type"`
	IncidentID         string           `json:"incident_id"`
	HarnessMode        string           `json:"harness_mode,omitempty"`
	RequestedModel     string           `json:"requested_model,omitempty"`
	SourceProvenance   string           `json:"source_provenance,omitempty"`
	ImplicatedServices []string         `json:"implicated_services,omitempty"`
	UnaffectedServices []string         `json:"unaffected_services,omitempty"`
	Child              *ChildTaskResult `json:"child,omitempty"`
	Step               *TDDStepRecord   `json:"step,omitempty"`
	MergedCommitSHA    string           `json:"merged_commit_sha,omitempty"`
	MergedUnifiedDiff  string           `json:"merged_unified_diff,omitempty"`
	RolloutDurationMs  int64            `json:"rollout_duration_ms,omitempty"`
	StorefrontHealed   bool             `json:"storefront_healed,omitempty"`
	Error              string           `json:"error,omitempty"`
	Timestamp          time.Time        `json:"timestamp"`
}

// ValidateObserverCallbackEvent validates untrusted ObserverCallbackEvent inputs against strict allow-lists.
func ValidateObserverCallbackEvent(ev ObserverCallbackEvent) error {
	switch strings.TrimSpace(ev.EventType) {
	case ObserverEventRootStarted,
		ObserverEventRootTriaged,
		ObserverEventChildSpawned,
		ObserverEventChildStep,
		ObserverEventChildCompleted,
		ObserverEventRootMerged,
		ObserverEventRootFailed:
	default:
		return fmt.Errorf("unsupported observer event_type %q", ev.EventType)
	}

	if strings.TrimSpace(ev.ExecutionID) == "" &&
		strings.TrimSpace(ev.RootTaskName) == "" &&
		(ev.Child == nil || strings.TrimSpace(ev.Child.TaskName) == "") {
		return fmt.Errorf("execution_id, root_task_name, or child.task_name is required")
	}

	if svc := strings.TrimSpace(ev.Service); svc != "" && !isAllowedStorefrontService(svc) {
		return fmt.Errorf("unknown service %q", svc)
	}
	if ev.Child != nil && strings.TrimSpace(ev.Child.Service) != "" && !isAllowedStorefrontService(ev.Child.Service) {
		return fmt.Errorf("unknown child service %q", ev.Child.Service)
	}
	return nil
}

func isAllowedStorefrontService(svc string) bool {
	for _, known := range storefront.AllServices() {
		if svc == known {
			return true
		}
	}
	return false
}

// PostObserverEvent sends a typed ObserverCallbackEvent over HTTP POST to callbackURL.
func PostObserverEvent(ctx context.Context, callbackURL string, ev ObserverCallbackEvent) error {
	trimmedURL := strings.TrimSpace(callbackURL)
	if trimmedURL == "" {
		return nil
	}
	if u, err := url.Parse(trimmedURL); err == nil && (u.Path == "" || u.Path == "/") {
		u.Path = "/api/internal/observer-events"
		trimmedURL = u.String()
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshaling observer callback event: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trimmedURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", trimmedURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("POST %s returned %d: %s", trimmedURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ApplyObserverCallbackEvent idempotently merges a validated ObserverCallbackEvent into the Orchestrator's
// BugBashTreeExecution ledger and notifies any WaitForExecution callers when a terminal state is reached.
func (o *Orchestrator) ApplyObserverCallbackEvent(ev ObserverCallbackEvent) (*BugBashTreeExecution, error) {
	if err := ValidateObserverCallbackEvent(ev); err != nil {
		return nil, err
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	var (
		snapshot        *BugBashTreeExecution
		rolloutCallback func(tree *BugBashTreeExecution)
		observerHook    func(ev ObserverCallbackEvent, tree *BugBashTreeExecution)
	)

	o.mu.Lock()
	tree := o.findOrCreateTreeLocked(ev)
	if tree == nil {
		o.mu.Unlock()
		return nil, fmt.Errorf("no matching BugBashTreeExecution found for event %q (exec=%q root=%q)", ev.EventType, ev.ExecutionID, ev.RootTaskName)
	}

	switch ev.EventType {
	case ObserverEventRootStarted:
		if !isTerminalPhase(tree.Phase) && tree.Phase == "" {
			tree.Phase = "TRIAGING"
		}
		if tree.RootTask.Phase == "" {
			tree.RootTask.Phase = v1alpha1.PhaseRunning
		}
		if tree.RootTask.StartedAt.IsZero() {
			tree.RootTask.StartedAt = ev.Timestamp
		}

	case ObserverEventRootTriaged:
		if !isTerminalPhase(tree.Phase) {
			tree.Phase = "FANOUT_TDD"
		}
		if len(ev.ImplicatedServices) > 0 {
			tree.ImplicatedServices = append([]string(nil), ev.ImplicatedServices...)
			tree.RootTask.ImplicatedServices = append([]string(nil), ev.ImplicatedServices...)
		}
		if len(ev.UnaffectedServices) > 0 {
			tree.UnaffectedServices = append([]string(nil), ev.UnaffectedServices...)
			tree.RootTask.UnaffectedServices = append([]string(nil), ev.UnaffectedServices...)
		}

	case ObserverEventChildSpawned:
		if !isTerminalPhase(tree.Phase) && tree.Phase == "TRIAGING" {
			tree.Phase = "FANOUT_TDD"
		}
		if ev.Child != nil && ev.Child.TaskName != "" {
			upsertChildTaskLocked(tree, *ev.Child)
			if !containsString(tree.RootTask.SpawnedChildren, ev.Child.TaskName) {
				tree.RootTask.SpawnedChildren = append(tree.RootTask.SpawnedChildren, ev.Child.TaskName)
			}
		}

	case ObserverEventChildStep:
		cName := strings.TrimSpace(ev.TaskName)
		if cName == "" && ev.Child != nil {
			cName = strings.TrimSpace(ev.Child.TaskName)
		}
		if cName != "" && ev.Step != nil {
			idx := findChildIndexLocked(tree, cName)
			if idx < 0 {
				placeholder := ChildTaskResult{
					TaskName:   cName,
					ParentTask: tree.RootTask.TaskName,
					IncidentID: tree.IncidentID,
					Service:    ev.Service,
				}
				tree.ChildTasks = append(tree.ChildTasks, placeholder)
				idx = len(tree.ChildTasks) - 1
			}
			appendStepIdempotent(&tree.ChildTasks[idx], *ev.Step)
		}

	case ObserverEventChildCompleted:
		if ev.Child != nil && ev.Child.TaskName != "" {
			upsertChildTaskLocked(tree, *ev.Child)
			if !containsString(tree.RootTask.SpawnedChildren, ev.Child.TaskName) {
				tree.RootTask.SpawnedChildren = append(tree.RootTask.SpawnedChildren, ev.Child.TaskName)
			}
			tree.SourceProvenance = aggregateTreeProvenance(tree.ChildTasks)
		}

	case ObserverEventRootMerged:
		wasTerminal := isTerminalPhase(tree.Phase)
		if ev.MergedCommitSHA != "" {
			tree.MergedGitCommitSHA = ev.MergedCommitSHA
		}
		if ev.MergedUnifiedDiff != "" {
			tree.MergedUnifiedDiff = ev.MergedUnifiedDiff
		}
		if ev.RolloutDurationMs > 0 {
			tree.RolloutDurationMs = ev.RolloutDurationMs
		} else if !tree.StartedAt.IsZero() && tree.RolloutDurationMs == 0 {
			dur := ev.Timestamp.Sub(tree.StartedAt).Milliseconds()
			if dur <= 0 {
				dur = 1
			}
			tree.RolloutDurationMs = dur
		}
		if ev.SourceProvenance != "" {
			tree.SourceProvenance = ev.SourceProvenance
		} else if tree.SourceProvenance == "" {
			tree.SourceProvenance = aggregateTreeProvenance(tree.ChildTasks)
		}
		tree.StorefrontHealed = ev.StorefrontHealed || ev.MergedCommitSHA != ""
		tree.Phase = "HEALED"
		tree.CompletedAt = ev.Timestamp
		tree.RootTask.Phase = v1alpha1.PhaseSuspended
		tree.RootTask.SuspendedAt = ev.Timestamp
		delete(o.activeByIncID, tree.IncidentID)
		if !wasTerminal {
			rolloutCallback = o.cfg.OnRolloutCompleted
		}

	case ObserverEventRootFailed:
		tree.Phase = "FAILED"
		tree.Error = ev.Error
		tree.StorefrontHealed = false
		tree.CompletedAt = ev.Timestamp
		tree.RootTask.Phase = v1alpha1.PhaseSuspended
		tree.RootTask.SuspendedAt = ev.Timestamp
		delete(o.activeByIncID, tree.IncidentID)
	}

	snapshot = cloneExecution(tree)
	observerHook = o.cfg.OnObserverEvent
	o.broadcastNotifyLocked()
	o.mu.Unlock()

	if observerHook != nil {
		observerHook(ev, snapshot)
	}
	if rolloutCallback != nil {
		rolloutCallback(snapshot)
	}
	return snapshot, nil
}

func (o *Orchestrator) findOrCreateTreeLocked(ev ObserverCallbackEvent) *BugBashTreeExecution {
	if id := strings.TrimSpace(ev.ExecutionID); id != "" {
		if ex, ok := o.byID[id]; ok {
			return ex
		}
	}
	rootName := strings.TrimSpace(ev.RootTaskName)
	if rootName == "" && ev.Child != nil {
		rootName = strings.TrimSpace(ev.Child.ParentTask)
	}
	if rootName != "" {
		for i := len(o.executions) - 1; i >= 0; i-- {
			if o.executions[i].RootTask.TaskName == rootName {
				return o.executions[i]
			}
		}
	}
	if ev.Child != nil && ev.Child.TaskName != "" {
		for i := len(o.executions) - 1; i >= 0; i-- {
			if findChildIndexLocked(o.executions[i], ev.Child.TaskName) >= 0 {
				return o.executions[i]
			}
		}
	}
	incID := strings.ToUpper(strings.TrimSpace(ev.IncidentID))
	if incID != "" {
		for i := len(o.executions) - 1; i >= 0; i-- {
			if strings.EqualFold(o.executions[i].IncidentID, incID) && !isTerminalPhase(o.executions[i].Phase) {
				return o.executions[i]
			}
		}
		if rootName != "" {
			sc, _ := storefront.GetScenario(incID)
			execID := strings.TrimSpace(ev.ExecutionID)
			if execID == "" {
				o.seq++
				execID = fmt.Sprintf("tree-%s-%d", strings.ToLower(incID), o.seq)
			}
			created := &BugBashTreeExecution{
				ExecutionID:    execID,
				ExecutionMode:  o.mode,
				IncidentID:     incID,
				Title:          sc.Title,
				Summary:        sc.Summary,
				WhyCIMissedIt:  sc.WhyCIMissedIt,
				HarnessMode:    firstNonEmptyString(ev.HarnessMode, o.cfg.DefaultHarnessMode),
				RequestedModel: firstNonEmptyString(ev.RequestedModel, o.cfg.DefaultRequestedModel),
				Phase:          "TRIAGING",
				RootTask: RootTaskNode{
					TaskName:  rootName,
					Phase:     v1alpha1.PhaseRunning,
					StartedAt: ev.Timestamp,
				},
				StartedAt: ev.Timestamp,
			}
			o.executions = append(o.executions, created)
			o.byID[execID] = created
			return created
		}
	}
	return nil
}

func findChildIndexLocked(tree *BugBashTreeExecution, taskName string) int {
	for i := range tree.ChildTasks {
		if tree.ChildTasks[i].TaskName == taskName {
			return i
		}
	}
	return -1
}

func upsertChildTaskLocked(tree *BugBashTreeExecution, incoming ChildTaskResult) {
	idx := findChildIndexLocked(tree, incoming.TaskName)
	if idx < 0 {
		tree.ChildTasks = append(tree.ChildTasks, incoming)
		return
	}
	existing := tree.ChildTasks[idx]
	if len(incoming.TDDSteps) == 0 && len(existing.TDDSteps) > 0 {
		incoming.TDDSteps = existing.TDDSteps
	} else if len(existing.TDDSteps) > 0 {
		merged := append([]TDDStepEvent(nil), existing.TDDSteps...)
		tmp := ChildTaskResult{TDDSteps: merged}
		for _, st := range incoming.TDDSteps {
			appendStepIdempotent(&tmp, st)
		}
		incoming.TDDSteps = tmp.TDDSteps
	}
	if incoming.ParentTask == "" {
		incoming.ParentTask = existing.ParentTask
	}
	if incoming.IncidentID == "" {
		incoming.IncidentID = existing.IncidentID
	}
	if incoming.Service == "" {
		incoming.Service = existing.Service
	}
	if incoming.Language == "" {
		incoming.Language = existing.Language
	}
	if incoming.Harness == "" {
		incoming.Harness = existing.Harness
	}
	if incoming.RequestedModel == "" {
		incoming.RequestedModel = existing.RequestedModel
	}
	if incoming.RegressionTestName == "" {
		incoming.RegressionTestName = existing.RegressionTestName
	}
	tree.ChildTasks[idx] = incoming
}

func appendStepIdempotent(child *ChildTaskResult, step TDDStepEvent) {
	for _, existing := range child.TDDSteps {
		if existing.Phase == step.Phase && existing.Summary == step.Summary && existing.Status == step.Status {
			return
		}
	}
	child.TDDSteps = append(child.TDDSteps, step)
}

func aggregateTreeProvenance(children []ChildTaskResult) string {
	if len(children) == 0 {
		return ""
	}
	hasLive := false
	for _, c := range children {
		if strings.EqualFold(c.SourceProvenance, "FIXTURE") {
			return "FIXTURE"
		}
		if strings.EqualFold(c.SourceProvenance, "LIVE") {
			hasLive = true
		}
	}
	if hasLive {
		return "LIVE"
	}
	return ""
}

func isTerminalPhase(phase string) bool {
	switch strings.ToUpper(strings.TrimSpace(phase)) {
	case "HEALED", "DEPLOYED", "FAILED":
		return true
	default:
		return false
	}
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
