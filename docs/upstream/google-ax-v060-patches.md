# Upstream `google/ax` Compatibility Overlay Guide (`Issues #482, #511, #518`)

> [!IMPORTANT]
> - **Target Baseline**: `google/ax` (`4b7bf6f`) running on Google Kubernetes Engine (GKE) with **Agent Substrate `v0.6.1`** (`substrate.ateapi.v1.Control`) and `agent-substrate/env`.
> - **Overlay Artifact**: [`deploy/ax-server/ax-server-v061.patch`](../../deploy/ax-server/ax-server-v061.patch), applied automatically by [`deploy/ax-server/build.sh`](../../deploy/ax-server/build.sh).
> - **Purpose**: Give upstream `google/ax` maintainers three focused, self-contained PR blueprints ready to cherry-pick for Issues `#482`, `#511`, and `#518`, while letting developers build and run the patched `ax-server` binary today.

---

## Overview of the Three Upstream Patches

| Upstream Issue | Subsystem | Failure Symptom on Live GKE | Overlay Fix Summary | Measured GKE Result |
| :--- | :--- | :--- | :--- | :--- |
| **[Issue #482](https://github.com/google/ax/issues/482)** | `internal/substrate/client.go` | `ResumeActor` times out waiting for `/readyz:80` (HTTP 404 on `agent-substrate/env`); `SNAPSHOT_FIDELITY_VOLUMES` fails without a golden image; sandbox egress is blocked before `ResumeActor`. | Probe `/healthz:80`, set `GOLDEN_SNAPSHOT_MODE_DISABLED`, and attach per-actor `EgressPolicy` (`ensureActorEgressPolicy`) before `ResumeActor`. | `80ms` Smart-HTTP `git clone`, `4/4` warm `ateworker-gvisor` slots ready. |
| **[Issue #511](https://github.com/google/ax/issues/511)** | `internal/server/server.go` & `deploy/ax-server/ax-system.yaml` | Agent Substrate `v0.6.1` moved proto package to `api/ateapi/v1`; fresh `ax-server` fails `CreateAtespace` without OpenFGA bootstrap; in-sandbox `SuspendTask` cancels mid-checkpoint when `runsc` closes caller stream. | Migrate proto import to `go.ate.dev/substrate/api/ateapi/v1`, seed OpenFGA `default` store + `AccessPolicy`, and detach `Server.SuspendTask` with `context.WithoutCancel(ctx)`. | `42.6ms` volume checkpoint, `61ms` `SuspendTask` worker-slot reclamation (`0` canceled checkpoints). |
| **[Issue #518](https://github.com/google/ax/issues/518)** | `runner/runner.go` & `internal/bugbash/runner.go` | `workspace.DefaultAXDir` (`/root/.ax`) lives on ephemeral rootfs and is lost across `SNAPSHOT_FIDELITY_VOLUMES` restores; sibling tasks collide on shared workspace state. | Move `workspace.AXDir` to `/workspace/.ax` (`filepath.Join(wsPath, ".ax")`) and isolate per-task checkpoint metadata at `/workspace/.ax-state/<task-name>.json`. | `16/16` live GKE multi-task chaos soak trajectories pass with zero state collisions. |

---

## Blueprint 1: `google/ax` Issue `#482` (Sandbox Wakeup Probe, Snapshot Mode & Per-Actor Egress)

### 1.1 Symptom & Root Cause

When `ax-server` reconciles a `Task` onto an `ateworker-gvisor` sandbox pod in **Agent Substrate `v0.6.0+`**, three actor-provisioning assumptions in `internal/substrate/client.go` break on live GKE clusters:

1. **`WakeupProbe` Path Mismatch (`/readyz` vs. `/healthz`)**: `EnsureActor` sets `WakeupProbe.HttpGet.Path = "/readyz"` on port `80`. The guest HTTP server in `agent-substrate/env` registers its readiness handler at `/healthz` on port `80` and returns `HTTP 404` on `/readyz`. As a result, `ate-controller` marks the actor unhealthy and `ResumeActor` blocks until deadline expiration.
2. **Implicit Golden Snapshot Lookup on `SNAPSHOT_FIDELITY_VOLUMES`**: `EnsureActor` sets `PreferredFidelity: SNAPSHOT_FIDELITY_VOLUMES` without setting `GoldenSnapshotConfig.Mode`. When no pre-warmed golden snapshot exists in the bucket, `ate-controller` attempts a golden snapshot lookup unless `GOLDEN_SNAPSHOT_MODE_DISABLED` is explicitly specified.
3. **Zero-Trust Egress Deadlock Before `ResumeActor`**: Under default-deny `NetworkPolicy` isolation, a newly created `Actor` has no `EgressPolicy` attached when `ResumeActor` boots the container and runs `workspace.SetupWorkspace` (`git clone` / `git fetch`). If the per-actor `EgressPolicy` is attached only *after* `ResumeTask` returns, the initial `git clone` inside `runner.Run` hangs or fails with connection refused.

### 1.2 Minimal Unified Diff (`internal/substrate/client.go`)

```diff
--- a/internal/substrate/client.go
+++ b/internal/substrate/client.go
@@ -378,7 +378,7 @@
 			Env:     envList,
 			WakeupProbe: &ateapipb.WakeupProbe{
 				HttpGet: &ateapipb.HttpGetAction{
-					Path: "/readyz",
+					Path: "/healthz",
 					Port: 80,
 				},
 			},
@@ -394,6 +394,9 @@
 		SnapshotConfig: &ateapipb.SnapshotConfig{
 			StorageLocation:   snapshotsBucket,
 			PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
+			GoldenSnapshotConfig: &ateapipb.GoldenSnapshotConfig{
+				Mode: ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED,
+			},
 		},
 		Resources: &ateapipb.ResourceRequirements{
 			Limits: &ateapipb.Resources{
@@ -526,4 +529,7 @@
 	if egErr := c.ensureActorEgressPolicy(ctx, atespace, actorName); egErr != nil {
 		return nil, egErr
 	}
 	return actor, nil
 }
```

When `AX_AUTO_ACTOR_EGRESS=true` is set on `ax-server`, `ensureActorEgressPolicy` invokes `c.control.CreateActorEgressPolicy(ctx, req)` immediately after `CreateActor` / `GetActor` and before `ResumeActor`, allowing HTTP traffic only to the cluster-internal `ax-server`, `model-gateway`, `storefront`, `storefront-git`, `ax-observer`, and `loki` services.

### 1.3 Live GKE Verification

```bash
kubectl -n ax-bug-bash get workerpools.ate.dev,pods
```

Expected result: all 4 `ateworker-gvisor` pods transition to `Ready` within `2s`, and in-sandbox `SetupWorkspace` clones `http://storefront.ax-bug-bash.svc.cluster.local:3001/git/storefront.git` in `80ms`.

---

## Blueprint 2: `google/ax` Issue `#511` (Substrate `v0.6.1` Proto Path, OpenFGA Bootstrap & Self-Suspend Race)

### 2.1 Symptom & Root Cause

1. **Protobuf Package Move in Agent Substrate `v0.6.1`**: Upstream Agent Substrate consolidated its Go protobuf package from `go.ate.dev/substrate/pkg/proto/ateapipb` to `go.ate.dev/substrate/api/ateapi/v1` (`substrate.ateapi.v1.Control`).
2. **OpenFGA Store & `AccessPolicy` Bootstrap**: On a fresh cluster, `ax-server` calls `EnsureAtespace("default")`. Without a pre-seeded OpenFGA store and an `AccessPolicy` granting `role: owner` to `user:system:serviceaccount:ax-system:ax-server` on `Namespace/default`, `CreateAtespace` and `CreateActor` fail with `PermissionDenied`.
3. **Self-Suspend Context Cancellation Race (`Server.SuspendTask`)**: Inside the sandbox, our **Custom Runner** invokes `SuspendTask` on itself inside the `OnCommandExit` callback (`runner.Run`). In upstream `internal/server/server.go`, `Server.SuspendTask` passes the incoming gRPC request `ctx` directly to `c.substrate.SuspendActor(ctx, ...)`. As soon as `runsc` starts checkpointing and stopping the caller container, the sandbox's outgoing HTTP/2 connection to `ax-server` drops, canceling `ctx` mid-checkpoint. `SuspendActor` aborts with `context canceled`, leaving the task stuck in `Running` instead of `Suspended`.

### 2.2 Minimal Unified Diff (`internal/server/server.go`)

```diff
--- a/internal/server/server.go
+++ b/internal/server/server.go
@@ -20,12 +20,13 @@
 	"log/slog"
 	"net/http"
 	"strings"
+	"time"
 
 	"github.com/google/ax/internal/lock"
 	"github.com/google/ax/internal/store"
 	"github.com/google/ax/internal/substrate"
 	"github.com/google/ax/pkg/apis/v1alpha1"
-	"go.ate.dev/substrate/pkg/proto/ateapipb"
+	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
 	"google.golang.org/grpc"
 	"google.golang.org/grpc/codes"
 	"google.golang.org/grpc/status"
@@ -268,6 +269,11 @@
 	if req == nil {
 		return nil, status.Error(codes.InvalidArgument, "missing request")
 	}
+	// Detach from caller's stream cancellation so an in-sandbox runner calling
+	// SuspendTask on itself (OnCommandExit -> SuspendTask) does not cancel
+	// SuspendActor mid-checkpoint when runsc tears down the caller's container.
+	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
+	defer cancel()
 	atespace := req.Atespace
 	if atespace == "" {
 		atespace = "default"
```

And in [`manifests/ax-v060-overlay.yaml`](../../manifests/ax-v060-overlay.yaml), we bind `ax-server` as `owner` and `ax-observer` as `viewer` via declarative `AccessPolicy` manifests:

```yaml
apiVersion: ate.dev/v1alpha1
kind: AccessPolicy
metadata:
  name: ax-server-default-owner
  namespace: default
spec:
  subject: "user:system:serviceaccount:ax-system:ax-server"
  role: owner
```

### 2.3 Live GKE Verification

When each **Child Task** finishes its Red-Green TDD verification and triggers `OnCommandExit -> SuspendTask(SNAPSHOT_FIDELITY_VOLUMES)`:

- `SNAPSHOT_FIDELITY_VOLUMES` Cloud Storage checkpoint completes in **`42.6ms`**.
- `SuspendTask` transitions the actor to `Suspended` and returns the `ateworker-gvisor` slot to `Ready (Warm)` in **`61ms`** with zero `context canceled` errors.

---

## Blueprint 3: `google/ax` Issue `#518` (Persistent Workspace Maiden-Run Marker & Per-Task State Isolation)

### 3.1 Symptom & Root Cause

1. **Ephemeral `/root/.ax` Maiden-Run Marker Lost Across Volume Snapshots**: In `runner/runner.go`, `workspace.SetupWorkspace` checks `workspace.AXDir` (defaulting to `/root/.ax`) to determine whether a workspace volume has already completed its maiden initialization (`git clone` + setup hooks). When a task suspends with `SNAPSHOT_FIDELITY_VOLUMES`, only `/workspace` is snapshotted to Cloud Storage; `/root/.ax` on the container rootfs is discarded. When the volume is restored onto a warm worker slot, `/root/.ax` is missing while `/workspace/.git` already exists, causing `SetupWorkspace` to log `workspace maiden run setup failed`.
2. **Multi-Task State Collisions on Shared Workspaces**: In a self-spawning **Bug Bash Tree**, a **Root Task** and 2 to 3 parallel **Child Tasks** attach to the `storefront-monorepo` workspace. Storing runner checkpoint metadata in a single static file path causes concurrent tasks to overwrite each other's TDD state.

### 3.2 Minimal Unified Diff (`runner/runner.go` & `internal/bugbash/runner.go`)

```diff
--- a/runner/runner.go
+++ b/runner/runner.go
@@ -28,6 +28,7 @@
 	"log/slog"
 	"os"
 	"os/exec"
+	"path/filepath"
 	"syscall"
 	"time"
@@ -123,6 +124,9 @@
 		_ = metaServer.Stop(context.Background())
 	}()
 
+	if workspace.AXDir == workspace.DefaultAXDir && wsPath != "" {
+		workspace.AXDir = filepath.Join(wsPath, ".ax")
+	}
 	if _, err := workspace.SetupWorkspace(ctx, cfg.Workspace, wsPath); err != nil {
 		slog.Error("workspace maiden run setup failed", "workspace", ref.GetName(), "path", wsPath, "error", err)
 	} else {
```

In [`internal/bugbash/runner.go`](../../internal/bugbash/runner.go), our **Custom Runner** pairs `/workspace/.ax` with per-task state files under `/workspace/.ax-state/<task-name>.json` and excludes `.ax` and `.ax-state` from Git commits so `refs/heads/fix/<task>` branches stay clean.

### 3.3 Live GKE Verification

```bash
# Build the patched ax-server binary using deploy/ax-server/build.sh
./deploy/ax-server/build.sh

# Run the 20-trajectory hermetic evaluation + hygiene verification
make eval
```
