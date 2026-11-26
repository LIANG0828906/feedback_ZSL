package dialogue

import (
	"fmt"
	"path/filepath"
)

type PathResult struct {
	Name         string   `json:"name"`
	CommitOrder  []string `json:"commit_order"`
	SegmentCount int      `json:"segment_count"`
	FrameCount   int      `json:"frame_count"`
	Checkpoint   string   `json:"checkpoint,omitempty"`
	ReusedNodes  int      `json:"reused_nodes"`
	RebuiltNodes int      `json:"rebuilt_nodes"`
}

type VerificationReport struct {
	Equal       bool       `json:"equal"`
	Realtime    PathResult `json:"realtime"`
	Resume      PathResult `json:"resume"`
	Full        PathResult `json:"full"`
	FinalTarget int64      `json:"final_target_loudness"`
	Invalidated []string   `json:"last_invalidation,omitempty"`
}

// VerifyThreePaths executes realtime reuse, restart/resume, and a full
// recomputation for the same project and edit sequence, then compares every
// frame and calibration value.
func VerifyThreePaths(project *Project, edits []Edit, directory string) (*VerificationReport, error) {
	if project == nil {
		return nil, newError(CodeSchema, "", "project", "project is nil")
	}
	realtimeStore := NewCheckpointStore(filepath.Join(directory, "realtime.checkpoint.json"))
	realtime, err := NewPipeline(project, realtimeStore)
	if err != nil {
		return nil, err
	}
	realtimeResult, err := realtime.Build()
	if err != nil {
		return nil, err
	}
	for _, edit := range edits {
		if _, err := realtime.ApplyEdit(edit); err != nil {
			return nil, err
		}
		realtimeResult, err = realtime.Build()
		if err != nil {
			return nil, err
		}
	}
	finalProject := realtime.Project()

	resumeStore := NewCheckpointStore(filepath.Join(directory, "resume.checkpoint.json"))
	resume, err := NewPipeline(project, resumeStore)
	if err != nil {
		return nil, err
	}
	resumeResult, err := resume.Build()
	if err != nil {
		return nil, err
	}
	for _, edit := range edits {
		if _, err := resume.ApplyEdit(edit); err != nil {
			return nil, err
		}
		if _, err := resume.Build(); err != nil {
			return nil, err
		}
		resume, err = NewPipeline(resume.Project(), resumeStore)
		if err != nil {
			return nil, err
		}
		resumeResult, err = resume.Build()
		if err != nil {
			return nil, err
		}
	}

	full, err := NewPipeline(finalProject, nil)
	if err != nil {
		return nil, err
	}
	full.ClearCache()
	fullResult, err := full.Build()
	if err != nil {
		return nil, err
	}

	if err := CompareBuildResults(realtimeResult, resumeResult); err != nil {
		return nil, err
	}
	if err := CompareBuildResults(realtimeResult, fullResult); err != nil {
		return nil, err
	}
	report := &VerificationReport{
		Equal: true, FinalTarget: finalProject.TargetLoudness,
		Realtime: pathResult("realtime", realtimeResult, realtimeStore.Path()),
		Resume:   pathResult("resume", resumeResult, resumeStore.Path()),
		Full:     pathResult("full", fullResult, ""),
	}
	if realtimeResult.Plan != nil {
		report.Invalidated = append([]string(nil), realtimeResult.Plan.Invalidated...)
	}
	return report, nil
}

func pathResult(name string, result *BuildResult, checkpoint string) PathResult {
	out := PathResult{Name: name, CommitOrder: append([]string(nil), result.CommitOrder...), Checkpoint: checkpoint}
	out.SegmentCount = len(result.Segments)
	for _, segment := range result.Segments {
		if segment.Envelope != nil {
			out.FrameCount += len(segment.Envelope.Frames)
		}
	}
	if result.Plan != nil {
		out.ReusedNodes = len(result.Plan.Reused)
		out.RebuiltNodes = len(result.Plan.Invalidated)
	}
	return out
}

