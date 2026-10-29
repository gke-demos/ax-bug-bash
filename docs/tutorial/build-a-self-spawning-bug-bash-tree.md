# Hands-On Tutorial: Build a Self-Spawning Bug Bash Tree on google/ax and Agent Substrate v0.6.0

This five-lab tutorial walks you through running, extending, and deploying a self-spawning **Bug Bash Tree** on `google/ax` and **Agent Substrate `v0.6.0`** on Google Kubernetes Engine (GKE). By the end of these five labs, you will have run the 5-service polyglot **Retail Storefront** locally in hermetic `SIMULATED` mode, inspected how the in-sandbox Go **Custom Runner** coordinates Red-Green TDD and volume checkpoints, configured the zero-secret **Model Gateway** on **Gemini Enterprise Agent Platform**, deployed the full stack to a live GKE cluster, and verified the sequential `4x4` chaos soak with Google Cloud telemetry dual-export.

For the architectural deep-dive and live GKE benchmark breakdown, read the companion blog post in [`docs/blog/self-spawning-bug-bash-tree-on-google-ax.md`](../blog/self-spawning-bug-bash-tree-on-google-ax.md).

![Live GKE Bug Bash Tree Terminal Recording](../assets/ax-live-gke-terminal.gif)

---

## Lab 1: Local Hermetic `SIMULATED` Mode and the 20-Trajectory Evaluation Matrix

In Lab 1, you will verify your local polyglot toolchains, launch all three decoupled production surfaces on `localhost` in hermetic `SIMULATED` **Execution Mode**, trigger **Incident Scenario** `INC-101` (`Stacked Promo Negative Subtotal Crash`), inspect the `/about` interactive SVG **Architecture Topology Catalog**, and run the 20-trajectory automated evaluation suite (`make eval`).

### Step 1.1: Verify Local Toolchain Prerequisites

The **Retail Storefront** runs five real microservice codebases across four languages (`Node.js/TypeScript`, `Go`, `Python/FastAPI`, and `Rust/Axum`). Confirm that `go`, `node`, `python3`, `cargo`, and `git` are on your `PATH`:

```bash
go version        # Go 1.23+
node --version    # Node.js 20+ or 22+
python3 --version # Python 3.11+
cargo --version   # Rust stable toolchain
git --version
```

Run the baseline microservice unit test suite across all five services to confirm that baseline CI passes before any **Incident Scenario** is activated:

```bash
./services/run-service-tests.sh --all
```

Every individual service test suite (`storefront-web`, `checkout-service`, `shipping-service`, `pricing-service`, and `inventory-service`) exits `0` because each service tests its own happy path in isolation.

### Step 1.2: Start the Three Decoupled Surfaces with `make run-local`

Launch all three decoupled surfaces locally in a single terminal using `make run-local`:

```bash
make run-local
```

`hack/run-local.sh` starts the **Retail Storefront** (`:3001`), the Prometheus and Alertmanager preview server (`:3002`), and the **AX Observer** (`:3000`):

| Surface | Local Endpoint | Role in the Bug Bash Tree |
| :--- | :--- | :--- |
| **Surface 1: Retail Storefront** | `http://localhost:3001` | Standalone 5-service polyglot e-commerce storefront (`storefront-web`, `checkout-service`, `shipping-service`, `pricing-service`, `inventory-service`) plus the embedded Smart-HTTP Git server (`/git/storefront.git`). |
| **Surface 2: Monitoring Stack** | `http://localhost:3002/monitoring` | Fast-scrape Prometheus metrics (`/metrics`), Alertmanager v4 webhook dispatcher, and structured JSON error logs with Loki trace breadcrumbs. |
| **Surface 3: AX Observer** | `http://localhost:3000` | Real-time visualizer for incoming alerts, the recursive **Root Task** and **Child Task** tree, per-service **Regression Test** and fix diffs, `/workers` warm slot status, and `/about` architecture topology. |

### Step 1.3: Trigger `INC-101` via the `⌘K` Modal or `curl`

You can trigger any of the four multi-service **Incident Scenarios** (`INC-101` through `INC-104`) either from the browser UI or from the command line:

1. **Via the Browser UI**: Open `http://localhost:3000`, press `⌘K` (or `Ctrl+K`), select **`INC-101` (Stacked Promo Negative Subtotal Crash)**, choose the **`multi`** **Coding Harness** mode, and click **Trigger Incident**.
2. **Via `curl` in a Second Terminal**:

