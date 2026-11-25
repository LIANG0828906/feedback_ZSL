package checkpoint_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"credrot/internal/bizerr"
	"credrot/internal/checkpoint"
	"credrot/internal/engine"
	"credrot/internal/generator"
)

func TestTwoPhaseOrderingAndCrashRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.checkpoint")
	store, state, _, err := checkpoint.OpenOrRebuild(path, "manifest-a", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedTick != -1 {
		t.Fatalf("fresh state tick=%d", state.CommittedTick)
	}
	if err := store.Commit(testState("manifest-a", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitWithCrash(testState("manifest-a", 2), checkpoint.CrashAfterState); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, recovered, info, err := checkpoint.Open(path, "manifest-a", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CommittedTick != 1 {
		t.Fatalf("uncommitted state leaked into recovery: tick=%d", recovered.CommittedTick)
	}
	if !info.IgnoredDanglingState {
		t.Fatal("dangling state was not reported")
	}
	if err := store.Commit(testState("manifest-a", 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitWithCrash(testState("manifest-a", 3), checkpoint.CrashDuringCommit); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, recovered, info, err = checkpoint.Open(path, "manifest-a", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CommittedTick != 2 {
		t.Fatalf("partial commit leaked into recovery: tick=%d", recovered.CommittedTick)
	}
	if !info.TruncatedTail || !info.Repaired {
		t.Fatalf("partial tail was not repaired: %+v", info)
	}
	store.Close()
	if _, err := checkpoint.Verify(path, "manifest-a"); err != nil {
		t.Fatalf("repaired checkpoint is not strict-valid: %v", err)
	}
}

func TestTruncatedStrictAndManifestRebuild(t *testing.T) {
	dir := t.TempDir()
	truncatedPath := filepath.Join(dir, "truncated.checkpoint")
	store, _, _, err := checkpoint.Open(truncatedPath, "manifest-a", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(testState("manifest-a", 1)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	file, err := os.OpenFile(truncatedPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":4,"kind":"commit","payload":{`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := checkpoint.Verify(truncatedPath, "manifest-a"); !bizerr.IsCode(err, bizerr.CodeCheckpointTruncated) {
		t.Fatalf("strict truncation returned %v", err)
	}

	mismatchPath := filepath.Join(dir, "mismatch.checkpoint")
	store, _, _, err = checkpoint.Open(mismatchPath, "manifest-a", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(testState("manifest-a", 7)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, _, _, err := checkpoint.Open(mismatchPath, "manifest-b", checkpoint.OpenOptions{}); !bizerr.IsCode(err, bizerr.CodeManifestMismatch) {
		t.Fatalf("manifest mismatch returned %v", err)
	}
	store, state, info, err := checkpoint.OpenOrRebuild(mismatchPath, "manifest-b", checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !info.Rebuilt || state.ManifestVersion != "manifest-b" || state.CommittedTick != -1 {
		t.Fatalf("rebuild reused old state: info=%+v state=%+v", info, state)
	}
}

func TestBuildStateFromSolverViewAndVersionPairing(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := engine.NewSolver(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAt(6)
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(bundle.Project)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(bundle.Project, propagation)
	if err != nil {
		t.Fatal(err)
	}
	state, err := checkpoint.BuildState(bundle.Project, 6, validity, propagation, schedule, map[string]bool{"sub-api": true})
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedTick != 6 || len(state.Subscriptions) != len(bundle.Project.Subscriptions) ||
		len(state.Pending) != len(propagation.Suspended) || len(state.Batches) != len(schedule.Batches) {
		t.Fatalf("unexpected built state: %+v", state)
	}
	for _, sub := range state.Subscriptions {
		if len(sub.CredentialIDs) != len(sub.Versions) {
			t.Fatalf("credential id/version pairing broken: %+v", sub)
		}
	}

	path := filepath.Join(t.TempDir(), "built.checkpoint")
	store, _, _, err := checkpoint.Open(path, state.ManifestVersion, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state.Subscriptions[0].CredentialIDs = []string{"z", "a"}
	state.Subscriptions[0].Versions = []uint64{2, 1}
	if err := store.Commit(*state); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, recovered, _, err := checkpoint.Open(path, state.ManifestVersion, checkpoint.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got := recovered.Subscriptions[0]
	if !reflect.DeepEqual(got.CredentialIDs, []string{"a", "z"}) || !reflect.DeepEqual(got.Versions, []uint64{1, 2}) {
		t.Fatalf("credential pairs were not normalized together: %+v", got)
	}
}

func testState(manifest string, tick int64) checkpoint.State {
	state := *checkpoint.EmptyState(manifest)
	state.CommittedTick = tick
	state.Subscriptions = []checkpoint.CommittedSubscription{{
		SubscriptionID: "sub-api",
		CredentialIDs:  []string{"cred-v1", "cred-v2"},
		Versions:       []uint64{1, 2},
		Source:         "committed",
	}}
	state.Pending = []checkpoint.PendingCredential{{
		CredentialID:    "cred-v2",
		SourceVersion:   1,
		TargetVersion:   2,
		ReloadBatch:     1,
		SubscriptionIDs: []string{"sub-api"},
	}}
	state.Batches = []checkpoint.BatchProgress{{
		Batch:         1,
		Planned:       []string{"sub-api"},
		Actual:        []string{"sub-api"},
		Failed:        []string{},
		Attempts:      1,
		Confirmed:     true,
		CompletedTick: tick,
	}}
	return state
}
