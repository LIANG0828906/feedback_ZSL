package batching

import (
	"fmt"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// PressureRequest is the deterministic arbitration input for a contested
// material slot or squeezed batch budget.
type PressureRequest struct {
	BatchNumber       int
	Kind              string
	Candidate         model.Part
	Incumbent         *model.Part
	TiePolicy         string
	CandidateRequired int
	BatchUsed         int
	Budget            int
	AvailableSlots    int
	NeededSlots       int
	ConstraintVersion string
}

// PressureResult explains the selected split/defer action.
type PressureResult struct {
	WinnerID string
	Result   string
	Basis    []string
}

// ArbitratePressure resolves pressure according to priority and tie policy.
// Under strict ties it returns E_ARB with both parties and a basis chain.
func ArbitratePressure(req PressureRequest) (PressureResult, error) {
	basis := []string{
		fmt.Sprintf("constraint_version=%s", req.ConstraintVersion),
		fmt.Sprintf("pressure_kind=%s", req.Kind),
		fmt.Sprintf("candidate=%s priority=%d action=%s", req.Candidate.ID, req.Candidate.ArbitrationPriority, req.Candidate.PressureAction),
	}
	parties := []string{req.Candidate.ID}

	winner := req.Candidate
	if req.Incumbent != nil {
		incumbent := *req.Incumbent
		parties = []string{incumbent.ID, req.Candidate.ID}
		basis = append(basis, fmt.Sprintf("incumbent=%s priority=%d", incumbent.ID, incumbent.ArbitrationPriority))
		switch {
		case incumbent.ArbitrationPriority > req.Candidate.ArbitrationPriority:
			winner = incumbent
			basis = append(basis, fmt.Sprintf("priority:%s(%d)>%s(%d)", incumbent.ID, incumbent.ArbitrationPriority, req.Candidate.ID, req.Candidate.ArbitrationPriority))
		case req.Candidate.ArbitrationPriority > incumbent.ArbitrationPriority:
			basis = append(basis, fmt.Sprintf("priority:%s(%d)>%s(%d)", req.Candidate.ID, req.Candidate.ArbitrationPriority, incumbent.ID, incumbent.ArbitrationPriority))
		default:
			basis = append(basis, fmt.Sprintf("priority_tie=%d", req.Candidate.ArbitrationPriority))
			if req.TiePolicy == model.TieStrict {
				basis = append(basis, "tie_policy=strict: no deterministic winner is permitted")
				return PressureResult{}, &xerr.Error{
					Code: xerr.EArb, Message: "cannot arbitrate equal-priority contention",
					Parties: parties, BasisChain: basis,
				}
			}
			if incumbent.ID < req.Candidate.ID {
				winner = incumbent
				basis = append(basis, fmt.Sprintf("tie_break=lexicographic_id winner=%s", incumbent.ID))
			} else {
				basis = append(basis, fmt.Sprintf("tie_break=lexicographic_id winner=%s", req.Candidate.ID))
			}
		}
	}

	if req.Budget > 0 {
		remaining := req.Budget - req.BatchUsed
		basis = append(basis,
			fmt.Sprintf("batch=%d used=%d required=%d remaining=%d budget=%d",
				req.BatchNumber, req.BatchUsed, req.CandidateRequired, remaining, req.Budget),
		)
	}
	if req.NeededSlots > 0 || req.AvailableSlots > 0 {
		basis = append(basis, fmt.Sprintf("material_slots needed=%d available=%d", req.NeededSlots, req.AvailableSlots))
	}

	result := model.PressureSplit
	if winner.ID != req.Candidate.ID {
		result = req.Candidate.PressureAction
		basis = append(basis, fmt.Sprintf("incumbent %s retains batch; candidate action=%s", winner.ID, result))
	} else {
		result = model.PressureSplit
		basis = append(basis, "candidate has arbitration right; v1 kernel opens a fresh batch to avoid rewriting committed offsets")
	}
	if result != model.PressureDefer {
		result = model.PressureSplit
		basis = append(basis, "decision=split_new_batch")
	} else {
		basis = append(basis, "decision=defer_to_later_deterministic_pass")
	}
	return PressureResult{WinnerID: winner.ID, Result: result, Basis: basis}, nil
}
