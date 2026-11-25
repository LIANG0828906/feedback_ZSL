package textureconv

import (
	"encoding/json"
	"testing"
)

func planJSON(t *testing.T, p *Plan) []byte {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPlanMissingFieldIsSchema(t *testing.T) {
	p := SamplePlan()
	data := planJSON(t, p)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	entries := raw["entries"].([]any)
	first := entries[0].(map[string]any)
	delete(first, "targetDepth")
	bad, _ := json.Marshal(raw)
	_, err := ParsePlan(bad)
	e := mustCode(t, err, CodeSchema)
	if e.Entry != 1 || e.Field != "targetDepth" {
		t.Fatalf("unexpected error context: %+v", e)
	}
}

func TestPlanMissingFillIsSchema(t *testing.T) {
	p := SamplePlan()
	data := planJSON(t, p)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	first := raw["entries"].([]any)[0].(map[string]any)
	delete(first, "fill")
	bad, _ := json.Marshal(raw)
	_, err := ParsePlan(bad)
	e := mustCode(t, err, CodeSchema)
	if e.Entry != 1 || e.Field != "fill" {
		t.Fatalf("unexpected error context: %+v", e)
	}
}

func TestPlanIllegalDepthAndPermutation(t *testing.T) {
	p := SamplePlan()
	p.Entries[1].TargetDepth = 12
	_, err := ParsePlan(planJSON(t, p))
	e := mustCode(t, err, CodeParam)
	if e.Entry != 2 || e.Field != "targetDepth" {
		t.Fatalf("unexpected depth error: %+v", e)
	}

	p = SamplePlan()
	p.Entries[0].Permutation = []int{2, 2, 0, 3}
	_, err = ParsePlan(planJSON(t, p))
	e = mustCode(t, err, CodeParam)
	if e.Entry != 1 || e.Field != "permutation" {
		t.Fatalf("unexpected permutation error: %+v", e)
	}
}

func TestPlanFillDropCompatibility(t *testing.T) {
	im := testImage(t, 130, 100, Depth8, LayoutRGBA)
	p := SamplePlan()
	p.Entries[4].TargetDepth = Depth8
	_, err := ParsePlan(planJSON(t, p))
	e := mustCode(t, err, CodeParam)
	if e.Entry != 5 || e.Field != "fill.value" {
		t.Fatalf("unexpected fill error: %+v", e)
	}

	p = SamplePlan()
	p.Entries[3].Permutation = []int{0, 1, 2}
	p.Entries[3].Drop = []int{}
	_, err = ParsePlan(planJSON(t, p))
	if err != nil {
		t.Fatalf("shape-only parse should leave source-dependent drop to image validation: %v", err)
	}
	err = p.ValidateForImage(im)
	e = mustCode(t, err, CodeParam)
	if e.Entry != 4 || e.Field != "drop" {
		t.Fatalf("unexpected drop error: %+v", e)
	}
}

func TestPlanSequenceContinuity(t *testing.T) {
	p := SamplePlan()
	p.Entries[2].Seq = 9
	_, err := ParsePlan(planJSON(t, p))
	e := mustCode(t, err, CodeSchema)
	if e.Field != "seq" {
		t.Fatalf("unexpected sequence error: %+v", e)
	}
}

func TestPlanUnknownField(t *testing.T) {
	p := SamplePlan()
	data := planJSON(t, p)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	raw["surprise"] = true
	bad, _ := json.Marshal(raw)
	_, err := ParsePlan(bad)
	mustCode(t, err, CodeSchema)
}
