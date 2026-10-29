package observer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// TopologyViewBox defines the coordinate space of the interactive /about SVG canvas.
type TopologyViewBox struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// TopologyPoint is an (X, Y) coordinate inside the TopologyViewBox.
type TopologyPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// TopologySwimlane groups topology nodes into horizontal architectural layers.
type TopologySwimlane struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Y        int    `json:"y"`
	Height   int    `json:"height"`
}

// TopologyNode represents one of the 12 interactive nodes in the 3-Surface Mesh & Agent Substrate topology.
type TopologyNode struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Sublabel        string   `json:"sublabel"`
	Swimlane        string   `json:"swimlane"`
	Category        string   `json:"category"`
	Language        string   `json:"language"`
	Port            string   `json:"port"`
	X               int      `json:"x"`
	Y               int      `json:"y"`
	Width           int      `json:"width"`
	Height          int      `json:"height"`
	Description     string   `json:"description"`
	KeyCapabilities []string `json:"key_capabilities"`
	CodePath        string   `json:"code_path"`
}

// TopologyEdge represents one of the 11 animated signal paths in the topology SVG.
type TopologyEdge struct {
	ID            string          `json:"id"`
	FromNode      string          `json:"from_node"`
	ToNode        string          `json:"to_node"`
	Protocol      string          `json:"protocol"`
	Label         string          `json:"label"`
	PulseToken    string          `json:"pulse_token"`
	PathWaypoints []TopologyPoint `json:"path_waypoints"`
}

// CatalogServiceEntry describes one of the 5 polyglot services in the Retail Storefront mesh.
type CatalogServiceEntry struct {
	Service        string `json:"service"`
	Language       string `json:"language"`
	Port           int    `json:"port"`
	SupervisorPort int    `json:"supervisor_port"`
	SourceFile     string `json:"source_file"`
	TestFile       string `json:"test_file"`
	UnitTestCmd    string `json:"unit_test_cmd"`
	RoleSummary    string `json:"role_summary"`
}

// CatalogScenarioBlastRadius maps an incident scenario (INC-101..INC-104) to its topology blast radius.
type CatalogScenarioBlastRadius struct {
	IncidentID         string   `json:"incident_id"`
	Title              string   `json:"title"`
	ErrorCode          string   `json:"error_code"`
	Summary            string   `json:"summary"`
	WhyCIMissedIt      string   `json:"why_ci_missed_it"`
	ImplicatedServices []string `json:"implicated_services"`
	UnaffectedServices []string `json:"unaffected_services"`
	ActiveNodeIDs      []string `json:"active_node_ids"`
	ActiveEdgeIDs      []string `json:"active_edge_ids"`
	RecommendedHarness string   `json:"recommended_harness"`
}

// CatalogHarnessEntry describes one of the 3 CLI coding harnesses pre-baked in ax-bug-bash-runner.
type CatalogHarnessEntry struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	BinaryPath      string `json:"binary_path"`
	ConfigPath      string `json:"config_path"`
	CommandTemplate string `json:"command_template"`
	WireProtocol    string `json:"wire_protocol"`
	AuthHeader      string `json:"auth_header"`
	StrengthSummary string `json:"strength_summary"`
}

// CatalogCascadeModelEntry describes one tier in the 4-model Gemini Flash fallback cascade.
type CatalogCascadeModelEntry struct {
	Tier            int    `json:"tier"`
	ModelID         string `json:"model_id"`
	Role            string `json:"role"`
	FallbackTrigger string `json:"fallback_trigger"`
	LatencyTargetMs int    `json:"latency_target_ms"`
}

// LifecycleSlotState models the state of the 4 warm gVisor sandbox worker slots at a lifecycle stage.
type LifecycleSlotState struct {
	SlotID   string `json:"slot_id"`
	State    string `json:"state"` // "WARM_IDLE", "ACTIVE_SANDBOX", "RECLAIMED_WARM"
	TaskRole string `json:"task_role"`
	Harness  string `json:"harness"`
}

// CatalogLifecycleStage represents one of the 6 interactive steps in Tab 2 (Recursive Task-Tree & TDD Lifecycle).
type CatalogLifecycleStage struct {
	StepNumber             int                  `json:"step_number"`
	ID                     string               `json:"id"`
	Title                  string               `json:"title"`
	Subtitle               string               `json:"subtitle"`
	Summary                string               `json:"summary"`
	HighlightedNodes       []string             `json:"highlighted_nodes"`
	ActiveEdges            []string             `json:"active_edges"`
	RootTaskPhase          string               `json:"root_task_phase"`
	ChildTaskPhase         string               `json:"child_task_phase"`
	SlotStates             []LifecycleSlotState `json:"slot_states"`
	ProtocolLabel          string               `json:"protocol_label"`
	ProtocolPayloadPreview string               `json:"protocol_payload_preview"`
}

// CatalogADRCard represents one of the 11 Architecture Decision Records with live runtime telemetry counters.
type CatalogADRCard struct {
	ID              string            `json:"id"`
	Number          int               `json:"number"`
	Filename        string            `json:"filename"`
	Title           string            `json:"title"`
	Status          string            `json:"status"`
	Category        string            `json:"category"`
	Summary         string            `json:"summary"`
	KeyDecisions    []string          `json:"key_decisions"`
	RelatedNodes    []string          `json:"related_nodes"`
	RuntimeCounters map[string]string `json:"runtime_counters"`
}

// CatalogRuntimeTelemetry holds live stage-safe counters injected into GET /api/about/catalog.
type CatalogRuntimeTelemetry struct {
	ActiveExecutionMode   string `json:"active_execution_mode"`
	DefaultHarness        string `json:"default_harness"`
	LiveWorkerSlots       int    `json:"live_worker_slots"`
	WarmIdleSlots         int    `json:"warm_idle_slots"`
	ActiveSandboxSlots    int    `json:"active_sandbox_slots"`
	ReclaimedWarmSlots    int    `json:"reclaimed_warm_slots"`
	GatewaySessionsCount  int    `json:"gateway_sessions_count"`
	ActiveCascadeModel    string `json:"active_cascade_model"`
	SourceProvenance      string `json:"source_provenance"`
	StorefrontBaselineTag string `json:"storefront_baseline_tag"`
	ActiveTreesCount      int    `json:"active_trees_count"`
}

// ArchitectureTopologyCatalog is the typed source of truth served at GET /api/about/catalog.
type ArchitectureTopologyCatalog struct {
	ViewBox         TopologyViewBox              `json:"view_box"`
	StatusStrip     CatalogRuntimeTelemetry      `json:"status_strip"`
	Swimlanes       []TopologySwimlane           `json:"swimlanes"`
	Nodes           []TopologyNode               `json:"nodes"`
	Edges           []TopologyEdge               `json:"edges"`
	Services        []CatalogServiceEntry        `json:"services"`
	Scenarios       []CatalogScenarioBlastRadius `json:"scenarios"`
	Harnesses       []CatalogHarnessEntry        `json:"harnesses"`
	CascadeModels   []CatalogCascadeModelEntry   `json:"cascade_models"`
	LifecycleStages []CatalogLifecycleStage      `json:"lifecycle_stages"`
	ADRs            []CatalogADRCard             `json:"adrs"`
}

var expectedADRFileList = []string{
	"0001-three-decoupled-surfaces-and-subtle-production-incidents.md",
	"0002-in-cluster-prometheus-alertmanager-loki-grafana-with-gcp-export.md",
	"0003-polyglot-five-service-storefront-and-incident-catalog.md",
	"0004-telemetry-driven-triage-and-tdd-child-task-lifecycle.md",
	"0005-clean-modern-ax-observer-ui-and-routing.md",
	"0006-agent-substrate-v060-and-ax-deployment-hardening.md",
	"0007-multi-harness-sandbox-and-gemini-flash-fallback-cascade.md",
	"0008-dual-mode-execution-and-zero-trust-sandbox-coordination.md",
	"0009-polyglot-http-mesh-and-zero-downtime-git-hot-reload.md",
	"0010-interactive-about-architecture-catalog-and-drift-verification.md",
	"0011-two-tier-evaluation-chaos-soak-and-gcp-dual-export-verification.md",
	"0012-bom20-blog-tutorial-and-stage-walkthrough-contract.md",
	"0013-upstream-google-ax-v060-compatibility-overlay.md",
	"0014-bom19-three-layer-architecture-guide-and-stage-warmup-gate.md",
}

// ExpectedADRFilenames returns the canonical ADR filenames in docs/adr/.
func ExpectedADRFilenames() []string {
	return append([]string(nil), expectedADRFileList...)
}

var stageUnsafePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)gs://[a-z0-9._/-]+`),
	regexp.MustCompile(`(?i)gke_[a-z0-9_-]+`),
	regexp.MustCompile(`(?i)gke-[a-z0-9-]+-(?:pool|node|default)[a-z0-9_-]*`),
	regexp.MustCompile(`\b\d{12}\b`),
	regexp.MustCompile(`(?i)projects/[a-z][a-z0-9-]{4,29}`),
}

// ScrubStageSafeString removes any cluster, project, node hostname, or bucket URI tokens from stage-facing strings.
func ScrubStageSafeString(s string) string {
	out := s
	for _, re := range stageUnsafePatterns {
		out = re.ReplaceAllString(out, "[redacted-stage-safe]")
	}
	for _, envKey := range []string{"PROJECT_ID", "GOOGLE_CLOUD_PROJECT", "KUBE_CONTEXT", "CLUSTER_NAME", "AX_SNAPSHOTS_BUCKET"} {
		if val := strings.TrimSpace(os.Getenv(envKey)); len(val) >= 3 {
			re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(val))
			if err == nil {
				out = re.ReplaceAllString(out, "[redacted-stage-safe]")
			}
		}
	}
	return out
}

// BuildArchitectureTopologyCatalog constructs the complete 3-tab /about catalog enriched with live runtime counters.
func BuildArchitectureTopologyCatalog(rt CatalogRuntimeTelemetry) ArchitectureTopologyCatalog {
	if strings.TrimSpace(rt.ActiveExecutionMode) == "" {
		rt.ActiveExecutionMode = "SIMULATED"
	}
	if strings.TrimSpace(rt.DefaultHarness) == "" {
		rt.DefaultHarness = "multi"
	}
	if rt.LiveWorkerSlots <= 0 {
		rt.LiveWorkerSlots = 4
	}
	if strings.TrimSpace(rt.ActiveCascadeModel) == "" {
		rt.ActiveCascadeModel = gateway.DefaultFallbackCascade[0]
	}
	if strings.TrimSpace(rt.SourceProvenance) == "" {
		rt.SourceProvenance = string(gateway.ProvenanceFixture)
	}
	if strings.TrimSpace(rt.StorefrontBaselineTag) == "" {
		rt.StorefrontBaselineTag = storefront.BaselineGitTag
	}
	rt.ActiveExecutionMode = ScrubStageSafeString(rt.ActiveExecutionMode)
	rt.StorefrontBaselineTag = ScrubStageSafeString(rt.StorefrontBaselineTag)

	swimlanes := []TopologySwimlane{
		{
			ID:       "swimlane-storefront",
			Title:    "Polyglot Storefront & GitOps",
			Subtitle: "Surface 1 (:3001) — 5 real microservices + Git Smart-HTTP origin with selective hot-reload",
			Y:        20,
			Height:   135,
		},
		{
			ID:       "swimlane-observability",
			Title:    "Observability & Dual Export",
			Subtitle: "Surface 2 (:3002) — Prometheus, Alertmanager v4, Loki LogQL, Grafana & Google Cloud Dual Export",
			Y:        170,
			Height:   135,
		},
		{
			ID:       "swimlane-control-plane",
			Title:    "AX Control Plane & Model Gateway",
			Subtitle: "Surface 3 (:3000) — AX Observer, google/ax gRPC server + OpenFGA RBAC, Zero-Secret Model Gateway",
			Y:        320,
			Height:   135,
		},
		{
			ID:       "swimlane-substrate",
			Title:    "Agent Substrate Sandboxes",
			Subtitle: "Agent Substrate v0.6.0 — 4 warm gVisor worker slots, 3 CLI harnesses & Cloud Storage volume snapshots",
			Y:        470,
			Height:   130,
		},
	}

	nodes := []TopologyNode{
		{
			ID:          "storefront-web",
			Label:       "storefront-web",
			Sublabel:    "Node.js / TypeScript (:3001)",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "service",
			Language:    "Node.js / TypeScript",
			Port:        ":3001 / sup :13001",
			X:           40,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "Edge storefront gateway normalizing cart payloads, promo arrays, and shopper checkout sessions.",
			KeyCapabilities: []string{
				"Normalizes cart coupons, quantities, and currency headers",
				"Forwards validated orders to checkout-service (:8081)",
				"Runs under per-service hot-reload supervisor (:13001)",
			},
			CodePath: "services/storefront-web/src/server.ts",
		},
		{
			ID:          "checkout-service",
			Label:       "checkout-service",
			Sublabel:    "Go 1.24 (:8081)",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "service",
			Language:    "Go",
			Port:        ":8081 / sup :18081",
			X:           230,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "Checkout orchestrator coordinating pricing quotes, SKU reservation locks, and shipping rate tiers.",
			KeyCapabilities: []string{
				"Coordinates pricing-service, inventory-service, and shipping-service",
				"Enforces integer cent arithmetic and reservation rollback",
				"Hot-reloads via loopback supervisor (:18081)",
			},
			CodePath: "services/checkout-service/checkout.go",
		},
		{
			ID:          "pricing-service",
			Label:       "pricing-service",
			Sublabel:    "Python / FastAPI (:8083)",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "service",
			Language:    "Python / FastAPI",
			Port:        ":8083 / sup :18083",
			X:           420,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "Promotional discount and tax engine computing subtotal floors and regional tax basis points.",
			KeyCapabilities: []string{
				"Evaluates stacked percentage + fixed-dollar promo codes",
				"Clamps promotional discounts to zero subtotal floor",
				"Executes Python unittest suite inside gVisor sandboxes",
			},
			CodePath: "services/pricing-service/pricing.py",
		},
		{
			ID:          "inventory-service",
			Label:       "inventory-service",
			Sublabel:    "Rust / Axum (:8084)",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "service",
			Language:    "Rust / Axum",
			Port:        ":8084 / sup :18084",
			X:           610,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "High-concurrency SKU stock ledger and bundle reservation engine written in Rust.",
			KeyCapabilities: []string{
				"Validates composite bundle component stock invariants",
				"Prevents unsigned integer underflow on multi-item holds",
				"Verifies fixes via cargo test in airgapped sandboxes",
			},
			CodePath: "services/inventory-service/src/lib.rs",
		},
		{
			ID:          "shipping-service",
			Label:       "shipping-service",
			Sublabel:    "Go 1.24 (:8082)",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "service",
			Language:    "Go",
			Port:        ":8082 / sup :18082",
			X:           800,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "Carrier rate, free-shipping threshold, and lithium-ion hazmat weight boundary calculator.",
			KeyCapabilities: []string{
				"Computes weight-tier surcharges and hazmat handling fees",
				"Uses inclusive <= boundary checks for weight brackets",
				"Supports zero-decimal JPY currency minor units",
			},
			CodePath: "services/shipping-service/shipping.go",
		},
		{
			ID:          "storefront-git-origin",
			Label:       "storefront-git-origin",
			Sublabel:    "Git Smart-HTTP + Hot-Reload",
			Swimlane:    "Polyglot Storefront & GitOps",
			Category:    "gitops",
			Language:    "Git / Go Supervisor",
			Port:        ":3001/git/storefront.git",
			X:           990,
			Y:           55,
			Width:       170,
			Height:      82,
			Description: "In-cluster Git origin and selective hot-reload coordinator restarting only modified microservices in < 10s.",
			KeyCapabilities: []string{
				"Serves /git/storefront.git to root and child sandboxes",
				"Diffs baseline-v1..HEAD to reload only changed services",
				"Leaves unaffected services running with zero connection drops",
			},
			CodePath: "internal/storefront/git_repo.go",
		},
		{
			ID:          "prometheus-alertmanager",
			Label:       "prometheus-alertmanager",
			Sublabel:    "Prometheus + Alertmanager v4 + GMP",
			Swimlane:    "Observability & Dual Export",
			Category:    "observability",
			Language:    "Prometheus / Go",
			Port:        ":9090 / :9093",
			X:           80,
			Y:           205,
			Width:       320,
			Height:      82,
			Description: "Scrapes per-service HTTP 200/500 counters every 5s, fires Alertmanager v4 webhooks, and dual-exports to Google Cloud Managed Service for Prometheus.",
			KeyCapabilities: []string{
				"5s scrape interval across all 5 polyglot services",
				"Fires StorefrontHTTP500Spike webhook with fingerprint deduplication",
				"Dual-exports time-series to Google Cloud Managed Prometheus",
			},
			CodePath: "internal/monitoring/alertmanager.go",
		},
		{
			ID:          "loki-grafana",
			Label:       "loki-grafana",
			Sublabel:    "Loki LogQL + Grafana (:3002)",
			Swimlane:    "Observability & Dual Export",
			Category:    "observability",
			Language:    "Loki / Grafana",
			Port:        ":3100 / :3002",
			X:           460,
			Y:           205,
			Width:       320,
			Height:      82,
			Description: "Indexes structured per-service stack traces for LogQL triage queries and dual-exports structured logs to Google Cloud Logging.",
			KeyCapabilities: []string{
				"Serves LogQL query_range stack traces to Root Triage Agent",
				"Hosts live Storefront Golden Signals dashboard (:3002)",
				"Mirrors structured incident logs to Google Cloud Logging",
			},
			CodePath: "internal/monitoring/scraper.go",
		},
		{
			ID:          "ax-observer",
			Label:       "ax-observer",
			Sublabel:    "Live Control Room (:3000)",
			Swimlane:    "AX Control Plane & Model Gateway",
			Category:    "control-plane",
			Language:    "Go 1.24 + Vanilla ES",
			Port:        ":3000",
			X:           60,
			Y:           355,
			Width:       330,
			Height:      82,
			Description: "Path-routed Control Room (/, /workers, /about) receiving Alertmanager webhooks, streaming SSE updates, and tracking gVisor sandbox slots.",
			KeyCapabilities: []string{
				"Async 202 Alertmanager ingestion + ?wait=true sync eval mode",
				"Ingests live sandbox callbacks at POST /api/observer/events",
				"Streams real-time SSE task-tree and worker pool transitions",
			},
			CodePath: "internal/observer/server.go",
		},
		{
			ID:          "ax-server-openfga",
			Label:       "ax-server + openfga",
			Sublabel:    "google/ax gRPC + OpenFGA RBAC (:8080)",
			Swimlane:    "AX Control Plane & Model Gateway",
			Category:    "control-plane",
			Language:    "Go / gRPC / OpenFGA",
			Port:        ":8080",
			X:           435,
			Y:           355,
			Width:       330,
			Height:      82,
			Description: "google/ax control plane scheduling recursive Root and Child Tasks onto Agent Substrate v0.6.0 with OpenFGA owner/viewer relationship tuples.",
			KeyCapabilities: []string{
				"Schedules Root Triage and parallel Child Fixer Tasks",
				"Enforces OpenFGA owner vs viewer authorization tuples",
				"Executes SuspendTask(SNAPSHOT_FIDELITY_VOLUMES) in < 50ms",
			},
			CodePath: "internal/bugbash/orchestrator.go",
		},
		{
			ID:          "model-gateway",
			Label:       "model-gateway",
			Sublabel:    "Zero-Secret Agent Platform Proxy (:8090)",
			Swimlane:    "AX Control Plane & Model Gateway",
			Category:    "security",
			Language:    "Go 1.24 / Workload Identity",
			Port:        ":8090",
			X:           810,
			Y:           355,
			Width:       330,
			Height:      82,
			Description: "Zero-secret proxy holding GKE Workload Identity credentials and executing the 4-tier Gemini Flash fallback cascade for compute obtainability.",
			KeyCapabilities: []string{
				"Validates scoped ephemeral TaskSessionToken from sandboxes",
				"Translates google-genai and openai-compat wire protocols",
				"Cascades across 4 Gemini Flash tiers on HTTP 429/503",
			},
			CodePath: "internal/gateway/gateway.go",
		},
		{
			ID:          "ateworker-gvisor-pool",
			Label:       "ateworker-gvisor-pool (4 Warm Sandbox Slots)",
			Sublabel:    "Agent Substrate v0.6.0 — Antigravity, OpenCode & Pi CLI Harnesses",
			Swimlane:    "Agent Substrate Sandboxes",
			Category:    "substrate",
			Language:    "gVisor / Polyglot Toolchains",
			Port:        "4 Warm Slots (10.244.1.11..14)",
			X:           60,
			Y:           505,
			Width:       1080,
			Height:      82,
			Description: "Pre-warmed gVisor sandbox pool running Root Triage and parallel Child Fixer agents with zero cloud credentials and instant volume snapshotting.",
			KeyCapabilities: []string{
				"Pre-baked Antigravity, OpenCode, and Pi CLI coding harnesses",
				"Red-Green-Verify TDD loop with real Go, Python, Rust, and Node compilers",
				"Snapshots /workspace volume to Cloud Storage and reclaims warm slot",
			},
			CodePath: "internal/bugbash/live_sandbox.go",
		},
	}

	edges := []TopologyEdge{
		{
			ID:         "edge-storefront-checkout",
			FromNode:   "storefront-web",
			ToNode:     "checkout-service",
			Protocol:   "HTTP/1.1 POST /checkout",
			Label:      "Shopper Cart Payload",
			PulseToken: "POST /checkout",
			PathWaypoints: []TopologyPoint{
				{X: 210, Y: 96},
				{X: 230, Y: 96},
			},
		},
		{
			ID:         "edge-checkout-pricing",
			FromNode:   "checkout-service",
			ToNode:     "pricing-service",
			Protocol:   "HTTP/1.1 POST /quote",
			Label:      "Promo & Tax Quote",
			PulseToken: "POST /quote",
			PathWaypoints: []TopologyPoint{
				{X: 400, Y: 96},
				{X: 420, Y: 96},
			},
		},
		{
			ID:         "edge-checkout-inventory",
			FromNode:   "checkout-service",
			ToNode:     "inventory-service",
			Protocol:   "HTTP/1.1 POST /reserve",
			Label:      "SKU Bundle Lock",
			PulseToken: "POST /reserve",
			PathWaypoints: []TopologyPoint{
				{X: 315, Y: 137},
				{X: 505, Y: 150},
				{X: 695, Y: 137},
			},
		},
		{
			ID:         "edge-checkout-shipping",
			FromNode:   "checkout-service",
			ToNode:     "shipping-service",
			Protocol:   "HTTP/1.1 POST /rate",
			Label:      "Weight & Currency Tier",
			PulseToken: "POST /rate",
			PathWaypoints: []TopologyPoint{
				{X: 315, Y: 55},
				{X: 600, Y: 38},
				{X: 885, Y: 55},
			},
		},
		{
			ID:         "edge-storefront-prometheus",
			FromNode:   "checkout-service",
			ToNode:     "prometheus-alertmanager",
			Protocol:   "OpenMetrics /metrics (5s)",
			Label:      "HTTP 500 Spike Counter",
			PulseToken: "HTTP_500_SPIKE",
			PathWaypoints: []TopologyPoint{
				{X: 315, Y: 137},
				{X: 240, Y: 205},
			},
		},
		{
			ID:         "edge-storefront-loki",
			FromNode:   "pricing-service",
			ToNode:     "loki-grafana",
			Protocol:   "Structured JSON Logs",
			Label:      "Stack Trace & Request Context",
			PulseToken: "LOGQL_TRACE",
			PathWaypoints: []TopologyPoint{
				{X: 505, Y: 137},
				{X: 620, Y: 205},
			},
		},
		{
			ID:         "edge-alertmanager-observer",
			FromNode:   "prometheus-alertmanager",
			ToNode:     "ax-observer",
			Protocol:   "POST /api/webhooks/alertmanager",
			Label:      "Alertmanager v4 Firing Webhook",
			PulseToken: "WEBHOOK_FIRING",
			PathWaypoints: []TopologyPoint{
				{X: 240, Y: 287},
				{X: 225, Y: 355},
			},
		},
		{
			ID:         "edge-loki-observer",
			FromNode:   "loki-grafana",
			ToNode:     "ax-observer",
			Protocol:   "GET /loki/api/v1/query_range",
			Label:      "LogQL Stack Trace Enrichment",
			PulseToken: "LOGQL_ENRICH",
			PathWaypoints: []TopologyPoint{
				{X: 620, Y: 287},
				{X: 300, Y: 355},
			},
		},
		{
			ID:         "edge-observer-axserver",
			FromNode:   "ax-observer",
			ToNode:     "ax-server-openfga",
			Protocol:   "gRPC v1alpha1 CreateTask",
			Label:      "Root & Child Task Orchestration",
			PulseToken: "gRPC CreateTask",
			PathWaypoints: []TopologyPoint{
				{X: 390, Y: 396},
				{X: 435, Y: 396},
			},
		},
		{
			ID:         "edge-axserver-sandbox",
			FromNode:   "ax-server-openfga",
			ToNode:     "ateworker-gvisor-pool",
			Protocol:   "Agent Substrate v0.6.0 Lease",
			Label:      "Warm gVisor Slot Allocation & SuspendTask",
			PulseToken: "SUBSTRATE_LEASE",
			PathWaypoints: []TopologyPoint{
				{X: 600, Y: 437},
				{X: 600, Y: 505},
			},
		},
		{
			ID:         "edge-sandbox-gateway-git",
			FromNode:   "ateworker-gvisor-pool",
			ToNode:     "model-gateway",
			Protocol:   "TaskSessionToken + Git Push",
			Label:      "Zero-Secret Gemini Cascade & Hot-Reload",
			PulseToken: "BEARER_TASK_TOKEN",
			PathWaypoints: []TopologyPoint{
				{X: 975, Y: 505},
				{X: 975, Y: 437},
			},
		},
	}

	services := []CatalogServiceEntry{
		{
			Service:        "storefront-web",
			Language:       "Node.js / TypeScript",
			Port:           3001,
			SupervisorPort: 13001,
			SourceFile:     "services/storefront-web/src/server.ts",
			TestFile:       "services/storefront-web/test/storefront.test.mjs",
			UnitTestCmd:    "npm test",
			RoleSummary:    "Shopper HTTP edge gateway normalizing cart items, coupon arrays, and currency headers.",
		},
		{
			Service:        "checkout-service",
			Language:       "Go",
			Port:           8081,
			SupervisorPort: 18081,
			SourceFile:     "services/checkout-service/checkout.go",
			TestFile:       "services/checkout-service/checkout_test.go",
			UnitTestCmd:    "go test -v ./...",
			RoleSummary:    "Order orchestrator coordinating pricing quotes, inventory locks, and shipping calculations.",
		},
		{
			Service:        "pricing-service",
			Language:       "Python / FastAPI",
			Port:           8083,
			SupervisorPort: 18083,
			SourceFile:     "services/pricing-service/pricing.py",
			TestFile:       "services/pricing-service/test_pricing.py",
			UnitTestCmd:    "python3 -m unittest discover -s . -p \"test_*.py\" -v",
			RoleSummary:    "Promotional discount and regional tax engine enforcing zero-floor subtotal invariants.",
		},
		{
			Service:        "inventory-service",
			Language:       "Rust / Axum",
			Port:           8084,
			SupervisorPort: 18084,
			SourceFile:     "services/inventory-service/src/lib.rs",
			TestFile:       "services/inventory-service/src/lib.rs",
			UnitTestCmd:    "cargo test",
			RoleSummary:    "Stock reservation and composite bundle multiplier engine written in Rust.",
		},
		{
			Service:        "shipping-service",
			Language:       "Go",
			Port:           8082,
			SupervisorPort: 18082,
			SourceFile:     "services/shipping-service/shipping.go",
			TestFile:       "services/shipping-service/shipping_test.go",
			UnitTestCmd:    "go test -v ./...",
			RoleSummary:    "Carrier shipping rate, free-shipping threshold, and lithium-ion hazmat boundary engine.",
		},
	}

	var scenarios []CatalogScenarioBlastRadius
	harnessRot := []string{"multi", "antigravity", "opencode", "pi"}
	for idx, sc := range storefront.ScenarioCatalog() {
		activeNodes := []string{
			"prometheus-alertmanager",
			"loki-grafana",
			"ax-observer",
			"ax-server-openfga",
			"model-gateway",
			"ateworker-gvisor-pool",
			"storefront-git-origin",
		}
		activeNodes = append(activeNodes, sc.ImplicatedServices...)

		activeEdges := []string{
			"edge-storefront-prometheus",
			"edge-storefront-loki",
			"edge-alertmanager-observer",
			"edge-loki-observer",
			"edge-observer-axserver",
			"edge-axserver-sandbox",
			"edge-sandbox-gateway-git",
		}
		for _, svc := range sc.ImplicatedServices {
			switch svc {
			case "storefront-web", "checkout-service":
				activeEdges = append(activeEdges, "edge-storefront-checkout")
			case "pricing-service":
				activeEdges = append(activeEdges, "edge-checkout-pricing")
			case "inventory-service":
				activeEdges = append(activeEdges, "edge-checkout-inventory")
			case "shipping-service":
				activeEdges = append(activeEdges, "edge-checkout-shipping")
			}
		}

		scenarios = append(scenarios, CatalogScenarioBlastRadius{
			IncidentID:         sc.ID,
			Title:              sc.Title,
			ErrorCode:          sc.ErrorCode,
			Summary:            sc.Summary,
			WhyCIMissedIt:      sc.WhyCIMissedIt,
			ImplicatedServices: append([]string(nil), sc.ImplicatedServices...),
			UnaffectedServices: append([]string(nil), sc.UnaffectedServices...),
			ActiveNodeIDs:      activeNodes,
			ActiveEdgeIDs:      activeEdges,
			RecommendedHarness: harnessRot[idx%len(harnessRot)],
		})
	}

	harnesses := []CatalogHarnessEntry{
		{
			ID:              "antigravity",
			DisplayName:     "Antigravity CLI",
			BinaryPath:      "harnesses/bin/antigravity",
			ConfigPath:      "harnesses/config/antigravity.json",
			CommandTemplate: "antigravity --print --endpoint $MODEL_GATEWAY_URL/v1beta --model $MODEL --workspace /workspace/$SERVICE",
			WireProtocol:    "google-genai",
			AuthHeader:      "x-goog-api-key: agw_task_<ephemeral>",
			StrengthSummary: "Native Google GenAI structured tool-use harness optimized for Gemini Flash multi-file reasoning.",
		},
		{
			ID:              "opencode",
			DisplayName:     "OpenCode CLI",
			BinaryPath:      "harnesses/bin/opencode-harness",
			ConfigPath:      "harnesses/config/opencode.json",
			CommandTemplate: "opencode-harness run --api-base $MODEL_GATEWAY_URL/v1 --model $MODEL --cwd /workspace/$SERVICE",
			WireProtocol:    "openai-compat",
			AuthHeader:      "Authorization: Bearer agw_task_<ephemeral>",
			StrengthSummary: "OpenAI-compatible autonomous coding harness with fast unified diff synthesis and test-runner hooks.",
		},
		{
			ID:              "pi",
			DisplayName:     "Pi Coding Agent CLI",
			BinaryPath:      "harnesses/bin/pi-harness",
			ConfigPath:      "harnesses/config/pi.json",
			CommandTemplate: "pi-harness --print --no-session --base-url $MODEL_GATEWAY_URL/v1 --model $MODEL --dir /workspace/$SERVICE",
			WireProtocol:    "openai-compat",
			AuthHeader:      "Authorization: Bearer agw_task_<ephemeral>",
			StrengthSummary: "Minimalist surgical patch agent focused on minimal-diff boundary fixes and rapid unit test verification.",
		},
	}

	cascadeRoles := []struct {
		role    string
		trigger string
		latency int
	}{
		{
			role:    "Primary Frontier Flash Tier — default model for Root Task and Child Tasks",
			trigger: "Initial attempt (0ms backoff)",
			latency: 420,
		},
		{
			role:    "Secondary High-Throughput Flash Tier — first automatic failover on quota exhaustion",
			trigger: "HTTP 429 TooManyRequests or 503 ResourceExhausted on Tier 1",
			latency: 380,
		},
		{
			role:    "Tertiary Compute Obtainability Flash Tier — regional capacity overflow tier",
			trigger: "HTTP 429 / 503 or upstream deadline exceeded on Tier 2",
			latency: 340,
		},
		{
			role:    "Quaternary Ultra-Low-Latency Flash Tier — guaranteed completion floor",
			trigger: "HTTP 429 / 503 on Tier 3 (ensures zero dropped bug bash tasks)",
			latency: 290,
		},
	}
	var cascadeModels []CatalogCascadeModelEntry
	for i, modelID := range gateway.DefaultFallbackCascade {
		meta := cascadeRoles[i%len(cascadeRoles)]
		cascadeModels = append(cascadeModels, CatalogCascadeModelEntry{
			Tier:            i + 1,
			ModelID:         modelID,
			Role:            meta.role,
			FallbackTrigger: meta.trigger,
			LatencyTargetMs: meta.latency,
		})
	}

	lifecycleStages := []CatalogLifecycleStage{
		{
			StepNumber: 1,
			ID:         "stage-1-alert-enrichment",
			Title:      "1. Alert & Loki LogQL Enrichment",
			Subtitle:   "Prometheus detects HTTP 500 spike -> Alertmanager v4 webhook -> Loki stack trace query",
			Summary:    "A subtle multi-service checkout bug triggers HTTP 500s on the polyglot Storefront. Prometheus fires StorefrontHTTP500Spike via Alertmanager v4 to ax-observer, which immediately enriches the alert with Loki LogQL stack traces and returns 202 Accepted.",
			HighlightedNodes: []string{
				"checkout-service",
				"pricing-service",
				"prometheus-alertmanager",
				"loki-grafana",
				"ax-observer",
			},
			ActiveEdges: []string{
				"edge-storefront-prometheus",
				"edge-storefront-loki",
				"edge-alertmanager-observer",
				"edge-loki-observer",
			},
			RootTaskPhase:  "Pending",
			ChildTaskPhase: "NotCreated",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
				{SlotID: "ateworker-gvisor-02", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
				{SlotID: "ateworker-gvisor-03", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "POST /api/webhooks/alertmanager + LogQL query_range",
			ProtocolPayloadPreview: `{"version":"4","status":"firing","commonLabels":{"alertname":"StorefrontHTTP500Spike","incident":"INC-101","services":"pricing-service,shipping-service"},"loki_query":"{app=~\"pricing-service|shipping-service\"} |= \"ERROR\""}`,
		},
		{
			StepNumber: 2,
			ID:         "stage-2-root-triage-creation",
			Title:      "2. Root Task Creation on ax-server",
			Subtitle:   "ax-observer dispatches CreateTask(root-bug-bash) -> OpenFGA owner tuple -> Slot 01 leased",
			Summary:    "ax-observer calls google/ax gRPC CreateTask to spawn the Root Task on warm sandbox slot ateworker-gvisor-01. The Root Task analyzes Loki stack traces and pinpoints the exact implicated services while marking healthy services unaffected.",
			HighlightedNodes: []string{
				"ax-observer",
				"ax-server-openfga",
				"ateworker-gvisor-pool",
			},
			ActiveEdges: []string{
				"edge-observer-axserver",
				"edge-axserver-sandbox",
			},
			RootTaskPhase:  "Running",
			ChildTaskPhase: "Pending",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "ACTIVE_SANDBOX", TaskRole: "Root Task (Incident Triage)", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-02", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
				{SlotID: "ateworker-gvisor-03", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "gRPC ax.v1alpha1.TaskService/CreateTask (root-triage)",
			ProtocolPayloadPreview: `{"atespace":"default","task":{"metadata":{"name":"root-inc-101"},"spec":{"config":{"role":"root-triage","incident_id":"INC-101","implicated_services":["pricing-service","shipping-service"],"unaffected_services":["storefront-web","checkout-service","inventory-service"]}}}}`,
		},
		{
			StepNumber: 3,
			ID:         "stage-3-parallel-child-fanout",
			Title:      "3. Parallel Child Tasks Fan-Out",
			Subtitle:   "Root Task spawns 1 isolated Child Task per implicated service with parent_task lineage",
			Summary:    "The Root Task calls ax-server from inside its sandbox to fan out parallel Child Tasks—one per implicated microservice—assigning Antigravity, OpenCode, or Pi CLI harnesses onto warm gVisor slots 02 and 03.",
			HighlightedNodes: []string{
				"ax-server-openfga",
				"ateworker-gvisor-pool",
				"pricing-service",
				"shipping-service",
			},
			ActiveEdges: []string{
				"edge-axserver-sandbox",
				"edge-sandbox-gateway-git",
			},
			RootTaskPhase:  "WaitingForChildren",
			ChildTaskPhase: "Running",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "ACTIVE_SANDBOX", TaskRole: "Root Task (Waiting)", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-02", State: "ACTIVE_SANDBOX", TaskRole: "Child Task: pricing-service", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-03", State: "ACTIVE_SANDBOX", TaskRole: "Child Task: shipping-service", Harness: "opencode"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "gRPC ax.v1alpha1.TaskService/CreateTask (child-fixer fan-out)",
			ProtocolPayloadPreview: `{"parent_task":"root-inc-101","children":[{"name":"child-inc-101-pricing-service","service":"pricing-service","harness":"antigravity","worker_slot":"ateworker-gvisor-02"},{"name":"child-inc-101-shipping-service","service":"shipping-service","harness":"opencode","worker_slot":"ateworker-gvisor-03"}]}`,
		},
		{
			StepNumber: 4,
			ID:         "stage-4-sandbox-red-green-tdd",
			Title:      "4. Sandbox Red-Green-Verify TDD Loop",
			Subtitle:   "Write failing regression test (Red) -> synthesize minimal fix via Model Gateway -> pass suite (Green)",
			Summary:    "Inside each airgapped gVisor sandbox, the Child Task writes a unit test reproducing the exact failing cart payload, runs the native language test runner to confirm a Red failure, calls model-gateway using its ephemeral TaskSessionToken to synthesize the fix, and re-runs the full test suite to prove Green.",
			HighlightedNodes: []string{
				"ateworker-gvisor-pool",
				"model-gateway",
				"pricing-service",
				"shipping-service",
			},
			ActiveEdges: []string{
				"edge-sandbox-gateway-git",
			},
			RootTaskPhase:  "WaitingForChildren",
			ChildTaskPhase: "Running (Red -> Green TDD)",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "ACTIVE_SANDBOX", TaskRole: "Root Task (Verifying)", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-02", State: "ACTIVE_SANDBOX", TaskRole: "Red -> Green: pricing.py", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-03", State: "ACTIVE_SANDBOX", TaskRole: "Red -> Green: shipping.go", Harness: "opencode"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "POST /v1beta/models/gemini-3.8-flash:generateContent (TaskSessionToken)",
			ProtocolPayloadPreview: `{"event_type":"child.step","task_name":"child-inc-101-pricing-service","tdd_phases":["1. Reproducer Test (RED: AssertionError: -750 < 0)","2. Minimal Code Fix (pricing.py max(0, subtotal))","3. Full Unit Suite (GREEN: OK 4 tests in 0.012s)"]}`,
		},
		{
			StepNumber: 5,
			ID:         "stage-5-root-merge-hot-reload",
			Title:      "5. Root Verification Gate & Selective Hot-Reload",
			Subtitle:   "Root merges verified child branches -> pushes to storefront-git-origin -> reloads only changed services (< 10s)",
			Summary:    "Once all Child Tasks pass Green verification, the Root Task fetches and merges their branches (--no-ff) into main, pushes to storefront-git-origin, and triggers selective hot-reload. Only pricing-service and shipping-service restart in < 10s; storefront-web, checkout-service, and inventory-service stay untouched.",
			HighlightedNodes: []string{
				"ateworker-gvisor-pool",
				"storefront-git-origin",
				"pricing-service",
				"shipping-service",
				"ax-observer",
			},
			ActiveEdges: []string{
				"edge-sandbox-gateway-git",
				"edge-checkout-pricing",
				"edge-checkout-shipping",
			},
			RootTaskPhase:  "MergingAndReloading",
			ChildTaskPhase: "Completed",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "ACTIVE_SANDBOX", TaskRole: "Root Merge & Hot-Reload", Harness: "antigravity"},
				{SlotID: "ateworker-gvisor-02", State: "RECLAIMED_WARM", TaskRole: "Reclaimed Warm Slot", Harness: "ready"},
				{SlotID: "ateworker-gvisor-03", State: "RECLAIMED_WARM", TaskRole: "Reclaimed Warm Slot", Harness: "ready"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "POST /api/incidents/reload-git (Selective Hot-Reload < 10s)",
			ProtocolPayloadPreview: `{"event_type":"root.merged","incident_id":"INC-101","reloaded_services":["pricing-service","shipping-service"],"untouched_services":["storefront-web","checkout-service","inventory-service"],"checkout_replay_http_status":200,"under_ten_second_sla":true}`,
		},
		{
			StepNumber: 6,
			ID:         "stage-6-suspend-task-reclamation",
			Title:      "6. SuspendTask Snapshot & Slot Reclamation",
			Subtitle:   "gRPC SuspendTask(SNAPSHOT_FIDELITY_VOLUMES) -> Cloud Storage snapshot shelf -> all 4 slots warm & ready",
			Summary:    "Child and Root Tasks invoke gRPC SuspendTask with SNAPSHOT_FIDELITY_VOLUMES. Agent Substrate v0.6.0 snapshots each task's /workspace diff and TDD log to the Cloud Storage Volume Snapshot Shelf and transitions all gVisor slots back to Ready (Warm) in single-digit milliseconds.",
			HighlightedNodes: []string{
				"ax-server-openfga",
				"ateworker-gvisor-pool",
				"ax-observer",
			},
			ActiveEdges: []string{
				"edge-observer-axserver",
				"edge-axserver-sandbox",
			},
			RootTaskPhase:  "Suspended",
			ChildTaskPhase: "Suspended",
			SlotStates: []LifecycleSlotState{
				{SlotID: "ateworker-gvisor-01", State: "RECLAIMED_WARM", TaskRole: "Reclaimed Warm (Snapshot Saved)", Harness: "ready"},
				{SlotID: "ateworker-gvisor-02", State: "RECLAIMED_WARM", TaskRole: "Reclaimed Warm (Snapshot Saved)", Harness: "ready"},
				{SlotID: "ateworker-gvisor-03", State: "RECLAIMED_WARM", TaskRole: "Reclaimed Warm (Snapshot Saved)", Harness: "ready"},
				{SlotID: "ateworker-gvisor-04", State: "WARM_IDLE", TaskRole: "Idle Warm Sandbox", Harness: "ready"},
			},
			ProtocolLabel:          "gRPC ax.v1alpha1.TaskService/SuspendTask (SNAPSHOT_FIDELITY_VOLUMES)",
			ProtocolPayloadPreview: `{"atespace":"default","name":"child-inc-101-pricing-service","fidelity":"SNAPSHOT_FIDELITY_VOLUMES","snapshot_path":"/workspace/.ax-state/child-inc-101-pricing-service.json","slot_reclamation_ms":4,"worker_state":"Ready (Warm)"}`,
		},
	}

	commonCounters := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"active_execution_mode":   rt.ActiveExecutionMode,
			"live_worker_slots":       fmt.Sprintf("%d slots (%d warm)", rt.LiveWorkerSlots, rt.WarmIdleSlots),
			"gateway_sessions_count":  fmt.Sprintf("%d sessions", rt.GatewaySessionsCount),
			"storefront_baseline_tag": rt.StorefrontBaselineTag,
		}
		for k, v := range extra {
			m[k] = ScrubStageSafeString(v)
		}
		return m
	}

	adrs := []CatalogADRCard{
		{
			ID:       "ADR-0001",
			Number:   1,
			Filename: expectedADRFileList[0],
			Title:    "Three Decoupled Production Surfaces and Subtle Cross-Service Incidents",
			Status:   "Accepted",
			Category: "Architecture & Surfaces",
			Summary:  "Splits the demo into three independent browser-accessible surfaces (:3001 Retail Storefront, :3002 Observability Stack, :3000 AX Observer) connected exclusively via real HTTP, OpenMetrics, LogQL, and Alertmanager v4 webhooks.",
			KeyDecisions: []string{
				"Decouple Retail Storefront (:3001), Monitoring (:3002), and AX Observer (:3000)",
				"Replace toy chaos toggles with 4 realistic business-logic edge cases (INC-101..INC-104)",
				"Keep presenter controls inside the discrete Cmd+K drawer on AX Observer",
			},
			RelatedNodes:    []string{"storefront-web", "prometheus-alertmanager", "ax-observer"},
			RuntimeCounters: commonCounters(map[string]string{"decoupled_surfaces": "3 (:3001, :3002, :3000)"}),
		},
		{
			ID:       "ADR-0002",
			Number:   2,
			Filename: expectedADRFileList[1],
			Title:    "In-Cluster Prometheus, Alertmanager, Loki & Grafana with Google Cloud Dual Export",
			Status:   "Accepted",
			Category: "Observability",
			Summary:  "Runs in-cluster Prometheus (5s scrape), Alertmanager v4, Loki LogQL, and Grafana for instant local triage while dual-exporting metrics and structured logs to Google Cloud Managed Service for Prometheus and Cloud Logging.",
			KeyDecisions: []string{
				"5-second OpenMetrics scrape across all 5 polyglot services",
				"Alertmanager v4 webhook dispatch with fingerprint deduplication",
				"Dual export to Google Cloud Managed Prometheus and Cloud Logging",
			},
			RelatedNodes:    []string{"prometheus-alertmanager", "loki-grafana", "ax-observer"},
			RuntimeCounters: commonCounters(map[string]string{"scrape_interval": "5s", "dual_export": "GMP + Cloud Logging"}),
		},
		{
			ID:       "ADR-0003",
			Number:   3,
			Filename: expectedADRFileList[2],
			Title:    "Polyglot Five-Service Storefront and Four-Incident Catalog",
			Status:   "Accepted",
			Category: "Storefront Mesh",
			Summary:  "Implements the Retail Storefront as 5 microservices across 4 languages (Node.js/TypeScript, Go, Python/FastAPI, Rust/Axum) backed by a real Git repository and 4 multi-service incidents.",
			KeyDecisions: []string{
				"5 microservices across Node.js/TS, Go, Python/FastAPI, and Rust/Axum",
				"4 cross-service incidents (INC-101..INC-104) missed by naive single-service CI",
				"Selective blast-radius tracking proving unaffected services stay healthy",
			},
			RelatedNodes:    []string{"storefront-web", "checkout-service", "pricing-service", "inventory-service", "shipping-service"},
			RuntimeCounters: commonCounters(map[string]string{"polyglot_services": "5 services / 4 languages", "incidents": "4 (INC-101..INC-104)"}),
		},
		{
			ID:       "ADR-0004",
			Number:   4,
			Filename: expectedADRFileList[3],
			Title:    "Telemetry-Driven Root Task Triage and Red-Green TDD Child Task Lifecycle",
			Status:   "Accepted",
			Category: "Agent Orchestration",
			Summary:  "Defines the two-level self-spawning task tree on google/ax: a Root Task that queries Loki/Prometheus and spawns parallel Child Tasks that execute a strict Red-Green-Verify TDD loop before auto-suspending.",
			KeyDecisions: []string{
				"Root Task correlates Alertmanager labels with Loki stack traces",
				"1 parallel Child Task per implicated service with parent_task lineage",
				"Mandatory Red reproducer test -> minimal code fix -> Green test suite -> SuspendTask",
			},
			RelatedNodes:    []string{"ax-observer", "ax-server-openfga", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"tdd_phases": "4 (Red -> Fix -> Green -> Suspend)"}),
		},
		{
			ID:       "ADR-0005",
			Number:   5,
			Filename: expectedADRFileList[4],
			Title:    "Clean Modern AX Observer UI, Path-Based Routing, and Projector Light Mode",
			Status:   "Accepted",
			Category: "UI & Routing",
			Summary:  "Establishes path-based routing (/, /workers, /about), an icon-only zero-layout-shift Projector Light Mode toggle, inline SVG iconography (zero emojis), and safe DOM construction (zero innerHTML).",
			KeyDecisions: []string{
				"Path-based routing (/, /workers, /about) rejecting ?view= query params",
				"Icon-only Sun/Moon Projector Light Mode toggle with zero layout shift",
				"Safe DOM APIs (createElement / createElementNS) with strict CSP headers",
			},
			RelatedNodes:    []string{"ax-observer"},
			RuntimeCounters: commonCounters(map[string]string{"routes": "/, /workers, /about", "dom_safety": "0 innerHTML / 0 emojis"}),
		},
		{
			ID:       "ADR-0006",
			Number:   6,
			Filename: expectedADRFileList[5],
			Title:    "Agent Substrate v0.6.0 Warm gVisor Pool and Cloud Storage Volume Snapshots",
			Status:   "Accepted",
			Category: "Agent Substrate",
			Summary:  "Deploys 4 warm gVisor sandbox worker pods (ateworker-gvisor) managed by Agent Substrate v0.6.0, reclaiming compute slots in milliseconds via SuspendTask(SNAPSHOT_FIDELITY_VOLUMES) to Cloud Storage.",
			KeyDecisions: []string{
				"4 pre-warmed gVisor sandbox slots eliminating cold-start container delays",
				"OpenFGA relationship tuples enforcing owner vs viewer task isolation",
				"Volume snapshots persisted to Cloud Storage upon SuspendTask",
			},
			RelatedNodes:    []string{"ax-server-openfga", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"reclaimed_slots": fmt.Sprintf("%d reclaimed", rt.ReclaimedWarmSlots)}),
		},
		{
			ID:       "ADR-0007",
			Number:   7,
			Filename: expectedADRFileList[6],
			Title:    "Multi-Harness Sandbox Execution and Gemini Flash Fallback Cascade",
			Status:   "Accepted",
			Category: "Model Gateway & Harnesses",
			Summary:  "Pre-bakes 3 distinct CLI coding harnesses (Antigravity, OpenCode, Pi) into the runner image and routes all model calls through the Zero-Secret Model Gateway with a 4-tier Gemini Flash cascade for compute obtainability.",
			KeyDecisions: []string{
				"Support Antigravity (google-genai), OpenCode (openai-compat), and Pi (openai-compat)",
				"Zero cloud credentials inside gVisor sandboxes; authenticate via TaskSessionToken",
				"Automatic 4-model Gemini Flash cascade on HTTP 429/503",
			},
			RelatedNodes:    []string{"model-gateway", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"active_model": rt.ActiveCascadeModel, "provenance": rt.SourceProvenance}),
		},
		{
			ID:       "ADR-0008",
			Number:   8,
			Filename: expectedADRFileList[7],
			Title:    "Dual-Mode Orchestration (LIVE_GKE vs SIMULATED) and Zero-Trust Sandbox Coordination",
			Status:   "Accepted",
			Category: "Agent Orchestration",
			Summary:  "Enables the exact same orchestrator and evaluation suite to run against real gVisor sandboxes on GKE (LIVE_GKE) or deterministic in-process fixtures (SIMULATED) with structured progress callbacks.",
			KeyDecisions: []string{
				"Auto-detect LIVE_GKE mode when AX_SERVER_ADDR is configured",
				"Stream structured sandbox callbacks to POST /api/observer/events",
				"Async 202 Accepted webhook handling with ?wait=true sync option",
			},
			RelatedNodes:    []string{"ax-observer", "ax-server-openfga", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"default_harness": rt.DefaultHarness}),
		},
		{
			ID:       "ADR-0009",
			Number:   9,
			Filename: expectedADRFileList[8],
			Title:    "Polyglot HTTP Mesh and Zero-Downtime Git Smart-HTTP Selective Hot-Reload",
			Status:   "Accepted",
			Category: "Storefront Mesh",
			Summary:  "Replaces mock in-memory handlers with real Node.js, Go, Python, and Rust child processes supervised on loopback ports, reloading only git-modified services in under 10 seconds.",
			KeyDecisions: []string{
				"Per-service process supervisors on loopback ports 13001 and 18081..18084",
				"Git Smart-HTTP server at /git/storefront.git for sandbox clone/push",
				"Selective git diff baseline-v1..HEAD reload in < 10s",
			},
			RelatedNodes:    []string{"storefront-git-origin", "storefront-web", "checkout-service", "pricing-service", "inventory-service", "shipping-service"},
			RuntimeCounters: commonCounters(map[string]string{"hot_reload_sla": "< 10s selective"}),
		},
		{
			ID:       "ADR-0010",
			Number:   10,
			Filename: expectedADRFileList[9],
			Title:    "Interactive 3-Tab SVG /about Architecture Catalog and Automated Drift Verification",
			Status:   "Accepted",
			Category: "UI & Routing",
			Summary:  "Backs the /about page with a typed Go catalog (GET /api/about/catalog) and automated drift verification test suite checking all 11 ADRs, 5 service directories, 3 harness binaries, 4 cascade models, and non-overlapping SVG geometry.",
			KeyDecisions: []string{
				"3 interactive SVG tabs: System Topology, Recursive TDD Lifecycle, Security & ADRs",
				"Automated VerifyArchitectureCatalogDrift test guarding against doc/code drift",
				"Stage-safe telemetry scrubbing removing cluster/project/bucket identifiers",
			},
			RelatedNodes:    []string{"ax-observer"},
			RuntimeCounters: commonCounters(map[string]string{"catalog_nodes": "12 nodes / 11 edges", "catalog_adrs": "11 ADRs verified"}),
		},
		{
			ID:       "ADR-0011",
			Number:   11,
			Filename: expectedADRFileList[10],
			Title:    "Two-Tier Evaluation Suite, Chaos Soak Loop, and Google Cloud Dual-Export Verification",
			Status:   "Accepted",
			Category: "Evaluation & SRE",
			Summary:  "Verifies all 4 incident trajectories across both fast hermetic CI (make eval) and live GKE sandboxes (make eval-live-gke) across all 3 harnesses, plus continuous chaos soak and Cloud Monitoring/Logging verification.",
			KeyDecisions: []string{
				"Tier 1 hermetic evaluation (< 30s) + Tier 2 live GKE evaluation across all 4 incidents",
				"Continuous chaos soak runner cycling incidents and harnesses with invariant checks",
				"Verify Google Cloud Managed Prometheus and Cloud Logging dual export",
			},
			RelatedNodes:    []string{"prometheus-alertmanager", "loki-grafana", "ax-observer", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"eval_tiers": "Tier 1 Hermetic + Tier 2 Live GKE"}),
		},
		{
			ID:       "ADR-0012",
			Number:   12,
			Filename: expectedADRFileList[11],
			Title:    "Flagship BOM-20 Blog Narrative, Five-Lab Developer Tutorial, and Stage Walkthrough Contract",
			Status:   "Accepted",
			Category: "Documentation & Launch",
			Summary:  "Defines the 7-section BOM-20 technical blog narrative across the Three-Layer Substrate Stack, the 5-lab developer tutorial from SIMULATED mode to live GKE soak, and automated humanizer/stage-safe prose verification.",
			KeyDecisions: []string{
				"7-section BOM-20 blog post framing the Three-Layer Substrate Stack with live GKE benchmarks",
				"5-lab reproducible tutorial from make run-local to make deploy and make eval-live-gke",
				"Automated humanizer/unslop and stage-safe hygiene checks plus Google Doc sync",
			},
			RelatedNodes:    []string{"ax-observer", "ax-server-openfga", "model-gateway", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"launch_deliverables": "Blog + 5-Lab Tutorial + Stage Walkthrough"}),
		},
		{
			ID:       "ADR-0013",
			Number:   13,
			Filename: expectedADRFileList[12],
			Title:    "Upstream google/ax Compatibility Overlay and Three-Patch PR Blueprints (Issues #482, #511, #518)",
			Status:   "Accepted",
			Category: "Agent Substrate",
			Summary:  "Maintains a reproducible compatibility overlay (deploy/ax-server/ax-server-v061.patch) and contributor guide (docs/upstream/google-ax-v060-patches.md) mapping our 3 live-GKE fixes to upstream google/ax Issues #482, #511, and #518.",
			KeyDecisions: []string{
				"Issue #482: /healthz:80 WakeupProbe, GOLDEN_SNAPSHOT_MODE_DISABLED, and per-actor EgressPolicy before ResumeActor",
				"Issue #511: substrate.ateapi.v1.Control proto path, OpenFGA default store bootstrap, and context.WithoutCancel(ctx) on SuspendTask",
				"Issue #518: persistent /workspace/.ax maiden-run marker and isolated /workspace/.ax-state/<task>.json checkpoint metadata",
			},
			RelatedNodes:    []string{"ax-server-openfga", "ateworker-gvisor-pool", "ax-observer"},
			RuntimeCounters: commonCounters(map[string]string{"overlay_patches": "3 (#482, #511, #518)", "substrate_api": "substrate.ateapi.v1.Control"}),
		},
		{
			ID:       "ADR-0014",
			Number:   14,
			Filename: expectedADRFileList[13],
			Title:    "BOM-19 Three-Layer Architecture Guide (substrate, env, ax), Layer Selection Matrix, and Stage Warmup Gate",
			Status:   "Accepted",
			Category: "Documentation & Launch",
			Summary:  "Defines the 5-section BOM-19 three-layer architecture guide (docs/architecture/three-layer-substrate-env-ax.md), the 7-workload Layer Selection Matrix across substrate, env, and google/ax, the Stage Presenter Runbook, and the make stage-ready Stage Warmup Gate.",
			KeyDecisions: []string{
				"5-section BOM-19 architecture guide covering Layer 1 (substrate), Layer 2 (env), and Layer 3 (google/ax)",
				"7-workload Layer Selection Matrix mapping developer patterns to Layer 1, Layer 1+2, or Full Three-Layer Stack",
				"Standalone BOM-19 Google Doc deliverable and one-command make stage-ready Stage Warmup Gate",
			},
			RelatedNodes:    []string{"ax-observer", "ax-server-openfga", "ateworker-gvisor-pool"},
			RuntimeCounters: commonCounters(map[string]string{"layer_matrix": "7 workload patterns", "catalog_adrs": "14 ADRs verified"}),
		},
	}

	adrs = enrichADRsFromDisk(adrs)

	return ArchitectureTopologyCatalog{
		ViewBox:         TopologyViewBox{Width: 1200, Height: 620},
		StatusStrip:     rt,
		Swimlanes:       swimlanes,
		Nodes:           nodes,
		Edges:           edges,
		Services:        services,
		Scenarios:       scenarios,
		Harnesses:       harnesses,
		CascadeModels:   cascadeModels,
		LifecycleStages: lifecycleStages,
		ADRs:            adrs,
	}
}

