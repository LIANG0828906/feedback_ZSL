package policy

import (
	"testing"

	"driftconverge/internal/model"
)

func TestCheckProdFindsAllDevOnlyFields(t *testing.T) {
	dev := snapshot("cluster-dev", model.BranchPolicy{
		ProdReplicaCeiling: 2,
		ReplicaCeilingSet:  true,
		ForbiddenImageTags: []string{"3.9-debug"},
		ForbiddenEnvKeys:   []string{"DEBUG_MODE"},
	})
	dev.Deployments[0].Containers[0].Image = "registry.k8s.io/pause:3.9-debug"
	dev.Deployments[0].Containers[0].Env = append(dev.Deployments[0].Containers[0].Env, model.EnvVar{Name: "DEBUG_MODE", Value: "true"})
	prod := snapshot("cluster-prod", model.BranchPolicy{})
	prod.Deployments[0].Containers[0].Image = "registry.k8s.io/pause:3.9-debug"
	prod.Deployments[0].Containers[0].Env = append(prod.Deployments[0].Containers[0].Env, model.EnvVar{Name: "DEBUG_MODE", Value: "true"})
	prod.Deployments[0].Replicas = 3

	blocks := CheckProd(prod, Build(dev, prod))
	if len(blocks) != 3 {
		t.Fatalf("expected 3 policy blocks, got %d: %+v", len(blocks), blocks)
	}
	fields := map[string]bool{}
	for _, block := range blocks {
		fields[block.Field] = true
		if block.SourceBranch != "cluster-prod" || block.DeclaredInBranch != "cluster-dev" {
			t.Fatalf("unexpected branch attribution: %+v", block)
		}
	}
	for _, expected := range []string{"image:app", "env:DEBUG_MODE", "replicas"} {
		if !fields[expected] {
			t.Fatalf("missing policy field %s in %+v", expected, blocks)
		}
	}
}

func TestBuildInfersDevOnlyValues(t *testing.T) {
	dev := snapshot("cluster-dev", model.BranchPolicy{})
	dev.Deployments[0].Replicas = 4
	dev.Deployments[0].Containers[0].Image = "example/app:dev"
	dev.Deployments[0].Containers[0].Env = append(dev.Deployments[0].Containers[0].Env, model.EnvVar{Name: "DEV_ONLY", Value: "x"})
	prod := snapshot("cluster-prod", model.BranchPolicy{})
	prod.Deployments[0].Containers[0].Image = "example/app:stable"

	effective := Build(dev, prod)
	if !effective.ForbiddenImageTags["dev"] {
		t.Fatal("expected inferred dev-only image tag")
	}
	if !effective.ForbiddenEnvKeys["DEV_ONLY"] {
		t.Fatal("expected inferred dev-only env key")
	}
	if effective.ReplicaCeiling != 4 {
		t.Fatalf("expected inferred replica ceiling 4, got %d", effective.ReplicaCeiling)
	}
}

func TestImageTag(t *testing.T) {
	tests := map[string]string{
		"repo/app:1.2":                     "1.2",
		"registry.local:5000/repo/app:dev": "dev",
		"repo/app":                         "latest",
		"repo/app@sha256:abcdef":           "latest",
	}
	for image, expected := range tests {
		if actual := ImageTag(image); actual != expected {
			t.Fatalf("ImageTag(%q)=%q, expected %q", image, actual, expected)
		}
	}
}

func snapshot(cluster string, branchPolicy model.BranchPolicy) *model.Snapshot {
	return &model.Snapshot{
		Cluster: cluster,
		Branch:  cluster,
		Deployments: []model.DesiredDeployment{{
			ResourceKey: cluster + "/" + cluster + "/app",
			Cluster:     cluster,
			Namespace:   cluster,
			Name:        "app",
			Replicas:    1,
			Containers: []model.DesiredContainer{{
				Name:  "app",
				Image: "example/app:stable",
				Env:   []model.EnvVar{{Name: "MODE", Value: cluster}},
			}},
		}},
		Policy:        branchPolicy,
		PolicyPresent: true,
	}
}
