// Package schema loads and validates acoustic engineering documents.
package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"acousticfield/internal/acoustic"
)

// LoadLevel decodes JSON and performs E_SCHEMA validation.
func LoadLevel(data []byte) (*acoustic.Level, error) {
	var level acoustic.Level
	if err := json.Unmarshal(data, &level); err != nil {
		return nil, acoustic.SchemaError("level", "json", "invalid JSON: %v", err)
	}
	NormalizeParams(&level.Params)
	if err := ValidateLevel(&level); err != nil {
		return nil, err
	}
	return &level, nil
}

func NormalizeParams(p *acoustic.Params) {
	d := acoustic.DefaultParams()
	if p.MaxBounces <= 0 {
		p.MaxBounces = d.MaxBounces
	}
	if p.BoundaryWeight <= 0 {
		p.BoundaryWeight = d.BoundaryWeight
	}
	if p.BoundaryTransmissionPermille <= 0 {
		p.BoundaryTransmissionPermille = d.BoundaryTransmissionPermille
	}
	if p.ConservationToleranceQ <= 0 {
		p.ConservationToleranceQ = d.ConservationToleranceQ
	}
	if p.AlgorithmVersion == "" {
		p.AlgorithmVersion = acoustic.AlgorithmVersion
	}
	if p.Budget.MaxParallel <= 0 {
		p.Budget.MaxParallel = d.Budget.MaxParallel
	}
	if p.Budget.PathBufferPerWorker <= 0 {
		p.Budget.PathBufferPerWorker = d.Budget.PathBufferPerWorker
	}
	if p.Budget.MaxQueuedChunks <= 0 {
		p.Budget.MaxQueuedChunks = d.Budget.MaxQueuedChunks
	}
	p.ParamVersion = p.Version()
}

func ValidateLevel(level *acoustic.Level) error {
	if level == nil {
		return acoustic.SchemaError("level", "", "document is null")
	}
	if level.SchemaVersion == "" {
		return acoustic.SchemaError("level", "schema_version", "field is missing")
	}
	if level.SchemaVersion != acoustic.SchemaVersion {
		return acoustic.SchemaError("level", "schema_version", "unsupported schema %q", level.SchemaVersion)
	}
	if len(level.BandIDs) == 0 {
		return acoustic.SchemaError("level", "band_ids", "field is missing or empty")
	}
	if err := validateContinuousIDs("level", "band_ids", level.BandIDs); err != nil {
		return err
	}
	if level.Bounds.Max.X <= level.Bounds.Min.X || level.Bounds.Max.Y <= level.Bounds.Min.Y {
		return acoustic.SchemaError("level", "bounds", "invalid level rectangle")
	}
	if len(level.Chunks) == 0 {
		return acoustic.SchemaError("level", "chunks", "field is missing or empty")
	}
	if err := validateParams("level", &level.Params); err != nil {
		return err
	}

	chunkByID := map[string]*acoustic.Chunk{}
	for i := range level.Chunks {
		c := &level.Chunks[i]
		if c.ID == "" {
			return acoustic.SchemaError(fmt.Sprintf("chunks[%d]", i), "id", "field is missing")
		}
		if _, exists := chunkByID[c.ID]; exists {
			return acoustic.SchemaError(c.ID, "id", "duplicate chunk id")
		}
		chunkByID[c.ID] = c
		if err := validateChunk(level, c); err != nil {
			return err
		}
	}
	if err := validateTiling(level); err != nil {
		return err
	}
	if err := validateSourcesListeners(level, chunkByID); err != nil {
		return err
	}
	return nil
}

func validateParams(objectID string, p *acoustic.Params) error {
	if p.MaxBounces < 0 || p.MaxBounces > 8 {
		return acoustic.SchemaError(objectID, "params.max_bounces", "must be in [0,8]")
	}
	if p.BoundaryWeight < 1 {
		return acoustic.SchemaError(objectID, "params.boundary_weight", "must be positive")
	}
	if p.BoundaryTransmissionPermille <= 0 || p.BoundaryTransmissionPermille > 1000 {
		return acoustic.SchemaError(objectID, "params.boundary_transmission_permille", "must be in (0,1000]")
	}
	if p.ConservationToleranceQ < 0 {
		return acoustic.SchemaError(objectID, "params.conservation_tolerance_q", "must be non-negative")
	}
	if p.Budget.MaxParallel < 1 || p.Budget.MaxParallel > 64 {
		return acoustic.SchemaError(objectID, "params.budget.max_parallel", "must be in [1,64]")
	}
	if p.Budget.PathBufferPerWorker < 1 {
		return acoustic.SchemaError(objectID, "params.budget.path_buffer_per_worker", "must be positive")
	}
	if p.Budget.MaxQueuedChunks < 1 {
		return acoustic.SchemaError(objectID, "params.budget.max_queued_chunks", "must be positive")
	}
	return nil
}

