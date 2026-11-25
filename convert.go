package lightshow

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const (
	MicrosPerSecond = int64(1_000_000)
	MicrosPerMilli  = int64(1_000)
	MaxColor16      = uint16(65535)
)

func roundRatioNearest(numerator, denominator int64) int64 {
	if denominator <= 0 {
		panic("lightshow: non-positive denominator")
	}
	if numerator >= 0 {
		return (numerator + denominator/2) / denominator
	}
	return -((-numerator + denominator/2) / denominator)
}

func floorRatio(numerator, denominator int64) int64 {
	if numerator >= 0 {
		return numerator / denominator
	}
	return -((-numerator + denominator - 1) / denominator)
}

func ceilRatio(numerator, denominator int64) int64 {
	if numerator >= 0 {
		return (numerator + denominator - 1) / denominator
	}
	return -((-numerator) / denominator)
}

func roundRatio(numerator, denominator int64, mode TimeRounding) int64 {
	switch mode {
	case RoundFloor:
		return floorRatio(numerator, denominator)
	case RoundCeil:
		return ceilRatio(numerator, denominator)
	default:
		return roundRatioNearest(numerator, denominator)
	}
}

// FrameToUS converts a frame index to whole microseconds using integer arithmetic.
func FrameToUS(frame, fps int64, mode TimeRounding) (int64, error) {
	if fps <= 0 {
		return 0, engineError(CodeVersion, "fps", "fps must be positive")
	}
	if mode != RoundNearest && mode != RoundFloor && mode != RoundCeil {
		return 0, engineError(CodeVersion, "time_rounding", "unsupported rounding %q", mode)
	}
	return roundRatio(frame*MicrosPerSecond, fps, mode), nil
}

// USToFrame converts microseconds to a frame using the declared rounding mode.
func USToFrame(us, fps int64, mode TimeRounding) (int64, error) {
	if fps <= 0 {
		return 0, engineError(CodeVersion, "fps", "fps must be positive")
	}
	return roundRatio(us*fps, MicrosPerSecond, mode), nil
}

func MSToUS(ms int64) int64 {
	return ms * MicrosPerMilli
}

func USToMS(us int64, mode TimeRounding) int64 {
	return roundRatio(us, MicrosPerMilli, mode)
}

func ParseColorStep(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "1/255"
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, engineError(CodeVersion, "color_step", "invalid positive rational %q", s)
	}
	return r, nil
}

func ratFromJSON(v any) (*big.Rat, error) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case float64:
		s = strconv.FormatFloat(n, 'g', -1, 64)
	case int:
		s = strconv.Itoa(n)
	case int64:
		s = strconv.FormatInt(n, 10)
	case string:
		s = n
	default:
		return nil, fmt.Errorf("unsupported numeric value %T", v)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	return r, nil
}

// NormalizedToRGB16 quantizes a normalized channel by the declared step and
// expands the quantized value to the engine's 16-bit channel space.
func NormalizedToRGB16(values [3]any, step *big.Rat) (RGB, error) {
	if step == nil || step.Sign() <= 0 {
		return RGB{}, engineError(CodeVersion, "color_step", "invalid positive rational")
	}
	channels := [3]uint16{}
	max := new(big.Rat).SetFrac64(65535, 1)
	for i, raw := range values {
		r, err := ratFromJSON(raw)
		if err != nil {
			return RGB{}, engineError(CodeVersion, fmt.Sprintf("color[%d]", i), "%v", err)
		}
		q := new(big.Rat).Quo(r, step)
		n := roundRatNearest(q)
		physical := new(big.Rat).Mul(new(big.Rat).SetInt64(n), step)
		if physical.Sign() < 0 {
			physical.SetInt64(0)
		}
		if physical.Cmp(max) > 0 {
			physical.SetInt64(65535)
		}
		scaled := new(big.Rat).Mul(physical, max)
		channels[i] = uint16(roundRatNearest(scaled))
	}
	return RGB{R: channels[0], G: channels[1], B: channels[2]}, nil
}

func roundRatNearest(r *big.Rat) int64 {
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())
	half := new(big.Int).Div(den, big.NewInt(2))
	if num.Sign() >= 0 {
		num.Add(num, half)
		return new(big.Int).Quo(num, den).Int64()
	}
	num.Neg(num)
	num.Add(num, half)
	return -new(big.Int).Quo(num, den).Int64()
}

// RGB8To16 expands an 8-bit channel losslessly into the 16-bit engine space.
func RGB8To16(r, g, b uint8) RGB {
	return RGB{R: uint16(r) * 257, G: uint16(g) * 257, B: uint16(b) * 257}
}

// RGB16To8 quantizes exactly to the declared 1/255 step with nearest rounding.
func RGB16To8(c RGB) (uint8, uint8, uint8) {
	q := func(v uint16) uint8 {
		n := (uint32(v) + 128) / 257
		if n > 255 {
			n = 255
		}
		return uint8(n)
	}
	return q(c.R), q(c.G), q(c.B)
}

func Color16ToNormalized(v uint16) float64 {
	return float64(v) / 65535.0
}

func ColorTolerance(version Version) int {
	if version == Version1 {
		return 257
	}
	return 0
}

// TimeTolerance is one frame at the declared frame rate, rounded up.
func TimeTolerance(fps int64) int64 {
	if fps <= 0 {
		return 0
	}
	return (MicrosPerSecond + fps - 1) / fps
}

func interpolateInt(from, to int, elapsedUS, durationUS int64) int {
	if durationUS <= 0 || elapsedUS <= 0 {
		return from
	}
	if elapsedUS >= durationUS {
		return to
	}
	delta := int64(to - from)
	numerator := int64(from)*durationUS + delta*elapsedUS
	return int(roundRatioNearest(numerator, durationUS))
}

func clampInt(v int, r IntRange) int {
	return r.Clamp(v)
}
