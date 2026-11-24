package segment

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/sample"
)

func TestDeterministicGradientAndMarkers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	m, err := sample.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := m.Images[0]
	img, _, err := sample.LoadImage(dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	rgb := imaging.ToRGB(img)
	p := model.DefaultParams()

	first, err := Segment(rgb, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Segment(rgb, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Gradient, second.Gradient) {
		t.Fatal("gradient magnitudes are not deterministic")
	}
	if !reflect.DeepEqual(first.Markers.Labels, second.Markers.Labels) {
		t.Fatal("marker labels are not deterministic")
	}
	if !reflect.DeepEqual(first.InitialLabels, second.InitialLabels) {
		t.Fatal("watershed labels are not deterministic")
	}
}

func TestMergeCapTighteningMonotonic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	m, err := sample.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := m.Images[1]
	img, _, err := sample.LoadImage(dir, rec)
	if err != nil {
		t.Fatal(err)
	}
	rgb := imaging.ToRGB(img)
	base := model.DefaultParams()

	caps := []float64{160, 80, 42, 20, 0}
	previous := -1
	for _, cap := range caps {
		p := base
		p.MergeCostCap = cap
		res, err := Segment(rgb, p)
		if err != nil {
			t.Fatal(err)
		}
		count := len(res.Regions)
		t.Logf("cap=%.0f regions=%d", cap, count)
		if previous >= 0 && count < previous {
			t.Fatalf("tightening cap reduced region count: cap=%.0f count=%d previous=%d", cap, count, previous)
		}
		previous = count
	}
}

func TestMergeIterationLimitReturnsEMerge(t *testing.T) {
	labels := []int32{1, 2, 3}
	gradient := []float64{0, 0, 0}
	mr := MarkerResult{Markers: []Marker{{ID: 1, Class: 1}, {ID: 2, Class: 1}, {ID: 3, Class: 1}}}
	p := model.DefaultParams()
	p.MinRegionArea = 1
	p.MergeCostCap = 1000
	p.AreaDiffCap = 1000

	_, err := MergeRegions(labels, gradient, mr, 3, 1, p, 1)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeMerge {
		t.Fatalf("expected E_MERGE, got %v", err)
	}
	if me.RemainingPairs < 1 || me.MinCost != 0 {
		t.Fatalf("unexpected merge diagnostics: %+v", me)
	}
}

func TestParameterRangeError(t *testing.T) {
	p := model.DefaultParams()
	p.FloodThreshold = 999
	err := model.ValidateParams(p)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodePara || me.Field != "flood_threshold" {
		t.Fatalf("expected E_PARA flood_threshold, got %v", err)
	}
}
