package macrokit

import (
	"fmt"
	"reflect"
)

func DiffError(frame int64, branchID, field, format string, args ...any) *Error {
	return &Error{Code: EDiff, Frame: frame, BranchID: branchID, Field: field, Message: fmt.Sprintf(format, args...)}
}

func CompareResults(want, got *RunResult) error {
	if want == nil || got == nil {
		return DiffError(0, "", "result", "one result is nil")
	}
	if err := CompareCommitSequences(want.Commits, got.Commits); err != nil {
		return err
	}
	return CompareTraces(want.Frames, got.Frames)
}

func CompareCommitSequences(want, got []CommitRecord) error {
	wantIDs := CommitIDs(want)
	gotIDs := CommitIDs(got)
	if !reflect.DeepEqual(wantIDs, gotIDs) {
		frame := int64(0)
		branch := ""
		if len(want) > 0 {
			frame = want[0].Frame
		}
		for i := 0; i < maxLen(len(wantIDs), len(gotIDs)); i++ {
			var w, g string
			if i < len(wantIDs) {
				w, frame, branch = wantIDs[i], want[i].Frame, want[i].BranchID
			}
			if i < len(gotIDs) {
				g = gotIDs[i]
				if branch == "" {
					branch = got[i].BranchID
					frame = got[i].Frame
				}
			}
			if w != g {
				return DiffError(frame, branch, fmt.Sprintf("commits[%d].branch_id", i), "commit sequence mismatch want=%q got=%q", w, g)
			}
		}
	}
	return nil
}

func CommitIDs(commits []CommitRecord) []string {
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.BranchID)
	}
	return out
}

func CompareTraces(want, got []FrameTrace) error {
	if len(want) != len(got) {
		frame := int64(len(want))
		if len(got) < len(want) {
			frame = int64(len(got))
		}
		return DiffError(frame, "", "frame_count", "trace length mismatch want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Frame != g.Frame {
			return DiffError(g.Frame, "", fmt.Sprintf("frames[%d].frame", i), "frame number mismatch want=%d got=%d", w.Frame, g.Frame)
		}
		frame := w.Frame
		if err := compareStringSlice(frame, "triggered_branches", w.Triggered, g.Triggered); err != nil {
			return err
		}
		if err := compareExecutions(frame, i, w.ExecutedSteps, g.ExecutedSteps); err != nil {
			return err
		}
		if err := compareDelays(frame, i, w.RemainingDelays, g.RemainingDelays); err != nil {
			return err
		}
		if err := compareCancellations(frame, i, w.Cancellations, g.Cancellations); err != nil {
			return err
		}
		if err := compareAdjudications(frame, i, w.Adjudications, g.Adjudications); err != nil {
			return err
		}
		if w.ConsumedOffset != g.ConsumedOffset {
			return DiffError(frame, "", "consumed_offset", "want=%d got=%d", w.ConsumedOffset, g.ConsumedOffset)
		}
		if !reflect.DeepEqual(w.ConditionSnapshot, g.ConditionSnapshot) {
			key, wv, gv := firstMapBoolDiff(w.ConditionSnapshot, g.ConditionSnapshot)
			return DiffError(frame, "", "condition_snapshot."+key, "want=%v got=%v", wv, gv)
		}
		if w.SnapshotHash != g.SnapshotHash {
			return DiffError(frame, "", "snapshot_hash", "want=%s got=%s", w.SnapshotHash, g.SnapshotHash)
		}
	}
	return nil
}

func compareStringSlice(frame int64, field string, want, got []string) error {
	if len(want) != len(got) {
		branch := firstBranch(got)
		return DiffError(frame, branch, field, "length mismatch want=%v got=%v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			return DiffError(frame, got[i], fmt.Sprintf("%s[%d]", field, i), "want=%q got=%q", want[i], got[i])
		}
	}
	return nil
}

