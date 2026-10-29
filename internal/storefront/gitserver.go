package storefront

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/httputil"
)

// GitResetResult describes the outcome of resetting storefront.git back to baseline-v1.
type GitResetResult struct {
	Status           string   `json:"status"`
	BaselineTag      string   `json:"baseline_tag"`
	CommitSHA        string   `json:"commit_sha"`
	PreviousMainSHA  string   `json:"previous_main_sha,omitempty"`
	ReloadedServices []string `json:"reloaded_services"`
	PrunedBranches   []string `json:"pruned_branches"`
	DurationMs       int64    `json:"duration_ms"`
}

// GitWorkspaceManager manages the in-process Smart HTTP Git repository seeding the 5-service monorepo.
type GitWorkspaceManager struct {
	mu                sync.RWMutex
	rootDir           string
	repoDir           string
	gitBin            string
	onPushHook        func(repoDir string)
	onMainUpdatedHook func(oldSHA, newSHA string, modifiedServices []string)
}

// NewGitWorkspaceManager initializes the storefront.git repository inside rootDir.
func NewGitWorkspaceManager(rootDir string, onPush func(repoDir string)) (*GitWorkspaceManager, error) {
	if rootDir == "" {
		tmp, err := os.MkdirTemp("", "ax-storefront-git-*")
		if err != nil {
			return nil, fmt.Errorf("creating temp git root: %w", err)
		}
		rootDir = tmp
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir git root: %w", err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("locating git binary: %w", err)
	}

	repoDir := filepath.Join(rootDir, "storefront.git")
	mgr := &GitWorkspaceManager{
		rootDir:    rootDir,
		repoDir:    repoDir,
		gitBin:     gitBin,
		onPushHook: onPush,
	}
	if err := mgr.ResetToBaseline(); err != nil {
		return nil, err
	}
	return mgr, nil
}

// SetOnMainUpdatedHook registers a callback invoked whenever refs/heads/main advances via Smart HTTP push.
func (m *GitWorkspaceManager) SetOnMainUpdatedHook(hook func(oldSHA, newSHA string, modifiedServices []string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onMainUpdatedHook = hook
}

// RootDir returns the parent directory containing storefront.git (used as base-path for git-http-backend and git-daemon).
func (m *GitWorkspaceManager) RootDir() string {
	return m.rootDir
}

// RepoDir returns the filesystem path to the seeded storefront.git working repository.
func (m *GitWorkspaceManager) RepoDir() string {
	return m.repoDir
}

// CurrentCommitSHA resolves ref (defaulting to refs/heads/main) to its commit SHA, or "" if unresolvable.
func (m *GitWorkspaceManager) CurrentCommitSHA(ref string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentCommitSHALocked(ref)
}

// BaselineCommitSHA returns the commit SHA pointed to by baseline-v1.
func (m *GitWorkspaceManager) BaselineCommitSHA() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentCommitSHALocked(BaselineGitTag + "^{commit}")
}

func (m *GitWorkspaceManager) currentCommitSHALocked(ref string) string {
	if strings.TrimSpace(ref) == "" {
		ref = "refs/heads/main"
	}
	cmd := exec.Command(m.gitBin, "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = m.repoDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ModifiedServicesBetween returns the sorted list of storefront services modified between oldRef and newRef.
func (m *GitWorkspaceManager) ModifiedServicesBetween(oldRef, newRef string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.modifiedServicesBetweenLocked(oldRef, newRef)
}

func (m *GitWorkspaceManager) modifiedServicesBetweenLocked(oldRef, newRef string) ([]string, error) {
	if oldRef == "" || newRef == "" || oldRef == newRef {
		return []string{}, nil
	}
	cmd := exec.Command(m.gitBin, "diff", "--name-only", oldRef+".."+newRef)
	cmd.Dir = m.repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s..%s failed: %w (%s)", oldRef, newRef, err, string(out))
	}

	knownServices := make(map[string]bool, 5)
	for _, svc := range AllServices() {
		knownServices[svc] = true
	}

	seen := make(map[string]bool)
	for _, rawLine := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "services/") {
			continue
		}
		parts := strings.Split(line, "/")
		if len(parts) >= 2 && knownServices[parts[1]] {
			seen[parts[1]] = true
		}
	}

	modified := make([]string, 0, len(seen))
	for svc := range seen {
		modified = append(modified, svc)
	}
	sort.Strings(modified)
	return modified, nil
}

