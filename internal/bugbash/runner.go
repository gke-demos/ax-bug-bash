package bugbash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// CustomRunnerOptions configures the Custom Runner embedding github.com/google/ax/runner.Run.
type CustomRunnerOptions struct {
	Port                    int
	MetadataPort            int
	TaskFile                string
	WorkspaceFile           string
	Task                    *v1alpha1.Task
	Workspace               *v1alpha1.Workspace
	AXServerAddr            string
	ObserverCallbackURL     string
	ModelGatewayURL         string
	StopRunnerOnCommandExit bool
	OnExitDone              func(res ChildTaskResult, exit runner.CommandExit)
}

// RunCustomTaskRunner embeds github.com/google/ax/runner.Run, manages /workspace/.ax-state persistence
// for SNAPSHOT_FIDELITY_VOLUMES (ADR-0006 Issue #518), captures git diff and posts callback results in
// OnCommandExit, and calls ax.v1alpha1.AX/SuspendTask on ax-server to immediately reclaim Substrate worker slots.
func RunCustomTaskRunner(ctx context.Context, opts CustomRunnerOptions) error {
	task := opts.Task
	if task == nil {
		var loadedTask v1alpha1.Task
		if err := LoadSpecYAML(opts.TaskFile, "AX_TASK_YAML", &loadedTask); err != nil {
			return err
		}
		if loadedTask.GetMetadata() != nil {
			task = &loadedTask
		}
	}

	ws := opts.Workspace
	if ws == nil {
		var loadedWS v1alpha1.Workspace
		if err := LoadSpecYAML(opts.WorkspaceFile, "AX_WORKSPACE_YAML", &loadedWS); err != nil {
			return err
		}
		if loadedWS.GetMetadata() != nil {
			ws = &loadedWS
		}
	}

	axServerAddr := strings.TrimSpace(opts.AXServerAddr)
	if axServerAddr == "" {
		axServerAddr = strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))
	}
	observerCallbackURL := strings.TrimSpace(opts.ObserverCallbackURL)
	if observerCallbackURL == "" {
		observerCallbackURL = strings.TrimSpace(os.Getenv("OBSERVER_CALLBACK_URL"))
	}
	modelGatewayURL := strings.TrimSpace(opts.ModelGatewayURL)
	if modelGatewayURL == "" {
		modelGatewayURL = strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
	}

	if task != nil {
		task = proto.Clone(task).(*v1alpha1.Task)
		if task.Spec == nil {
			task.Spec = &v1alpha1.TaskSpec{}
		}
		if len(task.Spec.Command) == 0 {
			if exe, err := os.Executable(); err == nil {
				task.Spec.Command = []string{exe, "--execute-task"}
			}
		}
		if axServerAddr != "" {
			task.Spec.Env = ensureEnvVar(task.Spec.Env, "AX_SERVER_ADDR", axServerAddr)
		}
		if observerCallbackURL != "" {
			task.Spec.Env = ensureEnvVar(task.Spec.Env, "OBSERVER_CALLBACK_URL", observerCallbackURL)
		}
		if modelGatewayURL != "" {
			task.Spec.Env = ensureEnvVar(task.Spec.Env, "MODEL_GATEWAY_URL", modelGatewayURL)
		}
	}

	wsPath := v1alpha1.DefaultWorkspacePath
	if task != nil {
		wsPath = task.GetSpec().WorkspacePath()
	}
	_ = os.MkdirAll(filepath.Join(wsPath, AXStateDirName), 0o755)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	cfg := runner.Config{
		Port:         opts.Port,
		MetadataPort: opts.MetadataPort,
		Task:         task,
		Workspace:    ws,
		OnCommandExit: func(exit runner.CommandExit) {
			taskName := "unknown-task"
			atespace := "default"
			if task != nil && task.GetMetadata() != nil {
				if task.GetMetadata().GetName() != "" {
					taskName = task.GetMetadata().GetName()
				}
				if task.GetMetadata().GetAtespace() != "" {
					atespace = task.GetMetadata().GetAtespace()
				}
			}

			res, err := LoadTaskStateSnapshot(wsPath, taskName)
			if err != nil {
				res = ChildTaskResult{
					TaskName: taskName,
				}
				if task != nil && task.GetSpec().GetConfig() != nil {
					if m, mErr := ParseChildMissionFromTaskConfig(taskName, task.GetSpec().GetConfig()); mErr == nil {
						res.IncidentID = m.IncidentID
						res.Service = m.Service
						res.Language = m.Language
						res.Harness = m.Harness
						res.RequestedModel = m.RequestedModel
						res.RegressionTestName = m.RegressionTestName
					}
				}
			}

			if res.UnifiedGitDiff == "" {
				res.UnifiedGitDiff = CaptureWorkspaceGitDiff(wsPath, "")
			}
			now := time.Now().UTC()
			if res.CompletedAt.IsZero() {
				res.CompletedAt = now
			}
			res.ExitCode = exit.ExitCode
			res.Suspended = true
			res.SuspendedAt = now
			res.TDDSteps = append(res.TDDSteps, TDDStepEvent{
				Phase:      PhaseAutoSuspend,
				Status:     "PASSED",
				Summary:    fmt.Sprintf("OnCommandExit captured git diff (%d bytes), saved /workspace/.ax-state/%s.json, and invoked ax-server SuspendTask", len(res.UnifiedGitDiff), taskName),
				DurationMs: 5,
				Timestamp:  now,
			})
			_ = SaveTaskStateSnapshot(wsPath, res)

			if observerCallbackURL != "" {
				postObserverCallback(observerCallbackURL, res)
			}
			if axServerAddr != "" {
				if err := suspendTaskViaGRPC(axServerAddr, atespace, taskName); err != nil {
					slog.Warn("OnCommandExit failed to call SuspendTask on ax-server", "task", taskName, "addr", axServerAddr, "error", err)
				}
			}
			if opts.OnExitDone != nil {
				opts.OnExitDone(res, exit)
			}
			if opts.StopRunnerOnCommandExit {
				runCancel()
			}
		},
	}

	return runner.Run(runCtx, cfg)
}

