package observer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/observer"
)

func TestAboutArchitectureCatalogEndpointAndGeometry(t *testing.T) {
	ctx := context.Background()
	srv, err := observer.NewServer(ctx, observer.Config{})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	defer srv.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/about/catalog")
	if err != nil {
		t.Fatalf("GET /api/about/catalog failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/about/catalog status = %d, want 200", resp.StatusCode)
	}

	var cat observer.ArchitectureTopologyCatalog
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatalf("decoding ArchitectureTopologyCatalog failed: %v", err)
	}

	if cat.ViewBox.Width != 1200 || cat.ViewBox.Height != 620 {
		t.Errorf("ViewBox = %+v, want 1200x620", cat.ViewBox)
	}
	if len(cat.Swimlanes) != 4 {
		t.Errorf("len(Swimlanes) = %d, want 4", len(cat.Swimlanes))
	}
	if len(cat.Nodes) != 12 {
		t.Fatalf("len(Nodes) = %d, want 12 topology nodes", len(cat.Nodes))
	}
	if len(cat.Edges) != 11 {
		t.Fatalf("len(Edges) = %d, want 11 animated signal edges", len(cat.Edges))
	}
	if len(cat.Services) != 5 {
		t.Fatalf("len(Services) = %d, want 5 polyglot services", len(cat.Services))
	}
	if len(cat.Scenarios) != 4 {
		t.Fatalf("len(Scenarios) = %d, want 4 failure scenarios", len(cat.Scenarios))
	}
	if len(cat.Harnesses) != 3 {
		t.Fatalf("len(Harnesses) = %d, want 3 coding harnesses", len(cat.Harnesses))
	}
	if len(cat.CascadeModels) != 4 {
		t.Fatalf("len(CascadeModels) = %d, want 4 Gemini Flash cascade models", len(cat.CascadeModels))
	}
	for i, m := range cat.CascadeModels {
		if m.ModelID != gateway.DefaultFallbackCascade[i] {
			t.Errorf("CascadeModels[%d].ModelID = %q, want %q", i, m.ModelID, gateway.DefaultFallbackCascade[i])
		}
	}
	if len(cat.LifecycleStages) != 6 {
		t.Fatalf("len(LifecycleStages) = %d, want 6 interactive lifecycle stages", len(cat.LifecycleStages))
	}
	if len(cat.ADRs) != len(observer.ExpectedADRFilenames()) {
		t.Fatalf("len(ADRs) = %d, want %d ADR cards", len(cat.ADRs), len(observer.ExpectedADRFilenames()))
	}

	if err := observer.ValidateCatalogGeometry(cat); err != nil {
		t.Fatalf("ValidateCatalogGeometry failed on default catalog: %v", err)
	}

	// Verify stage-safe scrubbing: zero bucket URIs, GKE node/context strings, or retired branding
	rawJSON, _ := json.Marshal(cat)
	rawStr := strings.ToLower(string(rawJSON))
	for _, forbidden := range []string{
		"gke_",
		"gs://",
		"fluid compute",
		"vertex" + " ai",
	} {
		if strings.Contains(rawStr, forbidden) {
			t.Errorf("ArchitectureTopologyCatalog contains forbidden identifier or branding %q", forbidden)
		}
	}

	// Verify runtime telemetry counters are populated on ADRs and StatusStrip
	if cat.StatusStrip.ActiveExecutionMode == "" {
		t.Errorf("StatusStrip.ActiveExecutionMode is empty")
	}
	if cat.StatusStrip.StorefrontBaselineTag != "baseline-v1" {
		t.Errorf("StatusStrip.StorefrontBaselineTag = %q, want baseline-v1", cat.StatusStrip.StorefrontBaselineTag)
	}
	for _, adr := range cat.ADRs {
		if adr.ID == "" || adr.Title == "" || adr.Status == "" || adr.Summary == "" {
			t.Errorf("ADR card incomplete: %+v", adr)
		}
		if len(adr.RuntimeCounters) == 0 {
			t.Errorf("ADR %s missing RuntimeCounters", adr.ID)
		}
	}
}

func TestVerifyArchitectureCatalogDrift(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	if err := observer.VerifyArchitectureCatalogDrift(repoRoot); err != nil {
		t.Fatalf("VerifyArchitectureCatalogDrift(%q) failed on clean repository: %v", repoRoot, err)
	}

	// Test fail-closed when ADR file is missing
	tmpDir := t.TempDir()
	if err := observer.VerifyArchitectureCatalogDrift(tmpDir); err == nil {
		t.Fatalf("VerifyArchitectureCatalogDrift expected error on empty directory, got nil")
	}

	// Test fail-closed on overlapping node bounding boxes
	cat := observer.BuildArchitectureTopologyCatalog(observer.CatalogRuntimeTelemetry{
		ActiveExecutionMode:   "SIMULATED",
		LiveWorkerSlots:       4,
		WarmIdleSlots:         4,
		GatewaySessionsCount:  0,
		StorefrontBaselineTag: "baseline-v1",
	})
	overlappingCat := cat
	overlappingCat.Nodes = append([]observer.TopologyNode(nil), cat.Nodes...)
	overlappingCat.Nodes[1].X = overlappingCat.Nodes[0].X
	overlappingCat.Nodes[1].Y = overlappingCat.Nodes[0].Y
	if err := observer.ValidateCatalogGeometry(overlappingCat); err == nil {
		t.Errorf("ValidateCatalogGeometry expected error for overlapping nodes, got nil")
	}

	// Test fail-closed on out-of-bounds node coordinates
	oobCat := cat
	oobCat.Nodes = append([]observer.TopologyNode(nil), cat.Nodes...)
	oobCat.Nodes[0].X = 1150
	oobCat.Nodes[0].Width = 100
	if err := observer.ValidateCatalogGeometry(oobCat); err == nil {
		t.Errorf("ValidateCatalogGeometry expected error for out-of-bounds node, got nil")
	}

	// Test fail-closed on dangling edge node reference
	danglingEdgeCat := cat
	danglingEdgeCat.Edges = append([]observer.TopologyEdge(nil), cat.Edges...)
	danglingEdgeCat.Edges[0].ToNode = "nonexistent-node-id"
	if err := observer.ValidateCatalogGeometry(danglingEdgeCat); err == nil {
		t.Errorf("ValidateCatalogGeometry expected error for dangling edge node reference, got nil")
	}

	// Test StageSafeScrub removes sensitive strings and dynamic env vars
	t.Setenv("PROJECT_ID", "custom-stage-project")
	scrubbed := observer.ScrubStageSafeString("cluster gke_demo_us-central1-a_cluster node gke-demo-default-pool-1234 project custom-stage-project number 123456789012 bucket gs://demo-snapshots/ax")
	for _, sensitive := range []string{"gke_demo", "gke-demo-default-pool", "custom-stage-project", "123456789012", "gs://"} {
		if strings.Contains(scrubbed, sensitive) {
			t.Errorf("ScrubStageSafeString failed to scrub sensitive token %q: %q", sensitive, scrubbed)
		}
	}

	// Test partial repo missing a single harness binary fails closed
	partialRepo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(partialRepo, "docs", "adr"), 0o755)
	for _, adrName := range observer.ExpectedADRFilenames() {
		name := filepath.Join(partialRepo, "docs", "adr", adrName)
		_ = os.WriteFile(name, []byte("# ADR\n\n## Status\nAccepted\n"), 0o644)
	}
	if err := observer.VerifyArchitectureCatalogDrift(partialRepo); err == nil {
		t.Errorf("VerifyArchitectureCatalogDrift expected error when services/ and harnesses/ are missing, got nil")
	}
}
