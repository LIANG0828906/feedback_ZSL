package budget

import (
	"testing"

	"samplingkernel/internal/model"
)

func TestGlobalRejectAndDeclaredOvershoot(t *testing.T) {
	newLedger := func(rule string) *Ledger {
		return NewLedger(&model.Project{Budget: model.BudgetPolicy{
			WindowMS: 1000, GlobalLimitSpans: 10, GlobalExhaustionRule: rule,
		}})
	}

	rejectLedger := newLedger(model.GlobalExhaustReject)
	first := rejectLedger.Decide(1, 0, "tr-1", "svc", map[string]int{"svc": 8}, 8, false)
	second := rejectLedger.Decide(2, 1, "tr-2", "svc", map[string]int{"svc": 4}, 4, false)
	if !first.Accepted || second.Accepted || second.RejectCode != RejectGlobal {
		t.Fatalf("expected global rejection, first=%+v second=%+v", first, second)
	}

	overshootLedger := newLedger(model.GlobalExhaustAllowOvershoot)
	first = overshootLedger.Decide(1, 0, "tr-1", "svc", map[string]int{"svc": 8}, 8, false)
	second = overshootLedger.Decide(2, 1, "tr-2", "svc", map[string]int{"svc": 4}, 4, false)
	if !first.Accepted || !second.Accepted || !second.Overshot || second.GlobalUsedSpans != 12 {
		t.Fatalf("declared overshoot should be accepted and charged: %+v", second)
	}

	forcedLedger := newLedger(model.GlobalExhaustReject)
	forced := forcedLedger.Decide(1, 0, "tr-forced", "svc", map[string]int{"svc": 12}, 12, true)
	if !forced.Accepted || !forced.Overshot || forced.GlobalRemaining >= 0 {
		t.Fatalf("forced retention must be accepted with overshoot: %+v", forced)
	}
}
