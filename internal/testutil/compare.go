// Package testutil provides cross-version and cross-engine comparisons used
// by the acceptance tests.
package testutil

import (
	"fmt"

	"coldkernel/internal/model"
)

// TB is the minimal testing surface the comparison helpers need. Both
// *testing.T and lightweight demo adapters satisfy it.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// AssertProjectsEqual verifies the three schema versions load to identical
// function sets, request sequences, limits and warm pool members.
func AssertProjectsEqual(t TB, ps ...*model.Project) {
	t.Helper()
	base := ps[0]
	for i := 1; i < len(ps); i++ {
		cur := ps[i]
		if len(base.Functions) != len(cur.Functions) {
			t.Fatalf("function count mismatch v1 vs v%d", i+1)
		}
		if len(base.Requests) != len(cur.Requests) {
			t.Fatalf("request count mismatch v1 vs v%d", i+1)
		}
		if base.HorizonTicks != cur.HorizonTicks || base.TickMs != cur.TickMs {
			t.Fatalf("horizon/tick mismatch v1 vs v%d", i+1)
		}
		for j, bf := range base.Functions {
			cf := cur.Functions[j]
			if bf.ID != cf.ID ||
				bf.ConcurrencyLimit != cf.ConcurrencyLimit ||
				bf.Reserved != cf.Reserved ||
				bf.WarmPoolSize != cf.WarmPoolSize ||
				bf.ColdMinTicks != cf.ColdMinTicks ||
				bf.ColdMaxTicks != cf.ColdMaxTicks ||
				bf.ColdTimeoutTicks != cf.ColdTimeoutTicks ||
				bf.ColdBudgetRatio != cf.ColdBudgetRatio ||
				bf.ColdBudgetAbsolute != cf.ColdBudgetAbsolute ||
				bf.QueueLimit != cf.QueueLimit ||
				bf.WaitLimitTicks != cf.WaitLimitTicks {
				t.Fatalf("function %d mismatch:\n%+v\n%+v", j, bf, cf)
			}
		}
		for j, br := range base.Requests {
			cr := cur.Requests[j]
			if br.ID != cr.ID || br.FunctionID != cr.FunctionID ||
				br.ArriveTick != cr.ArriveTick || br.Slots != cr.Slots ||
				br.Priority != cr.Priority || br.Tenant != cr.Tenant ||
				br.HoldTicks != cr.HoldTicks {
				t.Fatalf("request %d mismatch:\n%+v\n%+v", j, br, cr)
			}
		}
	}
}

// AssertRunsEqual compares two runs request-by-request and tick-state by
// tick-state, returning a descriptive error on the first divergence.
func AssertRunsEqual(label string, want, got *model.RunResult) error {
	if len(want.Ticks) != len(got.Ticks) {
		return fmt.Errorf("%s: tick count %d != %d", label, len(want.Ticks), len(got.Ticks))
	}
	for i := range want.Ticks {
		wt, gt := want.Ticks[i], got.Ticks[i]
		if err := compareOutcomes(label+":admit", wt.Admitted, gt.Admitted); err != nil {
			return err
		}
		if err := compareOutcomes(label+":queue", wt.Queued, gt.Queued); err != nil {
			return err
		}
		if err := compareOutcomes(label+":reject", wt.Rejected, gt.Rejected); err != nil {
			return err
		}
		for fnID, ws := range wt.Functions {
			gs, ok := gt.Functions[fnID]
			if !ok {
				return fmt.Errorf("%s: missing function state %s tick %d", label, fnID, i)
			}
			if *ws != *gs {
				return fmt.Errorf("%s: state mismatch fn=%s tick=%d\nwant=%+v\ngot =%+v",
					label, fnID, i, ws, gs)
			}
		}
	}
	for id, wo := range want.ByRequest {
		go2, ok := got.ByRequest[id]
		if !ok {
			return fmt.Errorf("%s: missing outcome for %s", label, id)
		}
		if wo.Decision != go2.Decision || wo.ViolatedRule != go2.ViolatedRule ||
			wo.ColdStarted != go2.ColdStarted || wo.ReusedWarm != go2.ReusedWarm ||
			wo.QueueRank != go2.QueueRank {
			return fmt.Errorf("%s: outcome mismatch %s want=%+v got=%+v", label, id, wo, go2)
		}
	}
	return nil
}

func compareOutcomes(label string, want, got []*model.RequestOutcome) error {
	if len(want) != len(got) {
		return fmt.Errorf("%s: count %d != %d", label, len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.RequestID != g.RequestID || w.Decision != g.Decision ||
			w.ViolatedRule != g.ViolatedRule || w.QueueRank != g.QueueRank ||
			w.ColdStarted != g.ColdStarted || w.ReusedWarm != g.ReusedWarm {
			return fmt.Errorf("%s: element %d want=%+v got=%+v", label, i, w, g)
		}
}
	return nil
}
