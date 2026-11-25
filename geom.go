package iconcut

import (
	"math"
)

type Mapper struct {
	G         Geometry
	Scale     int64
	paramHash string
}

func NewMapper(g Geometry) (*Mapper, error) {
	if err := ValidateGeometry(g); err != nil {
		return nil, err
	}
	scale := g.CoordScale
	if scale <= 0 {
		scale = 16
	}
	return &Mapper{G: g, Scale: int64(scale), paramHash: HashJSON(g)}, nil
}

func ValidateGeometry(g Geometry) error {
	h := g.Homography
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if !finite(h[r][c]) {
				return NewError(EGeom, "geometry.homography", "entry [%d][%d] must be finite", r, c)
			}
		}
	}
	det := h[0][0]*(h[1][1]*h[2][2]-h[1][2]*h[2][1]) -
		h[0][1]*(h[1][0]*h[2][2]-h[1][2]*h[2][0]) +
		h[0][2]*(h[1][0]*h[2][1]-h[1][1]*h[2][0])
	if math.Abs(det) < 1e-12 {
		return WithDetails(NewError(EGeom, "geometry.homography", "determinant must be non-zero"), map[string]any{
			"determinant": det, "condition": "abs(det) >= 1e-12",
		})
	}
	r := g.Radial
	for name, v := range map[string]float64{
		"center_x": r.CenterX, "center_y": r.CenterY, "radius": r.Radius,
		"k1": r.K1, "k2": r.K2,
	} {
		if !finite(v) {
			return NewError(EGeom, "geometry.radial."+name, "value must be finite")
		}
	}
	if !(r.Radius > 0) {
		return WithDetails(NewError(EGeom, "geometry.radial.radius", "radius must be positive"), map[string]any{
			"radius": r.Radius, "condition": "radius > 0",
		})
	}
	if g.CoordScale < 0 {
		return NewError(EGeom, "geometry.coord_scale", "coord_scale must be positive")
	}
	w, hh := g.Corrected.W, g.Corrected.H
	if w > 0 && hh > 0 {
		for _, p := range []Point{{0, 0}, {float64(w - 1), 0}, {0, float64(hh - 1)}, {float64(w - 1), float64(hh - 1)}, {float64(w) / 2, float64(hh) / 2}} {
			u, denom := applyHomography(h, p)
			if math.Abs(denom) < 1e-9 {
				return WithDetails(NewError(EGeom, "geometry.homography", "projective denominator degenerates in corrected canvas"), map[string]any{
					"point": p, "denominator": denom, "condition": "abs(w) >= 1e-9",
				})
			}
			s := applyRadial(u, r)
			if !finite(s.X) || !finite(s.Y) {
				return NewError(EGeom, "geometry.radial", "mapping produces non-finite source coordinate")
			}
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func applyHomography(h [3][3]float64, p Point) (Point, float64) {
	w := h[2][0]*p.X + h[2][1]*p.Y + h[2][2]
	return Point{
		X: (h[0][0]*p.X + h[0][1]*p.Y + h[0][2]) / w,
		Y: (h[1][0]*p.X + h[1][1]*p.Y + h[1][2]) / w,
	}, w
}

func applyRadial(p Point, r RadialParams) Point {
	dx := p.X - r.CenterX
	dy := p.Y - r.CenterY
	r2 := (dx*dx + dy*dy) / (r.Radius * r.Radius)
	factor := 1 + r.K1*r2 + r.K2*r2*r2
	return Point{X: r.CenterX + dx*factor, Y: r.CenterY + dy*factor}
}

func (m *Mapper) MapPoint(p Point) Point {
	u, _ := applyHomography(m.G.Homography, p)
	return applyRadial(u, m.G.Radial)
}

func (m *Mapper) Version() string { return m.paramHash }

func invert3(a [3][3]float64) [3][3]float64 {
	d := a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) -
		a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) +
		a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
	return [3][3]float64{
		{(a[1][1]*a[2][2] - a[1][2]*a[2][1]) / d, (a[0][2]*a[2][1] - a[0][1]*a[2][2]) / d, (a[0][1]*a[1][2] - a[0][2]*a[1][1]) / d},
		{(a[1][2]*a[2][0] - a[1][0]*a[2][2]) / d, (a[0][0]*a[2][2] - a[0][2]*a[2][0]) / d, (a[0][2]*a[1][0] - a[0][0]*a[1][2]) / d},
		{(a[1][0]*a[2][1] - a[1][1]*a[2][0]) / d, (a[0][1]*a[2][0] - a[0][0]*a[2][1]) / d, (a[0][0]*a[1][1] - a[0][1]*a[1][0]) / d},
	}
}

