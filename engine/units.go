package engine

import (
	"fmt"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

func parseRounding(s string) (Rounding, error) {
	switch Rounding(s) {
	case "", RoundFloor:
		return RoundFloor, nil
	case RoundCeil:
		return RoundCeil, nil
	case RoundNearest:
		return RoundNearest, nil
	default:
		return "", &Error{Code: CodePara, Path: "rounding", Message: fmt.Sprintf("unsupported rounding direction %q", s)}
	}
}

func divRound(n, d int64, mode Rounding) int64 {
	if d == 0 {
		return 0
	}
	if d < 0 {
		n, d = -n, -d
	}
	q := n / d
	r := n % d
	if r == 0 {
		return q
	}
	switch mode {
	case RoundCeil:
		if n >= 0 {
			return q + 1
		}
		return q
	case RoundNearest:
		if r < 0 {
			r = -r
		}
		if r*2 >= d {
			if n >= 0 {
				return q + 1
			}
			return q - 1
		}
		return q
	default:
		if n < 0 && r != 0 {
			return q - 1
		}
		return q
	}
}

func mulDivRound(v, m, d int64) int64 {
	if d == 0 {
		return 0
	}
	return divRound(v*m, d, RoundNearest)
}

func millisToTicks(ms int64) int64 {
	return ms * TimeTicksPerMillisecond
}

func ticksToMillis(t int64, mode Rounding) int64 {
	return divRound(t, TimeTicksPerMillisecond, mode)
}

func framesToTicks(frames int64) int64 {
	return frames * TimeTicksPerFrame
}

func ticksToFrames(t int64, mode Rounding) int64 {
	return divRound(t, TimeTicksPerFrame, mode)
}

// RoundTripTime converts a time to the target schema clock and back.
// It returns E_RT when the declared direction cannot reconstruct the input.
func RoundTripTime(ticks int64, target Version, mode Rounding) (int64, error) {
	var encoded int64
	switch target {
	case Version1, Version3:
		encoded = ticksToMillis(ticks, mode)
	case Version2:
		encoded = ticksToFrames(ticks, mode)
	default:
		return 0, &Error{Code: CodeVer, Path: "targetVersion", Message: string(target)}
	}
	var back int64
	switch target {
	case Version1, Version3:
		back = millisToTicks(encoded)
	case Version2:
		back = framesToTicks(encoded)
	}
	if back != ticks {
		return back, &Error{
			Code:    CodeRT,
			Path:    "time",
			Field:   "time",
			Left:    fmt.Sprintf("%d", ticks),
			Right:   fmt.Sprintf("%d", back),
			Message: fmt.Sprintf("time is not representable in version %s with %s rounding", target, mode),
		}
	}
	return back, nil
}

// RoundTripCoordDelta checks the exact integer absolute/relative conversion.
func RoundTripCoordDelta(base, absolute Vec2) error {
	rel := absolute.Sub(base)
	back := base.Add(rel)
	if back != absolute {
		return &Error{
			Code:    CodeRT,
			Path:    "controlPoint",
			Field:   "coordinate",
			Left:    fmt.Sprintf("%+v", absolute),
			Right:   fmt.Sprintf("%+v", back),
			Message: "relative coordinate conversion is not closed",
		}
	}
	return nil
}

// PixelLength converts Q10 internal length into logical pixels, rounded up.
func PixelLength(length int64, scale int64) int64 {
	if length <= 0 {
		return 0
	}
	n := length * scale
	d := CoordScale * CoordScale
	return divRound(n, d, RoundCeil)
}

// ScaledCoord converts a Q10 internal coordinate into a floating pixel coordinate.
func ScaledCoord(v, origin int64, scale int64) float64 {
	return float64(v-origin) * float64(scale) / float64(CoordScale*CoordScale)
}
