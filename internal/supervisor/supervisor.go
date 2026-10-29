package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	axhttputil "github.com/gke-demos/ax-bug-bash/internal/httputil"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

// Config configures a PID-1 Polyglot Hot-Reload Supervisor instance.
type Config struct {
	Service        string
	GitRepoURL     string
	WorkDir        string
	LokiPushURL    string
	ChildEnv       []string
	StartupTimeout time.Duration
	GitRetryWindow time.Duration
}

type runningChild struct {
	commitSHA string
	port      int
	stageDir  string
	cmd       *exec.Cmd
	doneCh    chan struct{}
	proxy     *httputil.ReverseProxy
	inFlight  sync.WaitGroup
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() < 16<<10 {
		return b.buf.Write(p)
	}
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Supervisor owns the public HTTP listener and persistent Prometheus metrics for one microservice,
// compiling staged commits and atomically swapping loopback child processes with zero downtime.
type Supervisor struct {
	cfg        Config
	service    string
	language   string
	workDir    string
	ownsDir    bool
	gitBin     string
	repoCache  string
	reloadMu   sync.Mutex
	cleanupWG  sync.WaitGroup
	closedFlag atomic.Bool

	activeChild          atomic.Pointer[runningChild]
	stageSeq             atomic.Uint64
	reloadCount          atomic.Uint64
	http200Total         atomic.Uint64
	http500Total         atomic.Uint64
	lastReloadDurationMs atomic.Int64

	nodeStripOnce      sync.Once
	nodeSupportsStrip  bool
}

// New validates cfg, clones and builds the initial baseline revision, starts the loopback child process,
// and returns a ready Supervisor once /healthz responds 200 OK.
func New(cfg Config) (*Supervisor, error) {
	svc := strings.TrimSpace(cfg.Service)
	if !isAllowedService(svc) {
		return nil, fmt.Errorf("disallowed service name %q (must be one of %v)", svc, storefront.AllServices())
	}
	gitURL := strings.TrimSpace(cfg.GitRepoURL)
	if gitURL == "" {
		return nil, errors.New("GitRepoURL is required")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("locating git binary: %w", err)
	}

	ownsDir := false
	workDir := strings.TrimSpace(cfg.WorkDir)
	if workDir == "" {
		tmp, mErr := os.MkdirTemp("", "ax-supervisor-"+svc+"-")
		if mErr != nil {
			return nil, fmt.Errorf("creating supervisor workdir: %w", mErr)
		}
		workDir = tmp
		ownsDir = true
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir supervisor workdir: %w", err)
	}

	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 25 * time.Second
	}

	s := &Supervisor{
		cfg:       cfg,
		service:   svc,
		language:  storefront.ServiceLanguage(svc),
		workDir:   workDir,
		ownsDir:   ownsDir,
		gitBin:    gitBin,
		repoCache: filepath.Join(workDir, "repo-cache"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.StartupTimeout+cfg.GitRetryWindow+10*time.Second)
	defer cancel()
	if _, err := s.Reload(ctx, "refs/heads/main"); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("initial boot for %s failed: %w", svc, err)
	}
	// Initial boot is baseline startup, reset reload count/duration to 0 until first hot-reload.
	s.reloadCount.Store(0)
	s.lastReloadDurationMs.Store(0)
	return s, nil
}

func isAllowedService(svc string) bool {
	for _, allowed := range storefront.AllServices() {
		if svc == allowed {
			return true
		}
	}
	return false
}

// CurrentCommitSHA returns the commit SHA currently served by the active child process.
func (s *Supervisor) CurrentCommitSHA() string {
	ch := s.activeChild.Load()
	if ch == nil {
		return ""
	}
	return ch.commitSHA
}

