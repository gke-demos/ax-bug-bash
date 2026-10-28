package bugbash

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SubstrateWorkerNode represents an Agent Substrate v0.6.0 ateworker-gvisor warm sandbox node.
type SubstrateWorkerNode struct {
	WorkerID            string   `json:"worker_id"`
	RuntimeClass        string   `json:"runtime_class"`
	SnapshotFidelity    string   `json:"snapshot_fidelity"`
	WorkerIP            string   `json:"worker_ip"`
	TotalCPUMillicores  int      `json:"total_cpu_millicores"`
	UsedCPUMillicores   int      `json:"used_cpu_millicores"`
	TotalMemoryMB       int      `json:"total_memory_mb"`
	UsedMemoryMB        int      `json:"used_memory_mb"`
	ActiveTasks         []string `json:"active_tasks"`
	SuspendedSnapshots  []string `json:"suspended_snapshots"`
	ReclaimedSlotsCount int      `json:"reclaimed_slots_count"`
	Status              string   `json:"status"`
}

// WorkerPoolSummary captures the aggregate state of the Agent Substrate v0.6.0 worker pool.
type WorkerPoolSummary struct {
	SubstrateVersion       string                `json:"substrate_version"`
	Workers                []SubstrateWorkerNode `json:"workers"`
	ActiveTasksCount       int                   `json:"active_tasks_count"`
	SuspendedTasksCount    int                   `json:"suspended_tasks_count"`
	TotalReclaimedSlots    int                   `json:"total_reclaimed_slots"`
	TotalCPUMillicores     int                   `json:"total_cpu_millicores"`
	AllocatedCPUMillicores int                   `json:"allocated_cpu_millicores"`
	TotalMemoryMB          int                   `json:"total_memory_mb"`
	AllocatedMemoryMB      int                   `json:"allocated_memory_mb"`
}

type taskPlacement struct {
	workerIdx int
	cpuMilli  int
	memoryMB  int
	active    bool
}

// EmbeddedAXServer implements ax.v1alpha1.AXServer over gRPC and tracks Agent Substrate v0.6.0 worker slots.
type EmbeddedAXServer struct {
	v1alpha1.UnimplementedAXServer

	mu         sync.RWMutex
	grpcSrv    *grpc.Server
	lis        net.Listener
	addr       string
	tasks      map[string]*v1alpha1.Task
	workspaces map[string]*v1alpha1.Workspace
	models     map[string]*v1alpha1.Model
	workers    []SubstrateWorkerNode
	placements map[string]*taskPlacement
	rrCursor   int

	onTaskResumed   func(task *v1alpha1.Task, workerID string)
	onTaskSuspended func(task *v1alpha1.Task, workerID string, suspendedAt time.Time)
}

// NewEmbeddedAXServer creates a new in-memory AXServer with 4 warm ateworker-gvisor nodes.
func NewEmbeddedAXServer() *EmbeddedAXServer {
	workers := make([]SubstrateWorkerNode, 4)
	for i := 0; i < 4; i++ {
		workers[i] = SubstrateWorkerNode{
			WorkerID:           fmt.Sprintf("ateworker-gvisor-0%d", i+1),
			RuntimeClass:       "gvisor",
			SnapshotFidelity:   "SNAPSHOT_FIDELITY_VOLUMES",
			WorkerIP:           fmt.Sprintf("10.244.1.%d", 11+i),
			TotalCPUMillicores: 4000,
			UsedCPUMillicores:  0,
			TotalMemoryMB:      8192,
			UsedMemoryMB:       0,
			ActiveTasks:        []string{},
			SuspendedSnapshots: []string{},
			Status:             "WARM_IDLE",
		}
	}
	return &EmbeddedAXServer{
		tasks:      make(map[string]*v1alpha1.Task),
		workspaces: make(map[string]*v1alpha1.Workspace),
		models:     make(map[string]*v1alpha1.Model),
		workers:    workers,
		placements: make(map[string]*taskPlacement),
	}
}

