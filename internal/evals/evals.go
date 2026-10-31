package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// HarnessModes defines the 4 harness configurations evaluated across all 4 Incident Scenarios.
var HarnessModes = []string{
	"antigravity",
	"opencode",
	"pi",
	"multi",
}

// TrajectorySpec configures a single end-to-end Bug Bash Tree evaluation run.
type TrajectorySpec struct {
	IncidentID           string
	HarnessMode          string
	RequestedModel       string
	ProvenanceMode       gateway.SourceProvenance
	SimulatedCascadeDrop int // Number of leading cascade models to fail with 429/503 in LIVE mode before 200 OK
}

// ChildTaskGrade captures the per-service evaluation grade for a spawned Child Task.
type ChildTaskGrade struct {
	TaskName             string   `json:"task_name"`
	Service              string   `json:"service"`
	Language             string   `json:"language"`
	Harness              string   `json:"harness"`
	RequestedModel       string   `json:"requested_model"`
	ActiveModel          string   `json:"active_model"`
	SourceProvenance     string   `json:"source_provenance"`
	RegressionTestName   string   `json:"regression_test_name"`
	HasStructuredConfig  bool     `json:"has_structured_config"`
	ReproduceRedVerified bool     `json:"reproduce_red_verified"`
	PatchGreenVerified   bool     `json:"patch_green_verified"`
	DiffCaptured         bool     `json:"diff_captured"`
	VolumeStateSaved     bool     `json:"volume_state_saved"`
	SuspendedAndReclaim  bool     `json:"suspended_and_reclaim"`
	SlotReclamationMs    int64    `json:"slot_reclamation_ms"`
	TDDPhases            []string `json:"tdd_phases"`
}

// TrajectoryGrade records the 6-invariant evaluation result for a single Scenario x Harness trajectory.
type TrajectoryGrade struct {
	IncidentID         string                   `json:"incident_id"`
	HarnessMode        string                   `json:"harness_mode"`
	ProvenanceMode     gateway.SourceProvenance `json:"provenance_mode"`
	RequestedModel     string                   `json:"requested_model"`
	ActiveModel        string                   `json:"active_model"`
	ExecutionID        string                   `json:"execution_id"`
	RootTaskName       string                   `json:"root_task_name"`
	ImplicatedServices []string                 `json:"implicated_services"`
	UnaffectedServices []string                 `json:"unaffected_services"`
	SpawnedChildCount  int                      `json:"spawned_child_count"`
	ChildGrades        []ChildTaskGrade         `json:"child_grades"`

	// 6 Core Invariants
	BaselineCIGreen                     bool `json:"baseline_ci_green"`
	AlertmanagerWebhookTriggered        bool `json:"alertmanager_webhook_triggered"`
	SelectiveBlastRadiusFanOut          bool `json:"selective_blast_radius_fan_out"`
	RedGreenRegressionTDD               bool `json:"red_green_regression_tdd"`
	OnCommandExitAndSuspendReclaimed    bool `json:"on_command_exit_and_suspend_reclaimed"`
	PreWarmedRolloutAndStorefrontHealed bool `json:"pre_warmed_rollout_and_storefront_healed"`

	PreRolloutCheckoutStatus  int      `json:"pre_rollout_checkout_status"`
	PostRolloutCheckoutStatus int      `json:"post_rollout_checkout_status"`
	AlertStatusAfterRollout   string   `json:"alert_status_after_rollout"`
	RolloutDurationMs         int64    `json:"rollout_duration_ms"`
	TotalReclaimedWorkerSlots int      `json:"total_reclaimed_worker_slots"`
	CascadeAttemptsObserved   int      `json:"cascade_attempts_observed"`
	FailureReasons            []string `json:"failure_reasons,omitempty"`
	Passed                    bool     `json:"passed"`
}

// SuiteConfig configures the full 4-Scenario x 4-Harness evaluation matrix.
type SuiteConfig struct {
	Scenarios                []string
	Harnesses                []string
	IncludeLiveCascadeChecks bool
	MaxConcurrency           int
}

// SuiteReport summarizes the execution of the full evaluation matrix.
type SuiteReport struct {
	TotalTrajectories     int               `json:"total_trajectories"`
	PassedTrajectories    int               `json:"passed_trajectories"`
	FailedTrajectories    int               `json:"failed_trajectories"`
	AllPassed             bool              `json:"all_passed"`
	ScenarioCoverage      map[string]int    `json:"scenario_coverage"`
	HarnessCoverage       map[string]int    `json:"harness_coverage"`
	CascadeModelsVerified map[string]bool   `json:"cascade_models_verified"`
	Trajectories          []TrajectoryGrade `json:"trajectories"`
}

