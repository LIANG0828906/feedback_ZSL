package gait

import (
	"fmt"
	"sort"
)

type Mismatch struct {
	Frame int    `json:"frame"`
	Cycle int    `json:"cycle"`
	Foot  string `json:"foot,omitempty"`
	Field string `json:"field"`
	Left  any    `json:"left"`
	Right any    `json:"right"`
}

// CompareResults compares phase annotations, root trajectories, foot poses,
// event timelines and correction records. It returns E_DIVERGE with the first
// mismatch in frame order.
func CompareResults(left, right Result) error {
	if left.SourceID != right.SourceID {
		return diverge(0, -1, "", "source_id", left.SourceID, right.SourceID)
	}
	if left.TargetID != right.TargetID {
		return diverge(0, -1, "", "target_id", left.TargetID, right.TargetID)
	}
	leftEvents := groupEvents(left.Events)
	rightEvents := groupEvents(right.Events)
	leftCorrections := groupCorrections(left.Corrections)
	rightCorrections := groupCorrections(right.Corrections)
	commonFrames := min(len(left.Frames), len(right.Frames))
	for frame := 0; frame < commonFrames; frame++ {
		lf := left.Frames[frame]
		rf := right.Frames[frame]
		if lf.Frame != rf.Frame {
			return diverge(frame, cycleForTargetFrame(left, frame), "", "frame", lf.Frame, rf.Frame)
		}
		if lf.SourceFrame != rf.SourceFrame {
			return diverge(frame, cycleForTargetFrame(left, frame), "", "source_frame", lf.SourceFrame, rf.SourceFrame)
		}
		if lf.Phase.Global != rf.Phase.Global {
			return diverge(frame, cycleForTargetFrame(left, frame), "", "phase.global", lf.Phase.Global, rf.Phase.Global)
		}
		if lf.Phase.Left != rf.Phase.Left {
			return diverge(frame, cycleForTargetFrame(left, frame), left.FootID(0), "phase.left", lf.Phase.Left, rf.Phase.Left)
		}
		if lf.Phase.Right != rf.Phase.Right {
			return diverge(frame, cycleForTargetFrame(left, frame), left.FootID(1), "phase.right", lf.Phase.Right, rf.Phase.Right)
		}
		if lf.Root != rf.Root {
			field := firstVecDifference("root", lf.Root, rf.Root)
			return diverge(frame, cycleForTargetFrame(left, frame), "", field, lf.Root, rf.Root)
		}
		if len(lf.Feet) != len(rf.Feet) {
			return diverge(frame, cycleForTargetFrame(left, frame), "", "feet.count", len(lf.Feet), len(rf.Feet))
		}
		for footIndex := 0; footIndex < len(lf.Feet); footIndex++ {
			lfFoot := lf.Feet[footIndex]
			rfFoot := rf.Feet[footIndex]
			if lfFoot.Foot != rfFoot.Foot {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, "foot.id", lfFoot.Foot, rfFoot.Foot)
			}
			if field := firstVecDifference("foot.position", lfFoot.Position, rfFoot.Position); field != "" {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, field, lfFoot.Position, rfFoot.Position)
			}
			if lfFoot.Contact != rfFoot.Contact {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, "foot.contact", lfFoot.Contact, rfFoot.Contact)
			}
			if field := firstVecDifference("foot.speed", lfFoot.Speed, rfFoot.Speed); field != "" {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, field, lfFoot.Speed, rfFoot.Speed)
			}
			if len(lfFoot.Toes) != len(rfFoot.Toes) {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, "foot.toes.count", len(lfFoot.Toes), len(rfFoot.Toes))
			}
			for toeIndex := range lfFoot.Toes {
				if lfFoot.Toes[toeIndex] != rfFoot.Toes[toeIndex] {
					return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot,
						fmt.Sprintf("foot.toes.%d", toeIndex), lfFoot.Toes[toeIndex], rfFoot.Toes[toeIndex])
				}
			}
			if len(lfFoot.Joints) != len(rfFoot.Joints) {
				return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot, "foot.joints.count", len(lfFoot.Joints), len(rfFoot.Joints))
			}
			for jointIndex := range lfFoot.Joints {
				if lfFoot.Joints[jointIndex] != rfFoot.Joints[jointIndex] {
					return diverge(frame, cycleForTargetFrame(left, frame), lfFoot.Foot,
						fmt.Sprintf("foot.joints.%d", jointIndex), lfFoot.Joints[jointIndex], rfFoot.Joints[jointIndex])
				}
			}
		}
		if mismatch := compareEventsAt(leftEvents[frame], rightEvents[frame], frame, cycleForTargetFrame(left, frame)); mismatch != nil {
			return mismatch
		}
		if mismatch := compareCorrectionsAt(leftCorrections[frame], rightCorrections[frame], frame, cycleForTargetFrame(left, frame)); mismatch != nil {
			return mismatch
		}
	}
	if len(left.Frames) != len(right.Frames) {
		return diverge(commonFrames, cycleForTargetFrame(left, commonFrames), "", "frame_count", len(left.Frames), len(right.Frames))
	}
	// Events may exist beyond a missing frame range only when malformed; retain
	// deterministic detection in that case.
	allEventFrames := unionEventFrames(leftEvents, rightEvents)
	for _, frame := range allEventFrames {
		if frame < commonFrames {
			continue
		}
		if mismatch := compareEventsAt(leftEvents[frame], rightEvents[frame], frame, cycleForTargetFrame(left, frame)); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

func diverge(frame, cycle int, foot, field string, left, right any) error {
	e := errAt(CodeDiverge, fmt.Sprintf("first mismatch: field=%s left=%v right=%v", field, left, right), frame)
	e.Cycle = cycle
	e.Foot = foot
	e.Field = field
	return e
}

func firstVecDifference(prefix string, a, b Vec3) string {
	if a.X != b.X {
		return prefix + ".x"
	}
	if a.Y != b.Y {
		return prefix + ".y"
	}
	if a.Z != b.Z {
		return prefix + ".z"
	}
	return ""
}

func groupEvents(events []FootEvent) map[int][]FootEvent {
	result := make(map[int][]FootEvent)
	for _, event := range events {
		result[event.Frame] = append(result[event.Frame], event)
	}
	for frame := range result {
		sort.Slice(result[frame], func(i, j int) bool {
			if result[frame][i].Foot != result[frame][j].Foot {
				return result[frame][i].Foot < result[frame][j].Foot
			}
			return result[frame][i].Type < result[frame][j].Type
		})
	}
	return result
}

func compareEventsAt(left, right []FootEvent, frame, cycle int) error {
	if len(left) != len(right) {
		return diverge(frame, cycle, firstEventFoot(left, right), "events.count", len(left), len(right))
	}
	for i := range left {
		if left[i] != right[i] {
			foot := left[i].Foot
			if foot == "" {
				foot = right[i].Foot
			}
			return diverge(frame, cycle, foot, "event."+left[i].Type, left[i], right[i])
		}
	}
	return nil
}

func firstEventFoot(left, right []FootEvent) string {
	if len(left) > 0 {
		return left[0].Foot
	}
	if len(right) > 0 {
		return right[0].Foot
	}
	return ""
}

func groupCorrections(corrections []Correction) map[int][]Correction {
	result := make(map[int][]Correction)
	for _, correction := range corrections {
		result[correction.Frame] = append(result[correction.Frame], correction)
	}
	for frame := range result {
		sort.Slice(result[frame], func(i, j int) bool {
			return result[frame][i].FieldlessKey() < result[frame][j].FieldlessKey()
		})
	}
	return result
}

func compareCorrectionsAt(left, right []Correction, frame, cycle int) error {
	if len(left) != len(right) {
		foot := ""
		if len(left) > 0 {
			foot = left[0].Foot
		} else if len(right) > 0 {
			foot = right[0].Foot
		}
		return diverge(frame, cycle, foot, "corrections.count", len(left), len(right))
	}
	for i := range left {
		if left[i] != right[i] {
			return diverge(frame, cycle, left[i].Foot, "correction", left[i], right[i])
		}
	}
	return nil
}

func unionEventFrames(left, right map[int][]FootEvent) []int {
	set := make(map[int]bool)
	for frame := range left {
		set[frame] = true
	}
	for frame := range right {
		set[frame] = true
	}
	frames := make([]int, 0, len(set))
	for frame := range set {
		frames = append(frames, frame)
	}
	sort.Ints(frames)
	return frames
}

func cycleForTargetFrame(result Result, frame int) int {
	if frame < 0 {
		return -1
	}
	for _, cycle := range result.Cycles {
		if frame >= cycle.TargetStartFrame && frame <= cycle.TargetEndFrame {
			return cycle.Cycle
		}
	}
	return -1
}

func (r Result) FootID(index int) string {
	if len(r.Frames) > 0 && index >= 0 && index < len(r.Frames[0].Feet) {
		return r.Frames[0].Feet[index].Foot
	}
	return ""
}
