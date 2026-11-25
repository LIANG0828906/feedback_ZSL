package kernel

import "testing"

func TestHandCalculatedVerticalBounce(t *testing.T) {
	scene := &Scene{
		SchemaVersion: SchemaV3,
		LevelID:       "hand-calculated",
		Width:         1280,
		Height:        720,
		MSPerTick:     50,
		StepTicks:     1,
		StepMS:        50,
		Gravity:       Vec2{X: 0, Y: 1000 * PosScale},
		Segments: []Segment{{
			ID: "floor", A: Vec2{X: 200 * PosScale, Y: 500 * PosScale},
			B:           Vec2{X: 100 * PosScale, Y: 500 * PosScale},
			NormalSide:  "left",
			Restitution: 8000,
		}},
		Launchers: []Launcher{{
			ID: "dropper", Position: Vec2{X: 150 * PosScale, Y: 100 * PosScale},
			Velocity: Vec2{},
		}},
		Requests: []PredictionRequest{{
			ID: "req", LogicalFrame: 0, LauncherID: "dropper", MaxFrames: 24,
		}},
	}
	report, err := Predict(scene)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Trajectories) != 1 || len(report.Trajectories[0].Bounces) == 0 {
		t.Fatalf("expected hand-calculated bounce: %+v", report)
	}
	bounce := report.Trajectories[0].Bounces[0]
	if bounce.Frame != 18 || bounce.SegmentID != "floor" {
		t.Fatalf("bounce frame/id = %d/%s, want 18/floor", bounce.Frame, bounce.SegmentID)
	}
	want := Vec2{X: 150 * PosScale, Y: 500 * PosScale}
	if !WithinTruthTolerance(bounce.Position, want, PosScale) {
		t.Fatalf("bounce position=%+v want~%+v", bounce.Position, want)
	}
	if bounce.VelocityAfter.Y >= 0 {
		t.Fatalf("reflected vy=%d want upward/negative", bounce.VelocityAfter.Y)
	}
	ratio := -bounce.VelocityAfter.Y * CoeffScale / bounce.VelocityBefore.Y
	if ratio < 7500 || ratio > 8500 {
		t.Fatalf("restitution ratio=%d outside fixed-point tolerance around 8000", ratio)
	}
}
