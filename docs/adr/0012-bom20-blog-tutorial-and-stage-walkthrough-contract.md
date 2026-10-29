# ADR-0012: Flagship BOM-20 Blog Narrative, Five-Lab Developer Tutorial, and Stage Walkthrough Contract

## Status

Accepted

## Context

With Phase 1 (`SIMULATED` showcase) and Phase 2 (live GKE `google/ax` + Agent Substrate `v0.6.0` execution, real 5-service polyglot HTTP mesh with `< 3s` selective hot-reload, in-cluster Prometheus/Alertmanager/Loki/Grafana + Google Cloud Dual-Export, and the 3-tab interactive `/about` `Architecture Topology Catalog`) verified on GKE, Phase 3 delivers the public-facing **BOM-20** technical blog post, a self-contained hands-on developer tutorial, and the recorded **Stage Walkthrough** (`demo.mp4`).

Technical launch posts and tutorials written with default AI phrasing suffer from recognizable tells (em dashes everywhere, `"not X, but Y"` contrasts, staged openers, and generic filler) and risk leaking developer-specific cluster, project, or node identifiers. At the same time, readers evaluating **Agent Substrate `v0.6.0`** and `google/ax` need a clear mental model of how the **Three-Layer Substrate Stack** composes—from low-level sandbox actors and volume snapshots up to declarative `Task`, `Workspace`, `Model`, and `Custom Runner` orchestration—backed by real code excerpts and measured GKE latency numbers.

## Decision

1. **Seven-Section `BOM-20` Blog Post Narrative (`docs/blog/self-spawning-bug-bash-tree-on-google-ax.md` + Google Doc Sync)**:
   - **Section 1 — Opening Incident Hook (*"Why CI Passed and Production Still Returned HTTP 500"*)**: Open directly on a multi-service checkout failure (`INC-101` Stacked Promo Negative Subtotal and `INC-102` Flash-Sale Bundle `u32` Underflow) across the 5-service polyglot **Retail Storefront** (`Node.js/TypeScript`, `Go`, `Python/FastAPI`, `Rust/Axum`) where every single-service unit test passes, yet a cross-service cart payload triggers a `StorefrontHTTP500Spike` alert within `15s`.
   - **Section 2 — From Single-Container Chat Loops to a Self-Spawning Bug Bash Tree**: Contrast manual stack-trace copy-pasting with an event-driven **Root Task** on `google/ax` that correlates Alertmanager labels and Loki LogQL stack traces to fan out isolated **Child Tasks** only for the 2–3 implicated microservices.
   - **Section 3 — The Three-Layer Substrate Stack on GKE (`BOM-19` / `BOM-20` Bridge)**:
     - **Layer 1 (`Agent Substrate v0.6.0`)**: Pre-warmed `ateworker-gvisor` sandbox pool (`WorkerPool/ax-bug-bash-workers`), per-actor zero-trust `EgressPolicy` (`substrate.ateapi.v1.Control`), and sub-50ms `SNAPSHOT_FIDELITY_VOLUMES` Cloud Storage checkpoints.
     - **Layer 2 (`agent-substrate/env`)**: Guest readiness probes (`/healthz`), workspace initialization hooks, and persistent volume state (`/workspace/.ax` and `/workspace/.ax-state`).
     - **Layer 3 (`google/ax`)**: Declarative `Task`, `Workspace`, and `Model` resources, free-form `Task.spec.config` (`google.protobuf.Struct`) mission payloads, and the in-sandbox Go **Custom Runner** built on `github.com/google/ax/runner.Run` (`OnCommandExit` + `SuspendTask`).
   - **Section 4 — Multi-Harness Red-Green TDD & Zero-Secret Model Gateway**: Detail how sibling **Child Tasks** execute **Antigravity**, **OpenCode**, and **Pi** in parallel (`multi` mode) against the zero-secret **Model Gateway** on **Gemini Enterprise Agent Platform**, using the 4-tier Gemini Flash fallback cascade (`gemini-3.8-flash` → `gemini-3.7-flash` → `gemini-3.6-flash` → `gemini-3.5-flash`) to design for **compute obtainability**.
   - **Section 5 — Root Verification Gate & `< 3s` Selective Git Hot-Reload**: Explain how the **Root Task** verifies `refs/heads/fix/<task>` branches, merges `--no-ff` into `main`, and triggers the PID-1 **Hot-Reload Supervisor** to recompile and swap only modified services while unaffected services drop zero requests.
   - **Section 6 — Live GKE Benchmarks & Two-Tier Evaluation**: Present measured GKE numbers (`80ms` Smart-HTTP Git clone, `4ms` pre-warmed `go test`, `42.6ms` `SNAPSHOT_FIDELITY_VOLUMES` checkpoint, `61ms` `SuspendTask` worker-slot reclamation, `820ms–4.2s` selective polyglot hot-reload, and Google Cloud Logging + Managed Prometheus dual-export).
   - **Section 7 — Try It Yourself**: Point readers to `docs/tutorial/build-a-self-spawning-bug-bash-tree.md` and `make run-local` / `make deploy`.
   - **Google Doc Sync**: Import the completed blog markdown into a formatted Google Doc via the `gdocs` CLI and publish a `UserFacing: true` URL artifact bookmark for collaborative review.