// Reload stages targetRef from GitRepoURL, compiles/validates the service against warm caches,
// boots a new loopback child process, verifies /healthz 200 OK, and atomically swaps traffic.
// If staging, compilation, or /healthz fails, the prior healthy child continues serving uninterrupted.
func (s *Supervisor) Reload(ctx context.Context, targetRef string) (int64, error) {
	if s.closedFlag.Load() {
		return 0, errors.New("supervisor is closed")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	start := time.Now()
	commitSHA, stageDir, err := s.stageRevision(ctx, targetRef)
	if err != nil {
		return 0, err
	}

	port, err := allocateLoopbackPort()
	if err != nil {
		_ = os.RemoveAll(stageDir)
		return 0, fmt.Errorf("allocating loopback port: %w", err)
	}

	cmd, err := s.buildAndPrepareChildCommand(ctx, stageDir, commitSHA, port)
	if err != nil {
		_ = os.RemoveAll(stageDir)
		return 0, err
	}

	errCapture := &syncBuffer{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, errCapture)

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(stageDir)
		return 0, fmt.Errorf("starting child for %s: %w", s.service, err)
	}

	doneCh := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(doneCh)
	}()

	if err := s.waitForChildHealth(ctx, port, doneCh, errCapture); err != nil {
		terminateCmd(cmd, doneCh)
		_ = os.RemoveAll(stageDir)
		return 0, err
	}

	targetURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := s.buildReverseProxy(targetURL)

	candidate := &runningChild{
		commitSHA: commitSHA,
		port:      port,
		stageDir:  stageDir,
		cmd:       cmd,
		doneCh:    doneCh,
		proxy:     proxy,
	}

	prev := s.activeChild.Swap(candidate)
	durMs := time.Since(start).Milliseconds()
	if durMs <= 0 {
		durMs = 1
	}
	s.lastReloadDurationMs.Store(durMs)
	s.reloadCount.Add(1)

	if prev != nil {
		s.cleanupWG.Add(1)
		go func(old *runningChild) {
			defer s.cleanupWG.Done()
			// Wait for all in-flight HTTP requests already dispatched to old.proxy (including git-receive-pack) to finish cleanly.
			drained := make(chan struct{})
			go func() {
				old.inFlight.Wait()
				close(drained)
			}()
			select {
			case <-drained:
			case <-old.doneCh:
			case <-time.After(5 * time.Second):
			}
			terminateCmd(old.cmd, old.doneCh)
			_ = os.RemoveAll(old.stageDir)
		}(prev)
	}

	return durMs, nil
}

