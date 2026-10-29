package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// DefaultLiveGKEReportPath is the stage-safe JSON report location written by `make eval-live-gke` (ADR-0011).
const DefaultLiveGKEReportPath = "/tmp/ax-live-gke-eval-report.json"

// LiveChaosSoakPair defines one (IncidentID, HarnessMode) pair in the 4x4 diagonal chaos soak.
type LiveChaosSoakPair struct {
	IncidentID  string `json:"incident_id"`
	HarnessMode string `json:"harness_mode"`
}

// LiveChaosSoakDiagonal defines the canonical 4-incident x 4-harness diagonal evaluated at MaxConcurrency = 1.
var LiveChaosSoakDiagonal = []LiveChaosSoakPair{
	{IncidentID: storefront.IncidentStackedPromoNegativeSubtotal, HarnessMode: "antigravity"},
	{IncidentID: storefront.IncidentFlashSaleBundleUnderflow, HarnessMode: "opencode"},
	{IncidentID: storefront.IncidentZeroDecimalJPYRounding, HarnessMode: "pi"},
	{IncidentID: storefront.IncidentLithiumHazmatWeightTier, HarnessMode: "multi"},
}

// LiveGKEEvalConfig configures the live GKE 4x4 chaos soak and Google Cloud dual-export verifier.
type LiveGKEEvalConfig struct {
	ObserverURL        string
	StorefrontURL      string
	StorefrontGitURL   string
	PrometheusURL      string
	ModelGatewayURL    string
	ProjectID          string
	AccessToken        string
	CloudLoggingURL    string
	CloudMonitoringURL string
	ReportPath         string
	TrajectoryTimeout  time.Duration
	AlertDrainTimeout  time.Duration
	DualExportTimeout  time.Duration
	PollInterval       time.Duration
	HTTPClient         *http.Client
}

// LiveGKETrajectoryReport records the per-trajectory verification grade during a live GKE chaos soak.
type LiveGKETrajectoryReport struct {
	IncidentID                          string   `json:"incident_id"`
	HarnessMode                         string   `json:"harness_mode"`
	ExecutionID                         string   `json:"execution_id"`
	RootTaskName                        string   `json:"root_task_name"`
	ImplicatedServices                  []string `json:"implicated_services"`
	SpawnedServices                     []string `json:"spawned_services"`
	SpawnedChildCount                   int      `json:"spawned_child_count"`
	ActiveModel                         string   `json:"active_model"`
	SourceProvenance                    string   `json:"source_provenance"`
	RolloutDurationMs                   int64    `json:"rollout_duration_ms"`
	PostRolloutHTTPStatus               int      `json:"post_rollout_http_status"`
	BaselineCIGreen                     bool     `json:"baseline_ci_green"`
	AlertmanagerWebhookTriggered        bool     `json:"alertmanager_webhook_triggered"`
	SelectiveBlastRadiusFanOut          bool     `json:"selective_blast_radius_fan_out"`
	RedGreenRegressionTDD               bool     `json:"red_green_regression_tdd"`
	OnCommandExitAndSuspendReclaimed    bool     `json:"on_command_exit_and_suspend_reclaimed"`
	PreWarmedRolloutAndStorefrontHealed bool     `json:"pre_warmed_rollout_and_storefront_healed"`
	ResetCommitSHA                      string   `json:"reset_commit_sha"`
	AlertDrainedAfterReset              bool     `json:"alert_drained_after_reset"`
	DurationMs                          int64    `json:"duration_ms"`
	Passed                              bool     `json:"passed"`
	FailureReasons                      []string `json:"failure_reasons,omitempty"`
}

// GCPDualExportReport captures verification results for Google Cloud Logging and Google Cloud Managed Service for Prometheus.
type GCPDualExportReport struct {
	CloudLoggingVerified         bool     `json:"cloud_logging_verified"`
	CloudLoggingEntriesCount     int      `json:"cloud_logging_entries_count"`
	ManagedPrometheusVerified    bool     `json:"managed_prometheus_verified"`
	ManagedPrometheusSeriesCount int      `json:"managed_prometheus_series_count"`
	Passed                       bool     `json:"passed"`
	FailureReasons               []string `json:"failure_reasons,omitempty"`
}