// StartEmbeddedAXServer starts a real gRPC listener serving ax.v1alpha1.AX on 127.0.0.1:0.
func StartEmbeddedAXServer(ctx context.Context) (*EmbeddedAXServer, string, error) {
	srv := NewEmbeddedAXServer()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen tcp for embedded ax-server: %w", err)
	}
	grpcSrv := grpc.NewServer()
	v1alpha1.RegisterAXServer(grpcSrv, srv)

	srv.grpcSrv = grpcSrv
	srv.lis = lis
	srv.addr = lis.Addr().String()

	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()

	return srv, srv.addr, nil
}

// Addr returns the bound TCP address of the embedded gRPC server.
func (s *EmbeddedAXServer) Addr() string {
	return s.addr
}

// Stop gracefully stops the gRPC server.
func (s *EmbeddedAXServer) Stop() {
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	if s.lis != nil {
		_ = s.lis.Close()
	}
}

// SetLifecycleHooks registers optional callbacks invoked when tasks resume or suspend.
func (s *EmbeddedAXServer) SetLifecycleHooks(
	onResumed func(task *v1alpha1.Task, workerID string),
	onSuspended func(task *v1alpha1.Task, workerID string, suspendedAt time.Time),
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTaskResumed = onResumed
	s.onTaskSuspended = onSuspended
}

// WorkerPoolSnapshot returns a thread-safe snapshot of the 4 Agent Substrate v0.6.0 workers.
func (s *EmbeddedAXServer) WorkerPoolSnapshot() WorkerPoolSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summary := WorkerPoolSummary{
		SubstrateVersion: "v0.6.0",
		Workers:          make([]SubstrateWorkerNode, len(s.workers)),
	}
	for i, w := range s.workers {
		copied := w
		copied.ActiveTasks = append([]string(nil), w.ActiveTasks...)
		copied.SuspendedSnapshots = append([]string(nil), w.SuspendedSnapshots...)
		summary.Workers[i] = copied

		summary.ActiveTasksCount += len(w.ActiveTasks)
		summary.SuspendedTasksCount += len(w.SuspendedSnapshots)
		summary.TotalReclaimedSlots += w.ReclaimedSlotsCount
		summary.TotalCPUMillicores += w.TotalCPUMillicores
		summary.AllocatedCPUMillicores += w.UsedCPUMillicores
		summary.TotalMemoryMB += w.TotalMemoryMB
		summary.AllocatedMemoryMB += w.UsedMemoryMB
	}
	return summary
}

func resourceKey(atespace, name string) string {
	if strings.TrimSpace(atespace) == "" {
		atespace = "default"
	}
	return atespace + "/" + name
}

// CreateTask implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) CreateTask(_ context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	if req.GetTask() == nil || req.GetTask().GetMetadata() == nil {
		return nil, status.Error(codes.InvalidArgument, "task metadata is required")
	}
	t := proto.Clone(req.GetTask()).(*v1alpha1.Task)
	if t.ApiVersion == "" {
		t.ApiVersion = v1alpha1.APIVersion
	}
	if t.Kind == "" {
		t.Kind = v1alpha1.KindTask
	}
	if t.Metadata.Atespace == "" {
		t.Metadata.Atespace = "default"
	}
	if t.Metadata.CreateTime == nil {
		t.Metadata.CreateTime = timestamppb.Now()
	}
	if err := v1alpha1.ValidateTask(t); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	key := resourceKey(t.Metadata.Atespace, t.Metadata.Name)
	s.mu.Lock()
	defer s.mu.Unlock()

	if t.Status == nil {
		t.Status = &v1alpha1.TaskStatus{
			Phase: v1alpha1.PhasePending,
			Id:    "task-" + t.Metadata.Name,
		}
	}
	s.tasks[key] = t
	return proto.Clone(t).(*v1alpha1.Task), nil
}

