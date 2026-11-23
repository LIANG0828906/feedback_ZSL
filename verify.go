package orchestrator

import "fmt"

// Violation describes the first rule violation found by the
// independent verifier.
type Violation struct {
	DialogueID string `json:"dialogue_id"`
	Item       string `json:"item"`
	Detail     string `json:"detail"`
}

func (v *Violation) Error() string {
	return fmt.Sprintf("violation: dialogue=%s item=%s: %s", v.DialogueID, v.Item, v.Detail)
}

// VerifyResults independently re-checks persisted dialogue results
// against the project manifest: character intervals must tile the
// presentation window, cutoffs must respect the limit, resume
// positions must be reachable, and the queue order must be intact.
// It also cross-compares against a fresh orchestration of the main
// pipeline. It returns the first violation, or nil when clean.
func VerifyResults(p *Project, results []*DialogueResult) *Violation {
	if v := verifyQueueOrder(p, results); v != nil {
		return v
	}
	for _, res := range results {
		if v := verifyOne(p, res); v != nil {
			return v
		}
	}
	// Cross-compare with the main pipeline.
	fresh, _, runErr := FullRecompute(p)
	if runErr != nil {
		return &Violation{DialogueID: runErr.DialogueID, Item: "orchestration",
			Detail: "main pipeline failed during verification: " + runErr.Error()}
	}
	byID := map[string]*DialogueResult{}
	for _, r := range results {
		byID[r.DialogueID] = r
	}
	if derr := CompareResults(fresh, byID, queueOrderOf(p)); derr != nil {
		return &Violation{DialogueID: derr.DialogueID, Item: derr.Field, Detail: derr.Message}
	}
	return nil
}

func queueOrderOf(p *Project) []string {
	out := make([]string, len(p.Policy.QueueOrder))
	copy(out, p.Policy.QueueOrder)
	return out
}

func verifyQueueOrder(p *Project, results []*DialogueResult) *Violation {
	if len(results) != len(p.Policy.QueueOrder) {
		return &Violation{Item: "queue_order",
			Detail: fmt.Sprintf("persisted %d results for %d queued dialogues",
				len(results), len(p.Policy.QueueOrder))}
	}
	for i, id := range p.Policy.QueueOrder {
		if results[i].DialogueID != id {
			return &Violation{DialogueID: id, Item: "queue_order",
				Detail: fmt.Sprintf("position %d holds %s", i, results[i].DialogueID)}
		}
		if results[i].QueuePos != i {
			return &Violation{DialogueID: id, Item: "queue_order",
				Detail: fmt.Sprintf("recorded queue position %d, expected %d", results[i].QueuePos, i)}
		}
	}
	return nil
}

func verifyOne(p *Project, res *DialogueResult) *Violation {
	d := p.DialogueByID(res.DialogueID)
	if d == nil {
		return &Violation{DialogueID: res.DialogueID, Item: "reference", Detail: "unknown dialogue"}
	}
	s := res.Schedule
	if s == nil {
		return &Violation{DialogueID: res.DialogueID, Item: "schedule", Detail: "missing schedule"}
	}
	// Character intervals: contiguous, non-overlapping, tiling the window.
	if len(s.Chars) != len(d.Chars()) {
		return &Violation{DialogueID: res.DialogueID, Item: "char_intervals",
			Detail: "interval count does not match character count"}
	}
	if len(s.Chars) > 0 {
		if s.Chars[0].Start != s.PresentStart {
			return &Violation{DialogueID: res.DialogueID, Item: "char_intervals",
				Detail: "first interval does not start at presentation start"}
		}
		for i, c := range s.Chars {
			if c.End < c.Start {
				return &Violation{DialogueID: res.DialogueID, Item: "char_intervals",
					Detail: fmt.Sprintf("interval %d reversed", i)}
			}
			if i > 0 && c.Start != s.Chars[i-1].End {
				return &Violation{DialogueID: res.DialogueID, Item: "char_intervals",
					Detail: fmt.Sprintf("gap or overlap at interval %d", i)}
			}
		}
		if s.Chars[len(s.Chars)-1].End != s.PresentEnd {
			return &Violation{DialogueID: res.DialogueID, Item: "char_intervals",
				Detail: "last interval does not end at presentation end"}
		}
	}
	// Interruption: cutoff within limit, resume reachable.
	if res.Interrupt != nil {
		ir := res.Interrupt
		if ir.CutoffEnd-ir.InterruptAt > p.Policy.CutoffLimitSamples {
			return &Violation{DialogueID: res.DialogueID, Item: "cutoff",
				Detail: fmt.Sprintf("cutoff tail %d exceeds limit %d",
					ir.CutoffEnd-ir.InterruptAt, p.Policy.CutoffLimitSamples)}
		}
		if ir.ResumeIndex < 0 || ir.ResumeIndex > len(s.Chars) {
			return &Violation{DialogueID: res.DialogueID, Item: "resume",
				Detail: "resume index out of range"}
		}
		if ir.ResumeIndex < len(s.Chars) && s.Chars[ir.ResumeIndex].Start != ir.ResumeStart {
			return &Violation{DialogueID: res.DialogueID, Item: "resume",
				Detail: "resume start does not align with the resume character"}
		}
		if ir.ResumeIndex > 0 && s.Chars[ir.ResumeIndex-1].End != ir.ResumeStart {
			return &Violation{DialogueID: res.DialogueID, Item: "resume",
				Detail: "cutoff tail does not connect to the resume start"}
		}
	}
	return nil
}

// VerifyFromStore loads persisted results from the checkpoint store
// and runs the independent verification.
func VerifyFromStore(p *Project, store *CheckpointStore) *Violation {
	results, err := store.ReadResults()
	if err != nil {
		return &Violation{Item: "checkpoint", Detail: err.Error()}
	}
	return VerifyResults(p, results)
}
