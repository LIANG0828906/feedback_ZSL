package vectorcore

import (
	"fmt"
	"math/big"
	"sort"
)

type EvaluatedPath struct {
	PathID        string
	Paint         Paint
	Contour       Contour
	KeyframeIndex []int
}

type FrameEvaluation struct {
	TimeIndex int
	TimeTicks int64
	Paths     []EvaluatedPath
}

func keyframeBounds(keys []Keyframe, t int64) (left, right int, exact bool, ok bool) {
	if len(keys) == 0 {
		return 0, 0, false, false
	}
	if t <= keys[0].Time {
		return 0, 0, t == keys[0].Time, true
	}
	if t >= keys[len(keys)-1].Time {
		i := len(keys) - 1
		return i, i, t == keys[i].Time, true
	}
	i := sort.Search(len(keys), func(i int) bool { return keys[i].Time >= t })
	if keys[i].Time == t {
		return i, i, true, true
	}
	return i - 1, i, false, true
}

// KeyframeIndicesForTime returns the keyframes whose values participate in
// evaluating t. This is also the exact invalidation boundary for keyframe edits.
func KeyframeIndicesForTime(path Path, t int64) []int {
	left, right, exact, ok := keyframeBounds(path.Keyframes, t)
	if !ok {
		return nil
	}
	if exact || left == right {
		return []int{left}
	}
	return []int{left, right}
}

