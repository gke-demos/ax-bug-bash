# AGENTS.md

Context for AI coding agents working in the `gke-demos/ax-bug-bash` repository (**The Self-Spawning Bug Bash Tree**: Recursive Sub-Task Fan-Out on `google/ax` and Agent Substrate `v0.6.0` on GKE).

## Project overview

`ax-bug-bash` demonstrates what makes `google/ax` (`docs/concepts.md`) powerful on top of **Agent Substrate `v0.6.0`**: *"A single task may be the whole job, or it may be the root of a large tree of tasks spawned as the agent breaks the problem down."*

See [`README.md`](README.md) for the quickstart and [`GLOSSARY.md`](GLOSSARY.md) for the ubiquitous domain vocabulary.

### Three decoupled production surfaces (`ADR-0001` to `ADR-0010`)

The architecture runs as three independent, browser-accessible production surfaces ([`ADR-0001`](docs/adr/0001-three-decoupled-surfaces-and-subtle-production-incidents.md)):

1. **Standalone 5-Service Polyglot Retail Storefront (`:3001`, [`ADR-0003`](docs/adr/0003-polyglot-five-service-storefront-and-incident-catalog.md), [`ADR-0009`](docs/adr/0009-polyglot-http-mesh-and-zero-downtime-git-hot-reload.md))**:
   - `storefront-web` (**Node.js / TypeScript**): Customer-facing retail storefront UI and HTTP edge gateway (`services/storefront-web/src/server.ts`).
   - `checkout-service` (**Go**): Order orchestration, cart validation, and payment capture (`services/checkout-service/checkout.go`).
   - `shipping-service` (**Go**): Carrier rate calculation, weight tiers, and fulfillment dispatch (`services/shipping-service/shipping.go`).
   - `pricing-service` (**Python / FastAPI**): Promo code stacking, tax calculation, and currency conversion (`services/pricing-service/pricing.py`).
   - `inventory-service` (**Rust / Axum**): High-concurrency SKU stock reservation and warehouse ledger (`services/inventory-service/src/lib.rs`).
   - Backed by an in-cluster Git Smart-HTTP server (`storefront-git` at `/git/storefront.git`, `baseline-v1` tag) and a PID-1 Go **Hot-Reload Supervisor** (`cmd/polyglot-supervisor`, `internal/supervisor/`) that recompiles and swaps only modified services in under 3 seconds (`< 10s` SLA) with zero dropped requests on unaffected services.
   - Pre-existing unit and integration tests pass on the happy path (`./services/run-service-tests.sh --all`), while 4 subtle cross-service **Incident Scenarios** (`INC-101` through `INC-104`) can be activated on demand or randomized.

2. **Off-the-Shelf Monitoring and Alerting Stack (`:3002`, [`ADR-0002`](docs/adr/0002-in-cluster-prometheus-alertmanager-loki-grafana-with-gcp-export.md))**:
   - Upstream **Prometheus + Alertmanager + Loki + Grafana** in-cluster with a 5-second scrape and evaluation window (`for: 0s`) that fires Alertmanager v4 webhooks (`POST /api/webhooks/alertmanager`) within 3 to 5 seconds of any `HTTP 500` spike.
   - Dual-exports structured JSON error logs (`X-Storefront-Request-ID`) to Google Cloud Logging and `PodMonitoring` metrics (`manifests/monitoring/03-gmp-podmonitoring.yaml`) to Google Cloud Managed Service for Prometheus.