// LiveGKESoakReport aggregates the 4-trajectory chaos soak and GCP dual-export verification report.
type LiveGKESoakReport struct {
	GeneratedAt        time.Time                 `json:"generated_at"`
	ExecutionMode      string                    `json:"execution_mode"`
	MaxConcurrency     int                       `json:"max_concurrency"`
	TotalTrajectories  int                       `json:"total_trajectories"`
	PassedTrajectories int                       `json:"passed_trajectories"`
	Trajectories       []LiveGKETrajectoryReport `json:"trajectories"`
	DualExport         GCPDualExportReport       `json:"dual_export"`
	ReportPath         string                    `json:"report_path"`
	AllPassed          bool                      `json:"all_passed"`
}

// RunLiveGKEChaosSoak executes the sequential (MaxConcurrency = 1) 4-incident x 4-harness chaos soak
// against a live GKE cluster (or loopback mock cluster), verifies baseline-v1 reset and Prometheus alert drain
// between runs, verifies Google Cloud Logging + Managed Prometheus dual-export, and writes a stage-safe report.
func RunLiveGKEChaosSoak(ctx context.Context, cfg LiveGKEEvalConfig) (LiveGKESoakReport, error) {
	if cfg.ReportPath == "" {
		cfg.ReportPath = DefaultLiveGKEReportPath
	}
	if cfg.TrajectoryTimeout <= 0 {
		cfg.TrajectoryTimeout = 150 * time.Second
	}
	if cfg.AlertDrainTimeout <= 0 {
		cfg.AlertDrainTimeout = 20 * time.Second
	}
	if cfg.DualExportTimeout <= 0 {
		cfg.DualExportTimeout = 60 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	cleanupTunnels, resolvedCfg, err := ensureLiveEndpoints(ctx, cfg)
	if err != nil {
		return LiveGKESoakReport{}, err
	}
	defer cleanupTunnels()
	cfg = resolvedCfg

	report := LiveGKESoakReport{
		GeneratedAt:       time.Now().UTC(),
		ExecutionMode:     "LIVE_GKE",
		MaxConcurrency:    1,
		TotalTrajectories: len(LiveChaosSoakDiagonal),
		ReportPath:        cfg.ReportPath,
	}

	for _, pair := range LiveChaosSoakDiagonal {
		tr := runSingleLiveTrajectory(ctx, cfg, pair)
		tr = scrubTrajectoryReport(tr)
		report.Trajectories = append(report.Trajectories, tr)
		if tr.Passed {
			report.PassedTrajectories++
		}
	}

	dualReport, dualErr := VerifyGCPDualExport(ctx, cfg)
	if dualErr != nil {
		dualReport.FailureReasons = append(dualReport.FailureReasons, observer.ScrubStageSafeString(dualErr.Error()))
	}
	for i := range dualReport.FailureReasons {
		dualReport.FailureReasons[i] = observer.ScrubStageSafeString(dualReport.FailureReasons[i])
	}
	report.DualExport = dualReport

	report.AllPassed = report.PassedTrajectories == report.TotalTrajectories && report.DualExport.Passed

	if err := writeStageSafeSoakReport(cfg.ReportPath, report); err != nil {
		return report, fmt.Errorf("writing stage-safe soak report: %w", err)
	}
	return report, nil
}

func runSingleLiveTrajectory(ctx context.Context, cfg LiveGKEEvalConfig, pair LiveChaosSoakPair) LiveGKETrajectoryReport {
	start := time.Now()
	sc, ok := storefront.GetScenario(pair.IncidentID)
	if !ok {
		return LiveGKETrajectoryReport{
			IncidentID:     pair.IncidentID,
			HarnessMode:    pair.HarnessMode,
			FailureReasons: []string{fmt.Sprintf("unknown incident %q", pair.IncidentID)},
		}
	}

	tr := LiveGKETrajectoryReport{
		IncidentID:         sc.ID,
		HarnessMode:        pair.HarnessMode,
		ImplicatedServices: append([]string(nil), sc.ImplicatedServices...),
	}

	obsBase := strings.TrimRight(cfg.ObserverURL, "/")
	sfBase := strings.TrimRight(cfg.StorefrontURL, "/")
	gitBase := strings.TrimRight(httputil.EnvOr("STOREFRONT_GIT_URL", cfg.StorefrontGitURL), "/")
	if gitBase == "" {
		gitBase = sfBase
	}
	promBase := strings.TrimRight(cfg.PrometheusURL, "/")

	// 1. Verify Storefront baseline health + configure runtime default_harness on AX Observer
	if status, _, err := doJSONRequest(ctx, cfg.HTTPClient, http.MethodGet, sfBase+"/healthz", nil); err == nil && status == http.StatusOK {
		tr.BaselineCIGreen = true
	} else {
		tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("storefront /healthz failed: status=%d err=%v", status, err))
	}

	if status, body, err := doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, obsBase+"/api/config/harness", map[string]string{
		"default_harness": pair.HarnessMode,
	}); err != nil || status >= 300 {
		tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("POST /api/config/harness failed: status=%d err=%v body=%s", status, err, string(body)))
	}

	// Ensure clean baseline before starting the trajectory
	_, _, _ = doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, obsBase+"/api/incidents/reset", map[string]string{
		"default_harness": pair.HarnessMode,
	})

	// Capture existing execution IDs so we only grade the new trajectory for this incident
	seenExecIDs := make(map[string]bool)
	if snap, err := fetchObserverState(ctx, cfg.HTTPClient, obsBase); err == nil {
		for _, t := range snap.Trees {
			if t != nil && t.ExecutionID != "" {
				seenExecIDs[t.ExecutionID] = true
			}
		}
	}

	// 2. Activate incident and fire failing checkout requests against Storefront
	_, _, _ = doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, sfBase+"/api/incidents/activate", map[string]string{
		"incident_id": sc.ID,
	})

	preStatus, _, _ := doJSONRequestWithHeaders(ctx, cfg.HTTPClient, http.MethodPost, sfBase+"/api/cart/checkout", sc.TriggerCart, map[string]string{
		"X-Incident-Scenario": sc.ID,
	})
	if preStatus == http.StatusInternalServerError {
		tr.AlertmanagerWebhookTriggered = true
	}

	// Wait briefly for organic Prometheus -> Alertmanager webhook dispatch; if not yet started, invoke /api/incidents/trigger
	organicWait := 12 * time.Second
	if cfg.PollInterval < 200*time.Millisecond {
		organicWait = cfg.PollInterval * 2
	}
	var activeTree *observer.ObserverTreeView
	organicDeadline := time.Now().Add(organicWait)
	for time.Now().Before(organicDeadline) {
		if snap, err := fetchObserverState(ctx, cfg.HTTPClient, obsBase); err == nil {
			if tree := findNewTreeForIncident(snap, sc.ID, seenExecIDs); tree != nil {
				activeTree = tree
				break
			}
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(cfg.PollInterval):
		}
	}

	if activeTree == nil {
		status, body, err := doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, obsBase+"/api/incidents/trigger", map[string]string{
			"incident_id": sc.ID,
			"harness":     pair.HarnessMode,
		})
		if err != nil || (status != http.StatusOK && status != http.StatusAccepted) {
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("POST /api/incidents/trigger failed: status=%d err=%v body=%s", status, err, string(body)))
		} else {
			tr.AlertmanagerWebhookTriggered = true
		}
	} else {
		tr.AlertmanagerWebhookTriggered = true
	}

	// 3. Poll GET /api/state until the tree reaches Phase == "HEALED" (or FAILED / timeout)
	trajDeadline := time.Now().Add(cfg.TrajectoryTimeout)
	for time.Now().Before(trajDeadline) {
		snap, err := fetchObserverState(ctx, cfg.HTTPClient, obsBase)
		if err == nil {
			if tree := findNewTreeForIncident(snap, sc.ID, seenExecIDs); tree != nil {
				activeTree = tree
				if strings.EqualFold(tree.Phase, "HEALED") || strings.EqualFold(tree.Phase, "FAILED") {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(cfg.PollInterval):
		}
	}

	if activeTree == nil {
		tr.FailureReasons = append(tr.FailureReasons, "no Bug Bash Tree execution observed for incident "+sc.ID)
	} else {
		gradeLiveObserverTree(&tr, sc, pair.HarnessMode, activeTree)
	}

	// Verify post-rollout checkout returns HTTP 200 OK
	postStatus, _, postErr := doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, sfBase+"/api/cart/checkout", sc.TriggerCart)
	tr.PostRolloutHTTPStatus = postStatus
	if postErr == nil && postStatus == http.StatusOK && activeTree != nil && strings.EqualFold(activeTree.Phase, "HEALED") && activeTree.RolloutDurationMs > 0 && activeTree.RolloutDurationMs < 10000 {
		tr.PreWarmedRolloutAndStorefrontHealed = true
	} else {
		tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("post-rollout checkout status=%d err=%v treePhase=%v", postStatus, postErr, treePhaseOrEmpty(activeTree)))
	}

	// 4. Reset storefront.git to baseline-v1 and drain Prometheus firing alerts
	resetStatus, resetBody, resetErr := doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, gitBase+"/api/incidents/reset", map[string]string{
		"default_harness": pair.HarnessMode,
	})
	if resetErr == nil && resetStatus == http.StatusOK {
		var resetResp struct {
			CommitSHA string `json:"commit_sha"`
		}
		_ = json.Unmarshal(resetBody, &resetResp)
		tr.ResetCommitSHA = strings.TrimSpace(resetResp.CommitSHA)
	}
	_, _, _ = doJSONRequest(ctx, cfg.HTTPClient, http.MethodPost, obsBase+"/api/incidents/reset", map[string]string{
		"default_harness": pair.HarnessMode,
	})
	if tr.ResetCommitSHA == "" {
		tr.ResetCommitSHA = "baseline-v1"
	}

	if promBase != "" {
		drained, drainErr := waitForPrometheusAlertsDrained(ctx, cfg.HTTPClient, promBase, cfg.AlertDrainTimeout, cfg.PollInterval)
		tr.AlertDrainedAfterReset = drained
		if !drained || drainErr != nil {
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("prometheus alert drain failed: %v", drainErr))
		}
	} else {
		tr.AlertDrainedAfterReset = true
	}

	tr.DurationMs = time.Since(start).Milliseconds()
	tr.Passed = tr.BaselineCIGreen &&
		tr.AlertmanagerWebhookTriggered &&
		tr.SelectiveBlastRadiusFanOut &&
		tr.RedGreenRegressionTDD &&
		tr.OnCommandExitAndSuspendReclaimed &&
		tr.PreWarmedRolloutAndStorefrontHealed &&
		tr.AlertDrainedAfterReset &&
		len(tr.FailureReasons) == 0
	return tr
}