```bash
# 1. Activate INC-101 on Surface 1 (:3001 Retail Storefront) and verify the live HTTP 500 checkout failure
curl -sS -X POST http://localhost:3001/api/incidents/activate \
  -H "Content-Type: application/json" \
  -d '{"incident_id":"INC-101"}' | jq .

curl -i -sS -X POST http://localhost:3001/api/cart/checkout \
  -H "Content-Type: application/json" \
  -d '{
    "currency": "USD",
    "promo_codes": ["SAVE20", "FLAT15"],
    "items": [
      {
        "sku": "sku-cable-usbc",
        "name": "Braided USB-C 240W Cable",
        "quantity": 1,
        "unit_price_cents": 1600,
        "weight_grams": 120,
        "hazmat_class": "NONE"
      }
    ]
  }'

# 2. Trigger the full Bug Bash Tree remediation on Surface 3 (:3000 AX Observer)
curl -sS -X POST http://localhost:3000/api/incidents/trigger \
  -H "Content-Type: application/json" \
  -d '{"incident_id":"INC-101","harness":"multi","model":"gemini-3.8-flash"}' | jq .
```

Submitting this `$16.00` (`1600` cents) `"sku-cable-usbc"` cart with stacked `SAVE20` (`$3.20` off) and `FLAT15` (`$15.00` off) promo codes produces `subtotal_cents = -220`, raising `NegativeSubtotalError` in `pricing-service` and `CarrierTariffBoundsError` in `shipping-service` (because baseline tests only checked carts over `$40.00`). Similarly, triggering `INC-102` (`"sku-mechanical-kb-bundle"`, `available = 2`, `requested = 2`, `bonus_bundle_units = 2`) causes a `2u32 - 4u32` unsigned underflow in `inventory-service` and `checkout-service`.

Inspect the live state snapshot from `/api/state` to see the **Root Task** (`triage-inc-101`) isolate only the 2 implicated services (`pricing-service` and `shipping-service`), skip the 3 healthy services (`storefront-web`, `checkout-service`, `inventory-service`), run Red-Green TDD (`test_stacked_promos_clamp_at_zero_subtotal` in Python and `TestQuoteCarrierRateZeroSubtotalStackedPromo` / `TestShippingQuoteClampsNegativeSubtotal` in Go) across **Antigravity** and **OpenCode**, merge the Git branches, and heal `:3001` **Retail Storefront** back to `HTTP 200`:

```bash
curl -sS http://localhost:3000/api/state | jq '{
  execution_mode: .execution_mode,
  incident_id: .latest_tree.incident_id,
  phase: .latest_tree.phase,
  implicated_services: .latest_tree.implicated_services,
  unaffected_services: .latest_tree.unaffected_services,
  rollout_duration_ms: .latest_tree.rollout_duration_ms,
  reclaimed_worker_slots: .worker_pool.total_reclaimed_slots
}'
```

### Step 1.4: Inspect `/about` and the Projector Light Mode Toggle

Navigate to `http://localhost:3000/about` in your browser to explore the three interactive SVG tabs powered by `internal/observer/about_catalog.go`:

- **Tab 1 (`3-Surface Mesh & Blast-Radius Explorer`)**: Click through `INC-101`..`INC-104` to highlight which microservices are implicated and which unaffected services stay untouched.
- **Tab 2 (`6-Stage Recursive Task-Tree & TDD Stepper`)**: Step through the 6 lifecycle stages from Alertmanager webhook arrival through **Child Task** `REPRODUCE_RED` and `PATCH_GREEN` to `SuspendTask(SNAPSHOT_FIDELITY_VOLUMES)` and `< 3s` selective hot-reload.
- **Tab 3 (`Zero-Trust Security, 4-Model Cascade Simulator & 12 ADRs`)**: Simulate upstream `429` and `503` responses across the 4-tier Gemini Flash cascade (`gemini-3.8-flash` -> `gemini-3.7-flash` -> `gemini-3.6-flash` -> `gemini-3.5-flash`) and browse all 12 Architecture Decision Records.

Click the icon-only **Sun / Moon** button in the top-right header to toggle high-contrast **Projector Light Mode**. The theme switch uses pure CSS custom property overrides (`html[data-theme="light"]`) so switching between dark and light modes produces zero layout shift.

### Step 1.5: Run the 20-Trajectory Evaluation Suite (`make eval`)

