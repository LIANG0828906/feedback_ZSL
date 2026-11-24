package runner

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"

	"economycore/checkpoint"
	"economycore/model"
)

func runnerModel(name string) *model.Model {
	return &model.Model{
		ID:                "runner-test",
		Name:              name,
		SourceVersion:     model.SchemaV3,
		ConversionVersion: model.DefaultConversionVersion,
		CycleMs:           1000,
		WindowCycles:      2,
		WindowCount:       3,
		RateScale:         10000,
		WorkerScale:       10000,
		WorkersTotal:      1,
		WorkersMaximum:    100,
		Resources: []model.Resource{
			{ID: "wood", Name: "Wood", Kind: model.KindRaw},
		},
		InitialStocks: map[string]int64{"wood": 0},
		Capacities:    map[string]int64{"wood": 1000},
		Nodes: []*model.Node{
			{
				ID:                    "forest",
				Name:                  "Forest",
				Built:                 true,
				Level:                 1,
				Recipe:                "logs",
				RateExpr:              model.ConstExpr(10000),
				WorkerEfficiencyFixed: 10000,
				Workers:               1,
				Recipes: []model.Recipe{
					{ID: "logs", Inputs: map[string]int64{}, Outputs: map[string]int64{"wood": 2}},
				},
				BuildCost: map[string]int64{},
				Upgrade: model.Upgrade{
					MaxLevel:     2,
					RateAddFixed: 10000,
					CapacityAdd:  map[string]int64{},
					Cost:         map[string]int64{},
				},
			},
		},
		Intents: []model.Intent{},
		Presence: model.Presence{
			WorkersMaximum:  true,
			OverflowPolicy:  true,
			UpgradeDiscount: true,
		},
		RoundMode: "floor",
	}
}

func TestRestartResumeAndCompletedReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(runnerModel("original"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.RunNextWindow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Window != 0 || len(first.Cycles) != 2 {
		t.Fatalf("unexpected first window: %+v", first)
	}
	if count, _ := store.Count(context.Background()); count != 1 {
		t.Fatalf("expected one checkpoint, got %d", count)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err = New(runnerModel("original"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recovery := r.Recovery()
	if recovery.ValidWindows != 1 || recovery.Complete || recovery.LastWindow != 0 {
		t.Fatalf("unexpected recovery: %+v", recovery)
	}
	report, err := r.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.CommittedWindows != 3 || len(report.Cycles) != 6 {
		t.Fatalf("unexpected completed report: complete=%v windows=%d cycles=%d", report.Complete, report.CommittedWindows, len(report.Cycles))
	}
	if report.FinalState.Stocks["wood"] != 12 {
		t.Fatalf("final wood=12, got %d", report.FinalState.Stocks["wood"])
	}
}

func TestGlobalInvalidationOnContentOrVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(runnerModel("original"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	changed := runnerModel("changed")
	r, err = New(changed, store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recovery := r.Recovery()
	if !recovery.Invalidated || recovery.ValidWindows != 0 {
		t.Fatalf("expected global invalidation, got %+v", recovery)
	}
	if count, _ := store.Count(context.Background()); count != 0 {
		t.Fatalf("snapshots must be discarded, got %d", count)
	}
}

func TestFailureIsolationAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var failOnce atomic.Bool
	failOnce.Store(true)
	r, err := New(runnerModel("fault"), store, Options{
		Fault: func(window int, cycle int64) error {
			if window == 1 && failOnce.CompareAndSwap(true, false) {
				return model.NewError(model.EUnit, "/injected", "injected failure").WithWindow(window).WithNode("forest", "")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = r.RunNextWindow(context.Background())
	if !model.IsCode(err, model.EUnit) {
		t.Fatalf("expected injected E_UNIT, got %v", err)
	}
	count, _ := store.Count(context.Background())
	if count != 1 {
		t.Fatalf("failed window must not advance checkpoints, got %d", count)
	}
	failures, err := store.Failures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || failures[0].Window != 1 || failures[0].Code != string(model.EUnit) {
		t.Fatalf("unexpected failure record: %+v", failures)
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if count, _ := store.Count(context.Background()); count != 2 {
		t.Fatalf("retry should commit window 1, got %d", count)
	}
	failures, _ = store.Failures(context.Background())
	if len(failures) != 0 {
		t.Fatalf("successful retry should clear failure, got %+v", failures)
	}
}

func TestCancellationLeavesOnlyCommittedWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(runnerModel("cancel"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := r.RunAll(ctx)
	if !model.IsCode(err, model.ECancel) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if report.Complete || report.CommittedWindows != 1 {
		t.Fatalf("cancellation must report partial state: %+v", report)
	}
	store.Close()

	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err = New(runnerModel("cancel"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err = r.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.CommittedWindows != 3 {
		t.Fatalf("resume did not complete: %+v", report)
	}
}

func TestOutOfOrderCommitRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := New(runnerModel("order"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	fp := r.Fingerprint()
	_, err = store.CommitWindow(context.Background(), checkpoint.Record{Window: 1, Fingerprint: fp, StateJSON: []byte(`{}`), ResultJSON: []byte(`{}`)})
	if !model.IsCode(err, model.EWindow) {
		t.Fatalf("expected out-of-order E_WINDOW, got %v", err)
	}
}

func TestCorruptCheckpointStateInvalidatesWholePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(runnerModel("corrupt"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunNextWindow(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE checkpoints SET state_json = '{}' WHERE window_no = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err = New(runnerModel("corrupt"), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recovery := r.Recovery()
	if !recovery.Invalidated || recovery.ValidWindows != 0 {
		t.Fatalf("corrupt checkpoint should invalidate all windows: %+v", recovery)
	}
	if count, _ := store.Count(context.Background()); count != 0 {
		t.Fatalf("corrupt snapshot should be removed, got %d", count)
	}
}
