// Package generator creates the deterministic local acoustic sample and its
// analytic truth manifest. Truth geometry uses an independent float64 mirror
// implementation rather than calling the fixed-point solver.
package generator

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"roomacoustics/pkg/acoustics"
)

const DefaultSeed int64 = 250250

type Spec struct {
	Level *acoustics.Level
	Truth *acoustics.TruthFile
}

func GenerateFiles(dir string, seed int64) (*Spec, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	spec, err := Generate(seed)
	if err != nil {
		return nil, err
	}
	if err := acoustics.WriteJSONFile(filepath.Join(dir, "level.json"), spec.Level); err != nil {
		return nil, err
	}
	if err := acoustics.WriteJSONFile(filepath.Join(dir, "truth.json"), spec.Truth); err != nil {
		return nil, err
	}
	return spec, nil
}

func Generate(seed int64) (*Spec, error) {
	level := newLevel(seed)
	if err := acoustics.Validate(level); err != nil {
		return nil, err
	}
	truth, err := BuildTruth(level)
	if err != nil {
		return nil, err
	}
	if err := truth.MatchesListeners(level); err != nil {
		return nil, err
	}
	return &Spec{Level: level, Truth: truth}, nil
}

func newLevel(seed int64) *acoustics.Level {
	rng := rand.New(rand.NewSource(seed))
	l := &acoustics.Level{
		SchemaVersion: acoustics.LevelSchemaVersion,
		Seed:          seed,
		Units:         "m",
		Params:        acoustics.DefaultParams(),
		Rooms: []acoustics.Room{
			{ID: "R1", Rect: acoustics.Rect{X1: 0, Y1: 0, X2: 10, Y2: 8}},
			{ID: "R2", Rect: acoustics.Rect{X1: 10, Y1: 0, X2: 22, Y2: 8}},
			{ID: "R3", Rect: acoustics.Rect{X1: 22, Y1: 0, X2: 32, Y2: 8}},
			{ID: "R4", Rect: acoustics.Rect{X1: 11, Y1: 8, X2: 21, Y2: 14}},
		},
		Walls: []acoustics.Wall{
			{ID: "W12", RoomA: "R1", RoomB: "R2", A: p(10, 0), B: p(10, 8)},
			{ID: "W23", RoomA: "R2", RoomB: "R3", A: p(22, 0), B: p(22, 8)},
			{ID: "W24", RoomA: "R2", RoomB: "R4", A: p(11, 8), B: p(21, 8)},
		},
		Doors: []acoustics.Door{
			{ID: "D12", WallID: "W12", Center: p(10, 4.0), Width: 1.6},
			{ID: "D23", WallID: "W23", Center: p(22, 4.0), Width: 1.6},
			{ID: "D24", WallID: "W24", Center: p(16, 8.0), Width: 1.8},
		},
		Sources: []acoustics.Source{
			{ID: "SRC-R1", RoomID: "R1", Position: p(3.0, 2.0), PowerDB: 98},
			{ID: "SRC-R2", RoomID: "R2", Position: p(15.0, 2.2), PowerDB: 98},
			{ID: "SRC-R3", RoomID: "R3", Position: p(27.0, 2.0), PowerDB: 98},
			{ID: "SRC-R4", RoomID: "R4", Position: p(16.0, 10.0), PowerDB: 96},
		},
		Listeners: []acoustics.Listener{
			{ID: "LIS-R1-A", RoomID: "R1", Position: p(7.0, 6.0)},
			{ID: "LIS-R1-B", RoomID: "R1", Position: p(5.4, 5.1)},
			{ID: "LIS-R2-A", RoomID: "R2", Position: p(18.0, 6.0)},
			{ID: "LIS-R2-B", RoomID: "R2", Position: p(14.2, 5.4)},
			{ID: "LIS-R3-A", RoomID: "R3", Position: p(29.0, 6.0)},
			{ID: "LIS-R4-A", RoomID: "R4", Position: p(16.0, 12.6)},
		},
	}

	type rfSpec struct {
		id, room string
		a, b, n  acoustics.Point
	}
	specs := []rfSpec{
		{"RF-R1-B", "R1", p(1, 0), p(9, 0), p(0, 1)},
		{"RF-R1-T", "R1", p(1, 8), p(9, 8), p(0, -1)},
		{"RF-R1-L", "R1", p(0, 1), p(0, 7), p(1, 0)},
		{"RF-R1-RL", "R1", p(10, 0.5), p(10, 2.7), p(-1, 0)},
		{"RF-R1-RH", "R1", p(10, 5.3), p(10, 7.5), p(-1, 0)},

		{"RF-R2-B", "R2", p(11, 0), p(21, 0), p(0, 1)},
		{"RF-R2-TL", "R2", p(11.2, 8), p(14.6, 8), p(0, -1)},
		{"RF-R2-TR", "R2", p(17.4, 8), p(20.8, 8), p(0, -1)},
		{"RF-R2-LL", "R2", p(10, 0.5), p(10, 2.7), p(1, 0)},
		{"RF-R2-LH", "R2", p(10, 5.3), p(10, 7.5), p(1, 0)},
		{"RF-R2-RL", "R2", p(22, 0.5), p(22, 2.7), p(-1, 0)},
		{"RF-R2-RH", "R2", p(22, 5.3), p(22, 7.5), p(-1, 0)},

		{"RF-R3-B", "R3", p(23, 0), p(31, 0), p(0, 1)},
		{"RF-R3-T", "R3", p(23, 8), p(31, 8), p(0, -1)},
		{"RF-R3-R", "R3", p(32, 1), p(32, 7), p(-1, 0)},
		{"RF-R3-LL", "R3", p(22, 0.5), p(22, 2.7), p(1, 0)},
		{"RF-R3-LH", "R3", p(22, 5.3), p(22, 7.5), p(1, 0)},

		{"RF-R4-BL", "R4", p(11.3, 8), p(14.7, 8), p(0, 1)},
		{"RF-R4-BR", "R4", p(17.3, 8), p(20.7, 8), p(0, 1)},
		{"RF-R4-T", "R4", p(12, 14), p(20, 14), p(0, -1)},
		{"RF-R4-L", "R4", p(11, 9), p(11, 13), p(1, 0)},
		{"RF-R4-R", "R4", p(21, 9), p(21, 13), p(-1, 0)},
	}
	for i, s := range specs {
		alpha := math.Round((0.16+rng.Float64()*0.38)*1000) / 1000
		l.Reflectors = append(l.Reflectors, acoustics.Reflector{
			ID: s.id, RoomID: s.room, A: s.a, B: s.b, Normal: s.n,
			Absorption: alpha, Description: "deterministic reflector " + string(rune('A'+i%26)),
		})
	}
	return l
}

func p(x, y float64) acoustics.Point { return acoustics.Point{X: x, Y: y} }

func sortedSources(l *acoustics.Level) []acoustics.Source {
	out := append([]acoustics.Source{}, l.Sources...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedListeners(l *acoustics.Level) []acoustics.Listener {
	out := append([]acoustics.Listener{}, l.Listeners...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
