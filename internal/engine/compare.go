package engine

import (
	"math"

	"atelier/internal/apperr"
	"atelier/internal/model"
)

func CompareSnapshots(incremental, full Snapshot) (ComparisonResult, error) {
	result := ComparisonResult{
		PointCount: len(incremental.Points),
		PixelCount: len(incremental.Heatmap.Values),
		ZoneCount:  len(incremental.ZoneStates),
	}
	if len(incremental.Points) != len(full.Points) {
		result.Mismatch = &ComparisonMismatch{
			Kind: "point-count", Index: minInt(len(incremental.Points), len(full.Points)),
			Message: "point collection lengths differ",
		}
		return result, diffError(result.Mismatch)
	}
	for i := range incremental.Points {
		a, b := incremental.Points[i], full.Points[i]
		if !samePoint(a, b) {
			zoneID := a.ZoneID
			if zoneID == "" {
				zoneID = b.ZoneID
			}
			result.Mismatch = &ComparisonMismatch{
				Kind: "point", Index: i, ZoneID: zoneID,
				Incremental: a.X + a.Y*1000, Full: b.X + b.Y*1000,
				Message: "reconstructed point sequence differs",
			}
			return result, diffError(result.Mismatch)
		}
	}
	if len(incremental.Heatmap.Values) != len(full.Heatmap.Values) ||
		incremental.Heatmap.Width != full.Heatmap.Width ||
		incremental.Heatmap.Height != full.Heatmap.Height {
		result.Mismatch = &ComparisonMismatch{Kind: "heatmap-shape", Message: "heatmap dimensions differ"}
		return result, diffError(result.Mismatch)
	}
	for i := range incremental.Heatmap.Values {
		a, b := incremental.Heatmap.Values[i], full.Heatmap.Values[i]
		if math.Float64bits(a) != math.Float64bits(b) {
			x := i % full.Heatmap.Width
			y := i / full.Heatmap.Width
			zoneID := ""
			if i < len(full.Heatmap.ZoneIDs) {
				zoneID = full.Heatmap.ZoneIDs[i]
			}
			result.Mismatch = &ComparisonMismatch{
				Kind: "heatmap-pixel", Index: i, ZoneID: zoneID, PixelX: x, PixelY: y,
				Incremental: a, Full: b, Message: "heatmap pixels differ",
			}
			return result, diffError(result.Mismatch)
		}
	}
	if len(incremental.CacheHitSequence) != len(full.CacheHitSequence) {
		result.Mismatch = &ComparisonMismatch{Kind: "cache-order", Message: "zone cache key sequence lengths differ"}
		return result, diffError(result.Mismatch)
	}
	for i := range incremental.CacheHitSequence {
		a, b := incremental.CacheHitSequence[i], full.CacheHitSequence[i]
		if a != b {
			result.Mismatch = &ComparisonMismatch{
				Kind: "cache-order", Index: i, ZoneID: a.ZoneID,
				Message: "zone-level cache key sequence differs",
			}
			return result, diffError(result.Mismatch)
		}
	}
	result.Equal = true
	return result, nil
}

func diffError(m *ComparisonMismatch) error {
	e := apperr.Diff("%s at index %d", m.Message, m.Index)
	e.ZoneID = m.ZoneID
	e.PointIndex = m.Index
	e.PixelX = m.PixelX
	e.PixelY = m.PixelY
	return e
}

func samePoint(a, b model.ResourcePoint) bool {
	return math.Float64bits(a.X) == math.Float64bits(b.X) &&
		math.Float64bits(a.Y) == math.Float64bits(b.Y) &&
		a.ZoneID == b.ZoneID &&
		a.ID == b.ID &&
		a.Terrain == b.Terrain &&
		a.Rarity == b.Rarity
}

func canonicalPoints(states []model.ZoneState) []model.ResourcePoint {
	var points []model.ResourcePoint
	for _, state := range states {
		points = append(points, state.Points...)
	}
	return points
}
