# ADR-0014: BOM-19 Three-Layer Architecture Guide (substrate, env, ax), Layer Selection Matrix, and Stage Warmup Gate

## Status

Accepted

## Context

Developers evaluating or adopting **Agent Substrate `v0.6.0`**, `agent-substrate/env`, and `google/ax` on GKE face two recurring architectural and operational questions:

1. **Which layer of the Three-Layer Substrate Stack should a given workload target directly?**
   - Our `BOM-20` blog post (`docs/blog/self-spawning-bug-bash-tree-on-google-ax.md`) and 5-lab developer tutorial (`docs/tutorial/build-a-self-spawning-bug-bash-tree.md`) walk through a single end-to-end workload (**The Self-Spawning Bug Bash Tree**), while `docs/upstream/google-ax-v060-patches.md` documents the three surgical upstream `google/ax` compatibility patches (`Issues #482`, `#511`, and `#518`).
   - However, platform teams building other agent architectures (such as stateless code interpreters, interactive per-user browser IDEs like `learn-substrate`, or external Temporal/Cloud Workflows step runners) need a standalone **BOM-19** reference guide (`docs/architecture/three-layer-substrate-env-ax.md`) that specifies the exact CRD and gRPC boundary between Layer 1 (`agent-substrate/substrate`), Layer 2 (`agent-substrate/env`), and Layer 3 (`google/ax`), paired with a concrete **Layer Selection Matrix** and a documentation partitioning map across `ate.dev`, `google/ax/docs/`, and Google Cloud Docs.
2. **How do presenters guarantee a clean, deterministic live GKE environment before walking on stage?**
   - A live stage demonstration across the three decoupled surfaces (`:3001` **Retail Storefront**, `:3002` **Monitoring Stack**, and `:3000` **AX Observer**) requires all 5 live GKE deployments to be healthy, the storefront Git repository to be reset to `baseline-v1`, firing Prometheus/Alertmanager alerts to be drained, all 4 warm `ateworker-gvisor` worker slots to be ready, a fresh 6-node `Depth <= 2` tree with `4/4` Polyglot Hypothesis Coverage to be verified, and all 14 ADRs (`ADR-0001`..`ADR-0014`) to be served by `GET /api/about/catalog`.

## Decision

1. **Author the Standalone BOM-19 Three-Layer Architecture Guide (`docs/architecture/three-layer-substrate-env-ax.md`)**:
   - Structure `docs/architecture/three-layer-substrate-env-ax.md` into five focused sections:
     1. *Why the Agent Execution Stack Splits into Three Layers*: Architectural overview and Mermaid `architecture-beta` + `sequenceDiagram` diagrams showing how kernel/sandbox isolation, guest lifecycle plumbing, and declarative task orchestration decouple across `agent-substrate/substrate`, `agent-substrate/env`, and `google/ax`.
     2. *Layer 1 (`agent-substrate/substrate`) — Sandbox Actors, `WorkerPool`s, Volume Snapshots, OpenFGA & Zero-Trust Egress*: Concrete CRD and gRPC contracts for `ate.dev/v1` `WorkerPool` (`RuntimeClassName: gvisor`, `ateworker-gvisor` warm pools), `substrate.ateapi.v1.Control` (`CreateAtespace`, `CreateActorTemplate`, `CreateActor`, `ResumeActor`, `SuspendActor`), `SNAPSHOT_FIDELITY_VOLUMES` (`42.6ms` live GKE checkpoint), OpenFGA `AccessPolicy` (`role: owner` vs. `role: viewer`), and per-actor `EgressPolicy` (`atenet-egress`).
     3. *Layer 2 (`agent-substrate/env`) — In-Guest Lifecycle Daemon, Wakeup Probes, mTLS Tunnels & Persistent Mounts*: How the guest daemon inside `ateworker-gvisor` serves `GET /healthz` (`:80`) for `WakeupProbe`, terminates `atenet-router:443` mTLS tunnels, persists `/workspace` across `SuspendActor`/`ResumeActor`, and defines the Layer 1 to Layer 2 contract highlighted by `google/ax` Issues `#482` and `#518`.
     4. *Layer 3 (`google/ax`) — Declarative `Workspace`, `Model`, `Task` & `runner.Run` Orchestration*: How `ax-server` (`ax.v1alpha1.AX` gRPC + PostgreSQL store) synthesizes `<task>-tmpl-<hash>` `ActorTemplate`s, injects `Task.spec.config` (`google.protobuf.Struct`), and pairs with `github.com/google/ax/runner.Run` (`OnCommandExit` + `SuspendTask` in `61ms` with detached `context.WithoutCancel(ctx)` for Issue `#511`) to drive recursive **Root Task** to **Child Task** fan-out.
     5. *The Layer Selection Matrix & Cross-Doc Mapping (`ate.dev`, `google/ax/docs/`, and Google Cloud Docs)*: The 7-workload decision rubric and documentation boundary map across vendor-neutral CNCF `ate.dev` docs, upstream `google/ax/docs/`, and Google Cloud GKE / **Gemini Enterprise Agent Platform** docs.
   - Cross-link `docs/architecture/three-layer-substrate-env-ax.md` from `README.md` and enforce both `VerifyRepositoryHygieneAndManifests` and `VerifyMarkdownHumanizerAndHygiene` (`internal/evals/evals.go`) on the file.

