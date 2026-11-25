package acoustic_test

import (
	"math"
	"testing"

	acoustic "levelacoustic"
)

func solveAllListeners(t *testing.T, project *acoustic.Project) []acoustic.ListenerSolution {
	t.Helper()
	var solutions []acoustic.ListenerSolution
	for _, listener := range project.Listeners {
		solution, err := acoustic.SolveListener(project, listener.ID)
		if err != nil {
			t.Fatalf("listener %d: %v", listener.ID, err)
		}
		solutions = append(solutions, solution)
	}
	return solutions
}

func TestReflectionTruthAndErrorBounds(t *testing.T) {
	project := sampleProject(t)
	solutions := solveAllListeners(t, project)
	comparison := acoustic.CompareTruth(project, solutions)
	if !comparison.Passed {
		for _, mismatch := range comparison.Mismatches {
			t.Logf("%+v", mismatch)
		}
		t.Fatal("solver does not match analytic truth")
	}
	for _, solution := range solutions {
		for _, path := range solution.Paths {
			report := path.ErrorPropagation
			if report.ChainSegments <= 0 {
				t.Fatalf("listener=%d source=%d missing chain segments", solution.ListenerID, path.SourceID)
			}
			if report.AbsorptionQuantizationDB <= 0 || report.MirrorRoundTripUpperBoundDB <= 0 || report.DistanceRoundUpperBoundDB <= 0 {
				t.Fatalf("non-positive error bound: %+v", report)
			}
			sum := report.ChainLengthUpperBoundDB + report.MirrorRoundTripUpperBoundDB + report.DistanceRoundUpperBoundDB + report.AbsorptionQuantizationDB
			if math.Abs(sum-report.CumulativeUpperBoundDB) > 1e-12 {
				t.Fatalf("cumulative bound does not equal the four terms: sum=%f cumulative=%f", sum, report.CumulativeUpperBoundDB)
			}
			if report.CumulativeUpperBoundDB <= 0 {
				t.Fatalf("cumulative bound must be positive")
			}
		}
	}
}

func TestTolerancePassCountIsMonotonic(t *testing.T) {
	project := sampleProject(t)
	solutions := solveAllListeners(t, project)
	tolerances := []float64{1.0, 0.5, 0.1, 0.01, 0.001}
	previous := len(solutions)
	for i, tolerance := range tolerances {
		count := acoustic.CountTolerancePasses(solutions, tolerance, 0.5)
		if i > 0 && count > previous {
			t.Fatalf("pass count increased when tolerance tightened: %f -> %d after %d", tolerance, count, previous)
		}
		previous = count
	}
	if previous < 0 {
		t.Fatal("unreachable")
	}
}

func TestReflectionPathCountsPerListener(t *testing.T) {
	project := sampleProject(t)
	solutions := solveAllListeners(t, project)
	expected := map[int]int{21: 1, 22: 3, 23: 4}
	for _, solution := range solutions {
		if solution.ValidPathCount != expected[solution.ListenerID] {
			t.Fatalf("listener %d valid paths=%d want %d", solution.ListenerID, solution.ValidPathCount, expected[solution.ListenerID])
		}
	}
}
