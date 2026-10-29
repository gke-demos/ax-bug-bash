package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gke-demos/ax-bug-bash/internal/bugbash"
	"github.com/gke-demos/ax-bug-bash/internal/gateway"
	"github.com/gke-demos/ax-bug-bash/internal/storefront"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestRunMainExecuteTaskRoleFlagsAndChildFixerPush(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Invalid --role must fail closed
	if err := runMain(ctx, []string{"--execute-task", "--role=unknown-role"}); err == nil {
		t.Fatalf("expected invalid --role to fail")
	}

	sfRuntime, err := storefront.NewRuntime(storefront.Config{GitWorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("storefront.NewRuntime failed: %v", err)
	}
	sfHTTP := httptest.NewServer(sfRuntime.Handler())
	defer sfHTTP.Close()
	gitURL := sfHTTP.URL + "/git/storefront.git"

	gwCfg := gateway.DefaultConfig()
	gwCfg.EnableFixtureFallback = true
	gwSrv := gateway.NewServer(gwCfg, func(context.Context) (string, error) {
		return "", fmt.Errorf("fixture")
	})
	gwHTTP := httptest.NewServer(gwSrv.Handler())
	defer gwHTTP.Close()

	sc, _ := storefront.GetScenario(storefront.IncidentStackedPromoNegativeSubtotal)
	detail := sc.ServiceTraces[storefront.ServiceShipping]
	taskName := "fixer-inc-101-shipping-cli"

	cfgStruct, err := bugbash.ChildMissionToStruct(bugbash.ChildTaskMission{
		TaskName:           taskName,
		ParentTask:         "triage-inc-101-cli",
		IncidentID:         sc.ID,
		Service:            storefront.ServiceShipping,
		Language:           detail.Language,
		Harness:            "antigravity",
		RequestedModel:     "gemini-3.8-flash",
		SourceFile:         detail.SourceFile,
		TestFile:           detail.TestFile,
		RegressionTestName: detail.RegressionTestName,
	})
	if err != nil {
		t.Fatalf("ChildMissionToStruct failed: %v", err)
	}

	wsDir := filepath.Join(t.TempDir(), "cli-ws")
	taskObj := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: taskName, Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: bugbash.DefaultWorkspaceName, Path: wsDir},
			Config:    cfgStruct,
		},
	}
	rawJSON, _ := json.Marshal(taskObj)

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata/v1alpha1/ax/task" {
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(rawJSON)
			return
		}
		http.NotFound(w, r)
	}))
	defer metaSrv.Close()

	t.Setenv("AX_METADATA_URL", metaSrv.URL)
	t.Setenv("STOREFRONT_GIT_URL", gitURL)
	t.Setenv("MODEL_GATEWAY_URL", gwHTTP.URL)

	if err := runMain(ctx, []string{"--execute-task", "--role=child-fixer"}); err != nil {
		t.Fatalf("runMain(--execute-task --role=child-fixer) failed: %v", err)
	}

	lsCmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", gitURL, "refs/heads/fix/"+taskName)
	if out, lErr := lsCmd.CombinedOutput(); lErr != nil {
		t.Fatalf("expected refs/heads/fix/%s pushed to origin: %v (%s)", taskName, lErr, string(out))
	}
}