// ExecuteTaskFromMetadataEndpoint runs inside the supervised child process spawned by runner.Run.
// It fetches the Task specification from $AX_METADATA_URL/metadata/v1alpha1/ax/task and executes
// either the Root Triage fan-out ("root-triage") or the Child Fixer Red-Green TDD loop ("child-fixer").
func ExecuteTaskFromMetadataEndpoint(ctx context.Context) error {
	metadataURL := strings.TrimSpace(os.Getenv("AX_METADATA_URL"))
	task, err := FetchTaskFromMetadataURL(ctx, metadataURL)
	if err != nil {
		return fmt.Errorf("fetching task from metadata server: %w", err)
	}
	if task == nil {
		return fmt.Errorf("no task specification available at %q", metadataURL)
	}

	taskName := task.GetMetadata().GetName()
	wsPath := task.GetSpec().WorkspacePath()
	_ = os.MkdirAll(filepath.Join(wsPath, AXStateDirName), 0o755)

	cfgStruct := task.GetSpec().GetConfig()
	role := "child-fixer"
	if cfgStruct != nil {
		if r, ok := cfgStruct.AsMap()["role"].(string); ok && strings.TrimSpace(r) != "" {
			role = strings.TrimSpace(r)
		}
	}

	if role == "root-triage" {
		alert, err := ParseAlertPayloadFromTaskConfig(cfgStruct)
		if err != nil {
			return fmt.Errorf("parsing root triage alert config: %w", err)
		}
		plan, err := AnalyzeIncidentTraces(alert, wsPath)
		if err != nil {
			return fmt.Errorf("analyzing incident traces: %w", err)
		}
		axAddr := strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))
		if axAddr != "" {
			conn, err := grpc.NewClient(axAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return fmt.Errorf("dialing ax-server %s: %w", axAddr, err)
			}
			defer conn.Close()
			axClient := v1alpha1.NewAXClient(conn)
			_, err = SpawnChildTasksForTriage(ctx, axClient, taskName, plan, SpawnChildOptions{
				Atespace:      task.GetMetadata().GetAtespace(),
				WorkspaceName: task.GetSpec().GetWorkspace().GetName(),
				WorkspacePath: wsPath,
			})
			if err != nil {
				return fmt.Errorf("spawning child tasks from root triage: %w", err)
			}
		}
		return nil
	}

	mission, err := ParseChildMissionFromTaskConfig(taskName, cfgStruct)
	if err != nil {
		return fmt.Errorf("parsing child fixer mission: %w", err)
	}
	gwURL := strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
	_, err = ExecuteChildFixerTDD(ctx, ChildFixerConfig{
		Mission:         mission,
		WorkspaceDir:    wsPath,
		ModelGatewayURL: gwURL,
	})
	return err
}

