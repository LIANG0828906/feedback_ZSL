package slikernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointRoundTripAndStrictVersion(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	before, err := engine.CachedReport()
	if err != nil {
		t.Fatalf("CachedReport: %v", err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("OpenCheckpoint: %v", err)
	}
	defer store.Close()
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if meta.CommittedSeq != 1 || meta.ObservationSeq != data.Project.LastObservationSeq() {
		t.Fatalf("unexpected checkpoint meta: %+v", meta)
	}
	if meta.BaseCount != 108 || meta.WindowCount != 90 {
		t.Fatalf("unexpected checkpoint counts: %+v", meta)
	}
	snapshot, err := store.Load(context.Background(), engine.RuleVersion())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertReportCoreEqual(t, before, snapshot.Report)

	restored, err := NewEngine(data.Project, testOptions())
	if err != nil {
		t.Fatalf("NewEngine restored: %v", err)
	}
	info, err := restored.RestoreCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if info.Mode != RecoveryResume || info.FullRecompute {
		t.Fatalf("expected resume recovery, got %+v", info)
	}
	restoredReport, err := restored.CachedReport()
	if err != nil {
		t.Fatalf("CachedReport restored: %v", err)
	}
	assertReportCoreEqual(t, before, restoredReport)

	meta2, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("second SaveCheckpoint: %v", err)
	}
	if meta2.CommittedSeq != meta.CommittedSeq+1 {
		t.Fatalf("commit sequence did not advance: %+v", meta2)
	}
}

func TestCheckpointVersionMismatchFullRecompute(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	path := filepath.Join(t.TempDir(), "version.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("OpenCheckpoint: %v", err)
	}
	defer store.Close()
	if _, err := engine.SaveCheckpoint(context.Background(), store); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	edited := CloneProject(data.Project)
	edited.Services[0].SLIs[0].TargetPPM = 980_000
	newEngine, err := NewEngine(edited, testOptions())
	if err != nil {
		t.Fatalf("NewEngine edited: %v", err)
	}
	if _, err := store.Load(context.Background(), newEngine.RuleVersion()); ErrorCode(err) != CodeCheckpointVersion {
		t.Fatalf("expected strict %s, got %v", CodeCheckpointVersion, err)
	}
	info, err := newEngine.RestoreCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("RestoreCheckpoint mismatch: %v", err)
	}
	if info.Mode != RecoveryFullRecompute || info.Reason != CodeCheckpointVersion {
		t.Fatalf("unexpected recovery info: %+v", info)
	}
	recovered, err := newEngine.CachedReport()
	if err != nil {
		t.Fatalf("CachedReport new: %v", err)
	}
	full := compute(t, edited, testOptions())
	assertReportCoreEqual(t, full, recovered)
}

func TestCheckpointCorruptionAndFailedSavePreservePriorCommit(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	path := filepath.Join(t.TempDir(), "rollback.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("OpenCheckpoint: %v", err)
	}
	defer store.Close()
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("initial SaveCheckpoint: %v", err)
	}
	report, _ := engine.CachedReport()
	if _, err := store.Save(context.Background(), report, []*BaseAgg{nil}); ErrorCode(err) != CodeCheckpointCorrupt {
		t.Fatalf("expected rollback error, got %v", err)
	}
	snapshot, err := store.Load(context.Background(), engine.RuleVersion())
	if err != nil {
		t.Fatalf("previous checkpoint became unavailable: %v", err)
	}
	if snapshot.Meta.CommittedSeq != meta.CommittedSeq {
		t.Fatalf("failed save advanced commit seq: %+v", snapshot.Meta)
	}
	assertReportCoreEqual(t, report, snapshot.Report)

	if _, err := store.db.ExecContext(context.Background(),
		`DELETE FROM evaluation_window_checkpoints WHERE rowid = (SELECT rowid FROM evaluation_window_checkpoints LIMIT 1)`); err != nil {
		t.Fatalf("truncate checkpoint state: %v", err)
	}
	if _, err := store.Load(context.Background(), engine.RuleVersion()); ErrorCode(err) != CodeCheckpointCorrupt {
		t.Fatalf("expected %s after truncation, got %v", CodeCheckpointCorrupt, err)
	}
}

func TestPhysicallyTruncatedCheckpointReturnsStableCode(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	path := filepath.Join(t.TempDir(), "truncated.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("OpenCheckpoint: %v", err)
	}
	if _, err := engine.SaveCheckpoint(context.Background(), store); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := os.Truncate(path, info.Size()/3); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	truncated, err := OpenCheckpoint(path)
	if err == nil {
		_, err = truncated.Load(context.Background(), engine.RuleVersion())
		_ = truncated.Close()
	}
	if code := ErrorCode(err); code != CodeCheckpointCorrupt {
		t.Fatalf("expected %s, got %v", CodeCheckpointCorrupt, err)
	}
}

func TestCheckpointOrphanCommitRejected(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	path := filepath.Join(t.TempDir(), "orphan.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("OpenCheckpoint: %v", err)
	}
	defer store.Close()
	meta, err := engine.SaveCheckpoint(context.Background(), store)
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	var encoded string
	if err := store.db.QueryRow(`SELECT state_json FROM base_window_checkpoints LIMIT 1`).Scan(&encoded); err != nil {
		t.Fatalf("read base row: %v", err)
	}
	var base BaseAgg
	if err := json.Unmarshal([]byte(encoded), &base); err != nil {
		t.Fatalf("decode base: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO base_window_checkpoints
		(rule_version, commit_seq, service, sli, aggregation_revision, base_index, state_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		meta.RuleVersion, meta.CommittedSeq+1, base.Service, base.SLI,
		base.AggregationRevision, base.WindowIndex+1000, encoded); err != nil {
		t.Fatalf("insert orphan row: %v", err)
	}
	if _, err := store.Load(context.Background(), engine.RuleVersion()); ErrorCode(err) != CodeCheckpointCorrupt {
		t.Fatalf("expected %s, got %v", CodeCheckpointCorrupt, err)
	}
}
