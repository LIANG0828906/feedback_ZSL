package colortrace_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ct "colortrace"
	"colortrace/review"
	"colortrace/sample"
	"colortrace/sqlitecheckpoint"
)

func TestSQLiteRestartReuseReviewAndSnapshotInvalidation(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	meta := ct.NewSnapshotMeta(project, project.Truth.Fingerprint)
	store := openSQLiteOrSkip(t, dbPath, meta)
	scheduler := &ct.Scheduler{Store: store, Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 32, PendingTasks: 2}}
	run, err := scheduler.Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.CommitSequence) != 8 {
		t.Fatalf("sqlite commits=%d", len(run.CommitSequence))
	}
	reviewReport, err := review.RunStore(store, project.Truth.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !reviewReport.OK || reviewReport.FirstViolation != nil {
		t.Fatalf("review failed: %+v", reviewReport.FirstViolation)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openSQLiteOrSkip(t, dbPath, meta)
	resume, err := (&ct.Scheduler{Store: reopened, Budget: ct.Budget{MaxParallel: 2, PerWorkerSamples: 32, PendingTasks: 2}}).Run(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Skipped) != 8 || len(resume.CommitSequence) != 8 {
		t.Fatalf("expected full reuse skipped=%d commits=%d", len(resume.Skipped), len(resume.CommitSequence))
	}

	record, err := reopened.Unit(0, "c00x00")
	if err != nil || record == nil {
		t.Fatalf("unit missing: %v %v", record, err)
	}
	record.AlgorithmVersion = "tampered-algorithm"
	if err := reopened.SaveUnitState(*record); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	invalidated := openSQLiteOrSkip(t, dbPath, meta)
	units, err := invalidated.Units(project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := invalidated.CommitLog(project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 0 || len(log) != 0 {
		t.Fatalf("snapshot not invalidated: units=%d log=%d", len(units), len(log))
	}
	if err := invalidated.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteConcurrentCommitsAreSerial(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "serial.sqlite")
	store := openSQLiteOrSkip(t, dbPath, ct.NewSnapshotMeta(project, project.Truth.Fingerprint))
	defer store.Close()
	const count = 20
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.CommitUnit(ct.UnitRecord{
				ProjectID: project.ProjectID, Stage: 99, ChunkID: ct.ChunkID(ct.ChunkCoord{X: i, Y: 0}),
				State: ct.StateCommitted, CacheKey: "k", OutputDigest: "d", ContentHash: "c",
				StageSequenceFingerprint: ct.SequenceFingerprint(project.Stages),
				ParamVersion:             "test", AlgorithmVersion: ct.AlgorithmVersion,
			})
		}(i)
	}
	wg.Wait()
	log, err := store.CommitLog(project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != count {
		t.Fatalf("log length=%d", len(log))
	}
	for i, entry := range log {
		if entry.Seq != int64(i+1) {
			t.Fatalf("non-stable sequence at %d: %+v", i, entry)
		}
	}
}

func TestReviewReportsFirstTamperedField(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	store := ct.NewMemoryStore()
	scheduler := &ct.Scheduler{Store: store, Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 32, PendingTasks: 1}}
	if _, err := scheduler.Run(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	record, err := store.Unit(0, "c00x00")
	if err != nil || record == nil {
		t.Fatalf("unit missing: %v", err)
	}
	var chunk ct.ChunkResult
	if err := json.Unmarshal(record.OutputJSON, &chunk); err != nil {
		t.Fatal(err)
	}
	chunk.Planes["Y"].Codes[0]++
	edited, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	record.OutputJSON = edited
	if err := store.CommitUnit(*record); err != nil {
		t.Fatal(err)
	}
	report, err := review.Run(store, project, project.Truth.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.FirstViolation == nil {
		t.Fatal("expected review violation")
	}
	if report.FirstViolation.Field != "code" || report.FirstViolation.Stage != 0 || report.FirstViolation.Coord == nil {
		t.Fatalf("unexpected violation: %+v", report.FirstViolation)
	}
}

func TestSQLiteHashMismatchInvalidatesSnapshot(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "hash.sqlite")
	meta := ct.NewSnapshotMeta(project, project.Truth.Fingerprint)
	store := openSQLiteOrSkip(t, dbPath, meta)
	if _, err := (&ct.Scheduler{Store: store, Budget: ct.Budget{MaxParallel: 1, PerWorkerSamples: 32, PendingTasks: 1}}).Run(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	record, err := store.Unit(0, "c00x00")
	if err != nil || record == nil {
		t.Fatalf("unit missing: %v", err)
	}
	record.ContentHash = "sha256:tampered"
	if err := store.SaveUnitState(*record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openSQLiteOrSkip(t, dbPath, meta)
	defer reopened.Close()
	units, err := reopened.Units(project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 0 {
		t.Fatalf("expected hash mismatch to purge snapshot, got %d units", len(units))
	}
}

func openSQLiteOrSkip(t *testing.T, path string, meta ct.SnapshotMeta) *sqlitecheckpoint.Store {
	t.Helper()
	store, err := sqlitecheckpoint.Open(path, meta)
	if err != nil {
		if strings.Contains(err.Error(), "requires CGO") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	return store
}