// RunEvaluationSuite executes the 4-Scenario x 4-Harness evaluation matrix (plus optional LIVE Gemini Flash
// Cascade trajectories) with bounded concurrency and grades all 6 invariants on every trajectory.
func RunEvaluationSuite(ctx context.Context, cfg SuiteConfig) (SuiteReport, error) {
	scenarios := cfg.Scenarios
	if len(scenarios) == 0 {
		for _, sc := range storefront.ScenarioCatalog() {
			scenarios = append(scenarios, sc.ID)
		}
	}
	harnesses := cfg.Harnesses
	if len(harnesses) == 0 {
		harnesses = append([]string(nil), HarnessModes...)
	}

	var specs []TrajectorySpec
	for _, scID := range scenarios {
		for _, h := range harnesses {
			specs = append(specs, TrajectorySpec{
				IncidentID:     scID,
				HarnessMode:    h,
				RequestedModel: gateway.DefaultFallbackCascade[0],
				ProvenanceMode: gateway.ProvenanceFixture,
			})
		}
	}

	if cfg.IncludeLiveCascadeChecks {
		liveChecks := []TrajectorySpec{
			{
				IncidentID:           storefront.IncidentStackedPromoNegativeSubtotal,
				HarnessMode:          "antigravity",
				RequestedModel:       gateway.DefaultFallbackCascade[0],
				ProvenanceMode:       gateway.ProvenanceLive,
				SimulatedCascadeDrop: 0, // ActiveModel: gemini-3.8-flash
			},
			{
				IncidentID:           storefront.IncidentFlashSaleBundleUnderflow,
				HarnessMode:          "opencode",
				RequestedModel:       gateway.DefaultFallbackCascade[0],
				ProvenanceMode:       gateway.ProvenanceLive,
				SimulatedCascadeDrop: 1, // ActiveModel: gemini-3.7-flash
			},
			{
				IncidentID:           storefront.IncidentZeroDecimalJPYRounding,
				HarnessMode:          "pi",
				RequestedModel:       gateway.DefaultFallbackCascade[0],
				ProvenanceMode:       gateway.ProvenanceLive,
				SimulatedCascadeDrop: 2, // ActiveModel: gemini-3.6-flash
			},
			{
				IncidentID:           storefront.IncidentLithiumHazmatWeightTier,
				HarnessMode:          "multi",
				RequestedModel:       gateway.DefaultFallbackCascade[0],
				ProvenanceMode:       gateway.ProvenanceLive,
				SimulatedCascadeDrop: 3, // ActiveModel: gemini-3.5-flash
			},
		}
		specs = append(specs, liveChecks...)
	}

	maxConc := cfg.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 4
	}
	sem := make(chan struct{}, maxConc)

	grades := make([]TrajectoryGrade, len(specs))
	errs := make([]error, len(specs))
	var wg sync.WaitGroup

	for i, sp := range specs {
		i, sp := i, sp
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g, err := RunScenarioTrajectory(ctx, sp)
			grades[i] = g
			errs[i] = err
		}()
	}
	wg.Wait()

	report := SuiteReport{
		TotalTrajectories:     len(specs),
		ScenarioCoverage:      make(map[string]int),
		HarnessCoverage:       make(map[string]int),
		CascadeModelsVerified: make(map[string]bool),
		Trajectories:          grades,
	}

	var firstErr error
	for i, g := range grades {
		if errs[i] != nil && firstErr == nil {
			firstErr = errs[i]
		}
		if g.Passed && errs[i] == nil {
			report.PassedTrajectories++
			report.ScenarioCoverage[g.IncidentID]++
			report.HarnessCoverage[g.HarnessMode]++
			if g.ProvenanceMode == gateway.ProvenanceLive && g.ActiveModel != "" {
				report.CascadeModelsVerified[g.ActiveModel] = true
			}
		} else {
			report.FailedTrajectories++
		}
	}
	report.AllPassed = report.PassedTrajectories == report.TotalTrajectories && report.TotalTrajectories > 0
	return report, firstErr
}

// HygieneReport summarizes static repository hygiene and GKE/Agent Substrate v0.6.0 manifest contract checks.
type HygieneReport struct {
	CheckedFiles []string `json:"checked_files"`
	Violations   []string `json:"violations,omitempty"`
	Passed       bool     `json:"passed"`
}

var (
	inlineCodeSpanRe      = regexp.MustCompile("`[^`]*`")
	mdLinkTargetRe        = regexp.MustCompile(`\]\([^)]*\)`)
	contrastTellRe        = regexp.MustCompile(`(?i)(?:\bnot\s+(?:just|only|merely)\s+[^.!?\n]{1,120}?,\s*(?:but|it's|it\s+is)\b|\bnot\s+(?:a|an|the|about|by|for|in|on|to|with|[a-z0-9_-]+)\s+[^.!?\n]{0,80}?,\s*but\b|\bit(?:'s|\s+is)\s+not\s+[^.!?\n]{1,80}?,\s*it(?:'s|\s+is)\b)`)
	stagedOpenerRe        = regexp.MustCompile(`(?i)(?:\b(?:let's|let\s+us)\s+(?:dive\s+in|explore|break\s+this\s+down|be\s+honest)\b|\bhere's\s+the\s+thing\b|\bhere's\s+what\s+you\s+need\s+to\s+know\b|\bwithout\s+further\s+ado\b|\bin\s+this\s+(?:blog\s+)?post,?\s+we\s+will\b)`)
	dramaticCloserRe      = regexp.MustCompile("(?im)^\\s*(?:that is the real win\\.|that distinction matters\\.|read that again\\.|let that sink in\\.|the old rules were gone\\.)\\s*$")
	aiFillerWordRe        = regexp.MustCompile(`(?i)\b(?:delve|delves|delving|tapestry|testament|pivotal|seamlessly|unlock|unlocks|unlocking|paradigm|crucial|meticulous|meticulously|groundbreaking|breathtaking)\b`)
	markdownStageUnsafeRe = regexp.MustCompile(`(?i)(?:gs://[a-z0-9._/-]+|\bgke_[a-z0-9-]+_[a-z0-9-]+_[a-z0-9-]+\b|\bgke-[a-z0-9-]+-(?:pool|node|default)[a-z0-9_-]*|\b\d{12}\b|\bprojects/[a-z][a-z0-9-]{4,29}\b)`)
)

func forbiddenHygieneTokens() []string {
	return []string{
		"Vertex" + " AI",
		"fluid" + " compute",
		"ate" + "-cluster",
		"mofi" + "labs",
		"598464" + "211339",
		"gke_mofi" + "labs_us-central1-a_ate-cluster",
		"gke-ate" + "-cluster",
		"substrate" + "-poc",
		"/usr/local/google/" + "home/",
	}
}