func applyMatrix(h [3][3]float64, p Point) Point {
	w := h[2][0]*p.X + h[2][1]*p.Y + h[2][2]
	return Point{X: (h[0][0]*p.X + h[0][1]*p.Y + h[0][2]) / w, Y: (h[1][0]*p.X + h[1][1]*p.Y + h[1][2]) / w}
}

func invertRadial(r RadialParams, q Point) Point {
	dx := q.X - r.CenterX
	dy := q.Y - r.CenterY
	rd := math.Hypot(dx, dy)
	if rd < 1e-12 {
		return q
	}
	ru := rd
	lo, hi := 0.0, math.Max(r.Radius*2, rd*2+1)
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		r2 := mid * mid / (r.Radius * r.Radius)
		mapped := mid * (1 + r.K1*r2 + r.K2*r2*r2)
		if mapped < rd {
			lo = mid
		} else {
			hi = mid
		}
	}
	ru = (lo + hi) / 2
	scale := ru / rd
	return Point{X: r.CenterX + dx*scale, Y: r.CenterY + dy*scale}
}

func InverseMapPoint(g Geometry, p Point) Point {
	u := invertRadial(g.Radial, p)
	inv := invert3(g.Homography)
	return applyMatrix(inv, u)
}

type MapEntry struct {
	X          int     `json:"x"`
	Y          int     `json:"y"`
	SX         int     `json:"sx"`
	SY         int     `json:"sy"`
	SXQ        int64   `json:"sx_q"`
	SYQ        int64   `json:"sy_q"`
	QuantX     float64 `json:"quant_x"`
	QuantY     float64 `json:"quant_y"`
	ResidualPX float64 `json:"residual_px"`
}

func (m *Mapper) Quantize(v float64) int64 {
	return int64(math.Round(v * float64(m.Scale)))
}

func (m *Mapper) BuildEntry(x, y int) MapEntry {
	s := m.MapPoint(Point{X: float64(x), Y: float64(y)})
	qx := m.Quantize(s.X)
	qy := m.Quantize(s.Y)
	qxf := float64(qx) / float64(m.Scale)
	qyf := float64(qy) / float64(m.Scale)
	rx := s.X - qxf
	ry := s.Y - qyf
	return MapEntry{
		X: x, Y: y,
		SX: int(math.Floor(qxf)), SY: int(math.Floor(qyf)),
		SXQ: qx, SYQ: qy,
		QuantX: qxf, QuantY: qyf,
		ResidualPX: math.Hypot(rx, ry),
	}
}

func (m *Mapper) BuildMap(r Rect) []MapEntry {
	out := make([]MapEntry, 0, r.W*r.H)
	for y := r.Y; y < r.MaxY(); y++ {
		for x := r.X; x < r.MaxX(); x++ {
			out = append(out, m.BuildEntry(x, y))
		}
	}
	return out
}

type Resampler struct {
	Src   *Image
	M     *Mapper
	Order int
}

func NewResampler(src *Image, m *Mapper, order int) (*Resampler, error) {
	if !src.Valid() {
		return nil, NewError(EDecode, "source", "source image is empty or channel stride is invalid")
	}
	if order < 0 || order > 2 {
		return nil, WithDetails(NewError(EPara, "declared.interp_order", "interpolation order must be in [0,2]"), map[string]any{
			"min": 0, "max": 2, "actual": order,
		})
	}
	return &Resampler{Src: src, M: m, Order: order}, nil
}

func interpolationRadius(order int) int {
	switch order {
	case 0:
		return 0
	case 1:
		return 1
	default:
		return 2
	}
}

func (r *Resampler) Sample(sx, sy float64) [3]byte {
	switch r.Order {
	case 0:
		return r.sampleNearest(sx, sy)
	case 1:
		return r.sampleBilinear(sx, sy)
	default:
		return r.sampleBicubic(sx, sy)
	}
}

func (r *Resampler) SampleEntry(e MapEntry) [3]byte {
	sx := float64(e.SXQ) / float64(r.M.Scale)
	sy := float64(e.SYQ) / float64(r.M.Scale)
	return r.Sample(sx, sy)
}

func (r *Resampler) colorAt(x, y int) [3]float64 {
	x = clampInt(x, 0, r.Src.W-1)
	y = clampInt(y, 0, r.Src.H-1)
	i := (y*r.Src.W + x) * 3
	return [3]float64{float64(r.Src.Pix[i]), float64(r.Src.Pix[i+1]), float64(r.Src.Pix[i+2])}
}

func (r *Resampler) sampleNearest(sx, sy float64) [3]byte {
	x := int(math.Floor(sx + 0.5))
	y := int(math.Floor(sy + 0.5))
	c := r.colorAt(x, y)
	return [3]byte{byte(clampFloat(math.Round(c[0]), 0, 255)), byte(clampFloat(math.Round(c[1]), 0, 255)), byte(clampFloat(math.Round(c[2]), 0, 255))}
}

