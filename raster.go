package vectorcore

import (
	"fmt"
	"sort"
)

type Bitmap struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Pixels []byte `json:"-"`
}

func NewBitmap(width, height int) (*Bitmap, error) {
	if width < 1 || height < 1 {
		return nil, fmt.Errorf("invalid bitmap dimensions %dx%d", width, height)
	}
	n := width * height * 4
	if n < 0 {
		return nil, fmt.Errorf("bitmap dimensions overflow")
	}
	return &Bitmap{Width: width, Height: height, Pixels: make([]byte, n)}, nil
}

func (b *Bitmap) Clone() *Bitmap {
	if b == nil {
		return nil
	}
	return &Bitmap{Width: b.Width, Height: b.Height, Pixels: append([]byte(nil), b.Pixels...)}
}

type Coverage struct {
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Values []uint16 `json:"-"`
}

func (c Coverage) Clone() Coverage {
	return Coverage{Width: c.Width, Height: c.Height, Values: append([]uint16(nil), c.Values...)}
}

type Viewport struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bounds Bounds `json:"bounds"`
	Scale  Scale  `json:"scale"`
	Margin int    `json:"margin"`
}

func DeriveViewport(bounds Bounds, cfg Config) (Viewport, error) {
	if err := cfg.Validate(); err != nil {
		return Viewport{}, err
	}
	if bounds.Empty() {
		return Viewport{}, &Error{Code: CodeParam, Op: "viewport", Field: "bounds", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("empty geometry bounds")}
	}
	width, ok := checkedSub(bounds.MaxX, bounds.MinX)
	if !ok {
		return Viewport{}, &Error{Code: CodeOverflow, Op: "viewport", Field: "bounds.width", Keyframe: -1, TimeIndex: -1}
	}
	height, ok := checkedSub(bounds.MaxY, bounds.MinY)
	if !ok {
		return Viewport{}, &Error{Code: CodeOverflow, Op: "viewport", Field: "bounds.height", Keyframe: -1, TimeIndex: -1}
	}
	scaledW, err := MulDivRound(width, cfg.Scale.Num, cfg.Scale.Den, RoundCeil)
	if err != nil {
		return Viewport{}, &Error{Code: CodeOverflow, Op: "viewport", Field: "scale", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	scaledH, err := MulDivRound(height, cfg.Scale.Num, cfg.Scale.Den, RoundCeil)
	if err != nil {
		return Viewport{}, &Error{Code: CodeOverflow, Op: "viewport", Field: "scale", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	w := int(ceilDiv(scaledW, CoordScale)) + 2*cfg.Margin
	h := int(ceilDiv(scaledH, CoordScale)) + 2*cfg.Margin
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if w > 32768 || h > 32768 {
		return Viewport{}, &Error{Code: CodeLimit, Op: "viewport", Field: "dimensions", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("bitmap %dx%d exceeds dimension cap", w, h)}
	}
	return Viewport{Width: w, Height: h, Bounds: bounds, Scale: cfg.Scale, Margin: cfg.Margin}, nil
}

func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

type scanEdge struct {
	x0, y0, x1, y1 int64
	minY, maxY     int64
}

type crossing struct {
	x       int64
	winding int
}

// RasterizeCoverage performs horizontal supersampling. For every subrow the
// polygon crossings form winding intervals; subcolumn centers count coverage.
func RasterizeCoverage(rings []Polyline, vp Viewport, cfg Config) (Coverage, error) {
	if err := cfg.Validate(); err != nil {
		return Coverage{}, err
	}
	if err := checkPixelBuffer(vp.Width, vp.Height, 2, cfg.MaxPixelBufferBytes); err != nil {
		return Coverage{}, err
	}
	cov := Coverage{Width: vp.Width, Height: vp.Height, Values: make([]uint16, vp.Width*vp.Height)}
	var edges []scanEdge
	for _, ring := range rings {
		pts := ring.Points
		if len(pts) > 1 && pts[0] == pts[len(pts)-1] {
			pts = pts[:len(pts)-1]
		}
		if len(pts) < 2 {
			continue
		}
		area := signedPolygonArea(pts)
		if area < 0 && cfg.FillRule == FillNonZero {
			for i, j := 0, len(pts)-1; i < j; i, j = i+1, j-1 {
				pts[i], pts[j] = pts[j], pts[i]
			}
		}
		for i := range pts {
			j := (i + 1) % len(pts)
			if pts[i].Y == pts[j].Y {
				continue
			}
			e := scanEdge{x0: pts[i].X, y0: pts[i].Y, x1: pts[j].X, y1: pts[j].Y}
			if e.y0 < e.y1 {
				e.minY, e.maxY = e.y0, e.y1
			} else {
				e.minY, e.maxY = e.y1, e.y0
			}
			edges = append(edges, e)
		}
	}
	if len(edges) == 0 {
		return cov, nil
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].minY < edges[j].minY })
	active := make([]scanEdge, 0, 128)
	nextEdge := 0
	var counts []uint16
	for py := 0; py < vp.Height; py++ {
		for sy := 0; sy < cfg.Grid; sy++ {
			y := sampleWorld(vp, py, sy, cfg.Grid, true)
			for nextEdge < len(edges) && edges[nextEdge].minY <= y {
				active = append(active, edges[nextEdge])
				nextEdge++
			}
			write := 0
			for _, e := range active {
				if e.maxY > y {
					active[write] = e
					write++
				}
			}
			active = active[:write]
			crossings := crossingsAt(active, y)
			intervals := windingIntervals(crossings, cfg.FillRule)
			if len(intervals) == 0 {
				continue
			}
			if counts == nil {
				counts = make([]uint16, vp.Width*vp.Height)
			}
			for px := 0; px < vp.Width; px++ {
				for sx := 0; sx < cfg.Grid; sx++ {
					x := sampleWorld(vp, px, sx, cfg.Grid, false)
					for _, iv := range intervals {
						if x >= iv[0] && x < iv[1] {
							counts[py*vp.Width+px]++
							break
						}
					}
				}
			}
		}
	}
	total := uint32(cfg.Grid * cfg.Grid)
	for i, count := range counts {
		cov.Values[i] = uint16((uint32(count)*65535 + total/2) / total)
	}
	return cov, nil
}

func sampleWorld(vp Viewport, pixel, sub, grid int, vertical bool) int64 {
	num := int64(2*((pixel-vp.Margin)*grid+sub)+1) * vp.Scale.Den * CoordScale
	den := int64(2*grid) * vp.Scale.Num
	offset := divRoundInt(num, den)
	if vertical {
		return vp.Bounds.MinY + offset
	}
	return vp.Bounds.MinX + offset
}

func crossingsAt(edges []scanEdge, y int64) []crossing {
	out := make([]crossing, 0, 8)
	for _, e := range edges {
		if e.y0 <= y && y < e.y1 {
			num := (e.x1 - e.x0) * (y - e.y0)
			den := e.y1 - e.y0
			out = append(out, crossing{x: e.x0 + divRoundInt(num, den), winding: 1})
		} else if e.y1 <= y && y < e.y0 {
			num := (e.x1 - e.x0) * (y - e.y0)
			den := e.y1 - e.y0
			out = append(out, crossing{x: e.x0 + divRoundInt(num, den), winding: -1})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].x == out[j].x {
			return out[i].winding < out[j].winding
		}
		return out[i].x < out[j].x
	})
	return out
}

func windingIntervals(crossings []crossing, rule FillRule) [][2]int64 {
	var intervals [][2]int64
	winding := 0
	for i := 0; i < len(crossings); {
		x := crossings[i].x
		delta := 0
		for i < len(crossings) && crossings[i].x == x {
			delta += crossings[i].winding
			i++
		}
		next := winding + delta
		insideBefore := winding != 0
		insideAfter := next != 0
		if rule == FillEvenOdd {
			insideBefore = winding&1 != 0
			insideAfter = next&1 != 0
		}
		if !insideBefore && insideAfter {
			intervals = append(intervals, [2]int64{x, x})
		} else if insideBefore && !insideAfter {
			if len(intervals) > 0 {
				intervals[len(intervals)-1][1] = x
			}
		}
		winding = next
	}
	if len(intervals) > 0 && intervals[len(intervals)-1][1] == intervals[len(intervals)-1][0] {
		intervals = intervals[:len(intervals)-1]
	}
	return intervals
}

func checkPixelBuffer(width, height int, channels int, limit int64) error {
	bytes := int64(width) * int64(height) * int64(channels)
	if bytes <= 0 || bytes > limit {
		return &Error{Code: CodeLimit, Op: "buffer", Field: "maxPixelBufferBytes", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("buffer %d bytes exceeds limit %d", bytes, limit)}
	}
	return nil
}
