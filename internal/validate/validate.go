package validate

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"atelier/internal/apperr"
	"atelier/internal/geometry"
	"atelier/internal/model"
)

var terrainKinds = map[string]bool{
	"forest": true,
	"meadow": true,
	"ridge":  true,
	"marsh":  true,
}

func Scenario(s *model.Scenario) error {
	if s == nil {
		return apperr.Schema("scenario is required")
	}
	if s.SchemaVersion == "" {
		return apperr.Schema("schemaVersion is required")
	}
	if s.SchemaVersion != model.SchemaVersion {
		return apperr.Schema("unsupported schemaVersion %q", s.SchemaVersion)
	}
	if s.Region.ID == "" || s.Region.Name == "" {
		return apperr.Schema("region id and name are required")
	}
	if len(s.Region.Boundary.Vertices) < 4 {
		return apperr.Schema("region boundary must contain a closed polygon")
	}
	if len(s.Region.Zones) == 0 {
		return apperr.Schema("region must contain at least one zone")
	}
	if err := samplingConfig(s.Config); err != nil {
		return err
	}
	if s.Truth.RegionID == "" {
		return apperr.Schema("truth.regionId is required")
	}
	if len(s.Truth.Zones) == 0 || len(s.Truth.Points) == 0 {
		return apperr.Schema("truth manifest is required")
	}
	for i, o := range s.Observations {
		if o.ID == "" || o.ZoneID == "" || o.Terrain == "" {
			return apperr.Schema("observation %d is missing id, zoneId or terrain", i)
		}
		if o.Density < 0 || o.QuantizedDensity < 0 {
			return apperr.Schema("observation %s has negative density", o.ID)
		}
		if len(o.RarityCounts) == 0 {
			return apperr.Schema("observation %s is missing rarity counts", o.ID)
		}
	}
	if err := geometryValidation(s); err != nil {
		return err
	}
	return truthValidation(s)
}

func samplingConfig(c model.SamplingConfig) error {
	if c.SampleGridStep <= 0 || c.SamplesPerZone <= 0 ||
		c.HeatmapCellSize <= 0 || c.HeatmapTileSize <= 0 ||
		c.MaxParallelism <= 0 || c.PerWorkerBufferSize <= 0 ||
		c.QuantizationStep <= 0 {
		return apperr.Schema("sampling config contains a missing or non-positive field")
	}
	if c.SampleGridStep > 100 || c.HeatmapCellSize > 100 {
		return apperr.Schema("sampling config grid sizes are implausibly large")
	}
	if c.HeatmapTileSize*c.HeatmapTileSize > c.PerWorkerBufferSize {
		return apperr.Schema("heatmap tile requires %d grid cells but per-worker buffer is %d",
			c.HeatmapTileSize*c.HeatmapTileSize, c.PerWorkerBufferSize)
	}
	return nil
}

func geometryValidation(s *model.Scenario) error {
	boundary := s.Region.Boundary
	if !closed(boundary) {
		return apperr.Geom("", 0, "region boundary is not closed")
	}
	if idx, bad := geometry.FirstSelfIntersection(boundary); bad {
		return apperr.Geom("", idx+1, "region boundary self-intersects at segment starting at vertex %d", idx+1)
	}
	if boundary.Area() <= 1e-6 {
		return apperr.Geom("", 0, "region boundary has zero area")
	}
	bounds := boundary.Bounds()
	if err := rasterValidation(s.Region.Terrain, bounds); err != nil {
		return err
	}

	seen := map[string]bool{}
	for zi := range s.Region.Zones {
		z := &s.Region.Zones[zi]
		if z.ID == "" || z.Name == "" {
			return apperr.Schema("zone %d is missing id or name", zi)
		}
		if seen[z.ID] {
			return apperr.Geom(z.ID, 0, "duplicate zone id %s", z.ID)
		}
		seen[z.ID] = true
		if z.Params.DensityScale <= 0 || z.Params.DensityScale > 10 {
			return apperr.Schema("zone %s densityScale must be in (0,10]", z.ID)
		}
		if math.Abs(z.Params.RarityShift) > 0.5 {
			return apperr.Schema("zone %s rarityShift must be in [-0.5,0.5]", z.ID)
		}
		if !closed(z.Polygon) {
			return apperr.Geom(z.ID, 0, "zone %s polygon is not closed", z.ID)
		}
		if idx, bad := geometry.FirstSelfIntersection(z.Polygon); bad {
			return apperr.Geom(z.ID, idx+1, "zone %s self-intersects at segment starting at vertex %d", z.ID, idx+1)
		}
		if z.Polygon.Area() <= 1e-6 {
			return apperr.Geom(z.ID, 0, "zone %s polygon has zero area", z.ID)
		}
		if vi, ok := geometry.PolygonInside(z.Polygon, boundary); !ok {
			return apperr.Geom(z.ID, vi+1, "zone %s vertex %d falls outside the region", z.ID, vi+1)
		}
	}

	zones := append([]model.Zone(nil), s.Region.Zones...)
	sort.SliceStable(zones, func(i, j int) bool {
		if zones[i].Ordinal == zones[j].Ordinal {
			return zones[i].ID < zones[j].ID
		}
		return zones[i].Ordinal < zones[j].Ordinal
	})
	for i := 0; i < len(zones); i++ {
		for j := i + 1; j < len(zones); j++ {
			if geometry.PolygonsOverlap(zones[i].Polygon, zones[j].Polygon) {
				return apperr.Geom(zones[i].ID, 0, "zones %s and %s overlap", zones[i].ID, zones[j].ID)
			}
		}
	}

	for _, ex := range s.Region.Exclusions {
		if ex.ID == "" || ex.Name == "" || ex.Reason == "" {
			return apperr.Schema("exclusion %q is missing id, name or reason", ex.ID)
		}
		if !closed(ex.Polygon) {
			return apperr.Geom(ex.ID, 0, "exclusion %s polygon is not closed", ex.ID)
		}
		if idx, bad := geometry.FirstSelfIntersection(ex.Polygon); bad {
			return apperr.Geom(ex.ID, idx+1, "exclusion %s self-intersects at vertex %d", ex.ID, idx+1)
		}
		if vi, ok := geometry.PolygonInside(ex.Polygon, boundary); !ok {
			return apperr.Geom(ex.ID, vi+1, "exclusion %s vertex %d falls outside the region", ex.ID, vi+1)
		}
	}

	obsGrid := map[string]bool{}
	for _, o := range s.Observations {
		key := fmt.Sprintf("%d:%d", o.GridRow, o.GridCol)
		if obsGrid[key] {
			return apperr.Geom(o.ZoneID, 0, "duplicate observation grid cell %s", key)
		}
		obsGrid[key] = true
		inside, _ := geometry.PointInPolygon(model.Point{X: o.X, Y: o.Y}, boundary)
		if !inside {
			return apperr.Geom(o.ZoneID, 0, "observation %s lies outside the region", o.ID)
		}
	}
	return nil
}