// ResetToBaseline resets storefront.git to the baseline-v1 commit (or seeds the repository on first initialization).
func (m *GitWorkspaceManager) ResetToBaseline() error {
	_, err := m.ResetToBaselineReport()
	return err
}

// ResetToBaselineReport fast-resets refs/heads/main to baseline-v1, prunes refs/heads/fix/* branches,
// and reports which services changed between the prior main commit and baseline-v1.
func (m *GitWorkspaceManager) ResetToBaselineReport() (GitResetResult, error) {
	start := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	baselineSHA := m.currentCommitSHALocked(BaselineGitTag + "^{commit}")
	if baselineSHA == "" {
		if err := m.seedRepositoryLocked(); err != nil {
			return GitResetResult{}, err
		}
		seededSHA := m.currentCommitSHALocked("refs/heads/main")
		return GitResetResult{
			Status:           "reset",
			BaselineTag:      BaselineGitTag,
			CommitSHA:        seededSHA,
			ReloadedServices: []string{},
			PrunedBranches:   []string{},
			DurationMs:       time.Since(start).Milliseconds(),
		}, nil
	}

	oldMain := m.currentCommitSHALocked("refs/heads/main")
	modifiedSvcs, err := m.modifiedServicesBetweenLocked(oldMain, baselineSHA)
	if err != nil {
		// Fall back to full re-seed if history was corrupted.
		if seedErr := m.seedRepositoryLocked(); seedErr != nil {
			return GitResetResult{}, seedErr
		}
		seededSHA := m.currentCommitSHALocked("refs/heads/main")
		return GitResetResult{
			Status:           "reset",
			BaselineTag:      BaselineGitTag,
			CommitSHA:        seededSHA,
			PreviousMainSHA:  oldMain,
			ReloadedServices: AllServices(),
			PrunedBranches:   []string{},
			DurationMs:       time.Since(start).Milliseconds(),
		}, nil
	}

	resetCmds := [][]string{
		{"checkout", "-f", "-B", "main", BaselineGitTag},
		{"reset", "--hard", BaselineGitTag},
		{"clean", "-fdx"},
	}
	for _, args := range resetCmds {
		cmd := exec.Command(m.gitBin, args...)
		cmd.Dir = m.repoDir
		if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
			return GitResetResult{}, fmt.Errorf("git %v failed: %w (%s)", args, cmdErr, string(out))
		}
	}

	prunedBranches, err := m.pruneFixBranchesLocked()
	if err != nil {
		return GitResetResult{}, err
	}

	return GitResetResult{
		Status:           "reset",
		BaselineTag:      BaselineGitTag,
		CommitSHA:        baselineSHA,
		PreviousMainSHA:  oldMain,
		ReloadedServices: modifiedSvcs,
		PrunedBranches:   prunedBranches,
		DurationMs:       time.Since(start).Milliseconds(),
	}, nil
}

func (m *GitWorkspaceManager) pruneFixBranchesLocked() ([]string, error) {
	cmd := exec.Command(m.gitBin, "for-each-ref", "--format=%(refname)", "refs/heads/fix/")
	cmd.Dir = m.repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("listing refs/heads/fix/* failed: %w (%s)", err, string(out))
	}

	var refs []string
	for _, raw := range strings.Split(string(out), "\n") {
		ref := strings.TrimSpace(raw)
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)

	pruned := make([]string, 0, len(refs))
	for _, ref := range refs {
		delCmd := exec.Command(m.gitBin, "update-ref", "-d", ref)
		delCmd.Dir = m.repoDir
		if delOut, delErr := delCmd.CombinedOutput(); delErr != nil {
			return nil, fmt.Errorf("deleting branch %s failed: %w (%s)", ref, delErr, string(delOut))
		}
		pruned = append(pruned, ref)
	}
	return pruned, nil
}

