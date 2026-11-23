package stitch

import (
	"path/filepath"
	"testing"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/samplegen"
)

func TestMatchAndFusionErrors(t *testing.T) {
	root := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: root, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	set, err := imaging.LoadSet(filepath.Join(root, "harbor-06"))
	if err != nil {
		t.Fatal(err)
	}
	p := model.DefaultParams()
	p.MatchMin = 100000
	_, err = Register(RegisterOptions{Set: set, Params: p, Version: 1})
	e := errs.As(err)
	if e.Code != errs.EMatch || e.Actual == nil || e.Threshold == nil {
		t.Fatalf("expected E_MATCH with actual/threshold, got %#v", e)
	}

	bad := model.DefaultParams()
	bad.FusionLevels = 0
	_, err = Render(&Registration{}, bad, 1)
	e = errs.As(err)
	if e.Code != errs.EPara || e.Field != "fusion_levels" {
		t.Fatalf("expected E_PARA fusion_levels, got %#v", e)
	}
}