// ResumeTask implements ax.v1alpha1.AXServer and allocates CPU/memory on an ateworker-gvisor node.
func (s *EmbeddedAXServer) ResumeTask(_ context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())

	s.mu.Lock()
	t, ok := s.tasks[key]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %q not found", key)
	}

	placement, hasPlacement := s.placements[key]
	if !hasPlacement {
		workerIdx := s.rrCursor % len(s.workers)
		s.rrCursor++
		cpuMilli, memMB := parseTaskResources(t.GetSpec().GetResources())
		placement = &taskPlacement{
			workerIdx: workerIdx,
			cpuMilli:  cpuMilli,
			memoryMB:  memMB,
		}
		s.placements[key] = placement
	}

	w := &s.workers[placement.workerIdx]
	if !placement.active {
		placement.active = true
		w.UsedCPUMillicores += placement.cpuMilli
		w.UsedMemoryMB += placement.memoryMB
		if !containsString(w.ActiveTasks, t.Metadata.Name) {
			w.ActiveTasks = append(w.ActiveTasks, t.Metadata.Name)
		}
		w.SuspendedSnapshots = removeString(w.SuspendedSnapshots, t.Metadata.Name)
		w.Status = "ACTIVE_SANDBOX"
	}

	now := timestamppb.Now()
	t.Status = &v1alpha1.TaskStatus{
		Phase:    v1alpha1.PhaseRunning,
		Id:       "task-" + t.Metadata.Name,
		Actor:    "actor-" + t.Metadata.Name,
		WorkerIp: w.WorkerIP,
		Conditions: []*v1alpha1.Condition{
			{
				Type:       v1alpha1.ConditionWorkspaceReady,
				Status:     "True",
				UpdateTime: now,
				Reason:     v1alpha1.ReasonSetupComplete,
				Message:    "Workspace maiden run initialized from in-cluster Git server",
			},
			{
				Type:       v1alpha1.ConditionReady,
				Status:     "True",
				UpdateTime: now,
				Reason:     v1alpha1.ReasonTaskRunning,
				Message:    fmt.Sprintf("Running on Agent Substrate v0.6.0 worker %s", w.WorkerID),
			},
		},
	}

	cloned := proto.Clone(t).(*v1alpha1.Task)
	workerID := w.WorkerID
	hook := s.onTaskResumed
	s.mu.Unlock()

	if hook != nil {
		hook(cloned, workerID)
	}
	return cloned, nil
}

// SuspendTask implements ax.v1alpha1.AXServer and immediately reclaims the ateworker-gvisor CPU/memory slot.
func (s *EmbeddedAXServer) SuspendTask(_ context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())

	s.mu.Lock()
	t, ok := s.tasks[key]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %q not found", key)
	}

	workerID := "ateworker-gvisor-01"
	suspendedAt := time.Now().UTC()
	if placement, exists := s.placements[key]; exists {
		w := &s.workers[placement.workerIdx]
		workerID = w.WorkerID
		if placement.active {
			placement.active = false
			w.UsedCPUMillicores -= placement.cpuMilli
			if w.UsedCPUMillicores < 0 {
				w.UsedCPUMillicores = 0
			}
			w.UsedMemoryMB -= placement.memoryMB
			if w.UsedMemoryMB < 0 {
				w.UsedMemoryMB = 0
			}
			w.ActiveTasks = removeString(w.ActiveTasks, t.Metadata.Name)
			if !containsString(w.SuspendedSnapshots, t.Metadata.Name) {
				w.SuspendedSnapshots = append(w.SuspendedSnapshots, t.Metadata.Name)
			}
			w.ReclaimedSlotsCount++
			if len(w.ActiveTasks) == 0 {
				w.Status = "RECLAIMED_WARM"
			}
		}
	}

	now := timestamppb.New(suspendedAt)
	if t.Status == nil {
		t.Status = &v1alpha1.TaskStatus{
			Id:    "task-" + t.Metadata.Name,
			Actor: "actor-" + t.Metadata.Name,
		}
	}
	t.Status.Phase = v1alpha1.PhaseSuspended
	t.Status.Conditions = []*v1alpha1.Condition{
		{
			Type:       v1alpha1.ConditionWorkspaceReady,
			Status:     "True",
			UpdateTime: now,
			Reason:     v1alpha1.ReasonSetupComplete,
			Message:    "Volume state preserved in /workspace/.ax-state (SNAPSHOT_FIDELITY_VOLUMES)",
		},
		{
			Type:       v1alpha1.ConditionReady,
			Status:     "False",
			UpdateTime: now,
			Reason:     v1alpha1.PhaseSuspended,
			Message:    fmt.Sprintf("Task suspended; CPU and memory slot reclaimed on %s", workerID),
		},
	}

	cloned := proto.Clone(t).(*v1alpha1.Task)
	hook := s.onTaskSuspended
	s.mu.Unlock()

	if hook != nil {
		hook(cloned, workerID, suspendedAt)
	}
	return cloned, nil
}

