package gait

import (
	"fmt"
	"math"
)

// BuildMapping resamples each source cycle into the target skeleton's declared
// cycle structure. The mapping is explicit and reversible for every source
// frame, which keeps phase decisions auditable after a resume.
func BuildMapping(source SourceLoop, phases PhaseTrack, target TargetSkeleton, params Params) (Mapping, error) {
	mapping := Mapping{SourceID: source.ID, TargetID: target.ID}
	if len(target.CycleFrames) != len(source.Cycles) {
		conflicts := make([]AlignConflict, 0)
		count := min(len(target.CycleFrames), len(source.Cycles))
		for i := 0; i < count; i++ {
			targetRatios := PhaseRatios{}
			if target.CycleFrames[i] > 0 {
				targetRatios = ratiosForResampled(phases, source.Cycles[i], target.CycleFrames[i])
			}
			conflicts = append(conflicts, AlignConflict{
				Cycle:             i,
				SourceProportions: ratiosForRange(phases, source.Cycles[i].StartFrame, source.Cycles[i].EndFrame),
				TargetProportions: targetRatios,
				Reason:            "cycle count mismatch",
			})
		}
		e := errCycle(CodeAlign, fmt.Sprintf("target declares %d cycles, source has %d", len(target.CycleFrames), len(source.Cycles)), count)
		e.Conflicts = conflicts
		return mapping, e
	}
	targetStart := 0
	for i, span := range source.Cycles {
		sourceLen := span.EndFrame - span.StartFrame + 1
		targetLen := target.CycleFrames[i]
		if targetLen <= 0 {
			e := errCycle(CodeDuration, fmt.Sprintf("target cycle %d duration is %d", i, targetLen), i)
			e.Frame = span.StartFrame
			return mapping, e
		}
		if targetLen > params.CycleBufferLimit {
			e := errCycle(CodeBuffer, fmt.Sprintf("target cycle %d duration %d exceeds buffer limit %d", i, targetLen, params.CycleBufferLimit), i)
			e.Frame = targetLen - 1
			return mapping, e
		}
		sourceRatios := ratiosForRange(phases, span.StartFrame, span.EndFrame)
		expected := int(math.Round(float64(sourceLen) * target.LegLengthRatio))
		if expected < 1 {
			expected = 1
		}
		if abs(float64(targetLen-expected)) > 1.0 {
			targetRatios := ratiosForResampled(phases, span, targetLen)
			e := errCycle(CodeAlign, fmt.Sprintf("target cycle %d duration %d is inconsistent with source duration %d and leg ratio %.6f (expected about %d)",
				i, targetLen, sourceLen, target.LegLengthRatio, expected), i)
			e.Frame = span.StartFrame
			e.Conflicts = []AlignConflict{{
				Cycle: i, SourceProportions: sourceRatios, TargetProportions: targetRatios,
				Reason: "duration does not follow leg length ratio",
			}}
			return mapping, e
		}

		frameMap := make([]int, targetLen)
		targetPhases := make([]FramePhase, targetLen)
		for local := 0; local < targetLen; local++ {
			sourceRel := 0
			if targetLen > 1 {
				sourceRel = int(math.Round(float64(local) * float64(sourceLen-1) / float64(targetLen-1)))
			}
			if sourceRel < 0 {
				sourceRel = 0
			}
			if sourceRel >= sourceLen {
				sourceRel = sourceLen - 1
			}
			sourceAbs := span.StartFrame + sourceRel
			frameMap[local] = sourceAbs
			targetPhases[local] = phases.Frames[sourceAbs]
		}
		targetRatios := ratiosForPhases(targetPhases)
		if conflictReason := compareRatios(sourceRatios, targetRatios, params.PhaseRatioTolerance); conflictReason != "" {
			e := errCycle(CodeAlign, fmt.Sprintf("cycle %d cannot preserve phase proportions: %s", i, conflictReason), i)
			e.Frame = span.StartFrame
			e.Conflicts = []AlignConflict{{
				Cycle: i, SourceProportions: sourceRatios, TargetProportions: targetRatios,
				Reason: conflictReason,
			}}
			return mapping, e
		}
		mapped := MappedCycle{
			Cycle:             i,
			SourceStartFrame:  span.StartFrame,
			SourceEndFrame:    span.EndFrame,
			TargetStartFrame:  targetStart,
			TargetEndFrame:    targetStart + targetLen - 1,
			Scale:             round6(float64(targetLen) / float64(sourceLen)),
			SourceProportions: sourceRatios,
			TargetProportions: targetRatios,
			SourceSegments:    segmentsForPhases(phases.Frames[span.StartFrame:span.EndFrame+1], span.StartFrame),
			TargetSegments:    segmentsForPhases(targetPhases, targetStart),
			SourceFrameMap:    frameMap,
		}
		mapping.Cycles = append(mapping.Cycles, mapped)
		targetStart += targetLen
	}
	return mapping, nil
}