func EvaluateFrame(scene *Scene, timeIndex int, cfg Config) (*FrameEvaluation, error) {
	if scene == nil {
		return nil, newError(CodeSchema, "evaluate", fmt.Errorf("nil scene"))
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if timeIndex < 0 || timeIndex >= len(scene.SampleTimes) {
		return nil, &Error{Code: CodeParam, Op: "evaluate", Field: "timeIndex", Keyframe: -1, TimeIndex: timeIndex,
			Err: fmt.Errorf("outside [0,%d)", len(scene.SampleTimes))}
	}
	eval := &FrameEvaluation{TimeIndex: timeIndex, TimeTicks: scene.SampleTimes[timeIndex]}
	for pi := range scene.Paths {
		p := &scene.Paths[pi]
		contour, used, err := interpolatePath(p, eval.TimeTicks, cfg.Interpolation)
		if err != nil {
			return nil, err
		}
		eval.Paths = append(eval.Paths, EvaluatedPath{PathID: p.ID, Paint: p.Paint, Contour: contour, KeyframeIndex: used})
	}
	return eval, nil
}

func interpolatePath(path *Path, t int64, mode InterpMode) (Contour, []int, error) {
	left, right, exact, ok := keyframeBounds(path.Keyframes, t)
	if !ok {
		return Contour{}, nil, &Error{Code: CodeSchema, Op: "interpolate", PathID: path.ID, Field: "keyframes", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("path has no keyframes")}
	}
	if exact || left == right {
		return path.Keyframes[left].Contour.Clone(), []int{left}, nil
	}
	k0, k1 := path.Keyframes[left], path.Keyframes[right]
	den := k1.Time - k0.Time
	if den <= 0 {
		return Contour{}, nil, &Error{Code: CodeSchema, Op: "interpolate", PathID: path.ID, Keyframe: left, TimeIndex: -1,
			Err: fmt.Errorf("non-positive keyframe duration")}
	}
	n := t - k0.Time
	var num, cden *big.Int
	switch mode {
	case InterpLinear:
		num, cden = big.NewInt(n), big.NewInt(den)
	case InterpSmoothStep:
		nb, db := big.NewInt(n), big.NewInt(den)
		num = new(big.Int).Mul(nb, nb)
		threeDMinusTwoN := new(big.Int).Sub(new(big.Int).Mul(big.NewInt(3), db), new(big.Int).Lsh(nb, 1))
		num.Mul(num, threeDMinusTwoN)
		cden = new(big.Int).Exp(db, big.NewInt(3), nil)
	case InterpHold:
		num, cden = big.NewInt(0), big.NewInt(1)
	default:
		return Contour{}, nil, &Error{Code: CodeParam, Op: "interpolate", Field: "interpolation", PathID: path.ID, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported interpolation %q", mode)}
	}
	if len(k0.Contour.Segments) != len(k1.Contour.Segments) {
		return Contour{}, nil, &Error{Code: CodeSchema, Op: "interpolate", PathID: path.ID, Keyframe: right, TimeIndex: -1,
			Err: fmt.Errorf("keyframe segment topology differs")}
	}
	start, err := interpPointBig(k0.Contour.Start, k1.Contour.Start, num, cden)
	if err != nil {
		return Contour{}, nil, &Error{Code: CodeOverflow, Op: "interpolate", PathID: path.ID, Keyframe: left, TimeIndex: -1, Field: "contour.start", Err: err}
	}
	out := Contour{Start: start}
	for si := range k0.Contour.Segments {
		a, b := k0.Contour.Segments[si], k1.Contour.Segments[si]
		if a.Kind != b.Kind {
			return Contour{}, nil, &Error{Code: CodeSchema, Op: "interpolate", PathID: path.ID, Keyframe: right, TimeIndex: -1,
				Err: fmt.Errorf("segment %d kind differs", si)}
		}
		c1, err := interpPointBig(a.C1, b.C1, num, cden)
		if err != nil {
			return Contour{}, nil, &Error{Code: CodeOverflow, Op: "interpolate", PathID: path.ID, Keyframe: left, TimeIndex: -1, Field: fmt.Sprintf("contour.segments[%d].c1", si), Err: err}
		}
		to, err := interpPointBig(a.To, b.To, num, cden)
		if err != nil {
			return Contour{}, nil, &Error{Code: CodeOverflow, Op: "interpolate", PathID: path.ID, Keyframe: left, TimeIndex: -1, Field: fmt.Sprintf("contour.segments[%d].to", si), Err: err}
		}
		seg := Segment{Kind: a.Kind, C1: c1, To: to}
		if a.Kind == SegmentCubic {
			c2, err := interpPointBig(a.C2, b.C2, num, cden)
			if err != nil {
				return Contour{}, nil, &Error{Code: CodeOverflow, Op: "interpolate", PathID: path.ID, Keyframe: left, TimeIndex: -1, Field: fmt.Sprintf("contour.segments[%d].c2", si), Err: err}
			}
			seg.C2 = c2
		}
		out.Segments = append(out.Segments, seg)
	}
	return out, []int{left, right}, nil
}

func interpPointBig(a, b Point, num, den *big.Int) (Point, error) {
	x, err := interpIntBig(a.X, b.X, num, den)
	if err != nil {
		return Point{}, err
	}
	y, err := interpIntBig(a.Y, b.Y, num, den)
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func interpIntBig(a, b int64, num, den *big.Int) (int64, error) {
	d := new(big.Int).Sub(big.NewInt(b), big.NewInt(a))
	d.Mul(d, num)
	q, r := new(big.Int).QuoRem(d, den, new(big.Int))
	if r.Sign() < 0 {
		r.Neg(r)
	}
	twice := new(big.Int).Lsh(r, 1)
	if twice.Cmp(den) >= 0 {
		if d.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	res := new(big.Int).Add(big.NewInt(a), q)
	if !res.IsInt64() {
		return 0, fmt.Errorf("coordinate overflow")
	}
	return res.Int64(), nil
}

// SampleSegment evaluates one segment at fixed rational positions. The output
// includes t=0 and is useful for analytic truth records.
func SampleSegment(contour Contour, segmentIndex, divisions int) ([]Point, error) {
	if segmentIndex < 0 || segmentIndex >= len(contour.Segments) {
		return nil, fmt.Errorf("segment index %d outside [0,%d)", segmentIndex, len(contour.Segments))
	}
	if divisions < 1 {
		return nil, fmt.Errorf("divisions must be positive")
	}
	start := contour.Start
	for i := 0; i < segmentIndex; i++ {
		start = contour.Segments[i].To
	}
	seg := contour.Segments[segmentIndex]
	out := make([]Point, 0, divisions+1)
	for i := 0; i <= divisions; i++ {
		out = append(out, evalSegment(seg, start, i, divisions))
	}
	return out, nil
}

func evalSegment(seg Segment, start Point, num, den int) Point {
	if seg.Kind == SegmentQuad {
		a := lerpPoint(start, seg.C1, num, den)
		b := lerpPoint(seg.C1, seg.To, num, den)
		return lerpPoint(a, b, num, den)
	}
	a := lerpPoint(start, seg.C1, num, den)
	b := lerpPoint(seg.C1, seg.C2, num, den)
	c := lerpPoint(seg.C2, seg.To, num, den)
	d := lerpPoint(a, b, num, den)
	e := lerpPoint(b, c, num, den)
	return lerpPoint(d, e, num, den)
}

func lerpPoint(a, b Point, num, den int) Point {
	dx := b.X - a.X
	dy := b.Y - a.Y
	if den == 0 {
		return a
	}
	return Point{X: a.X + divRoundInt(dx*int64(num), int64(den)), Y: a.Y + divRoundInt(dy*int64(num), int64(den))}
}

func divRoundInt(n, d int64) int64 {
	if d == 0 {
		return 0
	}
	if d < 0 {
		n, d = -n, -d
	}
	q, r := n/d, n%d
	if r < 0 {
		r = -r
	}
	if r*2 >= d {
		if n < 0 {
			q--
		} else {
			q++
		}
	}
	return q
}
