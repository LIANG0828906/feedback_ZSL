package wb

import "math/rand"

const DefaultSeed int64 = 26031125

// GenerateSample builds the deterministic four-room teaching level. The
// geometry is hand-authored so every chain and occlusion is reviewable; the
// fixed-seed RNG only varies small source-power offsets.
func GenerateSample(seed int64) *Sample {
	rng := rand.New(rand.NewSource(seed))

	power := func(base int) Fixed {
		return Fixed(base*1000 + rng.Intn(1500))
	}

	s := &Sample{
		Seed:   seed,
		Name:   "shared-wall-four-room-suite",
		Bounds: Rect{X1: 0, Y1: 0, X2: 40, Y2: 24},
		Precision: PrecisionSpec{
			Scale:            FixedScale,
			SampleCount:      41,
			DeltaTMS:         50,
			ToleranceDB:      50, // 0.05 dB declared tolerance
			MaxParallel:      2,
			BufferSampleCap:  16,
			PathAttenPerUnit: 60, // 0.060 dB per Manhattan unit
			OcclusionAttenDB: 12000,
			EarlyBoostMaxDB:  2500,
			EarlyWindowMS:    80,
		},
		Rooms: []Room{
			{ID: "R1", Rect: Rect{0, 0, 20, 12}, Params: RoomParams{1200, 550, 1500}},
			{ID: "R2", Rect: Rect{20, 0, 40, 12}, Params: RoomParams{1500, 700, -500}},
			{ID: "R3", Rect: Rect{0, 12, 20, 24}, Params: RoomParams{1800, 400, 2000}},
			{ID: "R4", Rect: Rect{20, 12, 40, 24}, Params: RoomParams{1000, 800, 0}},
		},
		Walls: []Wall{
			{ID: "W_B1", X1: 0, Y1: 0, X2: 20, Y2: 0, RoomA: "R1"},
			{ID: "W_B2", X1: 20, Y1: 0, X2: 40, Y2: 0, RoomA: "R2"},
			{ID: "W_L1", X1: 0, Y1: 0, X2: 0, Y2: 12, RoomA: "R1"},
			{ID: "W_L3", X1: 0, Y1: 12, X2: 0, Y2: 24, RoomA: "R3"},
			{ID: "W_T1", X1: 0, Y1: 24, X2: 20, Y2: 24, RoomA: "R3"},
			{ID: "W_T4", X1: 20, Y1: 24, X2: 40, Y2: 24, RoomA: "R4"},
			{ID: "W_R2", X1: 40, Y1: 0, X2: 40, Y2: 12, RoomA: "R2"},
			{ID: "W_R4", X1: 40, Y1: 12, X2: 40, Y2: 24, RoomA: "R4"},

			{ID: "W_V1", X1: 20, Y1: 0, X2: 20, Y2: 12, RoomA: "R1", RoomB: "R2"},
			{ID: "W_V2", X1: 20, Y1: 12, X2: 20, Y2: 24, RoomA: "R3", RoomB: "R4"},
			{ID: "W_H1", X1: 0, Y1: 12, X2: 20, Y2: 12, RoomA: "R1", RoomB: "R3"},
			{ID: "W_H2", X1: 20, Y1: 12, X2: 40, Y2: 12, RoomA: "R2", RoomB: "R4"},
		},
		Openings: []Opening{
			{ID: "D1", WallID: "W_V1", Kind: "door", X1: 20, Y1: 4, X2: 20, Y2: 8, RoomA: "R1", RoomB: "R2"},
			{ID: "D2", WallID: "W_H1", Kind: "door", X1: 8, Y1: 12, X2: 10, Y2: 12, RoomA: "R1", RoomB: "R3"},
			{ID: "D3", WallID: "W_H2", Kind: "door", X1: 28, Y1: 12, X2: 32, Y2: 12, RoomA: "R2", RoomB: "R4"},
			{ID: "O_EXT", WallID: "W_T1", Kind: "opening", X1: 4, Y1: 24, X2: 8, Y2: 24, RoomA: "R3"},
		},
		Sources: []Source{
			{ID: "S1", X: 6, Y: 4, PowerDB: power(96)},
			{ID: "S2", X: 33, Y: 5, PowerDB: power(95)},
			{ID: "S3", X: 7, Y: 20, PowerDB: power(94)},
		},
		Listeners: []Listener{
			{ID: "L1", X: 15, Y: 9},
			{ID: "L2", X: 26, Y: 6},
			{ID: "L3", X: 10, Y: 19},
			{ID: "L4", X: 30, Y: 19},
		},
	}

	s.Truth = BuildAnalyticTruth(s)
	return s
}