func enrichADRsFromDisk(adrs []CatalogADRCard) []CatalogADRCard {
	candidates := []string{
		filepath.Join("docs", "adr"),
		filepath.Join("..", "..", "docs", "adr"),
		filepath.Join("/workspace", "docs", "adr"),
	}
	var adrDir string
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			adrDir = c
			break
		}
	}
	if adrDir == "" {
		return adrs
	}
	for i := range adrs {
		raw, err := os.ReadFile(filepath.Join(adrDir, adrs[i].Filename))
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for idx, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "# ") {
				heading := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
				if colonIdx := strings.Index(heading, ":"); colonIdx >= 0 && colonIdx+1 < len(heading) {
					heading = strings.TrimSpace(heading[colonIdx+1:])
				}
				if heading != "" {
					adrs[i].Title = ScrubStageSafeString(heading)
				}
			}
			if strings.EqualFold(trimmed, "## Status") && idx+1 < len(lines) {
				for j := idx + 1; j < len(lines) && j <= idx+4; j++ {
					stLine := strings.TrimSpace(lines[j])
					if stLine != "" && !strings.HasPrefix(stLine, "#") {
						adrs[i].Status = ScrubStageSafeString(stLine)
						break
					}
				}
			}
		}
	}
	return adrs
}

// ValidateCatalogGeometry verifies that all topology nodes fit within ViewBox without overlapping and all edge/stage/scenario references resolve.
func ValidateCatalogGeometry(cat ArchitectureTopologyCatalog) error {
	if cat.ViewBox.Width != 1200 || cat.ViewBox.Height != 620 {
		return fmt.Errorf("invalid ViewBox %+v: want 1200x620", cat.ViewBox)
	}
	if len(cat.Nodes) != 12 {
		return fmt.Errorf("expected 12 topology nodes, got %d", len(cat.Nodes))
	}
	if len(cat.Edges) != 11 {
		return fmt.Errorf("expected 11 topology edges, got %d", len(cat.Edges))
	}

	nodeMap := make(map[string]TopologyNode, len(cat.Nodes))
	for i, n := range cat.Nodes {
		if n.ID == "" {
			return fmt.Errorf("node[%d] has empty ID", i)
		}
		if _, dup := nodeMap[n.ID]; dup {
			return fmt.Errorf("duplicate node ID %q", n.ID)
		}
		if n.Width <= 0 || n.Height <= 0 {
			return fmt.Errorf("node %q has non-positive dimensions %dx%d", n.ID, n.Width, n.Height)
		}
		if n.X < 0 || n.Y < 0 || n.X+n.Width > cat.ViewBox.Width || n.Y+n.Height > cat.ViewBox.Height {
			return fmt.Errorf("node %q bounding box (%d,%d,%d,%d) exceeds ViewBox %dx%d", n.ID, n.X, n.Y, n.Width, n.Height, cat.ViewBox.Width, cat.ViewBox.Height)
		}
		for j := 0; j < i; j++ {
			other := cat.Nodes[j]
			if boxesOverlap(n, other) {
				return fmt.Errorf("node %q (%d,%d,%d,%d) overlaps with node %q (%d,%d,%d,%d)", n.ID, n.X, n.Y, n.Width, n.Height, other.ID, other.X, other.Y, other.Width, other.Height)
			}
		}
		nodeMap[n.ID] = n
	}

	edgeMap := make(map[string]TopologyEdge, len(cat.Edges))
	for _, e := range cat.Edges {
		if e.ID == "" {
			return fmt.Errorf("edge has empty ID")
		}
		if _, ok := nodeMap[e.FromNode]; !ok {
			return fmt.Errorf("edge %q references unknown FromNode %q", e.ID, e.FromNode)
		}
		if _, ok := nodeMap[e.ToNode]; !ok {
			return fmt.Errorf("edge %q references unknown ToNode %q", e.ID, e.ToNode)
		}
		if len(e.PathWaypoints) < 2 {
			return fmt.Errorf("edge %q must have at least 2 PathWaypoints", e.ID)
		}
		edgeMap[e.ID] = e
	}

	for _, sc := range cat.Scenarios {
		for _, nid := range sc.ActiveNodeIDs {
			if _, ok := nodeMap[nid]; !ok {
				return fmt.Errorf("scenario %q references unknown ActiveNodeID %q", sc.IncidentID, nid)
			}
		}
		for _, eid := range sc.ActiveEdgeIDs {
			if _, ok := edgeMap[eid]; !ok {
				return fmt.Errorf("scenario %q references unknown ActiveEdgeID %q", sc.IncidentID, eid)
			}
		}
	}

	for _, st := range cat.LifecycleStages {
		for _, nid := range st.HighlightedNodes {
			if _, ok := nodeMap[nid]; !ok {
				return fmt.Errorf("lifecycle stage %d (%s) references unknown HighlightedNode %q", st.StepNumber, st.ID, nid)
			}
		}
		for _, eid := range st.ActiveEdges {
			if _, ok := edgeMap[eid]; !ok {
				return fmt.Errorf("lifecycle stage %d (%s) references unknown ActiveEdge %q", st.StepNumber, st.ID, eid)
			}
		}
	}

	return nil
}

