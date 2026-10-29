# ADR-0013: Upstream google/ax Compatibility Overlay and Three-Patch PR Blueprints (Issues #482, #511, #518)

## Status

Accepted

## Context

Running a two-level recursive task tree (`Root Task` + 3 parallel `Child Tasks`) on `google/ax` (`4b7bf6f`) atop a live GKE cluster running **Agent Substrate `v0.6.1`** (`substrate.ateapi.v1.Control`) and `agent-substrate/env` uncovered three distinct cross-layer mismatches corresponding to upstream `google/ax` issues `#482`, `#511`, and `#518`:

1. **Sandbox Wakeup Probe, Snapshot Mode & Per-Actor Egress (`google/ax` Issue `#482`)**:
   - Upstream `google/ax` configures `WakeupProbe.HttpGet.Path = "/readyz"` on port `80` when creating a sandbox actor (`internal/substrate/client.go`), whereas `agent-substrate/env` serves `/healthz` on port `80` (returning `HTTP 404` on `/readyz`), causing `ResumeActor` to block until timeout.
   - Upstream leaves `GoldenSnapshotConfig` unset on `SnapshotConfig`, causing `ate-controller` to attempt golden-snapshot lookup when `SNAPSHOT_FIDELITY_VOLUMES` is requested without a pre-seeded golden image.
   - When `NetworkPolicy` default-deny egress is enforced on `ateworker-gvisor`, newly created sandbox actors lack an `EgressPolicy` allowing cluster-internal traffic to `ax-server`, `model-gateway`, `storefront`, `storefront-git`, `ax-observer`, and `loki` before `ResumeActor` starts the guest container.
2. **Agent Substrate `v0.6.1` Proto Import Path, OpenFGA Bootstrap & Self-Suspend Cancellation Race (`google/ax` Issue `#511`)**:
   - Agent Substrate `v0.6.1` moved its Go protobuf package from `go.ate.dev/substrate/pkg/proto/ateapipb` to `go.ate.dev/substrate/api/ateapi/v1` (`substrate.ateapi.v1.Control`).
   - Fresh `ax-server` deployments require an OpenFGA store (`default`), authorization model, and seeded `AccessPolicy` before `CreateWorkspace`, `CreateModel`, or `CreateTask` calls succeed.
   - When an in-sandbox **Custom Runner** calls `SuspendTask` on itself inside `OnCommandExit`, `ax-server`'s `Server.SuspendTask` handler forwards the caller's request `ctx` to `SuspendActor`. As soon as `runsc` begins checkpointing and tearing down the caller's container, the caller's HTTP/2 stream closes, canceling `ctx` mid-checkpoint and leaving the actor stuck in `Running` instead of `Suspended`.
3. **Multi-Task Workspace Maiden-Run Marker & Per-Task State Isolation (`google/ax` Issue `#518`)**:
   - Upstream `runner.Run` writes its workspace maiden-run marker to `workspace.DefaultAXDir` (`/root/.ax`), which lives on the container rootfs rather than the persistent `/workspace` volume snapshot (`SNAPSHOT_FIDELITY_VOLUMES`). When a warm worker slot restores `/workspace` for a subsequent task, `/root/.ax` is missing and `SetupWorkspace` fails or overwrites existing files.
   - Multiple sibling `Child Tasks` sharing a workspace volume require isolated per-task state files (`/workspace/.ax-state/<task-name>.json`) so parallel Red-Green TDD runs do not clobber each other's checkpoint metadata.

## Decision

1. **Maintain a Reproducible Upstream Compatibility Overlay (`deploy/ax-server/ax-server-v061.patch` + `deploy/ax-server/build.sh`)**:
   - Keep `deploy/ax-server/ax-server-v061.patch` as a clean `git diff` / `patch -p1` overlay applied by `deploy/ax-server/build.sh` against upstream `google/ax` (`4b7bf6f`) with `go.ate.dev/substrate` replaced by Agent Substrate `v0.6.1`.
   - Include:
     - `WakeupProbe.HttpGet.Path = "/healthz"` (`Port: 80`) and `GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED` in `internal/substrate/client.go`.
     - `Client.ensureActorEgressPolicy` (`AX_AUTO_ACTOR_EGRESS=true`) invoking `CreateActorEgressPolicy` for cluster-internal `ax-server`, `model-gateway`, `storefront`, `storefront-git`, `ax-observer`, and `loki` DNS/ports prior to `ResumeActor`.
     - Proto import path migration to `ateapipb "go.ate.dev/substrate/api/ateapi/v1"`.
     - Detached `ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)` in `Server.SuspendTask` (`internal/server/server.go`).
     - Persistent `workspace.AXDir = filepath.Join(wsPath, ".ax")` (`/workspace/.ax`) in `runner/runner.go`, paired with per-task state files at `/workspace/.ax-state/<task-name>.json` in `internal/bugbash/runner.go`.

2. **Publish a Standalone Upstream Contributor Guide (`docs/upstream/google-ax-v060-patches.md`)**:
   - Organize `docs/upstream/google-ax-v060-patches.md` into three self-contained upstream PR blueprints mapped to `google/ax` Issues `#482`, `#511`, and `#518`.
   - For each issue blueprint, document:
     - Exact failure symptom and gRPC / `runsc` / `ate-controller` root cause.
     - Minimal unified Go / YAML diff ready to cherry-pick into an upstream `google/ax` PR.
     - Live GKE verification command and measured latency (`42.6ms` `SNAPSHOT_FIDELITY_VOLUMES` checkpoint, `61ms` `SuspendTask` worker-slot reclamation, `4/4` warm `ateworker-gvisor` slots).
   - Cross-link `docs/upstream/google-ax-v060-patches.md` from `README.md` under the Agent Substrate `v0.6.0` & `google/ax` Hardening section.

3. **Automated Hygiene, Manifest & `/about` Architecture Catalog Enforcement**:
   - Register `ADR-0013` (`0013-upstream-google-ax-v060-compatibility-overlay.md`) in `internal/observer/about_catalog.go` (`ExpectedADRFilenames()` and `BuildArchitectureTopologyCatalog`) so `/api/about/catalog` and `TestVerifyArchitectureCatalogDrift` validate all 13 ADRs.
   - Enforce `VerifyRepositoryHygieneAndManifests` and `VerifyMarkdownHumanizerAndHygiene` (`internal/evals/evals.go`) on `docs/upstream/google-ax-v060-patches.md` so the guide remains stage-safe (zero personal cluster/project/node/bucket identifiers, current **Gemini Enterprise Agent Platform** and **compute obtainability** terminology, and zero AI-slop prose patterns).

## Consequences

- **Positive**: Upstream `google/ax` maintainers get three focused, ready-to-merge PR blueprints with live GKE verification data, while developers running this repository get a deterministic build script (`deploy/ax-server/build.sh`) and full `/about` catalog visibility.
- **Negative**: Once upstream `google/ax` merges fixes for Issues `#482`, `#511`, and `#518`, `deploy/ax-server/ax-server-v061.patch` can be retired in favor of a pinned upstream release tag.
