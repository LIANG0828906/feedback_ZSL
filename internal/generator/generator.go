package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
	"qualitybench/internal/ops"
	"qualitybench/internal/quality"
)

type Options struct {
	Seed             int64
	ImageCount       int
	Width            int
	Height           int
	BlockSize        int
	AttributionShare float64
	MetricTolerance  float64
	AlgorithmVersion string
}

func DefaultOptions() Options {
	return Options{
		Seed:             424242,
		ImageCount:       3,
		Width:            192,
		Height:           128,
		BlockSize:        16,
		AttributionShare: 0.22,
		MetricTolerance:  0.08,
		AlgorithmVersion: ops.AlgorithmVersion,
	}
}

type Dataset struct {
	Project   *model.Project
	Truth     *model.Truth
	Baseline  map[string]*core.Frame
	Reference map[string][]*core.Frame
	ProjectFP string
	TruthFP   string
}

func Generate(opts Options) (*Dataset, error) {
	if opts.ImageCount <= 0 {
		return nil, fmt.Errorf("image count must be positive")
	}
	if opts.Width <= 0 || opts.Height <= 0 || opts.BlockSize <= 0 {
		return nil, fmt.Errorf("invalid geometry")
	}
	if opts.Width%opts.BlockSize != 0 || opts.Height%opts.BlockSize != 0 {
		return nil, fmt.Errorf("block size must divide frame")
	}
	p := &model.Project{
		SchemaVersion:    "qualitybench.project.v1",
		Seed:             opts.Seed,
		Width:            opts.Width,
		Height:           opts.Height,
		Channels:         3,
		BlockSize:        opts.BlockSize,
		AttributionShare: opts.AttributionShare,
		MetricTolerance:  opts.MetricTolerance,
		AlgorithmVersion: opts.AlgorithmVersion,
	}
	for i := 0; i < opts.ImageCount; i++ {
		spec := makeImageSpec(opts, i)
		p.Images = append(p.Images, spec)
		p.Chains = append(p.Chains, makeChain(opts, i, spec.ID))
	}
	p.Edits = makeEdits(opts, p)
	if err := model.ValidateProject(p); err != nil {
		return nil, err
	}
	projectFP, err := model.Fingerprint(p)
	if err != nil {
		return nil, err
	}
	ds := &Dataset{
		Project:   p,
		Baseline:  make(map[string]*core.Frame),
		Reference: make(map[string][]*core.Frame),
		ProjectFP: projectFP,
	}
	truth := &model.Truth{
		SchemaVersion:        "qualitybench.truth.v1",
		ProjectFingerprint:   projectFP,
		AlgorithmVersion:     p.AlgorithmVersion,
		Seed:                 p.Seed,
		Width:                p.Width,
		Height:               p.Height,
		Channels:             p.Channels,
		BlockSize:            p.BlockSize,
		AttributionShare:     p.AttributionShare,
		AttributionTolerance: 0.02,
	}
	for i, spec := range p.Images {
		base, err := drawBaseline(p, spec)
		if err != nil {
			return nil, err
		}
		chain := p.Chains[i]
		frames := []*core.Frame{base}
		cur := base
		for stepIndex, step := range chain.Steps {
			next, err := ops.ApplyStep(cur, step, ops.Context{Seed: spec.Seed, StepIndex: stepIndex})
			if err != nil {
				return nil, err
			}
			frames = append(frames, next)
			cur = next
		}
		ds.Baseline[spec.ID] = base
		ds.Reference[spec.ID] = frames
		imageTruth, err := makeImageTruth(p, spec, chain, base, frames, opts.MetricTolerance)
		if err != nil {
			return nil, err
		}
		truth.Images = append(truth.Images, imageTruth)
	}
	truthFP, err := model.Fingerprint(truth)
	if err != nil {
		return nil, err
	}
	ds.Truth = truth
	ds.TruthFP = truthFP
	if err := model.ValidateTruth(truth, p); err != nil {
		return nil, err
	}
	return ds, nil
}

