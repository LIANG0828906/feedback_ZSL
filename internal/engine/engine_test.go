package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"driftconverge/internal/gitsource"
	"driftconverge/internal/kube"
	"driftconverge/internal/model"
)

func TestRoundAppliesEachResourceOnceAndConverges(t *testing.T) {
	source := newFakeSource(t, false)
	client := &fakeClient{
		states: map[string]model.DeploymentState{
			"cluster-dev/app":  devState("example/app:1", 1, "MODE", "stale"),
			"cluster-prod/app": stableState("cluster-prod", "app", "example/app:1", 1, "MODE", "prod"),
		},
	}
	roundStore := &fakeStore{}
	runner := newTestEngine(t, source, roundStore, map[string]kube.Client{
		"cluster-dev": client,
		"cluster-prod": &fakeClient{states: map[string]model.DeploymentState{
			"cluster-prod/app": stableState("cluster-prod", "app", "example/app:1", 1, "MODE", "prod"),
		}},
	}, 200*time.Millisecond)

	report, err := runner.RunRound(context.Background())
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	dev := reportCluster(t, report, "cluster-dev")
	if dev.Convergence != model.ConclusionConverged || dev.DiffCount < 3 {
		t.Fatalf("unexpected dev report: %+v", dev)
	}
	if client.applyCount != 1 {
		t.Fatalf("expected exactly one apply for the resource, got %d", client.applyCount)
	}
	if client.states["cluster-dev/app"].Containers[0].Image != "example/app:2" {
		t.Fatalf("apply did not update image: %+v", client.states["cluster-dev/app"])
	}
	if len(roundStore.reports) != 1 {
		t.Fatalf("expected one committed round, got %d", len(roundStore.reports))
	}
}

func TestPolicyRejectsProdBeforeApply(t *testing.T) {
	source := newFakeSource(t, true)
	devClient := &fakeClient{states: map[string]model.DeploymentState{
		"cluster-dev/app": stableState("cluster-dev", "app", "example/app:2", 2, "MODE", "dev"),
	}}
	prodClient := &fakeClient{states: map[string]model.DeploymentState{
		"cluster-prod/app": stableState("cluster-prod", "app", "example/app:1", 1, "MODE", "prod"),
	}}
	roundStore := &fakeStore{}
	runner := newTestEngine(t, source, roundStore, map[string]kube.Client{
		"cluster-dev":  devClient,
		"cluster-prod": prodClient,
	}, 500*time.Millisecond)

	report, err := runner.RunRound(context.Background())
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	prod := reportCluster(t, report, "cluster-prod")
	if prod.ErrorCode != model.CodePolicy {
		t.Fatalf("expected E_POLICY, got %+v", prod)
	}
	if prod.ComparisonSuccess {
		t.Fatal("policy-rejected snapshot must not be marked comparison_success")
	}
	if prod.PolicyBlockCount != 3 {
		t.Fatalf("expected 3 policy blocks, got %d: %+v", prod.PolicyBlockCount, prod.PolicyBlocks)
	}
	if prodClient.applyCount != 0 || prodClient.listCount != 0 {
		t.Fatalf("prod must not be compared or applied after policy rejection")
	}
}

func TestAdmissionTimeoutBlocksWithoutApply(t *testing.T) {
	source := newFakeSource(t, false)
	unstable := devState("example/app:1", 1, "MODE", "dev")
	unstable.Generation = 3
	unstable.Status.ObservedGeneration = 2
	unstable.Status.UpdatedReplicas = 0
	unstable.Status.AvailableReplicas = 1
	unstable.Status.UnavailableReplicas = 1
	client := &fakeClient{states: map[string]model.DeploymentState{"cluster-dev/app": unstable}}
	roundStore := &fakeStore{}
	runner := newTestEngine(t, source, roundStore, map[string]kube.Client{
		"cluster-dev": client,
		"cluster-prod": &fakeClient{states: map[string]model.DeploymentState{
			"cluster-prod/app": stableState("cluster-prod", "app", "example/app:1", 1, "MODE", "prod"),
		}},
	}, 40*time.Millisecond)

	report, err := runner.RunRound(context.Background())
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	dev := reportCluster(t, report, "cluster-dev")
	if dev.ErrorCode != model.CodeWait || dev.BlockedCount == 0 {
		t.Fatalf("expected blocked E_WAIT, got %+v", dev)
	}
	if dev.Convergence != model.ConclusionPartial {
		t.Fatalf("expected partial convergence, got %s", dev.Convergence)
	}
	if client.applyCount != 0 {
		t.Fatalf("blocked resource must not be applied")
	}
}

