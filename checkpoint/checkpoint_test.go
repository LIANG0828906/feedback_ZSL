package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func sampleRun(t *testing.T) (*registry.Project, *registry.RunResult, []registry.Snapshot) {
	t.Helper()
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	document, err := registry.LoadDocument(bundle.Files[registry.SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	project := document.Project
	run, snapshots, err := registry.ComputeProject(project, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return project, run, snapshots
}

func TestSaveLoadResumeAndVersionDecision(t *testing.T) {
	project, full, snapshots := sampleRun(t)
	hash, err := project.ModelHash()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := &Store{Path: path}
	checkpoint := &Checkpoint{
		Snapshot: snapshots[5],
		Steps:    append([]registry.StepResult(nil), full.Steps[:6]...),
	}
	if err := store.Save(checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Meta.CommitSeq != 5 || loaded.Meta.NextTick != 6 {
		t.Fatalf("unexpected checkpoint metadata: %+v", loaded.Meta)
	}
	stepsBefore, _ := json.Marshal(loaded.Steps)
	stepsWant, _ := json.Marshal(full.Steps[:6])
	if !bytes.Equal(stepsBefore, stepsWant) {
		t.Fatal("loaded committed steps differ")
	}
	snapshotBefore, _ := json.Marshal(loaded.Snapshot)
	snapshotWant, _ := json.Marshal(snapshots[5])
	if !bytes.Equal(snapshotBefore, snapshotWant) {
		t.Fatal("loaded snapshot differs")
	}
	status := ResumeStatus{}
	if _, status, err = store.LoadCompatible(Identity{
		EngineVersion:  registry.EngineVersion,
		ProjectVersion: project.ProjectVersion,
		ModelHash:      hash,
	}); err != nil {
		t.Fatal(err)
	}
	if status.Mode != ResumeModeFromCheckpoint {
		t.Fatalf("status = %+v", status)
	}
	if _, status, err = store.LoadCompatible(Identity{
		EngineVersion:  registry.EngineVersion,
		ProjectVersion: project.ProjectVersion,
		ModelHash:      "different",
	}); err != nil {
		t.Fatal(err)
	}
	if status.Mode != ResumeModeFullRecompute || status.Code != registry.CodeCheckpointVersion {
		t.Fatalf("mismatch status = %+v", status)
	}
	engine, err := registry.RestoreEngine(project, &loaded.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := engine.RunAll()
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]registry.StepResult(nil), loaded.Steps...), remaining.Steps...)
	combinedJSON, _ := json.Marshal(combined)
	fullJSON, _ := json.Marshal(full.Steps)
	if !bytes.Equal(combinedJSON, fullJSON) {
		t.Fatal("resumed execution differs from full execution")
	}
}

func TestCheckpointTruncatedCorruptAndAtomicFault(t *testing.T) {
	project, full, snapshots := sampleRun(t)
	hash, _ := project.ModelHash()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := &Store{Path: path}
	first := &Checkpoint{Snapshot: snapshots[2], Steps: append([]registry.StepResult(nil), full.Steps[:3]...)}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("simulated crash before rename")
	store.Fault = func(point FaultPoint) error {
		if point == FaultBeforeRename {
			return fault
		}
		return nil
	}
	second := &Checkpoint{Snapshot: snapshots[8], Steps: append([]registry.StepResult(nil), full.Steps[:9]...)}
	if err := store.Save(second); !errors.Is(err, fault) {
		t.Fatalf("faulted save error = %v", err)
	}
	afterFault, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, afterFault) {
		t.Fatal("failed atomic commit changed the previous checkpoint")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Meta.CommitSeq != 2 {
		t.Fatalf("recovered commit = %d, want 2", loaded.Meta.CommitSeq)
	}
	status := ResumeStatus{}
	if _, status, err = store.LoadCompatible(Identity{EngineVersion: registry.EngineVersion, ProjectVersion: project.ProjectVersion, ModelHash: hash}); err != nil {
		t.Fatal(err)
	}
	if status.Mode != ResumeModeFromCheckpoint {
		t.Fatalf("status = %+v", status)
	}

	truncatedPath := filepath.Join(t.TempDir(), "truncated.db")
	if err := os.WriteFile(truncatedPath, original[:len(original)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = (&Store{Path: truncatedPath}).Load()
	if registry.ErrorCode(err) != registry.CodeCheckpointTruncated {
		t.Fatalf("truncated checkpoint error = %v (%s)", err, registry.ErrorCode(err))
	}

	corrupt := append([]byte(nil), original...)
	needle := []byte("probe_result")
	index := bytes.Index(corrupt, needle)
	if index < 0 {
		t.Fatal("sample checkpoint has no probe_result text to corrupt")
	}
	corrupt[index] = 'X'
	corruptPath := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = (&Store{Path: corruptPath}).Load()
	code := registry.ErrorCode(err)
	if code != registry.CodeCheckpointCorrupt && code != registry.CodeCheckpointTruncated {
		t.Fatalf("corrupt checkpoint error = %v (%s)", err, code)
	}
}
