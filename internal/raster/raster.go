// Package raster converts world-space stroke bands into center-sampled masks
// and per-skeleton-point density statistics. It intentionally does not perform
// anti-aliased coverage sampling.
package raster

import (
	"math"
	"sort"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/model"
	"glyphbrush/internal/skeleton"
	"glyphbrush/internal/stroke"
)

type Config struct {
	Scale        float64 `json:"scale"`
	GridSize     int     `json:"gridSize"`
	MaxDimension int     `json:"maxDimension"`
}

func DefaultConfig() Config {
	return Config{Scale: 1, GridSize: 1, MaxDimension: 4096}
}

func (c Config) Validate() *errs.Error {
	if c.Scale <= 0 || math.IsNaN(c.Scale) || math.IsInf(c.Scale, 0) {
		return errs.New(errs.CodeParam, "scale must be finite and greater than zero").WithParam("scale")
	}
	if c.GridSize <= 0 {
		return errs.New(errs.CodeParam, "gridSize must be a positive integer").WithParam("gridSize")
	}
	if c.MaxDimension <= 0 {
		return errs.New(errs.CodeParam, "maxDimension must be positive").WithParam("maxDimension")
	}
	return nil
}

type Grid struct {
	Width       int         `json:"width"`
	Height      int         `json:"height"`
	Scale       float64     `json:"scale"`
	GridSize    int         `json:"gridSize"`
	TruthBounds geom.Bounds `json:"truthBounds"`
	WorldBounds geom.Bounds `json:"worldBounds"`
	MarginUnits float64     `json:"marginUnits"`
	Allowance   float64     `json:"allowance"`
}

// NewGrid derives the pixel grid from the truth bounding box, raster scale,
// grid quantum and the stroke allowance. The world bounds are snapped outward
// to the grid quantum.
func NewGrid(truthBounds geom.Bounds, width float64, cfg Config) (*Grid, *errs.Error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !truthBounds.Valid() {
		return nil, errs.New(errs.CodeGrid, "truth bounds are not valid").WithParam("grid")
	}
	allowance := 0.0
	if width > 0 {
		allowance = width/2*1.35 + 3
	}
	world := truthBounds.Expand(allowance)
	quant := float64(cfg.GridSize) / cfg.Scale
	world.MinX = math.Floor(world.MinX/quant) * quant
	world.MinY = math.Floor(world.MinY/quant) * quant
	world.MaxX = math.Ceil(world.MaxX/quant) * quant
	world.MaxY = math.Ceil(world.MaxY/quant) * quant
	w := int(math.Round(world.Width() * cfg.Scale))
	h := int(math.Round(world.Height() * cfg.Scale))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if cfg.GridSize > 1 {
		w = ((w + cfg.GridSize - 1) / cfg.GridSize) * cfg.GridSize
		h = ((h + cfg.GridSize - 1) / cfg.GridSize) * cfg.GridSize
		world.MaxX = world.MinX + float64(w)/cfg.Scale
		world.MaxY = world.MinY + float64(h)/cfg.Scale
	}
	if w > cfg.MaxDimension || h > cfg.MaxDimension {
		return nil, errs.New(errs.CodeGrid,
			"derived grid %dx%d exceeds maxDimension %d", w, h, cfg.MaxDimension).WithParam("grid")
	}
	return &Grid{
		Width:       w,
		Height:      h,
		Scale:       cfg.Scale,
		GridSize:    cfg.GridSize,
		TruthBounds: truthBounds,
		WorldBounds: world,
		MarginUnits: allowance,
		Allowance:   allowance,
	}, nil
}

// WorldToPixel maps a world point to continuous pixel coordinates, with Y
// pointing down. The caller can test the destination against the grid.
func (g *Grid) WorldToPixel(p geom.Point) (float64, float64) {
	return (p.X - g.WorldBounds.MinX) * g.Scale,
		(g.WorldBounds.MaxY - p.Y) * g.Scale
}