func TestUnobservablePreservesPreviousDifferences(t *testing.T) {
	source := newFakeSource(t, false)
	previous := model.PreviousObservation{
		RoundID: "round-previous",
		Differences: []model.Difference{{
			ResourceKey: "cluster-prod/cluster-prod/app",
			Field:       "env",
			Component:   "app",
			EnvKey:      "MODE",
			Expected:    "prod",
			Actual:      "unexpected",
			Resolution:  model.ResolutionConverged,
		}},
		DiffCount:          1,
		ConvergedResources: 1,
	}
	roundStore := &fakeStore{previous: map[string]model.PreviousObservation{"cluster-prod": previous}}
	runner := newTestEngine(t, source, roundStore, map[string]kube.Client{
		"cluster-dev": &fakeClient{states: map[string]model.DeploymentState{
			"cluster-dev/app": stableState("cluster-dev", "app", "example/app:2", 2, "MODE", "dev"),
		}},
		"cluster-prod": &fakeClient{listErr: errors.New("dial tcp: connection refused")},
	}, 500*time.Millisecond)

	report, err := runner.RunRound(context.Background())
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	prod := reportCluster(t, report, "cluster-prod")
	if prod.Observable {
		t.Fatal("prod must be marked unobservable")
	}
	if prod.ComparisonSuccess {
		t.Fatal("unobservable round must not be marked comparison_success")
	}
	if prod.ErrorCode != model.CodeCluster || prod.DiffCount != 1 {
		t.Fatalf("unexpected unobservable report: %+v", prod)
	}
	if len(prod.Differences) != 1 || prod.Differences[0].Actual != "unexpected" {
		t.Fatalf("previous differences were not preserved: %+v", prod.Differences)
	}
	if prod.LastObservedRoundID != "round-previous" {
		t.Fatalf("expected previous round id, got %q", prod.LastObservedRoundID)
	}
}

func TestManifestErrorStopsComparison(t *testing.T) {
	source := &fakeSource{files: map[string]gitsource.SnapshotFiles{
		"cluster-dev":  brokenSnapshotFiles(),
		"cluster-prod": validSnapshotFiles(t, "cluster-prod", false),
	}}
	devClient := &fakeClient{}
	roundStore := &fakeStore{}
	runner := newTestEngine(t, source, roundStore, map[string]kube.Client{
		"cluster-dev":  devClient,
		"cluster-prod": &fakeClient{},
	}, 500*time.Millisecond)
	report, err := runner.RunRound(context.Background())
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	dev := reportCluster(t, report, "cluster-dev")
	if dev.ErrorCode != model.CodeManifest {
		t.Fatalf("expected E_MANIFEST, got %+v", dev)
	}
	if dev.ErrorResourceKey == "" {
		t.Fatal("manifest error must include resource key")
	}
	if dev.ComparisonSuccess {
		t.Fatal("manifest failure must not be marked comparison_success")
	}
	if devClient.listCount != 0 {
		t.Fatal("manifest error must stop before comparison")
	}
}

type fakeSource struct {
	files map[string]gitsource.SnapshotFiles
}

func (s *fakeSource) ReadSnapshot(_ context.Context, branch string) (gitsource.SnapshotFiles, error) {
	files, found := s.files[branch]
	if !found {
		return gitsource.SnapshotFiles{}, errors.New("missing branch " + branch)
	}
	return files, nil
}

type fakeStore struct {
	mu        sync.Mutex
	reports   []model.RoundReport
	previous  map[string]model.PreviousObservation
	commitErr error
}

func (s *fakeStore) CommitRound(_ context.Context, report model.RoundReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return s.commitErr
	}
	s.reports = append(s.reports, report)
	return nil
}

func (s *fakeStore) LastObservable(_ context.Context, cluster string) (model.PreviousObservation, error) {
	if s.previous == nil {
		return model.PreviousObservation{}, errors.New("not found")
	}
	value, found := s.previous[cluster]
	if !found {
		return model.PreviousObservation{}, errors.New("not found")
	}
	return value, nil
}

type fakeClient struct {
	mu         sync.Mutex
	states     map[string]model.DeploymentState
	listErr    error
	applyError error
	listCount  int
	applyCount int
}

func (c *fakeClient) ListDeployments(_ context.Context, namespace string) ([]model.DeploymentState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCount++
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := make([]model.DeploymentState, 0)
	for _, state := range c.states {
		if state.Namespace == namespace {
			out = append(out, cloneState(state))
		}
	}
	return out, nil
}

func (c *fakeClient) GetDeployment(_ context.Context, namespace, name string) (model.DeploymentState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, found := c.states[namespace+"/"+name]
	if !found {
		return model.DeploymentState{}, kube.ErrNotFound
	}
	return cloneState(state), nil
}

func (c *fakeClient) ApplyDeployment(_ context.Context, desired model.DesiredDeployment) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyCount++
	if c.applyError != nil {
		return "update_failed", c.applyError
	}
	state := model.DeploymentState{
		Namespace:  desired.Namespace,
		Name:       desired.Name,
		Generation: 2,
		Replicas:   desired.Replicas,
		Status: model.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           desired.Replicas,
			UpdatedReplicas:    desired.Replicas,
			ReadyReplicas:      desired.Replicas,
			AvailableReplicas:  desired.Replicas,
		},
	}
	for _, container := range desired.Containers {
		env := make(map[string]string, len(container.Env))
		for _, item := range container.Env {
			env[item.Name] = item.Value
		}
		state.Containers = append(state.Containers, model.ContainerState{
			Name:  container.Name,
			Image: container.Image,
			Env:   env,
		})
	}
	c.states[desired.Namespace+"/"+desired.Name] = state
	return "updated", nil
}

