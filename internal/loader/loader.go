// Package loader parses and validates region datasets. Validation is explicit
// and returns the task's stable error codes with field paths or zone ids.
package loader

import (
	"encoding/json"
	"fmt"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/geom"
	"collectionrecon/internal/model"
)

// Load parses JSON region data and runs every structural and geometric check.
func Load(raw []byte) (*model.Region, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, errors.WithPath(errors.ESchema, "$", "invalid json: %v", err)
	}

	required := []string{"id", "seed", "boundary", "bbox", "rasterCell",
		"rasterNx", "rasterNy", "terrainRaster", "zones", "noCollect",
		"sampleGrid", "observations", "truth", "quantStep", "gridSpacing"}
	for _, key := range required {
		if _, ok := top[key]; !ok {
			return nil, errors.WithPath(errors.ESchema, key, "missing required field %q", key)
		}
	}

	var region model.Region
	if err := json.Unmarshal(raw, &region); err != nil {
		return nil, errors.WithPath(errors.ESchema, "$", "type mismatch: %v", err)
	}
	if err := validateRegion(&region); err != nil {
		return nil, err
	}
	return &region, nil
}

func validateRegion(region *model.Region) error {
	if len(region.Boundary.Vertices) < 4 {
		return errors.WithPath(errors.ESchema, "boundary.vertices",
			"boundary needs at least 4 vertices, got %d", len(region.Boundary.Vertices))
	}
	if !geom.IsClosed(region.Boundary) {
		return errors.WithContext(errors.EGeom,
			map[string]any{"regionId": region.ID}, "region %d boundary polygon is not closed", region.ID)
	}
	if !geom.IsSimple(region.Boundary) {
		return errors.WithContext(errors.EGeom,
			map[string]any{"regionId": region.ID}, "region %d boundary polygon self-intersects", region.ID)
	}

	bbox := geom.BBox(region.Boundary)
	if bbox != region.BBox {
		return errors.WithContext(errors.EGeom,
			map[string]any{"regionId": region.ID}, "declared bbox does not match boundary")
	}
	if err := geom.RasterCompatible(region.BBox, region.RasterCell, region.RasterNX, region.RasterNY); err != nil {
		return errors.WithContext(errors.EGeom,
			map[string]any{"regionId": region.ID}, "region %d: %v", region.ID, err)
	}
	if len(region.TerrainRaster) != region.RasterNX {
		return errors.WithContext(errors.EGeom,
			map[string]any{"regionId": region.ID, "zoneId": -1},
			"raster x dimension mismatch: got %d want %d", len(region.TerrainRaster), region.RasterNX)
	}
	for ix, col := range region.TerrainRaster {
		if len(col) != region.RasterNY {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": -1, "ix": ix},
				"raster column %d has %d rows, want %d", ix, len(col), region.RasterNY)
		}
	}

	if len(region.Zones) == 0 {
		return errors.WithPath(errors.ESchema, "zones", "at least one zone is required")
	}
	zoneByID := make(map[int]*model.Zone, len(region.Zones))
	for zi := range region.Zones {
		zone := &region.Zones[zi]
		path := fmt.Sprintf("zones[%d]", zi)
		if zone.ID == 0 {
			return errors.WithPath(errors.ESchema, path+".id", "missing zone id")
		}
		if len(zone.RarityMix) != model.NumRarity {
			return errors.WithPath(errors.ESchema, path+".rarityMix",
				"zone %d needs %d rarity weights", zone.ID, model.NumRarity)
		}
		if len(zone.TerrainWeights) != model.NumRarity {
			return errors.WithPath(errors.ESchema, path+".terrainWeights",
				"zone %d needs %d terrain weights", zone.ID, model.NumRarity)
		}
		if !geom.RectInRect(zone.Bounds, region.BBox) {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": zone.ID},
				"zone %d extends outside region bbox", zone.ID)
		}
		zoneByID[zone.ID] = zone
	}

	for i := 0; i < len(region.Zones); i++ {
		for j := i + 1; j < len(region.Zones); j++ {
			a, b := region.Zones[i], region.Zones[j]
			if geom.RectsOverlap(a.Bounds, b.Bounds) {
				return errors.WithContext(errors.EGeom,
					map[string]any{"regionId": region.ID, "zoneId": a.ID, "otherZoneId": b.ID},
					"zones %d and %d overlap", a.ID, b.ID)
			}
		}
	}

	for ni := range region.NoCollect {
		nz := &region.NoCollect[ni]
		owner, ok := zoneByID[nz.ZoneID]
		if !ok {
			return errors.WithPath(errors.ESchema,
				fmt.Sprintf("noCollect[%d].zoneId", ni), "unknown zone id %d", nz.ZoneID)
		}
		if !geom.RectInRect(nz.Bounds, owner.Bounds) {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": nz.ZoneID},
				"no-collect area %d escapes zone %d", nz.ID, nz.ZoneID)
		}
	}

	for ti := range region.Truth {
		tp := &region.Truth[ti]
		if !geom.PointInPolygon(tp.Coord, region.Boundary) {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": tp.ZoneID},
				"truth point %d outside region boundary", ti)
		}
		zone, ok := zoneByID[tp.ZoneID]
		if !ok {
			return errors.WithPath(errors.ESchema,
				fmt.Sprintf("truth[%d].zoneId", ti), "unknown zone %d", tp.ZoneID)
		}
		if !geom.PointInRect(tp.Coord, zone.Bounds) {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": tp.ZoneID},
				"truth point %d outside its declared zone", ti)
		}
		for _, nz := range region.NoCollect {
			if nz.ZoneID == tp.ZoneID && geom.PointInRect(tp.Coord, nz.Bounds) {
				return errors.WithContext(errors.EZone,
					map[string]any{"regionId": region.ID, "zoneId": tp.ZoneID,
						"point": tp.Coord},
					"truth point inside no-collect zone %d", nz.ID)
			}
		}
	}

	if err := validateObservations(region); err != nil {
		return err
	}
	return validateSampleGrid(region)
}