func (g *Grid) PixelToWorld(x, y float64) geom.Point {
	return geom.Point{
		X: g.WorldBounds.MinX + x/g.Scale,
		Y: g.WorldBounds.MaxY - y/g.Scale,
	}
}

func (g *Grid) InGrid(x, y int) bool {
	return x >= 0 && x < g.Width && y >= 0 && y < g.Height
}

type Mask struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Data   []byte `json:"data"`
}

func NewMask(width, height int) Mask {
	return Mask{Width: width, Height: height, Data: make([]byte, width*height)}
}

func (m Mask) At(x, y int) byte {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return 0
	}
	return m.Data[y*m.Width+x]
}

// RasterizeBand scan-converts one band into a binary component mask.
func RasterizeBand(g *Grid, polygon []geom.Point) (Mask, *errs.Error) {
	mask := NewMask(g.Width, g.Height)
	if len(polygon) < 3 {
		return mask, errs.New(errs.CodeGrid, "polygon has fewer than three points").WithParam("grid")
	}
	px := make([]geom.Point, len(polygon))
	for i, p := range polygon {
		if !g.WorldBounds.Contains(p) {
			return mask, errs.New(errs.CodeGrid,
				"polygon point (%.3f,%.3f) is outside derived grid bounds", p.X, p.Y).WithParam("grid")
		}
		x, y := g.WorldToPixel(p)
		px[i] = geom.Point{X: x, Y: y}
	}
	edges := make([][4]float64, 0, len(px))
	for i := range px {
		a, b := px[i], px[(i+1)%len(px)]
		edges = append(edges, [4]float64{a.X, a.Y, b.X, b.Y})
	}
	for y := 0; y < g.Height; y++ {
		scan := float64(y) + 0.5
		var xs []float64
		for _, e := range edges {
			x0, y0, x1, y1 := e[0], e[1], e[2], e[3]
			if (y0 <= scan && y1 > scan) || (y1 <= scan && y0 > scan) {
				t := (scan - y0) / (y1 - y0)
				xs = append(xs, x0+(x1-x0)*t)
			}
		}
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		for i := 0; i+1 < len(xs); i += 2 {
			start := int(math.Ceil(xs[i] - 0.5))
			end := int(math.Floor(xs[i+1] - 0.5))
			if end < start {
				continue
			}
			if start < 0 {
				start = 0
			}
			if end >= g.Width {
				end = g.Width - 1
			}
			for x := start; x <= end; x++ {
				mask.Data[y*g.Width+x] = 1
			}
		}
	}
	return mask, nil
}

// Composite overlays binary component masks. The resulting grayscale value is
// deterministic and preserves overlap information without subpixel coverage.
func Composite(components []Mask) Mask {
	if len(components) == 0 {
		return Mask{}
	}
	first := components[0]
	counts := make([]int, first.Width*first.Height)
	// Components always share the glyph grid, but handle mismatches safely.
	width, height := first.Width, first.Height
	for _, m := range components {
		if m.Width != width || m.Height != height {
			continue
		}
		for i, v := range m.Data {
			if v != 0 {
				counts[i]++
			}
		}
	}
	out := NewMask(width, height)
	for i, n := range counts {
		if n == 0 {
			continue
		}
		out.Data[i] = byte(255 * n / (n + 1))
	}
	return out
}

type DensitySample struct {
	SegmentID    int          `json:"segmentId,omitempty"`
	ContourIndex int          `json:"contourIndex"`
	CurveIndex   int          `json:"curveIndex"`
	ArcLength    float64      `json:"arcLength"`
	PixelX       int          `json:"pixelX"`
	PixelY       int          `json:"pixelY"`
	DensityPPM   int          `json:"densityPpm"`
	Boundary     []geom.Point `json:"boundary"`
}

