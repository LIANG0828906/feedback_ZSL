package combatjudge_test

import (
	"testing"

	"combatjudge/internal/engine"
	"combatjudge/internal/model"
	"combatjudge/internal/sample"
)

func TestSweepDurationAndTimeStepInvalidationPlans(t *testing.T) {
	old := sample.BuildProject()
	edited := sample.BuildProject()
	edited.Sweeps[0].EndFrame = 7
	edited.Requests = append(edited.Requests, model.FrameRequest{Frame: 7, SweepID: "sw_primary", AtMS: 96})

	durationPlan := engine.PlanEdit(old, edited, engine.DefaultIntervalFrames, engine.EditSpec{
		Kind: "sweep_duration", SweepID: "sw_primary", Field: "end_frame",
	})
	hasInterval4 := false
	for _, in := range durationPlan {
		if in.IntervalNo == 4 && in.Kind == "intersection" {
			hasInterval4 = true
		}
	}
	if !hasInterval4 {
		t.Fatalf("duration extension did not invalidate new interval 4: %+v", durationPlan)
	}

	stepPlan := engine.PlanEdit(old, old, engine.DefaultIntervalFrames, engine.EditSpec{Kind: "time_step", Field: "step_ms"})
	if len(stepPlan) != 4 {
		t.Fatalf("time step plan length = %d want all 4 intervals: %+v", len(stepPlan), stepPlan)
	}
	for i, in := range stepPlan {
		if in.IntervalNo != i+1 || in.Kind != "intersection" {
			t.Fatalf("time step plan entry %d = %+v", i, in)
		}
	}
}
