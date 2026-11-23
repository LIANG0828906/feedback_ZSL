package engine

import (
	"fmt"
	"math"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
)

type DiffSpan struct {
	Index            int     `json:"index"`
	ID               string  `json:"id"`
	StartFrame       int64   `json:"start_frame"`
	FrameCount       int64   `json:"frame_count"`
	DifferingSamples int64   `json:"differing_samples"`
	MaxAbsoluteDiff  float64 `json:"max_absolute_diff"`
}

type Divergence struct {
	SampleIndex  int64   `json:"sample_index"`
	FrameIndex   int64   `json:"frame_index"`
	Channel      int     `json:"channel"`
	SegmentIndex int     `json:"segment_index"`
	SegmentID    string  `json:"segment_id"`
	Left         float64 `json:"left"`
	Right        float64 `json:"right"`
}

type DifferenceReport struct {
	Identical          bool        `json:"identical"`
	GainDecisionsEqual bool        `json:"gain_decisions_equal"`
	FirstDivergence    *Divergence `json:"first_divergence,omitempty"`
	Segments           []DiffSpan  `json:"segments"`
	GainMismatchPath   string      `json:"gain_mismatch_path,omitempty"`
	GainLeft           any         `json:"gain_left,omitempty"`
	GainRight          any         `json:"gain_right,omitempty"`
}

func CompareSignals(left, right audio.Signal, spans []MixSpan) (*DifferenceReport, error) {
	report := &DifferenceReport{Identical: true, GainDecisionsEqual: true, Segments: make([]DiffSpan, len(spans))}
	for i, span := range spans {
		report.Segments[i] = DiffSpan{Index: span.Index, ID: span.ID, StartFrame: span.StartFrame, FrameCount: span.FrameCount}
	}
	if left.SampleRateHz != right.SampleRateHz || left.Channels != right.Channels {
		report.Identical = false
		err := apperr.New(apperr.CodeDivergence, "streams have different sample rate or channel count").
			WithSides(fmt.Sprintf("%d Hz/%d ch", left.SampleRateHz, left.Channels), fmt.Sprintf("%d Hz/%d ch", right.SampleRateHz, right.Channels))
		return report, err
	}
	totalSamples := len(left.Data)
	if len(right.Data) > totalSamples {
		totalSamples = len(right.Data)
	}
	var first *Divergence
	for i := 0; i < totalSamples; i++ {
		var l, r float64
		lMissing := i >= len(left.Data)
		rMissing := i >= len(right.Data)
		if !lMissing {
			l = left.Data[i]
		}
		if !rMissing {
			r = right.Data[i]
		}
		if !lMissing && !rMissing && l == r {
			continue
		}
		report.Identical = false
		frame := int64(0)
		channel := 0
		if left.Channels > 0 {
			frame = int64(i / left.Channels)
			channel = i % left.Channels
		}
		segmentIndex, segmentID := segmentForFrame(spans, frame)
		if first == nil {
			first = &Divergence{
				SampleIndex: int64(i), FrameIndex: frame, Channel: channel,
				SegmentIndex: segmentIndex, SegmentID: segmentID, Left: l, Right: r,
			}
		}
		if segmentIndex >= 0 && segmentIndex < len(report.Segments) {
			diff := math.Abs(l - r)
			report.Segments[segmentIndex].DifferingSamples++
			if diff > report.Segments[segmentIndex].MaxAbsoluteDiff {
				report.Segments[segmentIndex].MaxAbsoluteDiff = diff
			}
		}
	}
	if first != nil {
		report.FirstDivergence = first
		return report, divergenceError(first)
	}
	return report, nil
}

func CompareResults(left, right *Result) (*DifferenceReport, error) {
	if left == nil || right == nil {
		return nil, apperr.New(apperr.CodeDivergence, "both results are required")
	}
	report, err := CompareSignals(left.Mix, right.Mix, left.Offsets)
	if err != nil {
		return report, err
	}
	if len(left.Segments) != len(right.Segments) {
		report.Identical = false
		return report, apperr.New(apperr.CodeDivergence, "segment counts differ").
			WithSides(len(left.Segments), len(right.Segments))
	}
	for i := range left.Segments {
		l, r := left.Segments[i], right.Segments[i]
		if l.NormalizationGainDB != r.NormalizationGainDB {
			report.GainDecisionsEqual = false
			report.Identical = false
			report.GainMismatchPath = fmt.Sprintf("segments[%d].normalization_gain_db", i)
			report.GainLeft = l.NormalizationGainDB
			report.GainRight = r.NormalizationGainDB
			return report, apperr.New(apperr.CodeDivergence, "normalization gain decision differs").
				WithPath(report.GainMismatchPath).WithSides(l.NormalizationGainDB, r.NormalizationGainDB).
				WithSegment(i, l.ID)
		}
		if l.CompressionMaxReductionDB != r.CompressionMaxReductionDB {
			report.GainDecisionsEqual = false
			report.Identical = false
			report.GainMismatchPath = fmt.Sprintf("segments[%d].compression_max_reduction_db", i)
			report.GainLeft = l.CompressionMaxReductionDB
			report.GainRight = r.CompressionMaxReductionDB
			return report, apperr.New(apperr.CodeDivergence, "compression gain decision differs").
				WithPath(report.GainMismatchPath).WithSides(l.CompressionMaxReductionDB, r.CompressionMaxReductionDB).
				WithSegment(i, l.ID)
		}
		if l.LimiterSafetyTrimDB != r.LimiterSafetyTrimDB {
			report.GainDecisionsEqual = false
			report.Identical = false
			report.GainMismatchPath = fmt.Sprintf("segments[%d].limiter_safety_trim_db", i)
			report.GainLeft = l.LimiterSafetyTrimDB
			report.GainRight = r.LimiterSafetyTrimDB
			return report, apperr.New(apperr.CodeDivergence, "limiter gain decision differs").
				WithPath(report.GainMismatchPath).WithSides(l.LimiterSafetyTrimDB, r.LimiterSafetyTrimDB).
				WithSegment(i, l.ID)
		}
		if l.OutputHash != r.OutputHash {
			report.Identical = false
			report.GainMismatchPath = fmt.Sprintf("segments[%d].output_hash", i)
			report.GainLeft = l.OutputHash
			report.GainRight = r.OutputHash
			return report, apperr.New(apperr.CodeDivergence, "segment output hash differs").
				WithPath(report.GainMismatchPath).WithSides(l.OutputHash, r.OutputHash).
				WithSegment(i, l.ID)
		}
	}
	return report, nil
}

func segmentForFrame(spans []MixSpan, frame int64) (int, string) {
	for _, span := range spans {
		if frame >= span.StartFrame && frame < span.StartFrame+span.FrameCount {
			return span.Index, span.ID
		}
	}
	if len(spans) > 0 {
		last := spans[len(spans)-1]
		return last.Index, last.ID
	}
	return -1, ""
}

func divergenceError(first *Divergence) *apperr.Error {
	return apperr.New(apperr.CodeDivergence, "streams diverge at the first sample").
		WithField("samples").WithPath(fmt.Sprintf("samples[%d]", first.SampleIndex)).
		WithSegment(first.SegmentIndex, first.SegmentID).
		WithSample(first.SampleIndex, first.FrameIndex, first.Channel).
		WithSides(first.Left, first.Right).
		WithExpected(first.Left).WithActual(first.Right)
}