func (s *Supervisor) stageRevision(ctx context.Context, targetRef string) (string, string, error) {
	if _, err := os.Stat(filepath.Join(s.repoCache, ".git")); err != nil {
		deadline := time.Now().Add(s.cfg.GitRetryWindow)
		for {
			_ = os.RemoveAll(s.repoCache)
			cloneCmd := exec.CommandContext(ctx, s.gitBin, "clone", s.cfg.GitRepoURL, s.repoCache)
			out, cErr := cloneCmd.CombinedOutput()
			if cErr == nil {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return "", "", fmt.Errorf("cloning %s failed: %w (%s)", s.cfg.GitRepoURL, cErr, strings.TrimSpace(string(out)))
			}
			time.Sleep(250 * time.Millisecond)
		}
	} else {
		fetchCmd := exec.CommandContext(ctx, s.gitBin, "-C", s.repoCache, "fetch", "--tags", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*")
		if out, fErr := fetchCmd.CombinedOutput(); fErr != nil {
			return "", "", fmt.Errorf("git fetch failed: %w (%s)", fErr, strings.TrimSpace(string(out)))
		}
	}

	ref := strings.TrimSpace(targetRef)
	resolveArg := ref
	if ref == "" || ref == "HEAD" || ref == "main" || ref == "refs/heads/main" {
		resolveArg = "refs/remotes/origin/main^{commit}"
	} else if strings.HasPrefix(ref, "refs/heads/") {
		resolveArg = "refs/remotes/origin/" + strings.TrimPrefix(ref, "refs/heads/") + "^{commit}"
	} else {
		resolveArg = ref + "^{commit}"
	}

	shaOut, err := exec.CommandContext(ctx, s.gitBin, "-C", s.repoCache, "rev-parse", "--verify", resolveArg).CombinedOutput()
	if err != nil && ref != "" {
		// Try explicit fetch by SHA/ref if not under refs/heads/*
		_, _ = exec.CommandContext(ctx, s.gitBin, "-C", s.repoCache, "fetch", "origin", ref).CombinedOutput()
		shaOut, err = exec.CommandContext(ctx, s.gitBin, "-C", s.repoCache, "rev-parse", "--verify", ref+"^{commit}").CombinedOutput()
	}
	if err != nil {
		return "", "", fmt.Errorf("resolving commit %q failed: %w (%s)", targetRef, err, strings.TrimSpace(string(shaOut)))
	}
	commitSHA := strings.TrimSpace(string(shaOut))
	shortSHA := commitSHA
	if len(shortSHA) > 8 {
		shortSHA = shortSHA[:8]
	}

	seq := s.stageSeq.Add(1)
	stageDir := filepath.Join(s.workDir, fmt.Sprintf("stage-%d-%s", seq, shortSHA))
	if out, cErr := exec.CommandContext(ctx, s.gitBin, "clone", "--shared", s.repoCache, stageDir).CombinedOutput(); cErr != nil {
		return "", "", fmt.Errorf("staging shared clone failed: %w (%s)", cErr, strings.TrimSpace(string(out)))
	}
	if out, coErr := exec.CommandContext(ctx, s.gitBin, "-C", stageDir, "checkout", "--detach", commitSHA).CombinedOutput(); coErr != nil {
		_ = os.RemoveAll(stageDir)
		return "", "", fmt.Errorf("staging checkout %s failed: %w (%s)", commitSHA, coErr, strings.TrimSpace(string(out)))
	}
	return commitSHA, stageDir, nil
}

func (s *Supervisor) buildAndPrepareChildCommand(ctx context.Context, stageDir, commitSHA string, port int) (*exec.Cmd, error) {
	baseEnv := s.childEnvironment(commitSHA, port)
	binDir := filepath.Join(stageDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating stage bin dir: %w", err)
	}

	switch s.service {
	case storefront.ServiceCheckout, storefront.ServiceShipping:
		outBin := filepath.Join(binDir, s.service)
		pkg := "./services/" + s.service + "/cmd/server"
		buildCmd := exec.CommandContext(ctx, "go", "build", "-o", outBin, pkg)
		buildCmd.Dir = stageDir
		buildCmd.Env = baseEnv
		if out, err := buildCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("go build %s failed: %w (%s)", pkg, err, strings.TrimSpace(string(out)))
		}
		cmd := exec.Command(outBin)
		cmd.Dir = filepath.Join(stageDir, "services", s.service)
		cmd.Env = baseEnv
		return cmd, nil

	case storefront.ServicePricing:
		pyBin := resolvePythonBin()
		svcDir := filepath.Join(stageDir, "services", storefront.ServicePricing)
		checkCmd := exec.CommandContext(ctx, pyBin, "-m", "py_compile", "pricing.py", "app.py")
		checkCmd.Dir = svcDir
		checkCmd.Env = baseEnv
		if out, err := checkCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("python syntax check failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		cmd := exec.Command(pyBin, "app.py")
		cmd.Dir = svcDir
		cmd.Env = baseEnv
		return cmd, nil

	case storefront.ServiceInventory:
		svcDir := filepath.Join(stageDir, "services", storefront.ServiceInventory)
		if cargoBin, ok := resolveCargoBin(); ok {
			targetDir := strings.TrimSpace(os.Getenv("CARGO_TARGET_DIR"))
			if targetDir == "" {
				targetDir = filepath.Join(s.workDir, "cargo-target")
			}
			cargoEnv := append(append([]string(nil), baseEnv...), "CARGO_TARGET_DIR="+targetDir)
			manifestPath := filepath.Join(svcDir, "server", "Cargo.toml")
			args := []string{"build", "--release", "--manifest-path", manifestPath}
			if strings.EqualFold(strings.TrimSpace(os.Getenv("AX_CARGO_OFFLINE")), "true") {
				args = append(args, "--offline")
			}
			buildCmd := exec.CommandContext(ctx, cargoBin, args...)
			buildCmd.Dir = stageDir
			buildCmd.Env = cargoEnv
			if out, err := buildCmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("cargo build inventory-http-server failed: %w (%s)", err, strings.TrimSpace(string(out)))
			}
			builtBin := filepath.Join(targetDir, "release", "inventory-http-server")
			stagedBin := filepath.Join(binDir, "inventory-http-server")
			if err := copyExecutable(builtBin, stagedBin); err != nil {
				return nil, fmt.Errorf("staging inventory-http-server binary: %w", err)
			}
			cmd := exec.Command(stagedBin)
			cmd.Dir = svcDir
			cmd.Env = cargoEnv
			return cmd, nil
		}

		// Workstation fallback when cargo/rustc toolchain is not installed on host.
		pyBin := resolvePythonBin()
		checkCmd := exec.CommandContext(ctx, pyBin, "fallback_server.py", "--check")
		checkCmd.Dir = svcDir
		checkCmd.Env = baseEnv
		if out, err := checkCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("inventory-service source check failed: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		cmd := exec.Command(pyBin, "fallback_server.py")
		cmd.Dir = svcDir
		cmd.Env = baseEnv
		return cmd, nil

	case storefront.ServiceStorefrontWeb:
		nodeBin, err := exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("locating node binary: %w", err)
		}
		svcDir := filepath.Join(stageDir, "services", storefront.ServiceStorefrontWeb)
		var args []string
		if s.supportsNodeStripTypes(nodeBin) {
			args = []string{"--experimental-strip-types", "src/http_server.ts"}
		} else {
			args = []string{"--import", "./test/ts-loader.mjs", "src/start.mjs"}
		}
		cmd := exec.Command(nodeBin, args...)
		cmd.Dir = svcDir
		cmd.Env = baseEnv
		return cmd, nil

	default:
		return nil, fmt.Errorf("unsupported service %q", s.service)
	}
}

