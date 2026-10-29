package bugbash

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/structpb"
)

// DefaultWorkspaceName is the canonical Workspace resource name matching manifests/ax/workspace.yaml.
const DefaultWorkspaceName = "storefront-monorepo"

// AlertPayload represents the incoming production incident alert and stack trace telemetry
// passed to the Root Task via Task.spec.config.
type AlertPayload struct {
	IncidentID     string                                      `json:"incident_id"`
	AlertName      string                                      `json:"alert_name,omitempty"`
	ErrorCode      string                                      `json:"error_code,omitempty"`
	ErrorMessage   string                                      `json:"error_message,omitempty"`
	StackTraces    []string                                    `json:"stack_traces,omitempty"`
	ServiceTraces  map[string]storefront.ServiceIncidentDetail `json:"service_traces,omitempty"`
	HarnessMode    string                                      `json:"harness_mode,omitempty"`
	RequestedModel string                                      `json:"requested_model,omitempty"`
}

// ChildTaskMission holds the per-service remediation mission stored in a Child Task's Task.spec.config.
type ChildTaskMission struct {
	TaskName           string         `json:"task_name"`
	ParentTask         string         `json:"parent_task"`
	IncidentID         string         `json:"incident_id"`
	Service            string         `json:"service"`
	Language           string         `json:"language"`
	Harness            string         `json:"harness"`
	RequestedModel     string         `json:"requested_model"`
	SourceFile         string         `json:"source_file"`
	TestFile           string         `json:"test_file"`
	ErrorType          string         `json:"error_type"`
	ErrorMessage       string         `json:"error_message"`
	StackTrace         string         `json:"stack_trace"`
	RegressionTestName string         `json:"regression_test_name"`
	FailingRequest     map[string]any `json:"failing_request,omitempty"`
}

