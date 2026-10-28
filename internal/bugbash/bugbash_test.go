package bugbash

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

func seedTestServicesDir(destRoot string) error {
	return storefront.SeedServicesTree(destRoot)
}

func TestAXServerAllocatesAndReclaimsSubstrateWorkerSlotsOnSuspend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv, addr, err := StartEmbeddedAXServer(ctx)
	if err != nil {
		t.Fatalf("StartEmbeddedAXServer failed: %v", err)
	}
	defer srv.Stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient(%s) failed: %v", addr, err)
	}
	defer conn.Close()

	client := v1alpha1.NewAXClient(conn)

	// 1. Register Workspace and Model via AXClient
	ws, err := client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: DefaultWorkspaceName, Atespace: "default"},
			Spec: &v1alpha1.WorkspaceSpec{
				Git: []*v1alpha1.GitRepo{
					{Name: "storefront", Repo: "http://storefront.default:3001/git/storefront.git", Branch: "main", Dir: "."},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace failed: %v", err)
	}
	if ws.GetMetadata().GetName() != DefaultWorkspaceName {
		t.Fatalf("expected workspace %s, got %q", DefaultWorkspaceName, ws.GetMetadata().GetName())
	}

	cfgStruct, err := structpb.NewStruct(map[string]any{
		"role":        "child-fixer",
		"incident_id": "INC-101",
		"service":     "pricing-service",
		"harness":     "antigravity",
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct failed: %v", err)
	}

	// 2. CreateTask via AXClient
	created, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "fixer-inc-101-pricing-service", Atespace: "default"},
			Spec: &v1alpha1.TaskSpec{
				Workspace: &v1alpha1.WorkspaceRef{Name: DefaultWorkspaceName, Path: "/workspace"},
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "2048Mi"},
				},
				Config: cfgStruct,
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if created.GetMetadata().GetName() != "fixer-inc-101-pricing-service" {
		t.Fatalf("unexpected task name: %s", created.GetMetadata().GetName())
	}

	// 3. ResumeTask allocates an ateworker-gvisor slot (1000m CPU, 2048MB RAM)
	resumed, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
		Atespace: "default",
		Name:     "fixer-inc-101-pricing-service",
	})
	if err != nil {
		t.Fatalf("ResumeTask failed: %v", err)
	}
	if resumed.GetStatus().GetPhase() != v1alpha1.PhaseRunning {
		t.Fatalf("expected phase %q after ResumeTask, got %q", v1alpha1.PhaseRunning, resumed.GetStatus().GetPhase())
	}

	poolDuring := srv.WorkerPoolSnapshot()
	if poolDuring.ActiveTasksCount != 1 {
		t.Fatalf("expected 1 active task on Substrate worker pool, got %d", poolDuring.ActiveTasksCount)
	}
	if poolDuring.AllocatedCPUMillicores != 1000 {
		t.Fatalf("expected 1000m allocated CPU during task execution, got %d", poolDuring.AllocatedCPUMillicores)
	}
	if poolDuring.AllocatedMemoryMB != 2048 {
		t.Fatalf("expected 2048MB allocated memory during task execution, got %d", poolDuring.AllocatedMemoryMB)
	}

	// 4. SuspendTask immediately reclaims CPU & memory on the ateworker-gvisor node and records SNAPSHOT_FIDELITY_VOLUMES snapshot
	suspended, err := client.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
		Atespace: "default",
		Name:     "fixer-inc-101-pricing-service",
	})
	if err != nil {
		t.Fatalf("SuspendTask failed: %v", err)
	}
	if suspended.GetStatus().GetPhase() != v1alpha1.PhaseSuspended {
		t.Fatalf("expected phase %q after SuspendTask, got %q", v1alpha1.PhaseSuspended, suspended.GetStatus().GetPhase())
	}

	poolAfter := srv.WorkerPoolSnapshot()
	if poolAfter.ActiveTasksCount != 0 {
		t.Fatalf("expected 0 active tasks after SuspendTask, got %d", poolAfter.ActiveTasksCount)
	}
	if poolAfter.AllocatedCPUMillicores != 0 {
		t.Fatalf("expected 0m allocated CPU after SuspendTask reclamation, got %d", poolAfter.AllocatedCPUMillicores)
	}
	if poolAfter.AllocatedMemoryMB != 0 {
		t.Fatalf("expected 0MB allocated memory after SuspendTask reclamation, got %d", poolAfter.AllocatedMemoryMB)
	}
	if poolAfter.TotalReclaimedSlots != 1 {
		t.Fatalf("expected TotalReclaimedSlots=1 after SuspendTask, got %d", poolAfter.TotalReclaimedSlots)
	}
}

