package dialoguecal

import "context"

func carryFromResult(result FrameResult) calibrationCarry {
	hasActive := result.Features.RMSQ15 >= ActiveRMSThreshold ||
		result.Calibration.State != StateUncalibrated ||
		result.Calibration.StableFrames > 0
	return calibrationCarry{
		state:            result.Calibration.State,
		smoothCorrection: result.Calibration.CorrectionDBQ8,
		stableFrames:     result.Calibration.StableFrames,
		lastLoudnessDB:   result.Calibration.LoudnessDBQ8,
		hasActive:        hasActive,
	}
}

// RunCheckpointed loads a matching checkpoint and completes remaining frames,
// committing each frame before advancing. If no checkpoint exists it starts
// from frame zero.
func RunCheckpointed(ctx context.Context, p *Project, store CheckpointStore, version string, afterCommit func(globalSeq int)) (*RecomputeReport, error) {
	frames, err := BuildFrames(p)
	if err != nil {
		return nil, err
	}
	cp, err := store.LoadCheckpoint(ctx, version)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		cp = &Checkpoint{LastFrameSeq: -1, SegmentProgress: map[string]int64{}}
	}

	results := make([]FrameResult, len(frames))
	reused := []int{}
	recomputed := []int{}
	committed := map[int]bool{}
	for _, saved := range cp.Frames {
		if saved.Frame.GlobalIndex >= 0 && saved.Frame.GlobalIndex < len(results) {
			saved.Provenance.CacheState = "restored"
			saved.Provenance.CommitSeq = int64(saved.Frame.GlobalIndex)
			results[saved.Frame.GlobalIndex] = saved
			committed[saved.Frame.GlobalIndex] = true
			reused = append(reused, saved.Frame.GlobalIndex)
		}
	}

	var prev *calibrationCarry
	next := int(cp.LastFrameSeq + 1)
	if cp.LastFrameSeq >= 0 && int(cp.LastFrameSeq) < len(results) {
		last := results[cp.LastFrameSeq]
		carry := carryFromResult(last)
		if int(cp.LastFrameSeq)+1 < len(frames) && frames[cp.LastFrameSeq+1].SegmentID == last.Frame.SegmentID {
			prev = &carry
		}
	}

	for seq := next; seq < len(frames); seq++ {
		if err := ctx.Err(); err != nil {
			complete := make([]FrameResult, 0, seq)
			complete = append(complete, results[:seq]...)
			return &RecomputeReport{Revision: 1, Results: complete, ReusedSeqs: reused, RecomputedSeqs: recomputed}, err
		}
		frame := frames[seq]
		if seq == 0 || (seq > 0 && frames[seq-1].SegmentID != frame.SegmentID) {
			prev = nil
		}
		provenance := Provenance{Revision: 1, CacheState: "checkpoint_computed", Sources: []string{"checkpoint_run"}}
		result, carry, err := computeFrameResult(p, frame, 1, provenance, prev)
		if err != nil {
			return nil, err
		}
		if err := store.CommitFrame(ctx, version, result, int64(frame.IndexInSegment)); err != nil {
			return nil, err
		}
		results[seq] = result
		recomputed = append(recomputed, seq)
		prev = &carry
		if afterCommit != nil {
			afterCommit(seq)
		}
	}

	return &RecomputeReport{Revision: 1, Results: results, ReusedSeqs: reused, RecomputedSeqs: recomputed}, nil
}

// RecoverAndComplete treats E_CHECKPOINT_VERSION as an instruction to discard
// the old checkpoint and recompute from the first frame.
func RecoverAndComplete(ctx context.Context, p *Project, store CheckpointStore, version string, afterCommit func(globalSeq int)) (*RecomputeReport, bool, error) {
	_, loadErr := store.LoadCheckpoint(ctx, version)
	recovered := false
	if loadErr == nil {
		recovered = true
	} else if be, ok := AsBusinessError(loadErr); ok && be.Code == CodeCheckpointVersion {
		recovered = false
		if resetErr := store.Reset(ctx); resetErr != nil {
			return nil, false, resetErr
		}
	} else if loadErr != nil {
		return nil, false, loadErr
	}
	report, err := RunCheckpointed(ctx, p, store, version, afterCommit)
	return report, recovered, err
}