func compareExecutions(frame int64, traceIndex int, want, got []StepExecution) error {
	fieldPrefix := fmt.Sprintf("frames[%d].executed_steps", traceIndex)
	if len(want) != len(got) {
		branch := ""
		if len(got) > 0 {
			branch = got[0].BranchID
		}
		return DiffError(frame, branch, fieldPrefix, "length mismatch want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		prefix := fmt.Sprintf("%s[%d]", fieldPrefix, i)
		if w.BranchID != g.BranchID {
			return DiffError(frame, g.BranchID, prefix+".branch_id", "want=%q got=%q", w.BranchID, g.BranchID)
		}
		if w.Frame != g.Frame {
			return DiffError(g.Frame, g.BranchID, prefix+".frame", "want=%d got=%d", w.Frame, g.Frame)
		}
		if w.StepID != g.StepID {
			return DiffError(frame, g.BranchID, prefix+".step_id", "want=%q got=%q", w.StepID, g.StepID)
		}
		if w.Action != g.Action {
			return DiffError(frame, g.BranchID, prefix+".action", "want=%q got=%q", w.Action, g.Action)
		}
		if w.StepIndex != g.StepIndex {
			return DiffError(frame, g.BranchID, prefix+".step_index", "want=%d got=%d", w.StepIndex, g.StepIndex)
		}
	}
	return nil
}

func compareDelays(frame int64, traceIndex int, want, got []DelayState) error {
	prefix := fmt.Sprintf("frames[%d].remaining_delays", traceIndex)
	if len(want) != len(got) {
		branch := ""
		if len(got) > 0 {
			branch = got[0].BranchID
		}
		return DiffError(frame, branch, prefix, "length mismatch want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		p := fmt.Sprintf("%s[%d]", prefix, i)
		if w.BranchID != g.BranchID {
			return DiffError(frame, g.BranchID, p+".branch_id", "want=%q got=%q", w.BranchID, g.BranchID)
		}
		if w.Remaining != g.Remaining {
			return DiffError(frame, g.BranchID, p+".remaining_frames", "want=%d got=%d", w.Remaining, g.Remaining)
		}
		if w.Total != g.Total {
			return DiffError(frame, g.BranchID, p+".total_frames", "want=%d got=%d", w.Total, g.Total)
		}
		if w.Kind != g.Kind {
			return DiffError(frame, g.BranchID, p+".kind", "want=%q got=%q", w.Kind, g.Kind)
		}
	}
	return nil
}

func compareCancellations(frame int64, traceIndex int, want, got []Cancellation) error {
	prefix := fmt.Sprintf("frames[%d].cancellations", traceIndex)
	if len(want) != len(got) {
		branch := ""
		if len(got) > 0 {
			branch = got[0].BranchID
		}
		return DiffError(frame, branch, prefix, "length mismatch want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		p := fmt.Sprintf("%s[%d]", prefix, i)
		if w.BranchID != g.BranchID {
			return DiffError(frame, g.BranchID, p+".branch_id", "want=%q got=%q", w.BranchID, g.BranchID)
		}
		if w.Frame != g.Frame {
			return DiffError(g.Frame, g.BranchID, p+".frame", "want=%d got=%d", w.Frame, g.Frame)
		}
		if w.Reason != g.Reason {
			return DiffError(frame, g.BranchID, p+".reason", "want=%q got=%q", w.Reason, g.Reason)
		}
		if w.SuppressedBy != g.SuppressedBy {
			return DiffError(frame, g.BranchID, p+".suppressed_by", "want=%q got=%q", w.SuppressedBy, g.SuppressedBy)
		}
	}
	return nil
}

func compareAdjudications(frame int64, traceIndex int, want, got []Adjudication) error {
	prefix := fmt.Sprintf("frames[%d].adjudications", traceIndex)
	if len(want) != len(got) {
		return DiffError(frame, "", prefix, "length mismatch want=%d got=%d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		p := fmt.Sprintf("%s[%d]", prefix, i)
		if w.WinnerID != g.WinnerID || w.SuppressedID != g.SuppressedID {
			return DiffError(frame, g.SuppressedID, p, "winner/suppressed mismatch want=%s>%s got=%s>%s", w.WinnerID, w.SuppressedID, g.WinnerID, g.SuppressedID)
		}
		if w.Basis != g.Basis {
			return DiffError(frame, g.WinnerID, p+".basis", "want=%q got=%q", w.Basis, g.Basis)
		}
	}
	return nil
}

func firstMapBoolDiff(want, got map[string]bool) (string, bool, bool) {
	for k, wv := range want {
		gv, ok := got[k]
		if !ok || wv != gv {
			return k, wv, gv
		}
	}
	for k, gv := range got {
		if _, ok := want[k]; !ok {
			return k, false, gv
		}
	}
	return "", false, false
}

func firstBranch(ids []string) string {
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func maxLen(a, b int) int {
	if a > b {
		return a
	}
	return b
}
