package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

type PathVerifyOptions struct {
	Sampling        SamplingConfig
	LiveWorkers     int
	FullWorkers     int
	ResumeStopAfter int
}

// VerifyThreePaths runs live, checkpoint-resumed and one-shot full paths and
// compares every point, uncertainty flag and commit sequence.
func VerifyThreePaths(ctx context.Context, p *Project, baseDir string, opts PathVerifyOptions) (VerifyReport, error) {
	if opts.Sampling.Density == 0 {
		opts.Sampling = ExactSampling()
	}
	if opts.LiveWorkers <= 0 {
		opts.LiveWorkers = 1
	}
	if opts.FullWorkers <= 0 {
		opts.FullWorkers = 4
	}
	if opts.ResumeStopAfter <= 0 {
		opts.ResumeStopAfter = 2
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return VerifyReport{}, err
	}
	liveDir := filepath.Join(baseDir, "live")
	resumeDir := filepath.Join(baseDir, "resume")
	fullDir := filepath.Join(baseDir, "full")
	for _, dir := range []string{liveDir, resumeDir, fullDir} {
		_ = os.RemoveAll(dir)
	}

	live, err := RunProject(ctx, p, RunOptions{
		Workers: opts.LiveWorkers, Sampling: opts.Sampling,
		Store: NewCheckpointStore(liveDir),
	})
	if err != nil {
		return VerifyReport{}, err
	}
	full, err := RunProject(ctx, p, RunOptions{
		Workers: opts.FullWorkers, Sampling: opts.Sampling,
		Store: NewCheckpointStore(fullDir),
	})
	if err != nil {
		return VerifyReport{}, err
	}
	resumeStore := NewCheckpointStore(resumeDir)
	partial, err := RunProject(ctx, p, RunOptions{
		Workers: opts.LiveWorkers, Sampling: opts.Sampling,
		Store: resumeStore, Limit: opts.ResumeStopAfter,
	})
	if err != nil {
		return VerifyReport{}, err
	}
	if len(partial.Windows) != opts.ResumeStopAfter {
		return VerifyReport{}, &Error{Code: CodeValue, Message: "resume partial run did not stop at requested window", Field: "paths.resume_stop_after"}
	}
	resumed, err := RunProject(ctx, p, RunOptions{
		Workers: opts.LiveWorkers, Sampling: opts.Sampling, Store: resumeStore,
	})
	if err != nil {
		return VerifyReport{}, err
	}
	paths := []struct {
		name string
		run  RunResult
	}{
		{"live", live}, {"resume", resumed}, {"full", full},
	}
	report := VerifyReport{OK: true}
	for _, path := range paths {
		hash, err := HashJSON(path.run.Windows)
		if err != nil {
			return VerifyReport{}, err
		}
		report.Paths = append(report.Paths, PathDigest{
			Name: path.name, Windows: len(path.run.Windows),
			CommitSequence: append([]int(nil), path.run.CommitSequence...), ResultHash: hash,
		})
	}
	base := resumed
	for _, path := range []struct {
		name string
		run  RunResult
	}{{"live", live}, {"full", full}} {
		ok, mismatch := CompareRunResults(base, path.run)
		if !ok {
			report.OK = false
			report.First = mismatch
			return report, &Error{
				Code: CodeDiverge, Message: fmt.Sprintf("exact paths diverge: resume vs %s", path.name),
				Field: mismatch.Field, FieldName: mismatch.Field, WindowNumber: mismatch.WindowNumber,
				FixtureID: mismatch.FixtureID, Coordinate: mismatch.Coordinate,
				Actual: mismatch.Actual, Expected: mismatch.Expected,
			}
		}
	}
	return report, nil
}

// CompareRunResults locates the first divergent window, fixture and field.
func CompareRunResults(expected, actual RunResult) (bool, *Mismatch) {
	if !reflect.DeepEqual(expected.CommitSequence, actual.CommitSequence) {
		return false, &Mismatch{
			Path: "commit_sequence", Field: "commit_sequence",
			Actual: actual.CommitSequence, Expected: expected.CommitSequence,
		}
	}
	expectedByWindow := make(map[int]WindowResult, len(expected.Windows))
	for _, w := range expected.Windows {
		expectedByWindow[w.WindowNumber] = w
	}
	if len(expected.Windows) != len(actual.Windows) {
		return false, &Mismatch{Path: "windows", Field: "window_count", Actual: len(actual.Windows), Expected: len(expected.Windows)}
	}
	for i, aw := range actual.Windows {
		ew, ok := expectedByWindow[aw.WindowNumber]
		if !ok {
			return false, &Mismatch{Path: "windows", WindowNumber: aw.WindowNumber, Field: "window_number", Actual: aw.WindowNumber, Expected: "missing"}
		}
		if m := compareWindows(ew, aw); m != nil {
			return false, m
		}
		_ = i
	}
	return true, nil
}

