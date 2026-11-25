package seg

import (
	"reflect"
	"testing"

	"iconseg/internal/core"
	"iconseg/internal/sample"
)

func TestWatershedAndMergeArePixelDeterministic(t *testing.T) {
	icon := sample.Generate(777)[2]
	params := core.DefaultParams()
	firstGradient := ComputeGradient(icon.Image, params)
	secondGradient := ComputeGradient(icon.Image, params)
	if !reflect.DeepEqual(firstGradient.V, secondGradient.V) {
		t.Fatal("gradient differs between runs")
	}
	first := Flood(firstGradient, icon.Manifest.Seeds, params.FloodThreshold)
	second := Flood(secondGradient, icon.Manifest.Seeds, params.FloodThreshold)
	if !reflect.DeepEqual(first.Labels, second.Labels) {
		t.Fatal("watershed labels differ between runs")
	}
	first.FillUnlabeled(firstGradient)
	second.FillUnlabeled(secondGradient)
	firstMerge := MergeRegions(firstGradient.W, firstGradient.H, first.Labels, first.RegionKeys, firstGradient, params)
	secondMerge := MergeRegions(secondGradient.W, secondGradient.H, second.Labels, second.RegionKeys, secondGradient, params)
	if !reflect.DeepEqual(firstMerge.Labels, secondMerge.Labels) {
		t.Fatal("merged region labels differ between runs")
	}
	if firstMerge.MergeCount != secondMerge.MergeCount {
		t.Fatal("merge count differs between runs")
	}
}
