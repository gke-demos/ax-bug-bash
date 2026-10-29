package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	defaultMesh := strings.EqualFold(strings.TrimSpace(os.Getenv("POLYGLOT_MESH_ALL")), "true")
	meshFlag := flag.Bool("mesh", defaultMesh, "Run canonical storefront-git (:8090/:9418) and all 5 polyglot service supervisors (:3001, :8081..:8084) in one container")
	svcFlag := flag.String("service", httputil.EnvOr("SERVICE_NAME", ""), "Storefront microservice to supervise (storefront-web, checkout-service, shipping-service, pricing-service, inventory-service)")
	addrFlag := flag.String("addr", httputil.EnvOr("SUPERVISOR_ADDR", ""), "Public HTTP listen address (defaults to 127.0.0.1:<service-port> or 127.0.0.1:$PORT)")
	gitURLFlag := flag.String("git-url", httputil.EnvOr("STOREFRONT_GIT_URL", httputil.EnvOr("GIT_REPO_URL", "http://storefront-git:8090/git/storefront.git")), "Canonical storefront.git clone URL")
	workDirFlag := flag.String("work-dir", os.Getenv("SUPERVISOR_WORK_DIR"), "Scratch directory for staged checkouts and incremental builds")
	servicesDirFlag := flag.String("services-dir", os.Getenv("STOREFRONT_SERVICES_DIR"), "Path to canonical services/ directory for seeding storefront.git")
	gitWorkDirFlag := flag.String("git-work-dir", httputil.EnvOr("STOREFRONT_GIT_DIR", "/tmp/ax-bug-bash-storefront-git"), "Directory for canonical storefront.git repository in --mesh mode")
	gitAddrFlag := flag.String("git-addr", httputil.EnvOr("STOREFRONT_GIT_ADDR", "0.0.0.0:8090"), "HTTP listen address for storefront-git in --mesh mode")
	gitDaemonAddrFlag := flag.String("git-daemon-addr", httputil.EnvOr("STOREFRONT_GIT_DAEMON_ADDR", "0.0.0.0:9418"), "git-daemon listen address in --mesh mode")
	lokiURLFlag := flag.String("loki-push-url", os.Getenv("LOKI_PUSH_URL"), "Optional Loki /loki/api/v1/push endpoint propagated to child process")
	flag.Parse()

	if strings.TrimSpace(*servicesDirFlag) != "" {
		_ = os.Setenv("STOREFRONT_SERVICES_DIR", strings.TrimSpace(*servicesDirFlag))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	svc := strings.TrimSpace(*svcFlag)
	if *meshFlag || (svc == "" && defaultMesh) {
		if err := runPolyglotMesh(ctx, meshConfig{
			BaseWorkDir:   strings.TrimSpace(*workDirFlag),
			GitWorkDir:    strings.TrimSpace(*gitWorkDirFlag),
			GitAddr:       strings.TrimSpace(*gitAddrFlag),
			GitDaemonAddr: strings.TrimSpace(*gitDaemonAddrFlag),
			LokiPushURL:   strings.TrimSpace(*lokiURLFlag),
			BindHost:      httputil.EnvOr("MESH_BIND_HOST", "0.0.0.0"),
		}); err != nil {
			log.Fatalf("polyglot-supervisor (--mesh) failed: %v", err)
		}
		return
	}

	if svc == "" {
		log.Fatalf("--service (or SERVICE_NAME or --mesh) is required; must be one of %v", storefront.AllServices())
	}

	listenAddr := strings.TrimSpace(*addrFlag)
	if listenAddr == "" {
		port := strings.TrimSpace(os.Getenv("PORT"))
		if port == "" {
			port = defaultPortForService(svc)
		}
		listenAddr = "127.0.0.1:" + port
	}

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

type meshConfig struct {
	BaseWorkDir   string
	GitWorkDir    string
	GitAddr       string
	GitDaemonAddr string
	LokiPushURL   string
	BindHost      string
}

func runPolyglotMesh(ctx context.Context, cfg meshConfig) error {
	bindHost := strings.TrimSpace(cfg.BindHost)
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	gitAddr := strings.TrimSpace(cfg.GitAddr)
	if gitAddr == "" {
		gitAddr = bindHost + ":8090"
	}

	supervisorURLs := make(map[string]string, len(storefront.AllServices()))
	for _, svc := range storefront.AllServices() {
		supervisorURLs[svc] = "http://127.0.0.1:" + defaultPortForService(svc)
	}

	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir:     cfg.GitWorkDir,
		SupervisorURLs: supervisorURLs,
	})
	if err != nil {
		return fmt.Errorf("initializing storefront-git server: %w", err)
	}
	defer func() { _ = gitSrv.Close() }()

	if strings.TrimSpace(cfg.GitDaemonAddr) != "" {
		if err := gitSrv.StartGitDaemon(ctx, cfg.GitDaemonAddr); err != nil {
			log.Printf("warning: git-daemon startup on %s failed: %v", cfg.GitDaemonAddr, err)
		} else {
			log.Printf("storefront-git daemon listening on git://%s/storefront.git", cfg.GitDaemonAddr)
		}
	}

	gitLis, err := net.Listen("tcp", gitAddr)
	if err != nil {
		return fmt.Errorf("listening on git-addr %s: %w", gitAddr, err)
	}
	_, gitPort, _ := net.SplitHostPort(gitLis.Addr().String())
	localGitBaseURL := "http://127.0.0.1:" + gitPort
	localGitRepoURL := localGitBaseURL + "/git/storefront.git"

	var webSupMu sync.RWMutex
	var webSup *supervisor.Supervisor

	gitHandler := gitSrv.Handler()
	gitHTTPServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" {
				webSupMu.RLock()
				ws := webSup
				webSupMu.RUnlock()
				if ws != nil {
					ws.Handler().ServeHTTP(w, r)
					return
				}
			}
			gitHandler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = gitHTTPServer.Serve(gitLis)
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gitHTTPServer.Shutdown(shutCtx)
	}()

	log.Printf("storefront-git HTTP server listening on %s (repo: %s, commit: %s)", gitLis.Addr().String(), gitSrv.GitManager().RepoDir(), gitSrv.GitManager().CurrentCommitSHA("refs/heads/main"))

	baseWorkDir := cfg.BaseWorkDir
	if baseWorkDir == "" {
		baseWorkDir = "/tmp/ax-bug-bash-mesh-work"
	}

	// Start backend services first (pricing, inventory, shipping, checkout), then storefront-web
	orderedServices := []string{
		storefront.ServicePricing,
		storefront.ServiceInventory,
		storefront.ServiceShipping,
		storefront.ServiceCheckout,
		storefront.ServiceStorefrontWeb,
	}

	var (
		supervisors []*supervisor.Supervisor
		httpServers []*http.Server
	)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range httpServers {
			_ = srv.Shutdown(shutCtx)
		}
		for _, sup := range supervisors {
			_ = sup.Close()
		}
	}()

	for _, svc := range orderedServices {
		svcPort := defaultPortForService(svc)
		svcListenAddr := net.JoinHostPort(bindHost, svcPort)
		svcWorkDir := filepath.Join(baseWorkDir, svc)

		var childEnv []string
		if svc == storefront.ServiceStorefrontWeb {
			childEnv = []string{
				"PRICING_SERVICE_URL=" + httputil.EnvOr("PRICING_SERVICE_URL", "http://127.0.0.1:8083"),
				"INVENTORY_SERVICE_URL=" + httputil.EnvOr("INVENTORY_SERVICE_URL", "http://127.0.0.1:8084"),
				"SHIPPING_SERVICE_URL=" + httputil.EnvOr("SHIPPING_SERVICE_URL", "http://127.0.0.1:8082"),
				"CHECKOUT_SERVICE_URL=" + httputil.EnvOr("CHECKOUT_SERVICE_URL", "http://127.0.0.1:8081"),
				"STOREFRONT_GIT_HTTP_URL=" + localGitBaseURL,
			}
		}

		sup, err := supervisor.New(supervisor.Config{
			Service:        svc,
			GitRepoURL:     localGitRepoURL,
			WorkDir:        svcWorkDir,
			LokiPushURL:    cfg.LokiPushURL,
			ChildEnv:       childEnv,
			StartupTimeout: 45 * time.Second,
			GitRetryWindow: 30 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("starting supervisor for %s: %w", svc, err)
		}
		supervisors = append(supervisors, sup)
		if svc == storefront.ServiceStorefrontWeb {
			webSupMu.Lock()
			webSup = sup
			webSupMu.Unlock()
		}

		lis, err := net.Listen("tcp", svcListenAddr)
		if err != nil {
			return fmt.Errorf("listening on %s for %s: %w", svcListenAddr, svc, err)
		}
		httpSrv := &http.Server{
			Handler:           sup.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		httpServers = append(httpServers, httpSrv)
		go func(s string, l net.Listener, srv *http.Server) {
			if serveErr := srv.Serve(l); serveErr != nil && serveErr != http.ErrServerClosed {
				log.Printf("polyglot-supervisor (%s) serve error: %v", s, serveErr)
			}
		}(svc, lis, httpSrv)

		log.Printf("polyglot-supervisor (%s) listening on %s (commit: %s)", svc, lis.Addr().String(), sup.CurrentCommitSHA())
	}

	<-ctx.Done()
	return nil
}
