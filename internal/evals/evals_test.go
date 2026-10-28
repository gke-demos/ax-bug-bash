package evals_test

import (
	"context"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/evals"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
)

func TestSingleScenarioTrajectoryGradesAllSixInvariants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	grade, err := evals.RunScenarioTrajectory(ctx, evals.TrajectorySpec{
		IncidentID:     storefront.IncidentStackedPromoNegativeSubtotal,
		HarnessMode:    "multi",
		ProvenanceMode: gateway.ProvenanceFixture,
	})
	if err != nil {
		t.Fatalf("RunScenarioTrajectory(INC-101, multi, FIXTURE) failed: %v", err)
	}

	if !grade.Passed {
		t.Fatalf("expected trajectory grade Passed=true, got false: %+v", grade)
	}
	if !grade.BaselineCIGreen {
		t.Errorf("invariant 1 failed: BaselineCIGreen = false")
	}
	if !grade.AlertmanagerWebhookTriggered {
		t.Errorf("invariant 2 failed: AlertmanagerWebhookTriggered = false")
	}
	if !grade.SelectiveBlastRadiusFanOut {
		t.Errorf("invariant 3 failed: SelectiveBlastRadiusFanOut = false")
	}
	if !grade.RedGreenRegressionTDD {
		t.Errorf("invariant 4 failed: RedGreenRegressionTDD = false")
	}
	if !grade.OnCommandExitAndSuspendReclaimed {
		t.Errorf("invariant 5 failed: OnCommandExitAndSuspendReclaimed = false")
	}
	if !grade.PreWarmedRolloutAndStorefrontHealed {
		t.Errorf("invariant 6 failed: PreWarmedRolloutAndStorefrontHealed = false")
	}
	if len(grade.ImplicatedServices) != 2 {
		t.Errorf("len(ImplicatedServices) = %d, want 2 (pricing-service, shipping-service)", len(grade.ImplicatedServices))
	}
	if grade.SpawnedChildCount != 2 {
		t.Errorf("SpawnedChildCount = %d, want 2", grade.SpawnedChildCount)
	}
	if grade.RolloutDurationMs <= 0 || grade.RolloutDurationMs >= 10000 {
		t.Errorf("RolloutDurationMs = %d, want in (0, 10000)", grade.RolloutDurationMs)
	}
	if grade.PostRolloutCheckoutStatus != 200 {
		t.Errorf("PostRolloutCheckoutStatus = %d, want 200", grade.PostRolloutCheckoutStatus)
	}
}

func TestFourScenariosAcrossAllHarnessesAndLiveCascade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	report, err := evals.RunEvaluationSuite(ctx, evals.SuiteConfig{
		IncludeLiveCascadeChecks: true,
		MaxConcurrency:           4,
	})
	if err != nil {
		t.Fatalf("RunEvaluationSuite failed: %v", err)
	}
	if !report.AllPassed {
		for _, tr := range report.Trajectories {
			if !tr.Passed {
				t.Errorf("trajectory (%s, harness=%s, provenance=%s) failed: %v", tr.IncidentID, tr.HarnessMode, tr.ProvenanceMode, tr.FailureReasons)
			}
		}
		t.Fatalf("expected AllPassed=true, got %d/%d passed", report.PassedTrajectories, report.TotalTrajectories)
	}

	// 4 Scenarios x 4 Harnesses in FIXTURE mode (16) + 4 LIVE Gemini Flash Cascade runs (antigravity, opencode, pi, multi) = 20
	if report.TotalTrajectories < 20 {
		t.Errorf("TotalTrajectories = %d, want >= 20 (16 4x4 matrix + 4 LIVE cascade checks)", report.TotalTrajectories)
	}

	for _, scID := range []string{"INC-101", "INC-102", "INC-103", "INC-104"} {
		if report.ScenarioCoverage[scID] < 4 {
			t.Errorf("ScenarioCoverage[%s] = %d, want >= 4 (all 4 harness configurations)", scID, report.ScenarioCoverage[scID])
		}
	}

	for _, h := range []string{"antigravity", "opencode", "pi", "multi"} {
		if report.HarnessCoverage[h] < 5 {
			t.Errorf("HarnessCoverage[%s] = %d, want >= 5 (4 scenarios + LIVE cascade check)", h, report.HarnessCoverage[h])
		}
	}

	for _, model := range gateway.DefaultFallbackCascade {
		if !report.CascadeModelsVerified[model] {
			t.Errorf("expected Gemini Flash cascade model %q to be verified in LIVE cascade runs, got %v", model, report.CascadeModelsVerified)
		}
	}
}

func TestRepositoryHygieneAndManifestContract(t *testing.T) {
	hygiene, err := evals.VerifyRepositoryHygieneAndManifests("../..")
	if err != nil {
		t.Fatalf("VerifyRepositoryHygieneAndManifests failed: %v", err)
	}
	if !hygiene.Passed {
		t.Fatalf("expected hygiene & manifest contract Passed=true, got violations: %v", hygiene.Violations)
	}
	if len(hygiene.CheckedFiles) < 6 {
		t.Errorf("len(CheckedFiles) = %d, want >= 6", len(hygiene.CheckedFiles))
	}
}