func validateObservations(region *model.Region) error {
	seen := make(map[int]bool, len(region.Observations))
	cellByID := make(map[int]*model.SampleCell, len(region.SampleGrid))
	for ci := range region.SampleGrid {
		cellByID[region.SampleGrid[ci].Index] = &region.SampleGrid[ci]
	}
	for oi := range region.Observations {
		obs := &region.Observations[oi]
		cell, ok := cellByID[obs.GridIndex]
		if !ok {
			return errors.WithContext(errors.EObs,
				map[string]any{"gridIndex": obs.GridIndex, "x": obs.Coord.X, "y": obs.Coord.Y},
				"observation references unknown sample grid point")
		}
		if !cell.Valid {
			return errors.WithContext(errors.EObs,
				map[string]any{"gridIndex": obs.GridIndex, "x": obs.Coord.X, "y": obs.Coord.Y},
				"observation recorded at invalid grid point")
		}
		seen[obs.GridIndex] = true
	}
	for ci := range region.SampleGrid {
		cell := &region.SampleGrid[ci]
		if cell.Valid && !seen[cell.Index] {
			return errors.WithContext(errors.EObs,
				map[string]any{"gridIndex": cell.Index, "x": cell.Coord.X, "y": cell.Coord.Y},
				"missing observation for valid grid point (%g,%g)", cell.Coord.X, cell.Coord.Y)
		}
	}
	return nil
}

func validateSampleGrid(region *model.Region) error {
	for ci := range region.SampleGrid {
		cell := &region.SampleGrid[ci]
		if !geom.PointInPolygon(cell.Coord, region.Boundary) {
			return errors.WithContext(errors.EGeom,
				map[string]any{"regionId": region.ID, "zoneId": cell.ZoneID},
				"sample point %d outside region", cell.Index)
		}
	}
	return nil
}

// CheckObservationComplete returns E_OBS naming a coordinate when a valid cell
// has no matching observation. It is reused by the estimation stage.
func CheckObservationComplete(region *model.Region) error {
	return validateObservations(region)
}
