package model

import "sort"

const WeightScale int64 = 1_000_000

type CurvePoint struct {
	X int64
	Y int64
}

type Curve struct {
	Driver string
	Points []CurvePoint
}

type Noise struct {
	Seed      int64
	Amplitude int64
	ScaleMM   int64
}

type Layer struct {
	ID           string
	Order        int
	Expr         string
	Constant     int64
	Curve        *Curve
	Noise        *Noise
	Tint         int64
	SlopeCapURad int64
}

type Project struct {
	SourceVersion int
	ProjectID     string
	NormMode      string
	Seed          int64
	ChunkCols     int
	ChunkRows     int
	ChunkSizePx   int
	Layers        []Layer
}

type TransformTrace struct {
	Path        string `json:"path"`
	OldValue    string `json:"old_value"`
	DefaultFrom string `json:"default_from"`
}

type UpgradeResult struct {
	Project *Project
	Traces  []TransformTrace
}

type ChunkCoord struct {
	C int
	R int
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Layers = make([]Layer, len(p.Layers))
	for i, layer := range p.Layers {
		out.Layers[i] = layer
		if layer.Curve != nil {
			c := *layer.Curve
			c.Points = append([]CurvePoint(nil), layer.Curve.Points...)
			out.Layers[i].Curve = &c
		}
		if layer.Noise != nil {
			n := *layer.Noise
			out.Layers[i].Noise = &n
		}
	}
	return &out
}

func (p *Project) ChunkCount() int {
	if p == nil || p.ChunkCols <= 0 || p.ChunkRows <= 0 {
		return 0
	}
	return p.ChunkCols * p.ChunkRows
}

func (p *Project) ChunkByID(id int) (ChunkCoord, bool) {
	if p == nil || id < 0 || id >= p.ChunkCount() {
		return ChunkCoord{}, false
	}
	return ChunkCoord{C: id % p.ChunkCols, R: id / p.ChunkCols}, true
}

func (p *Project) ChunkOrigin(id int) (x, y int, ok bool) {
	c, ok := p.ChunkByID(id)
	if !ok {
		return 0, 0, false
	}
	return c.C * p.ChunkSizePx, c.R * p.ChunkSizePx, true
}

func (p *Project) ChunkIDs() []int {
	ids := make([]int, p.ChunkCount())
	for i := range ids {
		ids[i] = i
	}
	return ids
}

func (p *Project) LayerByID(id string) *Layer {
	for i := range p.Layers {
		if p.Layers[i].ID == id {
			return &p.Layers[i]
		}
	}
	return nil
}

func (p *Project) LayersByOrder() []Layer {
	out := append([]Layer(nil), p.Layers...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order == out[j].Order {
			return out[i].ID < out[j].ID
		}
		return out[i].Order < out[j].Order
	})
	return out
}
