package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/gke-demos/ax-bug-bash/internal/supervisor"
)

func defaultPortForService(svc string) string {
	switch svc {
	case storefront.ServiceStorefrontWeb:
		return "3001"
	case storefront.ServiceCheckout:
		return "8081"
	case storefront.ServiceShipping:
		return "8082"
	case storefront.ServicePricing:
		return "8083"
	case storefront.ServiceInventory:
		return "8084"
	default:
		return "8080"
	}
}

func main() {
	svcFlag := flag.String("service", httputil.EnvOr("SERVICE_NAME", ""), "Storefront microservice to supervise (storefront-web, checkout-service, shipping-service, pricing-service, inventory-service)")
	addrFlag := flag.String("addr", httputil.EnvOr("SUPERVISOR_ADDR", ""), "Public HTTP listen address (defaults to 127.0.0.1:<service-port> or 127.0.0.1:$PORT)")
	gitURLFlag := flag.String("git-url", httputil.EnvOr("STOREFRONT_GIT_URL", httputil.EnvOr("GIT_REPO_URL", "http://storefront-git:8090/git/storefront.git")), "Canonical storefront.git clone URL")
	workDirFlag := flag.String("work-dir", os.Getenv("SUPERVISOR_WORK_DIR"), "Scratch directory for staged checkouts and incremental builds")
	lokiURLFlag := flag.String("loki-push-url", os.Getenv("LOKI_PUSH_URL"), "Optional Loki /loki/api/v1/push endpoint propagated to child process")
	flag.Parse()

	svc := strings.TrimSpace(*svcFlag)
	if svc == "" {
		log.Fatalf("--service (or SERVICE_NAME) is required; must be one of %v", storefront.AllServices())
	}

	listenAddr := strings.TrimSpace(*addrFlag)
	if listenAddr == "" {
		port := strings.TrimSpace(os.Getenv("PORT"))
		if port == "" {
			port = defaultPortForService(svc)
		}
		listenAddr = "127.0.0.1:" + port
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sup, err := supervisor.New(supervisor.Config{
		Service:        svc,
		GitRepoURL:     *gitURLFlag,
		WorkDir:        *workDirFlag,
		LokiPushURL:    *lokiURLFlag,
		StartupTimeout: 30 * time.Second,
		GitRetryWindow: 30 * time.Second,
	})
	if err != nil {
		log.Fatalf("failed to initialize supervisor for %s: %v", svc, err)
	}
	defer func() { _ = sup.Close() }()

	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           sup.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("polyglot-supervisor (%s) listening on %s (commit: %s, git: %s)", svc, listenAddr, sup.CurrentCommitSHA(), *gitURLFlag)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("polyglot-supervisor (%s) HTTP server error: %v", svc, err)
	}
}
