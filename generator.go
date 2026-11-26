package cachebench

import "fmt"

const DefaultSeed int64 = 20261127

// DeterministicRNG is a splitmix64-based source. It avoids math/rand global
// state and guarantees stable samples across Go versions and scheduling.
type DeterministicRNG struct{ state uint64 }

func NewDeterministicRNG(seed int64) *DeterministicRNG {
	if seed == 0 {
		seed = DefaultSeed
	}
	return &DeterministicRNG{state: uint64(seed)}
}

func (r *DeterministicRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *DeterministicRNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type Bundle struct {
	Project Project
	Docs    map[string][]byte
	Truth   *SimulationResult
}

// GenerateBundle creates the one deterministic local sample used by tests and
// the CLI demo. Truth is produced by the same normalized kernel but is exposed
// as the generator's oracle for loader and scheduling comparisons.
func GenerateBundle(seed int64) (*Bundle, error) {
	rng := NewDeterministicRNG(seed)
	p := Project{
		SchemaVersion:       SchemaV3,
		ProjectID:           "edge-cache-capacity-seed",
		Seed:                seed,
		TickMS:              1,
		DurationMS:          60,
		WindowMS:            10,
		WeightSourceScale:   ScalePermyriad,
		CapacitySourceScale: ScalePermyriad,
		RoutingPolicy:       RoutingPolicy{Mode: "weighted", HashBucketScale: ScalePermyriad, ZeroWeightDeny: true},
		StatTolerancePPM:    5000,
	}

	nodeDefs := []struct {
		id, rack                                             string
		prio, order, cap, inflightTh, recTh                  int
		weight, service, trigger, recovery, errLimit, errRec int64
	}{
		{"cache-north-1", "rack-north", 1, 0, 5, 3, 1, 4000, 10, 5, 10, 250000, 100000},
		{"cache-north-2", "rack-north", 1, 1, 4, 2, 0, 3000, 10, 5, 10, 250000, 100000},
		{"cache-south-1", "rack-south", 1, 2, 6, 4, 2, 2000, 10, 5, 10, 200000, 80000},
		{"cache-south-2", "rack-south", 1, 3, 3, 1, 0, 1000, 5, 5, 5, 200000, 80000},
	}
	for _, d := range nodeDefs {
		p.Nodes = append(p.Nodes, Node{
			ID: d.id, Rack: d.rack, Priority: d.prio, DeclarationOrder: d.order,
			WeightPermyriad: d.weight, MaxInflight: d.cap, ServiceMS: d.service, TimeoutMS: d.service * 2,
			ConstraintID: "CONN-LIMIT-" + d.id, CapacityLimitPPM: 800000,
			InflightThreshold: d.inflightTh, RecoveryInflightThreshold: d.recTh,
			ErrorRateLimitPPM: d.errLimit, ErrorRateRecoveryPPM: d.errRec,
			TriggerMS: d.trigger, RecoveryMS: d.recovery, MinErrorSamples: 5,
		})
	}

	ordinal := 0
	add := func(at int64, candidates []string, forceError bool) {
		ordinal++
		p.Requests = append(p.Requests, Request{
			ID: fmt.Sprintf("req-%04d", ordinal), Ordinal: ordinal, AtMS: at,
			Candidates: append([]string(nil), candidates...), IsError: forceError,
		})
	}

	// Regular deterministic traffic arrives on the five-ms V1 tick grid.
	for tick := int64(0); tick < 12; tick++ {
		at := tick * 5
		batch := 6 + rng.Intn(4)
		for i := 0; i < batch; i++ {
			add(at, nil, rng.Intn(100) < 10)
		}
	}
	// Guaranteed capacity overflow critical sample.
	for i := 0; i < 8; i++ {
		add(10, []string{"cache-north-1"}, false)
	}
	// Guaranteed error-rate breaker critical sample.
	for i := 0; i < 8; i++ {
		add(25, []string{"cache-north-2"}, true)
	}
	// Request edit/delete tests operate after the generated stream.

	obsCounts := []struct {
		reqs, errs, lat int64
		over            bool
	}{
		{18, 1, 85, false},
		{27, 3, 140, false},
		{31, 9, 190, true},
		{22, 2, 110, false},
		{12, 0, 55, false},
	}
	for i, o := range obsCounts {
		p.Observations = append(p.Observations, Observation{
			WindowIndex: i, AtMS: int64(i) * p.WindowMS,
			Requests: o.reqs, Errors: o.errs, TotalLatencyMS: o.lat,
			ErrorRateSource: o.errs * ScalePermille / o.reqs,
			Overloaded:      o.over, OverloadMarkerPresent: i == 2 || i == 3,
		})
	}

	p.CriticalCases = []CriticalCase{
		{ID: "cc-permille-u8-saturate", Kind: "ratio", WidthBits: 8, FromValue: 13, FromScale: ScalePermille, ToScale: ScalePermyriad, TolerancePPM: 0, ExpectedValue: 127, ShouldSaturate: true},
		{ID: "cc-permille-u8-exact", Kind: "ratio", WidthBits: 16, FromValue: 13, FromScale: ScalePermille, ToScale: ScalePermyriad, TolerancePPM: 0, ExpectedValue: 130},
		{ID: "cc-permyriad-round", Kind: "ratio", WidthBits: 16, FromValue: 1234, FromScale: ScalePermyriad, ToScale: ScalePermille, TolerancePPM: 50000, ExpectedValue: 123},
		{ID: "cc-tick-u16-saturate", Kind: "tick_ms", WidthBits: 16, FromValue: 6554, TickMS: 5, TolerancePPM: 0, ExpectedValue: 32767, ShouldSaturate: true},
		{ID: "cc-tick-ms-exact", Kind: "tick_ms", WidthBits: 32, FromValue: 12, TickMS: 5, TolerancePPM: 0, ExpectedValue: 60},
		{ID: "cc-seconds-q3-to-ms", Kind: "seconds_ms", WidthBits: 32, FromValue: 1234, TickMS: 1000, TolerancePPM: 1000, ExpectedValue: 1234},
		{ID: "cc-throughput-q3", Kind: "throughput", WidthBits: 32, FromValue: 2000, FromScale: 1000, TickMS: 5, TolerancePPM: 0, ExpectedValue: 400000},
	}

	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	truth, err := Simulate(p)
	if err != nil {
		return nil, err
	}
	docs, err := EncodeSchemaDocs(p)
	if err != nil {
		return nil, err
	}
	return &Bundle{Project: p, Docs: docs, Truth: truth}, nil
}

// RunCriticalConversions executes every generator boundary case and returns a
// stable per-case report. It never panics on overflow; saturation is reported.
func RunCriticalConversions(p Project) []CriticalReport {
	out := make([]CriticalReport, 0, len(p.CriticalCases))
	for _, c := range p.CriticalCases {
		rep := CriticalReport{CriticalCase: c}
		var got FixedResult
		switch c.Kind {
		case "ratio":
			got = ConvertScale(c.FromValue, c.FromScale, c.ToScale, c.WidthBits)
		case "tick_ms":
			got = TicksToMS(c.FromValue, c.TickMS, c.WidthBits)
		case "seconds_ms":
			got = FixedSecondsToMS(c.FromValue, 1000, c.WidthBits)
		case "throughput":
			rateScale := c.FromScale
			if rateScale == 0 {
				rateScale = 1000
			}
			got = RatePerTickToPerSecond(c.FromValue, rateScale, c.TickMS, c.WidthBits)
		default:
			got = FixedResult{Err: NewError(CodeInvalidArgument, "unknown critical kind %s", c.Kind)}
		}
		rep.GotValue = got.Value
		rep.Saturated = got.Saturated
		rep.Overflow = got.Overflow
		rep.WithinTolerance = got.Err == nil || got.Saturated
		if !c.ShouldSaturate {
			rep.WithinTolerance = WithinTolerancePPM(got.Value, c.ExpectedValue, c.TolerancePPM)
		} else {
			rep.WithinTolerance = got.Saturated && got.Value == c.ExpectedValue
		}
		if got.Err != nil && !got.Saturated {
			rep.Code = got.Err.Code
		}
		out = append(out, rep)
	}
	return out
}

type CriticalReport struct {
	CriticalCase
	GotValue        int64
	Saturated       bool
	Overflow        bool
	WithinTolerance bool
	Code            ErrorCode
}
