package strokelab

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

const (
	// Internal weights are per-ten-thousand (0..10000). Internal hardness is
	// per-mille (0..1000). Coordinates are integer pixels.
	WeightMax   = 10000
	PermilleMax = 1000

	AlgorithmVersion     = "stroke-compose/v1"
	ConversionVersion    = "stroke-convert/v1"
	CheckpointFormat     = "stroke-checkpoint/v1"
	DiagnosticFormat     = "stroke-diagnostic/v1"
	DefaultBlockSize     = 16
	DefaultMaskThreshold = 128
)

// RoundingMode names the only remainder policies used by the converter and
// rasterizer. No floating point is accumulated anywhere in composition.
type RoundingMode string

const (
	RoundTowardZero       RoundingMode = "toward_zero"
	RoundHalfAwayFromZero RoundingMode = "half_away_from_zero"
	RoundHalfEven         RoundingMode = "half_even"
)

func (m RoundingMode) valid() bool {
	return m == RoundTowardZero || m == RoundHalfAwayFromZero || m == RoundHalfEven
}

type Point struct {
	X int
	Y int
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) PixelCount() int {
	if r.W <= 0 || r.H <= 0 {
		return 0
	}
	return r.W * r.H
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

func (r Rect) Intersects(o Rect) bool {
	return r.X < o.X+o.W && o.X < r.X+r.W && r.Y < o.Y+o.H && o.Y < r.Y+r.H
}

func (r Rect) Intersect(o Rect) Rect {
	x0 := max(r.X, o.X)
	y0 := max(r.Y, o.Y)
	x1 := min(r.X+r.W, o.X+o.W)
	y1 := min(r.Y+r.H, o.Y+o.H)
	return Rect{X: x0, Y: y0, W: max(0, x1-x0), H: max(0, y1-y0)}
}

func (r Rect) Union(o Rect) Rect {
	if r.Empty() {
		return o
	}
	if o.Empty() {
		return r
	}
	x0 := min(r.X, o.X)
	y0 := min(r.Y, o.Y)
	x1 := max(r.X+r.W, o.X+o.W)
	y1 := max(r.Y+r.H, o.Y+o.H)
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// Stroke is the normalized internal construction record.
type Stroke struct {
	ID         string `json:"id"`
	Group      string `json:"group"`
	DeclOrder  int    `json:"decl_order"`
	Start      Point  `json:"start"`
	End        Point  `json:"end"`
	Radius     int    `json:"radius"`
	Hardness   int    `json:"hardness_permille"`
	Weight     int    `json:"weight_pp10000"`
	Stage      int    `json:"stage"`
	Seq        int    `json:"seq"`
	SourcePath string `json:"source_path,omitempty"`
}

func (s Stroke) Validate() error {
	if s.ID == "" {
		return coded(CodeVersion, "stroke id is required")
	}
	if s.Radius < 0 || s.Radius > 4096 {
		return coded(CodeVersion, "stroke %s radius out of range: %d", s.ID, s.Radius)
	}
	if s.Hardness < 0 || s.Hardness > PermilleMax {
		return coded(CodeVersion, "stroke %s hardness out of range: %d", s.ID, s.Hardness)
	}
	if s.Weight < 0 || s.Weight > WeightMax {
		return coded(CodeVersion, "stroke %s weight out of range: %d", s.ID, s.Weight)
	}
	if s.Stage < 0 || s.Seq <= 0 {
		return coded(CodeVersion, "stroke %s stage/seq out of range: %d/%d", s.ID, s.Stage, s.Seq)
	}
	return nil
}

func (s Stroke) Bounds() Rect {
	x0 := min(s.Start.X, s.End.X) - s.Radius - 2
	y0 := min(s.Start.Y, s.End.Y) - s.Radius - 2
	x1 := max(s.Start.X, s.End.X) + s.Radius + 3
	y1 := max(s.Start.Y, s.End.Y) + s.Radius + 3
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func (s Stroke) ContentHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d,%d|%d,%d|r%d|h%d|w%d|stage%d|seq%d",
		s.ID, s.Group, s.Start.X, s.Start.Y, s.End.X, s.End.Y,
		s.Radius, s.Hardness, s.Weight, s.Stage, s.Seq)
	return hex.EncodeToString(h.Sum(nil))
}

type Group struct {
	Name  string `json:"name"`
	Stage int    `json:"stage"`
}

type ConversionTrace struct {
	Version       string `json:"version"`
	Path          string `json:"path"`
	OldValue      string `json:"old_value"`
	NewValue      string `json:"new_value"`
	Rule          string `json:"rule"`
	SourceDefault string `json:"source_default,omitempty"`
	Location      string `json:"location,omitempty"`
	Residual      int64  `json:"residual,omitempty"`
}

type LossItem struct {
	Code        string `json:"code"`
	Path        string `json:"path"`
	Reason      string `json:"reason"`
	Value       string `json:"value"`
	Disposition string `json:"disposition"`
	Residual    int64  `json:"residual,omitempty"`
}

type Project struct {
	ID                string            `json:"id"`
	Seed              int64             `json:"seed"`
	Name              string            `json:"name"`
	SourceVersion     string            `json:"source_version"`
	Width             int               `json:"width"`
	Height            int               `json:"height"`
	BlockSize         int               `json:"block_size"`
	Rounding          RoundingMode      `json:"rounding"`
	CandidateMask     []uint8           `json:"candidate_mask"`
	Groups            []Group           `json:"groups"`
	Strokes           []Stroke          `json:"strokes"`
	Truth             *Truth            `json:"truth"`
	ConversionTrace   []ConversionTrace `json:"conversion_trace"`
	SourceDocument    string            `json:"source_document,omitempty"`
	ConversionVersion string            `json:"conversion_version"`
}

func (p *Project) Validate() error {
	if p == nil {
		return coded(CodeVersion, "project is nil")
	}
	if p.ID == "" {
		return coded(CodeVersion, "project id is required")
	}
	if p.Width <= 0 || p.Height <= 0 || p.Width > 4096 || p.Height > 4096 {
		return coded(CodeVersion, "invalid project dimensions %dx%d", p.Width, p.Height)
	}
	if len(p.CandidateMask) != p.Width*p.Height {
		return coded(CodeVersion, "candidate mask length %d, want %d", len(p.CandidateMask), p.Width*p.Height)
	}
	if p.BlockSize <= 0 {
		return coded(CodeVersion, "block size must be positive")
	}
	if !p.Rounding.valid() {
		return coded(CodeVersion, "unknown rounding mode %q", p.Rounding)
	}
	if len(p.Strokes) == 0 {
		return coded(CodeVersion, "at least one stroke is required")
	}
	ids := make(map[string]struct{}, len(p.Strokes))
	groups := make(map[string]Group, len(p.Groups))
	for _, g := range p.Groups {
		if g.Name == "" {
			return coded(CodeVersion, "group name is required")
		}
		if _, ok := groups[g.Name]; ok {
			return coded(CodeVersion, "duplicate group %q", g.Name)
		}
		groups[g.Name] = g
	}
	for i := range p.Strokes {
		s := &p.Strokes[i]
		if _, ok := ids[s.ID]; ok {
			return coded(CodeVersion, "duplicate stroke id %q", s.ID)
		}
		ids[s.ID] = struct{}{}
		if err := s.Validate(); err != nil {
			return err
		}
		if s.Group != "" {
			if _, ok := groups[s.Group]; !ok {
				return coded(CodeVersion, "stroke %s references unknown group %q", s.ID, s.Group)
			}
		}
		if s.Start.X < 0 || s.Start.Y < 0 || s.Start.X >= p.Width || s.Start.Y >= p.Height ||
			s.End.X < 0 || s.End.Y < 0 || s.End.X >= p.Width || s.End.Y >= p.Height {
			return coded(CodeVersion, "stroke %s endpoints outside canvas", s.ID)
		}
	}
	if p.Truth == nil {
		return coded(CodeVersion, "truth is required")
	}
	if len(p.Truth.ReferenceMask) != len(p.CandidateMask) {
		return coded(CodeVersion, "truth mask length %d, want %d", len(p.Truth.ReferenceMask), len(p.CandidateMask))
	}
	return nil
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	q := *p
	q.CandidateMask = append([]uint8(nil), p.CandidateMask...)
	q.Groups = append([]Group(nil), p.Groups...)
	q.Strokes = append([]Stroke(nil), p.Strokes...)
	q.ConversionTrace = append([]ConversionTrace(nil), p.ConversionTrace...)
	if p.Truth != nil {
		t := *p.Truth
		t.ReferenceMask = append([]uint8(nil), p.Truth.ReferenceMask...)
		t.Domains = append([]TruthDomain(nil), p.Truth.Domains...)
		t.Contributions = make([]TruthContribution, len(p.Truth.Contributions))
		for i, c := range p.Truth.Contributions {
			t.Contributions[i] = c
			t.Contributions[i].Mask = append([]uint8(nil), c.Mask...)
		}
		q.Truth = &t
	}
	return &q
}

type OrderedStroke struct {
	Stroke   Stroke
	Origin   int
	Position int
}

// OrderStrokes applies the declared stage priority first, operation sequence
// second, and rejects ties because a tie would make composition ambiguous.
func OrderStrokes(strokes []Stroke) ([]OrderedStroke, error) {
	out := make([]OrderedStroke, len(strokes))
	for i, s := range strokes {
		out[i] = OrderedStroke{Stroke: s, Origin: i}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stroke.Stage != out[j].Stroke.Stage {
			return out[i].Stroke.Stage < out[j].Stroke.Stage
		}
		return out[i].Stroke.Seq < out[j].Stroke.Seq
	})
	seen := make(map[[2]int]string, len(out))
	for i := range out {
		key := [2]int{out[i].Stroke.Stage, out[i].Stroke.Seq}
		if prev, ok := seen[key]; ok {
			return nil, coded(CodeOrder,
				"ambiguous stroke order: %s and %s share stage=%d seq=%d",
				prev, out[i].Stroke.ID, key[0], key[1])
		}
		seen[key] = out[i].Stroke.ID
		out[i].Position = i + 1
	}
	return out, nil
}

func OrderVersion(ordered []OrderedStroke) string {
	h := sha256.New()
	for _, s := range ordered {
		fmt.Fprintf(h, "%s|%d|%d;", s.Stroke.ID, s.Stroke.Stage, s.Stroke.Seq)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalOrderedStrokes(p *Project) ([]OrderedStroke, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return OrderStrokes(p.Strokes)
}
