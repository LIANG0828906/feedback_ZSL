package pipeline

import (
	"math"

	"collectionrecon/internal/model"
	"collectionrecon/internal/rand"
)

// HeatRadius matches the generator's observation radius, so the heatmap uses
// exactly the same disc-count density definition as the observed values.
const HeatRadius = 12.0

// BuildHeatTiles rasterizes reconstructed points onto a fixed raster using
// disc-count density with the same radius as observations. Only tiles whose
// center belongs to the zone are emitted, so moving a no-collect boundary
// invalidates exactly the owning zone's tiles.
func BuildHeatTiles(region *model.Region, zoneID int, points []model.ReconstructedPoint) []model.HeatTile {
	zone := findZone(region, zoneID)
	ix0 := int((zone.Bounds.MinX - region.BBox.MinX) / region.RasterCell)
	ix1 := int((zone.Bounds.MaxX - region.BBox.MinX) / region.RasterCell)

	tiles := make([]model.HeatTile, 0, (ix1-ix0)*region.RasterNY)
	for ix := ix0; ix < ix1; ix++ {
		for iy := 0; iy < region.RasterNY; iy++ {
			tiles = append(tiles, HeatTileAt(region, zoneID, points, ix, iy))
		}
	}
	return tiles
}

// HeatTileAt computes one heat tile, exposed so the engine can stream tiles
// through a bounded pixel-buffer pool instead of allocating them all at once.
func HeatTileAt(region *model.Region, zoneID int, points []model.ReconstructedPoint, ix, iy int) model.HeatTile {
	center := model.Point{
		X: region.BBox.MinX + (float64(ix)+0.5)*region.RasterCell,
		Y: region.BBox.MinY + (float64(iy)+0.5)*region.RasterCell,
	}
	return model.HeatTile{
		IX:      ix,
		IY:      iy,
		Center:  center,
		ZoneID:  zoneID,
		Density: rand.Quantize(discDensity(points, center), QuantStep),
	}
}

// HeatAt evaluates disc-count density at an arbitrary coordinate, e.g. a known
// sample grid cell, using the same radius, area and quantization as
// observations so the two densities are directly comparable.
func HeatAt(points []model.ReconstructedPoint, at model.Point) float64 {
	return rand.Quantize(discDensity(points, at), QuantStep)
}

func discDensity(points []model.ReconstructedPoint, at model.Point) float64 {
	count := 0
	r2 := HeatRadius * HeatRadius
	for _, p := range points {
		dx, dy := p.Coord.X-at.X, p.Coord.Y-at.Y
		if dx*dx+dy*dy <= r2 {
			count++
		}
	}
	return float64(count) / (math.Pi * HeatRadius * HeatRadius)
}

// QuantStep is the density quantization shared with observations.
const QuantStep = 0.001