Stop `make run-local` (`Ctrl+C`) and run the complete Tier-1 evaluation matrix:

```bash
make eval
```

`make eval` executes 20 end-to-end trajectories in parallel (`TestFourScenariosAcrossAllHarnessesAndLiveCascade`) plus the static manifest and markdown hygiene contract (`TestRepositoryHygieneAndManifestContract` and `TestVerifyMarkdownHumanizerAndHygiene`):

- **16 `FIXTURE` Matrix Trajectories**: Every combination of `INC-101`, `INC-102`, `INC-103`, and `INC-104` across `antigravity`, `opencode`, `pi`, and `multi`, grading all 6 architectural invariants (`BaselineCIGreen`, `AlertmanagerWebhookTriggered`, `SelectiveBlastRadiusFanOut`, `RedGreenRegressionTDD`, `OnCommandExitAndSuspendReclaimed`, and `PreWarmedRolloutAndStorefrontHealed`).
- **4 `LIVE` Cascade Trajectories**: Exercises the **Model Gateway** against an upstream **Gemini Enterprise Agent Platform** test server with simulated `429` and `503` capacity drops across 0, 1, 2, and 3 leading models to verify every tier of the fallback cascade (`gemini-3.8-flash`, `gemini-3.7-flash`, `gemini-3.6-flash`, and `gemini-3.5-flash`).

---

## Lab 2: Authoring a Custom Runner on `google/ax` (`runner.Run`, `OnCommandExit`, and `SuspendTask`)

In Lab 2, you will examine how `cmd/ax-bug-bash-runner/main.go` and `internal/bugbash/runner.go` embed `github.com/google/ax/runner.Run` inside the `ateworker-gvisor` sandbox to decode structured mission payloads, enforce Red-Green TDD, persist volume state, and auto-suspend upon completion.

### Step 2.1: How the Outer Runner Supervises the Inner `--execute-task` Process

When an `ateworker-gvisor` sandbox boots a `google/ax` `Task`, `/usr/local/bin/ax-task-runner` starts as the container entrypoint (`cmd/ax-bug-bash-runner/main.go`). Without flags, it invokes `bugbash.RunCustomTaskRunner`, which configures `runner.Config` and delegates guest probe serving (`/healthz` and `/readyz` on port `80`) and loopback metadata serving (`/metadata/v1alpha1/ax/task` on port `8081`) to `runner.Run`:

```go
// cmd/ax-bug-bash-runner/main.go
if executeTask {
	return bugbash.ExecuteTaskFromMetadataEndpoint(ctx, bugbash.TaskExecutionOptions{
		ModelGatewayURL: opts.ModelGatewayURL,
		RoleOverride:    normRole,
	})
}
return bugbash.RunCustomTaskRunner(ctx, opts)
```

Inside `RunCustomTaskRunner` (`internal/bugbash/runner.go`), if `Task.spec.command` is empty or set to `["ax-task-runner"]`, the runner rewrites the supervised command to `["/usr/local/bin/ax-task-runner", "--execute-task"]` and creates `/workspace/.ax` and `/workspace/.ax-state` on the mounted volume before starting `runner.Run`.

### Step 2.2: Decoding Free-Form `Task.spec.config` (`google.protobuf.Struct`)

When `runner.Run` spawns the inner `ax-task-runner --execute-task` child process, `ExecuteTaskFromMetadataEndpoint` fetches the live `Task` specification over loopback HTTP from `$AX_METADATA_URL/metadata/v1alpha1/ax/task` and inspects `Task.spec.config` (`google.protobuf.Struct`):

- When `role == "root-triage"`, the **Root Task** parses the Alertmanager webhook payload and Loki stack traces (`AnalyzeIncidentTraces`), isolates the 2 to 3 implicated microservices, and calls `ax.v1alpha1.AX/CreateTask` and `ResumeTask` on `ax-server` (`SpawnChildTasksForTriage` in `internal/bugbash/triage.go`) to fan out one **Child Task** per implicated service.
- When `role == "child-fixer"`, the **Child Task** decodes its scoped `ChildFixerMission` from `Task.spec.config` (`ParseChildMissionFromTaskConfig`):

