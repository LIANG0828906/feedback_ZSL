package schedule_test

import (
	"testing"

	"captionx/internal/errs"
	"captionx/internal/model"
	"captionx/internal/schedule"
)

func TestScheduleContiguousExact(t *testing.T) {
	win := model.Window{Start: 1000, End: 5000}
	weights := []int64{1, 2, 3, 1, 2}
	res, err := schedule.Schedule(7, win, weights)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	n := len(weights)
	if len(res.Starts) != n || len(res.Ends) != n {
		t.Fatalf("bad lengths")
	}
	if res.Starts[0] != win.Start {
		t.Fatalf("first start %d != window start %d", res.Starts[0], win.Start)
	}
	for i := 0; i+1 < n; i++ {
		if res.Ends[i] != res.Starts[i+1] {
			t.Fatalf("gap/overlap at %d: end %d != next start %d", i, res.Ends[i], res.Starts[i+1])
		}
	}
	if res.Ends[n-1] != win.End {
		t.Fatalf("last end %d != window end %d", res.Ends[n-1], win.End)
	}
	for i := 0; i < n; i++ {
		if res.Ends[i] <= res.Starts[i] {
			t.Fatalf("non-positive interval at %d", i)
		}
	}
	// Total duration equals the window length.
	var total int64
	for i := 0; i < n; i++ {
		total += res.Ends[i] - res.Starts[i]
	}
	if total != win.Len() {
		t.Fatalf("total %d != window %d", total, win.Len())
	}
	// Rounding residual is strictly below one sample per character.
	if res.ResidualNum >= int64(n)*res.ResidualDen {
		t.Fatalf("residual %d/%d exceeds per-char bound", res.ResidualNum, res.ResidualDen)
	}
}

func TestScheduleDeterministic(t *testing.T) {
	win := model.Window{Start: 42, End: 4242}
	weights := []int64{3, 1, 4, 1, 5, 9, 2, 6}
	a, err1 := schedule.Schedule(1, win, weights)
	b, err2 := schedule.Schedule(1, win, weights)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	for i := range a.Starts {
		if a.Starts[i] != b.Starts[i] || a.Ends[i] != b.Ends[i] {
			t.Fatalf("non-deterministic at %d", i)
		}
	}
}

func TestScheduleErrors(t *testing.T) {
	// Window shorter than character count.
	_, err := schedule.Schedule(3, model.Window{Start: 0, End: 2}, []int64{1, 1, 1})
	if errs.Code(err) != errs.ESched {
		t.Fatalf("code = %s, want E_SCHED", errs.Code(err))
	}
	e := err.(*errs.Error)
	if e.CaptionID != 3 || !e.HasChar {
		t.Fatalf("missing caption/char localisation: %+v", e)
	}
	// Zero weight (denominator contribution).
	_, err = schedule.Schedule(4, model.Window{Start: 0, End: 100}, []int64{1, 0, 1})
	if errs.Code(err) != errs.ESched {
		t.Fatalf("code = %s, want E_SCHED", errs.Code(err))
	}
	if e := err.(*errs.Error); e.CharIdx != 1 {
		t.Fatalf("char idx = %d, want 1", e.CharIdx)
	}
}