// CheckoutRequestToMap converts a typed CheckoutRequest into a structpb-compatible map[string]any.
func CheckoutRequestToMap(req storefront.CheckoutRequest) map[string]any {
	raw, err := json.Marshal(req)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// TriagePlan is the output of the Root Task's stack-trace analysis.
type TriagePlan struct {
	IncidentID         string             `json:"incident_id"`
	Title              string             `json:"title"`
	Summary            string             `json:"summary"`
	WhyCIMissedIt      string             `json:"why_ci_missed_it"`
	ImplicatedServices []string           `json:"implicated_services"`
	UnaffectedServices []string           `json:"unaffected_services"`
	HarnessMode        string             `json:"harness_mode"`
	RequestedModel     string             `json:"requested_model"`
	ChildSpecs         []ChildTaskMission `json:"child_specs"`
}

// SpawnChildOptions configures how Child Tasks are created on ax-server.
type SpawnChildOptions struct {
	Atespace        string
	WorkspaceName   string
	WorkspacePath   string
	TaskRunnerImage string
	Command         []string
	ExtraEnv        []*v1alpha1.EnvVar
	NameSuffix      string
}

var nonDNSChars = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeTaskName normalizes an identifier into a valid lowercase RFC 1123 label for google/ax.
func SanitizeTaskName(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	cleaned := nonDNSChars.ReplaceAllString(lower, "-")
	cleaned = strings.Trim(cleaned, "-")
	if cleaned == "" {
		return "ax-task"
	}
	if len(cleaned) > v1alpha1.MaxNameLength {
		cleaned = strings.TrimRight(cleaned[:v1alpha1.MaxNameLength], "-")
	}
	return cleaned
}

// ResolveChildHarnesses assigns a Coding Harness ("antigravity", "opencode", or "pi")
// to each implicated microservice. In "multi" mode, harnesses rotate across sibling Child Tasks.
func ResolveChildHarnesses(implicatedServices []string, harnessMode string) []string {
	mode := strings.ToLower(strings.TrimSpace(harnessMode))
	out := make([]string, len(implicatedServices))
	switch mode {
	case "antigravity", "opencode", "pi":
		for i := range implicatedServices {
			out[i] = mode
		}
	default:
		// "multi", "parallel", or default rotates ["antigravity", "opencode", "pi"]
		for i := range implicatedServices {
			out[i] = gateway.SupportedHarnesses[i%len(gateway.SupportedHarnesses)]
		}
	}
	return out
}

// AnalyzeIncidentTraces inspects the alert stack traces against the bound monorepo workspace
// and identifies ONLY the 2-3 implicated microservices (never blindly all 5 services).
func AnalyzeIncidentTraces(alert AlertPayload, workspaceDir string) (TriagePlan, error) {
	combinedTraceText := strings.Join(alert.StackTraces, "\n") + "\n" + alert.ErrorMessage + "\n" + alert.ErrorCode

	incidentID := strings.ToUpper(strings.TrimSpace(alert.IncidentID))
	if incidentID == "" {
		incidentID = inferIncidentIDFromTraces(combinedTraceText)
	}

	sc, hasScenario := storefront.GetScenario(incidentID)

	detectedSet := make(map[string]bool)
	var orderedDetected []string
	addDetected := func(svc string) {
		if svc == "" || detectedSet[svc] {
			return
		}
		if workspaceDir != "" {
			svcPath := filepath.Join(workspaceDir, "services", svc)
			if st, err := os.Stat(svcPath); err != nil || !st.IsDir() {
				// Only enforce directory check if workspaceDir actually has a services/ folder
				if stRoot, errRoot := os.Stat(filepath.Join(workspaceDir, "services")); errRoot == nil && stRoot.IsDir() {
					return
				}
			}
		}
		detectedSet[svc] = true
		orderedDetected = append(orderedDetected, svc)
	}

	// 1. Parse raw stack traces in order of appearance
	for _, trace := range alert.StackTraces {
		for _, svc := range matchServicesInTrace(trace) {
			addDetected(svc)
		}
	}
	// 2. Check structured ServiceTraces map if provided
	if len(orderedDetected) == 0 && len(alert.ServiceTraces) > 0 {
		if hasScenario {
			for _, svc := range sc.ImplicatedServices {
				if _, ok := alert.ServiceTraces[svc]; ok {
					addDetected(svc)
				}
			}
		}
		for svc := range alert.ServiceTraces {
			addDetected(svc)
		}
	}
	// 3. Fall back to combined error message signatures or scenario catalog
	if len(orderedDetected) == 0 {
		for _, svc := range matchServicesInTrace(combinedTraceText) {
			addDetected(svc)
		}
	}
	if hasScenario {
		for _, svc := range sc.ImplicatedServices {
			addDetected(svc)
		}
	}

	// If scenario is known, order implicated services in the canonical scenario order
	if hasScenario && len(orderedDetected) > 0 {
		var canonical []string
		for _, svc := range sc.ImplicatedServices {
			if detectedSet[svc] {
				canonical = append(canonical, svc)
			}
		}
		for _, svc := range orderedDetected {
			if !containsString(canonical, svc) {
				canonical = append(canonical, svc)
			}
		}
		orderedDetected = canonical
	}

	if len(orderedDetected) == 0 {
		return TriagePlan{}, fmt.Errorf("no implicated microservices identified from alert traces (incident=%q)", incidentID)
	}
	if len(orderedDetected) >= len(storefront.AllServices()) {
		// Guardrail: never blindly fan out to all 5 services
		orderedDetected = orderedDetected[:3]
	}

	var unaffected []string
	for _, svc := range storefront.AllServices() {
		if !detectedSet[svc] {
			unaffected = append(unaffected, svc)
		}
	}

	requestedModel := strings.TrimSpace(alert.RequestedModel)
	if requestedModel == "" {
		requestedModel = gateway.DefaultFallbackCascade[0]
	}
	harnessMode := strings.ToLower(strings.TrimSpace(alert.HarnessMode))
	if harnessMode == "" {
		harnessMode = "multi"
	}
	harnesses := ResolveChildHarnesses(orderedDetected, harnessMode)

	failingReq := CheckoutRequestToMap(sc.TriggerCart)
	childSpecs := make([]ChildTaskMission, 0, len(orderedDetected))
	for i, svc := range orderedDetected {
		detail := resolveServiceDetail(sc, alert, svc)
		taskName := SanitizeTaskName(fmt.Sprintf("fixer-%s-%s", strings.ToLower(incidentID), svc))
		childSpecs = append(childSpecs, ChildTaskMission{
			TaskName:           taskName,
			IncidentID:         incidentID,
			Service:            svc,
			Language:           detail.Language,
			Harness:            harnesses[i],
			RequestedModel:     requestedModel,
			SourceFile:         detail.SourceFile,
			TestFile:           detail.TestFile,
			ErrorType:          detail.ErrorType,
			ErrorMessage:       detail.ErrorMessage,
			StackTrace:         detail.StackTrace,
			RegressionTestName: detail.RegressionTestName,
			FailingRequest:     failingReq,
		})
	}

	title := sc.Title
	if title == "" {
		title = fmt.Sprintf("Incident %s Multi-Service Failure", incidentID)
	}
	summary := sc.Summary
	if summary == "" {
		summary = alert.ErrorMessage
	}

	return TriagePlan{
		IncidentID:         incidentID,
		Title:              title,
		Summary:            summary,
		WhyCIMissedIt:      sc.WhyCIMissedIt,
		ImplicatedServices: orderedDetected,
		UnaffectedServices: unaffected,
		HarnessMode:        harnessMode,
		RequestedModel:     requestedModel,
		ChildSpecs:         childSpecs,
	}, nil
}

// SpawnChildTasksForTriage dynamically creates and resumes one Child Task on ax-server
// for each implicated microservice in plan.ChildSpecs.
func SpawnChildTasksForTriage(
	ctx context.Context,
	axClient v1alpha1.AXClient,
	parentTaskName string,
	plan TriagePlan,
	opts SpawnChildOptions,
) ([]*v1alpha1.Task, error) {
	if axClient == nil {
		return nil, fmt.Errorf("axClient is required")
	}
	atespace := strings.TrimSpace(opts.Atespace)
	if atespace == "" {
		atespace = "default"
	}
	wsName := strings.TrimSpace(opts.WorkspaceName)
	if wsName == "" {
		wsName = DefaultWorkspaceName
	}
	wsPath := strings.TrimSpace(opts.WorkspacePath)
	if wsPath == "" {
		wsPath = v1alpha1.DefaultWorkspacePath
	}

	spawned := make([]*v1alpha1.Task, 0, len(plan.ChildSpecs))
	for _, spec := range plan.ChildSpecs {
		mission := spec
		mission.ParentTask = SanitizeTaskName(parentTaskName)
		if opts.NameSuffix != "" {
			mission.TaskName = SanitizeTaskName(mission.TaskName + "-" + opts.NameSuffix)
		} else {
			mission.TaskName = SanitizeTaskName(mission.TaskName)
		}

		cfgStruct, err := ChildMissionToStruct(mission)
		if err != nil {
			return nil, fmt.Errorf("encoding config struct for %s: %w", mission.TaskName, err)
		}

		envVars := []*v1alpha1.EnvVar{
			{Name: "AX_TASK_NAME", Value: mission.TaskName},
			{Name: "AX_PARENT_TASK", Value: mission.ParentTask},
			{Name: "AX_INCIDENT_ID", Value: mission.IncidentID},
			{Name: "AX_TARGET_SERVICE", Value: mission.Service},
			{Name: "AX_CODING_HARNESS", Value: mission.Harness},
			{Name: "AX_REQUESTED_MODEL", Value: mission.RequestedModel},
			{Name: "GEMINI_API_KEY", Value: gateway.SandboxTokenPlaceholder},
			{Name: "OPENAI_API_KEY", Value: gateway.SandboxTokenPlaceholder},
		}
		envVars = append(envVars, opts.ExtraEnv...)

		taskReq := &v1alpha1.Task{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindTask,
			Metadata: &v1alpha1.ObjectMeta{
				Name:     mission.TaskName,
				Atespace: atespace,
			},
			Spec: &v1alpha1.TaskSpec{
				Image:   opts.TaskRunnerImage,
				Command: opts.Command,
				Env:     envVars,
				Workspace: &v1alpha1.WorkspaceRef{
					Name: wsName,
					Path: wsPath,
				},
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{
						Cpu:    "1000m",
						Memory: "2048Mi",
					},
				},
				Config: cfgStruct,
			},
		}

		if _, err := axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: taskReq}); err != nil {
			_, _ = axClient.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: atespace, Name: mission.TaskName})
			if _, err2 := axClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: taskReq}); err2 != nil {
				return nil, fmt.Errorf("CreateTask(%s): %w", mission.TaskName, err2)
			}
		}
		resumed, err := axClient.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{
			Atespace: atespace,
			Name:     mission.TaskName,
		})
		if err != nil {
			return nil, fmt.Errorf("ResumeTask(%s): %w", mission.TaskName, err)
		}
		spawned = append(spawned, resumed)
	}
	return spawned, nil
}