func (r *Resampler) sampleBilinear(sx, sy float64) [3]byte {
	x0 := int(math.Floor(sx))
	y0 := int(math.Floor(sy))
	tx := sx - float64(x0)
	ty := sy - float64(y0)
	c00 := r.colorAt(x0, y0)
	c10 := r.colorAt(x0+1, y0)
	c01 := r.colorAt(x0, y0+1)
	c11 := r.colorAt(x0+1, y0+1)
	var out [3]byte
	for i := 0; i < 3; i++ {
		a := c00[i]*(1-tx) + c10[i]*tx
		b := c01[i]*(1-tx) + c11[i]*tx
		v := a*(1-ty) + b*ty
		out[i] = byte(clampFloat(math.Round(v), 0, 255))
	}
	return out
}

func cubicKernel(x float64) float64 {
	x = math.Abs(x)
	if x <= 1 {
		return 1.5*x*x*x - 2.5*x*x + 1
	}
	if x < 2 {
		return -0.5*x*x*x + 2.5*x*x - 4*x + 2
	}
	return 0
}

func (r *Resampler) sampleBicubic(sx, sy float64) [3]byte {
	x0 := int(math.Floor(sx))
	y0 := int(math.Floor(sy))
	var out [3]float64
	var ws [4]float64
	for j := -1; j <= 2; j++ {
		wy := cubicKernel(float64(y0+j) - sy)
		for i := -1; i <= 2; i++ {
			wx := cubicKernel(float64(x0+i) - sx)
			w := wx * wy
			ws[i+1] += w
			c := r.colorAt(x0+i, y0+j)
			for k := 0; k < 3; k++ {
				out[k] += c[k] * w
			}
		}
	}
	_ = ws
	return [3]byte{byte(clampFloat(math.Round(out[0]), 0, 255)), byte(clampFloat(math.Round(out[1]), 0, 255)), byte(clampFloat(math.Round(out[2]), 0, 255))}
}

func CorrectWhole(src *Image, g Geometry, order int) (*Image, []MapEntry, error) {
	m, err := NewMapper(g)
	if err != nil {
		return nil, nil, err
	}
	rs, err := NewResampler(src, m, order)
	if err != nil {
		return nil, nil, err
	}
	if g.Corrected.W <= 0 || g.Corrected.H <= 0 {
		return nil, nil, NewError(EEmpty, "geometry.corrected", "corrected canvas is empty")
	}
	out := NewImage(g.Corrected.W, g.Corrected.H, 3)
	entries := m.BuildMap(Rect{W: g.Corrected.W, H: g.Corrected.H})
	for _, e := range entries {
		out.SetPixel(e.X, e.Y, rs.SampleEntry(e))
	}
	return out, entries, nil
}

type ControlResidual struct {
	Index       int     `json:"index"`
	GeometryPX  float64 `json:"geometry_px"`
	QuantPX     float64 `json:"quant_px"`
	EffectivePX float64 `json:"effective_px"`
}

type ResidualStats struct {
	Order int     `json:"order"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
	RMS   float64 `json:"rms"`
}

func ControlResiduals(g Geometry, controls []ControlPoint, order int) ([]ControlResidual, ResidualStats, error) {
	m, err := NewMapper(g)
	if err != nil {
		return nil, ResidualStats{}, err
	}
	out := make([]ControlResidual, 0, len(controls))
	var sum, sq, max float64
	for i, c := range controls {
		exact := m.MapPoint(c.Corrected)
		geom := math.Hypot(exact.X-c.Source.X, exact.Y-c.Source.Y)
		q := m.Quantize(exact.X)
		_ = m.Quantize(exact.Y)
		quant := math.Abs(exact.X - float64(q)/float64(m.Scale))
		entry := m.BuildEntry(int(math.Round(c.Corrected.X)), int(math.Round(c.Corrected.Y)))
		effective := math.Max(geom, entry.ResidualPX)
		if order == 0 {
			nearest := Point{X: math.Round(exact.X), Y: math.Round(exact.Y)}
			effective = math.Hypot(nearest.X-c.Source.X, nearest.Y-c.Source.Y)
		}
		row := ControlResidual{Index: i, GeometryPX: geom, QuantPX: quant, EffectivePX: effective}
		out = append(out, row)
		sum += effective
		sq += effective * effective
		max = math.Max(max, effective)
	}
	n := float64(len(controls))
	if n == 0 {
		n = 1
	}
	stats := ResidualStats{Order: order, Max: max, Mean: sum / n, RMS: math.Sqrt(sq / n)}
	return out, stats, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
