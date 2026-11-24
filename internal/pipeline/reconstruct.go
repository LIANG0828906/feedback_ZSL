package pipeline

import (
	"sort"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/geom"
	"collectionrecon/internal/model"
	"collectionrecon/internal/rand"
)

// ReconstructPoints rebuilds the point collection for one zone using a stream
// derived solely from region content (seed, zone id, estimate). Rejection
// sampling keeps points out of no-collect areas, and coordinates are rounded to
// a fixed grid so output is stable and comparable point by point.
func ReconstructPoints(region *model.Region, est model.DensityEstimate) ([]model.ReconstructedPoint, error) {
	zone := findZone(region, est.ZoneID)
	rng := rand.Derive(region.Seed, uint64(est.ZoneID)*104729+hashEstimate(est))

	points := make([]model.ReconstructedPoint, 0)
	for rarity, count := range est.RarityCounts {
		for placed := 0; placed < count; {
			px := zone.Bounds.MinX + rng.Float64()*(zone.Bounds.MaxX-zone.Bounds.MinX)
			py := zone.Bounds.MinY + rng.Float64()*(zone.Bounds.MaxY-zone.Bounds.MinY)
			coord := model.Point{X: snap(px, region.RasterCell), Y: snap(py, region.RasterCell)}
			if !geom.PointInRect(coord, zone.Bounds) || inNoCollect(region, coord) {
				continue
			}
			points = append(points, model.ReconstructedPoint{
				Coord:   coord,
				ZoneID:  zone.ID,
				Terrain: terrainAt(region, coord),
				Rarity:  rarity,
			})
			placed++
		}
	}

	sort.Slice(points, func(i, j int) bool {
		if points[i].Rarity != points[j].Rarity {
			return points[i].Rarity < points[j].Rarity
		}
		if points[i].Coord.X != points[j].Coord.X {
			return points[i].Coord.X < points[j].Coord.X
		}
		return points[i].Coord.Y < points[j].Coord.Y
	})

	for _, p := range points {
		if inNoCollect(region, p.Coord) {
			return nil, errors.WithContext(errors.EZone,
				map[string]any{"zoneId": zone.ID, "point": p.Coord},
				"reconstructed point (%g,%g) falls inside no-collect area of zone %d",
				p.Coord.X, p.Coord.Y, zone.ID)
		}
	}
	return points, nil
}

func inNoCollect(region *model.Region, p model.Point) bool {
	for _, nz := range region.NoCollect {
		if nz.ZoneID == zoneAtPoint(region, p) && geom.PointInRect(p, nz.Bounds) {
			return true
		}
	}
	return false
}

func zoneAtPoint(region *model.Region, p model.Point) int {
	for _, z := range region.Zones {
		if geom.PointInRect(p, z.Bounds) {
			return z.ID
		}
	}
	return -1
}

func terrainAt(region *model.Region, p model.Point) int {
	ix := int((p.X - region.BBox.MinX) / region.RasterCell)
	iy := int((p.Y - region.BBox.MinY) / region.RasterCell)
	if ix < 0 || ix >= region.RasterNX || iy < 0 || iy >= region.RasterNY {
		return -1
	}
	return region.TerrainRaster[ix][iy]
}

func snap(v, cell float64) float64 {
	return float64(int64((v/cell)+0.5)) * cell
}

func hashEstimate(est model.DensityEstimate) uint64 {
	h := uint64(est.ZoneID) * 1315423911
	h ^= uint64(est.CellCount) * 2654435761
	// Fold the density into an integer representation without string hashing.
	bits := uint64(int64(est.Density * 1e9))
	h ^= bits*11400714819323198485 + 1
	for _, c := range est.RarityCounts {
		h ^= uint64(c+1) * 1099511628211
	}
	return h
}
