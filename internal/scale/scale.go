// Package scale performs every cross-schema unit conversion using only
// declared-width fixed-point integers. Each conversion returns the exported
// intermediate steps so that correctness can be replayed against the
// generator's ground truth.
package scale

import (
	"errors"
	"strconv"
	"strings"

	"schedlab/internal/fixedint"
)

// Step is one exported fixed-point intermediate.
type Step struct {
	Op        string
	InA       int64
	InB       int64
	Result    int64
	Overflow  bool
	Saturated bool
}

// Trace is the full intermediate record of a conversion.
type Trace struct {
	Kind     string
	Input    string
	Steps    []Step
	Output   int64
	Overflow bool
}

func (t *Trace) add(s Step) {
	t.Steps = append(t.Steps, s)
	if s.Overflow {
		t.Overflow = true
	}
}

// ErrOverflow signals that a conversion exceeded the declared int64 width.
var ErrOverflow = errors.New("fixed-point overflow")

// ErrSyntax signals an unparseable quantity.
var ErrSyntax = errors.New("invalid quantity syntax")

// PermilleToBPS converts a per-mille weight to basis points (x10, exact).
func PermilleToBPS(permille int64) (int64, Trace, error) {
	tr := Trace{Kind: "permille_to_bps", Input: strconv.FormatInt(permille, 10)}
	r, ov := fixedint.Mul(permille, 10)
	tr.add(Step{Op: "mul10", InA: permille, InB: 10, Result: r, Overflow: ov, Saturated: ov})
	if ov {
		tr.Output = r
		return r, tr, ErrOverflow
	}
	tr.Output = r
	return r, tr, nil
}

// BPSToPermille converts basis points back to per-mille. Non-integral values
// (not a multiple of ten) cannot be represented exactly and return ErrSyntax
// rather than silently truncating.
func BPSToPermille(bps int64) (int64, Trace, error) {
	tr := Trace{Kind: "bps_to_permille", Input: strconv.FormatInt(bps, 10)}
	if bps%10 != 0 {
		return 0, tr, ErrSyntax
	}
	r, ov := fixedint.Scale(bps, 1, 10)
	tr.add(Step{Op: "div10", InA: bps, InB: 10, Result: r, Overflow: ov})
	tr.Output = r
	if ov {
		return r, tr, ErrOverflow
	}
	return r, tr, nil
}

// CoresToMilli converts whole/decimal cores to integer milli-cores (x1000).
func CoresToMilli(cores string) (int64, Trace, error) {
	tr := Trace{Kind: "cores_to_milli", Input: cores}
	rat, err := fixedint.ParseRational(cores)
	if err != nil {
		return 0, tr, ErrSyntax
	}
	r, ov := fixedint.Scale(rat.Num, 1000, rat.Den)
	tr.add(Step{Op: "scale1000", InA: rat.Num, InB: rat.Den, Result: r, Overflow: ov, Saturated: ov})
	tr.Output = r
	if ov {
		return r, tr, ErrOverflow
	}
	return r, tr, nil
}

// MilliToken parses an explicit milli-core token such as "1500m".
func MilliToken(token string) (int64, Trace, error) {
	tr := Trace{Kind: "milli_token", Input: token}
	if !strings.HasSuffix(token, "m") {
		return 0, tr, ErrSyntax
	}
	v, err := strconv.ParseInt(token[:len(token)-1], 10, 64)
	if err != nil {
		return 0, tr, ErrSyntax
	}
	tr.add(Step{Op: "identity_milli", InA: v, Result: v})
	tr.Output = v
	return v, tr, nil
}

// RatioToMilli resolves a percentage/decimal ratio against a reference
// capacity (in milli-cores), rounding half away from zero.
func RatioToMilli(ratio string, refMilliCPU int64) (int64, Trace, error) {
	tr := Trace{Kind: "ratio_to_milli", Input: ratio}
	rat, err := fixedint.ParseRational(ratio)
	if err != nil {
		return 0, tr, ErrSyntax
	}
	num := rat.Num
	den := rat.Den
	if rat.IsPercent {
		// percent means num/den/100, i.e. denominator x100.
		d, ov := fixedint.Mul(den, 100)
		if ov {
			tr.add(Step{Op: "percent_den_extend", InA: den, InB: 100, Result: d, Overflow: true, Saturated: true})
			tr.Output = d
			return d, tr, ErrOverflow
		}
		den = d
		tr.add(Step{Op: "percent_den_extend", InA: rat.Den, InB: 100, Result: den})
	}
	r, ov := fixedint.Scale(refMilliCPU, num, den)
	tr.add(Step{Op: "ratio_scale", InA: refMilliCPU, InB: den, Result: r, Overflow: ov, Saturated: ov})
	tr.Output = r
	if ov {
		return r, tr, ErrOverflow
	}
	return r, tr, nil
}

// MemoryToBytes parses a memory token into bytes. Accepted binary suffixes:
// Ki, Mi, Gi, Ti and plain integer bytes. Decimal is exact integer scaling.
var memSuffix = map[string]int64{
	"":   1,
	"Ki": 1 << 10,
	"Mi": 1 << 20,
	"Gi": 1 << 30,
	"Ti": 1 << 40,
}

func MemoryToBytes(token string) (int64, Trace, error) {
	tr := Trace{Kind: "memory_to_bytes", Input: token}
	suffix := ""
	body := token
	for s := range memSuffix {
		if s != "" && strings.HasSuffix(token, s) {
			suffix = s
			body = token[:len(token)-len(s)]
			break
		}
	}
	v, err := strconv.ParseInt(body, 10, 64)
	if err != nil {
		return 0, tr, ErrSyntax
	}
	mult := memSuffix[suffix]
	r, ov := fixedint.Mul(v, mult)
	tr.add(Step{Op: "mem_scale", InA: v, InB: mult, Result: r, Overflow: ov, Saturated: ov})
	tr.Output = r
	if ov {
		return r, tr, ErrOverflow
	}
	return r, tr, nil
}

// TickToMs converts tick timestamps to milliseconds at the declared period.
const TickPeriodMs int64 = 100

func TickToMs(ticks int64) (int64, Trace, error) {
	tr := Trace{Kind: "tick_to_ms", Input: strconv.FormatInt(ticks, 10)}
	r, ov := fixedint.Mul(ticks, TickPeriodMs)
	tr.add(Step{Op: "tick_x100", InA: ticks, InB: TickPeriodMs, Result: r, Overflow: ov, Saturated: ov})
	tr.Output = r
	if ov {
		return r, tr, ErrOverflow
	}
	return r, tr, nil
}
