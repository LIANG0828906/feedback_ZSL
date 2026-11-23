// Package stroke builds simple, closed ribbon polygons around flattened glyph
// skeletons. Spacing is expressed as a fraction of stroke width and is further
// tightened on high-curvature samples.
package stroke

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
)

type JoinStyle string
type TipStyle string

const (
	JoinRound JoinStyle = "round"
	JoinBevel JoinStyle = "bevel"
	JoinMiter JoinStyle = "miter"

	TipRound  TipStyle = "round"
	TipFlat   TipStyle = "flat"
	TipSquare TipStyle = "square"
)

type Params struct {
	Width           float64   `json:"width"`
	Spacing         float64   `json:"spacing"`
	Join            JoinStyle `json:"join"`
	Tip             TipStyle  `json:"tip"`
	CurvatureFactor float64   `json:"curvatureFactor"`
}

func DefaultParams() Params {
	return Params{
		Width:           28,
		Spacing:         0.55,
		Join:            JoinRound,
		Tip:             TipRound,
		CurvatureFactor: 0.18,
	}
}

func (p Params) Validate() *errs.Error {
	if p.Width <= 0 || math.IsNaN(p.Width) || math.IsInf(p.Width, 0) {
		return errs.New(errs.CodeParam, "width must be finite and greater than zero").WithParam("width")
	}
	if p.Spacing <= 0 || math.IsNaN(p.Spacing) || math.IsInf(p.Spacing, 0) {
		return errs.New(errs.CodeParam, "spacing must be finite and greater than zero").WithParam("spacing")
	}
	switch p.Join {
	case JoinRound, JoinBevel, JoinMiter:
	default:
		return errs.New(errs.CodeParam, "join must be round, bevel or miter").WithParam("join")
	}
	switch p.Tip {
	case TipRound, TipFlat, TipSquare:
	default:
		return errs.New(errs.CodeParam, "tip must be round, flat or square").WithParam("tip")
	}
	if p.CurvatureFactor < 0 || math.IsNaN(p.CurvatureFactor) || math.IsInf(p.CurvatureFactor, 0) {
		return errs.New(errs.CodeParam, "curvatureFactor must be finite and non-negative").WithParam("curvatureFactor")
	}
	return nil
}

func (p Params) Version() string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p Params) EffectiveSpacing(curvature float64) float64 {
	if p.Width <= 0 || p.Spacing <= 0 {
		return 0
	}
	curvature = math.Max(0, curvature)
	factor := 1 / (1 + p.CurvatureFactor*curvature*p.Width)
	factor = math.Max(0.18, math.Min(1, factor))
	spacing := p.Spacing * p.Width * factor
	if spacing < 0.35 {
		spacing = 0.35
	}
	// Very wide strokes do not benefit from ever-coarser centerline sampling;
	// preserving a fine transverse resolution also keeps area monotonic.
	if spacing > 18 {
		spacing = 18
	}
	return spacing
}

// ChangedParams returns stable names for parameters that differ.
func ChangedParams(a, b Params) []string {
	var out []string
	if a.Width != b.Width {
		out = append(out, "width")
	}
	if a.Spacing != b.Spacing {
		out = append(out, "spacing")
	}
	if a.Join != b.Join {
		out = append(out, "join")
	}
	if a.Tip != b.Tip {
		out = append(out, "tip")
	}
	if a.CurvatureFactor != b.CurvatureFactor {
		out = append(out, "curvatureFactor")
	}
	return out
}

type BoundarySample struct {
	ArcLength float64    `json:"arcLength"`
	Center    geom.Point `json:"center"`
	Left      geom.Point `json:"left"`
	Right     geom.Point `json:"right"`
}

type Band struct {
	Polygon          []geom.Point     `json:"polygon"`
	Area             float64          `json:"area"`
	Bounds           geom.Bounds      `json:"bounds"`
	EffectiveSpacing float64          `json:"effectiveSpacing"`
	Curvature        float64          `json:"curvature"`
	Centerline       []geom.Point     `json:"centerline"`
	BoundarySamples  []BoundarySample `json:"boundarySamples"`
}

