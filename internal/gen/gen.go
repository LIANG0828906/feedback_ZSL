package gen

import (
	"encoding/json"
	"fmt"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
	"terrainkernel/internal/schema"
)

type Options struct {
	Seed        int64
	ChunkCols   int
	ChunkRows   int
	ChunkSizePx int
}

type PixelCoord struct {
	DX int
	DY int
}

type PixelTruth struct {
	PX        int     `json:"px"`
	PY        int     `json:"py"`
	HeightMM  int64   `json:"height_mm"`
	SlopeURad int64   `json:"slope_urad"`
	Weights   []int64 `json:"weights"`
	Dominant  int     `json:"dominant"`
	Blend     int64   `json:"blend"`
}

type SampleSet struct {
	Project      *model.Project
	V1           []byte
	V2           []byte
	V3           []byte
	Traces       []model.TransformTrace
	GroundTruth  map[int][]PixelTruth
	SamplePixels []PixelCoord
	Losses       map[string][]kerr.LossItem
}

func Generate(opts Options) (*SampleSet, error) {
	if opts.ChunkCols <= 0 || opts.ChunkRows <= 0 || opts.ChunkSizePx <= 0 {
		return nil, fmt.Errorf("generator options must have positive dimensions")
	}
	p1 := buildProject(opts)
	up2, err := schema.Upgrade(p1, 2)
	if err != nil {
		return nil, err
	}
	up3, err := schema.Upgrade(up2.Project, 3)
	if err != nil {
		return nil, err
	}
	enrichV3(up3.Project, opts.Seed)
	traces := append([]model.TransformTrace(nil), up2.Traces...)
	traces = append(traces, up3.Traces...)

	v3, err := schema.Marshal(up3.Project, 3)
	if err != nil {
		return nil, err
	}
	v2, v2Loss, err := marshalLossy(up3.Project, 2)
	if err != nil {
		return nil, err
	}
	v1, v1Loss, err := marshalLossy(up3.Project, 1)
	if err != nil {
		return nil, err
	}
	truth, err := buildGroundTruth(up3.Project)
	if err != nil {
		return nil, err
	}
	return &SampleSet{
		Project:      up3.Project,
		V1:           v1,
		V2:           v2,
		V3:           v3,
		Traces:       traces,
		GroundTruth:  truth,
		SamplePixels: []PixelCoord{{0, 0}, {3, 2}, {7, 7}, {4, 5}},
		Losses: map[string][]kerr.LossItem{
			"v1": v1Loss,
			"v2": v2Loss,
		},
	}, nil
}

func marshalLossy(p *model.Project, version int) ([]byte, []kerr.LossItem, error) {
	data, err := schema.Marshal(p, version)
	if err == nil {
		return data, nil, nil
	}
	if loss, ok := kerr.AsLossError(err); ok {
		return data, loss.Items, nil
	}
	return nil, nil, err
}

func buildProject(opts Options) *model.Project {
	specs := []struct {
		id   string
		tint int64
		lo   int64
		hi   int64
	}{
		{id: "dirt", tint: 92, lo: 0, hi: 60_000},
		{id: "grass", tint: 146, lo: 20_000, hi: 80_000},
		{id: "rock", tint: 200, lo: 50_000, hi: 120_000},
		{id: "snow", tint: 240, lo: 90_000, hi: 140_000},
	}
	p := &model.Project{
		SourceVersion: 1,
		ProjectID:     fmt.Sprintf("terrain-%d", opts.Seed),
		NormMode:      "global",
		Seed:          opts.Seed,
		ChunkCols:     opts.ChunkCols,
		ChunkRows:     opts.ChunkRows,
		ChunkSizePx:   opts.ChunkSizePx,
		Layers:        make([]model.Layer, 0, len(specs)),
	}
	for i, spec := range specs {
		mid := (spec.lo + spec.hi) / 2
		p.Layers = append(p.Layers, model.Layer{
			ID:    spec.id,
			Order: i,
			Curve: &model.Curve{Points: []model.CurvePoint{
				{X: spec.lo, Y: 100_000},
				{X: mid, Y: 800_000},
				{X: spec.hi, Y: 200_000},
			}},
			Tint: spec.tint,
		})
	}
	return p
}

func enrichV3(p *model.Project, seed int64) {
	if len(p.Layers) >= 2 {
		p.Layers[1].Noise = &model.Noise{
			Seed:      seed + 11,
			Amplitude: 20_000,
			ScaleMM:   4_000,
		}
	}
	if len(p.Layers) >= 4 {
		p.Layers[3].Curve = &model.Curve{
			Driver: "slope",
			Points: []model.CurvePoint{
				{X: 0, Y: 100_000},
				{X: 750_000, Y: 500_000},
				{X: 1_600_000, Y: 900_000},
			},
		}
	}
}

func buildGroundTruth(p *model.Project) (map[int][]PixelTruth, error) {
	out := make(map[int][]PixelTruth, p.ChunkCount())
	samples := []PixelCoord{{0, 0}, {3, 2}, {7, 7}, {4, 5}}
	for id := 0; id < p.ChunkCount(); id++ {
		originX, originY, _ := p.ChunkOrigin(id)
		var truths []PixelTruth
		for _, sample := range samples {
			px, py := originX+sample.DX, originY+sample.DY
			pixel, err := bake.BakePixel(p, px, py)
			if err != nil {
				return nil, err
			}
			truths = append(truths, PixelTruth{
				PX:        px,
				PY:        py,
				HeightMM:  pixel.HeightMM,
				SlopeURad: pixel.SlopeURad,
				Weights:   append([]int64(nil), pixel.Weights...),
				Dominant:  pixel.Dominant,
				Blend:     pixel.Blend,
			})
		}
		out[id] = truths
	}
	return out, nil
}

func MarshalGroundTruth(truth map[int][]PixelTruth) ([]byte, error) {
	data, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}
