package stitch

import (
	"os"
	"path/filepath"
	"testing"

	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/samplegen"
)

func TestRegisterSample(t *testing.T) {
	dir := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: dir, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	set, err := imaging.LoadSet(filepath.Join(dir, "harbor-06"))
	if err != nil {
		t.Fatal(err)
	}
	params := model.DefaultParams()
	reg, err := Register(RegisterOptions{Set: set, Params: params, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("canvas=%+v truth=%+v", reg.Canvas, set.Manifest.Canvas)
	rendered, err := Render(reg, params, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("render=%dx%d seamGrad max=%.3f p95=%.3f avg=%.3f tol=%.3f", rendered.Width, rendered.Height, rendered.SeamMaxGrad, rendered.SeamP95Grad, rendered.SeamAvgGrad, set.Manifest.SeamTol)
	for _, pair := range reg.Pairs {
		t.Logf("%s matches=%d inliers=%d res=%.3f cond=%.2g err=%.5f coverage=%.3f path=%v",
			pair.PairID, pair.Matches, pair.Inliers, pair.ResidualMedian, pair.ConditionNumber, pair.HomographyErr, pair.OverlapCoverage, pair.Path)
	}
}

func TestAllGeneratedSamples(t *testing.T) {
	root := os.Getenv("STITCH_SAMPLES")
	if root == "" {
		root = filepath.Join("..", "..", "data", "samples")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skip("generated sample directory is absent")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			set, err := imaging.LoadSet(filepath.Join(root, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			reg, err := Register(RegisterOptions{Set: set, Params: model.DefaultParams(), Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := Render(reg, model.DefaultParams(), 1)
			if err != nil {
				t.Fatal(err)
			}
			maxH := 0.0
			accepted := 0
			var worst model.PairStats
			for _, p := range reg.Pairs {
				if !p.Decision {
					continue
				}
				accepted++
				if p.HomographyErr > maxH {
					maxH = p.HomographyErr
					worst = p
				}
			}
			t.Logf("images=%d accepted=%d canvas=%+v truth=%+v maxH=%.4f tol=%.4f seamP95=%.2f seamTol=%.2f",
				len(set.Images), accepted, reg.Canvas, set.Manifest.Canvas, maxH, set.Manifest.HomographyTol,
				rendered.SeamP95Grad, set.Manifest.SeamTol)
			if reg.Canvas != set.Manifest.Canvas {
				t.Fatalf("canvas mismatch")
			}
			if maxH > set.Manifest.HomographyTol {
				t.Logf("worst pair=%s path=%v matches=%d inliers=%d cov=%.3f H=%v", worst.PairID, worst.Path, worst.Matches, worst.Inliers, worst.OverlapCoverage, worst.Homography)
				t.Fatalf("homography error %.4f exceeds tolerance", maxH)
			}
			if rendered.SeamP95Grad > set.Manifest.SeamTol {
				t.Fatalf("seam gradient %.2f exceeds tolerance", rendered.SeamP95Grad)
			}
		})
	}
}
