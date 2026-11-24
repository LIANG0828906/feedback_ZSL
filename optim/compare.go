package optim

import (
	"fmt"
	"math"

	"diffrender/errs"
	"diffrender/render"
)

// Divergence localizes the first mismatch between two deterministic runs.
type Divergence struct {
	Field  string `json:"field"`
	Window int    `json:"window"`
	Left   any    `json:"left"`
	Right  any    `json:"right"`
	Detail string `json:"detail"`
}

// CompareResults requires bit-identical parameters, losses, and commit sequence.
func CompareResults(a, b *RunResult) error {
	if a == nil || b == nil {
		return divergenceErr("result", 0, nil, nil, "one run result is nil")
	}
	if len(a.Checkpoints) != len(b.Checkpoints) {
		return divergenceErr("windows.count", min(len(a.Checkpoints), len(b.Checkpoints)), len(a.Checkpoints), len(b.Checkpoints), "checkpoint count differs")
	}
	for i := 0; i < len(a.Checkpoints); i++ {
		ca, cb := a.Checkpoints[i], b.Checkpoints[i]
		if ca.Window != cb.Window || ca.State.Iteration != cb.State.Iteration {
			return divergenceErr("window.sequence", i, ca.Window, cb.Window, "window index or iteration differs")
		}
		if ca.ContentHash != cb.ContentHash {
			if err := compareParameters(ca, cb, i); err != nil {
				return err
			}
			return divergenceErr("window.contentHash", i, ca.ContentHash, cb.ContentHash, "window content hash differs")
		}
	}
	if len(a.Losses) != len(b.Losses) {
		return divergenceErr("losses.length", windowForLoss(min(len(a.Losses), len(b.Losses)), a), len(a.Losses), len(b.Losses), "loss sequence length differs")
	}
	for i := range a.Losses {
		if math.Float64bits(a.Losses[i]) != math.Float64bits(b.Losses[i]) {
			w := windowForLoss(i, a)
			return divergenceErr("losses", w, a.Losses[i], b.Losses[i], fmt.Sprintf("loss %d differs", i))
		}
	}
	if a.FinalScene == nil || b.FinalScene == nil {
		return divergenceErr("finalScene", lastWindow(a), a.FinalScene, b.FinalScene, "final scene missing")
	}
	specsA, err := render.ParameterSpecs(a.FinalScene)
	if err != nil {
		return err
	}
	specsB, err := render.ParameterSpecs(b.FinalScene)
	if err != nil {
		return err
	}
	if len(specsA) != len(specsB) {
		return divergenceErr("parameters.count", lastWindow(a), len(specsA), len(specsB), "different parameter count")
	}
	va := render.ParameterValues(a.FinalScene, specsA)
	vb := render.ParameterValues(b.FinalScene, specsB)
	for i := range va {
		if specsA[i].ID != specsB[i].ID {
			return divergenceErr("parameters.id", lastWindow(a), specsA[i].ID, specsB[i].ID, "parameter identity differs")
		}
		if math.Float64bits(va[i]) != math.Float64bits(vb[i]) {
			return divergenceErr(specsA[i].ID, lastWindow(a), va[i], vb[i], "final parameter differs")
		}
	}
	if len(a.Windows) != len(b.Windows) {
		return divergenceErr("windows.length", min(len(a.Windows), len(b.Windows)), len(a.Windows), len(b.Windows), "window sequence length differs")
	}
	for i := range a.Windows {
		wa, wb := a.Windows[i], b.Windows[i]
		if wa.Index != wb.Index || wa.Iteration != wb.Iteration || wa.ContentHash != wb.ContentHash ||
			math.Float64bits(wa.Loss) != math.Float64bits(wb.Loss) {
			return divergenceErr("window.commitSequence", i, wa, wb, "window commit sequence differs")
		}
	}
	return nil
}

func compareParameters(a, b *Checkpoint, window int) error {
	if len(a.Parameters) != len(b.Parameters) || len(a.ParameterIDs) != len(b.ParameterIDs) {
		return divergenceErr("parameters.count", window, len(a.Parameters), len(b.Parameters), "checkpoint parameter count differs")
	}
	for i := range a.Parameters {
		if a.ParameterIDs[i] != b.ParameterIDs[i] {
			return divergenceErr("parameters.id", window, a.ParameterIDs[i], b.ParameterIDs[i], "checkpoint parameter identity differs")
		}
		if math.Float64bits(a.Parameters[i]) != math.Float64bits(b.Parameters[i]) {
			return divergenceErr(a.ParameterIDs[i], window, a.Parameters[i], b.Parameters[i], "checkpoint parameter differs")
		}
	}
	return nil
}

func windowForLoss(index int, r *RunResult) int {
	if index <= 0 {
		return 0
	}
	for _, w := range r.Windows {
		if w.Iteration >= index {
			return w.Index
		}
	}
	return lastWindow(r)
}

func lastWindow(r *RunResult) int {
	if r != nil && len(r.Windows) > 0 {
		return r.Windows[len(r.Windows)-1].Index
	}
	return 0
}

func divergenceErr(field string, window int, left, right any, detail string) error {
	e := errs.New(errs.CodeDiverge, "<compare>", field, "%s (window %d): left=%v right=%v", detail, window, left, right)
	e.Details = Divergence{Field: field, Window: window, Left: left, Right: right, Detail: detail}
	return e
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