// ChildMissionToStruct serializes a ChildTaskMission into a protobuf Struct for Task.spec.config.
func ChildMissionToStruct(m ChildTaskMission) (*structpb.Struct, error) {
	failingReq := m.FailingRequest
	if len(failingReq) == 0 && m.IncidentID != "" {
		if sc, ok := storefront.GetScenario(m.IncidentID); ok {
			failingReq = CheckoutRequestToMap(sc.TriggerCart)
		}
	}
	if failingReq == nil {
		failingReq = map[string]any{}
	}
	return structpb.NewStruct(map[string]any{
		"role":                 "child-fixer",
		"task_name":            m.TaskName,
		"parent_task":          m.ParentTask,
		"incident_id":          m.IncidentID,
		"service":              m.Service,
		"language":             m.Language,
		"harness":              m.Harness,
		"model":                m.RequestedModel,
		"source_file":          m.SourceFile,
		"test_file":            m.TestFile,
		"error_type":           m.ErrorType,
		"error_message":        m.ErrorMessage,
		"stack_trace":          m.StackTrace,
		"regression_test_name": m.RegressionTestName,
		"failing_request":      failingReq,
	})
}

// ParseChildMissionFromTaskConfig extracts a ChildTaskMission from Task.spec.config.
func ParseChildMissionFromTaskConfig(taskName string, cfg *structpb.Struct) (ChildTaskMission, error) {
	if cfg == nil {
		return ChildTaskMission{}, fmt.Errorf("Task.spec.config is nil")
	}
	m := cfg.AsMap()
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	var failingReq map[string]any
	if fr, ok := m["failing_request"].(map[string]any); ok {
		failingReq = fr
	}
	out := ChildTaskMission{
		TaskName:           str("task_name"),
		ParentTask:         str("parent_task"),
		IncidentID:         str("incident_id"),
		Service:            str("service"),
		Language:           str("language"),
		Harness:            str("harness"),
		RequestedModel:     str("model"),
		SourceFile:         str("source_file"),
		TestFile:           str("test_file"),
		ErrorType:          str("error_type"),
		ErrorMessage:       str("error_message"),
		StackTrace:         str("stack_trace"),
		RegressionTestName: str("regression_test_name"),
		FailingRequest:     failingReq,
	}
	if out.TaskName == "" {
		out.TaskName = taskName
	}
	if out.Harness == "" {
		out.Harness = "antigravity"
	}
	if out.RequestedModel == "" {
		out.RequestedModel = gateway.DefaultFallbackCascade[0]
	}
	if out.Service == "" {
		return ChildTaskMission{}, fmt.Errorf("missing 'service' in Task.spec.config")
	}
	return out, nil
}

