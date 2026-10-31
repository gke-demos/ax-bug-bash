# Live Stage Presenter Runbook (`ax-bug-bash`)

This runbook walks a live stage presenter through a deterministic 5-to-6-minute demonstration of **The Self-Spawning Bug Bash Tree** across all three decoupled production surfaces on Google Kubernetes Engine (GKE). For background architecture and hands-on setup, see [`README.md`](../README.md), [`docs/architecture/three-layer-substrate-env-ax.md`](architecture/three-layer-substrate-env-ax.md), [`docs/blog/self-spawning-bug-bash-tree-on-google-ax.md`](blog/self-spawning-bug-bash-tree-on-google-ax.md), [`docs/tutorial/build-a-self-spawning-bug-bash-tree.md`](tutorial/build-a-self-spawning-bug-bash-tree.md), and [`docs/showcase/slides.html`](showcase/slides.html).

## 1. T-Minus 5 Minutes: Running the Stage Warmup Gate (`make stage-ready`)

Five minutes before walking on stage, run the one-command **Stage Warmup Gate** from the repository root:

```bash
make stage-ready
```

`make stage-ready` executes `./hack/stage-warmup.sh`, which completes in under 10 seconds and verifies six pre-stage invariants:

1. **Control Plane and Polyglot Mesh Readiness**: Confirms `ax-postgres`, `ax-server`, and `model-gateway` (`ax-system`), all five polyglot storefront deployments plus `storefront-git` and `ax-observer` (`ax-bug-bash`), and `prometheus-alertmanager` plus `loki-grafana` (`monitoring`) are `Available`, and verifies that `WorkerPool/ax-bug-bash-workers` (`ate.dev/v1`) has `4/4 Ready` warm `ateworker-gvisor` sandbox pods.
2. **Idempotent Local Port-Forwards (`/tmp/ax-bug-bash-stage-pf.pids`)**: Probes `http://127.0.0.1:3000/healthz` (**AX Observer**), `http://127.0.0.1:3001/healthz` (**Retail Storefront**), and `http://127.0.0.1:3002/api/health` (**Grafana**) first. If healthy listeners already exist, `make stage-ready` reuses them without failing on `bind: address already in use`. Otherwise it cleans up stale PIDs in `/tmp/ax-bug-bash-stage-pf.pids` and starts background `kubectl port-forward` processes.
3. **Baseline Git Reset (`baseline-v1`)**: Calls `POST http://127.0.0.1:3000/api/incidents/reset` (`{"default_harness":"multi"}`) and verifies that `GET http://127.0.0.1:3001/api/state` reports `git_revision == "baseline-v1"` and `active_incident == ""`.
4. **Alert Drain Verification**: Polls **AX Observer** and Prometheus `/api/v1/alerts` to confirm zero residual `StorefrontHTTP500Spike` alerts are firing.
5. **Warm Sandbox Pool and Model Gateway Verification**: Confirms `GET http://127.0.0.1:3000/api/workers` reports `execution_mode == "LIVE_GKE"` with 4 warm `ateworker-gvisor` slots ready.
6. **14-ADR `/about` Architecture Topology Catalog Verification**: Confirms `GET http://127.0.0.1:3000/api/about/catalog` serves all `14` ADRs (`ADR-0001` through `ADR-0014`), `12` topology nodes, and `11` animated signal edges.

Open three browser tabs side-by-side in the presentation window:

- **Tab 1 (Surface 1: Retail Storefront)**: `http://localhost:3001`
- **Tab 2 (Surface 2: Monitoring Stack / Grafana)**: `http://localhost:3002/d/ax-bug-bash-storefront/ax-bug-bash-polyglot-storefront-and-rollouts?orgId=1&kiosk`
- **Tab 3 (Surface 3: AX Observer)**: `http://localhost:3000`

> [!TIP]
> **Auditorium Projector Light Mode:** If the venue projector washes out dark backgrounds, click the icon-only **Sun/Moon toggle** in the top-right corner of **AX Observer** (`http://localhost:3000`). The toggle switches every surface card and SVG diagram to high-contrast **Projector Light Mode** using pure CSS color tokens with zero layout shift.

