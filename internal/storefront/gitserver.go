package storefront

import (
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// GitWorkspaceManager manages the in-process Smart HTTP Git repository seeding the 5-service monorepo.
type GitWorkspaceManager struct {
	mu         sync.Mutex
	rootDir    string
	repoDir    string
	gitBin     string
	onPushHook func(repoDir string)
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

// RepoDir returns the filesystem path to the seeded storefront.git working repository.
func (m *GitWorkspaceManager) RepoDir() string {
	return m.repoDir
}

// ResetToBaseline re-initializes storefront.git with the baseline 5-service monorepo files.
func (m *GitWorkspaceManager) ResetToBaseline() error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		{"config", "http.receivepack", "true"},
		{"config", "receive.denyCurrentBranch", "updateInstead"},
		{"add", "."},
		{"commit", "-m", "chore: seed baseline 5-service polyglot storefront monorepo"},
	}
	for _, args := range cmds {
		cmd := exec.Command(m.gitBin, args...)
		cmd.Dir = m.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v failed: %w (%s)", args, err, string(out))
		}
	}
	return nil
}

// ServeHTTP serves Smart HTTP Git requests under /git/storefront.git/*.
func (m *GitWorkspaceManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pathInfo := strings.TrimPrefix(r.URL.Path, "/git")
	if pathInfo == "" {
		pathInfo = "/"
	}

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
	handler.ServeHTTP(w, rClone)

	if r.Method == http.MethodPost && strings.HasSuffix(pathInfo, "/git-receive-pack") && m.onPushHook != nil {
		m.onPushHook(m.repoDir)
	}
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