// AlertPayloadToStruct serializes an AlertPayload into a protobuf Struct for Root Task.spec.config.
func AlertPayloadToStruct(alert AlertPayload) (*structpb.Struct, error) {
	traces := make([]any, len(alert.StackTraces))
	for i, tr := range alert.StackTraces {
		traces[i] = tr
	}
	return structpb.NewStruct(map[string]any{
		"role":            "root-triage",
		"incident_id":     alert.IncidentID,
		"alert_name":      alert.AlertName,
		"error_code":      alert.ErrorCode,
		"error_message":   alert.ErrorMessage,
		"stack_traces":    traces,
		"harness_mode":    alert.HarnessMode,
		"requested_model": alert.RequestedModel,
	})
}

// ParseAlertPayloadFromTaskConfig extracts an AlertPayload from Root Task.spec.config.
func ParseAlertPayloadFromTaskConfig(cfg *structpb.Struct) (AlertPayload, error) {
	if cfg == nil {
		return AlertPayload{}, fmt.Errorf("Task.spec.config is nil")
	}
	m := cfg.AsMap()
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	var traces []string
	if rawList, ok := m["stack_traces"].([]any); ok {
		for _, item := range rawList {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				traces = append(traces, s)
			}
		}
	}
	return AlertPayload{
		IncidentID:     str("incident_id"),
		AlertName:      str("alert_name"),
		ErrorCode:      str("error_code"),
		ErrorMessage:   str("error_message"),
		StackTraces:    traces,
		HarnessMode:    str("harness_mode"),
		RequestedModel: str("requested_model"),
	}, nil
}

