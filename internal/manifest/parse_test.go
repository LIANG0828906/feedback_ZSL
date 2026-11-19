package manifest

import (
	"errors"
	"strings"
	"testing"

	"driftconverge/internal/model"
)

func TestParseSnapshotValid(t *testing.T) {
	snapshot, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc123", map[string][]byte{
		"manifests/00-namespace.yaml": []byte(`
apiVersion: v1
kind: Namespace
metadata:
  name: cluster-dev
`),
		"manifests/10-app.yaml": []byte(deploymentYAML("cluster-dev", "app", 2, "example/app:1", "MODE", "dev")),
		"manifests/90-policy.yaml": []byte(`
apiVersion: driftconverge/v1
kind: BranchPolicy
metadata:
  name: guard
spec:
  prodReplicaCeiling: 2
  prodForbiddenImageTags: ["1-debug"]
  prodForbiddenEnvKeys: [DEBUG_MODE]
`),
	})
	if err != nil {
		t.Fatalf("ParseSnapshot returned error: %v", err)
	}
	if len(snapshot.Deployments) != 1 {
		t.Fatalf("expected 1 deployment, got %d", len(snapshot.Deployments))
	}
	dep := snapshot.Deployments[0]
	if dep.ResourceKey != "cluster-dev/cluster-dev/app" {
		t.Fatalf("unexpected resource key %q", dep.ResourceKey)
	}
	if dep.Replicas != 2 || dep.Containers[0].Image != "example/app:1" {
		t.Fatalf("unexpected deployment: %+v", dep)
	}
	if !snapshot.PolicyPresent || snapshot.Policy.ProdReplicaCeiling != 2 || !snapshot.Policy.ReplicaCeilingSet {
		t.Fatalf("policy was not parsed: %+v", snapshot.Policy)
	}
}

func TestParseSnapshotRejectsDuplicateResourceKey(t *testing.T) {
	files := map[string][]byte{
		"manifests/00-namespace.yaml": []byte(namespaceYAML("cluster-dev")),
		"manifests/10-a.yaml":         []byte(deploymentYAML("cluster-dev", "app", 1, "example/app:1", "MODE", "dev")),
		"manifests/20-b.yaml":         []byte(deploymentYAML("cluster-dev", "app", 2, "example/app:2", "MODE", "dev")),
	}
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", files)
	assertBusinessError(t, err, model.CodeManifest, "cluster-dev/cluster-dev/app")
}

func TestParseSnapshotRejectsUndeclaredNamespace(t *testing.T) {
	files := map[string][]byte{
		"manifests/00-namespace.yaml": []byte(namespaceYAML("other")),
		"manifests/10-a.yaml":         []byte(deploymentYAML("cluster-dev", "app", 1, "example/app:1", "MODE", "dev")),
	}
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", files)
	assertBusinessError(t, err, model.CodeManifest, "cluster-dev/cluster-dev/app")
	if !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("expected namespace explanation, got %v", err)
	}
}

func TestParseSnapshotRejectsMissingField(t *testing.T) {
	bad := strings.Replace(
		deploymentYAML("cluster-dev", "app", 1, "example/app:1", "MODE", "dev"),
		"  namespace: cluster-dev\n",
		"",
		1,
	)
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", map[string][]byte{
		"manifests/00-namespace.yaml": []byte(namespaceYAML("cluster-dev")),
		"manifests/10-a.yaml":         []byte(bad),
	})
	assertBusinessError(t, err, model.CodeManifest, "cluster-dev/<namespace-missing>/app")
}

func TestParseSnapshotRejectsMissingAPIVersion(t *testing.T) {
	bad := strings.Replace(
		deploymentYAML("cluster-dev", "app", 1, "example/app:1", "MODE", "dev"),
		"apiVersion: apps/v1\n",
		"",
		1,
	)
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", map[string][]byte{
		"manifests/00-namespace.yaml": []byte(namespaceYAML("cluster-dev")),
		"manifests/10-a.yaml":         []byte(bad),
	})
	assertBusinessError(t, err, model.CodeManifest, "cluster-dev/cluster-dev/app")
}

func TestParseSnapshotRejectsDuplicateEnvKey(t *testing.T) {
	manifest := deploymentYAML("cluster-dev", "app", 1, "example/app:1", "MODE", "dev")
	manifest = strings.Replace(
		manifest,
		"              value: dev",
		"              value: dev\n            - name: MODE\n              value: duplicate",
		1,
	)
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", map[string][]byte{
		"manifests/00-namespace.yaml": []byte(namespaceYAML("cluster-dev")),
		"manifests/10-a.yaml":         []byte(manifest),
	})
	assertBusinessError(t, err, model.CodeManifest, "cluster-dev/cluster-dev/app")
}

func TestParseSnapshotRejectsInvalidYAML(t *testing.T) {
	_, err := ParseSnapshot("cluster-dev", "cluster-dev", "abc", map[string][]byte{
		"manifests/broken.yaml": []byte("kind: [\n"),
	})
	assertBusinessError(t, err, model.CodeManifest, "manifests/broken.yaml#doc-1")
}

func assertBusinessError(t *testing.T, err error, code, resourceKey string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var businessErr *model.BusinessError
	if !errors.As(err, &businessErr) {
		t.Fatalf("expected BusinessError, got %T: %v", err, err)
	}
	if businessErr.Code != code {
		t.Fatalf("expected code %s, got %s", code, businessErr.Code)
	}
	if businessErr.ResourceKey != resourceKey {
		t.Fatalf("expected resource key %q, got %q (%v)", resourceKey, businessErr.ResourceKey, err)
	}
}

func namespaceYAML(namespace string) string {
	return `
apiVersion: v1
kind: Namespace
metadata:
  name: ` + namespace + `
`
}

func deploymentYAML(namespace, name string, replicas int, image, envName, envValue string) string {
	return `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
  namespace: ` + namespace + `
spec:
  replicas: ` + intString(replicas) + `
  selector:
    matchLabels:
      app: ` + name + `
  template:
    metadata:
      labels:
        app: ` + name + `
    spec:
      containers:
        - name: app
          image: ` + image + `
          env:
            - name: ` + envName + `
              value: ` + envValue + `
`
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