2. **Standardize the 7-Workload Layer Selection Matrix**:
   - Classify agent workloads across three adoption tiers (`Layer 1 Only`, `Layer 1 + Layer 2`, and `Full Three-Layer Stack` with or without a Go **Custom Runner**):
     1. *High-Frequency Stateless Code Interpreter / Eval Grader* -> **Layer 1 Only (`substrate`)**
     2. *Custom Non-HTTP Guest Daemon / Binary Protocol Sandbox* -> **Layer 1 Only (`substrate`)**
     3. *Interactive Browser IDE / Per-User PTY Sandbox* (`learn-substrate` / `agentscope`) -> **Layer 1 + Layer 2 (`substrate` + `env`)**
     4. *External Workflow Engine Step Runner* (Temporal / Cloud Workflows) -> **Layer 1 + Layer 2 (`substrate` + `env`)**
     5. *Single-Repo Autonomous Coding Task* (PR fixer / issue triager) -> **Full Three-Layer Stack (`substrate` + `env` + `ax`)**
     6. *Recursive Multi-Service Incident Response & Red-Green TDD Fan-Out* (`ax-bug-bash` **Bug Bash Tree**) -> **Full Three-Layer Stack + Custom Runner (`substrate` + `env` + `ax` + `runner.Run`)**
     7. *Multi-Harness Comparative Evaluation Benchmark* (`antigravity`, `opencode`, `pi` in parallel) -> **Full Three-Layer Stack + Custom Runner (`substrate` + `env` + `ax` + `runner.Run`)**

3. **Publish Standalone BOM-19 Google Doc, Sync Master BOM Strategy Doc & Register ADR-0014 in `/about`**:
   - Convert `docs/architecture/three-layer-substrate-env-ax.md` into a standalone Google Doc titled **"BOM-19: The Three-Layer Agent Execution Architecture (substrate, env, and google/ax on GKE)"** via `/google/bin/releases/gemini-agents-gdocs/gdocs create --from-file`, bookmark it as a `UserFacing: true` URL artifact (`BOM-19 Three-Layer Architecture Guide Google Doc.url.json`), and link its URL in the `BOM-19` cell on tab `t.0` of the master **Agent Substrate BOM & 2026–2027 Content Strategy** Google Doc (`1VO7qniicFxVzbsG-9V6GAxaejUjw03bQU3wHveSKjqA`) using `gdocs mutate replace --tab t.0 --direct` while preserving all 4 intact comment anchors verified via `gdocs readonly check-anchors`.
   - Register `ADR-0014` (`0014-bom19-three-layer-architecture-guide-and-stage-warmup-gate.md`) in `internal/observer/about_catalog.go` (`ExpectedADRFilenames()` and `BuildArchitectureTopologyCatalog`) so `GET /api/about/catalog` and `TestVerifyArchitectureCatalogDrift` verify all 14 ADRs (`ADR-0001`..`ADR-0014`).

## Consequences

- **Positive**: Developers and documentation authors get a clear, benchmark-backed architectural specification for `substrate`, `env`, and `ax` along with a 7-workload **Layer Selection Matrix**, while the live `/about` catalog and master BOM strategy doc remain 100% synchronized.
- **Negative**: Any future addition to the `substrate` / `env` / `ax` API boundary or ADR inventory must update `docs/architecture/three-layer-substrate-env-ax.md` and `internal/observer/about_catalog.go` to keep `TestRepositoryHygieneAndManifestContract` green.
