package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	ateapi "github.com/gke-demos/ax-bug-bash/internal/substrate/ateapiv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ViewerOptions configures the read-only Agent Substrate Control API viewer client.
type ViewerOptions struct {
	Addr          string
	Authority     string
	Atespace      string
	TokenFile     string
	Token         string
	CAFile        string
	InsecureTLS   bool
	Plaintext     bool
	ExtraDialOpts []grpc.DialOption
}

type bearerTokenAuth struct {
	tokenFile   string
	staticToken string
}

func (b bearerTokenAuth) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	if tok := strings.TrimSpace(b.staticToken); tok != "" {
		return map[string]string{"authorization": "Bearer " + tok}, nil
	}
	if strings.TrimSpace(b.tokenFile) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(b.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading substrate token file %q: %w", b.tokenFile, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

func (b bearerTokenAuth) RequireTransportSecurity() bool {
	return false
}

// WorkerTelemetry represents live telemetry for an Agent Substrate ateworker-gvisor node.
type WorkerTelemetry struct {
	WorkerID            string   `json:"worker_id"`
	WorkerPod           string   `json:"worker_pod"`
	WorkerPool          string   `json:"worker_pool"`
	NodeName            string   `json:"node_name"`
	RuntimeClass        string   `json:"runtime_class"`
	SnapshotFidelity    string   `json:"snapshot_fidelity"`
	WorkerIP            string   `json:"worker_ip"`
	TotalCPUMillicores  int      `json:"total_cpu_millicores"`
	UsedCPUMillicores   int      `json:"used_cpu_millicores"`
	TotalMemoryMB       int      `json:"total_memory_mb"`
	UsedMemoryMB        int      `json:"used_memory_mb"`
	TotalActorSlots     int      `json:"total_actor_slots"`
	UsedActorSlots      int      `json:"used_actor_slots"`
	ActiveTasks         []string `json:"active_tasks"`
	SuspendedSnapshots  []string `json:"suspended_snapshots"`
	ReclaimedSlotsCount int      `json:"reclaimed_slots_count"`
	State               string   `json:"state"`
	Status              string   `json:"status"`
}

// ActorTelemetry captures live state and snapshot fidelity for an Actor in an Atespace.
type ActorTelemetry struct {
	Name               string `json:"name"`
	Atespace           string `json:"atespace"`
	State              string `json:"state"`
	WorkerID           string `json:"worker_id,omitempty"`
	WorkerPod          string `json:"worker_pod,omitempty"`
	WorkerIP           string `json:"worker_ip,omitempty"`
	SnapshotFidelity   string `json:"snapshot_fidelity,omitempty"`
	SnapshotGeneration int64  `json:"snapshot_generation,omitempty"`
}

// ViewerSnapshot aggregates live WorkerPool, ActorAssignment, and Actor telemetry from ate-api-server.
type ViewerSnapshot struct {
	SubstrateVersion       string            `json:"substrate_version"`
	Workers                []WorkerTelemetry `json:"workers"`
	Actors                 []ActorTelemetry  `json:"actors"`
	ActiveTasksCount       int               `json:"active_tasks_count"`
	SuspendedTasksCount    int               `json:"suspended_tasks_count"`
	TotalReclaimedSlots    int               `json:"total_reclaimed_slots"`
	TotalCPUMillicores     int               `json:"total_cpu_millicores"`
	AllocatedCPUMillicores int               `json:"allocated_cpu_millicores"`
	TotalMemoryMB          int               `json:"total_memory_mb"`
	AllocatedMemoryMB      int               `json:"allocated_memory_mb"`
	TotalActorSlots        int               `json:"total_actor_slots"`
	AllocatedActorSlots    int               `json:"allocated_actor_slots"`
	PolledAt               time.Time         `json:"polled_at"`
}

// Viewer polls read-only substrate.ateapi.v1.Control telemetry (ListWorkers, ListWorkerActorAssignments, ListActors).
type Viewer struct {
	opts    ViewerOptions
	conn    *grpc.ClientConn
	control ateapi.ControlClient
}

// NewViewer dials the Agent Substrate Control gRPC service with read-only viewer credentials.
func NewViewer(opts ViewerOptions) (*Viewer, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		opts.Addr = strings.TrimSpace(os.Getenv("SUBSTRATE_API_ADDR"))
	}
	if opts.Addr == "" {
		opts.Addr = "api.ate-system.svc.cluster.local:443"
	}
	if strings.TrimSpace(opts.Atespace) == "" {
		opts.Atespace = "default"
	}
	if strings.TrimSpace(opts.TokenFile) == "" && strings.TrimSpace(opts.Token) == "" {
		if envTok := strings.TrimSpace(os.Getenv("SUBSTRATE_TOKEN_FILE")); envTok != "" {
			opts.TokenFile = envTok
		} else if _, err := os.Stat("/var/run/secrets/ateapi/token"); err == nil {
			opts.TokenFile = "/var/run/secrets/ateapi/token"
		}
	}
	if strings.TrimSpace(opts.CAFile) == "" && !opts.Plaintext {
		if envCA := strings.TrimSpace(os.Getenv("SUBSTRATE_CA_FILE")); envCA != "" {
			opts.CAFile = envCA
		} else if _, err := os.Stat("/run/servicedns-ca/trust-bundle.pem"); err == nil {
			opts.CAFile = "/run/servicedns-ca/trust-bundle.pem"
		} else if _, err := os.Stat("/run/servicedns-ca/ca.pem"); err == nil {
			opts.CAFile = "/run/servicedns-ca/ca.pem"
		}
	}

	// Auto-enable plaintext when targeting loopback in tests unless CAFile/InsecureTLS is explicitly set.
	if !opts.Plaintext && opts.CAFile == "" && !opts.InsecureTLS &&
		(strings.HasPrefix(opts.Addr, "127.0.0.1:") || strings.HasPrefix(opts.Addr, "localhost:")) {
		opts.Plaintext = true
	}

	dialOpts := append([]grpc.DialOption(nil), opts.ExtraDialOpts...)
	if opts.Plaintext {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if opts.InsecureTLS {
			tlsCfg.InsecureSkipVerify = true
		}
		if opts.Authority != "" {
			tlsCfg.ServerName = opts.Authority
		} else if strings.Contains(opts.Addr, "api.ate-system.svc") {
			tlsCfg.ServerName = "api.ate-system.svc"
		}
		if opts.CAFile != "" {
			pemBytes, err := os.ReadFile(opts.CAFile)
			if err != nil {
				return nil, fmt.Errorf("reading substrate CA file %q: %w", opts.CAFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("parsing PEM certificates from %q", opts.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	}

	authority := opts.Authority
	if authority == "" && strings.Contains(opts.Addr, "api.ate-system.svc") {
		authority = "api.ate-system.svc"
	}
	if authority != "" {
		dialOpts = append(dialOpts, grpc.WithAuthority(authority))
	}
	if opts.TokenFile != "" || opts.Token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(bearerTokenAuth{
			tokenFile:   opts.TokenFile,
			staticToken: opts.Token,
		}))
	}

	conn, err := grpc.NewClient(opts.Addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dialing substrate control API %s: %w", opts.Addr, err)
	}
	return &Viewer{
		opts:    opts,
		conn:    conn,
		control: ateapi.NewControlClient(conn),
	}, nil
}

// Close closes the underlying gRPC connection.
func (v *Viewer) Close() error {
	if v == nil || v.conn == nil {
		return nil
	}
	return v.conn.Close()
}

// ControlClient returns the underlying ateapi.ControlClient stub.
func (v *Viewer) ControlClient() ateapi.ControlClient {
	if v == nil {
		return nil
	}
	return v.control
}

// PollSnapshot queries ListWorkers, ListWorkerActorAssignments, and ListActors and maps live worker-slot
// and actor state telemetry.
func (v *Viewer) PollSnapshot(ctx context.Context) (ViewerSnapshot, error) {
	if v == nil || v.control == nil {
		return ViewerSnapshot{}, fmt.Errorf("substrate viewer is not initialized")
	}

	rawWorkers, err := v.listAllWorkers(ctx)
	if err != nil {
		return ViewerSnapshot{}, err
	}
	rawActors, err := v.listAllActors(ctx, v.opts.Atespace)
	if err != nil {
		return ViewerSnapshot{}, err
	}

	sort.Slice(rawWorkers, func(i, j int) bool {
		return rawWorkers[i].GetMetadata().GetName() < rawWorkers[j].GetMetadata().GetName()
	})
	sort.Slice(rawActors, func(i, j int) bool {
		return rawActors[i].GetMetadata().GetName() < rawActors[j].GetMetadata().GetName()
	})

	actorsOut := make([]ActorTelemetry, 0, len(rawActors))
	suspendedByWorker := make(map[string][]string)
	var unassignedSuspended []string
	runningActorCount := 0
	suspendedActorCount := 0
	reclaimedCount := 0

	for _, a := range rawActors {
		name := a.GetMetadata().GetName()
		atespace := a.GetMetadata().GetAtespace()
		if atespace == "" {
			atespace = v.opts.Atespace
		}
		stateStr := normalizeActorState(a.GetStatus().GetState())
		wa := a.GetStatus().GetWorkerAssignment()
		workerID := wa.GetWorker().GetName()
		workerPod := wa.GetWorkerPod()
		var workerIP string
		if ips := wa.GetWorkerPodIps(); len(ips) > 0 {
			workerIP = ips[0]
		}
		snapFidelity := "SNAPSHOT_FIDELITY_VOLUMES"
		var snapGen int64
		if snaps := a.GetStatus().GetSnapshots(); len(snaps) > 0 {
			latest := snaps[len(snaps)-1]
			snapGen = int64(latest.GetGeneration())
			if latest.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED {
				snapFidelity = latest.GetFidelity().String()
			}
		}

		switch stateStr {
		case "RUNNING", "RESUMING":
			runningActorCount++
		case "SUSPENDED":
			suspendedActorCount++
			reclaimedCount++
			targetWorker := firstNonEmptyStr(workerID, workerPod)
			if targetWorker != "" {
				suspendedByWorker[targetWorker] = append(suspendedByWorker[targetWorker], name)
			} else {
				unassignedSuspended = append(unassignedSuspended, name)
			}
		}

		actorsOut = append(actorsOut, ActorTelemetry{
			Name:               name,
			Atespace:           atespace,
			State:              stateStr,
			WorkerID:           workerID,
			WorkerPod:          workerPod,
			WorkerIP:           workerIP,
			SnapshotFidelity:   snapFidelity,
			SnapshotGeneration: snapGen,
		})
	}

	snap := ViewerSnapshot{
		SubstrateVersion:    "v0.6.1",
		Workers:             make([]WorkerTelemetry, 0, len(rawWorkers)),
		Actors:              actorsOut,
		ActiveTasksCount:    runningActorCount,
		SuspendedTasksCount: suspendedActorCount,
		TotalReclaimedSlots: reclaimedCount,
		PolledAt:            time.Now().UTC(),
	}

	for idx, w := range rawWorkers {
		wName := w.GetMetadata().GetName()
		wPod := firstNonEmptyStr(w.GetWorkerPod(), wName)
		wID := firstNonEmptyStr(wName, wPod)

		assignments, err := v.listWorkerAssignments(ctx, wName)
		if err != nil {
			return ViewerSnapshot{}, err
		}

		activeNames := make([]string, 0, len(assignments))
		var assignedCPUMilli int
		var assignedMemMB int
		for _, asgn := range assignments {
			if aName := asgn.GetActor().GetName(); aName != "" && !containsStr(activeNames, aName) {
				activeNames = append(activeNames, aName)
			}
			assignedCPUMilli += int(asgn.GetResources().GetCpuMillicores())
			assignedMemMB += int(asgn.GetResources().GetMemoryBytes() / (1024 * 1024))
		}

		capRes := w.GetStatus().GetCapacity()
		allocRes := w.GetStatus().GetAllocated()
		totalCPU := int(capRes.GetCpuMillicores())
		usedCPU := int(allocRes.GetCpuMillicores())
		if usedCPU == 0 && assignedCPUMilli > 0 {
			usedCPU = assignedCPUMilli
		}
		totalMemMB := int(capRes.GetMemoryBytes() / (1024 * 1024))
		usedMemMB := int(allocRes.GetMemoryBytes() / (1024 * 1024))
		if usedMemMB == 0 && assignedMemMB > 0 {
			usedMemMB = assignedMemMB
		}
		totalSlots := int(capRes.GetActors())
		usedSlots := int(allocRes.GetActors())
		if len(activeNames) > usedSlots {
			usedSlots = len(activeNames)
		}

		var workerIP string
		if ips := w.GetStatus().GetIps(); len(ips) > 0 {
			workerIP = ips[0]
		}
		runtimeClass := "gvisor"
		if classes := w.GetStatus().GetSandboxClasses(); len(classes) > 0 && classes[0].GetSandboxClass() != "" {
			runtimeClass = classes[0].GetSandboxClass()
		}

		suspendedList := append([]string(nil), suspendedByWorker[wID]...)
		if wPod != wID {
			for _, s := range suspendedByWorker[wPod] {
				if !containsStr(suspendedList, s) {
					suspendedList = append(suspendedList, s)
				}
			}
		}
		// Attribute unassigned suspended snapshots round-robin across workers so per-worker UI views stay consistent.
		for uIdx, s := range unassignedSuspended {
			if len(rawWorkers) > 0 && uIdx%len(rawWorkers) == idx && !containsStr(suspendedList, s) {
				suspendedList = append(suspendedList, s)
			}
		}

		stateStr := normalizeWorkerState(w.GetStatus().GetState())
		statusLabel := "WARM_IDLE"
		switch {
		case stateStr == "DRAINING" || stateStr == "UNAVAILABLE":
			statusLabel = stateStr
		case len(activeNames) > 0 || usedSlots > 0:
			statusLabel = "ACTIVE_SANDBOX"
		case len(suspendedList) > 0:
			statusLabel = "RECLAIMED_WARM"
		}

		wt := WorkerTelemetry{
			WorkerID:            wID,
			WorkerPod:           wPod,
			WorkerPool:          w.GetWorkerPool(),
			NodeName:            w.GetNodeName(),
			RuntimeClass:        runtimeClass,
			SnapshotFidelity:    "SNAPSHOT_FIDELITY_VOLUMES",
			WorkerIP:            workerIP,
			TotalCPUMillicores:  totalCPU,
			UsedCPUMillicores:   usedCPU,
			TotalMemoryMB:       totalMemMB,
			UsedMemoryMB:        usedMemMB,
			TotalActorSlots:     totalSlots,
			UsedActorSlots:      usedSlots,
			ActiveTasks:         activeNames,
			SuspendedSnapshots:  suspendedList,
			ReclaimedSlotsCount: len(suspendedList),
			State:               stateStr,
			Status:              statusLabel,
		}
		snap.Workers = append(snap.Workers, wt)

		snap.TotalCPUMillicores += totalCPU
		snap.AllocatedCPUMillicores += usedCPU
		snap.TotalMemoryMB += totalMemMB
		snap.AllocatedMemoryMB += usedMemMB
		snap.TotalActorSlots += totalSlots
		snap.AllocatedActorSlots += usedSlots
	}

	if snap.ActiveTasksCount == 0 && snap.AllocatedActorSlots > 0 {
		snap.ActiveTasksCount = snap.AllocatedActorSlots
	}

	return snap, nil
}

func (v *Viewer) listAllWorkers(ctx context.Context) ([]*ateapi.Worker, error) {
	var all []*ateapi.Worker
	pageToken := ""
	for page := 0; page < 50; page++ {
		resp, err := v.control.ListWorkers(ctx, &ateapi.ListWorkersRequest{
			Options: &ateapi.ListOptions{
				PageSize:  100,
				PageToken: pageToken,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("ListWorkers: %w", err)
		}
		all = append(all, resp.GetWorkers()...)
		pageToken = strings.TrimSpace(resp.GetNextPageToken())
		if pageToken == "" {
			break
		}
	}
	return all, nil
}

func (v *Viewer) listWorkerAssignments(ctx context.Context, workerName string) ([]*ateapi.ActorAssignment, error) {
	var all []*ateapi.ActorAssignment
	pageToken := ""
	for page := 0; page < 50; page++ {
		resp, err := v.control.ListWorkerActorAssignments(ctx, &ateapi.ListWorkerActorAssignmentsRequest{
			Worker: &ateapi.ObjectRef{Name: workerName},
			Options: &ateapi.ListOptions{
				PageSize:  100,
				PageToken: pageToken,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("ListWorkerActorAssignments(%s): %w", workerName, err)
		}
		all = append(all, resp.GetActorAssignments()...)
		pageToken = strings.TrimSpace(resp.GetNextPageToken())
		if pageToken == "" {
			break
		}
	}
	return all, nil
}

func (v *Viewer) listAllActors(ctx context.Context, atespace string) ([]*ateapi.Actor, error) {
	var all []*ateapi.Actor
	pageToken := ""
	for page := 0; page < 50; page++ {
		resp, err := v.control.ListActors(ctx, &ateapi.ListActorsRequest{
			Atespace: atespace,
			Options: &ateapi.ListOptions{
				PageSize:  100,
				PageToken: pageToken,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("ListActors(%s): %w", atespace, err)
		}
		all = append(all, resp.GetActors()...)
		pageToken = strings.TrimSpace(resp.GetNextPageToken())
		if pageToken == "" {
			break
		}
	}
	return all, nil
}

func normalizeActorState(st ateapi.ActorState) string {
	trimmed := strings.TrimPrefix(st.String(), "ACTOR_STATE_")
	if trimmed == "" || trimmed == "UNSPECIFIED" {
		return "UNKNOWN"
	}
	return trimmed
}

func normalizeWorkerState(st ateapi.WorkerState) string {
	trimmed := strings.TrimPrefix(st.String(), "WORKER_STATE_")
	if trimmed == "" || trimmed == "UNSPECIFIED" {
		return "ACTIVE"
	}
	return trimmed
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func containsStr(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
