package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type cancelAfterFirstSave struct {
	inner    CheckpointBackend
	cancel   context.CancelFunc
	saveCall int
}

func TestPhysicallyTruncatedSQLiteReturnsStableCode(t *testing.T) {
	p := loadVersion(t, "v2")
	path := filepath.Join(t.TempDir(), "truncated.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), p, RunOptions{
		Workers: 2, CommitEvery: 5, Checkpoint: store, Resume: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 128 {
		t.Fatalf("checkpoint unexpectedly small: %d", info.Size())
	}
	if err := os.Truncate(path, 128); err != nil {
		t.Fatal(err)
	}
	_, err = OpenCheckpointStore(path)
	if !errorsAsCode(err, CodeCheckpointTruncated) {
		t.Fatalf("physically truncated database returned %v", err)
	}
}

func (c *cancelAfterFirstSave) Reset() error { return c.inner.Reset() }
func (c *cancelAfterFirstSave) Load(p *Project, f string) (*CheckpointSnapshot, ResumeInfo, error) {
	return c.inner.Load(p, f)
}
func (c *cancelAfterFirstSave) Save(s *CheckpointSnapshot) error {
	if err := c.inner.Save(s); err != nil {
		return err
	}
	c.saveCall++
	if c.saveCall == 1 {
		c.cancel()
	}
	return nil
}

func TestSQLiteCheckpointResumeMatchesFullRun(t *testing.T) {
	p := loadVersion(t, "v2")
	full, err := Run(context.Background(), p, RunOptions{Workers: 2, CommitEvery: 4})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	backend := &cancelAfterFirstSave{inner: store, cancel: cancel}
	partial, err := Run(ctx, p, RunOptions{
		Workers: 2, CommitEvery: 5, Checkpoint: backend, Resume: true,
	})
	if ErrorCode(err) != CodeInterrupted {
		t.Fatalf("expected interrupted run, got %v", err)
	}
	if partial == nil || partial.LastCommittedSeq != 5 {
		t.Fatalf("unexpected partial checkpoint: %+v", partial)
	}
	if err := store.SimulateCrashBeforeCommit(&CheckpointSnapshot{LastEventSeq: 999}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta["last_event_seq"] != "5" {
		t.Fatalf("rolled-back write changed committed sequence: %q", meta["last_event_seq"])
	}
	resumed, err := Run(context.Background(), p, RunOptions{
		Workers: 3, CommitEvery: 7, Checkpoint: store, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumedFromSeq != 5 {
		t.Fatalf("expected resume at event 5, got %d", resumed.ResumedFromSeq)
	}
	if !reflect.DeepEqual(withoutTraversal(full.Windows), withoutTraversal(resumed.Windows)) {
		t.Fatalf("resumed window results differ from uninterrupted run")
	}
	if !reflect.DeepEqual(full.Discards, resumed.Discards) {
		t.Fatalf("resumed discard set differs from uninterrupted run")
	}
}

func TestVersionMismatchRebuildsFromStart(t *testing.T) {
	p := loadVersion(t, "v3")
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	backend := &cancelAfterFirstSave{inner: store, cancel: cancel}
	_, err = Run(ctx, p, RunOptions{Workers: 2, CommitEvery: 5, Checkpoint: backend, Resume: true})
	if ErrorCode(err) != CodeInterrupted {
		t.Fatalf("expected interrupted first run: %v", err)
	}
	if err := store.InjectVersionMismatch(); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), p, RunOptions{
		Workers: 2, CommitEvery: 6, Checkpoint: store, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CheckpointFallback {
		t.Fatalf("expected version mismatch fallback")
	}
	if result.ResumedFromSeq != 0 || result.LastCommittedSeq != int64(len(p.Events)) {
		t.Fatalf("mismatch did not rebuild from start: resumed=%d last=%d",
			result.ResumedFromSeq, result.LastCommittedSeq)
	}
	windows, discards, err := store.CheckpointCounts()
	if err != nil {
		t.Fatal(err)
	}
	if windows == 0 || discards == 0 {
		t.Fatalf("rebuilt checkpoint is incomplete: windows=%d discards=%d", windows, discards)
	}
}

func TestTruncatedCheckpointReturnsStableCode(t *testing.T) {
	p := loadVersion(t, "v2")
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Run(context.Background(), p, RunOptions{Workers: 2, CommitEvery: 6, Checkpoint: store, Resume: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.InjectTruncatedCheckpoint(); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Load(p, mustFingerprint(t, p))
	cleanErr := NewBusinessError(CodeCheckpointTruncated, "x")
	if !errorsAsCode(err, CodeCheckpointTruncated) {
		t.Fatalf("expected %s, got %v (sample %s)", CodeCheckpointTruncated, err, cleanErr.Code)
	}
	be, _ := AsBusinessError(err)
	if be.Context["missing_key"] == "" || be.Context["table"] != "fp_meta" {
		t.Fatalf("truncated error lacks locator: %+v", be.Context)
	}
}

func mustFingerprint(t *testing.T, p *Project) string {
	t.Helper()
	fp, err := ProjectFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func errorsAsCode(err error, code string) bool {
	be, ok := AsBusinessError(err)
	return ok && be.Code == code
}