func (s *Supervisor) supportsNodeStripTypes(nodeBin string) bool {
	s.nodeStripOnce.Do(func() {
		probeFile := filepath.Join(s.workDir, "ts-strip-probe.ts")
		if err := os.WriteFile(probeFile, []byte("const x: number = 1;\nvoid x;\n"), 0o644); err != nil {
			return
		}
		defer func() { _ = os.Remove(probeFile) }()
		cmd := exec.Command(nodeBin, "--experimental-strip-types", probeFile)
		if err := cmd.Run(); err == nil {
			s.nodeSupportsStrip = true
		}
	})
	return s.nodeSupportsStrip
}

func (s *Supervisor) childEnvironment(commitSHA string, port int) []string {
	env := axhttputil.EnsureGoBinOnPath(os.Environ())
	curPath := os.Getenv("PATH")
	homeDir := os.Getenv("HOME")
	extraPath := "/usr/local/cargo/bin:" + filepath.Join(homeDir, ".cargo", "bin") + ":" + filepath.Join(homeDir, ".local", "go", "bin") + ":/usr/local/go/bin:" + curPath
	env = append(env, s.cfg.ChildEnv...)
	env = append(env,
		"PATH="+extraPath,
		fmt.Sprintf("PORT=%d", port),
		fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", port),
		"SERVICE_NAME="+s.service,
		"COMMIT_SHA="+commitSHA,
		"RUST_BACKTRACE=1",
		"PYTHONUNBUFFERED=1",
	)
	if strings.TrimSpace(s.cfg.LokiPushURL) != "" {
		env = append(env, "LOKI_PUSH_URL="+strings.TrimSpace(s.cfg.LokiPushURL))
	}
	return env
}

