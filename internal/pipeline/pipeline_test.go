package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

func TestThreePathConsistency(t *testing.T) {
	sampleDir := t.TempDir()
	if _, appErr := synth.Generate(sampleDir, domain.DefaultBaseSeed); appErr != nil {
		t.Fatalf("generate samples: %v", appErr)
	}
	params := domain.DefaultParams(fontlib.Version)
	result, appErr := pipeline.Compare(context.Background(), sampleDir, domain.DefaultBaseSeed, "sparse_single_960x540", params)
	if appErr != nil {
		t.Fatalf("compare: %v", appErr)
	}
	if !result.Equal {
		t.Fatalf("paths differ: %+v", result.FirstDiff)
	}
	if len(result.Paths) != 3 {
		t.Fatalf("paths = %d", len(result.Paths))
	}
	for _, path := range result.Paths {
		if path.Status != "completed" {
			t.Fatalf("%s status=%s error=%v", path.Name, path.Status, path.Error)
		}
		if path.Stats.TextAccuracy != 1 {
			t.Fatalf("%s accuracy=%v", path.Name, path.Stats.TextAccuracy)
		}
	}
	if result.ComparedCommits != result.Paths[0].CommitCount {
		t.Fatalf("compared commits=%d path count=%d", result.ComparedCommits, result.Paths[0].CommitCount)
	}
}

func TestIllegalStateKeepsState(t *testing.T) {
	sampleDir := t.TempDir()
	if _, appErr := synth.Generate(sampleDir, domain.DefaultBaseSeed); appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	engine := &pipeline.Engine{Store: db, SampleDir: sampleDir, Seed: domain.DefaultBaseSeed}
	params := domain.DefaultParams(fontlib.Version)
	control := pipeline.NewControl(params, 0, "")
	control.SetAutoPauseAfter(1)
	result, appErr := engine.Run(context.Background(), "sparse_single_960x540", params, "live", control, nil)
	if appErr != nil {
		t.Fatalf("pause run: %v", appErr)
	}
	if result.Snapshot.Run.Status != "paused" {
		t.Fatalf("status = %s", result.Snapshot.Run.Status)
	}
	snapshot, snapErr := db.Snapshot(result.RunID)
	if snapErr != nil {
		t.Fatalf("snapshot: %v", snapErr)
	}
	committed := -1
	for _, tile := range snapshot.Tiles {
		if tile.State == domain.TileCommitted {
			committed = tile.Tile.Index
			break
		}
	}
	if committed < 0 {
		t.Fatal("expected one committed tile")
	}
	appErr = db.TransitionTile(result.RunID, committed, domain.TileDetecting)
	if appErr == nil || appErr.Code != domain.CodeState {
		t.Fatalf("expected E_STATE, got %v", appErr)
	}
	after, snapErr := db.Snapshot(result.RunID)
	if snapErr != nil {
		t.Fatalf("snapshot after: %v", snapErr)
	}
	if after.Tiles[committed].State != domain.TileCommitted {
		t.Fatalf("state changed to %s", after.Tiles[committed].State)
	}
	if len(after.Commits) != 1 {
		t.Fatalf("commit count changed: %d", len(after.Commits))
	}
}

func TestIdentityMismatchInvalidatesWholeSnapshot(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/identity.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	params := domain.DefaultParams(fontlib.Version)
	identity := domain.SnapshotIdentity{
		SampleID:         "sample-a",
		ImageHash:        "image-a",
		TemplateVersion:  fontlib.Version,
		ParamVersion:     "param-a",
		AlgorithmVersion: domain.AlgorithmVersion,
		BandWidth:        24,
	}
	run, invalidated, appErr := db.OpenOrCreateRun(identity, "sample-a", params, 5)
	if appErr != nil || len(invalidated) != 0 {
		t.Fatalf("create: %v invalidated=%v", appErr, invalidated)
	}
	if appErr := db.EnsureTiles(run.ID, []domain.Tile{{Index: 0, X: 0, Y: 0, W: 64, H: 64, CoreW: 64, CoreH: 64}}); appErr != nil {
		t.Fatalf("ensure tiles: %v", appErr)
	}
	identity.ImageHash = "image-b"
	identity.ParamVersion = "param-b"
	identity.TemplateVersion = "template-b"
	_, invalidated, appErr = db.OpenOrCreateRun(identity, "sample-a", params, 5)
	if appErr != nil {
		t.Fatalf("reopen: %v", appErr)
	}
	joined := strings.Join(invalidated, ",")
	if !strings.Contains(joined, "image_hash") || !strings.Contains(joined, "param_version") || !strings.Contains(joined, "template_version") {
		t.Fatalf("invalidated = %v", invalidated)
	}
	latest, found, appErr := db.LatestRun("sample-a")
	if appErr != nil || !found {
		t.Fatalf("latest: %v found=%v", appErr, found)
	}
	snapshot, appErr := db.Snapshot(latest.ID)
	if appErr != nil {
		t.Fatalf("snapshot: %v", appErr)
	}
	if len(snapshot.Tiles) != 0 || len(snapshot.Commits) != 0 {
		t.Fatalf("old snapshot survived: tiles=%d commits=%d", len(snapshot.Tiles), len(snapshot.Commits))
	}
}
