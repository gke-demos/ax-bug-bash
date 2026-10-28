# Upstream `google/ax` (`4b7bf6f`), Agent Substrate `v0.6.0` (`ate.dev/v1` + `kubectl-ate`), and Non-Interactive Harness CLI Contract

> **Context**: Primary-source research resolving [Upstream google/ax (4b7bf6f) Control Plane, ate.dev/v1 WorkerPool + kubectl-ate Policies & Non-Interactive Harness CLI Contract](https://github.com/gke-demos/ax-bug-bash/issues/10) for parent map [Live GKE google/ax + Agent Substrate v0.6.0 Execution, Real Polyglot Service Mesh & Interactive /about Visualizer Wayfinder Map](https://github.com/gke-demos/ax-bug-bash/issues/9).
>
> **Primary Sources Inspected**:
> - `google/ax` at commit `4b7bf6f` (`go.mod`, `deploy/ax-server.yaml`, `deploy/postgres.yaml`, `cmd/ax-server/main.go`, `internal/controller/reconciler.go`, `internal/substrate/client.go`, `internal/server/server.go`, `internal/store/postgres/{schema.sql,store.go,lock.go}`, `internal/workspace/setup.go`, `internal/metadata/server.go`, `pkg/apis/v1alpha1/{ax.proto,types.go,resources.go}`, `runner/runner.go`)
> - `agent-substrate/substrate` at `v0.6.0` (`ateapi.proto`, live GKE `ate.dev/v1` CRDs, `ate-system` Deployments/ConfigMaps/NetworkPolicies, `cmd/atenet/internal/router/{README.md,egress/egress.go,egress/request.go}`, `internal/egresspolicy/egresspolicy.go`, `docs/guides/{authorization.md,egress-traffic.md,network-egress.md,egress-trust-bundle.md}`, `docs/reference/kubectl-ate.md`)
> - CLI Coding Harnesses & `ax-bug-bash` runner (`pypi:google-antigravity@0.1.23`, `npm:opencode-ai@1.19.5` + `https://opencode.ai/config.json`, `npm:@mariozechner/pi-coding-agent@0.73.1`, `harnesses/Dockerfile.task-runner`, `harnesses/config/*`, `harnesses/bin/*`)

---

## Executive Summary & Critical Pre-Deployment Findings

Before deploying `ax-system` and `ax-bug-bash` to a live GKE cluster running Agent Substrate `v0.6.0`, downstream implementation tickets **must** account for seven primary-source mismatches discovered between current `ax-bug-bash` manifests/configs and the live upstream contracts:

1. **Three Distinct API Planes (Cannot Mix in `kubectl apply`)**:
   - **Kubernetes CRDs (`kubectl apply`)**: `WorkerPool` and `SandboxConfig` are `apiVersion: ate.dev/v1` (not `ate.dev/v1alpha1`). `WorkerPool.spec` uses `workerImage` (digest-pinned `@sha256:...`, not `runtimeImage`) and `spec.template.resources` (there is no `spec.snapshotConfig` or top-level `spec.resources`).
   - **Substrate Control gRPC (`kubectl-ate` -> `substrate.ateapi.v1.Control`)**: `Atespace`, `AccessPolicy`, `ActorTemplate`, `Actor`, and `EgressPolicy` live in Substrate's PostgreSQL database, **not** Kubernetes `etcd`. Having `kind: AccessPolicy` in `manifests/ax-v060-overlay.yaml` fails `kubectl apply` with `no matches for kind "AccessPolicy"`.
   - **AX Control gRPC (`ax` CLI / Go `ax.v1alpha1.AXClient`)**: `Workspace`, `Model`, and `Task` live in `ax-postgres`, **not** Kubernetes CRDs. Having `kind: Workspace` and `kind: Model` in `manifests/ax-v060-overlay.yaml` fails `kubectl apply`; additionally, `ModelSpec` in `ax.proto` has no `fallbackCascade` field and `ax` CLI rejects unknown YAML fields.
2. **OpenFGA Global `owner` Required for `ax-server` (`CreateAtespace`)**:
   - `internal/controller/reconciler.go:146` calls `EnsureAtespace` -> `CreateAtespace` unconditionally on every task reconciliation. In Substrate `v0.6.0` (`docs/guides/authorization.md:169`), `CreateAtespace` checks `can_create_atespace` on `global`, which **only `role: owner` in the `--global` `AccessPolicy`** grants (atespace-scoped `owner`/`editor` fails with `PermissionDenied` before `AlreadyExists` can be returned). Furthermore, `AccessPolicy` bindings use `role: owner|editor|viewer` and `members: ["user:system:serviceaccount:ax-system:ax-server"]` (not `role: admin` or `subjects:`).
3. **`ax-server` Port Alignment (`:80` / `:8080` vs `:9090`)**:
   - `cmd/ax-server/main.go:54` listens on `--addr=:8080` (`h2c` multiplexing HTTP `/healthz` + gRPC `ax.v1alpha1.AX`), and `deploy/ax-server.yaml:25-39` exposes `Service/ax-server` on `port: 80 -> targetPort: 8080`. `manifests/bug-bash-system.yaml:219` currently sets `AX_SERVER_ADDR` to `:9090` and must be updated to `ax-server.ax-system.svc.cluster.local:80` (or `:8080` if service port `8080` is exposed).
4. **Per-Task `ActorTemplate` Synthesis & No Pre-Created `default-template` Needed**:
   - Although `ax-server` accepts `--template=default-template` and `--template-atespace=ax-system`, `EnsureActorTemplateWithImage` (`internal/substrate/client.go:409-423`) ignores both base template arguments and **always** synthesizes a per-task immutable `ActorTemplate` named `<taskName>-tmpl-<8-hex-sha256>` in the task's own atespace (`default`).
5. **Per-Actor `EgressPolicy` Required for Cluster-Internal Services & `h2c` gRPC**:
   - `google/ax` at `4b7bf6f` has **no `Task.spec.egress` field** (`pkg/apis/v1alpha1/ax.proto:69-89`) and creates Actors with no `EgressPolicy`. Without an `EgressPolicy`, the sandbox DNS relay (`169.254.17.1:53`) returns `NXDOMAIN` for all queries and `atenet-egress` denies all TCP tunnels (`403 egress denied`).
   - Creating a per-actor `EgressPolicy` (for `<taskName>` in atespace `default`, where `Actor.name == Task.name`) with `http` rules and explicit `ports.numbers` allows `ateworker-gvisor` guests to reach cluster-internal `.svc.cluster.local` endpoints over both **HTTP/1.1 and `h2c` gRPC** (`ateapi.proto:658`, `internal/egresspolicy/egresspolicy.go:410-420`) with **no MITM CA trust bundle needed**.
6. **Golden Snapshot Build vs `Workspace.spec.git` Cold Boot**:
   - `BuildActorTemplate` leaves `golden_snapshot` at its default (`GOLDEN_SNAPSHOT_MODE_ENABLED`), so `CreateActorTemplate` boots a temporary UUID golden actor that has no `EgressPolicy`. When `Workspace.spec.git` is set, `SetupWorkspace` (`internal/workspace/setup.go:43-44, 101-104`) fails `git fetch` 5 times (`~10s` backoff), withholds `/ax/initialized-workspace`, and still returns `nil` error so `/readyz:80` returns `200 OK` within the 30s `WakeupProbe` window. Attaching the `EgressPolicy` to `<taskName>` **before** calling `ResumeTask` allows `SetupWorkspace` (or the custom runner) to clone `<500ms` on resume.
7. **`harnesses/Dockerfile.task-runner` & `pi-models.json` Fixes Needed**:
   - `harnesses/Dockerfile.task-runner:64` runs `node --check /tmp/prewarm/services/storefront-web/test_currency.mjs`, which does not exist (actual test is `services/storefront-web/test/storefront.test.mjs`) and breaks `docker build`.
   - `harnesses/config/pi-models.json` uses `"type": "openai-compatible"`, whereas `@mariozechner/pi-coding-agent@0.73.1` (`dist/core/model-registry.js`) requires `"api": "openai-completions"` under `providers.<name>` and reads `defaultProvider`/`defaultModel` from `settings.json` or CLI `--provider` / `--model` flags.
   - `opencode.json` requires `"permission": "allow"` (or `OPENCODE_PERMISSION='{"*":"allow"}'`) for guaranteed zero-prompt headless `bash`/`edit` execution under `opencode run`, and dynamic `/s/${AX_TASK_NAME}/v1` base URL injection per task.

---

## Part 1: Upstream `google/ax` (`4b7bf6f`) Control Plane, PostgreSQL & Runner Contract

### 1.1 Module Dependencies & `cmd/ax-server` Flags / Environment Variables

`google/ax/go.mod` (`line 9`) pins `go.ate.dev/substrate v0.6.0`. `cmd/ax-server/main.go` (`lines 53–77, 121–128`) and `internal/controller/reconciler.go` / `internal/substrate/client.go` read the following configuration:

| Flag | Env Override / Fallback | Default | Purpose & Source Citation |
| :--- | :--- | :--- | :--- |
| `--addr` | `ADDR` | `":8080"` | Single TCP listener serving unencrypted HTTP/2 (`h2c` gRPC `ax.v1alpha1.AX`) + HTTP/1.1 `GET /healthz` (`main.go:54`, `internal/server/server.go:91-104`). |
| `--store` | `AX_STORE` (+ libpq `PGPASSWORD`) | `"postgres://ax@localhost:5432/ax?sslmode=disable"` | `pgxpool` connection string (`main.go:55`, `deploy/ax-server.yaml:78-87`). |
| `--substrate-endpoint` | — | `"api.ate-system.svc.cluster.local:443"` | `ate-api-server` gRPC endpoint (`main.go:56`). |
| `--substrate-authority` | — | `"api.ate-system.svc"` | TLS `ServerName` / HTTP/2 `:authority` for `ate-api-server` (`main.go:57`). |
| `--substrate-token-file` | `SUBSTRATE_TOKEN_FILE`, then `/var/run/secrets/ateapi/token` | `""` (auto-detected) | Projected K8s ServiceAccount JWT (`audience: api.ate-system.svc`) sent per RPC (`internal/substrate/client.go:83-89`). |
| `--substrate-ca-file` | `SUBSTRATE_CA_FILE`, then `/run/servicedns-ca/trust-bundle.pem` | `""` (auto-detected) | `ClusterTrustBundle` PEM verifying `api.ate-system.svc` TLS cert (`internal/substrate/client.go:92-100`). |
| `--substrate-insecure-tls` | — | `false` | Skip TLS verification (dev/test only, `main.go:60`). |
| `--substrate-plaintext` | — | `false` | Use insecure gRPC credentials (dev/test only, `main.go:61`). |
| `--template` | — | `"default-template"` | Passed into `EnsureActorTemplateWithImage` but **unused** at `4b7bf6f` (`internal/substrate/client.go:409-423`). |
| `--template-atespace` | — | `"ax-system"` | Passed into `EnsureActorTemplateWithImage` but **unused** at `4b7bf6f` (`pkg/apis/v1alpha1/types.go:39`). |
| `--default-task-image` | `AX_DEFAULT_TASK_IMAGE` | `"gcr.io/ax-substrate/ate-images/ax-task-runner"` | Default guest image when `Task.spec.image` is omitted. **Must be `@sha256:` digest-pinned** on Substrate `v0.6.0` (`main.go:64, 74-76`). |
| *(env only)* | `ATENET_ROUTER_ADDR` | `""` (`deploy/ax-server.yaml:88`: `"https://atenet-router.ate-system.svc.cluster.local:443"`) | Used by `reconciler.go:309-325` to poll `GET /readyz?check=workspace` with header `ate-target-actor: <atespace>/<actor>` over mTLS. |
| *(env only)* | `AX_SNAPSHOTS_BUCKET` | `DefaultSnapshotsBucket` (`client.go:220`) | Populates `SnapshotConfig.storage_location` (`gs://<GCS_BUCKET>/ate-env/`) in `BuildActorTemplate` (`client.go:342`). Ignored by `ate-api-server` unless `--preview=PerTemplateSnapshotLocation` is enabled (`ateapi.proto:1279-1288`). |

### 1.2 Required Namespace Labels, RBAC & Projected Volumes (`deploy/ax-server.yaml`)

1. **Namespace Label (`ate.dev/api-access: "true"`)**:
   - `ate-system` `NetworkPolicy/ate-api-server` drops TCP `:443` traffic unless the source pod is in a namespace labeled `ate.dev/api-access: "true"` and does **not** carry the `ate.dev/worker-pool` label. Both `Namespace/ax-system` and `Namespace/ax-bug-bash` must set `ate.dev/api-access: "true"` (`deploy/ax-server.yaml:15-21`).
2. **Three Projected Credential Mounts on `Deployment/ax-server` (`deploy/ax-server.yaml:116-153`)**:
   - **`ate-token` (`/var/run/secrets/ateapi/token`)**: `serviceAccountToken` with `audience: api.ate-system.svc` and `expirationSeconds: 7200` for authenticating `ServiceAccount/ax-server` (`system:serviceaccount:ax-system:ax-server`) to `ate-api-server`.
   - **`servicedns-ca` (`/run/servicedns-ca/trust-bundle.pem`)**: `clusterTrustBundle` selecting `signerName: servicedns.podcert.ate.dev/identity` with `matchLabels: { podcert.ate.dev/canarying: live }`. Used to verify both `api.ate-system.svc:443` and `atenet-router.ate-system.svc:443`.
   - **`podidentity` (`/run/podidentity.podcert.ate.dev/credential-bundle.pem`)**: `podCertificate` (`signerName: podidentity.podcert.ate.dev/identity`, `keyType: ECDSAP256`) issuing SPIFFE client cert `spiffe://cluster.local/ns/ax-system/sa/ax-server`. Used by `newProbeHTTPClient` (`internal/controller/reconciler.go:94-118`) when calling `atenet-router`. Note: `deploy/atenet-router` in `ate-system` must include `spiffe://cluster.local/ns/ax-system/sa/ax-server` in `--ingress-allowed-spiffe-ids` (verified present on the live cluster).

### 1.3 PostgreSQL Schema & Distributed Locking (`deploy/postgres.yaml`, `internal/store/postgres/`)

- **Deployment**: `StatefulSet/ax-postgres` (`postgres:17-alpine`, `POSTGRES_USER=ax`, `POSTGRES_DB=ax`, `PGDATA=/var/lib/postgresql/data/pgdata`, 10Gi RWO PVC, `Secret/ax-postgres` key `password`).
- **Auto-Migration**: `postgres.Open` (`internal/store/postgres/store.go:65-89`) automatically executes `internal/store/postgres/schema.sql` (`lines 6–40`) on startup:
  ```sql
  CREATE TABLE IF NOT EXISTS tasks (
      atespace TEXT NOT NULL,
      name     TEXT NOT NULL,
      data     JSONB NOT NULL,
      saved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      PRIMARY KEY (atespace, name)
  );
  -- Identical (atespace, name, data JSONB, saved_at) tables for `workspaces` and `models`
  CREATE TABLE IF NOT EXISTS locks (
      key        TEXT PRIMARY KEY,
      token      TEXT NOT NULL,
      expires_at TIMESTAMPTZ NOT NULL
  );
  ```
- **Lease Locking & Status Split**: Per-key leases in `locks` use a 30s TTL renewed every 10s (`internal/store/postgres/lock.go:66-70, 163-195`). `storedStatus` (`internal/server/server.go:585-603`) strips Actor-owned fields (`actor`, `workerIP`, `Ready` condition, and runtime phases `Running`/`Resuming`/`Suspending`/`Suspended`) before persisting to PostgreSQL, overlaying them live from `ateapi.Control/{GetActor,ListActors}` on read (`server.go:463-547`).

### 1.4 `ActorTemplate` Synthesis, `Task.spec` Schema & Runner Execution (`reconciler.go`, `runner/runner.go`)

- **Per-Task `ActorTemplate` Generation**:
  - In `internal/controller/reconciler.go:189-235`, `ReconcileTask` serializes the `Task` into `extraEnv["AX_TASK_YAML"]` (and optional `Workspace` into `extraEnv["AX_WORKSPACE_YAML"]`). Because `len(extraEnv) > 0` is always true, `ax-server` **always** creates a task-specific `ActorTemplate` named `<taskName>-tmpl-<8-hex-sha256>` (`taskTemplateName`, `reconciler.go:428-440`) inside the task's `atespace` (`"default"`).
  - `BuildActorTemplate` (`internal/substrate/client.go:332-405`) constructs:
    - `containers[0]`: `name: "guest"`, `image: <digest-pinned image>`, `command: ["/usr/local/bin/ax-task-runner"]` (hardcoded `DefaultGuestCommand`; `Task.spec.command` is **not** container `ENTRYPOINT`, it is spawned as a child process by `ax-task-runner`), `wakeupProbe: { httpGet: { path: "/readyz", port: 80 } }`, `volumeMounts: [{ name: "workspace", mountPath: "/workspace" }]`.
    - `volumes[0]`: `{ name: "workspace", durableDir: {} }`.
    - `snapshotConfig`: `{ storageLocation: snapshotsBucket, preferredFidelity: SNAPSHOT_FIDELITY_VOLUMES }`.
    - `resources`: `{ limits: { cpuMillicores: <default 1000>, memoryBytes: <default 536870912> } }` (`Task.spec.resources.requests` is rejected by `ValidateResources`, `pkg/apis/v1alpha1/resources.go:40-43`).
    - `workerSelector`: `nil` (schedules onto any free worker in any `WorkerPool`).
- **`ax.v1alpha1` Protobuf Schemas (`pkg/apis/v1alpha1/ax.proto`)**:
  - `TaskSpec` (`lines 69–89`): `image` (`string`), `command` (`repeated string`), `env` (`repeated EnvVar`), `resources` (`ResourcesSpec`), `workspace` (`WorkspaceRef { name, path }`), `debug` (`bool`), `config` (`google.protobuf.Struct`). Size caps (`types.go:417-469`): `MaxEnvValueSize = 32 KiB`, `MaxTaskYAMLSize = 31 KiB`, `MaxTaskConfigSize = 16 KiB`. **`Task.spec.egress` does not exist at `4b7bf6f`.**
  - `WorkspaceSpec`: `git` (`repeated GitRepo { name, repo, branch, commit, dir }`), `gcs` (`repeated GCSMount`), `env` (`repeated EnvVar`).
  - `ModelSpec`: `provider` (`string`), `model` (`string`), `endpoint` (`string`), `api_key_secret` (`string`). *(No `fallbackCascade` field exists in `ModelSpec`.)*
- **Guest Container PID 1 (`runner.Run`, `runner/runner.go:96-167`)**:
  - Starts public probe server on `0.0.0.0:80` (`/healthz` -> `200 ok`; `/readyz` -> `503` until `SetWorkspaceReady(true)`, then `200 ok`) and loopback metadata server on `127.0.0.1:8099` (`AX_METADATA_URL`, serving `GET /metadata/v1alpha1/ax/task` and `/metadata/v1alpha1/ax/workspace`).
  - Runs `workspace.SetupWorkspace`, flips `/readyz` to `200 ok`, and spawns `Task.spec.command` in `/workspace` with `AX_METADATA_URL=http://127.0.0.1:8099`. When `Task.spec.command` exits, `OnCommandExit` fires while PID 1 stays alive until `SuspendTask` / container stop.
- **Three Lifecycle Nuances**:
  1. **`CreateTask` Starts `Suspended`**: `CreateTask` creates the Substrate Actor (`Actor.name == Task.name`) and immediately calls `SuspendActor` (`reconciler.go:251-260`). Callers **must** invoke `ResumeTask` to place the Actor onto a worker. (This gap between `CreateTask` and `ResumeTask` is the ideal window to attach a per-actor `EgressPolicy`!)
  2. **Golden Snapshot Boot Without `EgressPolicy`**: `CreateActorTemplate` boots a temporary UUID golden actor with no `EgressPolicy`. If `Workspace.spec.git` is set, `SetupWorkspace` (`internal/workspace/setup.go:43-44, 101-104`) retries `git fetch` 5×2s (`~10s`), withholds `/ax/initialized-workspace`, and returns `(res, nil)` so `/readyz:80` still passes within the 30s `WakeupProbe` timeout. Once `EgressPolicy` is attached to `<taskName>` prior to `ResumeTask`, `git clone` against in-cluster `storefront.ax-bug-bash.svc.cluster.local:3001` succeeds in `<500ms`.
  3. **`SNAPSHOT_FIDELITY_VOLUMES` Rootfs Reset**: On `SuspendTask` -> `ResumeTask`, only `/workspace` (`durableDir`) is snapshotted and restored; rootfs paths (`/ax`, `/tmp`) reset to the container image. Persisting `.ax-state/<taskName>.json` inside `/workspace/.ax-state` preserves TDD state across suspend/resume.

---

## Part 2: Resource Ownership Split (`kubectl` vs `kubectl-ate` vs `ax` gRPC) & Cluster-Internal Egress

### 2.1 Three-Plane Resource Ownership Matrix

| Resource | API Plane & Storage | CLI / Client | Scope | Key Schema & `v0.6.0` Notes |
| :--- | :--- | :--- | :--- | :--- |
| `Namespace` (`ax-system`, `ax-bug-bash`) | Kubernetes API (`etcd`) | `kubectl apply` | Cluster | Must include label `ate.dev/api-access: "true"` so pods can dial `api.ate-system.svc:443`. |
| `SandboxConfig` (`gvisor-default`) | Kubernetes CRD `ate.dev/v1` | `kubectl apply` | Cluster | Pre-installed on cluster (`runtime.gvisor: {}`). |
| `WorkerPool` (`ax-bug-bash-workers`) | Kubernetes CRD `ate.dev/v1` | `kubectl apply` | Namespaced (`ax-bug-bash`) | **Must be `ate.dev/v1`** (not `v1alpha1`). Uses `spec.replicas`, `spec.workerImage` (`@sha256:` pinned), `spec.sandboxClasses`, `spec.template.resources`. `Worker.labels` mirrors `WorkerPool.metadata.labels`. |
| `Atespace` (`default`) | `substrate.ateapi.v1.Control` (Postgres) | `kubectl-ate create atespace default` | Global | Isolation boundary for `ActorTemplate`, `Actor`, `Tag`, and `EgressPolicy`. |
| `AccessPolicy` (`default`) | `substrate.ateapi.v1.Control` (Postgres) | `kubectl-ate get/create/update access-policy` | `--global` or `-a <atespace>` | Singleton `metadata.name: default`. `ax-server` SA **must** hold `role: owner` on `--global` so `EnsureAtespace` (`CreateAtespace`) succeeds. |
| `ActorTemplate` (`<task>-tmpl-<hash>`) | `substrate.ateapi.v1.Control` (Postgres) | Synthesized by `ax-server` (`kubectl-ate get actor-templates -a default`) | Atespace (`default`) | Immutable; auto-created per task by `ax-server` `EnsureActorTemplateWithImage`. |
| `Actor` (`<task-name>`) | `substrate.ateapi.v1.Control` (Postgres) | Synthesized by `ax-server` (`kubectl-ate get actors -a default`) | Atespace (`default`) | `Actor.metadata.name` equals `Task.metadata.name`. |
| `EgressPolicy` (`default` per Actor) | `substrate.ateapi.v1.Control` (Postgres) | `kubectl-ate create egress-policy <actor> -a default -f ...` or Go `ateapi.ControlClient` | Per-Actor (`(atespace, actor)`) | Required for any outbound DNS or TCP connection from `ateworker-gvisor` guests. |
| `Workspace`, `Model`, `Task` | `ax.v1alpha1.AX` gRPC (`ax-postgres`) | `ax apply` / Go `v1alpha1.AXClient` | Atespace (`default`) | Strict `protojson` schema (`ax.proto`). |

### 2.2 Validated `ate.dev/v1` `WorkerPool` Manifest (`kubectl apply`)

```yaml
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: ax-bug-bash-workers
  namespace: ax-bug-bash
  labels:
    app.kubernetes.io/part-of: ax-bug-bash
    workload: ax-bug-bash
spec:
  replicas: 4
  workerImage: <IMAGE_REGISTRY>/ateworker-gvisor@sha256:<ATEWORKER_GVISOR_SHA256>
  sandboxClasses:
    - name: gvisor
      configRef:
        name: gvisor-default
  template:
    labels:
      app.kubernetes.io/part-of: ax-bug-bash
      workload: ax-bug-bash
    resources:
      requests:
        cpu: "1000m"
        memory: "2Gi"
      limits:
        cpu: "4000m"
        memory: "8Gi"
```

### 2.3 Validated OpenFGA Global `AccessPolicy` (`kubectl-ate`)

Per `docs/guides/authorization.md` (`lines 50–61, 165–195, 250–281`), `AccessPolicy` principals use `user:system:serviceaccount:<ns>:<sa>` and roles are `owner`, `editor` (atespace only), or `viewer`. Because `ax-server` calls `CreateAtespace` (`EnsureAtespace`) and `ax-observer` / `ax-bug-bash` creates per-actor `EgressPolicy` resources (`CreateActorEgressPolicy`) and inspects workers (`ListWorkers`), grant `owner` on `--global` by fetching the current `uid` + `version` and updating:

```bash
kubectl-ate get access-policy --global -o yaml > /tmp/global-policy.yaml
# Merge user:system:serviceaccount:ax-system:ax-server and
# user:system:serviceaccount:ax-bug-bash:ax-observer into role: owner members, preserving metadata.uid & metadata.version:
kubectl-ate update access-policy --global -f /tmp/global-policy.yaml
```

Schema of `AccessPolicy` (`ateapi.proto` / `authorization.md:253-264`):
```yaml
metadata:
  name: default
  uid: "<existing-uid>"
  version: "<existing-version>"
bindings:
  - role: owner
    members:
      - user:system:serviceaccount:ate-system:ate-client
      - user:system:serviceaccount:ax-system:ax-server
      - user:system:serviceaccount:ax-bug-bash:ax-observer
```

### 2.4 How `atenet-egress` & `atenet-router` Mediate Cluster-Internal Traffic from `ateworker-gvisor`

Primary-source trace across `docs/guides/egress-traffic.md`, `docs/guides/network-egress.md`, `cmd/atenet/internal/router/egress/{egress.go,request.go}`, and `internal/egresspolicy/egresspolicy.go`:

1. **Direction Split (`atenet-router` vs `atenet-egress`)**:
   - **`atenet-router` (`ate-system`) is strictly INGRESS *into* Actors**: Cluster services (such as `ax-server`'s `/readyz?check=workspace` probe) send HTTPS requests to `atenet-router.ate-system.svc.cluster.local:443` with mTLS client cert (`--ingress-allowed-spiffe-ids`) and header `ate-target-actor: <atespace>/<actor-name>`. Actors inside `ateworker-gvisor` never dial `atenet-router`.
   - **`atenet-egress` (`ate-system`) is strictly EGRESS *out of* Actors**: Inside the worker pod, `nftables` redirects all actor TCP traffic (except UDP/TCP `:53` to the sandbox DNS relay `169.254.17.1:53`) into `atunnel`, which opens an mTLS `CONNECT <origDstIP>:<origDstPort>` tunnel (authenticated with a per-actor `spiffe://<trustdomain>/worker-for-actor/<atespace>/<actor>` X.509 cert) to `atenet-egress.ate-system.svc:443`.
2. **DNS Relay + Cluster-Internal `.svc.cluster.local` Resolution**:
   - `ateworker` only drops link-local `169.254.0.0/16` (exempting `169.254.17.0/30`); it does **not** block RFC1918 / ClusterIP / PodIP ranges (`network-egress.md:21`).
   - When an Actor queries `ax-server.ax-system.svc.cluster.local`, the sandbox DNS relay (`169.254.17.1:53`) checks whether the hostname matches a pattern in that Actor's `EgressPolicy`. If no policy or rule matches, it answers `NXDOMAIN` (`egress-traffic.md:18-23`). If matched, it forwards the query to the worker pod's CoreDNS/kube-dns resolver and returns the ClusterIP!
   - When the Actor connects to `<ClusterIP>:<port>` and sends an HTTP/1.1 or `h2c` request with `Host` / `:authority: ax-server.ax-system.svc.cluster.local:<port>`, `atenet-egress` (`egress_cleartext` filter chain -> `request.go:63-124` -> `egresspolicy.go:369-420`) validates the lowercase RFC-1123 hostname and **dialed port**, then instructs Envoy's `dynamic_forward_proxy` to resolve and dial `ax-server.ax-system.svc.cluster.local` on the dialed port!
3. **`HttpRule` Supports Both HTTP/1.1 and `h2c` gRPC Without MITM Certs**:
   - `ateapi.proto:658` specifies that `http` rules allow *"Cleartext HTTP to the named hosts: HTTP/1.1, and h2c as used by gRPC"*.
   - **Critical Port Rule (`egresspolicy.go:93-95, 141-145`)**: Omitting `ports` on an `http` rule restricts it to port `80` only! Non-80 cluster ports (`8080`, `3000`, `3001`) **must** list `ports.numbers` explicitly.
4. **Exact Per-Actor `EgressPolicy` for Root & Child AX Tasks**:
   Attach immediately after `axClient.CreateTask(ctx, ...)` (while the Actor `<task-name>` is `ACTOR_STATE_SUSPENDED`) and **before** `axClient.ResumeTask(ctx, ...)`, via `ateapi.ControlClient.CreateActorEgressPolicy` (or `kubectl-ate create egress-policy <task-name> -a default -f -`):

   ```yaml
   rules:
     - http:
         hostnames:
           - ax-server.ax-system.svc.cluster.local
           - ax-server.ax-system.svc
         ports:
           numbers: [80, 8080]
     - http:
         hostnames:
           - model-gateway.ax-system.svc.cluster.local
           - model-gateway.ax-system.svc
         ports:
           numbers: [80, 8080]
     - http:
         hostnames:
           - storefront.ax-bug-bash.svc.cluster.local
           - storefront.ax-bug-bash.svc
           - retail-storefront.ax-bug-bash.svc.cluster.local
         ports:
           numbers: [3001]
     - http:
         hostnames:
           - ax-observer.ax-bug-bash.svc.cluster.local
           - ax-observer.ax-bug-bash.svc
         ports:
           numbers: [3000]
   ```
   *(Note: When Root Triage inside an Actor calls `axClient.CreateTask` to spawn Child Fixer tasks, `ax-observer` or a lightweight sidecar/watcher — or `ServiceAccount` token if delegated, though zero-secret sandboxes should NOT hold Substrate `ate-api` tokens — must attach the `EgressPolicy` to each spawned Child Actor before/as it resumes; policies attached to a running Actor take effect in `atunnel` DNS within ~5s and `atenet-egress` within `--egress-policy-cache-ttl` = 10s, or 0s if attached before `ResumeActor`.)*

---

## Part 3: Non-Interactive CLI Coding Harnesses Inside `harnesses/Dockerfile.task-runner`

All three coding harnesses run inside `/workspace` with zero cloud credentials (`GEMINI_API_KEY="ax-sandbox-token"`, `OPENAI_API_KEY="ax-sandbox-token"`) and route through the in-cluster **Gemini Enterprise Agent Platform Model Gateway** (`http://model-gateway.ax-system.svc.cluster.local:8080/s/<task-name>`), which exposes:
- **Gemini REST**: `POST /s/{task_name}/v1beta/models/{model}:generateContent`
- **OpenAI Chat Completions**: `POST /s/{task_name}/v1/chat/completions` (full bidirectional tool-call translation in `internal/gateway/openai_compat.go:128-386`) and `GET /s/{task_name}/v1/models`

### 3.1 Harness Comparison & Exact Non-Interactive Contracts

| Harness | Package & Version | Installed Binary | Headless / Non-Interactive CLI Invocation | Gateway Wire Protocol & Config Schema |
| :--- | :--- | :--- | :--- | :--- |
| **Antigravity** | PyPI `google-antigravity` (`0.1.23`) + wrapper `/usr/local/bin/antigravity` | `/usr/local/bin/antigravity` (Python CLI; `google-antigravity` wheel has `bin=None` and embeds Go `localharness` via SDK) | `/usr/local/bin/antigravity -p "<prompt>"` | **Wrapper**: `POST ${GATEWAY_BASE_URL}/s/${AX_TASK_NAME}/v1beta/models/${AGENT_MODEL}:generateContent` with `x-goog-api-key: ax-sandbox-token`, `X-AX-Harness: antigravity`, `X-AX-Task-Name: ${AX_TASK_NAME}` (`read_file`, `write_file`, `list_files`, `run_command` tools).<br>**Native Python SDK (`google.antigravity`)**: `LocalAgentConfig(models=[ModelTarget(name=model, endpoint=GeminiAPIEndpoint(base_url=f"{gw}/s/{task}", api_key="ax-sandbox-token", http_headers={"X-AX-Harness": "antigravity"}))], workspaces=["/workspace"], policies=[policy.allow_all()])` (note: `policy.allow_all()` is mandatory because default `LocalAgentConfig` denies `run_command` via `policy.confirm_run_command()`). |
| **OpenCode** | npm `opencode-ai` (`1.19.5`, pulls `opencode-linux-x64`) + `/usr/local/bin/opencode-harness` | `/usr/bin/opencode` (or `/usr/local/bin/opencode`) & `/usr/local/bin/opencode-harness` | `opencode run --model agent-platform-gateway/${AGENT_MODEL} "<prompt>"` | **Config (`/root/.config/opencode/opencode.json` or `OPENCODE_CONFIG_CONTENT`)**: Schema `https://opencode.ai/config.json`. Uses `@ai-sdk/openai-compatible` against `http://model-gateway.ax-system.svc.cluster.local:8080/s/${AX_TASK_NAME}/v1`. **Must set `"permission": "allow"`** (or `OPENCODE_PERMISSION='{"*":"allow"}'`) so `edit` and `bash` never block on interactive confirmation prompts. |
| **Pi** | npm `@mariozechner/pi-coding-agent` (`0.73.1`) + `/usr/local/bin/pi-harness` | `/usr/bin/pi` (or `/usr/local/bin/pi`, `dist/cli.js`) & `/usr/local/bin/pi-harness` | `pi --print --no-session --provider agent-platform-gateway --model ${AGENT_MODEL} --api-key ax-sandbox-token "<prompt>"` | **Config (`~/.pi/agent/models.json` or `${PI_CODING_AGENT_DIR}/models.json`)**: Validated by TypeBox `ModelsConfigSchema` (`dist/core/model-registry.js`). **Must use `"api": "openai-completions"`** (not `"type": "openai-compatible"`!) and `"baseUrl": "http://model-gateway.ax-system.svc.cluster.local:8080/s/${AX_TASK_NAME}/v1"`. Built-in tools (`read,bash,edit,write`) run non-interactively in `--print` (`-p`) mode. |

### 3.2 Verified Config File Schemas for Per-Task Dynamic Routing

Because `AX_TASK_NAME` varies per spawned Child Task (`fixer-inc-01-pricing-service-1`, etc.), static `/root/.config/opencode/opencode.json` and `/root/.pi/agent/models.json` pointing to `/s/default-task/v1` would attribute all calls to `default-task` instead of the real Child Task session on the Model Gateway. Each harness wrapper (or `ChildFixerTDD`) should materialize a task-scoped config before invoking the native CLI:

#### OpenCode (`OPENCODE_CONFIG_CONTENT` or `/root/.config/opencode/opencode.json`)
```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "agent-platform-gateway/gemini-3.8-flash",
  "small_model": "agent-platform-gateway/gemini-3.5-flash",
  "permission": "allow",
  "provider": {
    "agent-platform-gateway": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Gemini Enterprise Agent Platform Model Gateway",
      "options": {
        "baseURL": "http://model-gateway.ax-system.svc.cluster.local:8080/s/<TASK_NAME>/v1",
        "apiKey": "ax-sandbox-token",
        "headers": {
          "X-AX-Harness": "opencode",
          "X-AX-Task-Name": "<TASK_NAME>"
        }
      },
      "models": {
        "gemini-3.8-flash": { "name": "Gemini 3.8 Flash", "tool_call": true },
        "gemini-3.7-flash": { "name": "Gemini 3.7 Flash", "tool_call": true },
        "gemini-3.6-flash": { "name": "Gemini 3.6 Flash", "tool_call": true },
        "gemini-3.5-flash": { "name": "Gemini 3.5 Flash", "tool_call": true }
      }
    }
  }
}
```

#### Pi (`${PI_CODING_AGENT_DIR}/models.json`, validated against `ModelsConfigSchema` in `dist/core/model-registry.js`)
```json
{
  "providers": {
    "agent-platform-gateway": {
      "name": "Gemini Enterprise Agent Platform Model Gateway",
      "api": "openai-completions",
      "baseUrl": "http://model-gateway.ax-system.svc.cluster.local:8080/s/<TASK_NAME>/v1",
      "apiKey": "ax-sandbox-token",
      "headers": {
        "X-AX-Harness": "pi",
        "X-AX-Task-Name": "<TASK_NAME>"
      },
      "models": [
        { "id": "gemini-3.8-flash", "name": "Gemini 3.8 Flash", "contextWindow": 1048576, "maxTokens": 8192 },
        { "id": "gemini-3.7-flash", "name": "Gemini 3.7 Flash", "contextWindow": 1048576, "maxTokens": 8192 },
        { "id": "gemini-3.6-flash", "name": "Gemini 3.6 Flash", "contextWindow": 1048576, "maxTokens": 8192 },
        { "id": "gemini-3.5-flash", "name": "Gemini 3.5 Flash", "contextWindow": 1048576, "maxTokens": 8192 }
      ]
    }
  }
}
```

### 3.3 `harnesses/Dockerfile.task-runner` Prewarm & Runner Integration Checklist

1. **Fix Prewarm Syntax Check (`harnesses/Dockerfile.task-runner:64`)**:
   - Replace `node --check /tmp/prewarm/services/storefront-web/test_currency.mjs` with `node --check /tmp/prewarm/services/storefront-web/test/storefront.test.mjs` so `docker build -f harnesses/Dockerfile.task-runner .` succeeds cleanly.
2. **Binary Entrypoint Contract**:
   - Cross-compile `cmd/ax-bug-bash-runner` (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/linux_amd64/ax-task-runner ./cmd/ax-bug-bash-runner`) so `/usr/local/bin/ax-task-runner` matches `DefaultGuestCommand` in `google/ax`'s `BuildActorTemplate` (`internal/substrate/client.go:53`).
3. **Subprocess CLI Execution vs In-Process Gateway Fallback**:
   - Inside `ateworker-gvisor`, `ChildFixerTDD` (`internal/bugbash/child_fixer.go`) or the harness scripts (`/usr/local/bin/antigravity`, `/usr/local/bin/opencode-harness`, `/usr/local/bin/pi-harness`) can set `AX_TASK_NAME`, `GATEWAY_BASE_URL`, `AGENT_MODEL`, and invoke the real headless CLI (`google.antigravity` SDK / `opencode run` / `pi --print --no-session`) while retaining the multi-turn tool/inline `<file>` block loop and deterministic fixture fallback if upstream model quota is unavailable.
