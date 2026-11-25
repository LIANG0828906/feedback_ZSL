package channelpipe

import (
	"encoding/json"
	"testing"
)

func TestVersionAndSchemaErrors(t *testing.T) {
	_, err := LoadPlanSet([]byte(`{"plan_ver":7}`))
	if !IsCode(err, CodeVersion) {
		t.Fatalf("expected E_VERSION, got %v", err)
	}
	if domainErr, _ := AsError(err); domainErr.Field != "plan_ver" {
		t.Fatalf("expected version field plan_ver, got %q", domainErr.Field)
	}

	_, err = LoadPlanSet([]byte(`{"version":3,"plans":[{"id":"x"}]}`))
	if !IsCode(err, CodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	if domainErr, _ := AsError(err); domainErr.Field != "plans[0].width" {
		t.Fatalf("unexpected schema field %q", domainErr.Field)
	}
}

func TestIntegerScalingWithoutFloat(t *testing.T) {
	if got := ScaleValue(1, 8, 16, RoundNearest); got != 257 {
		t.Fatalf("8->16 scale = %d, want 257", got)
	}
	if got := ScaleValue(255, 8, 16, RoundNearest); got != 65535 {
		t.Fatalf("8->16 max = %d, want 65535", got)
	}
	if got := ScaleValue(65535, 16, 8, RoundNearest); got != 255 {
		t.Fatalf("16->8 max = %d, want 255", got)
	}
	if got := ScaleValue(128, 8, 16, RoundFloor); got != 128*257 {
		t.Fatalf("floor widening = %d", got)
	}
}

func TestApplyStageFillDropAndRange(t *testing.T) {
	input := Buffer{
		Width: 1, Height: 1, Depth: 8,
		Channels: []Channel{ChannelR, ChannelG, ChannelB, ChannelA},
		Pixels:   []uint16{10, 20, 30, 40},
	}
	stage := Stage{
		TargetDepth:    16,
		Rounding:       RoundNearest,
		TargetChannels: []Channel{ChannelR, ChannelB, ChannelA},
		Swizzle:        []int{0, 2, -1},
		Fills:          []FillSpec{{}, {}, {Kind: FillOpaque}},
		AlphaMode:      AlphaOpaque,
	}
	result, err := ApplyStage(stage, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Buffer.Pixels[0] != 10*257 || result.Buffer.Pixels[1] != 30*257 {
		t.Fatalf("unexpected mapped pixels: %v", result.Buffer.Pixels)
	}
	if result.Buffer.Pixels[2] != 65535 {
		t.Fatalf("opaque fill = %d, want 65535", result.Buffer.Pixels[2])
	}
	if len(result.Dropped) != 2 || result.Dropped[0] != ChannelA || result.Dropped[1] != ChannelG {
		t.Fatalf("dropped = %v, want A,G", result.Dropped)
	}
}

func TestStateMachineIllegalTransitionPreservesState(t *testing.T) {
	state, err := TransitionState(StatePending, StateCommitted)
	if err == nil || !IsCode(err, CodeState) {
		t.Fatalf("expected E_STATE, got state=%s err=%v", state, err)
	}
	if state != StatePending {
		t.Fatalf("state changed to %s", state)
	}
	next, err := TransitionState(StatePending, StateProcessing)
	if err != nil || next != StateProcessing {
		t.Fatalf("legal transition failed: %s %v", next, err)
	}
}

func TestInvalidationOnlyDownstream(t *testing.T) {
	plan := Plan{
		ID: "p", Width: 10, Height: 10, SourceDepth: 8,
		SourceLayout: []Channel{ChannelR, ChannelG, ChannelB, ChannelA},
		Stages: []Stage{
			{ID: "s0", TargetDepth: 16, TargetChannels: []Channel{ChannelR, ChannelG, ChannelB, ChannelA}, Swizzle: []int{0, 1, 2, 3}, AlphaMode: AlphaPassthrough},
			{ID: "s1", TargetDepth: 8, TargetChannels: []Channel{ChannelB, ChannelG, ChannelR}, Swizzle: []int{2, 1, 0}, AlphaMode: AlphaDiscard},
			{ID: "s2", TargetDepth: 16, TargetChannels: []Channel{ChannelR, ChannelG, ChannelB, ChannelA}, Swizzle: []int{2, 1, 0, -1}, Fills: []FillSpec{{}, {}, {}, {Kind: FillOpaque}}, AlphaMode: AlphaOpaque},
		},
	}
	if err := ValidatePlan(&plan); err != nil {
		t.Fatal(err)
	}
	edited := cloneForTest(plan)
	edited.Stages[1].TargetDepth = 16
	chunks := BuildChunks(10, 10, 6, 6)
	report, err := InvalidatePlanEdit(plan, edited, chunks)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Items {
		if item.StageIndex == 0 {
			t.Fatalf("stage 0 must remain valid")
		}
		if item.StageIndex < 1 || item.StageIndex > 2 {
			t.Fatalf("unexpected invalid stage %d", item.StageIndex)
		}
		if item.Region.Width <= 0 || item.Region.Height <= 0 {
			t.Fatalf("missing actual region: %+v", item.Region)
		}
	}
}

func TestCompareBuffersLocatesEDiff(t *testing.T) {
	expected := &Buffer{Width: 1, Height: 2, Depth: 8, Channels: []Channel{ChannelR}, Pixels: []uint16{10, 20}}
	actual := &Buffer{Width: 1, Height: 2, Depth: 8, Channels: []Channel{ChannelR}, Pixels: []uint16{10, 21}}
	mismatch := CompareBuffers("img", 2, expected, actual)
	if mismatch == nil {
		t.Fatal("expected mismatch")
	}
	if mismatch.X != 0 || mismatch.Y != 1 || mismatch.StageIndex != 2 || mismatch.Field != "R" {
		t.Fatalf("unexpected location: %+v", mismatch)
	}
	if diffErr := DiffError(mismatch); !IsCode(diffErr, CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", diffErr)
	}
}

func cloneForTest(plan Plan) Plan {
	data, _ := CanonicalJSON(plan)
	var result Plan
	_ = json.Unmarshal(data, &result)
	return result
}
