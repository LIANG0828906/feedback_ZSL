package checkpoint

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

func TestStateMachineCommitAndFrozenStages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meta := testMeta("state-machine")
	if _, err := store.EnsureSession(ctx, meta, ModeIncremental); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StateInProgress, meta.ParamsVersion, meta.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StatePending, meta.ParamsVersion, meta.AlgorithmVersion)
	if apperr.Code(err) != apperr.CodeState {
		t.Fatalf("illegal transition error = %v", err)
	}
	state, ok, err := store.GetStage(ctx, meta.SessionID, 0, model.StageGrowth)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if state.State != model.StateInProgress {
		t.Fatalf("illegal transition changed state to %s", state.State)
	}

	output := []byte{1, 2, 3, 4}
	rec, err := store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: output, ParamVersion: meta.ParamsVersion, AlgorithmVersion: meta.AlgorithmVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Seq != 0 || rec.ContentHash != model.HashBytes(output) {
		t.Fatalf("bad commit record: %+v", rec)
	}
	idempotent, err := store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: output, ParamVersion: meta.ParamsVersion, AlgorithmVersion: meta.AlgorithmVersion,
	})
	if err != nil || idempotent.Seq != 0 {
		t.Fatalf("idempotent commit = %+v, %v", idempotent, err)
	}
	_, err = store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: []byte{9}, ParamVersion: meta.ParamsVersion, AlgorithmVersion: meta.AlgorithmVersion,
	})
	if apperr.Code(err) != apperr.CodeState {
		t.Fatalf("frozen stage commit error = %v", err)
	}
	commits, err := store.Commits(ctx, meta.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d", len(commits))
	}
}

func TestVersionAndHashErrors(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meta := testMeta("versions")
	if _, err := store.EnsureSession(ctx, meta, ModeIncremental); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StateInProgress, meta.ParamsVersion, meta.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	_, err := store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: []byte{1}, ParamVersion: 2, AlgorithmVersion: meta.AlgorithmVersion,
	})
	if apperr.Code(err) != apperr.CodeVersion {
		t.Fatalf("version mixing error = %v", err)
	}
	_, err = store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: []byte{1}, ContentHash: "wrong", ParamVersion: meta.ParamsVersion,
		AlgorithmVersion: meta.AlgorithmVersion,
	})
	if apperr.Code(err) != apperr.CodeMeta || apperr.Field(err) != "content_hash" {
		t.Fatalf("content hash error = %v", err)
	}
}

func TestRestartInvalidatesWholeSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meta := testMeta("restart")
	if _, err := store.EnsureSession(ctx, meta, ModeIncremental); err != nil {
		t.Fatal(err)
	}
	commitGrowth(t, ctx, store, meta)

	changed := meta
	changed.ImageSHA256 = "different-image"
	result, err := store.EnsureSession(ctx, changed, ModeResume)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset || result.ResetReason == "" {
		t.Fatalf("expected reset, got %+v", result)
	}
	stages, err := store.AllStages(ctx, changed.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if stage.State != model.StatePending {
			t.Fatalf("stage after reset = %s", stage.State)
		}
	}
	commits, err := store.Commits(ctx, changed.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 0 {
		t.Fatalf("commits after reset = %d", len(commits))
	}
}

func TestRestartInvalidatesOnOutputHashCorruption(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meta := testMeta("hash-corruption")
	if _, err := store.EnsureSession(ctx, meta, ModeIncremental); err != nil {
		t.Fatal(err)
	}
	commitGrowth(t, ctx, store, meta)
	if _, err := store.DB().ExecContext(ctx, `UPDATE tile_stages SET output=? WHERE session_id=? AND tile_index=0 AND stage_name='growth'`,
		[]byte{9, 9, 9}, meta.SessionID); err != nil {
		t.Fatal(err)
	}
	result, err := store.EnsureSession(ctx, meta, ModeResume)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reset {
		t.Fatalf("expected reset after hash corruption, got %+v", result)
	}
}

func TestInterruptedStageBecomesRetryableFailed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	meta := testMeta("interrupted")
	if _, err := store.EnsureSession(ctx, meta, ModeIncremental); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StateInProgress, meta.ParamsVersion, meta.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	result, err := store.EnsureSession(ctx, meta, ModeResume)
	if err != nil {
		t.Fatal(err)
	}
	if result.Interrupted != 1 {
		t.Fatalf("interrupted count = %d", result.Interrupted)
	}
	stage, _, err := store.GetStage(ctx, meta.SessionID, 0, model.StageGrowth)
	if err != nil {
		t.Fatal(err)
	}
	if stage.State != model.StateFailed || stage.ErrorCode != "E_INTERRUPTED" {
		t.Fatalf("interrupted stage = %+v", stage)
	}
	if err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StateInProgress, meta.ParamsVersion, meta.AlgorithmVersion); err != nil {
		t.Fatalf("retry transition failed: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testMeta(sessionID string) Meta {
	params := model.Params{
		ColorTolerance: 20, GradientThreshold: 8, StructureElementSize: 5,
		Seeds: []model.Point{{X: 5, Y: 5}},
	}
	paramsJSON, _ := json.Marshal(params)
	return Meta{
		SessionID: sessionID, SampleID: "sample", ImageSHA256: "image-hash",
		TruthFingerprint: "truth-hash", ParamsVersion: 1, ParamsHash: model.HashJSON(params),
		ParamsJSON:       string(paramsJSON),
		AlgorithmVersion: model.AlgorithmVersion, Width: 64, Height: 64, TileSize: 32,
		TilesX: 2, TilesY: 2,
	}
}

func commitGrowth(t *testing.T, ctx context.Context, store *Store, meta Meta) {
	t.Helper()
	if err := store.Transition(ctx, meta.SessionID, 0, model.StageGrowth, model.StateInProgress, meta.ParamsVersion, meta.AlgorithmVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, CommitInput{
		SessionID: meta.SessionID, TileIndex: 0, Stage: model.StageGrowth,
		Output: []byte{1, 2, 3}, ParamVersion: meta.ParamsVersion, AlgorithmVersion: meta.AlgorithmVersion,
	}); err != nil {
		t.Fatal(err)
	}
}