## 2. Cue 1 (0:00 to 1:30): Surface 1 (`:3001` Retail Storefront) from Happy Path to Multi-Service HTTP 500

1. **Show the Healthy Polyglot Baseline (`baseline-v1`)**:
   - Start on **Tab 1 (`http://localhost:3001`)**. Point out that the **Retail Storefront** runs five real microservices across four languages (`storefront-web` in Node.js/TypeScript, `checkout-service` in Go, `shipping-service` in Go, `pricing-service` in Python/FastAPI, and `inventory-service` in Rust/Axum), and all five services passed their CI unit test suites.
   - Click **Place Order** on the default cart to show an immediate `HTTP 200 CONFIRMED` checkout response across all five services.
2. **Activate a Subtle Multi-Service Incident Scenario (`⌘K`)**:
   - Switch briefly to **Tab 3 (`http://localhost:3000`)** and press **`⌘K`** (or `Ctrl+K`) to open the discrete **Incident Scenario & Harness Launcher** modal.
   - Select **`INC-101` (Stacked Promo Negative Subtotal Crash)** or **`INC-102` (Flash-Sale Bundle `u32` Stock Underflow)** with Coding Harness mode set to **`multi`** (`Antigravity` + `OpenCode` + `Pi` in parallel).
3. **Trigger the Live Production Failure on `:3001`**:
   - Return to **Tab 1 (`http://localhost:3001`)** and click **Place Order**.
   - Show the live `HTTP 500 Internal Server Error` banner. For `INC-101`, point out that stacking `SAVE20` and `FLAT15` on a `$16.00` USB-C cable drove `pricing-service` (Python) to `-220` cents (`NegativeSubtotalError`) and caused `shipping-service` (Go) to reject the non-positive subtotal (`CarrierTariffBoundsError`), while `storefront-web`, `checkout-service`, and `inventory-service` remained healthy.

## 3. Cue 2 (1:30 to 2:30): Surface 2 (`:3002` Grafana and Alertmanager) Sub-15s Detection and LogQL Stack Trace Correlation

1. **Show the Real-Time Golden Signals Spike**:
   - Switch to **Tab 2 (`http://localhost:3002`)**.
   - Point out that Prometheus scrapes all five service `/metrics` endpoints every `5s` and evaluates `StorefrontHTTP500Spike` over a 3-sample `[15s]` window with `for: 0s`.
2. **Explain LogQL Correlation and Webhook Dispatch**:
   - Highlight the structured JSON error logs streamed into Loki with a shared `X-Storefront-Request-ID` header and show Alertmanager grouping the firing alert by `alertname` and dispatching `POST /api/webhooks/alertmanager` to the **AX Observer**.

## 4. Cue 3 (2:30 to 4:30): Surface 3 (`:3000` AX Observer) Recursive Root Triage, Parallel Red-Green TDD Child Tasks, and Selective Hot-Reload

1. **Show Selective Blast-Radius Triage by the Root Task**:
   - Switch to **Tab 3 (`http://localhost:3000`)**.
   - Point out the **Root Task** (`triage-inc-101`) running inside a warm `ateworker-gvisor` sandbox on `google/ax`. Emphasize that the **Root Task** queried Loki stack traces, identified **only the implicated microservices** (`pricing-service` and `shipping-service` for `INC-101`), and skipped the three healthy services (`storefront-web`, `checkout-service`, and `inventory-service`).
2. **Show Parallel Multi-Harness Child Tasks Executing Red-Green TDD**:
   - Watch the **Root Task** call `ax.v1alpha1.AX/CreateTask` + `ResumeTask` in parallel to spawn isolated **Child Tasks** (`fixer-inc-101-pricing-service` on **Antigravity** and `fixer-inc-101-shipping-service` on **OpenCode**), passing per-service failure context via `Task.spec.config` (`google.protobuf.Struct`).
   - Click a **Child Task** card to expand its live **Regression Test** (`REPRODUCE_RED`) and unified Git diff (`PATCH_GREEN`). Point out that every model request routes through the zero-secret **Gemini Enterprise Agent Platform** **Model Gateway** (`gemini-3.8-flash` -> `3.7` -> `3.6` -> `3.5` cascade) using an ephemeral per-task session token.