func validateContinuousIDs(objectID, field string, ids []int) error {
	for i, id := range ids {
		if id != i {
			return acoustic.SchemaError(objectID, field, "band ids must be continuous from 0; index %d has %d", i, id)
		}
	}
	return nil
}

func validateChunk(level *acoustic.Level, c *acoustic.Chunk) error {
	if c.Bounds.Max.X <= c.Bounds.Min.X || c.Bounds.Max.Y <= c.Bounds.Min.Y {
		return acoustic.SchemaError(c.ID, "bounds", "invalid or missing rectangle")
	}
	if c.Bounds.Min.X < level.Bounds.Min.X || c.Bounds.Max.X > level.Bounds.Max.X ||
		c.Bounds.Min.Y < level.Bounds.Min.Y || c.Bounds.Max.Y > level.Bounds.Max.Y {
		return acoustic.SchemaError(c.ID, "bounds", "chunk is outside level bounds")
	}
	if len(c.Bands) != len(level.BandIDs) {
		return acoustic.SchemaError(c.ID, "bands", "expected %d bands, got %d", len(level.BandIDs), len(c.Bands))
	}
	ids := make([]int, 0, len(c.Bands))
	for i, b := range c.Bands {
		if b.ID != i || b.ID != level.BandIDs[i] {
			return acoustic.SchemaError(c.ID, fmt.Sprintf("bands[%d].id", i), "band id is not continuous/matching level table")
		}
		ids = append(ids, b.ID)
		if b.Label == "" {
			return acoustic.SchemaError(c.ID, fmt.Sprintf("bands[%d].label", i), "field is missing")
		}
		if b.Center <= 0 {
			return acoustic.SchemaError(c.ID, fmt.Sprintf("bands[%d].center_hz", i), "must be positive")
		}
		if !acoustic.IsQuantumAligned(b.AlphaQ, acoustic.AbsorptionQuantum) {
			return acoustic.SchemaError(c.ID, fmt.Sprintf("bands[%d].alpha_q", i), "absorption must be positive and a multiple of %d", acoustic.AbsorptionQuantum)
		}
		if b.AlphaQ > acoustic.EnergyScale {
			return acoustic.SchemaError(c.ID, fmt.Sprintf("bands[%d].alpha_q", i), "absorption must not exceed %d", acoustic.EnergyScale)
		}
	}
	if err := validateContinuousIDs(c.ID, "bands", ids); err != nil {
		return err
	}

	surfaceIDs := map[string]bool{}
	for i, s := range c.Surfaces {
		id := s.ID
		if id == "" {
			id = c.ID + ".surfaces"
			return acoustic.SchemaError(id, fmt.Sprintf("surfaces[%d].id", i), "field is missing")
		}
		if surfaceIDs[s.ID] {
			return acoustic.SchemaError(s.ID, fmt.Sprintf("surfaces[%d].id", i), "duplicate surface id")
		}
		surfaceIDs[s.ID] = true
		if s.A == (acoustic.Point{}) && s.B == (acoustic.Point{}) {
			return acoustic.SchemaError(s.ID, fmt.Sprintf("surfaces[%d].a", i), "endpoints are missing")
		}
		if s.A.X == s.B.X && s.A.Y == s.B.Y {
			return acoustic.SchemaError(s.ID, fmt.Sprintf("surfaces[%d].b", i), "surface endpoints must differ")
		}
		if !c.Bounds.Contains(s.A) || !c.Bounds.Contains(s.B) {
			return acoustic.SchemaError(s.ID, fmt.Sprintf("surfaces[%d].a", i), "surface endpoint is outside chunk bounds")
		}
		nx := float64(s.Normal.X) / float64(acoustic.NormalScale)
		ny := float64(s.Normal.Y) / float64(acoustic.NormalScale)
		norm := math.Hypot(nx, ny)
		if math.Abs(norm-1.0) > 2e-6 {
			return acoustic.SchemaError(s.ID, fmt.Sprintf("surfaces[%d].normal_q", i), "normal is not normalized: norm=%.9f", norm)
		}
	}
	return nil
}

