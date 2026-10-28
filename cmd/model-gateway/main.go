package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
)

func main() {
	cfg := gateway.DefaultConfig()
	srv := gateway.NewServer(cfg, nil)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf(
			"model-gateway (Gemini Enterprise Agent Platform) listening on %s (project=%s, location=%s, cascade=%s, fixture_fallback=%v)",
			cfg.ListenAddr,
			cfg.ProjectID,
			cfg.Location,
			strings.Join(cfg.FallbackCascade, "->"),
			cfg.EnableFixtureFallback,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("model-gateway listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
