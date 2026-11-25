package core_test

import (
	"testing"

	"maskpreview/internal/core"
)

func TestParameterIntervals(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*core.Params)
		field string
	}{
		{name: "color low", edit: func(p *core.Params) { p.ColorTolerance = -1 }, field: "color_tolerance"},
		{name: "color high", edit: func(p *core.Params) { p.ColorTolerance = 256 }, field: "color_tolerance"},
		{name: "gradient low", edit: func(p *core.Params) { p.GradientThreshold = -1 }, field: "gradient_threshold"},
		{name: "gradient high", edit: func(p *core.Params) { p.GradientThreshold = 256 }, field: "gradient_threshold"},
		{name: "kernel low", edit: func(p *core.Params) { p.KernelSize = 1 }, field: "kernel_size"},
		{name: "kernel high", edit: func(p *core.Params) { p.KernelSize = 17 }, field: "kernel_size"},
		{name: "kernel even", edit: func(p *core.Params) { p.KernelSize = 4 }, field: "kernel_size"},
		{name: "iterations low", edit: func(p *core.Params) { p.GrowthIterations = 0 }, field: "growth_iterations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := core.DefaultParams()
			tc.edit(&params)
			err := params.Validate()
			if core.CodeOf(err) != core.CodeParam {
				t.Fatalf("error = %v, want E_PARA", err)
			}
			appErr, ok := err.(*core.Error)
			if !ok || appErr.Field != tc.field || appErr.Data["min"] == nil || appErr.Data["max"] == nil {
				t.Fatalf("missing field/interval: %#v", err)
			}
		})
	}
	if err := core.DefaultParams().Validate(); err != nil {
		t.Fatalf("default params invalid: %v", err)
	}
}

func TestSnapshotVersionCanonicalizesSeedOrder(t *testing.T) {
	params := core.DefaultParams()
	q := core.DefaultQuantization()
	seedsA := []core.Point{{X: 3, Y: 9}, {X: 1, Y: 2}}
	seedsB := []core.Point{{X: 1, Y: 2}, {X: 3, Y: 9}}
	a, err := core.NewSnapshot("img", "hash", params, seedsA, q)
	if err != nil {
		t.Fatal(err)
	}
	b, err := core.NewSnapshot("img", "hash", params, seedsB, q)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != b.Version {
		t.Fatalf("version depends on seed order: %s != %s", a.Version, b.Version)
	}
}