func matchServicesInTrace(trace string) []string {
	var matched []string
	add := func(svc string) {
		if !containsString(matched, svc) {
			matched = append(matched, svc)
		}
	}
	lower := strings.ToLower(trace)
	if strings.Contains(lower, "storefront-web") ||
		strings.Contains(trace, "src/server.ts") ||
		strings.Contains(trace, "convertToCurrencyMinorUnits") ||
		strings.Contains(trace, "StorefrontCurrencyPrecisionError") {
		add(storefront.ServiceStorefrontWeb)
	}
	if strings.Contains(lower, "pricing-service") ||
		strings.Contains(trace, "pricing.py") ||
		strings.Contains(trace, "apply_stacked_promos") ||
		strings.Contains(trace, "convert_currency_minor_units") ||
		strings.Contains(trace, "NegativeSubtotalError") ||
		strings.Contains(trace, "CurrencyPrecisionMismatchError") {
		add(storefront.ServicePricing)
	}
	if strings.Contains(lower, "shipping-service") ||
		strings.Contains(trace, "shipping.go") ||
		strings.Contains(trace, "QuoteCarrierRate") ||
		strings.Contains(trace, "CarrierTariffBoundsError") ||
		strings.Contains(trace, "HazmatWeightTierOverflowError") {
		add(storefront.ServiceShipping)
	}
	if strings.Contains(lower, "inventory-service") ||
		strings.Contains(trace, "inventory_service::") ||
		strings.Contains(trace, "reserve_bundle_stock") ||
		strings.Contains(trace, "classify_warehouse_hazmat") ||
		strings.Contains(trace, "RustU32UnderflowPanic") ||
		strings.Contains(trace, "UnhandledHazmatClassError") {
		add(storefront.ServiceInventory)
	}
	if strings.Contains(lower, "checkout-service") ||
		strings.Contains(trace, "checkout.go") ||
		strings.Contains(trace, "ReserveFlashBundleOrder") ||
		strings.Contains(trace, "CaptureCurrencyPayment") ||
		strings.Contains(trace, "FlashBundleUnderflowError") ||
		strings.Contains(trace, "PaymentCapturePrecisionError") {
		add(storefront.ServiceCheckout)
	}
	return matched
}

func inferIncidentIDFromTraces(text string) string {
	switch {
	case strings.Contains(text, "NegativeSubtotalError") || strings.Contains(text, "CarrierTariffBoundsError"):
		return storefront.IncidentStackedPromoNegativeSubtotal
	case strings.Contains(text, "RustU32UnderflowPanic") || strings.Contains(text, "FlashBundleUnderflowError"):
		return storefront.IncidentFlashSaleBundleUnderflow
	case strings.Contains(text, "StorefrontCurrencyPrecisionError") || strings.Contains(text, "CurrencyPrecisionMismatchError") || strings.Contains(text, "PaymentCapturePrecisionError"):
		return storefront.IncidentZeroDecimalJPYRounding
	case strings.Contains(text, "UnhandledHazmatClassError") || strings.Contains(text, "HazmatWeightTierOverflowError"):
		return storefront.IncidentLithiumHazmatWeightTier
	default:
		return storefront.IncidentStackedPromoNegativeSubtotal
	}
}

func resolveServiceDetail(sc storefront.IncidentScenario, alert AlertPayload, service string) storefront.ServiceIncidentDetail {
	if alert.ServiceTraces != nil {
		if d, ok := alert.ServiceTraces[service]; ok && d.Service != "" {
			return d
		}
	}
	if sc.ServiceTraces != nil {
		if d, ok := sc.ServiceTraces[service]; ok {
			return d
		}
	}
	return storefront.ServiceIncidentDetail{
		Service:      service,
		ErrorMessage: alert.ErrorMessage,
	}
}