// VerifyMarkdownHumanizerAndHygiene enforces the ADR-0012 humanizer/unslop prose rules and stage-safe
// hygiene contract on markdown documentation files. Code blocks and inline code spans are excluded from
// prose punctuation/phrasing checks while stage-safe token checks run across the entire document.
func VerifyMarkdownHumanizerAndHygiene(relPath, content string) []string {
	var violations []string

	lowerFull := strings.ToLower(content)
	for _, bad := range forbiddenHygieneTokens() {
		if strings.Contains(lowerFull, strings.ToLower(bad)) {
			violations = append(violations, fmt.Sprintf("%s contains forbidden token %q", relPath, bad))
		}
	}
	if match := markdownStageUnsafeRe.FindString(content); match != "" {
		violations = append(violations, fmt.Sprintf("%s contains forbidden stage-unsafe cluster/project/node/bucket identifier %q", relPath, match))
	}

	lines := strings.Split(content, "\n")
	inFence := false
	for idx, rawLine := range lines {
		lineNum := idx + 1
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}

		// Strip inline code spans and markdown link URLs before checking prose rules.
		proseLine := inlineCodeSpanRe.ReplaceAllString(rawLine, " ")
		proseLine = mdLinkTargetRe.ReplaceAllString(proseLine, "]()")

		if strings.ContainsRune(proseLine, '\u2014') {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden em dash in prose", relPath, lineNum))
		}
		if strings.ContainsRune(proseLine, '\u2013') {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden en dash in prose", relPath, lineNum))
		}
		if strings.Contains(proseLine, " -- ") {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden spaced double-hyphen dash in prose", relPath, lineNum))
		}
		if match := contrastTellRe.FindString(proseLine); match != "" {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden rhetorical contrast %q", relPath, lineNum, match))
		}
		if match := stagedOpenerRe.FindString(proseLine); match != "" {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden staged opener %q", relPath, lineNum, match))
		}
		if match := dramaticCloserRe.FindString(proseLine); match != "" {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden dramatic one-line closer %q", relPath, lineNum, strings.TrimSpace(match)))
		}
		if match := aiFillerWordRe.FindString(proseLine); match != "" {
			violations = append(violations, fmt.Sprintf("%s:%d contains forbidden AI filler word %q", relPath, lineNum, match))
		}
	}

	return violations
}