func (s *Supervisor) waitForChildHealth(ctx context.Context, port int, doneCh <-chan struct{}, stderrBuf *syncBuffer) error {
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(s.cfg.StartupTimeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s /healthz cancelled: %w (stderr: %s)", s.service, ctx.Err(), strings.TrimSpace(stderrBuf.String()))
		case <-doneCh:
			return fmt.Errorf("child process for %s exited before /healthz passed (stderr: %s)", s.service, strings.TrimSpace(stderrBuf.String()))
		default:
		}

		resp, err := client.Get(healthURL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s /healthz on 127.0.0.1:%d (stderr: %s)", s.service, port, strings.TrimSpace(stderrBuf.String()))
}

func (s *Supervisor) buildReverseProxy(targetURL *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.ModifyResponse = func(resp *http.Response) error {
		upstreamOnly := strings.EqualFold(strings.TrimSpace(resp.Header.Get("X-Storefront-Upstream-Error-Only")), "true")
		resp.Header.Del("X-Storefront-Upstream-Error-Only")

		reqPath := ""
		if resp.Request != nil && resp.Request.URL != nil {
			reqPath = resp.Request.URL.Path
		}
		if isControlOrAssetPath(reqPath) {
			if resp.StatusCode < 500 {
				s.http200Total.Add(1)
			}
			return nil
		}
		if resp.StatusCode >= 500 && !upstreamOnly {
			s.http500Total.Add(1)
		} else {
			s.http200Total.Add(1)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if !isControlOrAssetPath(r.URL.Path) {
			s.http500Total.Add(1)
		}
		axhttputil.WriteJSON(w, http.StatusBadGateway, map[string]string{
			"status":  "BAD_GATEWAY",
			"service": s.service,
			"error":   err.Error(),
		})
	}
	return proxy
}

func isControlOrAssetPath(p string) bool {
	return strings.HasPrefix(p, "/git/") ||
		strings.HasPrefix(p, "/api/incidents") ||
		strings.HasPrefix(p, "/static/") ||
		p == "/favicon.ico"
}

// Handler returns the public HTTP handler owned by the PID-1 Supervisor.
// TODO(security): Internal cluster microservice mesh; Kubernetes NetworkPolicy restricts /internal/reload ingress to storefront-git.
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/internal/status", s.handleInternalStatus)
	mux.HandleFunc("/internal/reload", s.handleInternalReload)
	mux.HandleFunc("/", s.handleProxy)
	return axhttputil.WithSecurityHeaders(mux)
}

func (s *Supervisor) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	commitSHA := s.CurrentCommitSHA()
	var sb strings.Builder
	sb.WriteString("# HELP storefront_http_requests_total Total HTTP requests processed by storefront microservice and status.\n")
	sb.WriteString("# TYPE storefront_http_requests_total counter\n")
	sb.WriteString(fmt.Sprintf("storefront_http_requests_total{status=\"200\",service=%q} %d\n", s.service, s.http200Total.Load()))
	sb.WriteString(fmt.Sprintf("storefront_http_requests_total{status=\"500\",service=%q} %d\n", s.service, s.http500Total.Load()))
	sb.WriteString("# HELP storefront_service_info Metadata gauge for the active microservice process.\n")
	sb.WriteString("# TYPE storefront_service_info gauge\n")
	sb.WriteString(fmt.Sprintf("storefront_service_info{service=%q,language=%q,commit_sha=%q} 1\n", s.service, s.language, commitSHA))
	sb.WriteString("# HELP storefront_hot_reload_duration_milliseconds Duration of the last atomic hot-reload in milliseconds.\n")
	sb.WriteString("# TYPE storefront_hot_reload_duration_milliseconds gauge\n")
	sb.WriteString(fmt.Sprintf("storefront_hot_reload_duration_milliseconds{service=%q} %d\n", s.service, s.lastReloadDurationMs.Load()))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Supervisor) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ch := s.activeChild.Load()
	if ch == nil {
		axhttputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting", "service": s.service})
		return
	}
	select {
	case <-ch.doneCh:
		axhttputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "service": s.service})
		return
	default:
	}
	axhttputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"service":    s.service,
		"commit_sha": ch.commitSHA,
		"child_port": ch.port,
	})
}

