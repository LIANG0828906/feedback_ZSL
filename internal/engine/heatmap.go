package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"atelier/internal/geometry"
	"atelier/internal/hashutil"
	"atelier/internal/model"
)

type heatTileStats struct {
	tileX  int
	tileY  int
	byZone map[string]tileZoneStats
	hash   string
}

type tileZoneStats struct {
	reused     int
	recomputed int
}

func (e *Engine) buildHeatmap(ctx context.Context, affected map[string]bool) (model.Heatmap, []heatTileStats, error) {
	b := e.model.Region.Boundary.Bounds()
	cell := e.model.Config.HeatmapCellSize
	width := int(math.Ceil(b.Width() / cell))
	height := int(math.Ceil(b.Height() / cell))
	tileSize := e.model.Config.HeatmapTileSize
	tilesX := (width + tileSize - 1) / tileSize
	tilesY := (height + tileSize - 1) / tileSize
	hm := model.Heatmap{
		OriginX:    b.MinX,
		OriginY:    b.MinY,
		CellSize:   cell,
		Width:      width,
		Height:     height,
		TileSize:   tileSize,
		Values:     make([]float64, width*height),
		ZoneIDs:    make([]string, width*height),
		TileHashes: make([]string, tilesX*tilesY),
		Revision:   e.revision,
	}
	prev := e.heat
	full := prev == nil || prev.Width != width || prev.Height != height ||
		math.Abs(prev.OriginX-b.MinX) > 1e-9 || math.Abs(prev.OriginY-b.MinY) > 1e-9 ||
		math.Abs(prev.CellSize-cell) > 1e-9 || len(prev.Values) != len(hm.Values)
	if full {
		affected = map[string]bool{}
		for _, z := range e.zoneOrder {
			affected[z.ID] = true
		}
	}
	totalTiles := tilesX * tilesY
	stats := make([]heatTileStats, totalTiles)
	errs := make([]error, totalTiles)
	err := e.pool.Run(ctx, totalTiles, func(index int) {
		tx := index % tilesX
		ty := index / tilesX
		tileStats := heatTileStats{tileX: tx, tileY: ty, byZone: map[string]tileZoneStats{}}
		startCol := tx * tileSize
		startRow := ty * tileSize
		endCol := minInt(startCol+tileSize, width)
		endRow := minInt(startRow+tileSize, height)
		var hashParts []string
		for row := startRow; row < endRow; row++ {
			for col := startCol; col < endCol; col++ {
				idx := row*width + col
				x := b.MinX + (float64(col)+0.5)*cell
				y := b.MinY + (float64(row)+0.5)*cell
				currentZone := e.zoneIDAt(model.Point{X: x, Y: y})
				hm.ZoneIDs[idx] = currentZone
				oldZone := ""
				if prev != nil && idx < len(prev.ZoneIDs) {
					oldZone = prev.ZoneIDs[idx]
				}
				zoneStatKey := currentZone
				if zoneStatKey == "" {
					zoneStatKey = "outside"
				}
				zs := tileStats.byZone[zoneStatKey]
				recompute := full || affected[currentZone] || affected[oldZone]
				if recompute {
					if e.exclusionAt(model.Point{X: x, Y: y}) {
						hm.Values[idx] = 0
					} else {
						hm.Values[idx] = e.densityAt(currentZone, e.model.Region.Terrain.AtXY(x, y))
					}
					zs.recomputed++
				} else {
					hm.Values[idx] = prev.Values[idx]
					zs.reused++
				}
				tileStats.byZone[zoneStatKey] = zs
				hashParts = append(hashParts, fmt.Sprintf("%d:%s:%.9g", idx, currentZone, hm.Values[idx]))
			}
		}
		tileStats.hash = hashutil.HashBytes([]byte(strings.Join(hashParts, "|")))
		stats[index] = tileStats
		hm.TileHashes[index] = tileStats.hash
		errs[index] = nil
	})
	if err != nil {
		return model.Heatmap{}, nil, err
	}
	for _, err := range errs {
		if err != nil {
			return model.Heatmap{}, nil, err
		}
	}
	return hm, stats, nil
}

func (e *Engine) zoneIDAt(p model.Point) string {
	for _, zone := range e.zoneOrder {
		if inside, _ := geometry.PointInPolygon(p, zone.Polygon); inside {
			return zone.ID
		}
	}
	return ""
}

func (e *Engine) densityAt(zoneID, terrain string) float64 {
	if zoneID == "" {
		return 0
	}
	state, ok := e.states[zoneID]
	if !ok {
		return 0
	}
	for _, layer := range state.Estimate.Layers {
		if layer.Terrain == terrain {
			return layer.Density
		}
	}
	return state.Estimate.Density
}

func (e *Engine) exclusionAt(p model.Point) bool {
	for _, ex := range e.model.Region.Exclusions {
		if inside, _ := geometry.PointInPolygon(p, ex.Polygon); inside {
			return true
		}
	}
	return false
}

func (e *Engine) excluded(p model.Point) bool {
	return e.exclusionAt(p)
}

func (e *Engine) zoneTerrainAreas(zone model.Zone) map[string]float64 {
	areas := map[string]float64{}
	r := e.model.Region.Terrain
	cellArea := r.CellSize * r.CellSize
	for row := 0; row < r.Height; row++ {
		for col := 0; col < r.Width; col++ {
			x := r.OriginX + (float64(col)+0.5)*r.CellSize
			y := r.OriginY + (float64(row)+0.5)*r.CellSize
			p := model.Point{X: x, Y: y}
			if !geometry.StrictlyInside(p, zone.Polygon) || e.excluded(p) {
				continue
			}
			areas[r.At(row, col)] += cellArea
		}
	}
	return areas
}

func (e *Engine) zoneRelevantExclusions(zone model.Zone) []model.Exclusion {
	var out []model.Exclusion
	for _, ex := range e.model.Region.Exclusions {
		if bboxTouches(zone.Polygon.Bounds(), ex.Polygon.Bounds()) {
			out = append(out, ex)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func bboxTouches(a, b model.Bounds) bool {
	return a.MinX <= b.MaxX && a.MaxX >= b.MinX && a.MinY <= b.MaxY && a.MaxY >= b.MinY
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