func WriteDataset(dir string, opts Options) (*Dataset, string, error) {
	ds, err := Generate(opts)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "baseline"), 0o755); err != nil {
		return nil, "", err
	}
	for id, frame := range ds.Baseline {
		if err := frame.WritePNG(filepath.Join(dir, "baseline", id+".png")); err != nil {
			return nil, "", err
		}
	}
	projectPath := filepath.Join(dir, "project.json")
	truthPath := filepath.Join(dir, "truth.json")
	if err := writeJSON(projectPath, ds.Project); err != nil {
		return nil, "", err
	}
	if err := writeJSON(truthPath, ds.Truth); err != nil {
		return nil, "", err
	}
	return ds, dir, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func makeImageSpec(opts Options, index int) model.ImageSpec {
	seed := opts.Seed + int64(index+1)*100003
	watermark := core.Rect{X: opts.Width - 58, Y: opts.Height - 22, W: 50, H: 16}
	shapes := []model.Shape{
		{ID: "shape-a", Kind: "rect", BBox: core.Rect{X: 18 + index*2, Y: 14, W: 38, H: 30}, Color: [3]uint8{190, 72, 56}},
		{ID: "shape-b", Kind: "ellipse", BBox: core.Rect{X: 72, Y: 12 + index*2, W: 44, H: 32}, Color: [3]uint8{55, 145, 122}},
		{ID: "shape-c", Kind: "triangle", BBox: core.Rect{X: 128, Y: 18, W: 42, H: 32}, Color: [3]uint8{186, 154, 52}},
		{ID: "shape-d", Kind: "diamond", BBox: core.Rect{X: 32 + index*3, Y: 62, W: 40, H: 28}, Color: [3]uint8{92, 104, 194}},
	}
	if index == 1 {
		shapes[0].Kind = "ellipse"
		shapes[1].Kind = "diamond"
		shapes[2].Kind = "rect"
		shapes[3].Kind = "triangle"
	}
	if index == 2 {
		shapes[0].Kind = "triangle"
		shapes[1].Kind = "rect"
		shapes[2].Kind = "ellipse"
		shapes[3].Kind = "rect"
	}
	details := []model.Detail{
		{BBox: core.Rect{X: 10, Y: opts.Height - 28, W: 48, H: 7}, Period: 4, Delta: 7},
		{BBox: core.Rect{X: 72, Y: opts.Height - 27, W: 48, H: 6}, Period: 6, Delta: 5},
	}
	return model.ImageSpec{
		ID:        fmt.Sprintf("img-%02d", index),
		Frame:     index*7 + 3,
		Seed:      seed,
		Shapes:    shapes,
		Watermark: watermark,
		Details:   details,
	}
}

func makeChain(opts Options, index int, imageID string) model.Chain {
	w, h := opts.Width, opts.Height
	left := core.Rect{X: 8, Y: 6, W: w / 2, H: h * 2 / 3}
	right := core.Rect{X: w/2 - 4, Y: 18, W: w/2 - 4, H: h * 3 / 5}
	topRight := core.Rect{X: w / 2, Y: 4, W: w/2 - 4, H: h / 2}
	center := core.Rect{X: w / 4, Y: h / 3, W: w / 2, H: h / 2}
	steps := []model.Step{
		{Type: model.StepNoise, Strength: 0.20},
		{Type: model.StepBlur, Strength: 0.50, Region: &left},
		{Type: model.StepBlockiness, Strength: 0.30},
		{Type: model.StepChroma, Strength: 0.34, Region: &right},
		{Type: model.StepOverexpose, Strength: 0.36, Region: &topRight},
	}
	switch index {
	case 1:
		steps = []model.Step{
			{Type: model.StepBlur, Strength: 0.55, Region: &left},
			{Type: model.StepNoise, Strength: 0.23},
			{Type: model.StepChroma, Strength: -0.30, Region: &center},
			{Type: model.StepBlockiness, Strength: 0.24},
			{Type: model.StepOverexpose, Strength: 0.30, Region: &topRight},
		}
	case 2:
		steps = []model.Step{
			{Type: model.StepNoise, Strength: 0.16},
			{Type: model.StepBlockiness, Strength: 0.31},
			{Type: model.StepBlur, Strength: 0.38, Region: &center},
			{Type: model.StepOverexpose, Strength: 0.42, Region: &topRight},
			{Type: model.StepChroma, Strength: 0.26, Region: &right},
		}
	}
	return model.Chain{ID: fmt.Sprintf("chain-%02d", index), ImageID: imageID, Steps: steps}
}

func makeEdits(opts Options, p *model.Project) []model.Edit {
	imageID := p.Images[0].ID
	strength := 0.72
	region := core.Rect{X: 8, Y: 18, W: opts.Width/2 - 8, H: opts.Height / 2}
	return []model.Edit{
		{Op: "set_strength", ImageID: imageID, Step: 2, Strength: &strength},
		{Op: "set_region", ImageID: imageID, Step: 4, Region: &region},
		{Op: "move_step", ImageID: imageID, From: 5, To: 2},
	}
}