// Build constructs one closed stroke band around an open polyline. glyphBounds
// is the truth bounds; the returned geometry must remain in glyphBounds
// expanded by the stroke allowance.
func Build(points []geom.Point, params Params, glyphBounds geom.Bounds, maxPoints int) (*Band, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	points = cleanPolyline(points)
	if len(points) < 2 {
		return nil, errs.New(errs.CodeZero, "stroke skeleton has fewer than two distinct points").WithParam("arcLength")
	}
	if self, at := geom.PolylineSelfIntersects(points); self {
		return nil, errs.New(errs.CodeSelf,
			"stroke skeleton self-intersects near (%.3f,%.3f)", at.X, at.Y).WithParam("controlPoints")
	}
	length := geom.PolylineLength(points)
	if length <= 1e-7 {
		return nil, errs.New(errs.CodeZero, "stroke skeleton has zero length").WithParam("arcLength")
	}
	curvature := MaxCurvature(points)
	step := params.EffectiveSpacing(curvature)
	if step <= 0 {
		return nil, errs.New(errs.CodeParam, "effective spacing is not positive").WithParam("spacing")
	}
	if maxPoints <= 0 {
		maxPoints = 200000
	}
	if int(math.Ceil(length/step))+2 > maxPoints {
		return nil, errs.New(errs.CodeParam,
			"polygon buffer limit %d would be exceeded (need at least %d points)",
			maxPoints, int(math.Ceil(length/step))+2).WithParam("maxPolygonPoints")
	}
	center := geom.ResamplePolyline(points, step)
	if len(center) < 2 {
		return nil, errs.New(errs.CodeZero, "resampled stroke skeleton is degenerate").WithParam("arcLength")
	}
	half := params.Width / 2
	tangents := tangentsAt(center)
	normals := make([]geom.Point, len(center))
	for i, t := range tangents {
		normals[i] = t.PerpLeft()
	}
	left := offsetSide(center, tangents, normals, half, 1, params.Join)
	right := offsetSide(center, tangents, normals, half, -1, params.Join)
	if len(left) < 2 || len(right) < 2 {
		return nil, errs.New(errs.CodeSelf, "offset boundary collapsed").WithParam("width")
	}

	startTangent := tangents[0]
	endTangent := tangents[len(tangents)-1]
	startCenter := center[0]
	endCenter := center[len(center)-1]

	var poly []geom.Point
	poly = append(poly, left...)
	poly = append(poly, capPoints(endCenter, endTangent, half, params.Tip)...)
	for i := len(right) - 1; i >= 0; i-- {
		poly = append(poly, right[i])
	}
	poly = append(poly, capPoints(startCenter, startTangent.Mul(-1), half, params.Tip)...)
	poly = cleanPolygon(poly)
	if len(poly) < 3 {
		return nil, errs.New(errs.CodeSelf, "stroke band has fewer than three boundary points").WithParam("width")
	}
	if len(poly) > maxPoints {
		return nil, errs.New(errs.CodeParam,
			"polygon buffer limit %d exceeded after joins/caps (%d points)", maxPoints, len(poly)).
			WithParam("maxPolygonPoints")
	}
	if self, at := geom.PolygonSelfIntersects(poly); self {
		return nil, errs.New(errs.CodeSelf,
			"stroke band self-intersects near (%.3f,%.3f)", at.X, at.Y).WithParam("width")
	}
	allowed := glyphBounds.Expand(half*1.35 + 3)
	bandBounds := geom.BoundsOf(poly)
	if !allowed.ContainsBounds(bandBounds) {
		return nil, errs.New(errs.CodeSelf,
			"stroke band bounds (%.3f,%.3f)-(%.3f,%.3f) exceed allowed expanded glyph bounds",
			bandBounds.MinX, bandBounds.MinY, bandBounds.MaxX, bandBounds.MaxY).WithParam("width")
	}
	area := geom.Area(poly)
	if area <= 1e-9 {
		return nil, errs.New(errs.CodeSelf, "stroke band has non-positive area").WithParam("width")
	}
	band := &Band{
		Polygon:          poly,
		Area:             area,
		Bounds:           bandBounds,
		EffectiveSpacing: step,
		Curvature:        curvature,
		Centerline:       center,
	}
	band.BoundarySamples = boundarySamples(points, tangentsAt(points), half)
	return band, nil
}

// MaxCurvature estimates the largest discrete curvature on a polyline.
func MaxCurvature(points []geom.Point) float64 {
	if len(points) < 3 {
		return 0
	}
	maxCurve := 0.0
	for i := 1; i < len(points)-1; i++ {
		a := points[i].Sub(points[i-1])
		b := points[i+1].Sub(points[i])
		la, lb := a.Len(), b.Len()
		if la < 1e-9 || lb < 1e-9 {
			continue
		}
		cross := math.Abs(a.Cross(b)) / (la * lb)
		cross = math.Max(-1, math.Min(1, cross))
		angle := math.Asin(cross)
		k := angle / ((la + lb) / 2)
		if k > maxCurve {
			maxCurve = k
		}
	}
	return maxCurve
}

func cleanPolyline(points []geom.Point) []geom.Point {
	out := make([]geom.Point, 0, len(points))
	for _, p := range points {
		if len(out) == 0 || out[len(out)-1].Distance(p) > 1e-9 {
			out = append(out, p)
		}
	}
	return out
}

func cleanPolygon(points []geom.Point) []geom.Point {
	out := make([]geom.Point, 0, len(points))
	for _, p := range points {
		if len(out) == 0 || out[len(out)-1].Distance(p) > 1e-8 {
			out = append(out, p)
		}
	}
	for len(out) > 1 && out[0].Distance(out[len(out)-1]) < 1e-8 {
		out = out[:len(out)-1]
	}
	return out
}

