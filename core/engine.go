package dialoguecal

import (
	"fmt"
	"slices"
	"time"
)

type cacheEntry struct {
	result FrameResult
	carry  calibrationCarry
}

type Engine struct {
	project         *Project
	frames          []Frame
	revision        int64
	cache           map[int]cacheEntry
	snapshotVersion string
}

func NewEngine(p *Project) (*Engine, error) {
	frames, err := BuildFrames(p)
	if err != nil {
		return nil, err
	}
	return &Engine{project: p, frames: frames, revision: 1, cache: map[int]cacheEntry{}, snapshotVersion: ContentHash(p)}, nil
}

func (e *Engine) Project() *Project { return e.project }
func (e *Engine) Frames() []Frame   { return e.frames }

// ComputeAll performs a deterministic sequential full recomputation.
func (e *Engine) ComputeAll() (RecomputeReport, error) {
	results := make([]FrameResult, 0, len(e.frames))
	recomputed := make([]int, 0, len(e.frames))
	timings := map[string]time.Duration{}
	var prev *calibrationCarry
	currentSegment := ""
	segmentStart := time.Now()

	for _, frame := range e.frames {
		if frame.SegmentID != currentSegment {
			if currentSegment != "" {
				timings[currentSegment] = time.Since(segmentStart)
			}
			currentSegment = frame.SegmentID
			segmentStart = time.Now()
			prev = nil
		}
		provenance := Provenance{Revision: e.revision, CacheState: "computed", Sources: []string{"full_recompute"}}
		result, carry, err := computeFrameResult(e.project, frame, e.revision, provenance, prev)
		if err != nil {
			return RecomputeReport{Revision: e.revision, Results: results, RecomputedSeqs: recomputed, SegmentTimings: timings}, err
		}
		results = append(results, result)
		recomputed = append(recomputed, frame.GlobalIndex)
		e.cache[frame.GlobalIndex] = cacheEntry{result: result, carry: carry}
		prev = &carry
	}
	if currentSegment != "" {
		timings[currentSegment] = time.Since(segmentStart)
	}
	return RecomputeReport{Revision: e.revision, Results: results, RecomputedSeqs: recomputed, SegmentTimings: timings}, nil
}

func resultsEqual(a, b []FrameResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !frameResultEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ResultsEquivalent compares all numerical and state outputs while ignoring
// provenance/cache annotations.
func ResultsEquivalent(a, b []FrameResult) bool { return resultsEqual(a, b) }

func frameResultEqual(a, b FrameResult) bool {
	if a.Frame.GlobalIndex != b.Frame.GlobalIndex || a.Frame.SegmentID != b.Frame.SegmentID ||
		a.Frame.IndexInSegment != b.Frame.IndexInSegment || a.Frame.StartSample != b.Frame.StartSample ||
		a.Frame.EndSample != b.Frame.EndSample || !slices.Equal(a.Frame.PhonemeIDs, b.Frame.PhonemeIDs) ||
		a.Features.RMSQ15 != b.Features.RMSQ15 ||
		a.Features.ZCRCount != b.Features.ZCRCount || a.Features.ZCRQ15 != b.Features.ZCRQ15 ||
		len(a.Features.F0Candidates) != len(b.Features.F0Candidates) {
		return false
	}
	for j := range a.Features.F0Candidates {
		if a.Features.F0Candidates[j] != b.Features.F0Candidates[j] {
			return false
		}
	}
	ac, bc := a.Calibration, b.Calibration
	return ac.GainedEnergyQ15 == bc.GainedEnergyQ15 &&
		ac.EnergyQ15 == bc.EnergyQ15 && ac.Limited == bc.Limited &&
		ac.LoudnessDBQ8 == bc.LoudnessDBQ8 && ac.EffectiveTargetDBQ8 == bc.EffectiveTargetDBQ8 &&
		ac.DesiredCorrectionDBQ8 == bc.DesiredCorrectionDBQ8 && ac.CorrectionDBQ8 == bc.CorrectionDBQ8 &&
		ac.CorrectionGainQ15 == bc.CorrectionGainQ15 && ac.CalibratedDBQ8 == bc.CalibratedDBQ8 &&
		ac.ResidualDBQ8 == bc.ResidualDBQ8 && ac.State == bc.State && ac.StableFrames == bc.StableFrames
}

func cloneResults(results []FrameResult) []FrameResult {
	out := make([]FrameResult, len(results))
	copy(out, results)
	return out
}

func rangeContainsFrame(r FrameRange, indexInSegment int) bool {
	return indexInSegment >= r.Start && indexInSegment < r.End
}

func frameRangeString(r FrameRange) string {
	return fmt.Sprintf("%s:[%d,%d)", r.SegmentID, r.Start, r.End)
}
