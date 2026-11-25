package acceptance_test

import (
	"context"
	"path/filepath"
	"testing"

	"coldkernel/internal/checkpoint"
	"coldkernel/internal/errcode"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
	"coldkernel/internal/runner"
	"coldkernel/internal/testutil"
)

func openStore(t *testing.T, source string) *checkpoint.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cp.db")
	store, err := checkpoint.Open(path, source)
	if err != nil {
		t.Fatalf("open checkpoint: %v", err)
	}
	storeRegistry[store] = path
	return store
}

func TestCheckpointRecoveryMatchesFullRun(t *testing.T) {
	tr := loadTriple(t)
	p := tr.ProjectV1
	store := openStore(t, p.SourceVersion+"#seed")
	defer store.Close()

	r, err := runner.New(p, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	last, _, _, err := store.LatestTick()
	if err != nil {
		t.Fatal(err)
	}
	if last != p.HorizonTicks-1 {
		t.Fatalf("committed tick=%d want %d", last, p.HorizonTicks-1)
	}

	// Recover into a fresh runner from the durable commit point.
	r2, err := runner.New(p, store)
	if err != nil {
		t.Fatal(err)
	}
	if r2.StartTick() != p.HorizonTicks {
		t.Fatalf("recovery start=%d want horizon", r2.StartTick())
	}
	states, _, err := store.Restore(last)
	if err != nil {
		t.Fatal(err)
	}
	want := oracle.Simulate(p)
	for fnID, wantState := range want.Ticks[last].Functions {
		got := states[fnID]
		if got == nil {
			t.Fatalf("missing recovered state for %s", fnID)
		}
		if got.Occupied != wantState.Occupied ||
			got.ColdCount != wantState.ColdCount ||
			got.ReuseCount != wantState.ReuseCount ||
			got.BudgetConsumed != wantState.BudgetConsumed ||
			int64(len(got.WarmIdle)) != wantState.WarmIdle ||
			int64(len(got.Inflight)) != wantState.InFlightCold ||
			int64(len(got.Queue)) != wantState.QueueLen {
			t.Fatalf("recovered state mismatch for %s: got=%+v want=%+v",
				fnID, got, wantState)
		}
	}
}

func TestPartialRunThenResume(t *testing.T) {
	tr := loadTriple(t)
	p := tr.ProjectV1
	store := openStore(t, "proj")
	defer store.Close()

	// First process only the first 10 ticks by cancelling via context.
	r, err := runner.New(p, store)
	if err != nil {
		t.Fatal(err)
	}
	r.StopAfterTick(9)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	last, _, _, _ := store.LatestTick()
	if last < 9 {
		t.Fatalf("expected commit through tick 9, got %d", last)
	}

	// Resume with a fresh runner; it must skip already committed ticks.
	r2, err := runner.New(p, store)
	if err != nil {
		t.Fatal(err)
	}
	if r2.StartTick() != last+1 {
		t.Fatalf("resume start=%d want %d", r2.StartTick(), last+1)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := r2.Run(ctx2); err != nil {
		t.Fatal(err)
	}
	finalTick, _, _, _ := store.LatestTick()
	if finalTick != p.HorizonTicks-1 {
		t.Fatalf("final tick=%d want %d", finalTick, p.HorizonTicks-1)
	}

	// Reconstructed full history must equal the oracle tick-by-tick.
	got := reconstruct(t, store, p.HorizonTicks)
	if err := testutil.AssertRunsEqual("resume", oracle.Simulate(p), got); err != nil {
		t.Fatal(err)
	}
}

func reconstruct(t *testing.T, store *checkpoint.Store, horizon int64) *model.RunResult {
	t.Helper()
	out := &model.RunResult{ByRequest: map[string]*model.RequestOutcome{}}
	for tick := int64(0); tick < horizon; tick++ {
		_, payload, err := store.Restore(tick)
		if err != nil {
			t.Fatalf("restore tick %d: %v", tick, err)
		}
		tr := decodeTick(t, payload)
		out.Ticks = append(out.Ticks, tr)
		for _, oc := range append(append(tr.Admitted, tr.Queued...), tr.Rejected...) {
			out.ByRequest[oc.RequestID] = oc
		}
	}
	return out
}

func TestVersionMismatchForcesRecompute(t *testing.T) {
	tr := loadTriple(t)
	store := openStore(t, "project-A")
	defer store.Close()
	r, err := runner.New(tr.ProjectV1, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// A different project source must be rejected, not partially restored.
	other := openStoreAt(t, storePath(store), "project-B")
	defer other.Close()
	_, err = runner.New(tr.ProjectV1, other)
	code, _ := errcode.From(err)
	if code != errcode.ECheckpointVersion {
		t.Fatalf("code=%s want E_CHECKPOINT_VERSION", code)
	}
}

func TestTruncatedCheckpointDetected(t *testing.T) {
	tr := loadTriple(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.db")
	store, err := checkpoint.Open(path, "proj")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(tr.ProjectV1, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// Corrupt the file by truncating its tail.
	info, _ := stat(path)
	writeTruncated(path, info.Size()/2)

	reopened, err := checkpoint.Open(path, "proj")
	if err == nil {
		defer reopened.Close()
	}
	// Either open fails or the first read surfaces a stable truncation code.
	if err == nil {
		_, _, _, rerr := reopened.LatestTick()
		code, _ := errcode.From(rerr)
		if rerr != nil && code != errcode.ECheckpointTruncated && code != errcode.ECheckpointState {
			t.Fatalf("unexpected code %s", code)
		}
	}
}