func newTestEngine(
	t *testing.T,
	source SnapshotReader,
	roundStore RoundStore,
	clients map[string]kube.Client,
	timeout time.Duration,
) *Engine {
	t.Helper()
	runner, err := New(Options{
		Clusters: []ClusterSpec{
			{Name: "cluster-dev", Branch: "cluster-dev", Role: "dev", Namespace: "cluster-dev"},
			{Name: "cluster-prod", Branch: "cluster-prod", Role: "prod", Namespace: "cluster-prod"},
		},
		Source:           source,
		Store:            roundStore,
		AdmissionTimeout: timeout,
		AdmissionPoll:    5 * time.Millisecond,
		ClientFactory: func(cluster ClusterSpec) (kube.Client, error) {
			client, found := clients[cluster.Name]
			if !found {
				return nil, errors.New("no fake client for " + cluster.Name)
			}
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runner
}

func newFakeSource(t *testing.T, policyBreach bool) *fakeSource {
	t.Helper()
	return &fakeSource{files: map[string]gitsource.SnapshotFiles{
		"cluster-dev":  validSnapshotFiles(t, "cluster-dev", false),
		"cluster-prod": validSnapshotFiles(t, "cluster-prod", policyBreach),
	}}
}

func validSnapshotFiles(t *testing.T, cluster string, policyBreach bool) gitsource.SnapshotFiles {
	t.Helper()
	replicas := "1"
	image := "example/app:1"
	envValue := "prod"
	debugEnv := ""
	policy := ""
	if cluster == "cluster-dev" {
		replicas = "2"
		image = "example/app:2"
		envValue = "dev"
		debugEnv = `
            - name: DEBUG_MODE
              value: "true"`
		policy = `
---
apiVersion: driftconverge/v1
kind: BranchPolicy
metadata:
  name: guard
spec:
  prodReplicaCeiling: 2
  prodForbiddenImageTags: ["2"]
  prodForbiddenEnvKeys: [DEBUG_MODE]
`
	}
	if policyBreach {
		image = "example/app:2"
		envValue = "debug"
		replicas = "3"
		debugEnv = `
            - name: DEBUG_MODE
              value: "true"`
	}
	deployment := `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: ` + cluster + `
spec:
  replicas: ` + replicas + `
  selector:
    matchLabels:
      app: app
  template:
    metadata:
      labels:
        app: app
    spec:
      containers:
        - name: app
          image: ` + image + `
          env:
            - name: MODE
              value: ` + envValue + debugEnv + `
`
	content := `
apiVersion: v1
kind: Namespace
metadata:
  name: ` + cluster + `
` + deployment + policy
	return gitsource.SnapshotFiles{
		Branch: cluster,
		Commit: "commit-" + cluster,
		Files:  map[string][]byte{"manifests/snapshot.yaml": []byte(content)},
	}
}

func brokenSnapshotFiles() gitsource.SnapshotFiles {
	return gitsource.SnapshotFiles{
		Branch: "cluster-dev",
		Commit: "broken",
		Files: map[string][]byte{
			"manifests/broken.yaml": []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: cluster-dev
spec:
  replicas: nope
`),
		},
	}
}

func reportCluster(t *testing.T, report model.RoundReport, cluster string) model.ClusterReport {
	t.Helper()
	for _, item := range report.Clusters {
		if item.Cluster == cluster {
			return item
		}
	}
	t.Fatalf("cluster %s not found in report", cluster)
	return model.ClusterReport{}
}

func devState(image string, replicas int32, envName, envValue string) model.DeploymentState {
	state := stableState("cluster-dev", "app", image, replicas, envName, envValue)
	state.Replicas = 1
	return state
}

func stableState(namespace, name, image string, replicas int32, envName, envValue string) model.DeploymentState {
	return model.DeploymentState{
		Namespace:  namespace,
		Name:       name,
		Generation: 1,
		Replicas:   replicas,
		Containers: []model.ContainerState{{
			Name:  "app",
			Image: image,
			Env:   map[string]string{envName: envValue},
		}},
		Status: model.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
		},
	}
}

func cloneState(state model.DeploymentState) model.DeploymentState {
	out := state
	out.Containers = append([]model.ContainerState(nil), state.Containers...)
	for i := range out.Containers {
		env := make(map[string]string, len(out.Containers[i].Env))
		for key, value := range out.Containers[i].Env {
			env[key] = value
		}
		out.Containers[i].Env = env
	}
	out.Status.Conditions = append([]model.DeploymentCondition(nil), state.Status.Conditions...)
	return out
}
