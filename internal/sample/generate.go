// Package sample contains the deterministic fixed-seed frame engineering
// generator and its parser-constructed truth tables.
package sample

import (
	"fmt"
	"math/rand"

	"multiplexgw/internal/engine"
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
)

const DefaultSeed int64 = 424242

type Bundle struct {
	Canonical      *model.Project  `json:"canonical"`
	V1             []byte          `json:"-"`
	V2             []byte          `json:"-"`
	V3             []byte          `json:"-"`
	Truth          *Truth          `json:"truth"`
	NumericalCases []NumericalCase `json:"numerical_cases"`
}

type Truth struct {
	Seed               int64              `json:"seed"`
	Generator          string             `json:"generator"`
	InitialWindows     map[string]int64   `json:"initial_windows"`
	EventTruth         []model.Trace      `json:"event_truth"`
	ScheduleTruth      []model.SendRecord `json:"schedule_truth"`
	ReferenceResult    *model.RunResult   `json:"-"`
	NumericalCases     []NumericalCase    `json:"numerical_cases"`
	ExpectedErrorCodes map[string]string  `json:"expected_error_codes"`
}

type NumericalCase struct {
	Name            string            `json:"name"`
	StreamID        int               `json:"stream_id,omitempty"`
	InputPermille   int64             `json:"input_permille,omitempty"`
	InputPermyriad  int64             `json:"input_permyriad,omitempty"`
	TotalWeight     int64             `json:"total_weight,omitempty"`
	AllocatedBytes  int64             `json:"allocated_bytes,omitempty"`
	TotalBytes      int64             `json:"total_bytes,omitempty"`
	RepeatCount     int               `json:"repeat_count,omitempty"`
	IncrementBytes  int64             `json:"increment_bytes,omitempty"`
	StartWindow     int64             `json:"start_window,omitempty"`
	CapWindow       int64             `json:"cap_window,omitempty"`
	ExpectedWindow  int64             `json:"expected_window,omitempty"`
	ExpectedWeight  int64             `json:"expected_weight,omitempty"`
	TruthShareQ     fixedpoint.Q      `json:"truth_share_q,omitempty"`
	ComputedShareQ  fixedpoint.Q      `json:"computed_share_q,omitempty"`
	DiffQ           fixedpoint.Q      `json:"diff_q,omitempty"`
	ToleranceQ      fixedpoint.Q      `json:"tolerance_q"`
	WithinTolerance bool              `json:"within_tolerance"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// Build creates the same engineering in canonical form plus truth.
func Build(seed int64) (*Bundle, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	projectID := fmt.Sprintf("gw-%04x-%04x", rng.Intn(1<<16), rng.Intn(1<<16))

	streamDefs := []model.StreamSpec{
		{ID: 1, Name: "control", ParentID: 0, WeightPermyriad: 2000, InitialWindowBytes: 2048, MaxWindowBytes: 16384, MaxFrameBytes: 2048},
		{ID: 3, Name: "orders", ParentID: 1, WeightPermyriad: 3000, InitialWindowBytes: 1024, MaxWindowBytes: 16384, MaxFrameBytes: 1024},
		{ID: 5, Name: "media", ParentID: 1, WeightPermyriad: 2000, InitialWindowBytes: 2048, MaxWindowBytes: 16384, MaxFrameBytes: 2048},
		{ID: 7, Name: "receipts", ParentID: 3, WeightPermyriad: 1500, InitialWindowBytes: 1024, MaxWindowBytes: 16384, MaxFrameBytes: 1024},
		{ID: 9, Name: "audit", ParentID: 3, WeightPermyriad: 1500, InitialWindowBytes: 1024, MaxWindowBytes: 16384, MaxFrameBytes: 1024},
	}
	for i := range streamDefs {
		streamDefs[i].OrphanPolicy = model.OrphanReparent
		streamDefs[i].BodySegments = []int64{1024, 1024, 1024, 1024, 1024, 1024}
	}

	p := &model.Project{
		SchemaVersion:   model.SchemaV1,
		SourceVersion:   "canonical-generator",
		ProjectID:       projectID,
		Seed:            seed,
		FixedPointBits:  fixedpoint.FractionBits,
		WeightScale:     "permyriad",
		TimeBase:        model.TimeFrameSequence,
		FrameIntervalMS: 10,
		OrphanPolicy:    model.OrphanReparent,
		Connection: model.ConnectionSpec{
			ID:                 "C-deterministic-1",
			InitialWindowBytes: 6144,
			MaxWindowBytes:     65536,
		},
		Streams: streamDefs,
	}

	var events []model.Event
	add := func(typ string, sid int) *model.Event {
		ev := model.Event{Type: typ, StreamID: sid}
		events = append(events, ev)
		return &events[len(events)-1]
	}

	for _, sid := range []int{1, 3, 5, 7, 9} {
		add(model.EventHeaders, sid)
	}
	// Five synchronized DATA rounds create backlog and window attribution.
	for round := 0; round < 5; round++ {
		for _, sid := range []int{1, 3, 5, 7, 9} {
			ev := add(model.EventData, sid)
			ev.SegmentIndex = round
			ev.LengthBytes = 1024
		}
	}
	ev := add(model.EventWindowUpdate, 0)
	ev.IncrementBytes = 16384
	for _, sid := range []int{1, 3, 5, 7, 9} {
		ev := add(model.EventWindowUpdate, sid)
		ev.IncrementBytes = 8192
	}
	ev = add(model.EventWindowUpdate, 0)
	ev.IncrementBytes = 8192

	// Reset a parent so 7/9 reparent to stream 1.
	add(model.EventRSTStream, 3)

	// Sixth round excludes reset stream 3.
	for _, sid := range []int{1, 5, 7, 9} {
		ev := add(model.EventData, sid)
		ev.SegmentIndex = 5
		ev.LengthBytes = 1024
		ev.Fin = true
	}

	p.Events = events
	normalizeEvents(p)
	p.FrameWatermark = p.Events[len(p.Events)-1].FrameSeq
	p.WatermarkExplicit = true

	ref, err := engine.Run(p, engine.Options{MaxPendingFrames: 8})
	if err != nil {
		return nil, fmt.Errorf("generate reference truth: %w", err)
	}
	cases := numericalCases()
	truth := &Truth{
		Seed:               seed,
		Generator:          "deterministic-frame-constructor/v1",
		InitialWindows:     initialWindows(p),
		EventTruth:         ref.Traces,
		ScheduleTruth:      ref.Sends,
		ReferenceResult:    ref,
		NumericalCases:     cases,
		ExpectedErrorCodes: expectedCodes(),
	}

	b := &Bundle{Canonical: p, Truth: truth, NumericalCases: cases}
	if err := b.render(); err != nil {
		return nil, err
	}
	return b, nil
}

func normalizeEvents(p *model.Project) {
	for i := range p.Events {
		p.Events[i].Index = i
		p.Events[i].FrameSeq = int64(i + 1)
		p.Events[i].TimeMS = int64(i+1) * p.FrameIntervalMS
	}
}

func initialWindows(p *model.Project) map[string]int64 {
	out := map[string]int64{"connection": p.Connection.InitialWindowBytes}
	for _, s := range p.Streams {
		out[fmt.Sprintf("stream-%d", s.ID)] = s.InitialWindowBytes
	}
	return out
}

func expectedCodes() map[string]string {
	return map[string]string{
		"negative_window":      "E_WINDOW_NEGATIVE",
		"window_over_cap":      "E_WINDOW_OVER_CAP",
		"weight_out_of_range":  "E_WEIGHT_RANGE",
		"zero_weight":          "E_WEIGHT_ZERO",
		"dependency_cycle":     "E_DEPENDENCY_CYCLE",
		"frame_too_large":      "E_FRAME_TOO_LARGE",
		"budget_out_of_range":  "E_BUDGET_RANGE",
		"buffer_full":          "E_BUFFER_FULL",
		"thread_budget":        "E_THREAD_BUDGET_EXHAUSTED",
		"checkpoint_truncated": "E_CHECKPOINT_CORRUPT",
		"checkpoint_version":   "E_CHECKPOINT_VERSION",
		"incremental_mismatch": "E_INCREMENTAL_MISMATCH",
	}
}

func numericalCases() []NumericalCase {
	tol := fixedpoint.DefaultTolerance
	mkShare := func(name string, sid int, weight, total, allocated, totalBytes int64) NumericalCase {
		truth, _ := fixedpoint.Share(weight, total)
		computed, _ := fixedpoint.AllocatedShare(allocated, totalBytes)
		diff := computed - truth
		if diff < 0 {
			diff = -diff
		}
		return NumericalCase{
			Name: name, StreamID: sid, InputPermyriad: weight, TotalWeight: total,
			AllocatedBytes: allocated, TotalBytes: totalBytes,
			TruthShareQ: truth, ComputedShareQ: computed, DiffQ: diff,
			ToleranceQ: tol, WithinTolerance: diff <= tol,
		}
	}
	cases := []NumericalCase{
		{
			Name: "permille-125-to-myriad", InputPermille: 125, ExpectedWeight: 1250,
			ToleranceQ: 0, WithinTolerance: true,
		},
		{
			Name: "permille-333-to-myriad", InputPermille: 333, ExpectedWeight: 3330,
			ToleranceQ: 0, WithinTolerance: true,
		},
		mkShare("one-third-weight-share", 3, 1, 3, 21845, 65536),
		mkShare("two-third-allocation-share", 5, 2, 3, 43691, 65536),
		{
			Name: "seven-window-updates-exact", RepeatCount: 7, StartWindow: 0,
			IncrementBytes: 1024, CapWindow: 65536, ExpectedWindow: 7168,
			ToleranceQ: 0, WithinTolerance: true,
		},
		{
			Name: "one-thousand-one-byte-updates-exact", RepeatCount: 1000, StartWindow: 0,
			IncrementBytes: 1, CapWindow: fixedpoint.MaxWindow, ExpectedWindow: 1000,
			ToleranceQ: 0, WithinTolerance: true,
		},
		{
			Name: "window-update-saturation-boundary", RepeatCount: 1, StartWindow: 60000,
			IncrementBytes: 10000, CapWindow: 65535, ExpectedWindow: 65535,
			ToleranceQ: 0, WithinTolerance: true,
		},
		{
			Name: "weight-roundtrip-100-adjustments", RepeatCount: 100,
			InputPermille: 123, ExpectedWeight: 1230, ToleranceQ: 0, WithinTolerance: true,
		},
	}
	return cases
}
