package scenario

import (
	"os"
	"path/filepath"

	"qkernel/internal/attribution"
	"qkernel/internal/chain"
	"qkernel/internal/imagex"
	"qkernel/internal/synth"
)

const (
	Width           = 576
	Height          = 192
	SegmentSize     = 64
	MetricBlockSize = 32
)

func Default() (synth.Config, chain.Chain) {
	regions := []imagex.Rect{
		imagex.R(0, 64, 64, 128),
		imagex.R(128, 64, 192, 128),
		imagex.R(256, 64, 320, 128),
		imagex.R(384, 64, 448, 128),
		imagex.R(512, 64, 576, 128),
	}
	steps := []chain.Step{
		{Type: chain.NoiseStep, Strength: 0.58, Region: chain.RectRegion(regions[0])},
		{Type: chain.BlurStep, Strength: 0.72, Region: chain.RectRegion(regions[1])},
		{Type: chain.BlockStep, Strength: 0.82, Region: chain.RectRegion(regions[2])},
		{Type: chain.ChromaStep, Strength: 0.82, Region: chain.RectRegion(regions[3])},
		{Type: chain.OverexposeStep, Strength: 0.72, Region: chain.RectRegion(regions[4])},
	}
	c := chain.Chain{Steps: steps}

	var implants []synth.ImplantTruth
	for i, r := range regions {
		stepNo := i + 1
		for by := r.MinY / MetricBlockSize; by < r.MaxY/MetricBlockSize; by++ {
			for bx := r.MinX / MetricBlockSize; bx < r.MaxX/MetricBlockSize; bx++ {
				implants = append(implants, synth.ImplantTruth{
					BlockX:        bx,
					BlockY:        by,
					StepIndex:     stepNo,
					StepType:      string(steps[i].Type),
					Region:        r,
					DeclaredShare: attribution.DefaultDeclaredShare,
				})
			}
		}
	}
	cfg := synth.DefaultConfig()
	cfg.Width = Width
	cfg.Height = Height
	cfg.Implants = implants
	return cfg, c
}

func SaveSet(root string, set *synth.Set) error {
	for _, b := range set.Benchmarks {
		pngPath, truthPath := synth.FramePaths(root, b.Truth.Frame.ID)
		if err := imagex.SavePNG(b.Frame, pngPath); err != nil {
			return err
		}
		rawPath := filepath.Join(root, "base", b.Truth.Frame.ID+".rgb")
		if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(rawPath, b.Frame.Pix, 0o644); err != nil {
			return err
		}
		if err := synth.SaveTruth(truthPath, b.Truth); err != nil {
			return err
		}
	}
	return nil
}

func LoadBenchmark(root, frameID string) (*synth.Benchmark, error) {
	pngPath := filepath.Join(root, "base", frameID+".png")
	truthPath := filepath.Join(root, "truth", frameID+".json")
	frame, err := imagex.LoadPNG(pngPath)
	if err != nil {
		return nil, err
	}
	truth, err := synth.LoadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	if err := synth.ValidateLoadedFrame(frame, truth); err != nil {
		return nil, err
	}
	return &synth.Benchmark{Frame: frame, Truth: truth}, nil
}
