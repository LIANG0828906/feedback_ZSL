package pipeline

import (
	"sort"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

// Cache is the persistent per-zone result cache backed by SQLite.
type Cache interface {
	Get(zoneID int) (model.ZoneResult, bool)
	Put(seq int, result model.ZoneResult) error
}

// RunOutcome is the result of an incremental or full rebuild.
type RunOutcome struct {
	Results       []model.ZoneResult
	Invalidations []model.InvalidationEntry
	HitSequence   []string
}

// FullRebuild recomputes every zone from an empty cache and stores each result
// atomically in ascending zone order.
func (e *Engine) FullRebuild(cache Cache) (RunOutcome, error) {
	zoneIDs := make([]int, 0, len(e.region.Zones))
	for _, zone := range e.region.Zones {
		zoneIDs = append(zoneIDs, zone.ID)
	}
	sort.Ints(zoneIDs)

	outcome := RunOutcome{
		Invalidations: make([]model.InvalidationEntry, 0, len(zoneIDs)),
		HitSequence:   make([]string, 0, len(zoneIDs)),
	}
	for seq, zoneID := range zoneIDs {
		result, err := e.ComputeZone(zoneID)
		if err != nil {
			return outcome, err
		}
		if err := cache.Put(seq, result); err != nil {
			return outcome, err
		}
		outcome.Results = append(outcome.Results, result)
		outcome.HitSequence = append(outcome.HitSequence, "miss")
		outcome.Invalidations = append(outcome.Invalidations, model.InvalidationEntry{
			ZoneID: zoneID, Reason: "full rebuild", Reused: false,
		})
	}
	return outcome, nil
}

// IncrementalRebuild reuses unchanged zones from the cache and recomputes only
// the zones reachable from changes, preserving topological (ascending id) order
// and the stable per-zone commit sequence.
func (e *Engine) IncrementalRebuild(cache Cache, changes []Change) (RunOutcome, error) {
	graph := BuildGraph(e.region)
	affected, entries, err := graph.PlanInvalidation(changes)
	if err != nil {
		return RunOutcome{}, err
	}

	zoneIDs := append([]int(nil), graph.ZoneIDs...)
	outcome := RunOutcome{Invalidations: entries, HitSequence: make([]string, 0, len(zoneIDs))}
	for seq, zoneID := range zoneIDs {
		if _, invalid := affected[zoneID]; invalid {
			result, err := e.ComputeZone(zoneID)
			if err != nil {
				return outcome, err
			}
			if err := cache.Put(seq, result); err != nil {
				return outcome, err
			}
			outcome.Results = append(outcome.Results, result)
			outcome.HitSequence = append(outcome.HitSequence, "miss")
			continue
		}
		cached, ok := cache.Get(zoneID)
		if !ok {
			return outcome, errors.WithContext(errors.EDep,
				map[string]any{"zoneId": zoneID},
				"zone %d expected to reuse cache but no checkpoint found", zoneID)
		}
		outcome.Results = append(outcome.Results, cached)
		outcome.HitSequence = append(outcome.HitSequence, "hit")
	}
	return outcome, nil
}

// Diff is the first mismatch located by CompareOutcomes, carrying both sides.
type Diff struct {
	Kind   string `json:"kind"`
	ZoneID int    `json:"zoneId"`
	Detail string `json:"detail"`
	Left   any    `json:"left"`
	Right  any    `json:"right"`
}

// CompareOutcomes verifies point sets and heat pixels element by element and
// returns E_DIFF with the first mismatch when they differ. The cache hit
// sequence is intentionally excluded here: an incremental run legitimately
// records "hit" where a from-scratch full rebuild records "miss". Hit sequence
// determinism is verified separately by CompareHitSequence across repeated runs
// of the same mode.
func CompareOutcomes(a, b RunOutcome) (*Diff, error) {
	if len(a.Results) != len(b.Results) {
		return &Diff{Kind: "zone-count", Left: len(a.Results), Right: len(b.Results)},
			errors.New(errors.EDiff, "zone result count differs: %d vs %d", len(a.Results), len(b.Results))
	}
	for i := range a.Results {
		ra, rb := a.Results[i], b.Results[i]
		if ra.ZoneID != rb.ZoneID {
			return &Diff{Kind: "zone-order", ZoneID: ra.ZoneID, Left: ra.ZoneID, Right: rb.ZoneID},
				errors.New(errors.EDiff, "zone order differs at position %d", i)
		}
		if d := diffPoints(ra, rb); d != nil {
			return d, errors.WithContext(errors.EDiff,
				map[string]any{"zoneId": ra.ZoneID}, "%s", d.Detail)
		}
		if d := diffHeat(ra, rb); d != nil {
			return d, errors.WithContext(errors.EDiff,
				map[string]any{"zoneId": ra.ZoneID}, "%s", d.Detail)
		}
	}
	return nil, nil
}

// CompareHitSequence verifies that two runs of the same mode produce exactly
// the same per-zone cache decision sequence, returning E_DIFF on first mismatch.
func CompareHitSequence(a, b RunOutcome) error {
	if len(a.HitSequence) != len(b.HitSequence) {
		return errors.New(errors.EDiff, "hit sequence length differs: %d vs %d",
			len(a.HitSequence), len(b.HitSequence))
	}
	for i := range a.HitSequence {
		if a.HitSequence[i] != b.HitSequence[i] {
			return errors.New(errors.EDiff,
				"hit sequence differs at position %d: %s vs %s",
				i, a.HitSequence[i], b.HitSequence[i])
		}
	}
	return nil
}

func diffPoints(ra, rb model.ZoneResult) *Diff {
	if len(ra.Points) != len(rb.Points) {
		return &Diff{Kind: "point-count", ZoneID: ra.ZoneID,
			Left: len(ra.Points), Right: len(rb.Points),
			Detail: "reconstructed point count differs"}
	}
	for i := range ra.Points {
		pa, pb := ra.Points[i], rb.Points[i]
		if pa != pb {
			return &Diff{Kind: "point", ZoneID: ra.ZoneID, Left: pa, Right: pb,
				Detail: "first differing point at index " + itoa(i)}
		}
	}
	return nil
}

func diffHeat(ra, rb model.ZoneResult) *Diff {
	if len(ra.HeatTiles) != len(rb.HeatTiles) {
		return &Diff{Kind: "heat-count", ZoneID: ra.ZoneID,
			Left: len(ra.HeatTiles), Right: len(rb.HeatTiles),
			Detail: "heat tile count differs"}
	}
	for i := range ra.HeatTiles {
		ta, tb := ra.HeatTiles[i], rb.HeatTiles[i]
		if ta.IX != tb.IX || ta.IY != tb.IY {
			return &Diff{Kind: "heat-coord", ZoneID: ra.ZoneID, Left: ta, Right: tb,
				Detail: "first differing heat raster coordinate"}
		}
		if ta.Density != tb.Density {
			return &Diff{Kind: "heat-value", ZoneID: ra.ZoneID,
				Left: ta.Density, Right: tb.Density,
				Detail: "first differing heat value at raster coordinate"}
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
