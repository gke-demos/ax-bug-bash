# AX Bug Bash Tree

A self-spawning hierarchical agent system built on google/ax and Agent Substrate v0.6.0 where production alerts from a 5-service polyglot retail storefront trigger a root triage task that dynamically fans out isolated child AX tasks to write regression tests, patch affected microservices, and redeploy the fix.

## Language

**Bug Bash Tree**:
A hierarchical execution graph of AX Tasks where a root triage task decomposes a multi-service production incident into isolated child tasks that repair only the affected microservices in parallel.
_Avoid_: Agent swarm, agent pool, worker pipeline.

**Root Task**:
The top-level AX Task triggered by a monitoring alert webhook that analyzes production error traces, determines which microservices are implicated, spawns one Child Task per affected service via `ax-server`, and coordinates verification and redeployment.
_Avoid_: Orchestrator pod, master agent, coordinator container.

**Child Task**:
An ephemeral AX Task scoped to a single affected microservice that reads incident context from `Task.spec.config`, authors a failing regression test reproducing the production 500 error, fixes the service code until all tests pass, and auto-suspends.
_Avoid_: Sub-agent, worker job, patch worker.

**Coding Harness**:
One of the three CLI coding agents (`antigravity`, `opencode`, or `pi`) pre-installed in the task-runner image and selectable per task via `Task.spec.config`.
_Avoid_: Agent wrapper, CLI plugin.

**Model Gateway**:
The zero-secret in-cluster proxy authenticating to Gemini Enterprise Agent Platform via GKE Workload Identity that serves both Gemini REST and OpenAI-compatible endpoints with an automatic `gemini-3.8-flash` → `gemini-3.7-flash` → `gemini-3.6-flash` → `gemini-3.5-flash` fallback cascade.
_Avoid_: Key injector, external LLM proxy.

**Incident Scenario**:
A reproducible, subtle production failure mode across one or more microservices, uncovered by existing unit or integration tests, that can be activated on demand or randomly to trigger real HTTP 500 errors and monitoring alerts.
_Avoid_: Mock error, fake test failure, static fixture.

**Regression Test**:
A new unit or integration test authored by a Child Task that reproduces the exact production failure signature from the incident alert before applying the code fix.
_Avoid_: Pre-existing e2e test, smoke check.

**Retail Storefront**:
The standalone 5-service polyglot e-commerce application (`Node.js`, `Go`, `Python`, `Rust`) that users interact with directly in the browser and where production 500 errors surface.
_Avoid_: Embedded preview widget, mock store tab.

**Hot-Reload Supervisor**:
The in-pod process wrapper around each Retail Storefront microservice that tracks the canonical Git repository, incrementally rebuilds modified service code against pre-warmed caches, and swaps the live HTTP server without rescheduling the pod.
_Avoid_: Sidecar deployer, CI/CD agent, rolling restart controller.

**AX Observer**:
The dedicated web interface that visualizes incoming monitoring alerts, the live recursive AX Task Tree across Agent Substrate v0.6.0 workers, per-service regression tests and Git patches, and live Kubernetes rollout status.
_Avoid_: Chat portal, monolithic dashboard.

**Custom Runner**:
The in-sandbox Go executable embedding `github.com/google/ax/runner.Run` that supervises the agent process, extracts patches and test results via `OnCommandExit`, and auto-suspends the task to release Substrate worker resources.
_Avoid_: Entrypoint script, wrapper container, sidecar proxy.

**Execution Mode**:
Whether a Bug Bash Tree run dispatches tasks onto a live GKE `google/ax` and Agent Substrate v0.6.0 cluster (`LIVE_GKE`) or executes inside the hermetic local runtime (`SIMULATED`).
_Avoid_: Mock mode, local flag, stub backend.

**Source Provenance**:
Whether a task's model output and code repair originated from a live Gemini Enterprise Agent Platform model invocation through a Coding Harness (`LIVE`) or from a deterministic scenario fallback (`FIXTURE`).
_Avoid_: Mock flag, live mode, fallback status.

**Architecture Topology Catalog**:
The canonical, drift-tested inventory of the system's surfaces, microservices, Incident Scenarios, Coding Harnesses, and task-lifecycle transitions that drives the interactive `/about` visualizer.
_Avoid_: Static README copy, hardcoded diagram labels.

**Stage Walkthrough**:
The scripted, deterministic live demonstration across the three decoupled surfaces (`:3001` Retail Storefront, `:3002` Monitoring, and `:3000` AX Observer) showing an Incident Scenario from HTTP 500 trigger through Child Task Red-Green TDD to `< 10s` selective hot-reload and slot reclamation.
_Avoid_: Canned screengrab, mocked replay, synthetic slideshow.

**Three-Layer Substrate Stack**:
The architectural composition of Agent Substrate v0.6.0 sandbox actors and volume snapshots (Layer 1), `agent-substrate/env` guest lifecycle probes and workspace hooks (Layer 2), and `google/ax` declarative `Task`, `Workspace`, `Model`, and Custom Runner orchestration (Layer 3).
_Avoid_: Monolithic agent framework, single-layer CRD wrapper.

**Upstream Compatibility Overlay**:
The surgical `ax-server` and guest-runner patches (`Issue #482` `/healthz` `WakeupProbe` and per-actor `EgressPolicy` before `ResumeActor`, `Issue #511` OpenFGA `AccessPolicy` and detached `context.WithoutCancel(ctx)` in `SuspendTask`, and `Issue #518` `/workspace/.ax` volume-snapshot persistence) that bridge upstream `google/ax` (`4b7bf6f`) to Agent Substrate v0.6.0 on GKE.
_Avoid_: Permanent fork, monkey patch, workaround hack.

**Layer Selection Matrix**:
The architectural decision rubric that guides developers on when to target Layer 1 directly (raw Agent Substrate Actors and volume snapshots), Layer 2 (`agent-substrate/env` guest lifecycle services), or Layer 3 (`google/ax` declarative Tasks, Workspaces, Models, and Custom Runners).
_Avoid_: Feature comparison table, marketing tier list.

**Stage Warmup Gate**:
The pre-presentation readiness verification that resets the Retail Storefront to its clean baseline revision, drains firing monitoring alerts, confirms all warm sandbox worker slots are ready, and verifies local port-forwards across all three surfaces before a live demonstration.
_Avoid_: Manual setup checklist, ad-hoc cluster restart.
