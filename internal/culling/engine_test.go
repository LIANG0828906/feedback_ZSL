package culling_test

import (
	"sort"
	"testing"

	"occlbench/internal/culling"
	"occlbench/internal/generator"
)

func TestPreciseCullingMatchesIndependentGroundTruth(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	eng := culling.NewEngine()
	res, err := eng.Run(w, culling.DefaultPreciseParams())
	if err != nil {
		t.Fatalf("precise run: %v", err)
	}
	if len(res.Samples) != len(w.Trajectory.Samples) {
		t.Fatalf("sample count = %d want %d", len(res.Samples), len(w.Trajectory.Samples))
	}
	for _, sr := range res.Samples {
		want := append([]int{}, w.GroundTruth.Visible[sr.SampleIndex]...)
		sort.Ints(want)
		got := append([]int{}, sr.Visible...)
		sort.Ints(got)
		if !equalInts(got, want) {
			t.Errorf("sample %d visible mismatch:\n engine=%v\n truth =%v", sr.SampleIndex, got, want)
		}
	}
}

func TestPreviewSharesFrustumAndQuant(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	eng := culling.NewEngine()
	precise, err := eng.Run(w, culling.DefaultPreciseParams())
	if err != nil {
		t.Fatalf("precise: %v", err)
	}
	preview, err := eng.Run(w, culling.DefaultPreviewParams())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.QuantVersion != precise.QuantVersion {
		t.Errorf("quant version differs: %s vs %s", preview.QuantVersion, precise.QuantVersion)
	}
	if preview.GridResolution >= precise.GridResolution {
		t.Errorf("preview should use coarser grid: %d vs %d", preview.GridResolution, precise.GridResolution)
	}
	if preview.SampleRatio >= 1 {
		t.Errorf("preview sample ratio should be < 1, got %f", preview.SampleRatio)
	}
	// 预览是精确结果的子集：预览可见集合不得包含精确路径判定为遮挡的分块。
	for _, ps := range preview.Samples {
		full := precise.Samples[0]
		for _, s := range precise.Samples {
			if s.SampleIndex == ps.SampleIndex {
				full = s
			}
		}
		occSet := map[int]bool{}
		for _, id := range full.Occluded {
			occSet[id] = true
		}
		for _, id := range ps.Visible {
			if occSet[id] {
				t.Errorf("preview marks chunk %d visible but precise marks occluded", id)
			}
		}
	}
}

func TestParamValidation(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	eng := culling.NewEngine()
	bad := culling.Params{GridResolution: 0, SampleStride: 1, Quant: culling.DefaultQuantPrecision()}
	if _, err := eng.Run(w, bad); err == nil {
		t.Fatal("expected E_PARA for gridResolution=0")
	}
	bad2 := culling.Params{GridResolution: 8, SampleStride: 0, Quant: culling.DefaultQuantPrecision()}
	if _, err := eng.Run(w, bad2); err == nil {
		t.Fatal("expected E_PARA for sampleStride=0")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
