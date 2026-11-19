package compare

import (
	"testing"

	"driftconverge/internal/model"
)

func TestDeploymentsReportsExpectedAndActual(t *testing.T) {
	expected := []model.DesiredDeployment{{
		ResourceKey: "cluster-dev/cluster-dev/app",
		Namespace:   "cluster-dev",
		Name:        "app",
		Replicas:    2,
		Containers: []model.DesiredContainer{{
			Name:  "app",
			Image: "example/app:2",
			Env: []model.EnvVar{
				{Name: "MODE", Value: "dev"},
				{Name: "NEW", Value: "yes"},
			},
		}},
	}}
	actual := []model.DeploymentState{{
		Namespace: "cluster-dev",
		Name:      "app",
		Replicas:  1,
		Containers: []model.ContainerState{{
			Name:  "app",
			Image: "example/app:1",
			Env:   map[string]string{"MODE": "prod", "EXTRA": "x"},
		}},
	}}
	differences := Deployments(expected, actual)
	if len(differences) != 5 {
		t.Fatalf("expected 5 differences, got %d: %+v", len(differences), differences)
	}
	seen := map[string]model.Difference{}
	for _, item := range differences {
		seen[item.Field+"/"+item.EnvKey] = item
	}
	if item := seen["replicas/"]; item.Expected != int32(2) || item.Actual != int32(1) {
		t.Fatalf("unexpected replica diff: %+v", item)
	}
	if item := seen["image/"]; item.Expected != "example/app:2" || item.Actual != "example/app:1" {
		t.Fatalf("unexpected image diff: %+v", item)
	}
	if item := seen["env/MODE"]; item.Expected != "dev" || item.Actual != "prod" {
		t.Fatalf("unexpected env diff: %+v", item)
	}
	if item := seen["env/NEW"]; item.Expected != "yes" || item.Actual != nil {
		t.Fatalf("unexpected missing env diff: %+v", item)
	}
	if item := seen["env/EXTRA"]; item.Expected != nil || item.Actual != "x" {
		t.Fatalf("unexpected extra env diff: %+v", item)
	}
}

func TestApplyResolutions(t *testing.T) {
	original := []model.Difference{
		{ResourceKey: "a", Field: "image", Component: "app"},
		{ResourceKey: "b", Field: "replicas"},
		{ResourceKey: "c", Field: "env", EnvKey: "MODE"},
	}
	remaining := []model.Difference{
		{ResourceKey: "b", Field: "replicas"},
		{ResourceKey: "c", Field: "env", EnvKey: "MODE"},
	}
	ApplyResolutions(original, remaining, map[string]bool{"c": true})
	got := map[string]string{}
	for _, item := range original {
		got[item.ResourceKey] = item.Resolution
	}
	if got["a"] != model.ResolutionConverged ||
		got["b"] != model.ResolutionUnresolved ||
		got["c"] != model.ResolutionBlocked {
		t.Fatalf("unexpected resolutions: %+v", got)
	}
}
