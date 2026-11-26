package kernel

import (
	"reflect"
)

const ModeAuto = "auto"

type Engine struct {
	Project *Project
	Budgets Budgets
	Mode    string
}

func RunEngine(p *Project, budgets Budgets, mode string) (*RunReport, error) {
	if mode == "" {
		mode = ModeAuto
	}
	if mode == ModeAuto || mode == "reference" {
		return RunReference(p, budgets)
	}
	if mode != PathExact && mode != PathPreDeduct && mode != PathParallel {
		return nil, NewBizError(CodeFieldConflict, "unknown engine mode %q", mode).
			WithDetail("allowed", []string{ModeAuto, PathExact, PathPreDeduct, PathParallel})
	}
	return runSimulation(p, budgets, mode)
}

// CanonicalState is the mode-independent terminal state used to prove that
// pre-deduction and exact settlement have identical outcomes.
type CanonicalState struct {
	Ticks []CanonicalTick `json:"ticks"`
}

type CanonicalTick struct {
	Tick           int64                 `json:"tick"`
	Ledgers        []TenantState         `json:"ledgers"`
	Nodes          []NodeState           `json:"nodes"`
	Queues         []QueueItem           `json:"queues"`
	AdmissionOrder []AdmissionOrderEntry `json:"admission_order"`
	Decisions      []CanonicalDecision   `json:"decisions"`
}

type CanonicalDecision struct {
	RequestID     string `json:"request_id"`
	Tick          int64  `json:"tick"`
	GrantedBytes  int64  `json:"granted_bytes"`
	QueuedBytes   int64  `json:"queued_bytes"`
	RejectedBytes int64  `json:"rejected_bytes"`
	Decision      string `json:"decision"`
	ReasonCode    string `json:"reason_code,omitempty"`
}

func CanonicalReport(r *RunReport) CanonicalState {
	out := CanonicalState{}
	for _, t := range r.Ticks {
		ct := CanonicalTick{Tick: t.Tick, Ledgers: t.Ledgers, Nodes: t.Nodes, Queues: t.Queues, AdmissionOrder: t.AdmissionOrder}
		for _, a := range t.Allocations {
			ct.Decisions = append(ct.Decisions, CanonicalDecision{
				RequestID: a.RequestID, Tick: a.Tick, GrantedBytes: a.GrantedBytes,
				QueuedBytes: a.QueuedBytes, RejectedBytes: a.RejectedBytes,
				Decision: a.Decision, ReasonCode: a.ReasonCode,
			})
		}
		out.Ticks = append(out.Ticks, ct)
	}
	return out
}

func ReportsEqualState(a, b *RunReport) bool {
	return reflect.DeepEqual(CanonicalReport(a), CanonicalReport(b))
}

type PathEquivalence struct {
	ExactEqual      bool `json:"exact_equal"`
	PreDeductEqual  bool `json:"pre_deduct_equal"`
	ParallelEqual   bool `json:"parallel_equal"`
	AllEqual        bool `json:"all_equal"`
	AllocationCount int  `json:"allocation_count"`
}

// VerifyPathEquivalence runs the same project through exact, pre-deduct and
// parallel paths. Latency budgets are raised only for this state-equivalence
// check so forced exact accounting is not confused with SLA mode selection.
func VerifyPathEquivalence(p *Project, budgets Budgets) (*PathEquivalence, error) {
	highBudget := p.Clone()
	for i := range highBudget.Events {
		if highBudget.Events[i].BudgetMS < 100 {
			highBudget.Events[i].BudgetMS = 100
		}
	}
	exact, err := RunEngine(highBudget, budgets, PathExact)
	if err != nil {
		return nil, err
	}
	pre, err := RunEngine(highBudget, budgets, PathPreDeduct)
	if err != nil {
		return nil, err
	}
	parallel, err := RunEngine(highBudget, budgets, PathParallel)
	if err != nil {
		return nil, err
	}
	eq := &PathEquivalence{
		ExactEqual:     true,
		PreDeductEqual: ReportsEqualState(exact, pre),
		ParallelEqual:  ReportsEqualState(exact, parallel),
	}
	eq.AllEqual = eq.PreDeductEqual && eq.ParallelEqual
	count := 0
	for _, t := range exact.Ticks {
		count += len(t.Allocations)
	}
	eq.AllocationCount = count
	return eq, nil
}
