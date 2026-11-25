package acousticfield

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestGeneratedSampleValidatesAndMatchesTruth(t *testing.T) {
	sample, err := Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLevel(sample.Level); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	results, plan, err := SolveLevel(context.Background(), sample.Level, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckConservation(sample.Level, opts, results, plan); err != nil {
		t.Fatal(err)
	}
	byChunk := make(map[int]ChunkResult, len(results))
	for _, res := range results {
		byChunk[res.ChunkID] = res
	}
	for _, tc := range sample.Truth.Chunks {
		res := byChunk[tc.ChunkID]
		for _, want := range tc.Energy {
			if got := bandValue(res.ArrivalEnergy, want.BandID); got != want.Energy {
				t.Fatalf("chunk %d band %s energy got %d want %d", tc.ChunkID, want.BandID, got, want.Energy)
			}
		}
	}
	truthPaths := make(map[string]TruthPath)
	for _, p := range sample.Truth.Paths {
		truthPaths[p.PathID] = p
	}
	for _, res := range results {
		if len(res.Paths) == 0 {
			t.Fatalf("chunk %d has no paths", res.ChunkID)
		}
		for _, p := range res.Paths {
			tp, ok := truthPaths[p.ID]
			if !ok {
				t.Fatalf("path %s not in truth", p.ID)
			}
			diff := tp.LengthMicros - p.LengthMicros
			if diff < 0 {
				diff = -diff
			}
			if diff > opts.PathToleranceMicros {
				t.Fatalf("path %s length got %d want %d", p.ID, p.LengthMicros, tp.LengthMicros)
			}
		}
	}
	listenerTruth := make(map[string]TruthListener)
	for _, l := range sample.Truth.Listeners {
		listenerTruth[l.ListenerID] = l
	}
	for _, res := range results {
		listener, ok := listenerByChunk(sample.Level, res.ChunkID)
		if !ok {
			t.Fatal("listener missing")
		}
		if got, want := sumBandEnergy(res.ArrivalEnergy), listenerTruth[listener.ID].TotalEnergy; got != want {
			t.Fatalf("listener %s total got %d want %d", listener.ID, got, want)
		}
	}

	results2, _, err := SolveLevel(context.Background(), sample.Level, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range results {
		if got, want := resultFieldDigest(results[i]), resultFieldDigest(results2[i]); got != want {
			t.Fatalf("non-deterministic result for chunk %d", results[i].ChunkID)
		}
	}
}

func TestTruthFingerprintStable(t *testing.T) {
	a, err := Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	if a.Truth.Fingerprint != b.Truth.Fingerprint {
		t.Fatal("truth fingerprint changed for fixed seed")
	}
	c, err := Generate(8)
	if err != nil {
		t.Fatal(err)
	}
	if a.Truth.Fingerprint == c.Truth.Fingerprint {
		t.Fatal("different seeds produced the same truth fingerprint")
	}
}

func TestRotatedReflectorUsesGenericFixedPointIntersection(t *testing.T) {
	sample, err := Generate(77)
	if err != nil {
		t.Fatal(err)
	}
	level := CloneLevel(sample.Level)
	c, _ := chunkByID(level, 3)
	a := Point{X: 6_200_000, Y: 200_000}
	b := Point{X: 6_800_000, Y: 1_800_000}
	dx := float64(b.X - a.X)
	dy := float64(b.Y - a.Y)
	length := math.Hypot(dx, dy)
	nx := int64(math.Round(dy / length * float64(Q16)))
	ny := int64(math.Round(-dx / length * float64(Q16)))
	for i := range c.Reflectors {
		if c.Reflectors[i].ID == fmt.Sprintf("c%03d-left", c.ID) {
			c.Reflectors[i].A = a
			c.Reflectors[i].B = b
			c.Reflectors[i].Normal = Vec{X: nx, Y: ny}
		}
	}
	if err := ValidateLevel(level); err != nil {
		t.Fatal(err)
	}
	src, _ := sourceByChunk(level, 3)
	lis, _ := listenerByChunk(level, 3)
	paths, _, err := enumerateSolverPaths(c, src.Position, lis.Point, DefaultOptions(), 64)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range paths {
		if len(p.Reflectors) == 1 && p.Reflectors[0] == c.Reflectors[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("rotated reflector produced no single-reflection path; paths=%+v", paths)
	}
	analytic := analyticChunkPaths(*c, src.Position, lis.Point, DefaultOptions().MaxReflections, 64)
	if len(analytic) != len(paths) {
		t.Fatalf("analytic paths=%d solver paths=%d", len(analytic), len(paths))
	}
}

func TestConservationMismatchReturnsEConsWithValues(t *testing.T) {
	sample, err := Generate(88)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	results, plan, err := SolveLevel(context.Background(), sample.Level, opts)
	if err != nil {
		t.Fatal(err)
	}
	results[3].SourceEnergy[0].Energy[0].Energy += opts.ConservationResidue + 1
	err = CheckConservation(sample.Level, opts, results, plan)
	if !IsCode(err, CodeCons) {
		t.Fatalf("expected E_CONS, got %v", err)
	}
	var fe *FieldError
	if !errors.As(err, &fe) || fe.BandID == "" || fe.SourceID == "" || fe.Values["emission"] == 0 {
		t.Fatalf("conservation error context missing: %v", err)
	}
}