func compareWindows(expected, actual WindowResult) *Mismatch {
	base := func(field string, a, e any) *Mismatch {
		return &Mismatch{
			Path: fmt.Sprintf("windows[%d]", actual.WindowNumber), WindowNumber: actual.WindowNumber,
			Field: field, Actual: a, Expected: e,
		}
	}
	if actual.ParameterVersion != expected.ParameterVersion {
		return base("parameter_version", actual.ParameterVersion, expected.ParameterVersion)
	}
	if actual.AlgorithmVersion != expected.AlgorithmVersion {
		return base("algorithm_version", actual.AlgorithmVersion, expected.AlgorithmVersion)
	}
	if actual.ContentHash != expected.ContentHash {
		return base("content_hash", actual.ContentHash, expected.ContentHash)
	}
	if !reflect.DeepEqual(actual.Dependencies, expected.Dependencies) {
		return base("dependencies", actual.Dependencies, expected.Dependencies)
	}
	if !reflect.DeepEqual(actual.Sampling, expected.Sampling) {
		return base("sampling", actual.Sampling, expected.Sampling)
	}
	if !reflect.DeepEqual(actual.SampleTimes, expected.SampleTimes) {
		return base("sample_times_ms", actual.SampleTimes, expected.SampleTimes)
	}
	if actual.UncertainCount != expected.UncertainCount {
		return base("uncertain_count", actual.UncertainCount, expected.UncertainCount)
	}
	expectedFixtures := make(map[string]FixtureSample, len(expected.Fixtures))
	for _, f := range expected.Fixtures {
		expectedFixtures[fixtureSampleKey(f)] = f
	}
	if len(expected.Fixtures) != len(actual.Fixtures) {
		return base("fixture_sample_count", len(actual.Fixtures), len(expected.Fixtures))
	}
	for _, af := range actual.Fixtures {
		ef, ok := expectedFixtures[fixtureSampleKey(af)]
		if !ok {
			return &Mismatch{
				Path: "fixtures", WindowNumber: actual.WindowNumber, FixtureID: af.FixtureID,
				Field: "fixture_sample", Actual: af.Pose.TimeMS, Expected: "missing",
			}
		}
		if af.CacheKey != ef.CacheKey {
			return &Mismatch{
				Path: "fixtures[" + af.FixtureID + "]", WindowNumber: actual.WindowNumber,
				FixtureID: af.FixtureID, Field: "cache_key", Actual: af.CacheKey, Expected: ef.CacheKey,
			}
		}
		if !reflect.DeepEqual(af.Pose, ef.Pose) {
			return &Mismatch{
				Path: "fixtures[" + af.FixtureID + "].pose", WindowNumber: actual.WindowNumber,
				FixtureID: af.FixtureID, Field: "pose", Actual: af.Pose, Expected: ef.Pose,
			}
		}
		if af.TotalIntensity != ef.TotalIntensity {
			return &Mismatch{
				Path: "fixtures[" + af.FixtureID + "]", WindowNumber: actual.WindowNumber,
				FixtureID: af.FixtureID, Field: "total_intensity", Actual: af.TotalIntensity, Expected: ef.TotalIntensity,
			}
		}
		if af.TotalColor != ef.TotalColor {
			return &Mismatch{
				Path: "fixtures[" + af.FixtureID + "]", WindowNumber: actual.WindowNumber,
				FixtureID: af.FixtureID, Field: "total_color_contribution", Actual: af.TotalColor, Expected: ef.TotalColor,
			}
		}
		if len(af.Points) != len(ef.Points) {
			return &Mismatch{
				Path: "fixtures[" + af.FixtureID + "].points", WindowNumber: actual.WindowNumber,
				FixtureID: af.FixtureID, Field: "point_count", Actual: len(af.Points), Expected: len(ef.Points),
			}
		}
		for j := range af.Points {
			ap, ep := af.Points[j], ef.Points[j]
			if ap.Key != ep.Key || ap.CoordinateMM != ep.CoordinateMM || ap.DistanceMM != ep.DistanceMM {
				return &Mismatch{
					Path: fmt.Sprintf("fixtures[%s].points[%d]", af.FixtureID, j), WindowNumber: actual.WindowNumber,
					FixtureID: af.FixtureID, Field: "sample_point",
					Coordinate: []int64{ap.CoordinateMM[0], ap.CoordinateMM[1], ap.CoordinateMM[2]},
					Actual:     ap, Expected: ep,
				}
			}
			if m := compareLightPoint(actual.WindowNumber, af.FixtureID, ap, ep, Tolerance{}); m != nil {
				return m
			}
		}
	}
	expectedAggregates := make(map[Vec3]AggregatePoint, len(expected.Aggregates))
	for _, a := range expected.Aggregates {
		expectedAggregates[a.Cell] = a
	}
	if len(expected.Aggregates) != len(actual.Aggregates) {
		return base("aggregate_count", len(actual.Aggregates), len(expected.Aggregates))
	}
	for _, aa := range actual.Aggregates {
		ea, ok := expectedAggregates[aa.Cell]
		if !ok {
			return base("aggregate_cell", aa.Cell, "missing")
		}
		if aa.Intensity != ea.Intensity || aa.LowerBound != ea.LowerBound || aa.UpperBound != ea.UpperBound ||
			aa.ColorContribution != ea.ColorContribution || aa.Uncertain != ea.Uncertain ||
			!reflect.DeepEqual(aa.Contributors, ea.Contributors) {
			return base("aggregate", aa, ea)
		}
	}
	return nil
}

// CompareTruth verifies a full run against the persisted truth manifest.
func CompareTruth(run RunResult, truth TruthManifest) error {
	if len(run.Windows) == 0 {
		return &Error{Code: CodeDiverge, Message: "truth comparison received no windows", Field: "windows"}
	}
	expected := RunResult{Windows: truth.Windows}
	for _, w := range truth.Windows {
		expected.CommitSequence = append(expected.CommitSequence, w.WindowNumber)
	}
	if ok, mismatch := CompareRunResults(expected, run); !ok {
		return divergeError(mismatch)
	}
	if run.Windows[0].ParameterVersion != truth.ParameterVersion || run.Windows[0].AlgorithmVersion != truth.AlgorithmVersion {
		return &Error{Code: CodeVersion, Message: "truth version mismatch", Field: "truth.version"}
	}
	return nil
}