3. **Dedicated `AX Observer` and Recursive Bug Bash Tree (`:3000`, [`ADR-0004`](docs/adr/0004-telemetry-driven-triage-and-tdd-child-task-lifecycle.md) through [`ADR-0010`](docs/adr/0010-interactive-about-architecture-catalog-and-drift-verification.md))**:
   - Clean web interface (`internal/observer/`) with path-based routing (`/`, `/workers`, `/about`), a top-right icon-only Moon/Sun **Projector Light Mode** toggle with zero layout shift, a discrete `⌘K` Incident Scenario and Harness launcher, and a drift-tested 3-tab interactive SVG **Architecture Topology Catalog** on `/about` (`GET /api/about/catalog`).
   - Receives Alertmanager webhooks, correlates Loki LogQL stack traces, and dispatches the **Root Task** (`triage-<id>`) on `google/ax` (`LIVE_GKE` when `AX_SERVER_ADDR` is configured, or hermetic `SIMULATED` mode for local runs).
   - The **Root Task** analyzes production stack traces from `Task.spec.config`, isolates only the 2 to 3 implicated microservices, and calls `ax-server` gRPC (`ax.v1alpha1.AX/CreateTask` + `ResumeTask`) to spawn one **Child Task** (`fixer-<id>-<service>`) per affected service.
   - Each **Child Task** runs inside an **Agent Substrate `v0.6.0`** `ateworker-gvisor` sandbox (`WorkerPool/ax-bug-bash-workers`) using one of three pre-installed CLI **Coding Harnesses** (**Antigravity** `google-antigravity`, **OpenCode** `opencode-ai`, or **Pi** `@mariozechner/pi-coding-agent`, or `multi` parallel mode) routed through the zero-secret in-cluster **Gemini Enterprise Agent Platform** (`Agent Platform`) **Model Gateway** (`:8080`, `internal/gateway/`).
   - To design for **compute obtainability**, the **Model Gateway** exchanges ephemeral task tokens for GKE Workload Identity credentials and executes an ordered 4-tier Gemini Flash fallback cascade (`gemini-3.8-flash` -> `gemini-3.7-flash` -> `gemini-3.6-flash` -> `gemini-3.5-flash`, plus deterministic `FIXTURE` fallback for offline CI).
   - **Child Tasks** enforce a strict Red-Green TDD cycle (`REPRODUCE_RED` -> `PATCH_GREEN`), capture their unified Git diff via the Go **Custom Runner** (`cmd/ax-bug-bash-runner`, `github.com/google/ax/runner.Run` `OnCommandExit`), and call `SuspendTask` (`SNAPSHOT_FIDELITY_VOLUMES`) so Agent Substrate `v0.6.0` checkpoints `/workspace` to Cloud Storage (`42.6ms`) and reclaims the warm worker slot (`61ms`).
   - The **Root Task** verifies child branches (`refs/heads/fix/<task>`), merges `--no-ff` into `main`, pushes to `storefront-git`, and triggers the selective hot-reload that heals `:3001` **Retail Storefront** back to `HTTP 200`.

### Three-layer architecture, evaluation tiers, and stage readiness (`ADR-0011` to `ADR-0014`)

- **Three-Layer Substrate Stack (`BOM-19`, [`ADR-0014`](docs/adr/0014-bom19-three-layer-architecture-guide-and-stage-warmup-gate.md))**: [`docs/architecture/three-layer-substrate-env-ax.md`](docs/architecture/three-layer-substrate-env-ax.md) specifies the CRD, gRPC, and container-boundary contracts across Layer 1 (`agent-substrate/substrate`), Layer 2 (`agent-substrate/env`), and Layer 3 (`google/ax`), paired with a 7-workload **Layer Selection Matrix** and cross-doc mapping for `ate.dev`, `google/ax/docs/`, and Google Cloud Docs.
- **Upstream `google/ax` Compatibility Overlay ([`ADR-0006`](docs/adr/0006-agent-substrate-v060-and-ax-deployment-hardening.md), [`ADR-0013`](docs/adr/0013-upstream-google-ax-v060-compatibility-overlay.md))**: [`docs/upstream/google-ax-v060-patches.md`](docs/upstream/google-ax-v060-patches.md) and [`deploy/ax-server/ax-server-v061.patch`](deploy/ax-server/ax-server-v061.patch) document the three upstream PR blueprints for `google/ax` Issues `#482` (`/healthz:80` `WakeupProbe`, `GOLDEN_SNAPSHOT_MODE_DISABLED`, and per-actor `EgressPolicy` before `ResumeActor`), `#511` (`go.ate.dev/substrate/api/ateapi/v1` proto path, OpenFGA bootstrap, and `context.WithoutCancel(ctx)` in `Server.SuspendTask`), and `#518` (`/workspace/.ax` and `/workspace/.ax-state/<task-name>.json` persistence across `SNAPSHOT_FIDELITY_VOLUMES`).
- **Flagship `BOM-20` Blog Post and 5-Lab Developer Tutorial ([`ADR-0012`](docs/adr/0012-bom20-blog-tutorial-and-stage-walkthrough-contract.md))**: [`docs/blog/self-spawning-bug-bash-tree-on-google-ax.md`](docs/blog/self-spawning-bug-bash-tree-on-google-ax.md) and [`docs/tutorial/build-a-self-spawning-bug-bash-tree.md`](docs/tutorial/build-a-self-spawning-bug-bash-tree.md), accompanied by [`docs/showcase/architecture-report.md`](docs/showcase/architecture-report.md), [`docs/showcase/slides.html`](docs/showcase/slides.html), and [`docs/assets/ax-live-gke-terminal.gif`](docs/assets/ax-live-gke-terminal.gif).
- **Live Stage Presenter Runbook and Stage Warmup Gate ([`ADR-0014`](docs/adr/0014-bom19-three-layer-architecture-guide-and-stage-warmup-gate.md))**: [`docs/stage-runbook.md`](docs/stage-runbook.md) and `make stage-ready` (`./hack/stage-warmup.sh`) verify GKE deployment readiness, `4/4 Ready` `ateworker-gvisor` slots, idempotent local port-forwards (`:3000`, `:3001`, `:3002`), `baseline-v1` reset, Prometheus alert drain, and 14-ADR `/api/about/catalog` health before a live talk.