func validateTiling(level *acoustic.Level) error {
	for i := 0; i < len(level.Chunks); i++ {
		for j := i + 1; j < len(level.Chunks); j++ {
			a, b := level.Chunks[i].Bounds, level.Chunks[j].Bounds
			if a.IntersectsInterior(b) {
				return acoustic.SchemaError(level.Chunks[j].ID, "bounds", "chunk interior overlaps %s", level.Chunks[i].ID)
			}
		}
	}

	xs := []int64{level.Bounds.Min.X, level.Bounds.Max.X}
	ys := []int64{level.Bounds.Min.Y, level.Bounds.Max.Y}
	for _, c := range level.Chunks {
		xs = append(xs, c.Bounds.Min.X, c.Bounds.Max.X)
		ys = append(ys, c.Bounds.Min.Y, c.Bounds.Max.Y)
	}
	xs = uniqueSorted(xs)
	ys = uniqueSorted(ys)

	for xi := 0; xi+1 < len(xs); xi++ {
		for yi := 0; yi+1 < len(ys); yi++ {
			x0, x1 := xs[xi], xs[xi+1]
			y0, y1 := ys[yi], ys[yi+1]
			if x0 < level.Bounds.Min.X || x1 > level.Bounds.Max.X || y0 < level.Bounds.Min.Y || y1 > level.Bounds.Max.Y {
				continue
			}
			covered := false
			for _, c := range level.Chunks {
				if c.Bounds.Min.X <= x0 && c.Bounds.Max.X >= x1 &&
					c.Bounds.Min.Y <= y0 && c.Bounds.Max.Y >= y1 {
					if covered {
						return acoustic.SchemaError(c.ID, "bounds", "elementary tile is covered more than once")
					}
					covered = true
				}
			}
			if !covered {
				return acoustic.SchemaError("level", "chunks", "chunk union does not equal level bounds near (%d,%d)", x0, y0)
			}
		}
	}
	return nil
}

func uniqueSorted(in []int64) []int64 {
	sort.Slice(in, func(i, j int) bool { return in[i] < in[j] })
	out := in[:0]
	var last int64
	for i, v := range in {
		if i == 0 || v != last {
			out = append(out, v)
		}
		last = v
	}
	return out
}

func validateSourcesListeners(level *acoustic.Level, chunks map[string]*acoustic.Chunk) error {
	sourceIDs := map[string]bool{}
	for i, s := range level.Sources {
		if s.ID == "" {
			return acoustic.SchemaError(fmt.Sprintf("sources[%d]", i), "id", "field is missing")
		}
		if sourceIDs[s.ID] {
			return acoustic.SchemaError(s.ID, "id", "duplicate source id")
		}
		sourceIDs[s.ID] = true
		c, ok := chunks[s.ChunkID]
		if !ok {
			return acoustic.SchemaError(s.ID, "chunk_id", "unknown chunk %q", s.ChunkID)
		}
		if !c.Bounds.Contains(s.Position) {
			return acoustic.SchemaError(s.ID, "position", "source position is outside chunk %s", c.ID)
		}
		if len(s.EmissionQ) != len(level.BandIDs) {
			return acoustic.SchemaError(s.ID, "emission_q", "expected %d band energies", len(level.BandIDs))
		}
		for b, e := range s.EmissionQ {
			if e <= 0 {
				return acoustic.SchemaError(s.ID, "emission_q", "band %d energy must be positive", b)
			}
		}
	}
	listenerIDs := map[string]bool{}
	for i, l := range level.Listeners {
		if l.ID == "" {
			return acoustic.SchemaError(fmt.Sprintf("listeners[%d]", i), "id", "field is missing")
		}
		if listenerIDs[l.ID] {
			return acoustic.SchemaError(l.ID, "id", "duplicate listener id")
		}
		listenerIDs[l.ID] = true
		c, ok := chunks[l.ChunkID]
		if !ok {
			return acoustic.SchemaError(l.ID, "chunk_id", "unknown chunk %q", l.ChunkID)
		}
		if !c.Bounds.Contains(l.Position) {
			return acoustic.SchemaError(l.ID, "position", "listener position is outside chunk %s", c.ID)
		}
	}
	return nil
}
