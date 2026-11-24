package engine

import (
	"fmt"
)

const (
	q15 = int64(1 << 15)
	q30 = int64(1 << 30)
)

// InterpolatePoint applies the configured integer interpolation to one control point.
func InterpolatePoint(a, b Vec2, numerator, denominator int64, opts InterpOptions) Vec2 {
	if denominator <= 0 || numerator <= 0 {
		return a
	}
	if numerator >= denominator {
		return b
	}
	precisionDen := int64(1) << opts.PrecisionBits
	q := divRound(numerator*precisionDen, denominator, RoundFloor)
	if q <= 0 {
		return a
	}
	if q >= precisionDen {
		return b
	}
	switch opts.Mode {
	case "step":
		return a
	case "smooth":
		u := divRound(q*q15, precisionDen, RoundNearest)
		s := divRound(u*u*(3*q15-2*u), q15*q15*q15, RoundNearest)
		return Vec2{
			X: a.X + mulDivRound(b.X-a.X, s, q15),
			Y: a.Y + mulDivRound(b.Y-a.Y, s, q15),
		}
	default:
		return Vec2{
			X: a.X + mulDivRound(b.X-a.X, q*1000000, precisionDen*1000000),
			Y: a.Y + mulDivRound(b.Y-a.Y, q*1000000, precisionDen*1000000),
		}
	}
}

// EvalPathAt returns the interpolated contour at an internal timestamp.
func EvalPathAt(path *Path, time int64, opts InterpOptions) ([]Segment, error) {
	if path == nil || len(path.Keyframes) == 0 {
		return nil, &Error{Code: CodeSchema, Path: "path.keyframes", Message: "path has no keyframes"}
	}
	if opts.Mode != "linear" && opts.Mode != "step" && opts.Mode != "smooth" {
		return nil, &Error{Code: CodePara, Path: "interpolation.mode", Field: "interpolation.mode", Message: fmt.Sprintf("unsupported interpolation %q", opts.Mode)}
	}
	if opts.PrecisionBits < 4 || opts.PrecisionBits > 24 {
		return nil, &Error{Code: CodePara, Path: "interpolation.precisionBits", Field: "interpolation.precisionBits", Message: "precision out of range"}
	}
	keys := path.Keyframes
	if time <= keys[0].Time {
		return cloneSegments(keys[0].Segments), nil
	}
	if time >= keys[len(keys)-1].Time {
		return cloneSegments(keys[len(keys)-1].Segments), nil
	}
	for i := 1; i < len(keys); i++ {
		if time > keys[i].Time {
			continue
		}
		left, right := keys[i-1], keys[i]
		if time == right.Time {
			return cloneSegments(right.Segments), nil
		}
		den := right.Time - left.Time
		num := time - left.Time
		out := make([]Segment, len(left.Segments))
		for j := range left.Segments {
			a, b := left.Segments[j], right.Segments[j]
			if a.Kind != b.Kind {
				return nil, &Error{Code: CodeSelf, Path: fmt.Sprintf("paths.%s.keyframes[%d].segments[%d].kind", path.ID, i, j), Message: "keyframe topology changed"}
			}
			out[j] = Segment{
				Kind:  a.Kind,
				Start: InterpolatePoint(a.Start, b.Start, num, den, opts),
				C1:    InterpolatePoint(a.C1, b.C1, num, den, opts),
				C2:    InterpolatePoint(a.C2, b.C2, num, den, opts),
				End:   InterpolatePoint(a.End, b.End, num, den, opts),
			}
		}
		return out, nil
	}
	return cloneSegments(keys[len(keys)-1].Segments), nil
}

func cloneSegments(in []Segment) []Segment {
	out := make([]Segment, len(in))
	copy(out, in)
	return out
}

// EvalCurvePoint evaluates a quadratic or cubic segment at tQ30 without float accumulation.
func EvalCurvePoint(seg Segment, tQ30 int64) Vec2 {
	if tQ30 < 0 {
		tQ30 = 0
	}
	if tQ30 > q30 {
		tQ30 = q30
	}
	if seg.Kind == Quadratic {
		a := lerpQ30(seg.Start, seg.C1, tQ30)
		b := lerpQ30(seg.C1, seg.End, tQ30)
		return lerpQ30(a, b, tQ30)
	}
	a := lerpQ30(seg.Start, seg.C1, tQ30)
	b := lerpQ30(seg.C1, seg.C2, tQ30)
	c := lerpQ30(seg.C2, seg.End, tQ30)
	d := lerpQ30(a, b, tQ30)
	e := lerpQ30(b, c, tQ30)
	return lerpQ30(d, e, tQ30)
}

func lerpQ30(a, b Vec2, t int64) Vec2 {
	if t <= 0 {
		return a
	}
	if t >= q30 {
		return b
	}
	return Vec2{
		X: a.X + mulDivRound(b.X-a.X, t, q30),
		Y: a.Y + mulDivRound(b.Y-a.Y, t, q30),
	}
}
