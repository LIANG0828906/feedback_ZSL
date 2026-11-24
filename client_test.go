package syncgame

import "testing"

func TestPredictionRollbackAndAuthorityReconcile(t *testing.T) {
	sample := testFixture(t)
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	client := sample.Clients[1]
	run, err := SimulateClient(sample, auth, client.ID, client.LatencyTier, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.RollbackCount == 0 {
		t.Fatal("fixture did not exercise rollback correction")
	}
	correctionSeen := false
	for _, event := range run.RollbackEvents {
		if event.Correction > 0 && len(event.Fields) > 0 {
			correctionSeen = true
		}
	}
	if !correctionSeen {
		t.Fatal("rollback events did not record observable correction fields")
	}
	for _, frame := range run.Frames {
		if frame.Mode != FrameModeAuthority {
			continue
		}
		authority := auth.States[frame.State.Tick]
		if mismatch := compareWorldStates(frame.State.Tick, frame.State, authority); mismatch != nil {
			t.Fatalf("authority frame %d is not field-identical: %+v", frame.Frame, mismatch)
		}
	}
}

func TestRollbackDepthBound(t *testing.T) {
	sample := cloneSample(t, testFixture(t))
	for i := range sample.LatencyTiers {
		if sample.LatencyTiers[i].ID == "medium" {
			sample.LatencyTiers[i].MaxRollbackDepth = 1
		}
	}
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = SimulateClient(sample, auth, "c2", "medium", ClientOptions{})
	gameError := requireCode(t, err, CodeRollback)
	if gameError.Tick < 0 || gameError.Field == "" || gameError.PlayerID != "p2" || gameError.Measured <= gameError.Limit {
		t.Fatalf("unexpected rollback error: %+v", gameError)
	}
}

func TestInputLatencyBound(t *testing.T) {
	sample := cloneSample(t, testFixture(t))
	sample.RenderFPS = 60
	for i := range sample.LatencyTiers {
		if sample.LatencyTiers[i].ID == "low" {
			sample.LatencyTiers[i].MaxInputLatencyFrames = 1
		}
	}
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = SimulateClient(sample, auth, "c1", "low", ClientOptions{})
	gameError := requireCode(t, err, CodeLatency)
	if gameError.PlayerID != "p1" || gameError.Frame < 0 || gameError.Measured <= gameError.Limit {
		t.Fatalf("unexpected latency error: %+v", gameError)
	}
}
