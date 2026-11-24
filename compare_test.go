package syncgame

import "testing"

func TestThreePathsConsistent(t *testing.T) {
	sample := testFixture(t)
	report, err := VerifyThreePaths(sample, VerificationOptions{
		RunOptions: RunOptions{
			Parallel:    true,
			Workers:     4,
			WindowTicks: 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("three-path verification failed: %+v", report)
	}
	if report.InterruptedTick == 0 || report.RecoveredFromTick == 0 {
		t.Fatalf("recovery did not use an interrupted checkpoint: %+v", report)
	}
}

func TestDivergenceReportsTickPlayerAndField(t *testing.T) {
	sample := testFixture(t)
	left, err := RunReplay(sample, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	right, err := RunReplay(sample, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	right.Authority.States[25].Units[0].X++
	mismatch := CompareRunResults(left, right)
	gameError := requireCode(t, mismatch, CodeDiverge)
	if gameError.Tick != 25 || gameError.PlayerID == "" || gameError.Field != "x" {
		t.Fatalf("unexpected divergence details: %+v", gameError)
	}
	if gameError.Left == nil || gameError.Right == nil {
		t.Fatalf("divergence must expose both measured values: %+v", gameError)
	}
}