func ratiosForRange(phases PhaseTrack, start, end int) PhaseRatios {
	if end < start {
		return PhaseRatios{}
	}
	return ratiosForPhases(phases.Frames[start : end+1])
}

func ratiosForResampled(phases PhaseTrack, span CycleSpan, targetLen int) PhaseRatios {
	sourceLen := span.EndFrame - span.StartFrame + 1
	frames := make([]FramePhase, targetLen)
	for local := 0; local < targetLen; local++ {
		sourceRel := 0
		if targetLen > 1 {
			sourceRel = int(math.Round(float64(local) * float64(sourceLen-1) / float64(targetLen-1)))
		}
		frames[local] = phases.Frames[span.StartFrame+sourceRel]
	}
	return ratiosForPhases(frames)
}

func ratiosForPhases(frames []FramePhase) PhaseRatios {
	if len(frames) == 0 {
		return PhaseRatios{}
	}
	var stance, swing, double float64
	for _, frame := range frames {
		switch frame.Global {
		case PhaseStance:
			stance++
		case PhaseSwing:
			swing++
		case PhaseDoubleSupport:
			double++
		}
	}
	n := float64(len(frames))
	return PhaseRatios{Stance: stance / n, Swing: swing / n, DoubleSupport: double / n}
}

func compareRatios(a, b PhaseRatios, tolerance float64) string {
	if abs(a.Stance-b.Stance) > tolerance {
		return fmt.Sprintf("stance ratio deviation %.6f exceeds %.6f", abs(a.Stance-b.Stance), tolerance)
	}
	if abs(a.Swing-b.Swing) > tolerance {
		return fmt.Sprintf("swing ratio deviation %.6f exceeds %.6f", abs(a.Swing-b.Swing), tolerance)
	}
	if abs(a.DoubleSupport-b.DoubleSupport) > tolerance {
		return fmt.Sprintf("double_support ratio deviation %.6f exceeds %.6f", abs(a.DoubleSupport-b.DoubleSupport), tolerance)
	}
	return ""
}

func segmentsForPhases(frames []FramePhase, offset int) []PhaseSegment {
	if len(frames) == 0 {
		return nil
	}
	segments := make([]PhaseSegment, 0, 4)
	start := 0
	current := frames[0].Global
	flush := func(end int) {
		segments = append(segments, PhaseSegment{StartFrame: start + offset, EndFrame: end + offset, Phase: current})
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].Global != current {
			flush(i - 1)
			start = i
			current = frames[i].Global
		}
	}
	flush(len(frames) - 1)
	return segments
}

// QueryCycle returns the explicit mapping for one cycle.
func (m Mapping) QueryCycle(cycle int) (MappedCycle, error) {
	if cycle < 0 || cycle >= len(m.Cycles) {
		e := errCycle(CodeAlign, fmt.Sprintf("mapped cycle %d is outside [0,%d)", cycle, len(m.Cycles)), cycle)
		return MappedCycle{}, e
	}
	return m.Cycles[cycle], nil
}

// QuerySourceFrame maps a source frame to a target frame using the recorded
// cycle frame map. If downsampling skipped a source frame, nearest sampling is
// used so every source frame remains traceable.
func (m Mapping) QuerySourceFrame(cycle, sourceFrame int) (int, error) {
	mc, err := m.QueryCycle(cycle)
	if err != nil {
		return -1, err
	}
	if sourceFrame < mc.SourceStartFrame || sourceFrame > mc.SourceEndFrame {
		e := errAt(CodeAlign, fmt.Sprintf("source frame %d is outside cycle %d (%d..%d)",
			sourceFrame, cycle, mc.SourceStartFrame, mc.SourceEndFrame), sourceFrame)
		return -1, e
	}
	best := 0
	bestDistance := math.MaxInt
	for local, mappedSource := range mc.SourceFrameMap {
		distance := mappedSource - sourceFrame
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			bestDistance = distance
			best = local
		}
	}
	return mc.TargetStartFrame + best, nil
}