// CompareBuildResults returns E_DIFF at the first differing segment, frame, and
// field. Segment and frame order follows the canonical project order.
func CompareBuildResults(left, right *BuildResult) error {
	if left == nil || right == nil {
		return &Error{Code: CodeDiff, Field: "result", Message: "nil build result"}
	}
	if len(left.CommitOrder) != len(right.CommitOrder) {
		return &Error{Code: CodeDiff, Field: "commit_order", Message: "commit sequences have different lengths",
			Values: []string{fmt.Sprint(left.CommitOrder), fmt.Sprint(right.CommitOrder)}}
	}
	for i := range left.CommitOrder {
		if left.CommitOrder[i] != right.CommitOrder[i] {
			return &Error{
				Code: CodeDiff, SegmentID: left.CommitOrder[i], Field: "commit_order",
				Message: fmt.Sprintf("commit sequence differs at position %d", i),
				Values:  []string{left.CommitOrder[i], right.CommitOrder[i]},
			}
		}
	}
	if len(left.Segments) != len(right.Segments) {
		return &Error{Code: CodeDiff, Field: "segments", Message: "segment counts differ",
			Values: []string{fmt.Sprintf("%d", len(left.Segments)), fmt.Sprintf("%d", len(right.Segments))}}
	}
	for i := range left.Segments {
		leftSegment := left.Segments[i]
		rightSegment := right.Segments[i]
		if leftSegment.SegmentID != rightSegment.SegmentID {
			return &Error{Code: CodeDiff, SegmentID: leftSegment.SegmentID, Field: "segment_id",
				Message: "segment order differs", Values: []string{leftSegment.SegmentID, rightSegment.SegmentID}}
		}
		segmentID := leftSegment.SegmentID
		if leftSegment.SignalHash != rightSegment.SignalHash {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "signal_hash", Values: []string{leftSegment.SignalHash, rightSegment.SignalHash}}
		}
		if leftSegment.Envelope == nil || rightSegment.Envelope == nil {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "envelope", Message: "missing envelope"}
		}
		if len(leftSegment.Envelope.Frames) != len(rightSegment.Envelope.Frames) {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "frame_count",
				Values: []string{fmt.Sprintf("%d", len(leftSegment.Envelope.Frames)), fmt.Sprintf("%d", len(rightSegment.Envelope.Frames))}}
		}
		for frameIndex := range leftSegment.Envelope.Frames {
			leftFrame := leftSegment.Envelope.Frames[frameIndex]
			rightFrame := rightSegment.Envelope.Frames[frameIndex]
			if leftFrame.FrameIndex != rightFrame.FrameIndex {
				return frameDiff(segmentID, leftFrame.FrameIndex, "frame_index", leftFrame.FrameIndex, rightFrame.FrameIndex)
			}
			if leftFrame.StartSample != rightFrame.StartSample {
				return frameDiff(segmentID, leftFrame.FrameIndex, "start_sample", leftFrame.StartSample, rightFrame.StartSample)
			}
			if leftFrame.EndSample != rightFrame.EndSample {
				return frameDiff(segmentID, leftFrame.FrameIndex, "end_sample", leftFrame.EndSample, rightFrame.EndSample)
			}
			if leftFrame.Energy != rightFrame.Energy {
				return frameDiff(segmentID, leftFrame.FrameIndex, "energy", leftFrame.Energy, rightFrame.Energy)
			}
			if leftFrame.PeakPosition != rightFrame.PeakPosition {
				return frameDiff(segmentID, leftFrame.FrameIndex, "peak_position", leftFrame.PeakPosition, rightFrame.PeakPosition)
			}
			if leftFrame.Envelope != rightFrame.Envelope {
				return frameDiff(segmentID, leftFrame.FrameIndex, "envelope", leftFrame.Envelope, rightFrame.Envelope)
			}
		}
		if leftSegment.Calibration == nil || rightSegment.Calibration == nil {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "calibration", Message: "missing calibration"}
		}
		if leftSegment.Calibration.GainAdjustment != rightSegment.Calibration.GainAdjustment {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "gain_adjustment",
				Values: []string{fmt.Sprintf("%d", leftSegment.Calibration.GainAdjustment), fmt.Sprintf("%d", rightSegment.Calibration.GainAdjustment)}}
		}
		if leftSegment.Calibration.AppliedGain != rightSegment.Calibration.AppliedGain {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "applied_gain",
				Values: []string{fmt.Sprintf("%d", leftSegment.Calibration.AppliedGain), fmt.Sprintf("%d", rightSegment.Calibration.AppliedGain)}}
		}
		if len(leftSegment.Calibration.Frames) != len(rightSegment.Calibration.Frames) {
			return &Error{Code: CodeDiff, SegmentID: segmentID, Field: "calibrated_frame_count",
				Values: []string{fmt.Sprintf("%d", len(leftSegment.Calibration.Frames)), fmt.Sprintf("%d", len(rightSegment.Calibration.Frames))}}
		}
		for frameIndex := range leftSegment.Calibration.Frames {
			leftFrame := leftSegment.Calibration.Frames[frameIndex]
			rightFrame := rightSegment.Calibration.Frames[frameIndex]
			if leftFrame.FrameIndex != rightFrame.FrameIndex {
				return frameDiff(segmentID, leftFrame.FrameIndex, "calibrated_frame_index", leftFrame.FrameIndex, rightFrame.FrameIndex)
			}
			if leftFrame.Envelope != rightFrame.Envelope {
				return frameDiff(segmentID, leftFrame.FrameIndex, "calibrated_envelope", leftFrame.Envelope, rightFrame.Envelope)
			}
		}
	}
	return nil
}

func frameDiff(segmentID string, frameIndex int, field string, left, right any) error {
	index := frameIndex
	return &Error{
		Code: CodeDiff, SegmentID: segmentID, FrameIndex: &index, Field: field,
		Message: "first divergence",
		Values:  []string{fmt.Sprint(left), fmt.Sprint(right)},
	}
}