func makeImageTruth(p *model.Project, spec model.ImageSpec, chain model.Chain, base *core.Frame,
	frames []*core.Frame, metricTolerance float64) (model.ImageTruth, error) {
	out := model.ImageTruth{
		ID:             spec.ID,
		Frame:          spec.Frame,
		StepCount:      len(chain.Steps),
		Steps:          chain.Clone().Steps,
		BaselineSHA256: base.Hash(),
		Shapes:         spec.Shapes,
		Watermark:      spec.Watermark,
	}
	points := chooseSamples(spec, p.Width, p.Height)
	final := frames[len(frames)-1]
	for _, sp := range points {
		x := int(math.Floor(sp.X))
		y := int(math.Floor(sp.Y))
		out.Samples = append(out.Samples, model.SampleTruth{
			X: sp.X, Y: sp.Y, Expected: final.At(x, y), Tolerance: 0, Label: sp.Label,
		})
	}
	for by := 0; by < p.Height; by += p.BlockSize {
		for bx := 0; bx < p.Width; bx += p.BlockSize {
			rect := core.Rect{X: bx, Y: by, W: p.BlockSize, H: p.BlockSize}
			eval := quality.EvaluateBlock(base, frames[1:], rect, p.AttributionShare, chain)
			out.Blocks = append(out.Blocks, model.BlockTruth{
				X: bx, Y: by, W: p.BlockSize, H: p.BlockSize,
				Metrics: quality.MetricIntervals(eval.Metrics, metricTolerance),
				Attribution: model.AttributionTruth{
					Step:      eval.Attribution.Step,
					Category:  eval.Attribution.Category,
					Share:     eval.Attribution.Share,
					Tolerance: 0.02,
				},
			})
		}
	}
	sort.Slice(out.Blocks, func(i, j int) bool {
		if out.Blocks[i].Y != out.Blocks[j].Y {
			return out.Blocks[i].Y < out.Blocks[j].Y
		}
		return out.Blocks[i].X < out.Blocks[j].X
	})
	return out, nil
}

type samplePoint struct {
	X, Y  float64
	Label string
}

func chooseSamples(spec model.ImageSpec, width, height int) []samplePoint {
	var out []samplePoint
	for _, s := range spec.Shapes {
		out = append(out, samplePoint{
			X:     float64(s.BBox.X + s.BBox.W/2),
			Y:     float64(s.BBox.Y + s.BBox.H/2),
			Label: s.ID,
		})
	}
	out = append(out,
		samplePoint{X: 8, Y: 8, Label: "background-top-left"},
		samplePoint{X: float64(width - 10), Y: 8, Label: "background-top-right"},
		samplePoint{X: float64(spec.Watermark.X + spec.Watermark.W/2), Y: float64(spec.Watermark.Y + spec.Watermark.H/2), Label: "watermark"},
	)
	if len(spec.Details) > 0 {
		d := spec.Details[0].BBox
		out = append(out, samplePoint{X: float64(d.X + d.W/2), Y: float64(d.Y + d.H/2), Label: "low-contrast-detail"})
	}
	seen := make(map[string]bool)
	filtered := out[:0]
	for _, s := range out {
		key := fmt.Sprintf("%d,%d", int(s.X), int(s.Y))
		if s.X < 0 || s.Y < 0 || s.X >= float64(width) || s.Y >= float64(height) || seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, s)
	}
	return filtered
}

func ValidateReferences(ds *Dataset) error {
	if ds == nil || ds.Project == nil || ds.Truth == nil {
		return fmt.Errorf("incomplete dataset")
	}
	for i, spec := range ds.Project.Images {
		refs := ds.Reference[spec.ID]
		if len(refs) != len(ds.Project.Chains[i].Steps)+1 {
			return fmt.Errorf("image %s has %d reference frames", spec.ID, len(refs))
		}
	}
	return nil
}

func FingerprintFiles(projectPath, truthPath string) (string, string, error) {
	pdata, err := os.ReadFile(projectPath)
	if err != nil {
		return "", "", err
	}
	tdata, err := os.ReadFile(truthPath)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(core.HashParts("project-file", string(pdata))),
		strings.TrimSpace(core.HashParts("truth-file", string(tdata))), nil
}