func boxesOverlap(a, b TopologyNode) bool {
	return a.X < b.X+b.Width && a.X+a.Width > b.X && a.Y < b.Y+b.Height && a.Y+a.Height > b.Y
}

// VerifyArchitectureCatalogDrift validates that the /about catalog matches the repository's ADRs, polyglot services, harnesses, scenarios, cascade models, and SVG geometry.
func VerifyArchitectureCatalogDrift(repoRoot string) error {
	cat := BuildArchitectureTopologyCatalog(CatalogRuntimeTelemetry{
		ActiveExecutionMode:   "SIMULATED",
		DefaultHarness:        "multi",
		LiveWorkerSlots:       4,
		WarmIdleSlots:         4,
		StorefrontBaselineTag: storefront.BaselineGitTag,
	})

	if err := ValidateCatalogGeometry(cat); err != nil {
		return fmt.Errorf("catalog geometry validation failed: %w", err)
	}

	// 1. Verify all ADR files exist on disk and are non-empty
	if len(cat.ADRs) != len(expectedADRFileList) {
		return fmt.Errorf("expected %d ADRs in catalog, got %d", len(expectedADRFileList), len(cat.ADRs))
	}
	for i, adr := range cat.ADRs {
		expectedFile := expectedADRFileList[i]
		if adr.Filename != expectedFile {
			return fmt.Errorf("ADR[%d] filename = %q, want %q", i, adr.Filename, expectedFile)
		}
		fullPath := filepath.Join(repoRoot, "docs", "adr", adr.Filename)
		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("missing ADR file %s: %w", fullPath, err)
		}
		if info.Size() < 64 {
			return fmt.Errorf("ADR file %s is unexpectedly small (%d bytes)", fullPath, info.Size())
		}
	}

	// 2. Verify all 5 polyglot service directories and source files exist under services/
	if len(cat.Services) != 5 {
		return fmt.Errorf("expected 5 polyglot services in catalog, got %d", len(cat.Services))
	}
	for _, svc := range cat.Services {
		svcDir := filepath.Join(repoRoot, "services", svc.Service)
		if st, err := os.Stat(svcDir); err != nil || !st.IsDir() {
			return fmt.Errorf("missing service directory %s: %v", svcDir, err)
		}
		srcPath := filepath.Join(repoRoot, filepath.FromSlash(svc.SourceFile))
		if _, err := os.Stat(srcPath); err != nil {
			return fmt.Errorf("missing service source file %s: %w", srcPath, err)
		}
		testPath := filepath.Join(repoRoot, filepath.FromSlash(svc.TestFile))
		if _, err := os.Stat(testPath); err != nil {
			return fmt.Errorf("missing service test file %s: %w", testPath, err)
		}
	}

	// 3. Verify all 3 CLI coding harness binaries exist under harnesses/bin/
	if len(cat.Harnesses) != 3 {
		return fmt.Errorf("expected 3 coding harnesses in catalog, got %d", len(cat.Harnesses))
	}
	for _, h := range cat.Harnesses {
		binPath := filepath.Join(repoRoot, filepath.FromSlash(h.BinaryPath))
		if _, err := os.Stat(binPath); err != nil {
			return fmt.Errorf("missing harness binary %s: %w", binPath, err)
		}
	}

	// 4. Verify all 4 failure scenarios match internal/storefront.ScenarioCatalog()
	sfScenarios := storefront.ScenarioCatalog()
	if len(cat.Scenarios) != len(sfScenarios) {
		return fmt.Errorf("catalog scenario count %d != storefront scenario count %d", len(cat.Scenarios), len(sfScenarios))
	}
	for i, sc := range sfScenarios {
		if cat.Scenarios[i].IncidentID != sc.ID {
			return fmt.Errorf("scenario[%d] ID = %q, want %q", i, cat.Scenarios[i].IncidentID, sc.ID)
		}
	}

	// 5. Verify all 4 Gemini Flash cascade models match internal/gateway.DefaultFallbackCascade
	if len(cat.CascadeModels) != len(gateway.DefaultFallbackCascade) {
		return fmt.Errorf("catalog cascade model count %d != gateway cascade count %d", len(cat.CascadeModels), len(gateway.DefaultFallbackCascade))
	}
	for i, wantModel := range gateway.DefaultFallbackCascade {
		if cat.CascadeModels[i].ModelID != wantModel {
			return fmt.Errorf("cascade model[%d] = %q, want %q", i, cat.CascadeModels[i].ModelID, wantModel)
		}
	}

	return nil
}
