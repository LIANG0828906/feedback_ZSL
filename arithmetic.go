package vectorcore

import (
	"fmt"
	"math"
	"math/big"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

func ParseRounding(s string) (Rounding, error) {
	switch Rounding(s) {
	case "", RoundNearest:
		return RoundNearest, nil
	case RoundFloor:
		return RoundFloor, nil
	case RoundCeil:
		return RoundCeil, nil
	default:
		return "", fmt.Errorf("unsupported rounding %q", s)
	}
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func checkedSub(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > 0 {
		if b > 0 {
			if a > math.MaxInt64/b {
				return 0, false
			}
		} else if b < math.MinInt64/a {
			return 0, false
		}
	} else {
		if b > 0 {
			if a < math.MinInt64/b {
				return 0, false
			}
		} else if b < math.MaxInt64/a {
			return 0, false
		}
	}
	return a * b, true
}

func divRound(n, d int64, mode Rounding) (int64, bool) {
	if d == 0 {
		return 0, false
	}
	if d < 0 {
		n, d = -n, -d
	}
	q := n / d
	r := n % d
	switch mode {
	case RoundFloor:
		if r != 0 && n < 0 {
			q--
		}
	case RoundCeil:
		if r != 0 && n > 0 {
			q++
		}
	case RoundNearest:
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
	}
	return q, true
}

// MulDivRound computes a*b/c exactly with a signed 128-bit intermediate.
func MulDivRound(a, b, c int64, mode Rounding) (int64, error) {
	if c == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	den := big.NewInt(c)
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	roundAway := false
	switch mode {
	case RoundFloor:
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		}
	case RoundCeil:
		if r.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
	case RoundNearest:
		twice := new(big.Int).Lsh(new(big.Int).Abs(r), 1)
		if twice.Cmp(den) >= 0 {
			roundAway = true
		}
		if roundAway {
			if num.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("result overflow")
	}
	return q.Int64(), nil
}

func FrameToTicks(frame, fps int64, mode Rounding) (int64, error) {
	if frame < 0 || fps <= 0 {
		return 0, &Error{Code: CodeParam, Op: "frameToTicks", Field: "frame/fps", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("frame must be non-negative and fps positive")}
	}
	if _, ok := checkedMul(frame, TicksPerSecond); !ok {
		return 0, &Error{Code: CodeOverflow, Op: "frameToTicks", Field: "frame", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("overflow")}
	}
	ticks, err := MulDivRound(frame, TicksPerSecond, fps, mode)
	if err != nil {
		return 0, &Error{Code: CodeOverflow, Op: "frameToTicks", Field: "frame", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return ticks, nil
}

func TicksToFrame(ticks, fps int64, mode Rounding) (int64, error) {
	if ticks < 0 || fps <= 0 {
		return 0, &Error{Code: CodeParam, Op: "ticksToFrame", Field: "ticks/fps", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("ticks must be non-negative and fps positive")}
	}
	frame, err := MulDivRound(ticks, fps, TicksPerSecond, mode)
	if err != nil {
		return 0, &Error{Code: CodeOverflow, Op: "ticksToFrame", Field: "ticks", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return frame, nil
}

func MillisToTicks(ms int64, mode Rounding) (int64, error) {
	if ms < 0 {
		return 0, &Error{Code: CodeParam, Op: "millisToTicks", Field: "milliseconds", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("milliseconds must be non-negative")}
	}
	if _, ok := checkedMul(ms, TicksPerSecond); !ok {
		return 0, &Error{Code: CodeOverflow, Op: "millisToTicks", Field: "milliseconds", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("overflow")}
	}
	ticks, err := MulDivRound(ms, TicksPerSecond, 1000, mode)
	if err != nil {
		return 0, &Error{Code: CodeOverflow, Op: "millisToTicks", Field: "milliseconds", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return ticks, nil
}

func TicksToMillis(ticks int64, mode Rounding) (int64, error) {
	if ticks < 0 {
		return 0, &Error{Code: CodeParam, Op: "ticksToMillis", Field: "ticks", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("ticks must be non-negative")}
	}
	ms, err := MulDivRound(ticks, 1000, TicksPerSecond, mode)
	if err != nil {
		return 0, &Error{Code: CodeOverflow, Op: "ticksToMillis", Field: "ticks", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	return ms, nil
}

func ToRelative(points []Point, base Point) ([]Point, error) {
	out := make([]Point, len(points))
	for i, p := range points {
		x, ok := checkedSub(p.X, base.X)
		if !ok {
			return nil, &Error{Code: CodeOverflow, Op: "toRelative", Field: "x", Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("coordinate overflow")}
		}
		y, ok := checkedSub(p.Y, base.Y)
		if !ok {
			return nil, &Error{Code: CodeOverflow, Op: "toRelative", Field: "y", Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("coordinate overflow")}
		}
		out[i] = Point{X: x, Y: y}
	}
	return out, nil
}

func ToAbsolute(points []Point, base Point) ([]Point, error) {
	out := make([]Point, len(points))
	for i, p := range points {
		x, ok := checkedAdd(p.X, base.X)
		if !ok {
			return nil, &Error{Code: CodeOverflow, Op: "toAbsolute", Field: "x", Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("coordinate overflow")}
		}
		y, ok := checkedAdd(p.Y, base.Y)
		if !ok {
			return nil, &Error{Code: CodeOverflow, Op: "toAbsolute", Field: "y", Keyframe: -1, TimeIndex: -1,
				Err: fmt.Errorf("coordinate overflow")}
		}
		out[i] = Point{X: x, Y: y}
	}
	return out, nil
}