func (m *GitWorkspaceManager) seedRepositoryLocked() error {
	if err := os.RemoveAll(m.repoDir); err != nil {
		return fmt.Errorf("removing existing repoDir: %w", err)
	}
	if err := os.MkdirAll(m.repoDir, 0o755); err != nil {
		return fmt.Errorf("creating repoDir: %w", err)
	}
	if err := SeedServicesTree(m.repoDir); err != nil {
		return fmt.Errorf("seeding services tree: %w", err)
	}
	readme := "# Storefront Polyglot Monorepo\n\n5-service retail storefront seeded for google/ax Workspace tasks.\n"
	if err := os.WriteFile(filepath.Join(m.repoDir, "README.md"), []byte(readme), 0o644); err != nil {
		return fmt.Errorf("writing README.md: %w", err)
	}

	cmds := [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "storefront-git@ax-bug-bash.local"},
		{"config", "user.name", "Storefront Git Server"},
		{"config", "commit.gpgsign", "false"},
		{"config", "tag.gpgSign", "false"},
		{"config", "http.receivepack", "true"},
		{"config", "receive.denyCurrentBranch", "updateInstead"},
		{"config", "uploadpack.allowTipSHA1InWant", "true"},
		{"config", "uploadpack.allowReachableSHA1InWant", "true"},
		{"add", "."},
		{"commit", "--no-gpg-sign", "-m", "chore: seed baseline 5-service polyglot storefront monorepo"},
		{"tag", "-f", "--no-sign", "-m", "baseline-v1 initial storefront state", BaselineGitTag, "main"},
	}
	for _, args := range cmds {
		cmd := exec.Command(m.gitBin, args...)
		cmd.Dir = m.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v failed: %w (%s)", args, err, string(out))
		}
	}

	exportOKPath := filepath.Join(m.repoDir, ".git", "git-daemon-export-ok")
	if err := os.WriteFile(exportOKPath, []byte("ok\n"), 0o644); err != nil {
		return fmt.Errorf("writing git-daemon-export-ok: %w", err)
	}
	return nil
}

// ServeHTTP serves Smart HTTP Git requests under /git/storefront.git/*.
func (m *GitWorkspaceManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathInfo := strings.TrimPrefix(r.URL.Path, "/git")
	if pathInfo == "" {
		pathInfo = "/"
	}
	isReceivePack := r.Method == http.MethodPost && strings.HasSuffix(pathInfo, "/git-receive-pack")

	rClone := r.Clone(r.Context())
	rClone.URL.Path = pathInfo

	handler := &cgi.Handler{
		Path: m.gitBin,
		Args: []string{"http-backend"},
		Dir:  m.rootDir,
		Env: []string{
			"GIT_PROJECT_ROOT=" + m.rootDir,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO=" + pathInfo,
		},
	}

	if !isReceivePack {
		m.mu.RLock()
		handler.ServeHTTP(w, rClone)
		m.mu.RUnlock()
		return
	}

	m.mu.Lock()
	oldMain := m.currentCommitSHALocked("refs/heads/main")
	handler.ServeHTTP(w, rClone)
	newMain := m.currentCommitSHALocked("refs/heads/main")

	var (
		mainAdvanced  bool
		modifiedSvcs  []string
		repoDir       = m.repoDir
		onPush        = m.onPushHook
		onMainUpdated = m.onMainUpdatedHook
	)
	if newMain != "" && newMain != oldMain {
		mainAdvanced = true
		syncCmd := exec.Command(m.gitBin, "reset", "--hard", "refs/heads/main")
		syncCmd.Dir = m.repoDir
		_, _ = syncCmd.CombinedOutput()
		svcs, err := m.modifiedServicesBetweenLocked(oldMain, newMain)
		if err != nil {
			modifiedSvcs = AllServices()
		} else {
			modifiedSvcs = svcs
		}
	}
	m.mu.Unlock()

	if mainAdvanced {
		if onPush != nil {
			onPush(repoDir)
		}
		if onMainUpdated != nil {
			onMainUpdated(oldMain, newMain, modifiedSvcs)
		}
	}
}

