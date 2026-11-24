package pipeline

import (
	"fmt"
	"reflect"

	"integritykernel/internal/core"
)

func CompareReports(expected, actual *core.Report) *core.Error {
	if expected == nil || actual == nil {
		return core.NewError(core.CodeDiff, "one report is nil")
	}
	if len(expected.Frames) != len(actual.Frames) {
		frame := minInt(len(expected.Frames), len(actual.Frames))
		return diffError(frame, "report", "frames", nil, nil, "frame count mismatch")
	}
	for i := range expected.Frames {
		ef := expected.Frames[i]
		af := actual.Frames[i]
		if ef.Frame != af.Frame {
			return diffError(i, "report", "frame", nil, nil, "frame index mismatch")
		}
		if err := compareEvidencePair(i, core.EvidenceWatermark, ef.Watermark, af.Watermark); err != nil {
			return err
		}
		if err := compareEvidencePair(i, core.EvidenceResidual, ef.Residual, af.Residual); err != nil {
			return err
		}
		if err := compareNoise(i, ef.Noise, af.Noise); err != nil {
			return err
		}
		if err := compareEvidencePair(i, core.EvidenceVerdict, ef.Verdict, af.Verdict); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(expected.Timeline, actual.Timeline) {
		for i := range expected.Timeline.DecodedValues {
			if expected.Timeline.DecodedValues[i] != actual.Timeline.DecodedValues[i] {
				return diffError(i, "watermark", "decoded_frame", nil, nil, "watermark curve differs")
			}
		}
		return diffError(0, "watermark", "breaks", nil, nil, "watermark timeline breaks differ")
	}
	if !reflect.DeepEqual(expected.Intervals, actual.Intervals) {
		frame := 0
		if len(expected.Intervals) > 0 {
			frame = expected.Intervals[0].StartFrame
		}
		return diffError(frame, "interval", "intervals", nil, nil,
			fmt.Sprintf("intervals differ: expected=%s actual=%s", core.MustJSON(expected.Intervals), core.MustJSON(actual.Intervals)))
	}
	if err := compareCommitSequence(expected.CommitSequence, actual.CommitSequence); err != nil {
		return err
	}
	return nil
}

func compareEvidencePair(frame int, evidenceType string, expected, actual any) *core.Error {
	expectedNil := isNilValue(expected)
	actualNil := isNilValue(actual)
	if expectedNil || actualNil {
		if expectedNil != actualNil {
			return diffError(frame, evidenceType, "payload", nil, nil, "evidence presence differs")
		}
		return nil
	}
	if !reflect.DeepEqual(expected, actual) {
		return diffError(frame, evidenceType, "payload", nil, nil, "evidence payload differs")
	}
	return nil
}

func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

func compareNoise(frame int, expected, actual *core.NoiseEvidence) *core.Error {
	if expected == nil || actual == nil {
		if expected != actual {
			return diffError(frame, core.EvidenceNoise, "payload", nil, nil, "noise evidence presence differs")
		}
		return nil
	}
	if expected.GridCols != actual.GridCols || expected.GridRows != actual.GridRows || len(expected.Blocks) != len(actual.Blocks) {
		return diffError(frame, core.EvidenceNoise, "grid", nil, nil, "noise grid dimensions differ")
	}
	for i := range expected.Blocks {
		eb := expected.Blocks[i]
		ab := actual.Blocks[i]
		if reflect.DeepEqual(eb, ab) {
			continue
		}
		bx, by := eb.BlockX, eb.BlockY
		field := firstBlockDifference(eb, ab)
		return diffError(frame, core.EvidenceNoise, field, &bx, &by, "noise block statistic differs")
	}
	for _, field := range []string{"median_variance", "mad_variance", "anomalous_blocks", "anomaly_fraction", "max_temporal_z", "edge_anomaly", "anomaly_score", "primitive_edges"} {
		ev := reflect.ValueOf(*expected)
		av := reflect.ValueOf(*actual)
		ef := ev.FieldByName(fieldNameToGo(field))
		af := av.FieldByName(fieldNameToGo(field))
		if ef.IsValid() && af.IsValid() && !reflect.DeepEqual(ef.Interface(), af.Interface()) {
			return diffError(frame, core.EvidenceNoise, field, nil, nil, "noise aggregate differs")
		}
	}
	return nil
}

func firstBlockDifference(a, b core.BlockStat) string {
	pairs := []struct {
		name string
		av   any
		bv   any
	}{
		{"noise_variance", a.NoiseVariance, b.NoiseVariance},
		{"spatial_z", a.SpatialZ, b.SpatialZ},
		{"temporal_z", a.TemporalZ, b.TemporalZ},
		{"dispersion", a.Dispersion, b.Dispersion},
		{"orientation_bin", a.OrientationBin, b.OrientationBin},
		{"orientation_strength", a.OrientationStrength, b.OrientationStrength},
		{"edge_consistency", a.EdgeConsistency, b.EdgeConsistency},
		{"anomalous", a.Anomalous, b.Anomalous},
	}
	for _, pair := range pairs {
		if !reflect.DeepEqual(pair.av, pair.bv) {
			return pair.name
		}
	}
	return "block"
}

func fieldNameToGo(name string) string {
	switch name {
	case "median_variance":
		return "MedianVariance"
	case "mad_variance":
		return "MADVariance"
	case "anomalous_blocks":
		return "AnomalousBlocks"
	case "anomaly_fraction":
		return "AnomalyFraction"
	case "max_temporal_z":
		return "MaxTemporalZ"
	case "edge_anomaly":
		return "EdgeAnomaly"
	case "anomaly_score":
		return "AnomalyScore"
	case "primitive_edges":
		return "PrimitiveEdges"
	default:
		return name
	}
}

func compareCommitSequence(expected, actual []core.CommitRecord) *core.Error {
	if len(expected) != len(actual) {
		frame := 0
		evidenceType := "commit_sequence"
		if len(expected) > 0 {
			idx := minInt(len(expected), len(actual)) - 1
			if idx >= 0 {
				frame = expected[idx].Frame
				evidenceType = expected[idx].Type
			}
		}
		return diffError(frame, evidenceType, "commit_sequence", nil, nil, "commit sequence length differs")
	}
	for i := range expected {
		e, a := expected[i], actual[i]
		if e.Ordinal != a.Ordinal || e.Frame != a.Frame || e.Type != a.Type || e.CacheKey != a.CacheKey || e.Digest != a.Digest {
			return diffError(e.Frame, e.Type, "cache_key", nil, nil,
				fmt.Sprintf("commit sequence item %d differs: expected=%s actual=%s", i, core.MustJSON(e), core.MustJSON(a)))
		}
		if !reflect.DeepEqual(e.UpstreamKeys, a.UpstreamKeys) {
			return diffError(e.Frame, e.Type, "upstream_keys", nil, nil, "commit dependency keys differ")
		}
	}
	return nil
}

func CheckTruthInterval(report *core.Report, fingerprint core.TruthFingerprint, tolerance int) *core.Error {
	if report == nil || len(report.Intervals) == 0 {
		frame := fingerprint.ReplacementStart
		return diffError(frame, core.EvidenceVerdict, "intervals", nil, nil, "no replacement interval localized")
	}
	best := report.Intervals[0]
	for _, interval := range report.Intervals[1:] {
		if distanceToTruth(interval, fingerprint) < distanceToTruth(best, fingerprint) {
			best = interval
		}
	}
	if absInt(best.StartFrame-fingerprint.ReplacementStart) > tolerance {
		frame := fingerprint.ReplacementStart
		return diffError(frame, "interval", "start_frame", nil, nil,
			fmt.Sprintf("start frame %d not within tolerance %d of truth %d", best.StartFrame, tolerance, fingerprint.ReplacementStart))
	}
	if absInt(best.EndFrame-fingerprint.ReplacementEnd) > tolerance {
		frame := fingerprint.ReplacementEnd
		return diffError(frame, "interval", "end_frame", nil, nil,
			fmt.Sprintf("end frame %d not within tolerance %d of truth %d", best.EndFrame, tolerance, fingerprint.ReplacementEnd))
	}
	return nil
}

func distanceToTruth(interval core.Interval, fp core.TruthFingerprint) int {
	return absInt(interval.StartFrame-fp.ReplacementStart) + absInt(interval.EndFrame-fp.ReplacementEnd)
}

func diffError(frame int, evidenceType, field string, blockX, blockY *int, message string) *core.Error {
	return &core.Error{
		Code: core.CodeDiff, Message: message, Frame: &frame, Field: field,
		EvidenceType: evidenceType, BlockX: blockX, BlockY: blockY,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