// GetTask implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) GetTask(_ context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.tasks[key]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %q not found", key)
	}
	return proto.Clone(t).(*v1alpha1.Task), nil
}

// ListTasks implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) ListTasks(_ context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	atespace := strings.TrimSpace(req.GetAtespace())
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*v1alpha1.Task
	for _, t := range s.tasks {
		if atespace != "" && t.GetMetadata().GetAtespace() != atespace {
			continue
		}
		out = append(out, proto.Clone(t).(*v1alpha1.Task))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetMetadata().GetName() < out[j].GetMetadata().GetName()
	})
	return &v1alpha1.ListTasksResponse{Tasks: out}, nil
}

// DeleteTask implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) DeleteTask(ctx context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	_, _ = s.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
		Atespace: req.GetAtespace(),
		Name:     req.GetName(),
	})
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.Lock()
	delete(s.tasks, key)
	delete(s.placements, key)
	s.mu.Unlock()
	return &v1alpha1.DeleteTaskResponse{}, nil
}

// UpdateWorkspace implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) UpdateWorkspace(_ context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	if req.GetWorkspace() == nil || req.GetWorkspace().GetMetadata() == nil {
		return nil, status.Error(codes.InvalidArgument, "workspace metadata is required")
	}
	ws := proto.Clone(req.GetWorkspace()).(*v1alpha1.Workspace)
	if ws.ApiVersion == "" {
		ws.ApiVersion = v1alpha1.APIVersion
	}
	if ws.Kind == "" {
		ws.Kind = v1alpha1.KindWorkspace
	}
	if ws.Metadata.Atespace == "" {
		ws.Metadata.Atespace = "default"
	}
	if ws.Metadata.CreateTime == nil {
		ws.Metadata.CreateTime = timestamppb.Now()
	}
	if err := v1alpha1.ValidateWorkspace(ws); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	key := resourceKey(ws.Metadata.Atespace, ws.Metadata.Name)
	s.mu.Lock()
	s.workspaces[key] = ws
	s.mu.Unlock()
	return proto.Clone(ws).(*v1alpha1.Workspace), nil
}

// GetWorkspace implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) GetWorkspace(_ context.Context, req *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.RLock()
	defer s.mu.RUnlock()

	ws, ok := s.workspaces[key]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "workspace %q not found", key)
	}
	return proto.Clone(ws).(*v1alpha1.Workspace), nil
}

// ListWorkspaces implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) ListWorkspaces(_ context.Context, req *v1alpha1.ListWorkspacesRequest) (*v1alpha1.ListWorkspacesResponse, error) {
	atespace := strings.TrimSpace(req.GetAtespace())
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*v1alpha1.Workspace
	for _, ws := range s.workspaces {
		if atespace != "" && ws.GetMetadata().GetAtespace() != atespace {
			continue
		}
		out = append(out, proto.Clone(ws).(*v1alpha1.Workspace))
	}
	return &v1alpha1.ListWorkspacesResponse{Workspaces: out}, nil
}

