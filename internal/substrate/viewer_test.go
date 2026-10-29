package substrate_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/substrate"
	ateapi "github.com/gke-demos/ax-bug-bash/internal/substrate/ateapiv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeSubstrateControlServer struct {
	ateapi.UnimplementedControlServer

	mu            sync.Mutex
	expectedToken string
	workers       []*ateapi.Worker
	assignments   map[string][]*ateapi.ActorAssignment
	actors        []*ateapi.Actor
}

func (f *fakeSubstrateControlServer) checkAuth(ctx context.Context) error {
	if f.expectedToken == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 || vals[0] != "Bearer "+f.expectedToken {
		return status.Errorf(codes.PermissionDenied, "invalid authorization header %v", vals)
	}
	return nil
}

func (f *fakeSubstrateControlServer) ListWorkers(ctx context.Context, req *ateapi.ListWorkersRequest) (*ateapi.ListWorkersResponse, error) {
	if err := f.checkAuth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	tok := req.GetOptions().GetPageToken()
	switch tok {
	case "":
		if len(f.workers) <= 1 {
			return &ateapi.ListWorkersResponse{Workers: f.workers}, nil
		}
		return &ateapi.ListWorkersResponse{
			Workers:       f.workers[:1],
			NextPageToken: "page-2",
		}, nil
	case "page-2":
		return &ateapi.ListWorkersResponse{
			Workers: f.workers[1:],
		}, nil
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unexpected page token %q", tok)
	}
}

func (f *fakeSubstrateControlServer) ListWorkerActorAssignments(ctx context.Context, req *ateapi.ListWorkerActorAssignmentsRequest) (*ateapi.ListWorkerActorAssignmentsResponse, error) {
	if err := f.checkAuth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	wName := req.GetWorker().GetName()
	return &ateapi.ListWorkerActorAssignmentsResponse{
		ActorAssignments: f.assignments[wName],
	}, nil
}