## Architecture Decision Records (`ADR-0001` through `ADR-0014`)

All 14 ADRs live in `docs/adr/` and are verified against `/api/about/catalog` by `VerifyArchitectureCatalogDrift` (`internal/observer/about_catalog.go`):

1. [`ADR-0001`](docs/adr/0001-three-decoupled-surfaces-and-subtle-production-incidents.md): Three Decoupled Surfaces and Subtle Production Incidents
2. [`ADR-0002`](docs/adr/0002-in-cluster-prometheus-alertmanager-loki-grafana-with-gcp-export.md): In-Cluster Prometheus, Alertmanager, Loki, and Grafana with GCP Dual-Export
3. [`ADR-0003`](docs/adr/0003-polyglot-five-service-storefront-and-incident-catalog.md): Polyglot Five-Service Storefront, Incident Catalog, and Pre-Warmed Git Rollouts
4. [`ADR-0004`](docs/adr/0004-telemetry-driven-triage-and-tdd-child-task-lifecycle.md): Telemetry-Driven Root Triage and TDD Child Task Auto-Suspend Lifecycle
5. [`ADR-0005`](docs/adr/0005-clean-modern-ax-observer-ui-and-routing.md): Clean Modern AX Observer UI with Path-Based Routing
6. [`ADR-0006`](docs/adr/0006-agent-substrate-v060-and-ax-deployment-hardening.md): Agent Substrate v0.6.0 and `google/ax` Deployment Hardening
7. [`ADR-0007`](docs/adr/0007-multi-harness-sandbox-and-gemini-flash-fallback-cascade.md): Multi-Harness Sandbox and Gemini Flash Fallback Cascade
8. [`ADR-0008`](docs/adr/0008-dual-mode-execution-and-zero-trust-sandbox-coordination.md): Dual-Mode Execution and Zero-Trust In-Sandbox Bug Bash Coordination
9. [`ADR-0009`](docs/adr/0009-polyglot-http-mesh-and-zero-downtime-git-hot-reload.md): Polyglot Five-Service HTTP Mesh and Zero-Downtime Pre-Warmed Git Hot-Reload
10. [`ADR-0010`](docs/adr/0010-interactive-about-architecture-catalog-and-drift-verification.md): Interactive `/about` Architecture Topology Catalog and Automated Drift Verification
11. [`ADR-0011`](docs/adr/0011-two-tier-evaluation-chaos-soak-and-gcp-dual-export-verification.md): Two-Tier Evaluation Suite, Sequential `baseline-v1` Chaos Soak, and GCP Dual-Export Verification
12. [`ADR-0012`](docs/adr/0012-bom20-blog-tutorial-and-stage-walkthrough-contract.md): Flagship BOM-20 Blog Narrative, Five-Lab Developer Tutorial, and Stage Walkthrough Contract
13. [`ADR-0013`](docs/adr/0013-upstream-google-ax-v060-compatibility-overlay.md): Upstream `google/ax` Compatibility Overlay and Three-Patch PR Blueprints (`Issues #482, #511, #518`)
14. [`ADR-0014`](docs/adr/0014-bom19-three-layer-architecture-guide-and-stage-warmup-gate.md): BOM-19 Three-Layer Architecture Guide (`substrate`, `env`, `ax`), Layer Selection Matrix, and Stage Warmup Gate

