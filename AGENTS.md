# AGENTS.md

Context for AI coding agents working in the `gke-demos/ax-bug-bash` repository (**The Self-Spawning Bug Bash Tree** — Recursive Sub-Task Fan-Out on `google/ax` and Agent Substrate `v0.6.0` on GKE).

## Project overview

`ax-bug-bash` demonstrates what makes `google/ax` (`docs/concepts.md`) uniquely powerful on top of **Agent Substrate `v0.6.0`**: *"A single task may be the whole job, or it may be the root of a large tree of tasks spawned as the agent breaks the problem down."*

The architecture is decoupled into three authentic production surfaces (`ADR-0001`):

1. **Standalone 5-Service Polyglot Retail Storefront (`:3001`, `ADR-0003`):**
   - `storefront-web` (**Node.js / TypeScript**): Customer-facing retail storefront UI and BFF gateway.
   - `checkout-service` (**Go**): Order orchestration, cart validation, and payment capture.
   - `shipping-service` (**Go**): Carrier rate calculation, weight tiers, and fulfillment dispatch.
   - `pricing-service` (**Python / FastAPI**): Promo code stacking, tax calculation, and currency conversion.
   - `inventory-service` (**Rust / Axum**): High-concurrency SKU stock reservation and warehouse ledger.
   - Pre-existing unit and integration tests pass on the happy path; 4 subtle production **Incident Scenarios** (`INC-101`–`INC-104`) can be activated or randomized on demand.

2. **Off-the-Shelf Monitoring & Alerting Stack (`:3002`, `ADR-0002`):**
   - Upstream **Prometheus + Alertmanager + Loki + Grafana** in-cluster with a 5-second scrape/evaluation window (`for: 0s`) that fires real Alertmanager webhooks (`POST /api/webhooks/alertmanager`) within 3–5 seconds of any `HTTP 500`, paired with Google Cloud Logging and Google Cloud Managed Service for Prometheus dual-export.

3. **Dedicated `AX Observer` & Recursive Bug Bash Tree (`:3000`, `ADR-0004`–`ADR-0007`):**
   - Receives Alertmanager webhooks and dispatches the **Root Task** (`triage-<id>`) on `google/ax`.
   - The Root Task analyzes production stack traces from `Task.spec.config`, determines which subset of the 5 microservices are implicated, and calls `ax-server` gRPC (`ax.v1alpha1.AX/CreateTask` + `ResumeTask`) to dynamically spawn one **Child Task** (`fixer-<service>`) per affected service.
   - Each Child Task runs inside an Agent Substrate `v0.6.0` `ateworker-gvisor` sandbox using one of three pre-installed **Coding Harnesses** (**Antigravity** `google-antigravity`, **OpenCode** `opencode-ai`, or **Pi** `@mariozechner/pi-coding-agent`) routed through a zero-secret in-cluster **Model Gateway** with an ordered fallback cascade of **`gemini-3.8-flash` → `gemini-3.7-flash` → `gemini-3.6-flash` → `gemini-3.5-flash`** (plus deterministic `FIXTURE` fallback for CI).
   - Child Tasks follow a strict Red-Green TDD cycle: author a new failing **Regression Test** reproducing the production 500 error (Red), patch the service code until all tests pass (Green), emit the Git patch via the **Custom Runner** (`runner.Run` `OnCommandExit`), and call `SuspendTask` so Agent Substrate `v0.6.0` immediately reclaims worker CPU/memory slots.
   - The Root Task merges all verified child patches, pushes to the in-cluster Git server, and triggers a `< 10s` pre-warmed incremental rollout that heals the live Retail Storefront.

## Agent skills

### Issue tracker

Issues and specs for this repo live in GitHub Issues (`gke-demos/ax-bug-bash`) managed via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Uses the five canonical triage labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout (`GLOSSARY.md` at repo root and ADRs in `docs/adr/`). See `docs/agents/domain.md`.
