package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"maskpipeline/internal/apperr"
)

const (
	AlgorithmVersion     = "maskpipe/v1"
	DefaultSpillAreaRate = 2.5
)

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type PointF struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

type RGB [3]uint8

type Image struct {
	Width    int
	Height   int
	Channels int
	Pix      []uint8
}

func (im *Image) RGBAt(x, y int) RGB {
	i := (y*im.Width + x) * im.Channels
	return RGB{im.Pix[i], im.Pix[i+1], im.Pix[i+2]}
}

func (im *Image) Clone() *Image {
	if im == nil {
		return nil
	}
	return &Image{
		Width:    im.Width,
		Height:   im.Height,
		Channels: im.Channels,
		Pix:      append([]uint8(nil), im.Pix...),
	}
}

type Params struct {
	ColorTolerance       float64 `json:"color_tolerance"`
	GradientThreshold    float64 `json:"gradient_threshold"`
	StructureElementSize int     `json:"structure_element_size"`
	Seeds                []Point `json:"seeds"`
}

func (p Params) Clone() Params {
	q := p
	q.Seeds = append([]Point(nil), p.Seeds...)
	return q
}

func NormalizeParams(p Params) Params {
	p = p.Clone()
	sort.Slice(p.Seeds, func(i, j int) bool {
		if p.Seeds[i].Y != p.Seeds[j].Y {
			return p.Seeds[i].Y < p.Seeds[j].Y
		}
		if p.Seeds[i].X != p.Seeds[j].X {
			return p.Seeds[i].X < p.Seeds[j].X
		}
		return false
	})
	return p
}

func ValidateParams(p Params, width, height int) (Params, error) {
	p = NormalizeParams(p)
	if math.IsNaN(p.ColorTolerance) || math.IsInf(p.ColorTolerance, 0) ||
		p.ColorTolerance < 0 || p.ColorTolerance > 441.6729559300637 {
		return Params{}, apperr.New(apperr.CodePara, "validate_params", "color tolerance must be in [0,441.672956]").
			WithField("color_tolerance").
			WithDetail("value", p.ColorTolerance)
	}
	if math.IsNaN(p.GradientThreshold) || math.IsInf(p.GradientThreshold, 0) ||
		p.GradientThreshold < 0 || p.GradientThreshold > 255 {
		return Params{}, apperr.New(apperr.CodePara, "validate_params", "gradient threshold must be in [0,255]").
			WithField("gradient_threshold").
			WithDetail("value", p.GradientThreshold)
	}
	if p.StructureElementSize < 1 || p.StructureElementSize > 31 || p.StructureElementSize%2 == 0 {
		return Params{}, apperr.New(apperr.CodePara, "validate_params", "structure element size must be an odd integer in [1,31]").
			WithField("structure_element_size").
			WithDetail("value", p.StructureElementSize)
	}
	if len(p.Seeds) == 0 {
		return Params{}, apperr.New(apperr.CodePara, "validate_params", "at least one seed is required").
			WithField("seeds")
	}
	if len(p.Seeds) > 255 {
		return Params{}, apperr.New(apperr.CodePara, "validate_params", "at most 255 seeds are supported").
			WithField("seeds")
	}
	for i, s := range p.Seeds {
		if s.X < 0 || s.X >= width || s.Y < 0 || s.Y >= height {
			return Params{}, apperr.New(apperr.CodePara, "validate_params", "seed coordinate outside image").
				WithField(fmt.Sprintf("seeds[%d]", i)).
				WithDetail("seed", s).
				WithDetail("width", width).
				WithDetail("height", height)
		}
	}
	return p, nil
}

func ValidateFinishParams(p Params, width, height int) error {
	_, err := ValidateParams(p, width, height)
	return err
}

type RegionStats struct {
	ID       int    `json:"id"`
	Seed     Point  `json:"seed,omitempty"`
	Name     string `json:"name,omitempty"`
	Area     int    `json:"area"`
	Centroid PointF `json:"centroid"`
	BBox     Rect   `json:"bbox"`
}

func (r RegionStats) BBoxPoint() Point {
	return Point{X: r.BBox.X, Y: r.BBox.Y}
}

func (r RegionStats) Equal(o RegionStats) bool {
	return r.ID == o.ID && r.Area == o.Area && r.BBox == o.BBox &&
		r.Centroid.X == o.Centroid.X && r.Centroid.Y == o.Centroid.Y
}

type Stats struct {
	RegionCount int           `json:"region_count"`
	Regions     []RegionStats `json:"regions"`
}

