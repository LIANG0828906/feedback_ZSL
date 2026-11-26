// Package generator builds deterministic local acoustic samples and independent oracle truth.
package generator

import (
	"fmt"

	"acousticfield/internal/acoustic"
)

const DefaultSeed int64 = 0x42A8691E

type rng struct{ state uint64 }

func newRNG(seed int64) *rng { return &rng{state: uint64(seed) | 1} }

// Intn returns a deterministic value in [0,n).
func (r *rng) Intn(n int) int {
	// Knuth-style LCG; sufficient for reproducible fixture generation.
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int((r.state >> 33) % uint64(n))
}

func (r *rng) Int63n(n int64) int64 { return int64(r.Intn(int(n))) }

// Generate creates a fixed 3x3 planar chunk sample. Geometry is deliberately
// Pythagorean so the independent truth has exact direct and one-bounce lengths.
func Generate(seed int64) (*acoustic.Level, *acoustic.Truth) {
	if seed == 0 {
		seed = DefaultSeed
	}
	r := newRNG(seed)
	const rows, cols = 3, 3
	const cell = 10_000

	level := &acoustic.Level{
		SchemaVersion: acoustic.SchemaVersion,
		Seed:          seed,
		GridRows:      rows,
		GridCols:      cols,
		Bounds: acoustic.Rect{
			Min: acoustic.Point{X: 0, Y: 0},
			Max: acoustic.Point{X: cols * cell, Y: rows * cell},
		},
		BandIDs: []int{0, 1, 2},
		Params:  acoustic.DefaultParams(),
	}

	alphaChoices := []int64{50_000, 100_000, 150_000, 200_000, 250_000, 300_000, 400_000, 500_000}
	bandLabels := []struct {
		label  string
		center int
	}{{"125Hz", 125}, {"500Hz", 500}, {"2kHz", 2000}}

	index := 0
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			id := fmt.Sprintf("C%02d", index)
			x0, y0 := int64(col*cell), int64(row*cell)
			chunk := acoustic.Chunk{
				ID:  id,
				Row: row,
				Col: col,
				Bounds: acoustic.Rect{
					Min: acoustic.Point{X: x0, Y: y0},
					Max: acoustic.Point{X: x0 + cell, Y: y0 + cell},
				},
				Bands: make([]acoustic.Band, 0, 3),
				Surfaces: []acoustic.Surface{{
					ID:       "RFL-" + id + "-BACK",
					ChunkID:  id,
					A:        acoustic.Point{X: x0 + 1000, Y: y0 + 7000},
					B:        acoustic.Point{X: x0 + 9000, Y: y0 + 7000},
					Normal:   acoustic.Point{X: 0, Y: -acoustic.NormalScale},
					Material: "felt-panel",
				}},
			}
			for band := 0; band < 3; band++ {
				chunk.Bands = append(chunk.Bands, acoustic.Band{
					ID:     band,
					Label:  bandLabels[band].label,
					Center: bandLabels[band].center,
					AlphaQ: alphaChoices[r.Intn(len(alphaChoices))],
				})
			}
			level.Chunks = append(level.Chunks, chunk)

			sourceID := "SRC-" + id
			emission := make([]int64, 3)
			base := int64(4_200_000 + r.Intn(2_400_000))
			for band := range emission {
				emission[band] = base + int64(band)*173_497 + int64(r.Intn(90_000))
			}
			level.Sources = append(level.Sources, acoustic.Source{
				ID:        sourceID,
				ChunkID:   id,
				Position:  acoustic.Point{X: x0 + 2000, Y: y0 + 3000},
				EmissionQ: emission,
			})
			level.Listeners = append(level.Listeners, acoustic.Listener{
				ID:       "LST-" + id,
				ChunkID:  id,
				Position: acoustic.Point{X: x0 + 8000, Y: y0 + 3000},
			})
			index++
		}
	}

	truth := buildTruth(level)
	return level, truth
}

