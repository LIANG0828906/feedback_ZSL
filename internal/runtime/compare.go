package runtime

import (
	"fmt"

	"presentkernel/internal/domain"
)

func resultMap(s *Snapshot) map[string]*domain.PartitionResult {
	m := map[string]*domain.PartitionResult{}
	if s == nil {
		return m
	}
	for _, r := range s.Results {
		m[r.PartitionID] = r
	}
	return m
}

func CompareSnapshots(expectedName string, expected *Snapshot, actualName string, actual *Snapshot) *domain.Error {
	if expected == nil || actual == nil {
		return domain.DiffError("", 0, 0, "snapshot", expectedName, actualName)
	}
	if len(expected.Sequence) != len(actual.Sequence) {
		de := domain.DiffError("", 0, 0, "partition_commit_sequence.length", len(expected.Sequence), len(actual.Sequence))
		de.Message = fmt.Sprintf("path %s vs %s: commit sequence length differs", expectedName, actualName)
		return de
	}
	for i := range expected.Sequence {
		if expected.Sequence[i] != actual.Sequence[i] {
			dialogueID, idx := parsePartition(expected.Sequence[i])
			de := domain.DiffError(dialogueID, idx, i, "partition_commit_sequence", expected.Sequence, actual.Sequence)
			de.Message = fmt.Sprintf("commit sequence first differs at position %d", i)
			return de
		}
	}
	em := resultMap(expected)
	am := resultMap(actual)
	for _, pid := range expected.Sequence {
		a := em[pid]
		b := am[pid]
		if a == nil || b == nil {
			dialogueID, idx := parsePartition(pid)
			return domain.DiffError(dialogueID, idx, 0, "partition_result_presence", a != nil, b != nil)
		}
		if de := comparePartition(a, b); de != nil {
			return de
		}
	}
	return nil
}

func parsePartition(pid string) (string, int) {
	for i := len(pid) - 1; i >= 0; i-- {
		if pid[i] == 'P' {
			var idx int
			_, _ = fmt.Sscanf(pid[i+1:], "%d", &idx)
			return pid[:i], idx
		}
	}
	return pid, 0
}

func diffPartition(r *domain.PartitionResult, frame int, field string, expected, actual any) *domain.Error {
	return domain.DiffError(r.DialogueID, r.PartitionIndex, frame, field, expected, actual)
}

func comparePartition(a, b *domain.PartitionResult) *domain.Error {
	if a.FrameCount != b.FrameCount {
		return diffPartition(a, 0, "frame_count", a.FrameCount, b.FrameCount)
	}
	if len(a.CharIntervals) != len(b.CharIntervals) {
		return diffPartition(a, 0, "char_intervals.length", len(a.CharIntervals), len(b.CharIntervals))
	}
	for i := range a.CharIntervals {
		x, y := a.CharIntervals[i], b.CharIntervals[i]
		if x != y {
			if x.StartFrame != y.StartFrame {
				return diffPartition(a, x.StartFrame, "char_intervals.start_frame", x.StartFrame, y.StartFrame)
			}
			if x.EndFrame != y.EndFrame {
				return diffPartition(a, x.EndFrame, "char_intervals.end_frame", x.EndFrame, y.EndFrame)
			}
			return diffPartition(a, x.StartFrame, "char_intervals", x, y)
		}
	}
	if len(a.CharFrames) != len(b.CharFrames) {
		return diffPartition(a, len(a.CharFrames), "char_frames.length", len(a.CharFrames), len(b.CharFrames))
	}
	for i := range a.CharFrames {
		x, y := a.CharFrames[i], b.CharFrames[i]
		if x.Frame != y.Frame {
			return diffPartition(a, i, "char_frames.frame", x.Frame, y.Frame)
		}
		if x.CharIndex != y.CharIndex {
			return diffPartition(a, i, "char_frames.char_index", x.CharIndex, y.CharIndex)
		}
		if x.RuneStart != y.RuneStart || x.RuneEnd != y.RuneEnd {
			return diffPartition(a, i, "char_frames.range", [2]int{x.RuneStart, x.RuneEnd}, [2]int{y.RuneStart, y.RuneEnd})
		}
		if x.IntervalStartFrame != y.IntervalStartFrame || x.IntervalEndFrame != y.IntervalEndFrame {
			return diffPartition(a, i, "char_frames.interval", [2]int{x.IntervalStartFrame, x.IntervalEndFrame}, [2]int{y.IntervalStartFrame, y.IntervalEndFrame})
		}
	}
	if len(a.LipFrames) != len(b.LipFrames) {
		return diffPartition(a, len(a.LipFrames), "lip_frames.length", len(a.LipFrames), len(b.LipFrames))
	}
	for i := range a.LipFrames {
		x, y := a.LipFrames[i], b.LipFrames[i]
		frame := x.BaseFrame
		if x.SequenceFrame != y.SequenceFrame {
			return diffPartition(a, frame, "lip_frames.sequence_frame", x.SequenceFrame, y.SequenceFrame)
		}
		if x.BaseFrame != y.BaseFrame {
			return diffPartition(a, frame, "lip_frames.base_frame", x.BaseFrame, y.BaseFrame)
		}
		if x.Category != y.Category {
			return diffPartition(a, frame, "lip_frames.category", x.Category, y.Category)
		}
		if x.Family != y.Family {
			return diffPartition(a, frame, "lip_frames.family", x.Family, y.Family)
		}
		if x.Inserted != y.Inserted {
			return diffPartition(a, frame, "lip_frames.inserted", x.Inserted, y.Inserted)
		}
		if x.TransitionID != y.TransitionID {
			return diffPartition(a, frame, "lip_frames.transition_id", x.TransitionID, y.TransitionID)
		}
	}
	if len(a.Transitions) != len(b.Transitions) {
		return diffPartition(a, 0, "transitions.length", len(a.Transitions), len(b.Transitions))
	}
	for i := range a.Transitions {
		x, y := a.Transitions[i], b.Transitions[i]
		if x != y {
			if x.BoundaryFrame != y.BoundaryFrame {
				return diffPartition(a, x.BoundaryFrame, "transitions.boundary_frame", x.BoundaryFrame, y.BoundaryFrame)
			}
			if x.InsertAtSequence != y.InsertAtSequence {
				return diffPartition(a, x.BoundaryFrame, "transitions.insert_at_sequence", x.InsertAtSequence, y.InsertAtSequence)
			}
			return diffPartition(a, x.BoundaryFrame, "transitions", x, y)
		}
	}
	if len(a.Compressions) != len(b.Compressions) {
		return diffPartition(a, 0, "compressions.length", len(a.Compressions), len(b.Compressions))
	}
	for i := range a.Compressions {
		if a.Compressions[i] != b.Compressions[i] {
			return diffPartition(a, a.Compressions[i].Frame, "compressions", a.Compressions[i], b.Compressions[i])
		}
	}
	return nil
}