```go
// internal/bugbash/triage.go
type ChildFixerMission struct {
	TaskName           string         `json:"task_name"`
	ParentTask         string         `json:"parent_task"`
	IncidentID         string         `json:"incident_id"`
	Service            string         `json:"service"`
	Language           string         `json:"language"`
	Harness            string         `json:"harness"`
	RequestedModel     string         `json:"requested_model"`
	SourceFile         string         `json:"source_file"`
	TestFile           string         `json:"test_file"`
	RegressionTestName string         `json:"regression_test_name"`
	ErrorMessage       string         `json:"error_message"`
	StackTrace         string         `json:"stack_trace"`
	FailingRequest     map[string]any `json:"failing_request"`
}
```

### Step 2.3: Executing the Two-Phase Red-Green TDD Loop (`REPRODUCE_RED` -> `PATCH_GREEN`)

Inside `ExecuteSandboxChildTask` (`internal/bugbash/live_sandbox.go`), each **Child Task** clones the monorepo from the in-cluster Git server (`http://storefront.ax-bug-bash.svc.cluster.local:3001/git/storefront.git`) in `80ms` and enforces two sequential verification gates:

1. **`REPRODUCE_RED`**: The **Child Task** runs its assigned **Coding Harness** (`antigravity`, `opencode`, or `pi`) to add `RegressionTestName` to `TestFile`. Before executing the language test runner (`npm test`, `go test`, `python3 -m unittest`, or `cargo test`), the sandbox restores `SourceFile` from baseline Git `HEAD` so the test runs against unpatched service code. This step passes only when the test suite exits non-zero on the new **Regression Test**.
2. **`PATCH_GREEN`**: The **Child Task** runs its **Coding Harness** to repair `SourceFile` while preserving the new **Regression Test** in `TestFile`. This step passes only when the full service test suite exits `0`. The **Child Task** then commits `services/<service>` and pushes `HEAD:refs/heads/fix/<task-name>` to `storefront-git`.

### Step 2.4: Capturing `UnifiedGitDiff` in `OnCommandExit` and Calling `SuspendTask(SNAPSHOT_FIDELITY_VOLUMES)`

When the `--execute-task` child process exits, `runner.Run` invokes the `OnCommandExit` callback configured in `internal/bugbash/runner.go`. This hook performs three actions in sequence:

1. Loads the task state from `/workspace/.ax-state/<task-name>.json` and captures `UnifiedGitDiff` via `CaptureWorkspaceGitDiff(wsPath, "")` if not already populated.
2. Persists the completed `ChildTaskResult` (including `RegressionTestDiff`, `CodeFixDiff`, `UnifiedGitDiff`, `TDDSteps`, and `SuspendedAt`) back to `/workspace/.ax-state/<task-name>.json` so volume checkpoints (`SNAPSHOT_FIDELITY_VOLUMES`) retain full state across suspend and resume cycles.
3. Calls `ax.v1alpha1.AX/SuspendTask` over gRPC (`suspendTaskViaGRPC`) to release the `ateworker-gvisor` sandbox slot:

```go
// internal/bugbash/runner.go
OnCommandExit: func(exit runner.CommandExit) {
	res, err := LoadTaskStateSnapshot(wsPath, taskName)
	if err != nil {
		res = ChildTaskResult{TaskName: taskName}
	}
	if res.UnifiedGitDiff == "" {
		res.UnifiedGitDiff = CaptureWorkspaceGitDiff(wsPath, "")
	}
	res.ExitCode = exit.ExitCode
	res.Suspended = true
	res.SuspendedAt = time.Now().UTC()
	_ = SaveTaskStateSnapshot(wsPath, res)

	if axServerAddr != "" {
		_ = suspendTaskViaGRPC(axServerAddr, atespace, taskName)
	}
},
```

Inside `ax-server`, handling self-suspension from an in-sandbox caller requires detaching the gRPC request context with `context.WithoutCancel(ctx)` before calling `SuspendActor` on **Agent Substrate `v0.6.0`**. Otherwise, when `runsc` tears down the sandbox network namespace during suspension, the caller's HTTP/2 stream resets and cancels the volume snapshot upload mid-flight. With `context.WithoutCancel(ctx)`, the `SNAPSHOT_FIDELITY_VOLUMES` Cloud Storage checkpoint completes in `42.6ms` and the warm worker slot returns to `Ready (Warm)` in `61ms`.

Run the unit tests covering the **Custom Runner**, `OnCommandExit`, and `SuspendTask` lifecycle:

```bash
go test -v -run "TestCustomTaskRunner|TestRootTriage" ./internal/bugbash/...
```

---

## Lab 3: Configuring the Zero-Secret Model Gateway and Multi-Harness Sandbox