func gradeLiveObserverTree(tr *LiveGKETrajectoryReport, sc storefront.IncidentScenario, harnessMode string, tree *observer.ObserverTreeView) {
	tr.ExecutionID = tree.ExecutionID
	tr.RootTaskName = tree.RootTask.TaskName
	tr.SpawnedChildCount = len(tree.ChildTasks)
	tr.RolloutDurationMs = tree.RolloutDurationMs
	tr.SourceProvenance = strings.ToUpper(strings.TrimSpace(tree.SourceProvenance))

	childBySvc := make(map[string]observer.ObserverChildTaskView, len(tree.ChildTasks))
	for _, ch := range tree.ChildTasks {
		tr.SpawnedServices = append(tr.SpawnedServices, ch.Service)
		childBySvc[ch.Service] = ch
		if tr.ActiveModel == "" && ch.ActiveModel != "" {
			tr.ActiveModel = ch.ActiveModel
		}
		if tr.SourceProvenance == "" && ch.SourceProvenance != "" {
			tr.SourceProvenance = strings.ToUpper(strings.TrimSpace(ch.SourceProvenance))
		}
	}

	selectiveOK := len(tree.ChildTasks) == len(sc.ImplicatedServices) && len(tree.ChildTasks) >= 2 && len(tree.ChildTasks) <= 3
	for _, expSvc := range sc.ImplicatedServices {
		if _, ok := childBySvc[expSvc]; !ok {
			selectiveOK = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("missing child task for implicated service %s", expSvc))
		}
	}
	for _, unaffSvc := range sc.UnaffectedServices {
		if _, spawned := childBySvc[unaffSvc]; spawned {
			selectiveOK = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("unaffected service %s was erroneously spawned", unaffSvc))
		}
	}
	tr.SelectiveBlastRadiusFanOut = selectiveOK

	allTDD := len(tree.ChildTasks) > 0
	allSuspend := len(tree.ChildTasks) > 0
	seenHarnesses := make(map[string]bool)

	for _, ch := range tree.ChildTasks {
		seenHarnesses[ch.Harness] = true
		prov := strings.ToUpper(strings.TrimSpace(ch.SourceProvenance))
		if prov != "LIVE" {
			allTDD = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("child %s SourceProvenance = %q, want LIVE (FIXTURE fallback is forbidden in live GKE evaluation)", ch.TaskName, ch.SourceProvenance))
		}
		if harnessMode != "multi" && ch.Harness != "" && !strings.EqualFold(ch.Harness, harnessMode) {
			allTDD = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("child %s harness = %q, want %q", ch.TaskName, ch.Harness, harnessMode))
		}
		hasRed := ch.RedTestOutput != ""
		hasGreen := ch.GreenTestOutput != ""
		for _, st := range ch.TDDSteps {
			if st.Phase == "REPRODUCE_RED" && (st.Status == "PASSED" || st.Status == "COMPLETED") {
				hasRed = true
			}
			if st.Phase == "PATCH_GREEN" && (st.Status == "PASSED" || st.Status == "COMPLETED") {
				hasGreen = true
			}
		}
		if !hasRed || !hasGreen {
			allTDD = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("child %s missing Red-Green TDD steps (red=%v green=%v)", ch.TaskName, hasRed, hasGreen))
		}
		if strings.TrimSpace(ch.UnifiedGitDiff) == "" && strings.TrimSpace(ch.CodeFixDiff) == "" {
			allSuspend = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("child %s missing UnifiedGitDiff", ch.TaskName))
		}
		if !ch.Suspended && !strings.EqualFold(ch.Phase, "Suspended") {
			allSuspend = false
			tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("child %s was not suspended (Suspended=%v Phase=%s)", ch.TaskName, ch.Suspended, ch.Phase))
		}
	}
	if harnessMode == "multi" && len(seenHarnesses) < 2 {
		allTDD = false
		tr.FailureReasons = append(tr.FailureReasons, fmt.Sprintf("multi harness mode did not rotate across >=2 harnesses: %v", seenHarnesses))
	}

	tr.RedGreenRegressionTDD = allTDD
	tr.OnCommandExitAndSuspendReclaimed = allSuspend
}