3. **Highlight `OnCommandExit` -> `SuspendTask` (`61ms` Slot Reclamation) and `< 3s` Selective Hot-Reload**:
   - Point out that as soon as each **Child Task** pushes `refs/heads/fix/<task>`, our Go **Custom Runner** (`github.com/google/ax/runner.Run`) executes `OnCommandExit` and calls `SuspendTask`, checkpointing `/workspace` to Cloud Storage (`SNAPSHOT_FIDELITY_VOLUMES`) in `42.6ms` and returning the `ateworker-gvisor` pod to the warm pool in `61ms`.
   - Show the **Root Task** merging the verified branches into `storefront-git` and triggering the PID-1 **Hot-Reload Supervisor** to swap only the modified services in under `3s` with zero pod restarts.
   - Switch back to **Tab 1 (`http://localhost:3001`)** and click **Place Order** on the exact same cart to show `HTTP 200 CONFIRMED`.

## 5. Cue 4 (4:30 to 6:00): `/workers`, `/about` Architecture Topology Catalog, and Emergency Venue Fallbacks

1. **Inspect Warm Sandbox Telemetry (`/workers`)**:
   - In **Tab 3 (`http://localhost:3000`)**, click **`/workers`** in the top navigation bar.
   - Show the 4 warm `ateworker-gvisor` slots (`WorkerPool/ax-bug-bash-workers`), their `SNAPSHOT_FIDELITY_VOLUMES` Cloud Storage checkpoint latencies (`42.6ms`), and the zero-secret **Model Gateway** session audit table showing `Source Provenance: LIVE`.
2. **Walk Through the Interactive 3-Tab `/about` Visualizer (`/about`)**:
   - Click **`/about`** in the top navigation bar to open the Go-backed **Architecture Topology Catalog**:
     - **Tab 1 (3-Surface Mesh & Agent Substrate Topology)**: Click any of the `12` interactive SVG nodes or `11` animated signal edges, or select `INC-101` through `INC-104` to preview its blast radius and click **"Launch in Bug Bash Tree ->"** to pre-populate the `⌘K` modal.
     - **Tab 2 (Recursive Task-Tree & TDD Lifecycle Simulator)**: Step through the 6 lifecycle stages (`1. Alert Webhook` -> `6. Selective Hot-Reload`) and watch the 4 warm sandbox slots transition between `WARM_IDLE`, `ACTIVE_SANDBOX`, and `RECLAIMED_WARM`.
     - **Tab 3 (Zero-Trust Security & 14-ADR Catalog)**: Point out the 14 drift-tested Architecture Decision Records (`ADR-0001` through `ADR-0014`), including `ADR-0013` (*Upstream `google/ax` Compatibility Overlay*) and `ADR-0014` (*BOM-19 Three-Layer Architecture Guide & Layer Selection Matrix*).
3. **Between Demos or Emergency Venue Fallbacks**:
   - **Resetting Between Back-to-Back Demos (`< 10s`)**: Run `make stage-ready` in the terminal (or click **Reset Baseline (`baseline-v1`)** inside the `⌘K` modal) to immediately restore `baseline-v1` and drain alerts.
   - **Stopping Background Port-Forwards After the Session**: Run `./hack/stage-warmup.sh --stop` to cleanly terminate the managed port-forward PIDs recorded in `/tmp/ax-bug-bash-stage-pf.pids`.
   - **Zero-Wi-Fi Hermetic Fallback (`SIMULATED` Mode)**: If venue Wi-Fi drops completely before your talk, run `make run-local` to launch all three surfaces locally on `:3000`, `:3001`, and `:3002` backed by the deterministic `SIMULATED` orchestrator and in-process polyglot mesh.