In Lab 3, you will inspect how `internal/gateway/` keeps cloud credentials out of agent sandboxes by exchanging a placeholder token (`ax-sandbox-token`) for GKE Workload Identity OAuth2 tokens, how the 4-tier Gemini Flash cascade designs for **compute obtainability** on **Gemini Enterprise Agent Platform**, and how `harnesses/Dockerfile.task-runner` packages **Antigravity**, **OpenCode**, and **Pi**.

### Step 3.1: Zero-Secret Token Exchange in `internal/gateway/gateway.go`

Task sandboxes inside `WorkerPool/ax-bug-bash-workers` execute generated tests and code patches, so they never receive Google Cloud service account keys or raw OAuth2 tokens. Instead, `harnesses/Dockerfile.task-runner` sets dummy environment variables inside the container:

```dockerfile
ENV GATEWAY_BASE_URL="http://model-gateway.ax-system.svc.cluster.local:8080" \
    GEMINI_API_KEY="ax-sandbox-token" \
    OPENAI_API_KEY="ax-sandbox-token" \
    AGENT_MODEL="gemini-3.8-flash"
```

When a **Coding Harness** sends a request to `http://model-gateway.ax-system.svc.cluster.local:8080/s/{task_name}/...`, the **Model Gateway** (`internal/gateway/gateway.go`) strips `ax-sandbox-token` and calls `DefaultWorkloadIdentityTokenFunc()`, which fetches and caches a short-lived Google Cloud OAuth2 access token from the GKE Workload Identity metadata server (`http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token`) before forwarding the request to **Gemini Enterprise Agent Platform** (`https://aiplatform.googleapis.com`).

### Step 3.2: Designing for Compute Obtainability with the 4-Tier Gemini Flash Cascade

When multiple **Child Tasks** fan out simultaneously during an incident, a single model endpoint can return `429 Too Many Requests` or `503 Service Unavailable`. To design for **compute obtainability**, `internal/gateway/cascade.go` defines an ordered 4-tier Gemini Flash fallback cascade:

```go
// internal/gateway/gateway.go
var DefaultFallbackCascade = []string{
	"gemini-3.8-flash",
	"gemini-3.7-flash",
	"gemini-3.6-flash",
	"gemini-3.5-flash",
}
```

`ExecuteGeminiCascade` (`internal/gateway/cascade.go`) iterates through `BuildCascadeOrder(requestedModel, s.cfg.FallbackCascade)`:

- If **Gemini Enterprise Agent Platform** returns `HTTP 200`, the gateway records `SourceProvenance: LIVE`, sets the `X-AX-Active-Model` response header to the tier that succeeded, and updates the per-task session ledger (`/api/gateway/sessions/{task_name}`).
- If the upstream endpoint returns `429`, `500`, `502`, `503`, `504`, or a network timeout, `ShouldFallback(statusCode, err)` returns `true` and the gateway retries the next model in the cascade (`gemini-3.8-flash` -> `gemini-3.7-flash` -> `gemini-3.6-flash` -> `gemini-3.5-flash`).

### Step 3.3: Packaging Antigravity, OpenCode, and Pi in `harnesses/Dockerfile.task-runner`

Inspect `harnesses/Dockerfile.task-runner` and `harnesses/bin/` to see how all three CLI **Coding Harnesses** are installed and pre-configured for single-shot non-interactive execution inside `ateworker-gvisor`:

1. **Antigravity (`google-antigravity` via `/usr/local/bin/antigravity`)**: Installed in `/opt/ax-venv` and configured to call the native Gemini REST endpoint (`/s/{task_name}/v1beta/models/{model}:generateContent`) on the **Model Gateway**.
2. **OpenCode (`opencode-ai` via `/usr/local/bin/opencode-harness`)**: Installed globally via `npm install -g opencode-ai` with `/root/.config/opencode/opencode.json` pointing to the gateway's OpenAI-compatible `/s/{task_name}/v1/chat/completions` route.
3. **Pi (`@mariozechner/pi-coding-agent` via `/usr/local/bin/pi-harness`)**: Installed globally via `npm install -g @mariozechner/pi-coding-agent` with `/root/.pi/agent/models.json` pointing to `/s/{task_name}/v1/chat/completions`.

