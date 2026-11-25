package acoustic_test

import (
	"testing"

	acoustic "levelacoustic"
	"levelacoustic/internal/samplegen"
)

func TestSampleTruthMatchesSolver(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var solutions []acoustic.ListenerSolution
	for _, listener := range project.Listeners {
		solution, err := acoustic.SolveListener(project, listener.ID)
		if err != nil {
			t.Fatalf("listener %d: %v", listener.ID, err)
		}
		solutions = append(solutions, solution)
	}
	comparison := acoustic.CompareTruth(project, solutions)
	if !comparison.Passed {
		for _, mismatch := range comparison.Mismatches {
			t.Logf("listener=%d source=%d reflector=%d field=%s expected=%v actual=%v tolerance=%v",
				mismatch.ListenerID, mismatch.SourceID, mismatch.ReflectorID, mismatch.Field,
				mismatch.Expected, mismatch.Actual, mismatch.Tolerance)
		}
		t.Fatalf("sample truth mismatch: expected %d paths, got %d", comparison.ExpectedPaths, comparison.ActualPaths)
	}
	t.Logf("sample seed=%d paths=%d rooms=%d sources=%d listeners=%d reflectors=%d",
		samplegen.DefaultSeed, comparison.ActualPaths, len(project.Rooms), len(project.Sources), len(project.Listeners), len(project.Reflectors))
}
