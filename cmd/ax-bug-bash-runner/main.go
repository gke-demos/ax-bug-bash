package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/google/ax/runner"
)

func main() {
	var (
		opts        bugbash.CustomRunnerOptions
		executeTask bool
	)
	flag.IntVar(&opts.Port, "port", runner.DefaultPort, "Port for the probe and guest server")
	flag.IntVar(&opts.MetadataPort, "metadata-port", runner.DefaultMetadataPort, "Loopback-only port for the metadata endpoints")
	flag.StringVar(&opts.TaskFile, "task-file", "", "Read the Task YAML from this file instead of AX_TASK_YAML")
	flag.StringVar(&opts.WorkspaceFile, "workspace-file", "", "Read the Workspace YAML from this file instead of AX_WORKSPACE_YAML")
	flag.StringVar(&opts.AXServerAddr, "ax-server-addr", "", "gRPC address of ax-server (defaults to $AX_SERVER_ADDR)")
	flag.StringVar(&opts.ObserverCallbackURL, "observer-callback-url", "", "HTTP callback URL for reporting child task results (defaults to $OBSERVER_CALLBACK_URL)")
	flag.StringVar(&opts.ModelGatewayURL, "model-gateway-url", "", "Base URL of the zero-secret Agent Platform Model Gateway (defaults to $MODEL_GATEWAY_URL)")
	flag.BoolVar(&executeTask, "execute-task", false, "Run the supervised inner task workload reading from $AX_METADATA_URL/metadata/v1alpha1/ax/task")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if executeTask {
		if err := bugbash.ExecuteTaskFromMetadataEndpoint(ctx); err != nil {
			slog.Error("task workload execution failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := bugbash.RunCustomTaskRunner(ctx, opts); err != nil {
		slog.Error("custom ax-task-runner failed", "error", err)
		os.Exit(1)
	}
}
