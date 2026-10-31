package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func main() {
	defaultAddr := os.Getenv("STOREFRONT_ADDR")
	if defaultAddr == "" {
		port := os.Getenv("PORT")
		if port != "" {
			defaultAddr = "127.0.0.1:" + port
		} else {
			defaultAddr = "127.0.0.1:3001"
		}
	}
	addr := flag.String("addr", defaultAddr, "HTTP listen address for Retail Storefront")
	gitDir := flag.String("git-dir", os.Getenv("STOREFRONT_GIT_DIR"), "Directory for in-cluster Git repository")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := storefront.NewRuntime(storefront.Config{
		GitWorkDir: *gitDir,
	})
	if err != nil {
		log.Fatalf("failed to initialize Retail Storefront runtime: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           rt.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("Retail Storefront listening on %s (git repo: %s)", *addr, rt.GitManager().RepoDir())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