Notice lines 63 to 68 of `harnesses/Dockerfile.task-runner`: during `docker build`, the image compiles the Go test binaries (`checkout-service` and `shipping-service`), runs `cargo test --no-run` for `inventory-service` to populate `/var/cache/ax-build/cargo-target`, and pre-compiles Python bytecode for `pricing-service`. Pre-warming these caches inside the image is what allows a **Child Task** to run `go test` in `4ms` and `cargo test` in `340ms` inside a fresh gVisor sandbox.

Run the **Model Gateway** unit test suite to verify token exchange, OpenAI-to-Gemini protocol translation, and the 4-tier fallback cascade:

```bash
go test -v ./internal/gateway/...
```

---

## Lab 4: Deploying to Google Kubernetes Engine with Agent Substrate `v0.6.0`

In Lab 4, you will validate the Kubernetes and Agent Substrate manifests (`make verify-manifests`), inspect the Layer-1 security and sandbox configurations in `manifests/ax-v060-overlay.yaml`, and deploy the system to your active GKE cluster context (`make deploy`).

### Step 4.1: Verify Your Active GKE Context and Manifest Contract

`hack/deploy-gke.sh` and `Makefile` resolve your target cluster and project dynamically from `kubectl config current-context` and `gcloud config get-value project` without hardcoded cluster or project names. Confirm your active context and run the dry-run manifest verification gate:

```bash
kubectl config current-context
gcloud config get-value project

make verify-manifests
```

`make verify-manifests` renders `manifests/bug-bash-system.yaml` with digest-pinned `@sha256:` image references and runs `TestRepositoryHygieneAndManifestContract` to verify every required resource and hygiene rule.

### Step 4.2: Understand `WorkerPool`, OpenFGA `AccessPolicy`, and Per-Actor `EgressPolicy`

Open `manifests/ax-v060-overlay.yaml` and examine the three core **Agent Substrate `v0.6.0`** declarations:

1. **Namespace Opt-In (`ate.dev/api-access: "true"`)**: Both `ax-system` and `ax-bug-bash` carry the `ate.dev/api-access: "true"` label so the Agent Substrate controller reconciles resources in those namespaces.
2. **Pre-Warmed `ateworker-gvisor` `WorkerPool`**: `WorkerPool/ax-bug-bash-workers` keeps 4 warm gVisor sandbox replicas (`SandboxConfig/gvisor-default`) ready in `ax-bug-bash` with `SNAPSHOT_FIDELITY_VOLUMES` enabled:

```yaml
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: ax-bug-bash-workers
  namespace: ax-bug-bash
  annotations:
    ax.dev/worker-runtime: "ateworker-gvisor"
    ax.dev/snapshot-fidelity: "SNAPSHOT_FIDELITY_VOLUMES"
spec:
  replicas: 4
  workerImage: ${ATEWORKER_GVISOR_IMAGE}
  sandboxClasses:
    - name: gvisor
      configRef:
        name: gvisor-default
```

3. **Least-Privilege OpenFGA `AccessPolicy`**: Grants `role: owner` to `user:system:serviceaccount:ax-system:ax-server` so `ax-server` can create Atespaces, actors, and volume snapshots, while granting read-only `role: viewer` to `user:system:serviceaccount:ax-bug-bash:ax-observer`:

```yaml
apiVersion: ate.dev/v1alpha1
kind: AccessPolicy
metadata:
  name: ax-server-global-owner
spec:
  bindings:
    - role: owner
      members:
        - user:system:serviceaccount:ax-system:ax-server
    - role: viewer
      members:
        - user:system:serviceaccount:ax-bug-bash:ax-observer
```

When `ax-server` creates an actor inside `WorkerPool/ax-bug-bash-workers`, it calls `CreateActorEgressPolicy` (`substrate.ateapi.v1.Control`) before `ResumeActor`, resolving concrete service ClusterIPs (`ax-server`, `model-gateway`, `storefront-git`, `ax-observer`, and `loki`) so `atenet-egress` allows internal control-plane and model traffic while blocking arbitrary external internet access.

### Step 4.3: Build Digest-Pinned Images and Run `make deploy`

Build and push the container images to Artifact Registry and deploy the full stack to GKE:

```bash
# Optional: build and push fresh images if you modified Go or service code
make images

# Apply the Agent Substrate v0.6.0 overlay, monitoring stack, and bug-bash-system manifests
make deploy
```

Verify that all pods in `ax-system`, `ax-bug-bash`, and `monitoring` are `Running` and that `WorkerPool/ax-bug-bash-workers` reports 4 warm replicas:

