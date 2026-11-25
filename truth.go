package macrokit

type Truth struct {
	Seed       int64            `json:"seed"`
	ProjectID  string           `json:"project_id"`
	FrameCount int64            `json:"frame_count"`
	Branches   []ExpectedBranch `json:"branches"`
}

type ExpectedBranch struct {
	ID           string   `json:"id"`
	MacroID      string   `json:"macro_id"`
	Triggered    bool     `json:"triggered"`
	TriggerFrame int64    `json:"trigger_frame"`
	DelayFrames  int64    `json:"delay_frames"`
	Steps        []string `json:"steps"`
	StepFrames   []int64  `json:"step_frames"`
	StepGaps     []int64  `json:"step_gaps"`
	Completed    bool     `json:"completed"`
	Canceled     bool     `json:"canceled"`
	CancelFrame  int64    `json:"cancel_frame,omitempty"`
	CancelReason string   `json:"cancel_reason,omitempty"`
	SuppressedBy string   `json:"suppressed_by,omitempty"`
}

func VerifyTruth(result *RunResult, truth *Truth) error {
	if result == nil {
		return DiffError(0, "", "result", "run result is nil")
	}
	for _, expected := range truth.Branches {
		state := result.Branches[expected.ID]
		if state == nil {
			if expected.Triggered {
				return DiffError(expected.TriggerFrame, expected.ID, "branch", "branch was not committed")
			}
			continue
		}

		if expected.Triggered && state.TriggerFrame != expected.TriggerFrame {
			return DiffError(state.TriggerFrame, expected.ID, "trigger_frame", "want %d got %d", expected.TriggerFrame, state.TriggerFrame)
		}
		if !expected.Triggered && state.TriggerFrame != 0 {
			return DiffError(state.TriggerFrame, expected.ID, "triggered", "branch unexpectedly triggered")
		}

		gotStepIDs := make([]string, 0, len(state.ExecutedSteps))
		gotStepFrames := make([]int64, 0, len(state.ExecutedSteps))
		for _, exec := range state.ExecutedSteps {
			gotStepIDs = append(gotStepIDs, exec.StepID)
			gotStepFrames = append(gotStepFrames, exec.Frame)
		}
		if !equalStrings(expected.Steps, gotStepIDs) {
			frame := expected.TriggerFrame + expected.DelayFrames
			if len(gotStepFrames) > 0 {
				frame = gotStepFrames[0]
			}
			return DiffError(frame, expected.ID, "steps", "step sequence want=%v got=%v", expected.Steps, gotStepIDs)
		}
		if !equalInt64s(expected.StepFrames, gotStepFrames) {
			for i := 0; i < maxLen(len(expected.StepFrames), len(gotStepFrames)); i++ {
				var w, g int64
				if i < len(expected.StepFrames) {
					w = expected.StepFrames[i]
				}
				if i < len(gotStepFrames) {
					g = gotStepFrames[i]
				}
				if w != g {
					return DiffError(g, expected.ID, "step_frames", "step %d frame want=%d got=%d", i, w, g)
				}
			}
		}

		if expected.Completed && state.Status != StatusCompleted {
			return DiffError(state.CommitFrame, expected.ID, "status", "expected completed got %s", state.Status)
		}
		if expected.Canceled {
			if state.Status != StatusCanceled {
				return DiffError(state.CommitFrame, expected.ID, "status", "expected canceled got %s", state.Status)
			}
			if state.CommitFrame != expected.CancelFrame {
				return DiffError(state.CommitFrame, expected.ID, "cancel_frame", "want %d got %d", expected.CancelFrame, state.CommitFrame)
			}
			if expected.CancelReason != "" && state.CancelReason != expected.CancelReason {
				return DiffError(state.CommitFrame, expected.ID, "cancel_reason", "want %q got %q", expected.CancelReason, state.CancelReason)
			}
		}
		if expected.SuppressedBy != "" {
			if state.Status != StatusSuppressed {
				return DiffError(state.CommitFrame, expected.ID, "status", "expected suppressed got %s", state.Status)
			}
			found := false
			for _, adj := range result.Adjudications {
				if adj.SuppressedID == expected.ID && adj.WinnerID == expected.SuppressedBy {
					found = true
				}
			}
			if !found {
				return DiffError(state.TriggerFrame, expected.ID, "suppressed_by", "missing adjudication winner=%s", expected.SuppressedBy)
			}
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