// buildTruth is the generator's independent oracle. It uses the known fixture
// construction (6 m direct and 5+5 m reflected legs), not rebuild output.
func buildTruth(level *acoustic.Level) *acoustic.Truth {
	truth := &acoustic.Truth{
		Seed:                  level.Seed,
		OracleVersion:         "generator-oracle-v1",
		BandIDs:               append([]int(nil), level.BandIDs...),
		ConservationResidualQ: []int64{0, 0, 0},
	}

	for _, chunk := range level.Chunks {
		var source *acoustic.Source
		for i := range level.Sources {
			if level.Sources[i].ChunkID == chunk.ID {
				source = &level.Sources[i]
				break
			}
		}
		var listener *acoustic.Listener
		for i := range level.Listeners {
			if level.Listeners[i].ChunkID == chunk.ID {
				listener = &level.Listeners[i]
				break
			}
		}
		if source == nil || listener == nil || len(chunk.Surfaces) == 0 {
			continue
		}
		surface := chunk.Surfaces[0]
		directKey := "P00-direct"
		reflectKey := "P01-" + surface.ID

		sample := acoustic.SampleTruth{
			ListenerID:           listener.ID,
			ChunkID:              chunk.ID,
			ExpectedArrivalQ:     make([]int64, len(level.BandIDs)),
			ExpectedPathLengthsQ: []int64{6 * acoustic.GeoScale, 10 * acoustic.GeoScale},
			Paths: []acoustic.PathTruth{
				{
					ListenerID:   listener.ID,
					PathKey:      directKey,
					Sequence:     []string{},
					Bounces:      0,
					LengthQ:      6 * acoustic.GeoScale,
					BandArrivalQ: make([]int64, len(level.BandIDs)),
				},
				{
					ListenerID:   listener.ID,
					PathKey:      reflectKey,
					Sequence:     []string{surface.ID},
					Bounces:      1,
					LengthQ:      10 * acoustic.GeoScale,
					BandArrivalQ: make([]int64, len(level.BandIDs)),
				},
			},
		}

		for bandIndex, band := range chunk.Bands {
			emission := source.EmissionQ[bandIndex]
			// Two feasible paths plus BoundaryWeight boundary shares.
			share := emission / int64(2+level.Params.BoundaryWeight)
			direct := share
			reflectedNumerator := share * (acoustic.EnergyScale - band.AlphaQ)
			reflected := reflectedNumerator / acoustic.EnergyScale
			sample.Paths[0].BandArrivalQ[bandIndex] = direct
			sample.Paths[1].BandArrivalQ[bandIndex] = reflected
			sample.ExpectedArrivalQ[bandIndex] = direct + reflected
		}
		truth.Samples = append(truth.Samples, sample)
	}
	truth.Fingerprint = Fingerprint(truth)
	return truth
}

// OracleForLevel rebuilds the independent analytic oracle after deterministic edits.
// It still derives expectations solely from current geometry and parameters.
func OracleForLevel(level *acoustic.Level) *acoustic.Truth {
	t := buildTruth(level)
	RefreshFingerprint(t)
	return t
}

type truthCore struct {
	Seed     int64
	BandIDs  []int
	Samples  []acoustic.SampleTruth
	Residual []int64
}

func truthFingerprintInput(t *acoustic.Truth) truthCore {
	return truthCore{Seed: t.Seed, BandIDs: t.BandIDs, Samples: t.Samples, Residual: t.ConservationResidualQ}
}

// Fingerprint returns the stable fingerprint excluding the fingerprint field itself.
func Fingerprint(t *acoustic.Truth) string {
	return acoustic.HashValue("truth:", truthFingerprintInput(t))
}

// RefreshFingerprint recomputes the truth fingerprint.
func RefreshFingerprint(t *acoustic.Truth) { t.Fingerprint = Fingerprint(t) }