// GitServerConfig configures the standalone canonical storefront-git HTTP and Git server.
type GitServerConfig struct {
	GitWorkDir     string
	SupervisorURLs map[string]string
	ReloadTimeout  time.Duration
}

// GitServer hosts canonical storefront.git Smart HTTP, optional git-daemon, selective reload webhooks,
// and incident metadata/reset endpoints (/api/incidents/*).
type GitServer struct {
	mu               sync.RWMutex
	gitMgr           *GitWorkspaceManager
	supervisorURLs   map[string]string
	httpClient       *http.Client
	activeIncidentID string
	daemonCmd        *exec.Cmd
}

// NewGitServer creates and seeds a canonical GitServer instance.
func NewGitServer(cfg GitServerConfig) (*GitServer, error) {
	timeout := cfg.ReloadTimeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	srv := &GitServer{
		supervisorURLs: ResolveSupervisorURLs(cfg.SupervisorURLs),
		httpClient:     &http.Client{Timeout: timeout},
	}

	gitMgr, err := NewGitWorkspaceManager(cfg.GitWorkDir, nil)
	if err != nil {
		return nil, err
	}
	gitMgr.SetOnMainUpdatedHook(func(_, newSHA string, modifiedServices []string) {
		_ = srv.dispatchSelectiveReloads(context.Background(), newSHA, modifiedServices)
	})
	srv.gitMgr = gitMgr
	return srv, nil
}

// ResolveSupervisorURLs merges environment-configured supervisor URLs with explicit overrides.
func ResolveSupervisorURLs(explicit map[string]string) map[string]string {
	out := make(map[string]string)
	if rawList := strings.TrimSpace(os.Getenv("SUPERVISOR_URLS")); rawList != "" {
		for _, pair := range strings.Split(rawList, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) == 2 && strings.TrimSpace(kv[0]) != "" && strings.TrimSpace(kv[1]) != "" {
				out[strings.TrimSpace(kv[0])] = strings.TrimRight(strings.TrimSpace(kv[1]), "/")
			}
		}
	}
	envKeys := map[string]string{
		ServiceStorefrontWeb: "STOREFRONT_WEB_SUPERVISOR_URL",
		ServiceCheckout:      "CHECKOUT_SERVICE_SUPERVISOR_URL",
		ServiceShipping:      "SHIPPING_SERVICE_SUPERVISOR_URL",
		ServicePricing:       "PRICING_SERVICE_SUPERVISOR_URL",
		ServiceInventory:     "INVENTORY_SERVICE_SUPERVISOR_URL",
	}
	for svc, envKey := range envKeys {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			out[svc] = strings.TrimRight(v, "/")
		}
	}
	for svc, u := range explicit {
		if strings.TrimSpace(u) != "" {
			out[svc] = strings.TrimRight(strings.TrimSpace(u), "/")
		}
	}
	return out
}

// GitManager returns the underlying GitWorkspaceManager.
func (s *GitServer) GitManager() *GitWorkspaceManager {
	return s.gitMgr
}

// SetSupervisorURLs updates the mapping from service name to supervisor base URL for selective reload webhooks.
func (s *GitServer) SetSupervisorURLs(urls map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.supervisorURLs = ResolveSupervisorURLs(urls)
}