## Key developer and verification commands

- `make run-local`: Start all three decoupled surfaces locally (`:3000` **AX Observer**, `:3001` **Retail Storefront**, `:3002` **Monitoring Stack**).
- `make test`: Run baseline unit and integration tests across all 5 polyglot services (`./services/run-service-tests.sh --all`) and all Go packages.
- `make smoke`: Run dry-run manifest/warmup script checks (`./hack/deploy-gke.sh --dry-run` and `./hack/stage-warmup.sh --dry-run`) and the fast `< 5s` `TestSmokeSuite` (`internal/evals/`).
- `make test-e2e`: Run the full end-to-end component and trajectory integration suite (`TestEndToEndIntegrationSuite`, `TestFourScenariosAcrossAllHarnessesAndLiveCascade`, `TestLiveGKEChaosSoakRunnerMockContract`, and `TestRepositoryHygieneAndManifestContract`).
- `make eval`: Run the Tier-1 hermetic 20-trajectory evaluation matrix (`4 Scenarios x 4 Harnesses` in `FIXTURE` mode + `4 LIVE` Gemini Flash cascade failover checks), manifest contract checks, and `/about` catalog drift verification (`internal/evals/`).
- `make verify-manifests`: Validate rendered Kubernetes/Substrate manifests (`@sha256:` image digests), static repository hygiene, and `VerifyMarkdownHumanizerAndHygiene` prose rules.
- `make deploy`: Deploy the Agent Substrate `v0.6.0` overlay, monitoring stack, and `bug-bash-system` manifests to the active GKE cluster (`kubectl config current-context`).
- `make stage-ready`: Run `./hack/stage-warmup.sh` to verify GKE pod and `WorkerPool` readiness, establish idempotent port-forwards (`:3000`, `:3001`, `:3002`), reset `storefront` to `baseline-v1`, drain alerts, and verify all 14 ADRs on `/api/about/catalog`.
- `make eval-live-gke`: Run the Tier-2 sequential (`MaxConcurrency = 1`) live GKE `4x4` chaos soak (`INC-101..104` across `antigravity`, `opencode`, `pi`, and `multi`) and verify Google Cloud Logging and Managed Service for Prometheus dual-export, writing a stage-safe scrubbed report to `/tmp/ax-live-gke-eval-report.json`.
- `make record-demo`: Record the live GKE terminal demo GIF (`docs/assets/ax-live-gke-terminal.gif`) and 6-scene stage walkthrough (`demo.mp4`).

## Terminology and hygiene guardrails

All code, comments, manifests, and markdown prose in this repository are checked by `VerifyRepositoryHygieneAndManifests` and `VerifyMarkdownHumanizerAndHygiene` (`internal/evals/evals.go`):

- Always use **Gemini Enterprise Agent Platform** or **Agent Platform** for Google's managed model platform.
- Always use **compute obtainability** or **designing for obtainability** when describing multi-tier model cascade failover and warm worker pool capacity.
- Never hardcode personal GKE cluster, Google Cloud project, node pool, or Cloud Storage bucket identifiers; resolve context dynamically via `kubectl config current-context` and `gcloud config get-value project`.
- Keep markdown prose crisp, natural, and free of em dashes, en dashes, spaced double-hyphens (` -- `), rhetorical `"not X, but Y"` contrasts, staged openers, dramatic one-line closers, and AI filler words.

## Agent skills

### Issue tracker

Issues and specs for this repo live in GitHub Issues (`gke-demos/ax-bug-bash`) managed via the `gh` CLI. See [`docs/agents/issue-tracker.md`](docs/agents/issue-tracker.md).

### Triage labels

Uses the five canonical triage labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See [`docs/agents/triage-labels.md`](docs/agents/triage-labels.md).

### Domain docs

Single-context layout ([`GLOSSARY.md`](GLOSSARY.md) at repo root and ADRs in [`docs/adr/`](docs/adr/0001-three-decoupled-surfaces-and-subtle-production-incidents.md)). See [`docs/agents/domain.md`](docs/agents/domain.md).