// VerifyGCPDualExport queries Google Cloud Logging (v2/entries:list) and Google Cloud Managed Service for Prometheus
// (v3/projects/<project>/timeSeries) with X-Goog-User-Project headers to verify telemetry dual-export within 60s.
func VerifyGCPDualExport(ctx context.Context, cfg LiveGKEEvalConfig) (GCPDualExportReport, error) {
	var report GCPDualExportReport
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	timeout := cfg.DualExportTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	projectID := strings.TrimSpace(cfg.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(os.Getenv("PROJECT_ID"))
	}
	if projectID == "" {
		out, err := exec.CommandContext(ctx, "gcloud", "config", "get-value", "project").Output()
		if err == nil {
			projectID = strings.TrimSpace(string(out))
		}
	}
	if projectID == "" {
		report.FailureReasons = append(report.FailureReasons, "unable to resolve GCP ProjectID")
		return report, nil
	}

	token := strings.TrimSpace(cfg.AccessToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GCP_ACCESS_TOKEN"))
	}
	if token == "" {
		out, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
		if err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	if token == "" {
		report.FailureReasons = append(report.FailureReasons, "unable to resolve GCP OAuth2 access token")
		return report, nil
	}

	loggingBase := strings.TrimRight(cfg.CloudLoggingURL, "/")
	if loggingBase == "" {
		loggingBase = "https://logging.googleapis.com"
	}
	monitoringBase := strings.TrimRight(cfg.CloudMonitoringURL, "/")
	if monitoringBase == "" {
		monitoringBase = "https://monitoring.googleapis.com"
	}

	headers := map[string]string{
		"Authorization":       "Bearer " + token,
		"X-Goog-User-Project": projectID,
	}

	deadline := time.Now().Add(timeout)
	var lastLogErr, lastMonErr string

	for {
		if !report.CloudLoggingVerified {
			since := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
			filter := fmt.Sprintf(`resource.type="k8s_container" AND resource.labels.namespace_name=("ax-bug-bash" OR "ax-system" OR "monitoring") AND timestamp >= %q`, since)
			reqBody := map[string]any{
				"resourceNames": []string{"projects/" + projectID},
				"filter":        filter,
				"pageSize":      10,
				"orderBy":       "timestamp desc",
			}
			status, body, err := doJSONRequestWithHeaders(ctx, client, http.MethodPost, loggingBase+"/v2/entries:list", reqBody, headers)
			if err != nil || status >= 300 {
				lastLogErr = fmt.Sprintf("status=%d err=%v body=%s", status, err, strings.TrimSpace(string(body)))
			} else {
				var parsed struct {
					Entries []any `json:"entries"`
				}
				if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil && len(parsed.Entries) > 0 {
					report.CloudLoggingVerified = true
					report.CloudLoggingEntriesCount = len(parsed.Entries)
				} else {
					lastLogErr = fmt.Sprintf("0 entries returned (err=%v)", jsonErr)
				}
			}
		}

		if !report.ManagedPrometheusVerified {
			endTime := time.Now().UTC().Format(time.RFC3339)
			startTime := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
			q := url.Values{}
			q.Set("filter", `metric.type="prometheus.googleapis.com/storefront_http_requests_total/counter"`)
			q.Set("interval.startTime", startTime)
			q.Set("interval.endTime", endTime)
			q.Set("pageSize", "10")
			tsURL := fmt.Sprintf("%s/v3/projects/%s/timeSeries?%s", monitoringBase, url.PathEscape(projectID), q.Encode())

			status, body, err := doJSONRequestWithHeaders(ctx, client, http.MethodGet, tsURL, nil, headers)
			if err != nil || status >= 300 {
				lastMonErr = fmt.Sprintf("status=%d err=%v body=%s", status, err, strings.TrimSpace(string(body)))
			} else {
				var parsed struct {
					TimeSeries []any `json:"timeSeries"`
				}
				if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil && len(parsed.TimeSeries) > 0 {
					report.ManagedPrometheusVerified = true
					report.ManagedPrometheusSeriesCount = len(parsed.TimeSeries)
				} else {
					lastMonErr = fmt.Sprintf("0 timeSeries returned for storefront_http_requests_total (err=%v)", jsonErr)
				}
			}
		}

		if report.CloudLoggingVerified && report.ManagedPrometheusVerified {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(pollInterval):
		}
		if ctx.Err() != nil {
			break
		}
	}

	if !report.CloudLoggingVerified {
		report.FailureReasons = append(report.FailureReasons, "Cloud Logging verification failed: "+observer.ScrubStageSafeString(lastLogErr))
	}
	if !report.ManagedPrometheusVerified {
		report.FailureReasons = append(report.FailureReasons, "Managed Prometheus verification failed: "+observer.ScrubStageSafeString(lastMonErr))
	}
	report.Passed = report.CloudLoggingVerified && report.ManagedPrometheusVerified
	return report, nil
}

func waitForPrometheusAlertsDrained(ctx context.Context, client *http.Client, promBase string, timeout, pollInterval time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, body, err := doJSONRequest(ctx, client, http.MethodGet, promBase+"/api/v1/alerts", nil)
		if err == nil && status == http.StatusOK {
			var parsed struct {
				Data struct {
					Alerts []struct {
						Labels map[string]string `json:"labels"`
						State  string            `json:"state"`
					} `json:"alerts"`
				} `json:"data"`
			}
			if jsonErr := json.Unmarshal(body, &parsed); jsonErr == nil {
				firing := 0
				for _, a := range parsed.Data.Alerts {
					if strings.EqualFold(a.State, "firing") && a.Labels["alertname"] == "StorefrontHTTP500Spike" {
						firing++
					}
				}
				if firing == 0 {
					return true, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("StorefrontHTTP500Spike still firing after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func fetchObserverState(ctx context.Context, client *http.Client, obsBase string) (observer.StateSnapshot, error) {
	status, body, err := doJSONRequest(ctx, client, http.MethodGet, obsBase+"/api/state", nil)
	if err != nil {
		return observer.StateSnapshot{}, err
	}
	if status != http.StatusOK {
		return observer.StateSnapshot{}, fmt.Errorf("GET /api/state returned HTTP %d", status)
	}
	var snap observer.StateSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return observer.StateSnapshot{}, err
	}
	return snap, nil
}

func findNewTreeForIncident(snap observer.StateSnapshot, incidentID string, seenExecIDs map[string]bool) *observer.ObserverTreeView {
	if snap.LatestTree != nil && strings.EqualFold(snap.LatestTree.IncidentID, incidentID) && !seenExecIDs[snap.LatestTree.ExecutionID] {
		return snap.LatestTree
	}
	for i := len(snap.Trees) - 1; i >= 0; i-- {
		t := snap.Trees[i]
		if t != nil && strings.EqualFold(t.IncidentID, incidentID) && !seenExecIDs[t.ExecutionID] {
			return t
		}
	}
	return nil
}

func treePhaseOrEmpty(t *observer.ObserverTreeView) string {
	if t == nil {
		return "<nil>"
	}
	return t.Phase
}

func doJSONRequest(ctx context.Context, client *http.Client, method, targetURL string, payload any) (int, []byte, error) {
	return doJSONRequestWithHeaders(ctx, client, method, targetURL, payload, nil)
}

func doJSONRequestWithHeaders(ctx context.Context, client *http.Client, method, targetURL string, payload any, headers map[string]string) (int, []byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

func scrubTrajectoryReport(tr LiveGKETrajectoryReport) LiveGKETrajectoryReport {
	tr.ExecutionID = observer.ScrubStageSafeString(tr.ExecutionID)
	tr.RootTaskName = observer.ScrubStageSafeString(tr.RootTaskName)
	tr.ActiveModel = observer.ScrubStageSafeString(tr.ActiveModel)
	tr.ResetCommitSHA = observer.ScrubStageSafeString(tr.ResetCommitSHA)
	for i := range tr.FailureReasons {
		tr.FailureReasons[i] = observer.ScrubStageSafeString(tr.FailureReasons[i])
	}
	return tr
}

func writeStageSafeSoakReport(reportPath string, report LiveGKESoakReport) error {
	rawJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	scrubbed := observer.ScrubStageSafeString(string(rawJSON)) + "\n"
	return os.WriteFile(reportPath, []byte(scrubbed), 0o600)
}

func ensureLiveEndpoints(ctx context.Context, cfg LiveGKEEvalConfig) (func(), LiveGKEEvalConfig, error) {
	if cfg.ObserverURL == "" {
		cfg.ObserverURL = strings.TrimSpace(os.Getenv("OBSERVER_URL"))
	}
	if cfg.StorefrontURL == "" {
		cfg.StorefrontURL = strings.TrimSpace(os.Getenv("STOREFRONT_URL"))
	}
	if cfg.StorefrontGitURL == "" {
		cfg.StorefrontGitURL = strings.TrimSpace(os.Getenv("STOREFRONT_GIT_URL"))
	}
	if cfg.PrometheusURL == "" {
		cfg.PrometheusURL = strings.TrimSpace(os.Getenv("PROMETHEUS_URL"))
	}

	var cmds []*exec.Cmd
	cleanup := func() {
		for _, cmd := range cmds {
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
	}

	startPortForward := func(namespace, resource, remotePort string) (string, error) {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		_, localPort, _ := net.SplitHostPort(lis.Addr().String())
		_ = lis.Close()

		cmd := exec.CommandContext(ctx, "kubectl", "-n", namespace, "port-forward", resource, fmt.Sprintf("%s:%s", localPort, remotePort), "--address=127.0.0.1")
		if err := cmd.Start(); err != nil {
			return "", err
		}
		cmds = append(cmds, cmd)

		baseURL := "http://127.0.0.1:" + localPort
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			conn, dErr := net.DialTimeout("tcp", "127.0.0.1:"+localPort, 250*time.Millisecond)
			if dErr == nil {
				_ = conn.Close()
				return baseURL, nil
			}
			time.Sleep(150 * time.Millisecond)
		}
		return "", fmt.Errorf("kubectl port-forward %s/%s :%s timed out", namespace, resource, remotePort)
	}

	if cfg.ObserverURL == "" {
		u, err := startPortForward("ax-bug-bash", "svc/ax-observer", "3000")
		if err != nil {
			cleanup()
			return func() {}, cfg, err
		}
		cfg.ObserverURL = u
	}
	if cfg.StorefrontURL == "" {
		u, err := startPortForward("ax-bug-bash", "svc/storefront", "3001")
		if err != nil {
			cleanup()
			return func() {}, cfg, err
		}
		cfg.StorefrontURL = u
	}
	if cfg.StorefrontGitURL == "" {
		if u, err := startPortForward("ax-bug-bash", "svc/storefront-git", "8090"); err == nil {
			cfg.StorefrontGitURL = u
		} else {
			cfg.StorefrontGitURL = cfg.StorefrontURL
		}
	}
	if cfg.PrometheusURL == "" {
		u, err := startPortForward("monitoring", "svc/prometheus", "9090")
		if err != nil {
			cleanup()
			return func() {}, cfg, err
		}
		cfg.PrometheusURL = u
	}
	return cleanup, cfg, nil
}