func (s *Supervisor) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ch := s.activeChild.Load()
	if ch == nil {
		axhttputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "service": s.service})
		return
	}
	axhttputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":     "ready",
		"service":    s.service,
		"commit_sha": ch.commitSHA,
	})
}

func (s *Supervisor) handleInternalStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	axhttputil.WriteJSON(w, http.StatusOK, map[string]any{
		"service":                 s.service,
		"language":                s.language,
		"commit_sha":              s.CurrentCommitSHA(),
		"http_200_total":          s.http200Total.Load(),
		"http_500_total":          s.http500Total.Load(),
		"last_reload_duration_ms": s.lastReloadDurationMs.Load(),
		"reload_count":            s.reloadCount.Load(),
	})
}

func (s *Supervisor) handleInternalReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Service   string `json:"service"`
		CommitSHA string `json:"commit_sha"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	if req.Service != "" && req.Service != s.service {
		axhttputil.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("reload target %q does not match supervisor service %q", req.Service, s.service),
		})
		return
	}

	durMs, err := s.Reload(r.Context(), req.CommitSHA)
	if err != nil {
		axhttputil.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"status":            "reload_failed",
			"service":           s.service,
			"active_commit_sha": s.CurrentCommitSHA(),
			"error":             err.Error(),
		})
		return
	}

	axhttputil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":      "reloaded",
		"service":     s.service,
		"commit_sha":  s.CurrentCommitSHA(),
		"duration_ms": durMs,
	})
}

func (s *Supervisor) handleProxy(w http.ResponseWriter, r *http.Request) {
	ch := s.activeChild.Load()
	if ch == nil {
		s.http500Total.Add(1)
		axhttputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":  "UNAVAILABLE",
			"service": s.service,
		})
		return
	}
	ch.inFlight.Add(1)
	defer ch.inFlight.Done()
	ch.proxy.ServeHTTP(w, r)
}

// Close terminates the active child process, waits for any in-flight drain goroutines, and cleans up owned scratch files.
func (s *Supervisor) Close() error {
	if !s.closedFlag.CompareAndSwap(false, true) {
		return nil
	}
	ch := s.activeChild.Swap(nil)
	if ch != nil {
		terminateCmd(ch.cmd, ch.doneCh)
		_ = os.RemoveAll(ch.stageDir)
	}
	s.cleanupWG.Wait()
	if s.ownsDir {
		_ = os.RemoveAll(s.workDir)
	}
	return nil
}

func allocateLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func terminateCmd(cmd *exec.Cmd, doneCh <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-doneCh:
		return
	case <-time.After(400 * time.Millisecond):
		_ = cmd.Process.Kill()
		<-doneCh
	}
}

func resolvePythonBin() string {
	if v := strings.TrimSpace(os.Getenv("AX_PYTHON_BIN")); v != "" {
		return v
	}
	if _, err := os.Stat("/opt/ax-venv/bin/python3"); err == nil {
		return "/opt/ax-venv/bin/python3"
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return "python"
}

func resolveCargoBin() (string, bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("AX_FORCE_RUST_FALLBACK")), "true") {
		return "", false
	}
	candidates := []string{
		"/usr/local/cargo/bin/cargo",
		filepath.Join(os.Getenv("HOME"), ".cargo", "bin", "cargo"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, true
		}
	}
	if p, err := exec.LookPath("cargo"); err == nil {
		return p, true
	}
	return "", false
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