2. **Five-Lab Hands-On Developer Tutorial (`docs/tutorial/build-a-self-spawning-bug-bash-tree.md`)**:
   - **Lab 1**: Run the 3 decoupled surfaces locally in hermetic `SIMULATED` mode (`make run-local`), trigger `INC-101`, inspect `:3000` **AX Observer** and `/about`, and run the 20-trajectory evaluation suite (`make eval`).
   - **Lab 2**: Build a **Custom Runner** in Go using `github.com/google/ax/runner.Run`, decoding `Task.spec.config`, capturing `UnifiedGitDiff` in `OnCommandExit`, and invoking `SuspendTask(SNAPSHOT_FIDELITY_VOLUMES)`.
   - **Lab 3**: Configure the zero-secret **Model Gateway** (`internal/gateway/cascade.go`) and multi-harness container image (`harnesses/Dockerfile.task-runner`).
   - **Lab 4**: Deploy to a live GKE cluster with Agent Substrate `v0.6.0` (`make deploy` / `./hack/deploy-gke.sh`).
   - **Lab 5**: Execute the live 4×4 chaos soak (`make eval-live-gke`), inspect `/tmp/ax-live-gke-eval-report.json`, and verify Google Cloud Logging and Managed Service for Prometheus dual-export.

3. **Automated `humanizer` / `unslop` Voice & Stage-Safe Hygiene Contract**:
   - Prose in `docs/blog/self-spawning-bug-bash-tree-on-google-ax.md` and `docs/tutorial/build-a-self-spawning-bug-bash-tree.md` must contain zero em dashes (`—`) or en dashes (`–`), zero `"not X, but Y"` / `"not just X, it's Y"` rhetorical contrasts, zero one-line dramatic closers, zero staged run-up openers (`"Let's dive in"`, `"Here's the thing"`), and zero AI filler words (`delve`, `tapestry`, `testament`, `pivotal`, `seamlessly`, `unlock`, `paradigm`).
   - Extend `VerifyRepositoryHygieneAndManifests` in `internal/evals/evals.go` to scan both markdown files for retired branding (`Vertex AI`, `fluid compute`), personal cluster/project/node/bucket identifiers, and prohibited prose tells.

## Consequences

- **Positive**: Aligns the **BOM-20** blog post and tutorial directly with the verified Phase 2 codebase and live GKE benchmark data while enforcing human-like prose and strict stage-safe hygiene in automated tests.
- **Negative**: Contributors editing `docs/blog/` or `docs/tutorial/` must adhere to the no-em-dash and stage-safe hygiene rules enforced by `make verify-manifests` and `make eval`.
