package macrokit

import "fmt"

type EvalState struct {
	Frame   int64
	Keys    KeySnapshot
	Flags   map[string]bool
	Results map[string]*BranchState
}

// EvaluateCondition evaluates a normalized condition tree against an immutable
// snapshot and already-committed branch results.
func EvaluateCondition(cond *Condition, state EvalState) bool {
	return evalCondition(cond, state, 1, "$", map[string]bool{})
}

// EvaluateBranchCondition returns the decision plus a stable node snapshot.
// The snapshot is suitable for checkpoint hashing and debugging.
func EvaluateBranchCondition(branch *Branch, state EvalState) (bool, map[string]bool) {
	values := make(map[string]bool)
	window := branch.TriggerWindowFrames
	if window <= 0 {
		window = 1
	}
	ok := evalCondition(branch.Condition, state, window, fmt.Sprintf("branch:%s", branch.ID), values)
	values[fmt.Sprintf("branch:%s", branch.ID)] = ok
	return ok, values
}

func evalCondition(cond *Condition, state EvalState, window int64, path string, values map[string]bool) bool {
	if cond == nil {
		values[path] = false
		return false
	}

	var result bool
	switch cond.Kind {
	case CondAll:
		result = true
		for i, child := range cond.Children {
			childPath := fmt.Sprintf("%s/all/%d", path, i)
			if !evalCondition(child, state, window, childPath, values) {
				result = false
			}
		}
	case CondAny:
		for i, child := range cond.Children {
			childPath := fmt.Sprintf("%s/any/%d", path, i)
			if evalCondition(child, state, window, childPath, values) {
				result = true
			}
		}
	case CondNot:
		if len(cond.Children) == 1 {
			result = !evalCondition(cond.Children[0], state, window, path+"/not/0", values)
		}
	case CondKey:
		result = evalKey(cond, state, window)
	case CondFlag:
		result = state.Flags[cond.Flag]
	case CondBranch:
		result = evalBranchResult(cond, state.Results)
	case CondConstant:
		if cond.Value != nil {
			result = *cond.Value
		}
	}

	values[path] = result
	return result
}

func evalKey(cond *Condition, state EvalState, window int64) bool {
	switch cond.KeyState {
	case KeyPressed:
		if window <= 1 {
			return state.Keys.Pressed[cond.Key]
		}
		pressedFrame, ok := state.Keys.PressedAt[cond.Key]
		if !ok {
			return false
		}
		age := state.Frame - pressedFrame
		return age >= 0 && age < window
	case KeyDown, "":
		return state.Keys.Down[cond.Key]
	default:
		return false
	}
}

func evalBranchResult(cond *Condition, results map[string]*BranchState) bool {
	result, ok := results[cond.BranchID]
	if !ok || result == nil {
		return false
	}
	switch cond.Outcome {
	case "", "completed", "success", "succeeded":
		return result.Status == StatusCompleted
	case "triggered", "any", "committed":
		return result.Status != ""
	case "canceled", "cancelled":
		return result.Status == StatusCanceled
	case "suppressed":
		return result.Status == StatusSuppressed
	case "running":
		return result.Status == StatusRunning || result.Status == StatusTriggered
	default:
		return false
	}
}
