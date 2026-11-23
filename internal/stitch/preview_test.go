package stitch

import (
	"path/filepath"
	"testing"

	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

func TestPreviewDecisionBudgets(t *testing.T) {
	set, err := imaging.LoadSet(filepath.Join("..", "..", "data", "samples", "harbor-06"))
	if err != nil {
		t.Skip(err)
	}
	p := model.DefaultParams()
	exact, err := Register(RegisterOptions{Set: set, Params: p, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{900, 2000, 4000, 6000, 8000} {
		pc := p
		pc.PreviewFeatureBudget = budget
		preview, err := Register(RegisterOptions{Set: set, Params: pc, Version: 1, Coarse: true, Downsample: 2})
		if err != nil {
			t.Logf("budget=%d err=%v", budget, err)
			continue
		}
		diff := 0
		maxCov := 0.0
		for i := 1; i < len(exact.Pairs); i++ {
			if exact.Pairs[i].Decision != preview.Pairs[i].Decision {
				diff++
			}
			d := preview.Pairs[i].OverlapCoverage - exact.Pairs[i].OverlapCoverage
			if d < 0 {
				d = -d
			}
			if d > maxCov {
				maxCov = d
			}
		}
		t.Logf("budget=%d diff=%d maxCov=%.4f ms? canvas=%+v", budget, diff, maxCov, preview.Canvas)
	}
}