// ComputeDensities measures binary coverage in a local square around every
// flattened skeleton point using an integral image.
func ComputeDensities(
	g *Grid,
	mask Mask,
	skeletonPoints []skeleton.Point,
	boundary []stroke.BoundarySample,
	width float64,
	densityRange model.DensityRange,
) ([]DensitySample, *errs.Error) {
	if g == nil || mask.Width != g.Width || mask.Height != g.Height {
		return nil, errs.New(errs.CodeGrid, "mask dimensions do not match grid").WithParam("grid")
	}
	integral := make([]int, (g.Width+1)*(g.Height+1))
	for y := 0; y < g.Height; y++ {
		row := 0
		for x := 0; x < g.Width; x++ {
			if mask.Data[y*g.Width+x] != 0 {
				row++
			}
			integral[(y+1)*(g.Width+1)+(x+1)] = integral[y*(g.Width+1)+(x+1)] + row
		}
	}
	radius := int(math.Ceil(width * g.Scale / 2))
	if radius < 1 {
		radius = 1
	}
	out := make([]DensitySample, 0, len(skeletonPoints))
	for i, sp := range skeletonPoints {
		x, y := g.WorldToPixel(sp.Position)
		pixX := int(math.Floor(x))
		pixY := int(math.Floor(y))
		if !g.InGrid(pixX, pixY) {
			return nil, errs.New(errs.CodeGrid,
				"skeleton sample is outside grid at pixel (%d,%d)", pixX, pixY).WithParam("grid")
		}
		x0, y0 := pixX-radius, pixY-radius
		x1, y1 := pixX+radius+1, pixY+radius+1
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x1 > g.Width {
			x1 = g.Width
		}
		if y1 > g.Height {
			y1 = g.Height
		}
		covered := rectSum(integral, g.Width+1, x0, y0, x1, y1)
		total := (x1 - x0) * (y1 - y0)
		ppm := 0
		if total > 0 {
			ppm = covered * 1000 / total
		}
		if ppm < densityRange.MinPPM || ppm > densityRange.MaxPPM {
			return nil, errs.New(errs.CodeDens,
				"density %d ppm is outside declared range [%d,%d]",
				ppm, densityRange.MinPPM, densityRange.MaxPPM).WithParam("density")
		}
		sample := DensitySample{
			ContourIndex: sp.ContourIndex,
			CurveIndex:   sp.CurveIndex,
			ArcLength:    sp.ArcLength,
			PixelX:       pixX,
			PixelY:       pixY,
			DensityPPM:   ppm,
		}
		if i < len(boundary) {
			sample.Boundary = []geom.Point{boundary[i].Left, boundary[i].Right}
			sample.ArcLength = boundary[i].ArcLength
		}
		out = append(out, sample)
	}
	return out, nil
}

func rectSum(integral []int, stride, x0, y0, x1, y1 int) int {
	a := integral[y0*stride+x0]
	b := integral[y0*stride+x1]
	c := integral[y1*stride+x0]
	d := integral[y1*stride+x1]
	return d - b - c + a
}

// FirstDifference locates the first pixel difference in row-major order.
func FirstDifference(a, b Mask) (x, y int, found bool) {
	if a.Width != b.Width || a.Height != b.Height {
		return 0, 0, true
	}
	for i := range a.Data {
		if a.Data[i] != b.Data[i] {
			return i % a.Width, i / a.Width, true
		}
	}
	return 0, 0, false
}

// DensityDifference locates the first differing density sample.
func DensityDifference(a, b []DensitySample) (index int, found bool) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i].SegmentID != b[i].SegmentID || a[i].DensityPPM != b[i].DensityPPM || a[i].PixelX != b[i].PixelX ||
			a[i].PixelY != b[i].PixelY || a[i].ContourIndex != b[i].ContourIndex ||
			a[i].CurveIndex != b[i].CurveIndex || math.Abs(a[i].ArcLength-b[i].ArcLength) > 1e-9 ||
			!sameBoundary(a[i].Boundary, b[i].Boundary) {
			return i, true
		}
	}
	if len(a) != len(b) {
		return n, true
	}
	return 0, false
}

func sameBoundary(a, b []geom.Point) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i].X-b[i].X) > 1e-9 || math.Abs(a[i].Y-b[i].Y) > 1e-9 {
			return false
		}
	}
	return true
}