func TestRootTriageSpawnsOnlyImplicatedServicesAndSupportsMultiHarness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsDir := t.TempDir()
	if err := seedTestServicesDir(wsDir); err != nil {
		t.Fatalf("seedTestServicesDir failed: %v", err)
	}

	// Verify all 4 scenarios parse stack traces to ONLY 2-3 implicated services (never all 5)
	cases := []struct {
		incidentID     string
		rawTraces      []string
		harnessMode    string
		wantImplicated []string
		wantUnaffected []string
		wantHarnesses  []string
	}{
		{
			incidentID: "INC-101",
			rawTraces: []string{
				`File "/app/services/pricing-service/pricing.py", line 25, in apply_stacked_promos: NegativeSubtotalError`,
				`github.com/gke-demos/ax-bug-bash/services/shipping-service.QuoteCarrierRate: /workspace/services/shipping-service/shipping.go:12`,
			},
			harnessMode:    "multi",
			wantImplicated: []string{"pricing-service", "shipping-service"},
			wantUnaffected: []string{"storefront-web", "checkout-service", "inventory-service"},
			wantHarnesses:  []string{"antigravity", "opencode"},
		},
		{
			incidentID: "INC-102",
			rawTraces: []string{
				`panicked at src/lib.rs:20:9: inventory_service::reserve_bundle_stock`,
				`/workspace/services/checkout-service/checkout.go:20 ReserveFlashBundleOrder`,
			},
			harnessMode:    "opencode",
			wantImplicated: []string{"inventory-service", "checkout-service"},
			wantUnaffected: []string{"storefront-web", "shipping-service", "pricing-service"},
			wantHarnesses:  []string{"opencode", "opencode"},
		},
		{
			incidentID: "INC-103",
			rawTraces: []string{
				`at convertToCurrencyMinorUnits (/workspace/services/storefront-web/src/server.ts:69:13)`,
				`File "/app/services/pricing-service/pricing.py", line 42, in convert_currency_minor_units`,
				`/workspace/services/checkout-service/checkout.go:32 CaptureCurrencyPayment`,
			},
			harnessMode:    "multi",
			wantImplicated: []string{"storefront-web", "pricing-service", "checkout-service"},
			wantUnaffected: []string{"shipping-service", "inventory-service"},
			wantHarnesses:  []string{"antigravity", "opencode", "pi"},
		},
		{
			incidentID: "INC-104",
			rawTraces: []string{
				`inventory_service::classify_warehouse_hazmat at ./src/lib.rs:36:14`,
				`/workspace/services/shipping-service/shipping.go:26 QuoteCarrierRate`,
			},
			harnessMode:    "pi",
			wantImplicated: []string{"inventory-service", "shipping-service"},
			wantUnaffected: []string{"storefront-web", "checkout-service", "pricing-service"},
			wantHarnesses:  []string{"pi", "pi"},
		},
	}

	srv, addr, err := StartEmbeddedAXServer(ctx)
	if err != nil {
		t.Fatalf("StartEmbeddedAXServer failed: %v", err)
	}
	defer srv.Stop()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer conn.Close()
	axClient := v1alpha1.NewAXClient(conn)

	for _, tc := range cases {
		plan, err := AnalyzeIncidentTraces(AlertPayload{
			IncidentID:  tc.incidentID,
			StackTraces: tc.rawTraces,
			HarnessMode: tc.harnessMode,
		}, wsDir)
		if err != nil {
			t.Fatalf("[%s] AnalyzeIncidentTraces failed: %v", tc.incidentID, err)
		}
		if len(plan.ImplicatedServices) != len(tc.wantImplicated) {
			t.Fatalf("[%s] expected %d implicated services %v, got %v", tc.incidentID, len(tc.wantImplicated), tc.wantImplicated, plan.ImplicatedServices)
		}
		for i, wantSvc := range tc.wantImplicated {
			if plan.ImplicatedServices[i] != wantSvc {
				t.Fatalf("[%s] implicated[%d] = %q, want %q", tc.incidentID, i, plan.ImplicatedServices[i], wantSvc)
			}
			if plan.ChildSpecs[i].Harness != tc.wantHarnesses[i] {
				t.Fatalf("[%s] child[%d] harness = %q, want %q", tc.incidentID, i, plan.ChildSpecs[i].Harness, tc.wantHarnesses[i])
			}
		}
		if len(plan.UnaffectedServices) != len(tc.wantUnaffected) {
			t.Fatalf("[%s] expected %d unaffected services %v, got %v", tc.incidentID, len(tc.wantUnaffected), tc.wantUnaffected, plan.UnaffectedServices)
		}

		parentName := "triage-" + tc.incidentID
		spawned, err := SpawnChildTasksForTriage(ctx, axClient, parentName, plan, SpawnChildOptions{
			Atespace:      "default",
			WorkspaceName: DefaultWorkspaceName,
			WorkspacePath: "/workspace",
		})
		if err != nil {
			t.Fatalf("[%s] SpawnChildTasksForTriage failed: %v", tc.incidentID, err)
		}
		if len(spawned) != len(tc.wantImplicated) {
			t.Fatalf("[%s] expected %d spawned child tasks, got %d", tc.incidentID, len(tc.wantImplicated), len(spawned))
		}
		for i, childTask := range spawned {
			if childTask.GetStatus().GetPhase() != v1alpha1.PhaseRunning {
				t.Fatalf("[%s] expected child task %s to be Running, got %s", tc.incidentID, childTask.GetMetadata().GetName(), childTask.GetStatus().GetPhase())
			}
			cfgMap := childTask.GetSpec().GetConfig().AsMap()
			if cfgMap["service"] != tc.wantImplicated[i] {
				t.Fatalf("[%s] child config service = %v, want %s", tc.incidentID, cfgMap["service"], tc.wantImplicated[i])
			}
			if cfgMap["harness"] != tc.wantHarnesses[i] {
				t.Fatalf("[%s] child config harness = %v, want %s", tc.incidentID, cfgMap["harness"], tc.wantHarnesses[i])
			}
			if cfgMap["failing_request"] == nil {
				t.Fatalf("[%s] expected child config failing_request to be populated, got nil", tc.incidentID)
			}
		}
	}
}

