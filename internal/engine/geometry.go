package engine

import (
	"math"
	"math/big"
	"strconv"

	"acousticfield/internal/acoustic"
)

type fpoint struct {
	x float64
	y float64
}

func toF(p acoustic.Point) fpoint { return fpoint{float64(p.X), float64(p.Y)} }

func quantize(p fpoint) acoustic.Point {
	return acoustic.Point{X: int64(math.Round(p.x)), Y: int64(math.Round(p.y))}
}

func normalF(s acoustic.Surface) fpoint {
	return fpoint{
		x: float64(s.Normal.X) / float64(acoustic.NormalScale),
		y: float64(s.Normal.Y) / float64(acoustic.NormalScale),
	}
}

func lineC(s acoustic.Surface, n fpoint) float64 {
	return n.x*float64(s.A.X) + n.y*float64(s.A.Y)
}

func reflectPoint(p fpoint, s acoustic.Surface) fpoint {
	n := normalF(s)
	c := lineC(s, n)
	d := n.x*p.x + n.y*p.y - c
	return fpoint{x: p.x - 2*d*n.x, y: p.y - 2*d*n.y}
}

func reflectVector(d fpoint, s acoustic.Surface) fpoint {
	n := normalF(s)
	dot := d.x*n.x + d.y*n.y
	return fpoint{x: d.x - 2*dot*n.x, y: d.y - 2*dot*n.y}
}

func cross(a, b fpoint) float64 { return a.x*b.y - a.y*b.x }
func sub(a, b fpoint) fpoint    { return fpoint{x: a.x - b.x, y: a.y - b.y} }
func add(a, b fpoint) fpoint    { return fpoint{x: a.x + b.x, y: a.y + b.y} }
func scale(a fpoint, t float64) fpoint {
	return fpoint{x: a.x * t, y: a.y * t}
}

// raySegment intersects O+tD with surface segment A+u(B-A).
func raySegment(o, d fpoint, s acoustic.Surface) (fpoint, float64, float64, bool) {
	a := toF(s.A)
	b := toF(s.B)
	seg := sub(b, a)
	den := cross(d, seg)
	if math.Abs(den) < 1e-12 {
		return fpoint{}, 0, 0, false
	}
	ap := sub(a, o)
	t := cross(ap, seg) / den
	u := cross(ap, d) / den
	const eps = 1e-8
	if t <= eps || u < -eps || u > 1+eps {
		return fpoint{}, t, u, false
	}
	return add(o, scale(d, t)), t, u, true
}

func segmentBlocked(p0, p1 fpoint, surfaces []acoustic.Surface, allowedAtStart, allowedAtEnd map[string]bool) bool {
	d := sub(p1, p0)
	length := math.Hypot(d.x, d.y)
	if length < 1e-9 {
		return false
	}
	for _, s := range surfaces {
		hit, t, _, ok := raySegment(p0, d, s)
		if !ok {
			continue
		}
		// raySegment's t is relative to d=p1-p0, so it is segment parameter.
		const endpointEps = 1e-7
		if t <= endpointEps {
			if allowedAtStart != nil && allowedAtStart[s.ID] {
				continue
			}
		}
		if t >= 1-endpointEps {
			if allowedAtEnd != nil && allowedAtEnd[s.ID] {
				continue
			}
		}
		if t > endpointEps && t < 1-endpointEps {
			_ = hit
			return true
		}
	}
	return false
}

func fixedLength(a, b acoustic.Point) (length int64, residual int64) {
	dx := big.NewInt(0).Sub(big.NewInt(b.X), big.NewInt(a.X))
	dy := big.NewInt(0).Sub(big.NewInt(b.Y), big.NewInt(a.Y))
	xx := new(big.Int).Mul(dx, dx)
	yy := new(big.Int).Mul(dy, dy)
	sq := new(big.Int).Add(xx, yy)
	root := new(big.Int).Sqrt(sq)
	rootSq := new(big.Int).Mul(root, root)
	res := new(big.Int).Sub(sq, rootSq)
	return root.Int64(), res.Int64()
}

func pointsToLength(pathID, location string, pts []acoustic.Point) (int64, []int64, []acoustic.QuantRecord) {
	var total int64
	segments := make([]int64, 0, len(pts)-1)
	records := make([]acoustic.QuantRecord, 0, len(pts)-1)
	for i := 0; i+1 < len(pts); i++ {
		l, residual := fixedLength(pts[i], pts[i+1])
		segments = append(segments, l)
		total += l
		dx := pts[i+1].X - pts[i].X
		dy := pts[i+1].Y - pts[i].Y
		records = append(records, acoustic.QuantRecord{
			PathID:     pathID,
			Stage:      "length_sqrt",
			Location:   location + ":segment:" + strconv.Itoa(i),
			InputQ:     dx*dx + dy*dy,
			TruncatedQ: l,
			ResidualQ:  residual,
		})
	}
	return total, segments, records
}