// ActiveScenario returns the currently active scenario metadata, if any.
func (s *GitServer) ActiveScenario() *IncidentScenario {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.activeIncidentID == "" {
		return nil
	}
	sc, ok := GetScenario(s.activeIncidentID)
	if !ok {
		return nil
	}
	return &sc
}

// StartGitDaemon launches a read-only git-daemon process serving storefront.git on listenAddr (e.g., 127.0.0.1:9418).
func (s *GitServer) StartGitDaemon(ctx context.Context, listenAddr string) error {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("invalid git-daemon listen address %q: %w", listenAddr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	args := []string{
		"daemon",
		"--reuseaddr",
		"--export-all",
		"--base-path=" + s.gitMgr.RootDir(),
		"--listen=" + host,
		"--port=" + port,
		s.gitMgr.RepoDir(),
	}
	cmd := exec.CommandContext(ctx, s.gitMgr.gitBin, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting git daemon on %s: %w", listenAddr, err)
	}
	s.mu.Lock()
	s.daemonCmd = cmd
	s.mu.Unlock()
	return nil
}

// Close terminates any background git-daemon child process.
func (s *GitServer) Close() error {
	s.mu.Lock()
	cmd := s.daemonCmd
	s.daemonCmd = nil
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	return nil
}

func (s *GitServer) dispatchSelectiveReloads(ctx context.Context, commitSHA string, services []string) error {
	s.mu.RLock()
	urls := make(map[string]string, len(s.supervisorURLs))
	for k, v := range s.supervisorURLs {
		urls[k] = v
	}
	s.mu.RUnlock()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	for _, svc := range services {
		baseURL, ok := urls[svc]
		if !ok || baseURL == "" {
			continue
		}
		svc, baseURL := svc, baseURL
		wg.Add(1)
		go func() {
			defer wg.Done()
			bodyBytes, _ := json.Marshal(map[string]string{
				"service":    svc,
				"commit_sha": commitSHA,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/internal/reload", bytes.NewReader(bodyBytes))
			if err != nil {
				recordErr(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.httpClient.Do(req)
			if err != nil {
				recordErr(fmt.Errorf("reloading %s: %w", svc, err))
				return
			}
			respBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				recordErr(fmt.Errorf("reloading %s returned HTTP %d: %s", svc, resp.StatusCode, strings.TrimSpace(string(respBody))))
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// Handler returns the HTTP handler for canonical storefront-git (:8090).
// TODO(security): Internal cluster Git + incident reset service; restrict network ingress via Kubernetes NetworkPolicy in production deployments.
func (s *GitServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/git/", s.gitMgr)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":       "ok",
			"baseline_tag": BaselineGitTag,
			"commit_sha":   s.gitMgr.CurrentCommitSHA("refs/heads/main"),
		})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":     "ready",
			"commit_sha": s.gitMgr.CurrentCommitSHA("refs/heads/main"),
		})
	})
	mux.HandleFunc("/api/incidents", s.handleIncidents)
	mux.HandleFunc("/api/incidents/activate", s.handleActivateIncident)
	mux.HandleFunc("/api/incidents/randomize", s.handleRandomizeIncident)
	mux.HandleFunc("/api/incidents/heal", s.handleHealIncident)
	mux.HandleFunc("/api/incidents/reset", s.handleResetIncident)
	return httputil.WithSecurityHeaders(mux)
}

func (s *GitServer) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios":       ScenarioCatalog(),
		"active_scenario": s.ActiveScenario(),
		"baseline_tag":    BaselineGitTag,
		"baseline_sha":    s.gitMgr.BaselineCommitSHA(),
		"commit_sha":      s.gitMgr.CurrentCommitSHA("refs/heads/main"),
	})
}

