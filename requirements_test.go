package maskqa

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestDeterministicGenerationAndSchemaError(t *testing.T) {
	a := GenerateDataset()
	b := GenerateDataset()
	if HashCanonical(a) != HashCanonical(b) {
		t.Fatal("generated dataset is not deterministic")
	}
	bad := GenerateDataset()
	bad.Params.RadiusMin = 4
	bad.Params.RadiusMax = 2
	bad.RadiusBand = [2]int{4, 2}
	err := ValidateDataset(bad)
	qe, ok := err.(*QAError)
	if !ok || qe.Code != ESchema || qe.Field != "radius_band.min" {
		t.Fatalf("expected E_SCHEMA radius_band.min, got %#v", err)
	}
}

func TestGradientParameterError(t *testing.T) {
	d := GenerateDataset()
	p := d.Params
	p.OperatorSize = 7
	_, err := ComputeGradientBlock(d.Samples[0], p, d.Samples[0].Blocks[0])
	qe, ok := err.(*QAError)
	if !ok || qe.Code != EPara || qe.Field != "gradient_operator_size" {
		t.Fatalf("expected E_PARA gradient_operator_size, got %#v", err)
	}
}

func TestRadiusMonotonicAndError(t *testing.T) {
	d := GenerateDataset()
	s := d.Samples[0]
	p := d.Params
	p.RadiusMin = 1
	p.RadiusMax = 4
	p.CurvatureBins = 5
	plan, err := BuildRadiusPlan(s, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(plan.Decisions); i++ {
		for j := i + 1; j < len(plan.Decisions); j++ {
			a, b := plan.Decisions[i], plan.Decisions[j]
			if a.Curvature < b.Curvature && a.ErodeRadius < b.ErodeRadius {
				t.Fatalf("lower curvature %+v mapped smaller radius than higher %+v", a, b)
			}
		}
	}
	if _, _, err := MapRadiusByCurvature(math.NaN(), "seg-bad", p); err == nil {
		t.Fatal("expected E_RADIUS for NaN curvature")
	} else if qe, ok := err.(*QAError); !ok || qe.Code != ERadius || qe.ObjectID != "seg-bad" {
		t.Fatalf("expected E_RADIUS seg-bad, got %#v", err)
	}
}

func TestIterationNotConverged(t *testing.T) {
	d := GenerateDataset()
	s := d.Samples[0]
	plan, err := BuildRadiusPlan(s, nil, d.Params)
	if err != nil {
		t.Fatal(err)
	}
	closed := AdaptiveClose(s.Rough, plan.Field)
	p := d.Params
	p.MaxIterations = 1
	p.ChangeThreshold = 0
	out, err := RunOpenIterations(s.Rough, closed, plan.Field, p, s.Blocks[0])
	if err == nil {
		t.Fatal("expected E_ITER")
	}
	qe, ok := err.(*QAError)
	if !ok || qe.Code != EIter || qe.Round != 1 || qe.MinChanges <= 0 {
		t.Fatalf("bad iteration error: %#v out=%+v", err, out)
	}
}

func TestStateMachineIllegalTransition(t *testing.T) {
	got, err := TransitionState(StatePending, StateCommitted)
	if err == nil {
		t.Fatal("expected E_STATE")
	}
	qe, ok := err.(*QAError)
	if !ok || qe.Code != EState {
		t.Fatalf("expected E_STATE, got %#v", err)
	}
	if got != StatePending {
		t.Fatalf("state changed to %s", got)
	}
}

func TestSchedulerBlocksOnPixelBudget(t *testing.T) {
	p := DefaultParams()
	p.MaxParallelBlocks = 1
	p.QueuedPixelBudget = 1
	p.ThreadPixelBufferLimit = 16
	s := NewBoundedScheduler(p)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	jobs := []Job{
		{ID: "1", Pixels: 1, Run: func([]byte) error { started <- struct{}{}; <-release; return nil }},
		{ID: "2", Pixels: 1, Run: func([]byte) error { started <- struct{}{}; return nil }},
	}
	done := make(chan []JobResult, 1)
	go func() { done <- s.Run(context.Background(), jobs) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	select {
	case res := <-done:
		t.Fatalf("scheduler returned before budget release: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case res := <-done:
		if len(res) != 2 || res[0].Err != nil || res[1].Err != nil {
			t.Fatalf("jobs failed: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not finish after release")
	}
	if len(s.Metrics().WaitRecords) == 0 {
		t.Fatal("expected queue budget wait record")
	}
}

func TestCheckpointHashRecoveryAndDiff(t *testing.T) {
	d := GenerateDataset()
	db := filepath.Join(t.TempDir(), "recover.sqlite")
	svc, err := NewService(d, db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	full, err := svc.Run(ctx, RunOptions{Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE commits SET output_json='{"corrupt":true}' WHERE seq=10`); err != nil {
		t.Fatal(err)
	}
	rec, err := st.RecoverIntegrity(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.FirstBadSeq != 10 || rec.DeletedCommits == 0 {
		t.Fatalf("unexpected recovery: %+v", rec)
	}
	rec2, err := st.RecoverIntegrity(ctx, map[Stage]string{StageGradient: "pv-wrong-after-upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	if rec2.FirstBadSeq != 1 || rec2.DeletedCommits == 0 {
		t.Fatalf("expected parameter version mismatch rollback, got %+v", rec2)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	full.SampleReports[0].Final.Data[0] ^= 1
	diff := CompareRunReports(full, full)
	if diff != nil {
		t.Fatalf("same report should compare equal: %v", diff)
	}
	other := *full
	other.SampleReports = append([]*SampleRunReport(nil), full.SampleReports...)
	other.SampleReports[0] = &SampleRunReport{SampleID: full.SampleReports[0].SampleID, Final: full.SampleReports[0].Final.Clone(), Components: full.SampleReports[0].Components}
	other.SampleReports[0].Final.Data[0] ^= 1
	ed := CompareRunReports(full, &other)
	if ed == nil {
		t.Fatal("expected E_DIFF")
	}
	if ed.Code != EDiff || ed.First.Field != "mask" || ed.First.Stage != StageCompose {
		t.Fatalf("bad diff: %#v", ed)
	}
}

func TestFailureIsolationAndRetry(t *testing.T) {
	d := GenerateDataset()
	db := filepath.Join(t.TempDir(), "failure.sqlite")
	svc, err := NewService(d, db)
	if err != nil {
		t.Fatal(err)
	}
	firstNode := NodeRef{SampleID: d.Samples[0].ID, Block: d.Samples[0].Blocks[0], Stage: StageGradient}.Key()
	_, runErr := svc.Run(context.Background(), RunOptions{Mode: "fault", InjectFailureNode: firstNode})
	if runErr == nil {
		t.Fatal("expected injected failure")
	}
	st, err := svc.Store.GetState(context.Background(), d.Samples[0].ID, d.Samples[0].Blocks[0], StageGradient)
	if err != nil || st == nil || st.State != StateFailed {
		t.Fatalf("expected failed state, got %+v err=%v", st, err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	retry, err := NewService(d, db)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := retry.Run(context.Background(), RunOptions{Mode: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.CommitSequence) != 180 {
		t.Fatalf("retry commits=%d want 180", len(rep.CommitSequence))
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
}
