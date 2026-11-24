package compare_test

import (
	"testing"

	"github.com/gameaudio/workbench/internal/compare"
	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/model"
)

func baseResults() []model.Result {
	return []model.Result{
		{SourceID: "S1", ListenerID: "L1", Occluded: true, DiffractionLen: 3.2,
			EffectiveLen: 3.2, SPL: 50, Curve: []float64{0, -1, -2, -3}},
		{SourceID: "S2", ListenerID: "L1", Occluded: false, DiffractionLen: 0,
			EffectiveLen: 2, SPL: 60, Curve: []float64{1, 0, -1, -2}},
	}
}

func TestResultsIdentical(t *testing.T) {
	if d := compare.Results(baseResults(), baseResults()); d != nil {
		t.Fatalf("identical results must match, got %s", d.String())
	}
}

func TestDiffLocatesFirstCurveSample(t *testing.T) {
	right := baseResults()
	right[1].Curve[2] = -9.9 // 第二个对的第 2 号采样
	d := compare.Results(baseResults(), right)
	if d == nil {
		t.Fatal("expected diff")
	}
	if d.PairKey != "S2->L1" || d.SampleIdx != 2 || d.Field != "curve" {
		t.Fatalf("locator wrong: pair=%s idx=%d field=%s",
			d.PairKey, d.SampleIdx, d.Field)
	}
	if d.Left != -1 || d.Right != -9.9 {
		t.Fatalf("diff must carry both measured values: %.2f/%.2f", d.Left, d.Right)
	}
	if errors.CodeOf(compare.AsError(d)) != errors.EDiff {
		t.Fatal("must map to E_DIFF")
	}
}

func TestDiffLocatesOcclusion(t *testing.T) {
	right := baseResults()
	right[0].Occluded = false
	d := compare.Results(baseResults(), right)
	if d == nil || d.Field != "occluded" || d.PairKey != "S1->L1" {
		t.Fatalf("occlusion locator wrong: %+v", d)
	}
}

func TestDiffLocatesSPL(t *testing.T) {
	right := baseResults()
	right[0].SPL = 51
	d := compare.Results(baseResults(), right)
	if d == nil || d.Field != "spl" {
		t.Fatalf("spl locator wrong: %+v", d)
	}
}

func TestProfilesDiff(t *testing.T) {
	a := []model.Profile{{ListenerID: "L1", MeanSPL: 10, MaxSPL: 12,
		CurveMean: []float64{1, 2}}}
	b := []model.Profile{{ListenerID: "L1", MeanSPL: 10, MaxSPL: 99,
		CurveMean: []float64{1, 2}}}
	d := compare.Profiles(a, b)
	if d == nil || d.Field != "profile.maxSpl" {
		t.Fatalf("profile locator wrong: %+v", d)
	}
}
