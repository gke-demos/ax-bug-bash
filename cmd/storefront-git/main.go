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
)

func main() {
	defaultAddr := httputil.EnvOr("STOREFRONT_GIT_ADDR", httputil.EnvOr("LISTEN_ADDR", ""))
	if defaultAddr == "" {
		if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
			defaultAddr = "127.0.0.1:" + port
		} else {
			defaultAddr = "127.0.0.1:8090"
		}
	}
	defaultEnableDaemon := strings.EqualFold(strings.TrimSpace(os.Getenv("ENABLE_GIT_DAEMON")), "true") ||
		strings.TrimSpace(os.Getenv("ENABLE_GIT_DAEMON")) == "1"

	addr := flag.String("addr", defaultAddr, "HTTP listen address for canonical storefront-git Smart HTTP server")
	gitDir := flag.String("git-dir", os.Getenv("STOREFRONT_GIT_DIR"), "Root directory for canonical storefront.git repository")
	enableDaemon := flag.Bool("enable-git-daemon", defaultEnableDaemon, "Also run read-only git-daemon on --git-daemon-addr")
	daemonAddr := flag.String("git-daemon-addr", httputil.EnvOr("GIT_DAEMON_ADDR", "127.0.0.1:9418"), "Listen address for git-daemon")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gitSrv, err := storefront.NewGitServer(storefront.GitServerConfig{
		GitWorkDir: *gitDir,
	})
	if err != nil {
		log.Fatalf("failed to initialize storefront-git server: %v", err)
	}
	defer func() { _ = gitSrv.Close() }()

	if *enableDaemon {
		if err := gitSrv.StartGitDaemon(ctx, *daemonAddr); err != nil {
			log.Fatalf("failed to start git-daemon on %s: %v", *daemonAddr, err)
		}
		log.Printf("storefront-git git-daemon listening on %s", *daemonAddr)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           gitSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("storefront-git Smart HTTP listening on %s (repo: %s, baseline: %s@%s)",
		*addr,
		gitSrv.GitManager().RepoDir(),
		storefront.BaselineGitTag,
		gitSrv.GitManager().BaselineCommitSHA(),
	)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("storefront-git HTTP server error: %v", err)
	}
}