func TestChildFixerExecutesRedGreenTDDCycleAndPersistsAXState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start in-process Model Gateway with deterministic FIXTURE fallback enabled for offline tests
	gwCfg := gateway.DefaultConfig()
	gwCfg.EnableFixtureFallback = true
	gwSrv := gateway.NewServer(gwCfg, func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	})
	gwHTTP := httptest.NewServer(gwSrv.Handler())
	defer gwHTTP.Close()

	// Seed a real Git repository with the 5-service monorepo
	gitMgr, err := storefront.NewGitWorkspaceManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewGitWorkspaceManager failed: %v", err)
	}
	wsDir := gitMgr.RepoDir()

	sc, ok := storefront.GetScenario(storefront.IncidentStackedPromoNegativeSubtotal)
	if !ok {
		t.Fatalf("missing scenario INC-101")
	}
	pricingDetail := sc.ServiceTraces[storefront.ServicePricing]

	mission := ChildTaskMission{
		TaskName:           "fixer-inc-101-pricing-service",
		ParentTask:         "triage-inc-101",
		IncidentID:         sc.ID,
		Service:            storefront.ServicePricing,
		Language:           pricingDetail.Language,
		Harness:            "opencode",
		RequestedModel:     "gemini-3.8-flash",
		SourceFile:         pricingDetail.SourceFile,
		TestFile:           pricingDetail.TestFile,
		ErrorType:          pricingDetail.ErrorType,
		ErrorMessage:       pricingDetail.ErrorMessage,
		StackTrace:         pricingDetail.StackTrace,
		RegressionTestName: pricingDetail.RegressionTestName,
	}

	var observedPhases []string
	res, err := ExecuteChildFixerTDD(ctx, ChildFixerConfig{
		Mission:         mission,
		WorkspaceDir:    wsDir,
		ModelGatewayURL: gwHTTP.URL,
		OnStep: func(step TDDStepEvent) {
			observedPhases = append(observedPhases, step.Phase)
		},
	})
	if err != nil {
		t.Fatalf("ExecuteChildFixerTDD failed: %v", err)
	}

	if res.RedTestOutput == "" || !strings.Contains(res.RedTestOutput, "NegativeSubtotalError") {
		t.Fatalf("expected RedTestOutput to contain failing NegativeSubtotalError, got:\n%s", res.RedTestOutput)
	}
	if res.GreenTestOutput == "" || !strings.Contains(res.GreenTestOutput, "OK") {
		t.Fatalf("expected GreenTestOutput to pass unittest with OK, got:\n%s", res.GreenTestOutput)
	}
	if !strings.Contains(res.UnifiedGitDiff, "test_stacked_promos_clamp_at_zero_subtotal") {
		t.Fatalf("expected UnifiedGitDiff to include new regression test, got:\n%s", res.UnifiedGitDiff)
	}
	if !strings.Contains(res.UnifiedGitDiff, "max(0, discounted)") {
		t.Fatalf("expected UnifiedGitDiff to include production code fix, got:\n%s", res.UnifiedGitDiff)
	}
	if len(observedPhases) < 3 {
		t.Fatalf("expected at least 3 TDD step phases (REPRODUCE_RED, PATCH_GREEN, EXTRACT_DIFF), got %v", observedPhases)
	}

	// Verify /workspace/.ax-state/<task-name>.json was persisted for SNAPSHOT_FIDELITY_VOLUMES (ADR-0006 Issue #518)
	loadedState, err := LoadTaskStateSnapshot(wsDir, mission.TaskName)
	if err != nil {
		t.Fatalf("LoadTaskStateSnapshot failed: %v", err)
	}
	if loadedState.Service != storefront.ServicePricing || loadedState.UnifiedGitDiff == "" {
		t.Fatalf("unexpected persisted .ax-state snapshot: %+v", loadedState)
	}

	// Verify Model Gateway recorded calls for this task session
	sess, ok := gwSrv.GetSession(mission.TaskName)
	if !ok || sess.RequestCount < 2 {
		t.Fatalf("expected Model Gateway session for %s with >=2 calls, got ok=%v sess=%+v", mission.TaskName, ok, sess)
	}

	// Also verify Rust inventory-service on INC-104 with antigravity harness (Gemini REST protocol)
	sc104, _ := storefront.GetScenario(storefront.IncidentLithiumHazmatWeightTier)
	invDetail := sc104.ServiceTraces[storefront.ServiceInventory]
	invRes, err := ExecuteChildFixerTDD(ctx, ChildFixerConfig{
		Mission: ChildTaskMission{
			TaskName:           "fixer-inc-104-inventory-service",
			ParentTask:         "triage-inc-104",
			IncidentID:         sc104.ID,
			Service:            storefront.ServiceInventory,
			Language:           invDetail.Language,
			Harness:            "antigravity",
			RequestedModel:     "gemini-3.8-flash",
			SourceFile:         invDetail.SourceFile,
			TestFile:           invDetail.TestFile,
			ErrorType:          invDetail.ErrorType,
			ErrorMessage:       invDetail.ErrorMessage,
			StackTrace:         invDetail.StackTrace,
			RegressionTestName: invDetail.RegressionTestName,
		},
		WorkspaceDir:    wsDir,
		ModelGatewayURL: gwHTTP.URL,
	})
	if err != nil {
		t.Fatalf("ExecuteChildFixerTDD for Rust inventory-service failed: %v", err)
	}
	if !strings.Contains(invRes.UnifiedGitDiff, "test_classify_un3480_lithium_heavy_ground_tier") {
		t.Fatalf("expected Rust inventory-service diff to include new regression test, got:\n%s", invRes.UnifiedGitDiff)
	}
}

