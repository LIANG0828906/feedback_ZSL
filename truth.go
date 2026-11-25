package colortrace

import "fmt"

// VerifyTruthFingerprint recomputes the manifest fingerprint.
func VerifyTruthFingerprint(truth *TruthManifest) bool {
	if truth == nil {
		return false
	}
	expected := truth.Fingerprint
	copyValue := *truth
	copyValue.Fingerprint = ""
	return HashCanonical("truth-manifest-v1", copyValue) == expected
}

// VerifyTruthOutputs compares assembled outputs with analytic truth. It returns
// the first mismatch in stable truth-point/stage order.
func VerifyTruthOutputs(project *Project, samples []*Sample) *Mismatch {
	if project.Truth == nil {
		return &Mismatch{Field: "truth", Expected: "manifest", Actual: nil}
	}
	for _, point := range project.Truth.Points {
		for _, expected := range point.Stages {
			if expected.Stage >= len(samples) {
				return &Mismatch{Stage: expected.Stage, Plane: point.Plane, Coord: &expected.Coord, Field: "stage_exists", Expected: true, Actual: false}
			}
			ps := samples[expected.Stage].Planes[point.Plane]
			if ps == nil {
				return &Mismatch{Stage: expected.Stage, Plane: point.Plane, Field: "plane", Expected: point.Plane, Actual: nil}
			}
			ledger := ps.LedgerAt(expected.Coord.X, expected.Coord.Y)
			if ledger == nil {
				return &Mismatch{Stage: expected.Stage, Plane: point.Plane, Coord: &expected.Coord, Field: "ledger", Expected: true, Actual: false}
			}
			if ledger.Code != expected.ExpectedCode {
				return mismatch(expected.Stage, point.Plane, expected.Coord, "code", expected.ExpectedCode, ledger.Code)
			}
			if ledger.OutputFixed != expected.ExpectedFixed {
				return mismatch(expected.Stage, point.Plane, expected.Coord, "output_fixed", expected.ExpectedFixed, ledger.OutputFixed)
			}
			if ledger.TotalDeviation != expected.ExpectedDeviation {
				return mismatch(expected.Stage, point.Plane, expected.Coord, "total_deviation", expected.ExpectedDeviation.String(), ledger.TotalDeviation.String())
			}
			if ledger.CumulativeDeviation != expected.ExpectedCumulative {
				return mismatch(expected.Stage, point.Plane, expected.Coord, "cumulative_deviation", expected.ExpectedCumulative.String(), ledger.CumulativeDeviation.String())
			}
			if ledger.Breakdown != expected.Breakdown {
				return mismatch(expected.Stage, point.Plane, expected.Coord, "breakdown", expected.Breakdown, ledger.Breakdown)
			}
		}
	}
	return nil
}

func mismatch(stage int, plane string, coord Coord, field string, expected, actual any) *Mismatch {
	return &Mismatch{
		Stage: stage, Plane: plane, Coord: &coord, Field: field,
		Expected: expected, Actual: actual,
	}
}

// DescribeMismatch renders a mismatch for logs.
func DescribeMismatch(m *Mismatch) string {
	if m == nil {
		return "<nil>"
	}
	coord := ""
	if m.Coord != nil {
		coord = fmt.Sprintf(" coord=(%d,%d)", m.Coord.X, m.Coord.Y)
	}
	return fmt.Sprintf("stage=%d plane=%s%s field=%s expected=%v actual=%v", m.Stage, m.Plane, coord, m.Field, m.Expected, m.Actual)
}