// DeleteWorkspace implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) DeleteWorkspace(_ context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.Lock()
	delete(s.workspaces, key)
	s.mu.Unlock()
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

// UpdateModel implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) UpdateModel(_ context.Context, req *v1alpha1.UpdateModelRequest) (*v1alpha1.Model, error) {
	if req.GetModel() == nil || req.GetModel().GetMetadata() == nil {
		return nil, status.Error(codes.InvalidArgument, "model metadata is required")
	}
	m := proto.Clone(req.GetModel()).(*v1alpha1.Model)
	if m.ApiVersion == "" {
		m.ApiVersion = v1alpha1.APIVersion
	}
	if m.Kind == "" {
		m.Kind = v1alpha1.KindModel
	}
	if m.Metadata.Atespace == "" {
		m.Metadata.Atespace = "default"
	}
	if m.Metadata.CreateTime == nil {
		m.Metadata.CreateTime = timestamppb.Now()
	}
	if err := v1alpha1.ValidateModel(m); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	key := resourceKey(m.Metadata.Atespace, m.Metadata.Name)
	s.mu.Lock()
	s.models[key] = m
	s.mu.Unlock()
	return proto.Clone(m).(*v1alpha1.Model), nil
}

// GetModel implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) GetModel(_ context.Context, req *v1alpha1.GetModelRequest) (*v1alpha1.Model, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.RLock()
	defer s.mu.RUnlock()

	m, ok := s.models[key]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "model %q not found", key)
	}
	return proto.Clone(m).(*v1alpha1.Model), nil
}

// ListModels implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) ListModels(_ context.Context, req *v1alpha1.ListModelsRequest) (*v1alpha1.ListModelsResponse, error) {
	atespace := strings.TrimSpace(req.GetAtespace())
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*v1alpha1.Model
	for _, m := range s.models {
		if atespace != "" && m.GetMetadata().GetAtespace() != atespace {
			continue
		}
		out = append(out, proto.Clone(m).(*v1alpha1.Model))
	}
	return &v1alpha1.ListModelsResponse{Models: out}, nil
}

// DeleteModel implements ax.v1alpha1.AXServer.
func (s *EmbeddedAXServer) DeleteModel(_ context.Context, req *v1alpha1.DeleteModelRequest) (*v1alpha1.DeleteModelResponse, error) {
	key := resourceKey(req.GetAtespace(), req.GetName())
	s.mu.Lock()
	delete(s.models, key)
	s.mu.Unlock()
	return &v1alpha1.DeleteModelResponse{}, nil
}

func parseTaskResources(res *v1alpha1.ResourcesSpec) (cpuMilli int, memoryMB int) {
	cpuMilli = 1000
	memoryMB = 2048
	if res == nil {
		return cpuMilli, memoryMB
	}
	list := res.GetLimits()
	if list == nil {
		list = res.GetRequests()
	}
	if list == nil {
		return cpuMilli, memoryMB
	}
	if rawCPU := strings.TrimSpace(list.GetCpu()); rawCPU != "" {
		if v, err := v1alpha1.ParseCPUMillicores(rawCPU); err == nil && v > 0 {
			cpuMilli = int(v)
		} else if strings.HasSuffix(rawCPU, "m") {
			if n, err := strconv.Atoi(strings.TrimSuffix(rawCPU, "m")); err == nil && n > 0 {
				cpuMilli = n
			}
		}
	}
	if rawMem := strings.TrimSpace(list.GetMemory()); rawMem != "" {
		if v, err := v1alpha1.ParseMemoryBytes(rawMem); err == nil && v > 0 {
			memoryMB = int(v / (1024 * 1024))
		}
	}
	return cpuMilli, memoryMB
}

func containsString(slice []string, target string) bool {
	for _, item := range slice {
		if item == target {
			return true
		}
	}
	return false
}

func removeString(slice []string, target string) []string {
	out := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != target {
			out = append(out, item)
		}
	}
	return out
}
