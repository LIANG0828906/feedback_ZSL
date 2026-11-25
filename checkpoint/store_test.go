package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/checkpoint"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/sample"
	"github.com/liang/collector-backpressure/testsupport"
)

func hasCode(err error, code bpderr.Code) bool {
	biz, ok := bpderr.As(err)
	return ok && biz.Code == code
}

func TestSQLiteCheckpointSaveAndResume(t *testing.T) {
	ctx := context.Background()
	p := sample.GenerateProject(sample.DefaultSeed)
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")

	store, err := checkpoint.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	budget := engine.DefaultBudget()
	runner, err := engine.NewRunner(budget, engine.WithSink(store))
	if err != nil {
		t.Fatal(err)
	}
	full, err := runner.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := checkpoint.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, err := reopened.LoadMatching(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.LastTick != 13 {
		t.Fatalf("unexpected latest checkpoint: %+v", latest)
	}
	if len(latest.Snapshot.Agents) != len(p.Agents) || len(latest.Snapshot.Partitions) != len(p.Partitions) {
		t.Fatalf("snapshot missing agent/partition state")
	}

	if err := reopened.SaveSnapshot(ctx, p, full.Snapshots[5]); err != nil {
		t.Fatal(err)
	}
	checkpointAt5, err := reopened.LoadMatching(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointAt5.LastTick != 5 {
		t.Fatalf("expected tick5, got %d", checkpointAt5.LastTick)
	}
	resumeRunner, err := engine.NewRunner(budget)
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := resumeRunner.RunFromSnapshot(ctx, p, &checkpointAt5.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(suffix.Result.Ticks) != 8 {
		t.Fatalf("expected ticks6..13 (8 reports), got %d", len(suffix.Result.Ticks))
	}
	combined := append([]model.TickReport{}, full.Result.Ticks[:6]...)
	combined = append(combined, suffix.Result.Ticks...)
	if !testsupport.EqualSemanticTicks(combined, full.Result.Ticks) {
		t.Fatal("resumed suffix does not reproduce full run")
	}
}

func TestVersionMismatchRequiresFullRecompute(t *testing.T) {
	ctx := context.Background()
	p := sample.GenerateProject(sample.DefaultSeed)
	store, err := checkpoint.Open(ctx, filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner, err := engine.NewRunner(engine.DefaultBudget(), engine.WithSink(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, p); err != nil {
		t.Fatal(err)
	}

	other := sample.GenerateProject(sample.DefaultSeed)
	other.Version = "engineering-v2-different"
	if _, err := store.LoadMatching(ctx, other, false); !hasCode(err, bpderr.ECheckpointVersion) {
		t.Fatalf("expected E_CHECKPOINT_VERSION_MISMATCH, got %v", err)
	}
	if err := store.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	cp, err := store.LoadLatest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Fatal("checkpoint was not reset after version mismatch")
	}
	full, err := runner.Run(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Result.Ticks) != other.Ticks {
		t.Fatal("version-mismatched project did not fully recompute")
	}
	latest, err := store.LoadMatching(ctx, other, false)
	if err != nil || latest == nil || latest.ProjectVersion != other.Version {
		t.Fatalf("new version checkpoint not written: %v %v", latest, err)
	}
}

func TestTruncatedCheckpointReturnsStableCode(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "truncated.db")
	store, err := checkpoint.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p := sample.GenerateProject(sample.DefaultSeed)
	runner, err := engine.NewRunner(engine.DefaultBudget(), engine.WithSink(store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 4096 {
		t.Fatalf("valid checkpoint unexpectedly small: %d", len(raw))
	}
	if err := os.WriteFile(path, raw[:120], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Open(ctx, path); !hasCode(err, bpderr.ECheckpointTruncated) {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED for short valid-header file, got %v", err)
	}

	tiny := filepath.Join(t.TempDir(), "tiny.db")
	if err := os.WriteFile(tiny, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Open(ctx, tiny); !hasCode(err, bpderr.ECheckpointTruncated) {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED for tiny file, got %v", err)
	}
}

type cancelAfterTickSink struct {
	store  *checkpoint.Store
	cancel context.CancelFunc
	after  int
}

func (s *cancelAfterTickSink) SaveSnapshot(ctx context.Context, p *model.Project, snap engine.Snapshot) error {
	if err := s.store.SaveSnapshot(ctx, p, snap); err != nil {
		return err
	}
	if snap.Tick == s.after {
		s.cancel()
	}
	return nil
}

func TestCancellationPersistsNearestCompleteTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, err := checkpoint.Open(context.Background(), filepath.Join(t.TempDir(), "signal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sink := &cancelAfterTickSink{store: store, cancel: cancel, after: 3}
	runner, err := engine.NewRunner(engine.DefaultBudget(), engine.WithSink(sink))
	if err != nil {
		t.Fatal(err)
	}
	sim, err := runner.Run(ctx, sample.GenerateProject(sample.DefaultSeed))
	if !hasCode(err, bpderr.EInterrupted) {
		t.Fatalf("expected E_SIGNAL_INTERRUPTED, got %v", err)
	}
	if len(sim.Result.Ticks) != 4 {
		t.Fatalf("expected complete ticks0..3, got %d", len(sim.Result.Ticks))
	}
	cp, err := store.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || cp.LastTick != 3 {
		t.Fatalf("expected durable complete tick3, got %+v err=%v", cp, err)
	}
}