func tangentsAt(points []geom.Point) []geom.Point {
	tangents := make([]geom.Point, len(points))
	for i := range points {
		var t geom.Point
		switch {
		case i == 0:
			t = points[1].Sub(points[0])
		case i == len(points)-1:
			t = points[i].Sub(points[i-1])
		default:
			in := points[i].Sub(points[i-1]).Normalize()
			out := points[i+1].Sub(points[i]).Normalize()
			t = in.Add(out).Normalize()
			if t.Len() < 0.5 {
				t = out
			}
		}
		tangents[i] = t.Normalize()
	}
	return tangents
}

func offsetSide(points, tangents, normals []geom.Point, half float64, side float64, join JoinStyle) []geom.Point {
	out := make([]geom.Point, 0, len(points)+16)
	for i := range points {
		if i == 0 || i == len(points)-1 {
			out = append(out, points[i].Add(normals[i].Mul(side*half)))
			continue
		}
		prevN := normals[i-1].Mul(side)
		nextN := normals[i].Mul(side)
		turn := tangents[i-1].Cross(tangents[i])
		outer := side*turn < -1e-9
		bisector := prevN.Add(nextN)
		if bisector.Len() < 1e-9 {
			bisector = prevN
		}
		innerPoint := points[i].Add(bisector.Normalize().Mul(half))
		switch join {
		case JoinMiter:
			if !outer {
				out = append(out, innerPoint)
				continue
			}
			m := prevN.Add(nextN)
			if m.Len() < 1e-6 {
				out = append(out, points[i].Add(prevN.Mul(half)), points[i].Add(nextN.Mul(half)))
				continue
			}
			m = m.Normalize()
			denom := math.Abs(m.Dot(prevN))
			scale := half
			if denom > 1e-4 {
				scale = half / denom
			}
			if scale > half*1.25 {
				scale = half * 1.25
			}
			out = append(out, points[i].Add(m.Mul(scale)))
		case JoinBevel:
			if !outer {
				out = append(out, innerPoint)
				continue
			}
			if prevN.Distance(nextN) > 1e-7 {
				out = append(out, points[i].Add(prevN.Mul(half)), points[i].Add(nextN.Mul(half)))
			} else {
				out = append(out, innerPoint)
			}
		case JoinRound:
			if outer {
				out = append(out, arcBetween(points[i].Add(prevN.Mul(half)), points[i], half, prevN, nextN, 6)...)
			} else {
				out = append(out, innerPoint)
			}
		}
	}
	return out
}

func capPoints(center, tangent geom.Point, half float64, tip TipStyle) []geom.Point {
	normal := tangent.PerpLeft()
	switch tip {
	case TipFlat:
		return nil
	case TipRound:
		start := math.Atan2(normal.Y, normal.X)
		out := make([]geom.Point, 0, 10)
		for i := 1; i <= 10; i++ {
			angle := start - math.Pi*float64(i)/10
			out = append(out, center.Add(geom.Point{X: math.Cos(angle), Y: math.Sin(angle)}.Mul(half)))
		}
		return out
	case TipSquare:
		ext := tangent.Mul(half)
		return []geom.Point{
			center.Add(normal.Mul(half)).Add(ext),
			center.Sub(normal.Mul(half)).Add(ext),
		}
	}
	return nil
}

func arcBetween(from, center geom.Point, radius float64, a, b geom.Point, segments int) []geom.Point {
	if segments < 2 {
		segments = 2
	}
	a0 := math.Atan2(a.Y, a.X)
	delta := math.Atan2(a.Cross(b), a.Dot(b))
	out := make([]geom.Point, 0, segments)
	for i := 1; i <= segments; i++ {
		angle := a0 + delta*float64(i)/float64(segments)
		out = append(out, center.Add(geom.Point{X: math.Cos(angle), Y: math.Sin(angle)}.Mul(radius)))
	}
	return out
}

func boundarySamples(points, tangents []geom.Point, half float64) []BoundarySample {
	out := make([]BoundarySample, 0, len(points))
	var arc float64
	for i, p := range points {
		if i > 0 {
			arc += p.Distance(points[i-1])
		}
		n := tangents[i].PerpLeft()
		out = append(out, BoundarySample{
			ArcLength: arc,
			Center:    p,
			Left:      p.Add(n.Mul(half)),
			Right:     p.Sub(n.Mul(half)),
		})
	}
	return out
}

// ValidateBoundary re-checks an externally supplied polygon.
func ValidateBoundary(poly []geom.Point, glyphBounds geom.Bounds, halfWidth float64) error {
	if self, at := geom.PolygonSelfIntersects(poly); self {
		return errs.New(errs.CodeSelf, "stroke band self-intersects near (%.3f,%.3f)", at.X, at.Y).WithParam("width")
	}
	allowed := glyphBounds.Expand(halfWidth*1.35 + 3)
	if !allowed.ContainsBounds(geom.BoundsOf(poly)) {
		return errs.New(errs.CodeSelf, "stroke band exceeds expanded glyph bounds").WithParam("width")
	}
	return nil
}

func DebugString(b *Band) string {
	return fmt.Sprintf("band(points=%d area=%.3f spacing=%.3f curvature=%.5f)", len(b.Polygon), b.Area, b.EffectiveSpacing, b.Curvature)
}
