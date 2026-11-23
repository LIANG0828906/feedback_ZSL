package engine

import (
	"math"

	"loudnessworkbench/apperr"
	"loudnessworkbench/project"
)

type TruthSegmentResult struct {
	Index          int     `json:"index"`
	ID             string  `json:"id"`
	LoudnessError  float64 `json:"loudness_error_lu"`
	TruePeakError  float64 `json:"true_peak_error_db"`
	GateStatsEqual bool    `json:"gate_stats_equal"`
}

type TruthReport struct {
	OK                 bool                 `json:"ok"`
	MaxLoudnessErrorLU float64              `json:"max_loudness_error_lu"`
	MaxTruePeakErrorDB float64              `json:"max_true_peak_error_db"`
	GainErrorDB        float64              `json:"gain_error_db"`
	Segments           []TruthSegmentResult `json:"segments"`
}

// VerifyTruth checks source measurements and global gain against the manifest.
func VerifyTruth(result *Result, manifest *project.Manifest) (*TruthReport, error) {
	if result == nil || manifest == nil {
		return nil, apperr.New(apperr.CodeSchema, "result and manifest are required")
	}
	if len(result.Segments) != len(manifest.Segments) {
		return nil, apperr.New(apperr.CodeSchema, "segment count differs from manifest").WithField("segments")
	}
	report := &TruthReport{OK: true, Segments: make([]TruthSegmentResult, len(result.Segments))}
	for i, actual := range result.Segments {
		expected := manifest.Segments[i]
		segmentReport := TruthSegmentResult{Index: i, ID: actual.ID}
		segmentReport.LoudnessError = math.Abs(actual.SourceMeasurement.IntegratedLUFS - expected.Truth.IntegratedLUFS)
		segmentReport.TruePeakError = math.Abs(actual.SourceMeasurement.TruePeakDBTP - expected.Truth.TruePeakDBTP)
		segmentReport.GateStatsEqual =
			actual.SourceMeasurement.FramesTotal == expected.Truth.Gate.FrameCount &&
				actual.SourceMeasurement.FramesAboveAbsolute == expected.Truth.Gate.AboveAbsolute &&
				actual.SourceMeasurement.FramesAboveBothGates == expected.Truth.Gate.AboveBothGates
		report.Segments[i] = segmentReport
		report.MaxLoudnessErrorLU = math.Max(report.MaxLoudnessErrorLU, segmentReport.LoudnessError)
		report.MaxTruePeakErrorDB = math.Max(report.MaxTruePeakErrorDB, segmentReport.TruePeakError)
		if !segmentReport.GateStatsEqual {
			report.OK = false
			err := apperr.New(apperr.CodeSchema, "gate frame statistics differ from truth").
				WithField("segments[].truth.gate").WithPath("segments["+itoa(i)+"].truth.gate").
				WithExpected(expected.Truth.Gate).WithActual(actual.SourceMeasurement).
				WithSegment(i, actual.ID)
			return report, err
		}
		if segmentReport.LoudnessError > manifest.Tolerance.LoudnessLU {
			report.OK = false
			err := apperr.New(apperr.CodeSchema, "integrated loudness exceeds truth tolerance").
				WithField("segments[].truth.integrated_lufs").
				WithPath("segments["+itoa(i)+"].truth.integrated_lufs").
				WithExpected(expected.Truth.IntegratedLUFS).WithActual(actual.SourceMeasurement.IntegratedLUFS).
				WithSegment(i, actual.ID)
			return report, err
		}
		if segmentReport.TruePeakError > manifest.Tolerance.TruePeakDB {
			report.OK = false
			err := apperr.New(apperr.CodeSchema, "true peak exceeds truth tolerance").
				WithField("segments[].truth.true_peak_dbtp").
				WithPath("segments["+itoa(i)+"].truth.true_peak_dbtp").
				WithExpected(expected.Truth.TruePeakDBTP).WithActual(actual.SourceMeasurement.TruePeakDBTP).
				WithSegment(i, actual.ID)
			return report, err
		}
	}
	report.GainErrorDB = math.Abs(result.GlobalGainDB - manifest.ProgramTruth.TargetGainDB)
	if report.GainErrorDB > manifest.Tolerance.GainDB {
		report.OK = false
		return report, apperr.New(apperr.CodeSchema, "global gain exceeds truth tolerance").
			WithField("program_truth.target_gain_db").
			WithExpected(manifest.ProgramTruth.TargetGainDB).WithActual(result.GlobalGainDB)
	}
	return report, nil
}

type TolerancePoint struct {
	LoudnessLU         float64 `json:"loudness_lu"`
	TruePeakDB         float64 `json:"true_peak_db"`
	SegmentsWithin     int     `json:"segments_within"`
	Accepted           bool    `json:"accepted"`
	MaxLoudnessErrorLU float64 `json:"max_loudness_error_lu"`
	MaxTruePeakErrorDB float64 `json:"max_true_peak_error_db"`
}

// ToleranceSweep makes the monotonic property explicit: tightening tolerance
// can never increase the number of accepted segments.
func ToleranceSweep(result *Result, manifest *project.Manifest, tolerances []float64) []TolerancePoint {
	points := make([]TolerancePoint, len(tolerances))
	for i, tolerance := range tolerances {
		point := TolerancePoint{
			LoudnessLU: tolerance, TruePeakDB: tolerance,
			Accepted: true,
		}
		for j, segment := range result.Segments {
			expected := manifest.Segments[j]
			loudnessError := math.Abs(segment.SourceMeasurement.IntegratedLUFS - expected.Truth.IntegratedLUFS)
			peakError := math.Abs(segment.SourceMeasurement.TruePeakDBTP - expected.Truth.TruePeakDBTP)
			point.MaxLoudnessErrorLU = math.Max(point.MaxLoudnessErrorLU, loudnessError)
			point.MaxTruePeakErrorDB = math.Max(point.MaxTruePeakErrorDB, peakError)
			if loudnessError <= tolerance && peakError <= tolerance {
				point.SegmentsWithin++
			} else {
				point.Accepted = false
			}
		}
		points[i] = point
	}
	return points
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := [20]byte{}
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