// VerifyRepositoryHygieneAndManifests validates that all deployment manifests, scripts, Makefile, README.md,
// and Phase 3 blog/tutorial markdown files satisfy the ADR-0006, ADR-0007, and ADR-0012 contracts.
func VerifyRepositoryHygieneAndManifests(repoRoot string) (HygieneReport, error) {
	if strings.TrimSpace(repoRoot) == "" {
		repoRoot = "."
	}
	filesToScan := []string{
		"manifests/ax-v060-overlay.yaml",
		"manifests/bug-bash-system.yaml",
		"manifests/monitoring/01-prometheus-alertmanager.yaml",
		"manifests/monitoring/02-loki-grafana.yaml",
		"manifests/monitoring/03-gmp-podmonitoring.yaml",
		"deploy/ax-observer/Dockerfile",
		"deploy/model-gateway/Dockerfile",
		"deploy/ax-server/Dockerfile",
		"deploy/ax-server/build.sh",
		"harnesses/Dockerfile.task-runner",
		"harnesses/config/opencode.json",
		"harnesses/config/pi-models.json",
		"Dockerfile.polyglot-mesh",
		"internal/httputil/httputil.go",
		"internal/observer/about_catalog.go",
		"services/run-service-tests.sh",
		"hack/deploy-gke.sh",
		"hack/run-local.sh",
		"hack/record-stage-walkthrough.sh",
		"hack/record-terminal-demo.py",
		"hack/stage-warmup.sh",
		"Makefile",
		"README.md",
		"docs/architecture/three-layer-substrate-env-ax.md",
		"docs/blog/self-spawning-bug-bash-tree-on-google-ax.md",
		"docs/tutorial/build-a-self-spawning-bug-bash-tree.md",
		"docs/stage-runbook.md",
		"docs/upstream/google-ax-v060-patches.md",
		"docs/showcase/slides.html",
		"docs/showcase/architecture-report.md",
	}

	forbiddenTokens := forbiddenHygieneTokens()

	contentsByFile := make(map[string]string, len(filesToScan))
	var report HygieneReport

	for _, rel := range filesToScan {
		fullPath := filepath.Join(repoRoot, rel)
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			report.Violations = append(report.Violations, fmt.Sprintf("missing or unreadable file %s: %v", rel, err))
			continue
		}
		report.CheckedFiles = append(report.CheckedFiles, rel)
		text := string(raw)
		contentsByFile[rel] = text

		if strings.HasPrefix(rel, "docs/architecture/") || strings.HasPrefix(rel, "docs/blog/") || strings.HasPrefix(rel, "docs/tutorial/") || strings.HasPrefix(rel, "docs/upstream/") || rel == "docs/stage-runbook.md" || rel == "docs/showcase/architecture-report.md" {
			report.Violations = append(report.Violations, VerifyMarkdownHumanizerAndHygiene(rel, text)...)
			continue
		}

		lowerText := strings.ToLower(text)
		for _, bad := range forbiddenTokens {
			if strings.Contains(lowerText, strings.ToLower(bad)) {
				report.Violations = append(report.Violations, fmt.Sprintf("%s contains forbidden string %q", rel, bad))
			}
		}
	}

	if err := observer.VerifyArchitectureCatalogDrift(repoRoot); err != nil {
		report.Violations = append(report.Violations, fmt.Sprintf("about architecture catalog drift: %v", err))
	}

	requireSnippets := func(rel string, snippets []string) {
		text, ok := contentsByFile[rel]
		if !ok {
			return
		}
		for _, s := range snippets {
			if !strings.Contains(text, s) {
				report.Violations = append(report.Violations, fmt.Sprintf("%s missing required contract snippet %q", rel, s))
			}
		}
	}

	requireSnippets("manifests/ax-v060-overlay.yaml", []string{
		"name: ax-system",
		"name: ax-bug-bash",
		`ate.dev/api-access: "true"`,
		"kind: AccessPolicy",
		"user:system:serviceaccount:ax-system:ax-server",
		"user:system:serviceaccount:ax-bug-bash:ax-observer",
		"role: owner",
		"role: viewer",
		"kind: WorkerPool",
		"name: ax-bug-bash-workers",
		"ate.dev/v1",
		"name: gvisor",
		"name: gvisor-default",
		"ateworker-gvisor",
		"SNAPSHOT_FIDELITY_VOLUMES",
		"kind: Workspace",
		"name: storefront-monorepo",
		"http://storefront.ax-bug-bash.svc.cluster.local:3001/git/storefront.git",
		"kind: Model",
		"name: gemini-flash-cascade",
		"http://model-gateway.ax-system.svc.cluster.local:8080",
	})

	requireSnippets("manifests/bug-bash-system.yaml", []string{
		"name: ax-postgres",
		"name: ax-server",
		"name: model-gateway",
		"containerPort: 8080",
		"name: storefront",
		"containerPort: 3001",
		"name: checkout-service",
		"name: shipping-service",
		"name: pricing-service",
		"name: inventory-service",
		"name: storefront-git",
		"name: ax-observer",
		"containerPort: 3000",
		"AX_DEFAULT_TASK_IMAGE",
		"@sha256:",
		"${MODEL_GATEWAY_SHA256}",
		"${STOREFRONT_SHA256}",
		"${AX_OBSERVER_SHA256}",
		"${TASK_RUNNER_SHA256}",
		"${AX_SERVER_SHA256}",
		"readinessProbe:",
		"livenessProbe:",
		"allowPrivilegeEscalation: false",
		"resources:",
		"requests:",
		"limits:",
	})

	requireSnippets("manifests/monitoring/01-prometheus-alertmanager.yaml", []string{
		"readinessProbe:",
		"livenessProbe:",
		"allowPrivilegeEscalation: false",
		"resources:",
	})

	requireSnippets("manifests/monitoring/02-loki-grafana.yaml", []string{
		"readinessProbe:",
		"livenessProbe:",
		"allowPrivilegeEscalation: false",
		"resources:",
	})

	requireSnippets("manifests/monitoring/03-gmp-podmonitoring.yaml", []string{
		"kind: PodMonitoring",
		"monitoring.googleapis.com/v1",
		"interval: 10s",
	})

	requireSnippets("hack/deploy-gke.sh", []string{
		"kubectl config current-context",
		"gcloud config get-value project",
		"@sha256:",
		"manifests/ax-v060-overlay.yaml",
		"manifests/monitoring/01-prometheus-alertmanager.yaml",
		"manifests/monitoring/02-loki-grafana.yaml",
		"manifests/monitoring/03-gmp-podmonitoring.yaml",
		"manifests/bug-bash-system.yaml",
	})

	requireSnippets("hack/run-local.sh", []string{
		"3000",
		"3001",
		"3002",
		"./cmd/ax-observer",
	})

	requireSnippets("hack/record-stage-walkthrough.sh", []string{
		"Xvfb",
		"ffmpeg",
		"gbrowser",
		"anullsrc",
		"ax-bug-bash-storefront",
		"/workers",
		"/about",
		"/tmp/ax-bug-bash-demo.mp4",
	})

	requireSnippets("hack/record-terminal-demo.py", []string{
		"#282828",
		"#ebdbb2",
		"#ff5f56",
		"#ffbd2e",
		"#27c93f",
		"workerpools.ate.dev",
		"docs/assets/ax-live-gke-terminal.gif",
	})

	requireSnippets("Makefile", []string{
		"build:",
		"test:",
		"smoke:",
		"test-e2e:",
		"eval:",
		"eval-live-gke:",
		"run-local:",
		"images:",
		"deploy:",
		"verify-manifests:",
		"stage-ready:",
		"record-demo:",
	})

	requireSnippets("hack/stage-warmup.sh", []string{
		"/tmp/ax-bug-bash-stage-pf.pids",
		"--stop",
		"baseline-v1",
		"/api/workers",
		"/api/about/catalog",
		"STAGE WARMUP GATE PASSED",
	})

	requireSnippets("docs/architecture/three-layer-substrate-env-ax.md", []string{
		"Why the Agent Execution Stack Splits into Three Layers",
		"Layer 1 (`agent-substrate/substrate`)",
		"Layer 2 (`agent-substrate/env`)",
		"Layer 3 (`google/ax`)",
		"The Layer Selection Matrix",
		"High-Frequency Stateless Code Interpreter",
		"Custom Non-HTTP Guest Daemon",
		"Interactive Browser IDE",
		"External Workflow Engine Step Runner",
		"Single-Repo Autonomous Coding Task",
		"Recursive Multi-Service Incident Response",
		"Multi-Harness Comparative Evaluation Benchmark",
		"Gemini Enterprise Agent Platform",
		"compute obtainability",
	})

	requireSnippets("docs/stage-runbook.md", []string{
		"make stage-ready",
		"Projector Light Mode",
		"Cue 1",
		"Cue 2",
		"Cue 3",
		"Cue 4",
		"/workers",
		"/about",
		"ADR-0014",
		"Gemini Enterprise Agent Platform",
	})

	requireSnippets("README.md", []string{
		"INC-101",
		"INC-102",
		"INC-103",
		"INC-104",
		"antigravity",
		"opencode",
		"pi",
		"gemini-3.8-flash",
		"gemini-3.7-flash",
		"gemini-3.6-flash",
		"gemini-3.5-flash",
		"Gemini Enterprise Agent Platform",
		"Agent Substrate",
		"docs/architecture/three-layer-substrate-env-ax.md",
		"docs/blog/self-spawning-bug-bash-tree-on-google-ax.md",
		"docs/tutorial/build-a-self-spawning-bug-bash-tree.md",
		"docs/stage-runbook.md",
		"docs/upstream/google-ax-v060-patches.md",
		"docs/showcase/slides.html",
		"docs/showcase/architecture-report.md",
		"docs/assets/ax-live-gke-terminal.gif",
	})

	requireSnippets("docs/upstream/google-ax-v060-patches.md", []string{
		"Issue #482",
		"Issue #511",
		"Issue #518",
		"/healthz",
		"GOLDEN_SNAPSHOT_MODE_DISABLED",
		"context.WithoutCancel",
		"/workspace/.ax",
	})

	requireSnippets("docs/showcase/slides.html", []string{
		`class="slide`,
		"The Self-Spawning Bug Bash Tree",
		"BOM-19",
		"BOM-20",
		"ADR-0013",
	})

	requireSnippets("docs/showcase/architecture-report.md", []string{
		"The Self-Spawning Bug Bash Tree",
		"Three-Layer Substrate Stack",
		"Gemini Enterprise Agent Platform",
		"compute obtainability",
		"Issue #482",
		"Issue #511",
		"Issue #518",
	})

	requireSnippets("docs/blog/self-spawning-bug-bash-tree-on-google-ax.md", []string{
		"Why CI Passed and Production Still Returned HTTP 500",
		"From Single-Container Chat Loops to a Self-Spawning Bug Bash Tree",
		"The Three-Layer Substrate Stack on GKE",
		"Multi-Harness Red-Green TDD and the Zero-Secret Model Gateway",
		"Root Verification Gate and Sub-3-Second Selective Git Hot-Reload",
		"Live GKE Benchmarks and Two-Tier Evaluation",
		"Try It Yourself",
		"Three-Layer Substrate Stack",
		"SNAPSHOT_FIDELITY_VOLUMES",
		"gemini-3.8-flash",
		"Gemini Enterprise Agent Platform",
		"compute obtainability",
		"80ms",
		"42.6ms",
		"61ms",
		"docs/assets/ax-live-gke-terminal.gif",
	})

	requireSnippets("docs/tutorial/build-a-self-spawning-bug-bash-tree.md", []string{
		"Lab 1",
		"Lab 2",
		"Lab 3",
		"Lab 4",
		"Lab 5",
		"make run-local",
		"make eval",
		"runner.Run",
		"OnCommandExit",
		"SuspendTask",
		"SNAPSHOT_FIDELITY_VOLUMES",
		"Gemini Enterprise Agent Platform",
		"make deploy",
		"make eval-live-gke",
		"/tmp/ax-live-gke-eval-report.json",
	})

	gifRel := "docs/assets/ax-live-gke-terminal.gif"
	gifFull := filepath.Join(repoRoot, gifRel)
	gifBytes, gifErr := os.ReadFile(gifFull)
	if gifErr != nil {
		report.Violations = append(report.Violations, fmt.Sprintf("missing or unreadable %s: %v", gifRel, gifErr))
	} else {
		if len(gifBytes) <= 10*1024 {
			report.Violations = append(report.Violations, fmt.Sprintf("%s size = %d bytes, want > 10 KB", gifRel, len(gifBytes)))
		}
		if len(gifBytes) >= 5*1024*1024 {
			report.Violations = append(report.Violations, fmt.Sprintf("%s size = %d bytes, want < 5 MB", gifRel, len(gifBytes)))
		}
		if !bytes.HasPrefix(gifBytes, []byte("GIF89a")) && !bytes.HasPrefix(gifBytes, []byte("GIF87a")) {
			report.Violations = append(report.Violations, fmt.Sprintf("%s does not begin with GIF89a or GIF87a header", gifRel))
		}
	}

	report.Passed = len(report.Violations) == 0 && len(report.CheckedFiles) == len(filesToScan)
	return report, nil
}