```bash
kubectl -n ax-bug-bash get workerpools.ate.dev,pods
kubectl -n ax-system get pods
kubectl -n monitoring get pods
```

---

## Lab 5: Running the Sequential `4x4` Live GKE Chaos Soak and Verifying Google Cloud Dual-Export

In Lab 5, you will port-forward the three live GKE services to your workstation, run the sequential `4x4` live GKE chaos soak (`make eval-live-gke`), and inspect the stage-safe scrubbed JSON report at `/tmp/ax-live-gke-eval-report.json`.

### Step 5.1: Port-Forward the Three Live GKE Surfaces

Open a background terminal (or run the commands below in background subshells) to forward the three cluster services using their exact service ports (`3000:3000`, `3001:3001`, `3002:3002`) plus `storefront-git` (`3003:8090`) and `prometheus` (`9090:9090`):

```bash
kubectl -n ax-bug-bash port-forward svc/ax-observer 3000:3000 >/tmp/pf-observer.log 2>&1 &
kubectl -n ax-bug-bash port-forward svc/storefront 3001:3001 >/tmp/pf-storefront.log 2>&1 &
kubectl -n monitoring port-forward svc/grafana 3002:3002 >/tmp/pf-grafana.log 2>&1 &
kubectl -n ax-bug-bash port-forward svc/storefront-git 3003:8090 >/tmp/pf-git.log 2>&1 &
kubectl -n monitoring port-forward svc/prometheus 9090:9090 >/tmp/pf-prometheus.log 2>&1 &
```

Verify that all local forwarders respond with `HTTP 200`:

```bash
curl -fsS http://127.0.0.1:3000/healthz
curl -fsS http://127.0.0.1:3001/healthz
curl -fsS http://127.0.0.1:3002/api/health
```

### Step 5.2: Run `make eval-live-gke` (`MaxConcurrency = 1`)

Execute the Tier-2 live GKE chaos soak:

```bash
make eval-live-gke
```

`make eval-live-gke` runs `TestLiveGKEFourByFourChaosSoakAndDualExport` (`internal/evals/live_gke.go`) with `MaxConcurrency = 1` across four orthogonal `(Incident Scenario, Coding Harness)` pairs on real `ateworker-gvisor` sandboxes:

1. **`INC-101` x `antigravity`** (`pricing-service` + `shipping-service`)
2. **`INC-102` x `opencode`** (`inventory-service` + `checkout-service`)
3. **`INC-103` x `pi`** (`storefront-web` + `pricing-service` + `checkout-service`)
4. **`INC-104` x `multi`** (`inventory-service` + `shipping-service`)

Between every trajectory, `RunLiveGKEChaosSoak` resets `storefront-git` to the clean `baseline-v1` tag (`POST /api/incidents/reset`), waits for Prometheus (`GET /api/v1/alerts`) to drain `StorefrontHTTP500Spike` back to zero active alerts, and fails closed if any **Child Task** reports `SourceProvenance == "FIXTURE"`.

### Step 5.3: Inspect the Scrubbed Report at `/tmp/ax-live-gke-eval-report.json`

When all four live trajectories and the Google Cloud telemetry queries finish, `RunLiveGKEChaosSoak` scrubs all cluster, project, node, and bucket identifiers (`ScrubStageSafeString`) and writes `/tmp/ax-live-gke-eval-report.json` with `0600` file permissions. Inspect the summary and dual-export verification block:

```bash
ls -l /tmp/ax-live-gke-eval-report.json
jq '{
  all_passed: .all_passed,
  max_concurrency: .max_concurrency,
  passed_trajectories: .passed_trajectories,
  dual_export: .dual_export,
  trajectories: [
    .trajectories[] | {
      incident_id,
      harness_mode,
      source_provenance,
      active_model,
      implicated_services,
      rollout_duration_ms,
      alert_drained_after_reset,
      passed
    }
  ]
}' /tmp/ax-live-gke-eval-report.json
```

Confirm that `all_passed` is `true`, every trajectory reports `source_provenance: "LIVE"` with `active_model: "gemini-3.8-flash"`, and `dual_export.cloud_logging_verified` and `dual_export.managed_prometheus_verified` are both `true`, proving that structured HTTP 500 traces reached Google Cloud Logging (`entries:list`) and `prometheus.googleapis.com/storefront_http_requests_total/counter` time series reached Google Cloud Managed Service for Prometheus.
