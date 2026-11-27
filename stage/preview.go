package stage

import (
	"context"
	"fmt"
	"time"
)

// RunPreview executes the declared low-density path. The report records actual
// density, accumulation precision, counts and elapsed time rather than claiming
// that the preview is full precision.
func RunPreview(ctx context.Context, p *Project, workers, budgetMS int64) (PreviewReport, error) {
	if budgetMS <= 0 {
		budgetMS = 250
	}
	cfg := PreviewSampling()
	start := time.Now()
	run, err := RunProject(ctx, p, RunOptions{Workers: int(workers), Sampling: cfg})
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return PreviewReport{}, err
	}
	stats := PreviewStatistics{WindowCount: len(run.Windows)}
	for _, w := range run.Windows {
		stats.FixtureSamples += len(w.Fixtures)
		for _, f := range w.Fixtures {
			stats.PointCount += len(f.Points)
		}
		stats.UncertainCount += w.UncertainCount
	}
	data, _ := canonicalJSON(run.Windows)
	stats.EstimatedBytes = int64(len(data))
	return PreviewReport{
		Result: run, Sampling: cfg, ElapsedMS: elapsed, BudgetMS: budgetMS,
		WithinBudget: elapsed <= budgetMS, Statistics: stats,
		Decision: "low-density radial rings and coarse overlap cells; compared with exact on shared fixture sample points",
	}, nil
}

// ComparePreviewWithExact compares every preview fixture point with the exact
// point at the same fixture, time, distance and coordinate. Exact-only points
// are intentionally permitted; preview-only or value divergence is not.
func ComparePreviewWithExact(preview, exact RunResult, tolerance Tolerance) (PreviewComparison, error) {
	cmp := PreviewComparison{OK: true, Tolerance: tolerance}
	exactByWindow := make(map[int]WindowResult, len(exact.Windows))
	for _, w := range exact.Windows {
		exactByWindow[w.WindowNumber] = w
	}
	for _, pw := range preview.Windows {
		ew, ok := exactByWindow[pw.WindowNumber]
		if !ok {
			m := &Mismatch{Path: "windows", WindowNumber: pw.WindowNumber, Field: "window_number", Actual: pw.WindowNumber, Expected: "missing"}
			return cmp, divergeError(m)
		}
		if pw.ParameterVersion != ew.ParameterVersion || pw.AlgorithmVersion != ew.AlgorithmVersion || !sameInt64s(pw.SampleTimes, ew.SampleTimes) {
			m := &Mismatch{
				Path: "windows", WindowNumber: pw.WindowNumber, Field: "version_or_timeline",
				Actual: []string{pw.ParameterVersion, pw.AlgorithmVersion}, Expected: []string{ew.ParameterVersion, ew.AlgorithmVersion},
			}
			return cmp, divergeError(m)
		}
		exactFixtures := make(map[string]FixtureSample, len(ew.Fixtures))
		for _, f := range ew.Fixtures {
			exactFixtures[fixtureSampleKey(f)] = f
		}
		for _, pf := range pw.Fixtures {
			ef, ok := exactFixtures[fixtureSampleKey(pf)]
			if !ok {
				m := &Mismatch{
					Path: "fixtures", WindowNumber: pw.WindowNumber, FixtureID: pf.FixtureID,
					Field: "fixture_sample", Actual: pf.Pose.TimeMS, Expected: "missing",
				}
				return cmp, divergeError(m)
			}
			exactPoints := make(map[string]LightPoint, len(ef.Points))
			for _, pt := range ef.Points {
				exactPoints[pointKey(pt)] = pt
			}
			for _, pp := range pf.Points {
				ep, ok := exactPoints[pointKey(pp)]
				if !ok {
					cmp.PreviewOnly++
					m := &Mismatch{
						Path: "sample_point", WindowNumber: pw.WindowNumber, FixtureID: pf.FixtureID,
						Field: "sample_point", Coordinate: []int64{pp.CoordinateMM[0], pp.CoordinateMM[1], pp.CoordinateMM[2]},
						Actual: pp.Intensity, Expected: "missing",
					}
					return cmp, divergeError(m)
				}
				cmp.Compared++
				if m := compareLightPoint(pw.WindowNumber, pf.FixtureID, pp, ep, tolerance); m != nil {
					cmp.OK = false
					cmp.First = m
					return cmp, divergeError(m)
				}
			}
		}
	}
	return cmp, nil
}

func compareLightPoint(window int, fixtureID string, actual, expected LightPoint, tol Tolerance) *Mismatch {
	coords := []int64{actual.CoordinateMM[0], actual.CoordinateMM[1], actual.CoordinateMM[2]}
	base := func(field string, a, e any) *Mismatch {
		return &Mismatch{
			Path: "fixtures[" + fixtureID + "].points", WindowNumber: window,
			FixtureID: fixtureID, Field: field, Coordinate: coords, Actual: a, Expected: e,
		}
	}
	if abs64(actual.Intensity-expected.Intensity) > tol.Intensity {
		return base("intensity", actual.Intensity, expected.Intensity)
	}
	if abs64(actual.LowerBound-expected.LowerBound) > tol.Intensity {
		return base("lower_bound", actual.LowerBound, expected.LowerBound)
	}
	if abs64(actual.UpperBound-expected.UpperBound) > tol.Intensity {
		return base("upper_bound", actual.UpperBound, expected.UpperBound)
	}
	for ch := 0; ch < 3; ch++ {
		if abs64(actual.ColorContribution[ch]-expected.ColorContribution[ch]) > tol.Chroma {
			return base(fmt.Sprintf("color_contribution.%d", ch), actual.ColorContribution[ch], expected.ColorContribution[ch])
		}
	}
	if actual.Uncertain != expected.Uncertain {
		return base("uncertain", actual.Uncertain, expected.Uncertain)
	}
	return nil
}

func divergeError(m *Mismatch) *Error {
	return &Error{
		Code: CodeDiverge, Message: "preview and exact paths diverged",
		Field: m.Field, FieldName: m.Field, WindowNumber: m.WindowNumber,
		FixtureID: m.FixtureID, Coordinate: m.Coordinate,
		Actual: m.Actual, Expected: m.Expected,
	}
}

func fixtureSampleKey(f FixtureSample) string {
	return fmt.Sprintf("%s@%d", f.FixtureID, f.Pose.TimeMS)
}

func pointKey(p LightPoint) string {
	return fmt.Sprintf("%d|%d,%d,%d", p.DistanceMM, p.CoordinateMM[0], p.CoordinateMM[1], p.CoordinateMM[2])
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