var (
	baselineCacheMu sync.Mutex
	baselinePassed  bool
	baselineRan     bool
	baselineErr     error
)

func verifyBaselineServicesCIGreen(ctx context.Context, repoDir string) (bool, error) {
	baselineCacheMu.Lock()
	defer baselineCacheMu.Unlock()
	if baselineRan {
		return baselinePassed, baselineErr
	}

	scriptPath := filepath.Join(repoDir, "services", "run-service-tests.sh")
	cmd := exec.CommandContext(ctx, "bash", scriptPath, "--all")
	cmd.Dir = repoDir
	cmd.Env = httputil.EnsureGoBinOnPath(os.Environ())
	out, err := cmd.CombinedOutput()
	baselineRan = true
	if err != nil {
		baselinePassed = false
		baselineErr = fmt.Errorf("baseline service test suite failed: %w\n%s", err, string(out))
		return false, baselineErr
	}
	baselinePassed = true
	return true, nil
}

// RunScenarioTrajectory executes a single end-to-end Incident Scenario x Coding Harness trajectory
// across all 3 decoupled surfaces and grades all 6 architectural invariants.
func RunScenarioTrajectory(ctx context.Context, spec TrajectorySpec) (TrajectoryGrade, error) {
	incID := strings.ToUpper(strings.TrimSpace(spec.IncidentID))
	if incID == "" {
		incID = storefront.IncidentStackedPromoNegativeSubtotal
	}
	sc, ok := storefront.GetScenario(incID)
	if !ok {
		return TrajectoryGrade{}, fmt.Errorf("unknown incident_id %q", incID)
	}

	harnessMode := strings.ToLower(strings.TrimSpace(spec.HarnessMode))
	if harnessMode == "" {
		harnessMode = "multi"
	}
	requestedModel := strings.TrimSpace(spec.RequestedModel)
	if requestedModel == "" {
		requestedModel = gateway.DefaultFallbackCascade[0]
	}
	provMode := spec.ProvenanceMode
	if provMode == "" {
		provMode = gateway.ProvenanceFixture
	}

	grade := TrajectoryGrade{
		IncidentID:         sc.ID,
		HarnessMode:        harnessMode,
		ProvenanceMode:     provMode,
		RequestedModel:     requestedModel,
		ImplicatedServices: append([]string(nil), sc.ImplicatedServices...),
		UnaffectedServices: append([]string(nil), sc.UnaffectedServices...),
	}

	// 1. Initialize StorefrontRuntime (Surface 1) and verify Invariant 1: Baseline CI Green
	tmpGitDir, err := os.MkdirTemp("", "eval-storefront-*")
	if err != nil {
		return grade, err
	}
	defer os.RemoveAll(tmpGitDir)

	sf, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: tmpGitDir,
	})
	if err != nil {
		return grade, fmt.Errorf("creating storefront runtime: %w", err)
	}
	sfServer := httptest.NewServer(sf.Handler())
	defer sfServer.Close()

	ciGreen, ciErr := verifyBaselineServicesCIGreen(ctx, sf.GitManager().RepoDir())
	if !ciGreen || ciErr != nil {
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("baseline CI failed: %v", ciErr))
	} else {
		grade.BaselineCIGreen = true
	}

	// 2. Configure Zero-Secret Agent Platform Model Gateway (LIVE cascade vs FIXTURE mode)
	var (
		upstreamAgentPlatform *httptest.Server
		gw                    *gateway.Server
	)
	if provMode == gateway.ProvenanceLive {
		dropCount := spec.SimulatedCascadeDrop
		if dropCount < 0 {
			dropCount = 0
		}
		if dropCount >= len(gateway.DefaultFallbackCascade) {
			dropCount = len(gateway.DefaultFallbackCascade) - 1
		}
		failingModels := make(map[string]int)
		for i := 0; i < dropCount; i++ {
			status := http.StatusTooManyRequests
			if i%2 == 1 {
				status = http.StatusServiceUnavailable
			}
			failingModels[gateway.DefaultFallbackCascade[i]] = status
		}

		upstreamAgentPlatform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer live-workload-identity-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			for m, code := range failingModels {
				if strings.Contains(r.URL.Path, "/models/"+m+":") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"simulated upstream capacity exhaustion on %s"}}`, code, m)
					return
				}
			}
			reqBytes, _ := io.ReadAll(r.Body)
			synthText := "[LIVE:Gemini Enterprise Agent Platform] " + gateway.SynthesizeFixtureText("live-eval", string(reqBytes))
			respJSON, _ := json.Marshal(map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role":  "model",
							"parts": []map[string]any{{"text": synthText}},
						},
						"finishReason": "STOP",
					},
				},
				"usageMetadata": map[string]any{
					"promptTokenCount":     64,
					"candidatesTokenCount": 48,
					"totalTokenCount":      112,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respJSON)
		}))
		defer upstreamAgentPlatform.Close()

		gwCfg := gateway.DefaultConfig()
		gwCfg.AgentPlatformBaseURL = upstreamAgentPlatform.URL
		gwCfg.EnableFixtureFallback = false
		gw = gateway.NewServer(gwCfg, gateway.StaticTokenFunc("live-workload-identity-token"))
	} else {
		gwCfg := gateway.DefaultConfig()
		gwCfg.EnableFixtureFallback = true
		gw = gateway.NewServer(gwCfg, func(context.Context) (string, error) {
			return "", fmt.Errorf("offline CI fixture mode")
		})
	}

	gwServer := httptest.NewServer(gw.Handler())
	defer gwServer.Close()

	// 3. Initialize AX Observer & Monitoring Stack (Surfaces 2 & 3)
	obsSrv, err := observer.NewServer(ctx, observer.Config{
		Storefront:          sf,
		StorefrontURL:       sfServer.URL,
		StorefrontPublicURL: sfServer.URL,
		ModelGateway:        gw,
		ModelGatewayURL:     gwServer.URL,
	})
	if err != nil {
		return grade, fmt.Errorf("creating observer server: %w", err)
	}
	defer obsSrv.Close()

	obsHTTP := httptest.NewServer(obsSrv.Handler())
	defer obsHTTP.Close()

	// 4. Verify Invariant 2: Activating the scenario causes POST /api/cart/checkout to return HTTP 500,
	// increments Prometheus 500 metrics, and fires an enriched Alertmanager webhook with Loki stack traces.
	if _, err := sf.ActivateIncident(sc.ID); err != nil {
		return grade, fmt.Errorf("activating incident %s: %w", sc.ID, err)
	}

	cartBytes, _ := json.Marshal(sc.TriggerCart)
	preCheckoutResp, err := http.Post(sfServer.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		return grade, fmt.Errorf("pre-rollout checkout request failed: %w", err)
	}
	_ = preCheckoutResp.Body.Close()
	grade.PreRolloutCheckoutStatus = preCheckoutResp.StatusCode

	metricsSnap, scrapeErr := obsSrv.Scraper().ScrapeOnce(ctx)
	if scrapeErr != nil {
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("prometheus scrape failed: %v", scrapeErr))
	}

	// Trigger the incident remediation via POST /api/incidents/trigger on AX Observer
	triggerReq, _ := json.Marshal(observer.TriggerIncidentRequest{
		IncidentID: sc.ID,
		Harness:    harnessMode,
		Model:      requestedModel,
	})
	trigResp, err := http.Post(obsHTTP.URL+"/api/incidents/trigger", "application/json", bytes.NewReader(triggerReq))
	if err != nil {
		return grade, fmt.Errorf("POST /api/incidents/trigger failed: %w", err)
	}
	_ = trigResp.Body.Close()
	if trigResp.StatusCode != http.StatusOK {
		return grade, fmt.Errorf("POST /api/incidents/trigger returned status %d", trigResp.StatusCode)
	}

	// Fetch unified StateSnapshot from GET /api/state
	stateResp, err := http.Get(obsHTTP.URL + "/api/state")
	if err != nil {
		return grade, fmt.Errorf("GET /api/state failed: %w", err)
	}
	var snap observer.StateSnapshot
	if err := json.NewDecoder(stateResp.Body).Decode(&snap); err != nil {
		_ = stateResp.Body.Close()
		return grade, fmt.Errorf("decoding StateSnapshot: %w", err)
	}
	_ = stateResp.Body.Close()

	if grade.PreRolloutCheckoutStatus == http.StatusInternalServerError &&
		metricsSnap.TotalHTTP500 > 0 &&
		snap.LatestAlert != nil &&
		snap.LatestAlert.IncidentID == sc.ID &&
		len(snap.LatestAlert.LokiBreadcrumbs) >= len(sc.ImplicatedServices) &&
		len(snap.LatestAlert.StackTraces) >= len(sc.ImplicatedServices) {
		grade.AlertmanagerWebhookTriggered = true
	} else {
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf(
			"alertmanager webhook check failed: preStatus=%d total500=%d latestAlert=%+v",
			grade.PreRolloutCheckoutStatus, metricsSnap.TotalHTTP500, snap.LatestAlert,
		))
	}

	// 5. Verify Invariant 3: Selective Blast-Radius Fan-Out (only 2-3 implicated services, never all 5)
	if snap.LatestTree == nil {
		grade.FailureReasons = append(grade.FailureReasons, "LatestTree is nil")
		return grade, nil
	}
	tree := snap.LatestTree
	grade.ExecutionID = tree.ExecutionID
	grade.RootTaskName = tree.RootTask.TaskName
	grade.SpawnedChildCount = len(tree.ChildTasks)

	expectedImplicated := sc.ImplicatedServices
	selectiveOK := len(tree.ChildTasks) == len(expectedImplicated) &&
		len(tree.ChildTasks) >= 2 &&
		len(tree.ChildTasks) <= 3 &&
		len(tree.ChildTasks) < len(storefront.AllServices()) &&
		len(tree.UnaffectedServices) == len(storefront.AllServices())-len(expectedImplicated)

	childByService := make(map[string]observer.ObserverChildTaskView)
	for _, ch := range tree.ChildTasks {
		childByService[ch.Service] = ch
	}
	for _, expSvc := range expectedImplicated {
		ch, exists := childByService[expSvc]
		if !exists {
			selectiveOK = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("missing Child Task for implicated service %s", expSvc))
			continue
		}
		if ch.TaskConfig == nil ||
			ch.TaskConfig["service"] != expSvc ||
			ch.TaskConfig["incident_id"] != sc.ID ||
			ch.TaskConfig["role"] != "child-fixer" ||
			ch.TaskConfig["regression_test_name"] == "" ||
			ch.TaskConfig["failing_request"] == nil {
			selectiveOK = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("child %s missing structured Task.spec.config: %+v", ch.TaskName, ch.TaskConfig))
		}
	}
	for _, unaffSvc := range sc.UnaffectedServices {
		if _, spawned := childByService[unaffSvc]; spawned {
			selectiveOK = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("unaffected service %s was erroneously spawned", unaffSvc))
		}
	}
	grade.SelectiveBlastRadiusFanOut = selectiveOK

	// 6. Verify Invariants 4 & 5: Red-Green Regression-Test-First (TDD) & OnCommandExit Diff + SuspendTask Slot Reclamation
	volSnapByTask := make(map[string]observer.VolumeSnapshotRecord)
	for _, vs := range snap.VolumeSnapshots {
		volSnapByTask[vs.TaskName] = vs
	}

	allTDD := len(tree.ChildTasks) > 0
	allSuspend := len(tree.ChildTasks) > 0
	seenHarnesses := make(map[string]bool)

	for _, ch := range tree.ChildTasks {
		seenHarnesses[ch.Harness] = true
		var phases []string
		hasRed := false
		hasGreen := false
		redIdx := -1
		greenIdx := -1
		for idx, st := range ch.TDDSteps {
			phases = append(phases, st.Phase)
			if st.Phase == "REPRODUCE_RED" && st.Status == "PASSED" {
				hasRed = true
				redIdx = idx
			}
			if st.Phase == "PATCH_GREEN" && st.Status == "PASSED" {
				hasGreen = true
				greenIdx = idx
			}
		}

		redVerified := hasRed && ch.RedTestOutput != "" && strings.Contains(ch.RegressionTestDiff, ch.RegressionTestName)
		greenVerified := hasGreen && redIdx >= 0 && greenIdx > redIdx && ch.GreenTestOutput != ""
		diffCaptured := strings.TrimSpace(ch.RegressionTestDiff) != "" &&
			strings.TrimSpace(ch.CodeFixDiff) != "" &&
			strings.TrimSpace(ch.UnifiedGitDiff) != ""
		vs, hasVS := volSnapByTask[ch.TaskName]
		volSaved := hasVS && vs.SnapshotFidelity == "SNAPSHOT_FIDELITY_VOLUMES" && strings.Contains(vs.SnapshotPath, "/workspace/.ax-state/")
		suspendedOK := ch.Suspended && ch.Phase == "Suspended" && ch.SlotReclamationMs > 0

		if harnessMode != "multi" && ch.Harness != harnessMode {
			allTDD = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("child %s harness = %s, want %s", ch.TaskName, ch.Harness, harnessMode))
		}
		if string(provMode) != ch.SourceProvenance {
			allTDD = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("child %s provenance = %s, want %s", ch.TaskName, ch.SourceProvenance, provMode))
		}

		if !redVerified || !greenVerified {
			allTDD = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("child %s TDD check failed (red=%v green=%v)", ch.TaskName, redVerified, greenVerified))
		}
		if !diffCaptured || !volSaved || !suspendedOK {
			allSuspend = false
			grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("child %s suspend/diff check failed (diff=%v vol=%v suspended=%v)", ch.TaskName, diffCaptured, volSaved, suspendedOK))
		}

		grade.ActiveModel = ch.ActiveModel
		grade.ChildGrades = append(grade.ChildGrades, ChildTaskGrade{
			TaskName:             ch.TaskName,
			Service:              ch.Service,
			Language:             ch.Language,
			Harness:              ch.Harness,
			RequestedModel:       ch.RequestedModel,
			ActiveModel:          ch.ActiveModel,
			SourceProvenance:     ch.SourceProvenance,
			RegressionTestName:   ch.RegressionTestName,
			HasStructuredConfig:  ch.TaskConfig != nil,
			ReproduceRedVerified: redVerified,
			PatchGreenVerified:   greenVerified,
			DiffCaptured:         diffCaptured,
			VolumeStateSaved:     volSaved,
			SuspendedAndReclaim:  suspendedOK,
			SlotReclamationMs:    ch.SlotReclamationMs,
			TDDPhases:            phases,
		})
	}

	if harnessMode == "multi" && len(seenHarnesses) < 2 {
		allTDD = false
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("multi harness mode did not rotate harnesses across children: %v", seenHarnesses))
	}

	grade.TotalReclaimedWorkerSlots = snap.WorkerPool.TotalReclaimedSlots
	if snap.WorkerPool.TotalReclaimedSlots < 1+len(tree.ChildTasks) {
		allSuspend = false
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf("TotalReclaimedSlots = %d, want >= %d", snap.WorkerPool.TotalReclaimedSlots, 1+len(tree.ChildTasks)))
	}

	for _, call := range gw.RecentCalls() {
		if len(call.CascadeAttempts) > grade.CascadeAttemptsObserved {
			grade.CascadeAttemptsObserved = len(call.CascadeAttempts)
		}
	}

	grade.RedGreenRegressionTDD = allTDD
	grade.OnCommandExitAndSuspendReclaimed = allSuspend

	// 7. Verify Invariant 6: Pre-Warmed < 10s Live Rollout & Storefront Healing (HTTP 500 -> HTTP 200)
	postCheckoutResp, err := http.Post(sfServer.URL+"/api/cart/checkout", "application/json", bytes.NewReader(cartBytes))
	if err != nil {
		return grade, fmt.Errorf("post-rollout checkout request failed: %w", err)
	}
	_ = postCheckoutResp.Body.Close()
	grade.PostRolloutCheckoutStatus = postCheckoutResp.StatusCode

	if snap.LatestAlert != nil {
		grade.AlertStatusAfterRollout = snap.LatestAlert.Status
	}
	if snap.LatestRollout != nil {
		grade.RolloutDurationMs = snap.LatestRollout.TotalDurationMs
	}

	rolloutOK := snap.LatestRollout != nil &&
		snap.LatestRollout.TotalDurationMs > 0 &&
		snap.LatestRollout.TotalDurationMs < 10000 &&
		snap.LatestRollout.UnderTenSecondSLA &&
		snap.LatestRollout.StorefrontCheckoutVerified &&
		grade.AlertStatusAfterRollout == "resolved" &&
		grade.PostRolloutCheckoutStatus == http.StatusOK &&
		tree.MergedGitCommitSHA != "" &&
		tree.StorefrontHealed

	if !rolloutOK {
		grade.FailureReasons = append(grade.FailureReasons, fmt.Sprintf(
			"rollout/healing check failed: rollout=%+v alertStatus=%q postCheckout=%d commit=%q",
			snap.LatestRollout, grade.AlertStatusAfterRollout, grade.PostRolloutCheckoutStatus, tree.MergedGitCommitSHA,
		))
	}
	grade.PreWarmedRolloutAndStorefrontHealed = rolloutOK

	grade.Passed = grade.BaselineCIGreen &&
		grade.AlertmanagerWebhookTriggered &&
		grade.SelectiveBlastRadiusFanOut &&
		grade.RedGreenRegressionTDD &&
		grade.OnCommandExitAndSuspendReclaimed &&
		grade.PreWarmedRolloutAndStorefrontHealed

	return grade, nil
}