func rasterValidation(r model.TerrainRaster, b model.Bounds) error {
	if r.CellSize <= 0 || r.Width <= 0 || r.Height <= 0 || len(r.Types) == 0 {
		return apperr.Schema("terrain raster is missing cellSize, dimensions or types")
	}
	if math.Abs(r.OriginX-b.MinX) > 1e-7 || math.Abs(r.OriginY-b.MinY) > 1e-7 {
		return apperr.Geom("", 0, "terrain raster origin (%.3f,%.3f) is incompatible with region bounds (%.3f,%.3f)", r.OriginX, r.OriginY, b.MinX, b.MinY)
	}
	expectedW := int(math.Ceil((b.Width() - 1e-9) / r.CellSize))
	expectedH := int(math.Ceil((b.Height() - 1e-9) / r.CellSize))
	if r.Width != expectedW || r.Height != expectedH {
		return apperr.Geom("", 0, "terrain raster %dx%d is incompatible with region bounds; expected %dx%d at cell size %.3f", r.Width, r.Height, expectedW, expectedH, r.CellSize)
	}
	if len(r.Types) != r.Width*r.Height {
		return apperr.Schema("terrain raster types length %d does not match %dx%d", len(r.Types), r.Width, r.Height)
	}
	for i, t := range r.Types {
		if !terrainKinds[t] {
			return apperr.Schema("terrain raster cell %d has unknown terrain %q", i, t)
		}
	}
	return nil
}

func truthValidation(s *model.Scenario) error {
	if s.Truth.RegionID != s.Region.ID {
		return apperr.Schema("truth.regionId %q does not match region %q", s.Truth.RegionID, s.Region.ID)
	}
	zoneIDs := map[string]bool{}
	for _, z := range s.Region.Zones {
		zoneIDs[z.ID] = true
	}
	truthZones := map[string]bool{}
	for _, zt := range s.Truth.Zones {
		if !zoneIDs[zt.ZoneID] {
			return apperr.Schema("truth references unknown zone %q", zt.ZoneID)
		}
		if zt.BaseDensity <= 0 || zt.PointCount < 0 || len(zt.Rarity) != len(model.RarityKinds) {
			return apperr.Schema("truth zone %s is incomplete or invalid", zt.ZoneID)
		}
		var sum float64
		for _, kind := range model.RarityKinds {
			v, ok := zt.Rarity[kind]
			if !ok || v < 0 {
				return apperr.Schema("truth zone %s rarity %s is missing", zt.ZoneID, kind)
			}
			sum += v
		}
		if math.Abs(sum-1) > 1e-6 {
			return apperr.Schema("truth zone %s rarity proportions sum to %.6f", zt.ZoneID, sum)
		}
		truthZones[zt.ZoneID] = true
	}
	if len(truthZones) != len(zoneIDs) {
		return apperr.Schema("truth manifest does not cover every zone")
	}
	for i, p := range s.Truth.Points {
		if p.ID == "" || p.ZoneID == "" || p.Terrain == "" || p.Rarity == "" {
			return apperr.Schema("truth point %d is missing fields", i)
		}
		if p.Density <= 0 {
			return apperr.Schema("truth point %s has non-positive density", p.ID)
		}
		if !permittedRarity(p.Rarity) {
			return apperr.Schema("truth point %s has unknown rarity %q", p.ID, p.Rarity)
		}
	}
	return nil
}

func permittedRarity(s string) bool {
	for _, k := range model.RarityKinds {
		if strings.EqualFold(k, s) {
			return true
		}
	}
	return false
}

func closed(p model.Polygon) bool {
	n := len(p.Vertices)
	if n < 4 {
		return false
	}
	a, b := p.Vertices[0], p.Vertices[n-1]
	return math.Abs(a.X-b.X) < 1e-7 && math.Abs(a.Y-b.Y) < 1e-7
}