func (s *GitServer) handleActivateIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IncidentID string `json:"incident_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.IncidentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incident_id is required"})
		return
	}
	sc, ok := GetScenario(req.IncidentID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown incident scenario: " + req.IncidentID})
		return
	}
	s.mu.Lock()
	s.activeIncidentID = sc.ID
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "activated",
		"active_scenario": sc,
	})
}

func (s *GitServer) handleRandomizeIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	catalog := ScenarioCatalog()
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(catalog))))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	sc := catalog[n.Int64()]
	s.mu.Lock()
	s.activeIncidentID = sc.ID
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "randomized",
		"active_scenario": sc,
	})
}

func (s *GitServer) handleHealIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IncidentID string `json:"incident_id"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	s.mu.Lock()
	if req.IncidentID == "" || s.activeIncidentID == req.IncidentID {
		s.activeIncidentID = ""
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "healed",
		"active_scenario": s.ActiveScenario(),
		"commit_sha":      s.gitMgr.CurrentCommitSHA("refs/heads/main"),
	})
}

func (s *GitServer) handleResetIncident(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	res, err := s.gitMgr.ResetToBaselineReport()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.activeIncidentID = ""
	s.mu.Unlock()

	if err := s.dispatchSelectiveReloads(r.Context(), res.CommitSHA, res.ReloadedServices); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status":            "reload_failed",
			"error":             err.Error(),
			"commit_sha":        res.CommitSHA,
			"reloaded_services": res.ReloadedServices,
		})
		return
	}
	res.DurationMs = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, res)
}

// IsServicePatchedForIncident checks whether the service's source file in repoDir has been patched for sc.
func IsServicePatchedForIncident(repoDir, incidentID, service string) bool {
	srcPath := filepath.Join(repoDir, "services", service)
	switch service {
	case ServicePricing:
		b, err := os.ReadFile(filepath.Join(srcPath, "pricing.py"))
		if err != nil {
			return false
		}
		s := string(b)
		if incidentID == IncidentStackedPromoNegativeSubtotal {
			return strings.Contains(s, "max(0,") && !strings.Contains(s, "NegativeSubtotalError")
		}
		if incidentID == IncidentZeroDecimalJPYRounding {
			return !strings.Contains(s, "CurrencyPrecisionMismatchError")
		}
	case ServiceShipping:
		b, err := os.ReadFile(filepath.Join(srcPath, "shipping.go"))
		if err != nil {
			return false
		}
		s := string(b)
		if incidentID == IncidentStackedPromoNegativeSubtotal {
			return !strings.Contains(s, "CarrierTariffBoundsError")
		}
		if incidentID == IncidentLithiumHazmatWeightTier {
			return strings.Contains(s, "UN3480-LITHIUM")
		}
	case ServiceInventory:
		b, err := os.ReadFile(filepath.Join(srcPath, "src", "lib.rs"))
		if err != nil {
			return false
		}
		s := string(b)
		if incidentID == IncidentFlashSaleBundleUnderflow {
			return !strings.Contains(s, "RustU32UnderflowPanic") || strings.Contains(s, "saturating_sub")
		}
		if incidentID == IncidentLithiumHazmatWeightTier {
			return strings.Contains(s, "UN3480-LITHIUM")
		}
	case ServiceCheckout:
		b, err := os.ReadFile(filepath.Join(srcPath, "checkout.go"))
		if err != nil {
			return false
		}
		s := string(b)
		if incidentID == IncidentFlashSaleBundleUnderflow {
			return !strings.Contains(s, "FlashBundleUnderflowError") || strings.Contains(s, "effectiveBonus")
		}
		if incidentID == IncidentZeroDecimalJPYRounding {
			return strings.Contains(s, `currency == "JPY"`) || !strings.Contains(s, "PaymentCapturePrecisionError")
		}
	case ServiceStorefrontWeb:
		b, err := os.ReadFile(filepath.Join(srcPath, "src", "server.ts"))
		if err != nil {
			return false
		}
		s := string(b)
		if incidentID == IncidentZeroDecimalJPYRounding {
			return !strings.Contains(s, "StorefrontCurrencyPrecisionError")
		}
	}
	return false
}
