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
	"github.com/gke-demos/ax-bug-bash/internal/monitoring"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func main() {
	observerAddr := flag.String("addr", envOr("LISTEN_ADDR", "127.0.0.1:3000"), "HTTP listen address for AX Observer (:3000)")
	storefrontAddr := flag.String("storefront-addr", envOr("STOREFRONT_LISTEN_ADDR", "127.0.0.1:3001"), "HTTP listen address for Retail Storefront in all-in-one mode (:3001)")
	monitoringAddr := flag.String("monitoring-addr", envOr("MONITORING_LISTEN_ADDR", "127.0.0.1:3002"), "HTTP listen address for Monitoring & Alertmanager surface in all-in-one mode (:3002)")
	allInOne := flag.Bool("all-in-one", envOr("ALL_IN_ONE", "true") != "false" && os.Getenv("STOREFRONT_URL") == "", "Start Storefront (:3001), Monitoring (:3002), and AX Observer (:3000) in a single process")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		sf      *storefront.Runtime
		orch    *bugbash.Orchestrator
		gw      *gateway.Server
		ab      *monitoring.AlertmanagerBridge
		rb      *monitoring.RolloutBridge
		scr     *monitoring.Scraper
		sfURL   = strings.TrimSpace(os.Getenv("STOREFRONT_URL"))
		gwURL   = strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
		axAddr  = strings.TrimSpace(os.Getenv("AX_SERVER_ADDR"))
		sfPub   = envOr("STOREFRONT_PUBLIC_URL", "http://localhost:3001")
		grafPub = envOr("GRAFANA_PUBLIC_URL", "http://localhost:3002/monitoring")
	)

	gw = gateway.NewServer(gateway.DefaultConfig(), nil)

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
			AXServerAddr:     axAddr,
			Storefront:       sf,
			StorefrontGitURL: os.Getenv("STOREFRONT_GIT_URL"),
			ModelGateway:     gw,
			ModelGatewayURL:  gwURL,
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
		Orchestrator:        orch,
		AlertmanagerBridge:  ab,
		RolloutBridge:       rb,
		Scraper:             scr,
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
		_ = httpSrv.Close()
	}()

	log.Printf("AX Observer (:3000) listening on http://%s (Storefront: %s, Grafana: %s)", *observerAddr, sfPub, grafPub)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("AX Observer server error: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
