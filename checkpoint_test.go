package locklab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointResumeMatchesUninterrupted(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	full, err := Run(project, RunOptions{})
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	dir := t.TempDir()
	store, err := OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	partial, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 3})
	if err != nil {
		t.Fatalf("partial run: %v", err)
	}
	if len(partial.Commits) != 3 {
		t.Fatalf("want 3 commits, got %d", len(partial.Commits))
	}
	checkpoint, err := store.Load(project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if checkpoint.CommitSeq != 3 {
		t.Fatalf("checkpoint commit seq = %d, want 3", checkpoint.CommitSeq)
	}
	resumed, info, err := ResumeOrRecompute(project, store, RunOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !info.Resumed {
		t.Fatalf("expected resume, got %+v", info)
	}
	if got, want := resumed.SemanticDigest(), full.SemanticDigest(); got != want {
		t.Fatalf("resumed digest %s != full %s", got, want)
	}
	if len(resumed.Steps) != len(full.Steps) {
		t.Fatalf("step count differs after resume")
	}
}

func TestCheckpointBodyIsNotVisibleBeforeMarker(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	dir := t.TempDir()
	store, _ := OpenCheckpointStore(dir)
	result, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	before, err := store.Load(project)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}
	if err := store.SaveWithOptions(result.FinalState, result.Commits, CheckpointSaveOptions{FailAfterBody: true}); ErrorCodeOf(err) != EInterrupted {
		t.Fatalf("want interrupted after body, got %v", err)
	}
	after, err := store.Load(project)
	if err != nil {
		t.Fatalf("load after orphan body: %v", err)
	}
	if after.CommitSeq != before.CommitSeq {
		t.Fatalf("orphan body changed commit point: before=%d after=%d", before.CommitSeq, after.CommitSeq)
	}
}

func TestCheckpointCorruptionAndVersionMismatch(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	dir := t.TempDir()
	store, _ := OpenCheckpointStore(dir)
	_, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	other := GenerateProject(7, DefaultGenerateOptions())
	if _, err := store.Load(other); ErrorCodeOf(err) != EProjectVersion {
		t.Fatalf("want project version mismatch, got %v", err)
	}

	marker := filepath.Join(dir, checkpointMarkerName)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(project); ErrorCodeOf(err) != ECheckpointTruncated {
		t.Fatalf("want truncated checkpoint, got %v", err)
	}
}

func TestCheckpointVersionMismatchFallsBackToReplay(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	dir := t.TempDir()
	store, _ := OpenCheckpointStore(dir)
	_, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	marker := filepath.Join(dir, checkpointMarkerName)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), `"store_version": 1`, `"store_version": 99`, 1)
	if err := os.WriteFile(marker, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	result, info, err := ResumeOrRecompute(project, store, RunOptions{})
	if err != nil {
		t.Fatalf("fallback replay: %v", err)
	}
	if info.Resumed || info.FallbackCode != ECheckpointVersion {
		t.Fatalf("want version fallback, got %+v", info)
	}
	if len(result.Commits) == 0 {
		t.Fatalf("fallback replay produced no commits")
	}
	full, err := Run(project, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("version fallback replay differs from full recomputation")
	}
}

func TestCheckpointSQLExportOrdersCommitMarkerLast(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	dir := t.TempDir()
	store, _ := OpenCheckpointStore(dir)
	_, err := Run(project, RunOptions{OnCommit: store.Observer(), StopAfterCommits: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	path := filepath.Join(dir, "checkpoint.sql")
	if err := store.ExportSQL(path); err != nil {
		t.Fatalf("export sql: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(data)
	if !strings.Contains(sql, "INSERT INTO lock_state") ||
		!strings.Contains(sql, "INSERT INTO checkpoint_meta") ||
		!strings.Contains(sql, "INSERT INTO committed_step") {
		t.Fatalf("missing expected SQL tables")
	}
	if strings.Index(sql, "INSERT INTO lock_state") > strings.Index(sql, "INSERT INTO checkpoint_meta") {
		t.Fatalf("checkpoint_meta must be inserted after lock state")
	}
}
