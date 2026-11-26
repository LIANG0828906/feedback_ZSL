// Package domain defines the canonical spectral project and block coordinates.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaV1 = "gameart-ms/v1"
	SchemaV2 = "spectra-exchange/v2"
	SchemaV3 = "multispectral-composition/v3"

	InternalQuantScale int64  = 10_000
	ParamVersion       string = "params-v1"
	AlgorithmVersion   string = "adaptive-quant-1.0.0"
	SQLSchemaVersion   int    = 1
)

type SampleScale string

const (
	ScaleSparse SampleScale = "sparse"
	ScaleMedium SampleScale = "medium"
	ScaleDense  SampleScale = "dense"
)

type BlockCoord struct {
	BX int `json:"bx"`
	BY int `json:"by"`
}

func (b BlockCoord) String() string { return fmt.Sprintf("%d,%d", b.BX, b.BY) }

func ParseBlockCoord(s string) (BlockCoord, error) {
	var bx, by int
	_, err := fmt.Sscanf(s, "%d,%d", &bx, &by)
	if err != nil {
		return BlockCoord{}, err
	}
	return BlockCoord{BX: bx, BY: by}, nil
}

type Trace struct {
	Op          string `json:"op"`
	Schema      string `json:"schema,omitempty"`
	Path        string `json:"path"`
	OldValue    string `json:"old_value,omitempty"`
	NewValue    string `json:"new_value,omitempty"`
	Source      string `json:"source,omitempty"`
	Description string `json:"description,omitempty"`
}

type LossItem struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Value       string `json:"value"`
	Action      string `json:"action"`
	PreservedIn string `json:"preserved_in,omitempty"`
	Reason      string `json:"reason"`
}

type LossReport struct {
	Code   string     `json:"code"`
	From   string     `json:"from"`
	To     string     `json:"to"`
	Digest string     `json:"digest"`
	Items  []LossItem `json:"items"`
}

func (r *LossReport) RecomputeDigest() string {
	if r == nil || len(r.Items) == 0 {
		r.Digest = ""
		return ""
	}
	b, _ := json.Marshal(struct {
		From  string     `json:"from"`
		To    string     `json:"to"`
		Items []LossItem `json:"items"`
	}{From: r.From, To: r.To, Items: r.Items})
	sum := sha256.Sum256(b)
	r.Digest = hex.EncodeToString(sum[:16])
	return r.Digest
}

type Band struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	GroupPath    []string `json:"group_path"`
	CenterNM     int      `json:"center_nm"`
	GainPPM      int64    `json:"gain_ppm"`
	GainExplicit bool     `json:"gain_explicit"`
	// Response is target-linear response in basis points per quantized unit.
	Response [3]int64 `json:"response_bp"`
}

type Exposure struct {
	ID         string `json:"id"`
	ExposureUS int64  `json:"exposure_us"`
	Reference  bool   `json:"reference,omitempty"`
}

type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Path     []string `json:"path"`
	BandIDs  []string `json:"band_ids,omitempty"`
	Children []*Group `json:"children,omitempty"`
}

type Project struct {
	ProjectID      string     `json:"project_id"`
	Name           string     `json:"name"`
	SourceSchema   string     `json:"source_schema"`
	Seed           int64      `json:"seed"`
	Width          int        `json:"width"`
	Height         int        `json:"height"`
	BlockWidth     int        `json:"block_width"`
	BlockHeight    int        `json:"block_height"`
	QuantScale     int64      `json:"quant_scale"`
	ReferenceExpUS int64      `json:"reference_exposure_us"`
	FullWellDose   int64      `json:"full_well_dose_us_rad"`
	WhitePointBP   [3]int64   `json:"white_point_bp"`
	Bands          []Band     `json:"bands"`
	Exposures      []Exposure `json:"exposures"`
	Tree           []*Group   `json:"tree,omitempty"`
	Traces         []Trace    `json:"field_traces,omitempty"`
}

func (p *Project) BandIndex(id string) int {
	for i := range p.Bands {
		if p.Bands[i].ID == id {
			return i
		}
	}
	return -1
}

func (p *Project) BlockCount() (bx, by int) {
	bx = (p.Width + p.BlockWidth - 1) / p.BlockWidth
	by = (p.Height + p.BlockHeight - 1) / p.BlockHeight
	return
}

func (p *Project) Coords() []BlockCoord {
	nx, ny := p.BlockCount()
	out := make([]BlockCoord, 0, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			out = append(out, BlockCoord{BX: x, BY: y})
		}
	}
	return out
}

func (p *Project) BlockPixels(c BlockCoord) (indices []int, xs, ys []int) {
	x0 := c.BX * p.BlockWidth
	y0 := c.BY * p.BlockHeight
	for y := y0; y < y0+p.BlockHeight && y < p.Height; y++ {
		for x := x0; x < x0+p.BlockWidth && x < p.Width; x++ {
			indices = append(indices, y*p.Width+x)
			xs = append(xs, x)
			ys = append(ys, y)
		}
	}
	return
}

// CanonicalHash hashes semantic project data but intentionally excludes source
// provenance/traces. Equivalent v1/v2/v3 documents therefore share a hash.
func (p *Project) CanonicalHash() string {
	cp := *p
	cp.SourceSchema = ""
	cp.Traces = nil
	b, _ := json.Marshal(cp.canonicalValue())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *Project) canonicalValue() any {
	bands := make([]map[string]any, 0, len(p.Bands))
	for _, b := range p.Bands {
		bands = append(bands, map[string]any{
			"id": b.ID, "name": b.Name, "group_path": b.GroupPath,
			"center_nm": b.CenterNM, "gain_ppm": b.GainPPM,
			"response_bp": b.Response,
		})
	}
	exps := make([]map[string]any, 0, len(p.Exposures))
	for i, ex := range p.Exposures {
		exps = append(exps, map[string]any{"index": i, "us": ex.ExposureUS, "ref": ex.Reference})
	}
	sort.Slice(bands, func(i, j int) bool { return bands[i]["id"].(string) < bands[j]["id"].(string) })
	return map[string]any{
		"project_id": p.ProjectID, "seed": p.Seed, "w": p.Width, "h": p.Height,
		"bw": p.BlockWidth, "bh": p.BlockHeight, "q": p.QuantScale,
		"ref_us": p.ReferenceExpUS, "full_well": p.FullWellDose,
		"white": p.WhitePointBP, "bands": bands, "exposures": exps,
	}
}

func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ShortHash(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:16]
}