func StatsFromLabels(labels []byte, width, height int) Stats {
	type accum struct {
		area       int
		sumX, sumY int64
		minX, minY int
		maxX, maxY int
	}
	byID := make(map[int]*accum)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			id := int(labels[y*width+x])
			if id == 0 {
				continue
			}
			a := byID[id]
			if a == nil {
				a = &accum{minX: x, minY: y, maxX: x, maxY: y}
				byID[id] = a
			}
			a.area++
			a.sumX += int64(x)
			a.sumY += int64(y)
			if x < a.minX {
				a.minX = x
			}
			if y < a.minY {
				a.minY = y
			}
			if x > a.maxX {
				a.maxX = x
			}
			if y > a.maxY {
				a.maxY = y
			}
		}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := Stats{RegionCount: len(ids), Regions: make([]RegionStats, 0, len(ids))}
	for _, id := range ids {
		a := byID[id]
		out.Regions = append(out.Regions, RegionStats{
			ID:   id,
			Area: a.area,
			Centroid: PointF{
				X: float64(a.sumX) / float64(a.area),
				Y: float64(a.sumY) / float64(a.area),
			},
			BBox: Rect{X: a.minX, Y: a.minY, W: a.maxX - a.minX + 1, H: a.maxY - a.minY + 1},
		})
	}
	return out
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return HashBytes(b)
}

func StatsHash(stats Stats) string {
	type hashRegion struct {
		ID       int    `json:"id"`
		Area     int    `json:"area"`
		Centroid PointF `json:"centroid"`
		BBox     Rect   `json:"bbox"`
	}
	type hashView struct {
		RegionCount int          `json:"region_count"`
		Regions     []hashRegion `json:"regions"`
	}
	regions := append([]RegionStats(nil), stats.Regions...)
	sort.Slice(regions, func(i, j int) bool { return regions[i].ID < regions[j].ID })
	viewRegions := make([]hashRegion, 0, len(regions))
	for _, r := range regions {
		viewRegions = append(viewRegions, hashRegion{ID: r.ID, Area: r.Area, Centroid: r.Centroid, BBox: r.BBox})
	}
	return HashJSON(hashView{RegionCount: stats.RegionCount, Regions: viewRegions})
}

func EqualLabels(a, b []byte) bool { return bytes.Equal(a, b) }

type TileStats struct {
	TileIndex int            `json:"tile_index"`
	Width     int            `json:"width"`
	Height    int            `json:"height"`
	Labels    []LabelPartial `json:"labels"`
}

type LabelPartial struct {
	Label int   `json:"label"`
	Area  int   `json:"area"`
	SumX  int64 `json:"sum_x"`
	SumY  int64 `json:"sum_y"`
	MinX  int   `json:"min_x"`
	MinY  int   `json:"min_y"`
	MaxX  int   `json:"max_x"`
	MaxY  int   `json:"max_y"`
}

func TilePartialStats(tileIndex int, labels []byte, width, height, originX, originY int) TileStats {
	type accum struct {
		area       int
		sumX, sumY int64
		minX, minY int
		maxX, maxY int
	}
	byID := make(map[int]*accum)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			id := int(labels[y*width+x])
			if id == 0 {
				continue
			}
			gx, gy := originX+x, originY+y
			a := byID[id]
			if a == nil {
				a = &accum{minX: gx, minY: gy, maxX: gx, maxY: gy}
				byID[id] = a
			}
			a.area++
			a.sumX += int64(gx)
			a.sumY += int64(gy)
			if gx < a.minX {
				a.minX = gx
			}
			if gy < a.minY {
				a.minY = gy
			}
			if gx > a.maxX {
				a.maxX = gx
			}
			if gy > a.maxY {
				a.maxY = gy
			}
		}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := TileStats{TileIndex: tileIndex, Width: width, Height: height, Labels: make([]LabelPartial, 0, len(ids))}
	for _, id := range ids {
		a := byID[id]
		out.Labels = append(out.Labels, LabelPartial{
			Label: id, Area: a.area, SumX: a.sumX, SumY: a.sumY,
			MinX: a.minX, MinY: a.minY, MaxX: a.maxX, MaxY: a.maxY,
		})
	}
	return out
}

func AggregateTileStats(tiles []TileStats) Stats {
	type accum struct {
		area       int
		sumX, sumY int64
		minX, minY int
		maxX, maxY int
	}
	byID := make(map[int]*accum)
	for _, tile := range tiles {
		for _, lp := range tile.Labels {
			a := byID[lp.Label]
			if a == nil {
				a = &accum{minX: lp.MinX, minY: lp.MinY, maxX: lp.MaxX, maxY: lp.MaxY}
				byID[lp.Label] = a
			}
			a.area += lp.Area
			a.sumX += lp.SumX
			a.sumY += lp.SumY
			if lp.MinX < a.minX {
				a.minX = lp.MinX
			}
			if lp.MinY < a.minY {
				a.minY = lp.MinY
			}
			if lp.MaxX > a.maxX {
				a.maxX = lp.MaxX
			}
			if lp.MaxY > a.maxY {
				a.maxY = lp.MaxY
			}
		}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := Stats{RegionCount: len(ids), Regions: make([]RegionStats, 0, len(ids))}
	for _, id := range ids {
		a := byID[id]
		out.Regions = append(out.Regions, RegionStats{
			ID:   id,
			Area: a.area,
			Centroid: PointF{
				X: float64(a.sumX) / float64(a.area),
				Y: float64(a.sumY) / float64(a.area),
			},
			BBox: Rect{X: a.minX, Y: a.minY, W: a.maxX - a.minX + 1, H: a.maxY - a.minY + 1},
		})
	}
	return out
}
