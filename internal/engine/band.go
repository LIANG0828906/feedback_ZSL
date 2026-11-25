package engine

import (
	"math"

	"strokeforge/internal/model"
)

// StrokeBand is the generated band geometry plus the nib boundary samples
// requested by the diagnostics UI.
type StrokeBand struct {
	SegmentID int
	// Offset polygons for left/right sides, joined per join rule.
	Left    []model.FPoint
	Right   []model.FPoint
	Outline model.Polygon
	// Boundary points sampled along the band at every spacing interval.
	Boundary []model.FPoint
	// Nib stamps are retained for rasterization union coverage.
	Stamps []model.Polygon
}

// NibStamps implements raster.BandLike.
func (b StrokeBand) NibStamps() []model.Polygon { return b.Stamps }

// unitNormal returns a normalized perpendicular of vector a->b.
func unitNormal(a, b model.FPoint) (model.FPoint, model.FPoint) {
	dx := b.X - a.X
	dy := b.Y - a.Y
	l := math.Hypot(dx, dy)
	if l < epsilon {
		return model.FPoint{X: 0, Y: 0}, model.FPoint{X: 0, Y: 0}
	}
	nx, ny := -dy/l, dx/l
	return model.FPoint{X: nx, Y: ny}, model.FPoint{X: -nx, Y: -ny}
}

// Nib describes the pen tip shape. Currently "round" and "square" are
// supported, both expressed as convex polygons of nSides vertices.
type Nib struct {
	Shape string
	Sides int
}

func nibFor(cap string) Nib {
	if cap == "square" {
		return Nib{Shape: "square", Sides: 4}
	}
	return Nib{Shape: "round", Sides: 16}
}

// stampPolygon returns the nib polygon centered at p with the given half width.
func stampPolygon(p model.FPoint, halfWidth float64, nib Nib) model.Polygon {
	pts := make([]model.FPoint, nib.Sides)
	for i := 0; i < nib.Sides; i++ {
		ang := 2 * math.Pi * float64(i) / float64(nib.Sides)
		if nib.Shape == "square" {
			// Rotate the square 45 degrees so vertices sit on the axes of
			// the local frame, matching a rotated square pen.
			ang += math.Pi / 4
			c := math.Cos(ang)
			s := math.Sin(ang)
			// Inscribed square: scale so that side distance is halfWidth.
			r := halfWidth * math.Sqrt2
			pts[i] = model.FPoint{X: p.X + c*r, Y: p.Y + s*r}
		} else {
			pts[i] = model.FPoint{X: p.X + halfWidth*math.Cos(ang), Y: p.Y + halfWidth*math.Sin(ang)}
		}
	}
	return model.Polygon{Points: pts}
}

// buildOutline stitches left chain (forward) and right chain (reversed) into
// one closed outline polygon.
func buildOutline(left, right []model.FPoint) model.Polygon {
	n := len(left)
	out := make([]model.FPoint, 0, 2*n)
	out = append(out, left...)
	for i := n - 1; i >= 0; i-- {
		out = append(out, right[i])
	}
	return model.Polygon{Points: out}
}

// BuildStrokeBand generates the band for one open stroke segment polyline.
// The segment polyline is treated as an open chain (its caps are nib stamps).
func BuildStrokeBand(segID int, pts []model.FPoint, params model.StrokeParams) (StrokeBand, error) {
	if len(pts) < 2 {
		return StrokeBand{}, model.NewError(model.EZlen, "zero-length stroke segment").
			WithLocation("", segID)
	}
	if idx := zeroLengthOpenEdge(pts); idx >= 0 {
		return StrokeBand{}, model.NewError(model.EZlen, "zero-length edge inside stroke segment").
			WithLocation("", segID)
	}
	half := params.Width / 2
	nib := nibFor(params.Cap)

	// Offset chains for the open chain: duplicate first/last normals.
	left, right := offsetOpen(pts, half, params.Join)
	outline := buildOutline(left, right)

	// Nib stamps along the chain at spacing intervals, including both caps.
	var stamps []model.Polygon
	var boundary []model.FPoint
	total := 0.0
	for i := 0; i < len(pts)-1; i++ {
		edgeLen := dist(pts[i], pts[i+1])
		steps := int(math.Ceil(edgeLen / math.Max(epsilon, params.Spacing)))
		for k := 0; k < steps; k++ {
			f := float64(k) / float64(steps)
			p := model.FPoint{X: pts[i].X + (pts[i+1].X-pts[i].X)*f, Y: pts[i].Y + (pts[i+1].Y-pts[i].Y)*f}
			stamps = append(stamps, stampPolygon(p, half, nib))
			boundary = append(boundary, model.FPoint{X: p.X, Y: p.Y})
		}
		total += edgeLen
	}
	stamps = append(stamps, stampPolygon(pts[len(pts)-1], half, nib))
	boundary = append(boundary, pts[len(pts)-1])

	return StrokeBand{
		SegmentID: segID,
		Left:      left,
		Right:     right,
		Outline:   outline,
		Boundary:  boundary,
		Stamps:    stamps,
	}, nil
}

func openAsClosed(pts []model.FPoint) []model.FPoint {
	if len(pts) >= 2 && dist(pts[0], pts[len(pts)-1]) < epsilon {
		return pts
	}
	return append(append([]model.FPoint{}, pts...), pts[0])
}

// offsetOpen computes offset chains for an open polyline.
func offsetOpen(pts []model.FPoint, halfWidth float64, join string) (left, right []model.FPoint) {
	n := len(pts)
	left = make([]model.FPoint, n)
	right = make([]model.FPoint, n)
	for i := 0; i < n; i++ {
		var nx, ny float64
		if i == 0 {
			n, _ := unitNormal(pts[0], pts[1])
			nx, ny = n.X, n.Y
		} else if i == n-1 {
			n, _ := unitNormal(pts[n-2], pts[n-1])
			nx, ny = n.X, n.Y
		} else {
			n1, _ := unitNormal(pts[i-1], pts[i])
			n2, _ := unitNormal(pts[i], pts[i+1])
			nx = n1.X + n2.X
			ny = n1.Y + n2.Y
			norm := math.Hypot(nx, ny)
			if norm < epsilon {
				nx, ny = n2.X, n2.Y
			} else {
				nx, ny = nx/norm, ny/norm
			}
			if join == "miter" {
				dot := nx*n2.X + ny*n2.Y
				scale := 1.0
				if math.Abs(dot) > epsilon {
					scale = 1 / math.Max(0.25, math.Abs(dot))
				}
				nx, ny = nx*scale, ny*scale
			}
		}
		norm := math.Hypot(nx, ny)
		if norm < epsilon {
			left[i] = pts[i]
			right[i] = pts[i]
			continue
		}
		nx, ny = nx/norm, ny/norm
		left[i] = model.FPoint{X: pts[i].X + nx*halfWidth, Y: pts[i].Y + ny*halfWidth}
		right[i] = model.FPoint{X: pts[i].X - nx*halfWidth, Y: pts[i].Y - ny*halfWidth}
	}
	return left, right
}
