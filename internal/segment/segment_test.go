package segment

import (
	"bytes"
	"path/filepath"
	"testing"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
)

func TestDeterministicRegionGrowthAndCleanup(t *testing.T) {
	s, err := loadGeneratedSample(t)
	if err != nil {
		t.Fatal(err)
	}
	params := s.Manifest.DefaultsWithSeeds()
	areas, rects := truthMaps(s)
	opt := Options{
		Params: params, TruthLabels: s.TruthLabels, TruthAreas: areas, TruthRects: rects,
		MaxAreaRatio: model.DefaultSpillAreaRate, OutsideTruthRatio: 0.25,
	}
	first, err := Run(s.Image, opt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.RegionCount != s.Manifest.Truth.RegionCount {
		t.Fatalf("regions = %d, want %d", first.Stats.RegionCount, s.Manifest.Truth.RegionCount)
	}
	reversed := params
	reversed.Seeds = append([]model.Point(nil), params.Seeds...)
	for i, j := 0, len(reversed.Seeds)-1; i < j; i, j = i+1, j-1 {
		reversed.Seeds[i], reversed.Seeds[j] = reversed.Seeds[j], reversed.Seeds[i]
	}
	opt.Params = reversed
	second, err := Run(s.Image, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Labels, second.Labels) {
		t.Fatal("mask changed when seed order changed")
	}
	if !bytes.Equal(first.GrowthLabels, second.GrowthLabels) {
		t.Fatal("growth stage changed when seed order changed")
	}
	if first.StatsHash != second.StatsHash {
		t.Fatal("stats changed when seed order changed")
	}
}

func TestPreviewIterationBudgetTruncates(t *testing.T) {
	s, err := loadGeneratedSample(t)
	if err != nil {
		t.Fatal(err)
	}
	areas, rects := truthMaps(s)
	res, err := Run(s.Image, Options{
		Params: s.Manifest.DefaultsWithSeeds(), TruthLabels: s.TruthLabels,
		TruthAreas: areas, TruthRects: rects, IterationBudget: 1,
		MaxAreaRatio: model.DefaultSpillAreaRate, OutsideTruthRatio: 0.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("expected preview growth to be truncated")
	}
}

func TestSegmentErrorCodes(t *testing.T) {
	s, err := loadGeneratedSample(t)
	if err != nil {
		t.Fatal(err)
	}
	areas, rects := truthMaps(s)
	base := Options{
		Params: s.Manifest.DefaultsWithSeeds(), TruthLabels: s.TruthLabels,
		TruthAreas: areas, TruthRects: rects,
		MaxAreaRatio: model.DefaultSpillAreaRate, OutsideTruthRatio: 0.25,
	}

	badParam := base
	badParam.Params.StructureElementSize = 4
	if _, err := Run(s.Image, badParam); apperr.Code(err) != apperr.CodePara || apperr.Field(err) != "structure_element_size" {
		t.Fatalf("bad parameter error = %v", err)
	}

	badSeed := base
	badSeed.Params.Seeds = []model.Point{{X: 0, Y: 0}}
	if _, err := Run(s.Image, badSeed); apperr.Code(err) != apperr.CodeSeed {
		t.Fatalf("bad seed error = %v", err)
	}

	spill := base
	spill.Params.ColorTolerance = 441.6729559300637
	spill.Params.GradientThreshold = 255
	if _, err := Run(s.Image, spill); apperr.Code(err) != apperr.CodeSpill {
		t.Fatalf("spill error = %v", err)
	}
}

func loadGeneratedSample(t *testing.T) (*sample.Sample, error) {
	t.Helper()
	root := t.TempDir()
	generated, err := sample.Generate(sample.GenerateOptions{
		Seed:   42,
		OutDir: root,
		Resolutions: []sample.Resolution{
			{Width: 384, Height: 256, TileSize: 64, DeadlineMS: 250},
		},
	})
	if err != nil {
		return nil, err
	}
	return sample.Load(filepath.Join(root, generated[0].SampleID))
}

func truthMaps(s *sample.Sample) (map[int]int, map[int]model.Rect) {
	areas := make(map[int]int, len(s.Manifest.Regions))
	rects := make(map[int]model.Rect, len(s.Manifest.Regions))
	for _, r := range s.Manifest.Regions {
		areas[r.ID] = r.Area
		rects[r.ID] = r.BBox
	}
	return areas, rects
}
