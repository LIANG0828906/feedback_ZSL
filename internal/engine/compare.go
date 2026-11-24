package engine

import (
	"context"
	"errors"

	"retargetlab/internal/model"
	"retargetlab/internal/solver"
)

func (e *Engine) CompareWithFull(ctx context.Context) (ComparisonReport, error) {
	e.opMu.Lock()
	frames := make([]int, len(e.clip.Frames))
	for i := range frames {
		frames[i] = i
	}
	if _, err := e.processFrames(ctx, frames, false); err != nil {
		e.opMu.Unlock()
		return ComparisonReport{}, err
	}
	cfg := Config{
		Source: e.source, Clip: e.clip, Target: e.target, Mapping: e.mapping.Clone(),
		Params: cloneParams(e.params), WindowSize: e.windowSize, MaxWorkers: e.maxWorkers,
		QueuePerWorker: e.queuePerWorker, BufferPerWorker: e.bufferPerWorker,
	}
	left := e.CommittedResults()
	e.mu.RLock()
	currentSnapshot := e.solve.Snapshot()
	e.mu.RUnlock()
	e.opMu.Unlock()

	full, err := New(cfg)
	if err != nil {
		return ComparisonReport{}, err
	}
	if _, err := full.SolveAll(ctx); err != nil {
		return ComparisonReport{}, err
	}
	right := full.CommittedResults()
	report := ComparisonReport{Equal: true, FirstFrame: -1}
	for _, frame := range frames {
		l, okL := left[frame]
		r, okR := right[frame]
		if !okL || !okR {
			report.Equal = false
			report.FirstFrame = frame
			report.Message = "frame is committed on only one side"
			err := model.ErrFrame(model.E_DIFF, "compare_full", frame, "", report.Message)
			return report, err
		}
		if !snapshotMatchesCurrent(l.Snapshot, currentSnapshot) {
			report.FrozenSkipped = append(report.FrozenSkipped, frame)
			continue
		}
		report.FramesCompared++
		if err := solver.CompareFrameResults(l, r); err != nil {
			report.Equal = false
			report.FirstFrame = frame
			var ce *model.CodedError
			if errors.As(err, &ce) {
				report.Joint = ce.Joint
				report.Message = ce.Message
				report.Left = ce.Left
				report.Right = ce.Right
			} else {
				report.Message = err.Error()
			}
			return report, err
		}
	}
	return report, nil
}

func snapshotMatchesCurrent(frame, current solver.Snapshot) bool {
	if frame.SkeletonHash != current.SkeletonHash ||
		frame.MappingHash != current.MappingHash ||
		frame.ParamVersion != current.ParamVersion ||
		frame.AlgorithmVersion != current.AlgorithmVersion ||
		frame.BodyScale != current.BodyScale ||
		len(frame.ScaleCoefficients) != len(current.ScaleCoefficients) {
		return false
	}
	for k, v := range current.ScaleCoefficients {
		if frame.ScaleCoefficients[k] != v {
			return false
		}
	}
	return true
}