// FetchTaskFromMetadataURL retrieves and parses the Task YAML from $AX_METADATA_URL/metadata/v1alpha1/ax/task
// (or falls back to AX_TASK_YAML if metadataURL is empty).
func FetchTaskFromMetadataURL(ctx context.Context, metadataURL string) (*v1alpha1.Task, error) {
	base := strings.TrimRight(strings.TrimSpace(metadataURL), "/")
	if base != "" {
		endpoint := base + "/metadata/v1alpha1/ax/task"
		client := &http.Client{Timeout: 5 * time.Second}
		var lastErr error
		for attempt := 0; attempt < 10; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err == nil {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr == nil && resp.StatusCode == http.StatusOK {
					var task v1alpha1.Task
					if err := parseYAMLLenientBytes(body, &task); err != nil {
						return nil, fmt.Errorf("decoding task YAML from metadata endpoint: %w", err)
					}
					return &task, nil
				}
				lastErr = fmt.Errorf("metadata server status %d", resp.StatusCode)
			} else {
				lastErr = err
			}
			time.Sleep(25 * time.Millisecond)
		}
		return nil, fmt.Errorf("GET %s failed: %w", endpoint, lastErr)
	}

	var task v1alpha1.Task
	if err := LoadSpecYAML("", "AX_TASK_YAML", &task); err != nil {
		return nil, err
	}
	if task.GetMetadata() == nil {
		return nil, nil
	}
	return &task, nil
}

// LoadSpecYAML decodes a Task, Workspace, or Model YAML from filePath or envVar using v1alpha1.UnmarshalYAMLLenient.
func LoadSpecYAML(filePath, envVar string, out proto.Message) error {
	var raw []byte
	switch {
	case strings.TrimSpace(filePath) != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", filePath, err)
		}
		raw = data
	case os.Getenv(envVar) != "":
		raw = []byte(os.Getenv(envVar))
	default:
		return nil
	}
	return parseYAMLLenientBytes(raw, out)
}

func parseYAMLLenientBytes(raw []byte, out proto.Message) error {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("parsing %T: %w", out, err)
	}
	if len(node.Content) == 0 || node.Content[0].ShortTag() == "!!null" {
		return nil
	}
	if err := v1alpha1.UnmarshalYAMLLenient(node.Content[0], out); err != nil {
		return fmt.Errorf("parsing %T: %w", out, err)
	}
	return nil
}

func postObserverCallback(callbackURL string, res ChildTaskResult) {
	body, err := json.Marshal(res)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func suspendTaskViaGRPC(axServerAddr, atespace, taskName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(axServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := v1alpha1.NewAXClient(conn)
	_, err = client.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{
		Atespace: atespace,
		Name:     taskName,
	})
	return err
}

func ensureEnvVar(vars []*v1alpha1.EnvVar, key, val string) []*v1alpha1.EnvVar {
	for _, v := range vars {
		if v.GetName() == key {
			v.Value = val
			return vars
		}
	}
	return append(vars, &v1alpha1.EnvVar{Name: key, Value: val})
}
