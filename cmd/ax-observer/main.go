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

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/monitoring"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/gke-demos/ax-bug-bash/internal/substrate"
)

func main() {
	observerAddr := flag.String("addr", httputil.EnvOr("LISTEN_ADDR", "127.0.0.1:3000"), "HTTP listen address for AX Observer (:3000)")
	storefrontAddr := flag.String("storefront-addr", httputil.EnvOr("STOREFRONT_LISTEN_ADDR", "127.0.0.1:3001"), "HTTP listen address for Retail Storefront in all-in-one mode (:3001)")
	monitoringAddr := flag.String("monitoring-addr", httputil.EnvOr("MONITORING_LISTEN_ADDR", "127.0.0.1:3002"), "HTTP listen address for Monitoring & Alertmanager surface in all-in-one mode (:3002)")
	allInOne := flag.Bool("all-in-one", httputil.EnvOr("ALL_IN_ONE", "true") != "false" && os.Getenv("STOREFRONT_URL") == "" && os.Getenv("AX_SERVER_ADDR") == "", "Start Storefront (:3001), Monitoring (:3002), and AX Observer (:3000) in a single process")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		sf             *storefront.Runtime
		orch           *bugbash.Orchestrator
		gw             *gateway.Server
		gwClient       *gateway.Client
		subViewer      *substrate.Viewer
		ab             *monitoring.AlertmanagerBridge
		rb             *monitoring.RolloutBridge
		scr            *monitoring.Scraper
		sfURL          = strings.TrimSpace(firstEnv("STOREFRONT_URL", "STOREFRONT_BASE_URL"))
		gwURL          = strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
		axAddr         = strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))
		subAddr        = strings.TrimSpace(firstEnv("SUBSTRATE_API_ADDR", "SUBSTRATE_CONTROL_ADDR"))
		defaultHarness = httputil.EnvOr("DEFAULT_HARNESS", "multi")
		sfPub          = httputil.EnvOr("STOREFRONT_PUBLIC_URL", "http://localhost:3001")
		grafPub        = httputil.EnvOr("GRAFANA_PUBLIC_URL", "http://localhost:3002/monitoring")
	)

	gw = gateway.NewServer(gateway.DefaultConfig(), nil)
	if gwURL != "" {
		gwClient = gateway.NewClient(gwURL, nil)
	}
	if subAddr != "" {
		v, err := substrate.NewViewer(substrate.ViewerOptions{Addr: subAddr})
		if err != nil {
			log.Printf("warning: initializing substrate viewer (%s): %v", subAddr, err)
		} else {
			subViewer = v
			defer func() {
				_ = subViewer.Close()
			}()
		}
	}

	if *allInOne {
		var err error
		sf, err = storefront.NewRuntime(storefront.Config{
			GitWorkDir: os.Getenv("STOREFRONT_GIT_DIR"),
		})
		if err != nil {
			log.Fatalf("initializing Retail Storefront runtime: %v", err)
		}
		sfSrv := &http.Server{
			Addr:              *storefrontAddr,
			Handler:           sf.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("Retail Storefront (:3001) listening on http://%s", *storefrontAddr)
			if err := sfSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("storefront server error: %v", err)
			}
		}()
		defer func() {
			_ = sfSrv.Close()
		}()
	} else if axAddr != "" || gwURL != "" || sfURL != "" {
		var err error
		orch, err = bugbash.NewOrchestrator(ctx, bugbash.OrchestratorConfig{
			AXServerAddr:        axAddr,
			TaskRunnerImage:     firstEnv("AX_DEFAULT_TASK_IMAGE", "TASK_RUNNER_IMAGE"),
			Storefront:          sf,
			StorefrontGitURL:    os.Getenv("STOREFRONT_GIT_URL"),
			ObserverCallbackURL: os.Getenv("OBSERVER_CALLBACK_URL"),
			LokiURL:             os.Getenv("LOKI_URL"),
			ModelGateway:        gw,
			ModelGatewayURL:     gwURL,
			GatewayClient:       gwClient,
			SubstrateViewer:     subViewer,
			DefaultHarnessMode:  defaultHarness,
		})
		if err != nil {
			log.Fatalf("initializing Bug Bash Orchestrator: %v", err)
		}
	}

	obsSrv, err := observer.NewServer(ctx, observer.Config{
		Storefront:          sf,
		StorefrontURL:       sfURL,
		StorefrontPublicURL: sfPub,
		GrafanaPublicURL:    grafPub,
		ModelGateway:        gw,
		ModelGatewayURL:     gwURL,
		GatewayClient:       gwClient,
		SubstrateViewer:     subViewer,
		Orchestrator:        orch,
		AlertmanagerBridge:  ab,
		RolloutBridge:       rb,
		Scraper:             scr,
		DefaultHarness:      defaultHarness,
	})
	if err != nil {
		log.Fatalf("initializing AX Observer server: %v", err)
	}
	defer obsSrv.Close()

	if *allInOne && obsSrv.Scraper() != nil {
		monSrv := &http.Server{
			Addr:              *monitoringAddr,
			Handler:           obsSrv.Scraper().Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Printf("Monitoring & Alerting Stack (:3002) listening on http://%s/monitoring", *monitoringAddr)
			if err := monSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("monitoring server error: %v", err)
			}
		}()
		defer func() {
			_ = monSrv.Close()
		}()
	}

	httpSrv := &http.Server{
		Addr:              *observerAddr,
		Handler:           obsSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("AX Observer (:3000) listening on http://%s (Storefront: %s, Grafana: %s, Mode: %s)", *observerAddr, sfPub, grafPub, obsSrv.ExecutionMode())
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("AX Observer server error: %v", err)
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