func TestHelperProcessTaskWorker(t *testing.T) {
	if os.Getenv("AX_BUGBASH_TEST_SUBPROCESS") != "1" {
		return
	}
	if err := ExecuteTaskFromMetadataEndpoint(context.Background()); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func findFreeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestCustomRunnerEmbedsAXRunnerCapturesDiffAndAutoSuspendsOnExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Start embedded AXServer (gRPC)
	axSrv, axAddr, err := StartEmbeddedAXServer(ctx)
	if err != nil {
		t.Fatalf("StartEmbeddedAXServer failed: %v", err)
	}
	defer axSrv.Stop()

	conn, err := grpc.NewClient(axAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer conn.Close()
	axClient := v1alpha1.NewAXClient(conn)

	// 2. Start Model Gateway
	gwCfg := gateway.DefaultConfig()
	gwCfg.EnableFixtureFallback = true
	gwSrv := gateway.NewServer(gwCfg, func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	})
	gwHTTP := httptest.NewServer(gwSrv.Handler())
	defer gwHTTP.Close()

	// 3. Start Observer Callback HTTP Server to receive OnCommandExit payload
	callbackCh := make(chan ChildTaskResult, 1)
	cbHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload ChildTaskResult
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			select {
			case callbackCh <- payload:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cbHTTP.Close()

	// 4. Seed Git workspace for checkout-service on INC-102
	gitMgr, err := storefront.NewGitWorkspaceManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewGitWorkspaceManager failed: %v", err)
	}
	wsDir := gitMgr.RepoDir()

	sc, _ := storefront.GetScenario(storefront.IncidentFlashSaleBundleUnderflow)
	checkoutDetail := sc.ServiceTraces[storefront.ServiceCheckout]

	taskName := "fixer-inc-102-checkout-service"
	mission := ChildTaskMission{
		TaskName:           taskName,
		ParentTask:         "triage-inc-102",
		IncidentID:         sc.ID,
		Service:            storefront.ServiceCheckout,
		Language:           checkoutDetail.Language,
		Harness:            "pi",
		RequestedModel:     "gemini-3.8-flash",
		SourceFile:         checkoutDetail.SourceFile,
		TestFile:           checkoutDetail.TestFile,
		ErrorType:          checkoutDetail.ErrorType,
		ErrorMessage:       checkoutDetail.ErrorMessage,
		StackTrace:         checkoutDetail.StackTrace,
		RegressionTestName: checkoutDetail.RegressionTestName,
	}
	cfgStruct, err := ChildMissionToStruct(mission)
	if err != nil {
		t.Fatalf("ChildMissionToStruct failed: %v", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable failed: %v", err)
	}

	taskSpec := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     taskName,
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Command: []string{exePath, "-test.run=^TestHelperProcessTaskWorker$"},
			Env: []*v1alpha1.EnvVar{
				{Name: "AX_BUGBASH_TEST_SUBPROCESS", Value: "1"},
				{Name: "MODEL_GATEWAY_URL", Value: gwHTTP.URL},
			},
			Workspace: &v1alpha1.WorkspaceRef{
				Name: DefaultWorkspaceName,
				Path: wsDir,
			},
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "1000m", Memory: "2048Mi"},
			},
			Config: cfgStruct,
		},
	}

	if _, err := axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: taskSpec}); err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if _, err := axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: "default", Name: taskName}); err != nil {
		t.Fatalf("ResumeTask failed: %v", err)
	}

	// Verify worker slot is allocated before Custom Runner runs
	if pool := axSrv.WorkerPoolSnapshot(); pool.ActiveTasksCount != 1 {
		t.Fatalf("expected 1 active task on worker pool before runner exit, got %d", pool.ActiveTasksCount)
	}

	// 5. Run CustomRunner (which embeds github.com/google/ax/runner.Run)
	err = RunCustomTaskRunner(ctx, CustomRunnerOptions{
		Port:                    findFreeLocalPort(t),
		MetadataPort:            findFreeLocalPort(t),
		Task:                    taskSpec,
		AXServerAddr:            axAddr,
		ObserverCallbackURL:     cbHTTP.URL,
		ModelGatewayURL:         gwHTTP.URL,
		StopRunnerOnCommandExit: true,
	})
	if err != nil {
		t.Fatalf("RunCustomTaskRunner failed: %v", err)
	}

	// 6. Verify callback received ChildTaskResult with unified git diff
	select {
	case cbRes := <-callbackCh:
		if cbRes.TaskName != taskName {
			t.Fatalf("callback TaskName = %q, want %q", cbRes.TaskName, taskName)
		}
		if !strings.Contains(cbRes.UnifiedGitDiff, "TestReserveFlashBundleOrderCapsBonusAtAvailableStock") {
			t.Fatalf("expected callback UnifiedGitDiff to include regression test, got:\n%s", cbRes.UnifiedGitDiff)
		}
		if !cbRes.Suspended {
			t.Fatalf("expected callback result to have Suspended=true")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for OnCommandExit observer callback")
	}

	// 7. Verify AXServer task was auto-suspended via gRPC SuspendTask and worker slot reclaimed
	gotTask, err := axClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: taskName})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if gotTask.GetStatus().GetPhase() != v1alpha1.PhaseSuspended {
		t.Fatalf("expected task phase %q after OnCommandExit auto-suspend, got %q", v1alpha1.PhaseSuspended, gotTask.GetStatus().GetPhase())
	}
	poolAfter := axSrv.WorkerPoolSnapshot()
	if poolAfter.ActiveTasksCount != 0 || poolAfter.TotalReclaimedSlots != 1 {
		t.Fatalf("expected 0 active tasks and 1 reclaimed slot after OnCommandExit, got %+v", poolAfter)
	}
}

