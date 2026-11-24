package checkpoint_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/generator"
	"performanceexpressions/workspace/session"
	"performanceexpressions/workspace/workbench"
)

func projectForTest(t *testing.T) domain.Project {
	t.Helper()
	return generator.GenerateProjects(generator.DefaultSeed)[0]
}

func testEdit(t *testing.T, project domain.Project, segment int, delta float64, batch string) domain.EditOp {
	t.Helper()
	seg := project.Segments[segment]
	ch, _, ok := seg.ChannelByID("velocity.main")
	if !ok {
		t.Fatal("missing velocity.main")
	}
	return domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: ch.ID, LayerID: ch.Layers[0].ID,
		PointIndex: 1, Beat: ch.Layers[0].Points[1].Beat, Value: ch.Layers[0].Points[1].Value + delta, BatchID: batch,
	}
}

func TestAppendBatchSerialOrderAndDuplicate(t *testing.T) {
	ctx := context.Background()
	project := projectForTest(t)
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess, err := session.New(project, domain.DefaultParameters(), session.Options{Sink: store})
	if err != nil {
		t.Fatal(err)
	}
	first := testEdit(t, project, 0, 0.02, "batch-a")
	if err := sess.ApplyEdit(first); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitBatch(ctx, "batch-a", []string{first.SegmentID}); err != nil {
		t.Fatal(err)
	}
	second := testEdit(t, project, 2, 0.03, "batch-b")
	if err := sess.ApplyEdit(second); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitBatch(ctx, "batch-b", []string{second.SegmentID}); err != nil {
		t.Fatal(err)
	}
	ids, err := store.BatchIDs(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "batch-a" || ids[1] != "batch-b" {
		t.Fatalf("unstable batch order: %v", ids)
	}
	count, err := store.Count(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows, got %d", count)
	}

	seg := project.Segments[0]
	segResult, err := engine.EvaluateSegment(project.SamplesPerBeat, seg, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	cp, err := domain.CheckpointSnapshot(seg, segResult, nil, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	cp.ProjectID = project.ID
	cp.BatchID = "batch-a"
	if _, err := store.AppendBatch(ctx, []domain.Checkpoint{cp}); domain.CodeOf(err) != domain.CodeDuplicate {
		t.Fatalf("expected E_DUP, got %v", err)
	}
}

func TestVersionMismatchInvalidatesAndReplays(t *testing.T) {
	ctx := context.Background()
	project := projectForTest(t)
	params := domain.DefaultParameters()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess, err := session.New(project, params, session.Options{Sink: store})
	if err != nil {
		t.Fatal(err)
	}
	op := testEdit(t, project, 1, 0.05, "version-batch")
	if err := sess.ApplyEdit(op); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitBatch(ctx, "version-batch", []string{op.SegmentID}); err != nil {
		t.Fatal(err)
	}
	expected := sess.CommittedResult()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE checkpoints SET param_version='tampered'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err = checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, report, err := workbench.Recover(ctx, store, project, params)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Invalidated || report.ReplayedBatches != 1 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	count, err := store.Count(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replayed batch not persisted, count=%d", count)
	}
	if cmp, err := engine.CompareResultSets(expected, resumed.CommittedResult()); err != nil || !cmp.Equal {
		t.Fatalf("replayed result differs: %v %+v", err, cmp)
	}
}

func TestProjectHashMismatchRejected(t *testing.T) {
	ctx := context.Background()
	project := projectForTest(t)
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	changed := domain.CloneProject(project)
	changed.Name = "changed name"
	if err := store.EnsureProject(ctx, changed); domain.CodeOf(err) != domain.CodeSchema {
		t.Fatalf("expected E_SCHEMA for changed base project, got %v", err)
	}
}

func TestSnapshotHashTamperCanReplayOperations(t *testing.T) {
	ctx := context.Background()
	project := projectForTest(t)
	params := domain.DefaultParameters()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess, err := session.New(project, params, session.Options{Sink: store})
	if err != nil {
		t.Fatal(err)
	}
	op := testEdit(t, project, 0, 0.01, "hash-batch")
	if err := sess.ApplyEdit(op); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitBatch(ctx, "hash-batch", []string{op.SegmentID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT snapshot_json FROM checkpoints LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot domain.SegmentSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.NoteDensity += 99
	tampered, _ := json.Marshal(snapshot)
	if _, err := db.ExecContext(ctx, `UPDATE checkpoints SET snapshot_json=?`, tampered); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err = checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := store.Load(ctx, project, params)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Invalidated || len(recovery.ReplayBatches) != 1 || len(recovery.ReplayBatches[0].Operations) != 1 {
		t.Fatalf("expected content-hash invalidation with replay ops: %+v", recovery)
	}
}

func TestSummaryTamperInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	project := projectForTest(t)
	params := domain.DefaultParameters()
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess, err := session.New(project, params, session.Options{Sink: store})
	if err != nil {
		t.Fatal(err)
	}
	op := testEdit(t, project, 0, 0.02, "summary-batch")
	if err := sess.ApplyEdit(op); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitBatch(ctx, "summary-batch", []string{op.SegmentID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE checkpoints SET summary_json='{"segment_id":"seg-01","result_hash":"tampered","sample_count":0,"decision_count":0}'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovery, err := store.Load(ctx, project, params)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Invalidated {
		t.Fatalf("summary tamper did not invalidate checkpoint: %+v", recovery)
	}
}

func TestOpenCreatesDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkpoints.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
}
