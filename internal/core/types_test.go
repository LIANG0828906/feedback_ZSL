package core

import "testing"

func TestParameterRangeErrorIncludesFieldAndInterval(t *testing.T) {
	params := DefaultParams()
	params.FloodThreshold = -0.1
	err := params.Validate()
	if err == nil {
		t.Fatal("negative flood threshold was accepted")
	}
	app := AsAppError(err)
	if app.Code != CodePara || app.Field != "flood_threshold" || app.Range != "[0,2048]" {
		t.Fatalf("error=%+v", app)
	}
}

func TestSnapshotVersionChangesWithParameters(t *testing.T) {
	first, err := NewSnapshot(1, "icon", DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams()
	params.MergeCostLimit++
	second, err := NewSnapshot(1, "icon", params)
	if err != nil {
		t.Fatal(err)
	}
	if first.ParamVersion == second.ParamVersion {
		t.Fatal("parameter change did not change the snapshot version")
	}
}