func (f *fakeSubstrateControlServer) ListActors(ctx context.Context, req *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	if err := f.checkAuth(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var filtered []*ateapi.Actor
	for _, a := range f.actors {
		if req.GetAtespace() == "" || a.GetMetadata().GetAtespace() == req.GetAtespace() {
			filtered = append(filtered, a)
		}
	}
	return &ateapi.ListActorsResponse{Actors: filtered}, nil
}

func TestSubstrateViewerPollsWorkersAssignmentsAndActorsOverLoopbackGRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fake := &fakeSubstrateControlServer{
		expectedToken: "viewer-sa-jwt-token",
		workers: []*ateapi.Worker{
			{
				Metadata:        &ateapi.ResourceMetadata{Name: "ateworker-gvisor-01"},
				WorkerNamespace: "ate-system",
				WorkerPool:      "default-pool",
				WorkerPod:       "ateworker-gvisor-01",
				NodeName:        "pool-node-a",
				Status: &ateapi.WorkerStatus{
					State: ateapi.WorkerState_WORKER_STATE_ACTIVE,
					Ips:   []string{"10.244.1.21"},
					SandboxClasses: []*ateapi.WorkerSandboxClass{
						{SandboxClass: "gvisor"},
					},
					Capacity: &ateapi.WorkerResources{
						CpuMillicores: 4000,
						MemoryBytes:   8192 << 20,
						Actors:        4,
					},
					Allocated: &ateapi.WorkerResources{
						CpuMillicores: 1000,
						MemoryBytes:   2048 << 20,
						Actors:        1,
					},
				},
			},
			{
				Metadata:        &ateapi.ResourceMetadata{Name: "ateworker-gvisor-02"},
				WorkerNamespace: "ate-system",
				WorkerPool:      "default-pool",
				WorkerPod:       "ateworker-gvisor-02",
				NodeName:        "pool-node-b",
				Status: &ateapi.WorkerStatus{
					State: ateapi.WorkerState_WORKER_STATE_ACTIVE,
					Ips:   []string{"10.244.1.22"},
					SandboxClasses: []*ateapi.WorkerSandboxClass{
						{SandboxClass: "gvisor"},
					},
					Capacity: &ateapi.WorkerResources{
						CpuMillicores: 4000,
						MemoryBytes:   8192 << 20,
						Actors:        4,
					},
					Allocated: &ateapi.WorkerResources{
						CpuMillicores: 0,
						MemoryBytes:   0,
						Actors:        0,
					},
				},
			},
		},
		assignments: map[string][]*ateapi.ActorAssignment{
			"ateworker-gvisor-01": {
				{
					Actor: &ateapi.ObjectRef{Atespace: "default", Name: "triage-inc-101-1"},
					Resources: &ateapi.Resources{
						CpuMillicores: 1000,
						MemoryBytes:   2048 << 20,
					},
				},
			},
			"ateworker-gvisor-02": nil,
		},
		actors: []*ateapi.Actor{
			{
				Metadata: &ateapi.ResourceMetadata{Atespace: "default", Name: "triage-inc-101-1"},
				Status: &ateapi.ActorStatus{
					State: ateapi.ActorState_ACTOR_STATE_RUNNING,
					WorkerAssignment: &ateapi.WorkerAssignment{
						Worker:       &ateapi.ObjectRef{Name: "ateworker-gvisor-01"},
						WorkerPod:    "ateworker-gvisor-01",
						WorkerPodIps: []string{"10.244.1.21"},
					},
				},
			},
			{
				Metadata: &ateapi.ResourceMetadata{Atespace: "default", Name: "fixer-inc-101-pricing-service-1"},
				Status: &ateapi.ActorStatus{
					State: ateapi.ActorState_ACTOR_STATE_SUSPENDED,
					Snapshots: []*ateapi.Snapshot{
						{
							Generation: 1,
							Fidelity:   ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
						},
					},
				},
			},
			{
				Metadata: &ateapi.ResourceMetadata{Atespace: "default", Name: "fixer-inc-101-shipping-service-1"},
				Status: &ateapi.ActorStatus{
					State: ateapi.ActorState_ACTOR_STATE_SUSPENDED,
					WorkerAssignment: &ateapi.WorkerAssignment{
						Worker:    &ateapi.ObjectRef{Name: "ateworker-gvisor-02"},
						WorkerPod: "ateworker-gvisor-02",
					},
					Snapshots: []*ateapi.Snapshot{
						{
							Generation: 2,
							Fidelity:   ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
						},
					},
				},
			},
		},
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen 127.0.0.1:0 failed: %v", err)
	}
	grpcSrv := grpc.NewServer()
	ateapi.RegisterControlServer(grpcSrv, fake)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	defer grpcSrv.Stop()

	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("viewer-sa-jwt-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	viewer, err := substrate.NewViewer(substrate.ViewerOptions{
		Addr:      lis.Addr().String(),
		Atespace:  "default",
		TokenFile: tokFile,
		Plaintext: true,
	})
	if err != nil {
		t.Fatalf("NewViewer failed: %v", err)
	}
	defer viewer.Close()

	snap, err := viewer.PollSnapshot(ctx)
	if err != nil {
		t.Fatalf("PollSnapshot failed: %v", err)
	}

	if len(snap.Workers) != 2 {
		t.Fatalf("expected 2 workers (across paginated ListWorkers), got %d", len(snap.Workers))
	}
	if snap.Workers[0].WorkerIP != "10.244.1.21" || snap.Workers[1].WorkerIP != "10.244.1.22" {
		t.Errorf("unexpected worker pod IPs: %q, %q", snap.Workers[0].WorkerIP, snap.Workers[1].WorkerIP)
	}
	if snap.Workers[0].TotalActorSlots != 4 || snap.Workers[0].UsedActorSlots != 1 {
		t.Errorf("worker[0] actor slots = (%d used / %d total), want (1 / 4)", snap.Workers[0].UsedActorSlots, snap.Workers[0].TotalActorSlots)
	}
	if snap.Workers[1].TotalActorSlots != 4 || snap.Workers[1].UsedActorSlots != 0 {
		t.Errorf("worker[1] actor slots = (%d used / %d total), want (0 / 4)", snap.Workers[1].UsedActorSlots, snap.Workers[1].TotalActorSlots)
	}
	if snap.TotalCPUMillicores != 8000 || snap.AllocatedCPUMillicores != 1000 {
		t.Errorf("CPU millicores = (%d / %d), want (1000 / 8000)", snap.AllocatedCPUMillicores, snap.TotalCPUMillicores)
	}
	if snap.TotalMemoryMB != 16384 || snap.AllocatedMemoryMB != 2048 {
		t.Errorf("Memory MB = (%d / %d), want (2048 / 16384)", snap.AllocatedMemoryMB, snap.TotalMemoryMB)
	}
	if snap.TotalActorSlots != 8 || snap.AllocatedActorSlots != 1 {
		t.Errorf("Actor slots = (%d / %d), want (1 / 8)", snap.AllocatedActorSlots, snap.TotalActorSlots)
	}
	if snap.ActiveTasksCount != 1 {
		t.Errorf("ActiveTasksCount = %d, want 1", snap.ActiveTasksCount)
	}
	if snap.SuspendedTasksCount != 2 {
		t.Errorf("SuspendedTasksCount = %d, want 2", snap.SuspendedTasksCount)
	}
	if snap.TotalReclaimedSlots != 2 {
		t.Errorf("TotalReclaimedSlots = %d, want 2", snap.TotalReclaimedSlots)
	}

	var runningActors, suspendedActors []string
	for _, a := range snap.Actors {
		switch a.State {
		case "RUNNING":
			runningActors = append(runningActors, a.Name)
		case "SUSPENDED":
			suspendedActors = append(suspendedActors, a.Name)
			if !strings.Contains(a.SnapshotFidelity, "VOLUMES") {
				t.Errorf("expected suspended actor %s SnapshotFidelity to contain VOLUMES, got %q", a.Name, a.SnapshotFidelity)
			}
		}
	}
	if len(runningActors) != 1 || runningActors[0] != "triage-inc-101-1" {
		t.Errorf("runningActors = %v, want [triage-inc-101-1]", runningActors)
	}
	if len(suspendedActors) != 2 {
		t.Errorf("suspendedActors = %v, want 2 suspended actors", suspendedActors)
	}
}