func TestOrchestratorEndToEndBugBashTreeHealsStorefrontViaGitPush(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// 1. Initialize live Retail Storefront runtime with Smart HTTP Git repo
	sfRuntime, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("storefront.NewRuntime failed: %v", err)
	}
	sfHTTP := httptest.NewServer(sfRuntime.Handler())
	defer sfHTTP.Close()

	// 2. Activate INC-103 (Zero-Decimal JPY Rounding across storefront-web, pricing-service, checkout-service)
	sc, err := sfRuntime.ActivateIncident(storefront.IncidentZeroDecimalJPYRounding)
	if err != nil {
		t.Fatalf("ActivateIncident(INC-103) failed: %v", err)
	}

	// Verify Storefront checkout currently fails with HTTP 500
	cartBytes, _ := json.Marshal(sc.TriggerCart)
	respBefore, err := http.Post(sfHTTP.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		t.Fatalf("checkout before fix failed: %v", err)
	}
	_ = respBefore.Body.Close()
	if respBefore.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 before Bug Bash Tree remediation, got %d", respBefore.StatusCode)
	}

	// 3. Start Model Gateway
	gwCfg := gateway.DefaultConfig()
	gwCfg.EnableFixtureFallback = true
	gwSrv := gateway.NewServer(gwCfg, func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	})
	gwHTTP := httptest.NewServer(gwSrv.Handler())
	defer gwHTTP.Close()

	// 4. Create Orchestrator backed by embedded AXServer, Storefront, and Model Gateway
	orch, err := NewOrchestrator(ctx, OrchestratorConfig{
		Storefront:      sfRuntime,
		ModelGateway:    gwSrv,
		ModelGatewayURL: gwHTTP.URL,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}
	defer orch.Close()

	// 5. Trigger recursive Bug Bash Tree in Multi-Harness Parallel Mode ("multi")
	treeExec, err := orch.TriggerIncidentBugBash(ctx, TriggerRequest{
		IncidentID:  storefront.IncidentZeroDecimalJPYRounding,
		HarnessMode: "multi",
	})
	if err != nil {
		t.Fatalf("TriggerIncidentBugBash failed: %v", err)
	}

	if treeExec.Phase != "HEALED" || !treeExec.StorefrontHealed {
		t.Fatalf("expected tree execution to reach HEALED, got phase=%q healed=%v", treeExec.Phase, treeExec.StorefrontHealed)
	}
	if len(treeExec.ImplicatedServices) != 3 {
		t.Fatalf("expected exactly 3 implicated services for INC-103, got %v", treeExec.ImplicatedServices)
	}
	if len(treeExec.UnaffectedServices) != 2 {
		t.Fatalf("expected 2 unaffected services (shipping-service, inventory-service) untouched, got %v", treeExec.UnaffectedServices)
	}
	if len(treeExec.ChildTasks) != 3 {
		t.Fatalf("expected 3 ChildTaskResults, got %d", len(treeExec.ChildTasks))
	}

	wantHarnesses := []string{"antigravity", "opencode", "pi"}
	for i, child := range treeExec.ChildTasks {
		if child.Harness != wantHarnesses[i] {
			t.Fatalf("child[%d] harness = %q, want %q", i, child.Harness, wantHarnesses[i])
		}
		if !child.Suspended {
			t.Fatalf("expected child[%d] (%s) to be auto-suspended", i, child.TaskName)
		}
		if child.RedTestOutput == "" || child.GreenTestOutput == "" || child.UnifiedGitDiff == "" {
			t.Fatalf("expected child[%d] (%s) to have Red, Green, and UnifiedGitDiff populated", i, child.TaskName)
		}
	}

	if treeExec.MergedGitCommitSHA == "" {
		t.Fatalf("expected non-empty MergedGitCommitSHA after Root Task git push")
	}

	// 6. Verify all Substrate worker slots were reclaimed (1 Root Task + 3 Child Tasks = 4 slots reclaimed, 0 active)
	pool := orch.WorkerPoolSnapshot()
	if pool.ActiveTasksCount != 0 {
		t.Fatalf("expected 0 active tasks after Bug Bash Tree completion, got %d", pool.ActiveTasksCount)
	}
	if pool.TotalReclaimedSlots < 4 {
		t.Fatalf("expected >=4 reclaimed worker slots (root + 3 children), got %d", pool.TotalReclaimedSlots)
	}

	// 7. Re-run the exact same JPY checkout against the live Retail Storefront and verify HTTP 200 CONFIRMED!
	respAfter, err := http.Post(sfHTTP.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		t.Fatalf("checkout after fix failed: %v", err)
	}
	defer respAfter.Body.Close()
	if respAfter.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 after Root Task git push healed storefront, got %d", respAfter.StatusCode)
	}
}




