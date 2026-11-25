package fixedpoint

// CriticalCase is one generator-defined boundary conversion used to prove
// the declared fixed-point tolerance never changes an admission decision.
type CriticalCase struct {
	Name      string
	Width     int
	Got       int64
	Want      int64
	Tolerance int64
	// Error is the accumulated conversion error for the case.
	Error int64
}

// WithinTolerance marks whether the case stays inside its declared bound.
func (c CriticalCase) WithinTolerance() bool {
	return WithinTolerance(c.Got, c.Want, c.Tolerance)
}

// CriticalCases builds deterministic boundary conversions: non-divisible
// ms->tick durations, ‰->‱ scale changes and remaining->absolute limits.
func CriticalCases(width int, tickMs, tolerance int64) []CriticalCase {
	cases := make([]CriticalCase, 0, 6)

	// 250ms with a 100ms tick -> 2 ticks, accumulated 50ms remainder.
	ms := int64(250)
	ticks := MsToTicks(width, ms, tickMs)
	acc := &Accumulator{Width: width, Tolerance: tolerance}
	_ = acc.AddRemainder(ticks.Remainder)
	cases = append(cases, CriticalCase{
		Name: "ms_to_tick_nondivisible", Width: width,
		Got: ticks.Value, Want: ms / tickMs, Tolerance: tolerance,
		Error: acc.Sum(),
	})

	// Round trip ticks->ms must be exact and stay inside the width.
	back := TicksToMs(width, ticks.Value, tickMs)
	cases = append(cases, CriticalCase{
		Name: "tick_to_ms_roundtrip", Width: width,
		Got: back.Value, Want: ticks.Value * tickMs, Tolerance: tolerance,
		Error: 0,
	})

	// Seconds->ms is an exact scale change.
	sec := SecondsToMs(width, 3)
	cases = append(cases, CriticalCase{
		Name: "seconds_to_ms", Width: width,
		Got: sec.Value, Want: 3000, Tolerance: tolerance, Error: 0,
	})

	// ‰ -> ‱ scale change, exact.
	permille, _ := BudgetRatio(width, 500, ScalePermille)
	cases = append(cases, CriticalCase{
		Name: "permille_to_permyriad", Width: width,
		Got: permille.Value, Want: 5000, Tolerance: tolerance, Error: 0,
	})

	// remaining-available -> absolute limit.
	abs := AbsoluteFromRemaining(width, 3, 7)
	cases = append(cases, CriticalCase{
		Name: "remaining_to_absolute", Width: width,
		Got: abs.Value, Want: 10, Tolerance: tolerance, Error: 0,
	})

	// A deliberately saturated narrow-width addition.
	sat := Add(32, maxInt32(), 1)
	cases = append(cases, CriticalCase{
		Name: "narrow_width_saturation", Width: 32,
		Got: sat.Value, Want: maxInt32(), Tolerance: 0, Error: 0,
	})
	return cases
}

func maxInt32() int64 { return int64(1)<<31 - 1 }

// AllWithinTolerance reports whether every critical case stays in bounds.
func AllWithinTolerance(cases []CriticalCase) bool {
	for _, c := range cases {
		if !c.WithinTolerance() {
			return false
		}
	}
	return true
}
