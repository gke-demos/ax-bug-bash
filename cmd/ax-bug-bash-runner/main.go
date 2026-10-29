package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/google/ax/runner"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runMain(ctx, os.Args[1:]); err != nil {
		slog.Error("ax-task-runner failed", "error", err)
		os.Exit(1)
	}
}

func runMain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ax-task-runner", flag.ContinueOnError)
	var (
		opts        bugbash.CustomRunnerOptions
		executeTask bool
		roleFlag    string
	)
	fs.IntVar(&opts.Port, "port", runner.DefaultPort, "Port for the probe and guest server")
	fs.IntVar(&opts.MetadataPort, "metadata-port", runner.DefaultMetadataPort, "Loopback-only port for the metadata endpoints")
	fs.StringVar(&opts.TaskFile, "task-file", "", "Read the Task YAML from this file instead of AX_TASK_YAML")
	fs.StringVar(&opts.WorkspaceFile, "workspace-file", "", "Read the Workspace YAML from this file instead of AX_WORKSPACE_YAML")
	fs.StringVar(&opts.AXServerAddr, "ax-server-addr", "", "gRPC address of ax-server (defaults to $AX_SERVER_ADDR)")
	fs.StringVar(&opts.ObserverCallbackURL, "observer-callback-url", "", "HTTP callback URL for reporting task events (defaults to $OBSERVER_CALLBACK_URL)")
	fs.StringVar(&opts.ModelGatewayURL, "model-gateway-url", "", "Base URL of the zero-secret Agent Platform Model Gateway (defaults to $MODEL_GATEWAY_URL)")
	fs.BoolVar(&executeTask, "execute-task", false, "Run the supervised inner task workload reading from $AX_METADATA_URL/metadata/v1alpha1/ax/task")
	fs.StringVar(&roleFlag, "role", "", "Optional explicit task role override (root-triage or child-fixer)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	normRole := strings.ToLower(strings.TrimSpace(roleFlag))
	if normRole != "" && normRole != "root-triage" && normRole != "child-fixer" {
		return fmt.Errorf("invalid --role %q (expected root-triage or child-fixer)", roleFlag)
	}

	if executeTask {
		return bugbash.ExecuteTaskFromMetadataEndpoint(ctx, bugbash.TaskExecutionOptions{
			ModelGatewayURL: opts.ModelGatewayURL,
			RoleOverride:    normRole,
		})
	}

	return bugbash.RunCustomTaskRunner(ctx, opts)
}
