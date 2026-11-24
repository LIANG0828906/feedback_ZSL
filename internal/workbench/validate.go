package workbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

func LoadRegion(data []byte) (*Region, *Error) {
	if err := validateRequiredJSON(data); err != nil {
		return nil, err
	}
	var region Region
	if err := jsonUnmarshalStrict(data, &region); err != nil {
		return nil, schemaError("$", "invalid region JSON: %v", err)
	}
	if err := ValidateRegion(&region); err != nil {
		return nil, err
	}
	return normalizeRegion(&region), nil
}

func validateRequiredJSON(data []byte) *Error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return schemaError("$", "invalid region JSON: %v", err)
	}
	if err := requireObjectKeys(root, "$", "schema_version", "region_id", "bounds", "boundary",
		"terrain_types", "terrain_raster", "zones", "sampling_config", "sample_sites", "truth"); err != nil {
		return err
	}
	if err := requireKeysInRaw(root, "bounds", "bounds", "min_x", "min_y", "max_x", "max_y"); err != nil {
		return err
	}
	if err := requireKeysInRaw(root, "terrain_raster", "terrain_raster", "cell_size", "cols", "rows", "values"); err != nil {
		return err
	}
	if err := requireKeysInRaw(root, "sampling_config", "sampling_config",
		"grid_spacing", "max_samples", "min_per_stratum", "per_thread_sample_buffer",
		"max_parallel", "density_step", "rarity_step", "density_tolerance",
		"count_tolerance", "heatmap_tolerance", "bias_threshold", "heatmap_cell_size"); err != nil {
		return err
	}
	if err := requireArrayObjectKeys(root, "zones", "zones",
		"id", "polygon", "terrain_types", "density_limit"); err != nil {
		return err
	}
	if _, exists := root["no_collect"]; exists {
		if err := requireArrayObjectKeys(root, "no_collect", "no_collect", "id", "polygon"); err != nil {
			return err
		}
	}
	if err := requireArrayObjectKeys(root, "sample_sites", "sample_sites",
		"id", "x", "y", "observed_density"); err != nil {
		return err
	}
	var truth map[string]json.RawMessage
	if raw, ok := root["truth"]; ok {
		if err := json.Unmarshal(raw, &truth); err != nil {
			return schemaError("truth", "truth must be an object")
		}
	}
	if err := requireObjectKeys(truth, "truth", "zone_truth", "points"); err != nil {
		return err
	}
	var zoneTruth map[string]json.RawMessage
	if raw, ok := truth["zone_truth"]; ok {
		if err := json.Unmarshal(raw, &zoneTruth); err != nil {
			return schemaError("truth.zone_truth", "truth.zone_truth must be an object")
		}
	}
	for zoneID, raw := range zoneTruth {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil {
			return schemaError("truth.zone_truth."+zoneID, "zone truth must be an object")
		}
		if err := requireObjectKeys(entry, "truth.zone_truth."+zoneID, "density", "rarity_mix"); err != nil {
			return err
		}
	}
	if err := requireArrayObjectKeys(truth, "points", "truth.points", "zone_id", "x", "y", "terrain_type", "rarity"); err != nil {
		return err
	}
	return nil
}

func requireObjectKeys(obj map[string]json.RawMessage, path string, keys ...string) *Error {
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return schemaError(joinFieldPath(path, key), "field is required")
		}
	}
	return nil
}

func requireKeysInRaw(obj map[string]json.RawMessage, rawKey, path string, keys ...string) *Error {
	var child map[string]json.RawMessage
	if err := json.Unmarshal(obj[rawKey], &child); err != nil {
		return schemaError(path, "%s must be an object", path)
	}
	return requireObjectKeys(child, path, keys...)
}

func requireArrayObjectKeys(obj map[string]json.RawMessage, rawKey, path string, keys ...string) *Error {
	var items []json.RawMessage
	if err := json.Unmarshal(obj[rawKey], &items); err != nil {
		return schemaError(path, "%s must be an array", path)
	}
	for i, item := range items {
		var child map[string]json.RawMessage
		if err := json.Unmarshal(item, &child); err != nil {
			return schemaError(fmt.Sprintf("%s[%d]", path, i), "array item must be an object")
		}
		if err := requireObjectKeys(child, fmt.Sprintf("%s[%d]", path, i), keys...); err != nil {
			return err
		}
	}
	return nil
}

func joinFieldPath(base, key string) string {
	if base == "$" || base == "" {
		return key
	}
	return base + "." + key
}

func ValidateRegion(region *Region) *Error {
	if region == nil {
		return schemaError("$", "region document is required")
	}
	if region.SchemaVersion == "" {
		return schemaError("schema_version", "schema_version is required")
	}
	if region.SchemaVersion != SchemaVersion {
		return schemaError("schema_version", "unsupported schema_version %q", region.SchemaVersion)
	}
	if region.RegionID == "" {
		return schemaError("region_id", "region_id is required")
	}
	if region.Bounds.Width() <= geometryEpsilon || region.Bounds.Height() <= geometryEpsilon ||
		!finite(region.Bounds.MinX) || !finite(region.Bounds.MinY) ||
		!finite(region.Bounds.MaxX) || !finite(region.Bounds.MaxY) {
		return schemaError("bounds", "bounds must be finite and have positive width and height")
	}
	if err := validatePolygon(region.Boundary, "region:"+region.RegionID, "boundary"); err != nil {
		return err
	}
	for _, p := range region.Boundary {
		if !pointInBounds(p, region.Bounds) {
			return geomError("region:"+region.RegionID, "boundary vertex (%.6f,%.6f) lies outside bounds", p.X, p.Y)
		}
	}
	if len(region.TerrainTypes) == 0 {
		return schemaError("terrain_types", "terrain_types is required")
	}
	terrainIndex := make(map[string]int, len(region.TerrainTypes))
	for i, name := range region.TerrainTypes {
		if name == "" {
			return schemaError(fmt.Sprintf("terrain_types[%d]", i), "terrain type name is required")
		}
		if _, exists := terrainIndex[name]; exists {
			return schemaError(fmt.Sprintf("terrain_types[%d]", i), "duplicate terrain type %q", name)
		}
		terrainIndex[name] = i
	}
	if region.TerrainRaster.CellSize <= 0 {
		return schemaError("terrain_raster.cell_size", "terrain_raster.cell_size must be positive")
	}
	if region.TerrainRaster.Cols <= 0 {
		return schemaError("terrain_raster.cols", "terrain_raster.cols must be positive")
	}
	if region.TerrainRaster.Rows <= 0 {
		return schemaError("terrain_raster.rows", "terrain_raster.rows must be positive")
	}
	if len(region.TerrainRaster.Values) == 0 {
		return schemaError("terrain_raster.values", "terrain_raster.values is required")
	}
	if len(region.TerrainRaster.Values) != region.TerrainRaster.Rows*region.TerrainRaster.Cols {
		return geomError("terrain_raster", "terrain raster has %d values, expected rows*cols=%d",
			len(region.TerrainRaster.Values), region.TerrainRaster.Rows*region.TerrainRaster.Cols)
	}
	rasterWidth := float64(region.TerrainRaster.Cols) * region.TerrainRaster.CellSize
	rasterHeight := float64(region.TerrainRaster.Rows) * region.TerrainRaster.CellSize
	if !nearlyEqual(rasterWidth, region.Bounds.Width()) || !nearlyEqual(rasterHeight, region.Bounds.Height()) {
		return geomError("terrain_raster",
			"raster extent %.3fx%.3f does not match region bounds %.3fx%.3f",
			rasterWidth, rasterHeight, region.Bounds.Width(), region.Bounds.Height())
	}
	for i, value := range region.TerrainRaster.Values {
		if value < 0 || value >= len(region.TerrainTypes) {
			return geomError("terrain_raster",
				"terrain cell %d has type index %d outside [0,%d)", i, value, len(region.TerrainTypes))
		}
	}
	if len(region.Zones) == 0 {
		return schemaError("zones", "at least one zone is required")
	}

	zoneIndex := make(map[string]int, len(region.Zones))
	for i := range region.Zones {
		z := &region.Zones[i]
		path := fmt.Sprintf("zones[%d]", i)
		if z.ID == "" {
			return schemaError(path+".id", "zone id is required")
		}
		if _, exists := zoneIndex[z.ID]; exists {
			return schemaError(path+".id", "duplicate zone id %q", z.ID)
		}
		zoneIndex[z.ID] = i
		if err := validatePolygon(z.Polygon, "zone:"+z.ID, path+".polygon"); err != nil {
			return err
		}
		for _, p := range z.Polygon {
			if !pointInBounds(p, region.Bounds) {
				return geomError("zone:"+z.ID, "zone vertex (%.6f,%.6f) lies outside region bounds", p.X, p.Y)
			}
		}
		if len(z.TerrainTypes) == 0 {
			return schemaError(path+".terrain_types", "zone terrain_types is required")
		}
		seenTerrain := make(map[string]struct{}, len(z.TerrainTypes))
		for j, name := range z.TerrainTypes {
			if _, ok := terrainIndex[name]; !ok {
				return schemaError(fmt.Sprintf("%s.terrain_types[%d]", path, j), "unknown terrain type %q", name)
			}
			if _, dup := seenTerrain[name]; dup {
				return schemaError(fmt.Sprintf("%s.terrain_types[%d]", path, j), "duplicate zone terrain type %q", name)
			}
			seenTerrain[name] = struct{}{}
		}
		if !finite(z.DensityLimit) || z.DensityLimit < 0 {
			return schemaError(path+".density_limit", "density_limit must be finite and non-negative")
		}
		if z.SampleCountLimit < 0 {
			return schemaError(path+".sample_count_limit", "sample_count_limit must be non-negative")
		}
		if z.RarityMix != nil {
			if err := validateMix(z.RarityMix, path+".rarity_mix"); err != nil {
				return err
			}
		}
		for j, dep := range z.DependsOn {
			if dep == "" {
				return schemaError(fmt.Sprintf("%s.depends_on[%d]", path, j), "dependency zone id is required")
			}
			if dep == z.ID {
				return schemaError(fmt.Sprintf("%s.depends_on[%d]", path, j), "zone cannot depend on itself")
			}
		}
		intersection := polygonIntersectionArea(z.Polygon, region.Boundary)
		if math.Abs(intersection-polygonArea(z.Polygon)) > 1e-5 {
			return geomError("zone:"+z.ID, "zone extends outside the region boundary")
		}
	}
	for i := range region.Zones {
		z := region.Zones[i]
		for j := i + 1; j < len(region.Zones); j++ {
			other := region.Zones[j]
			if polygonOverlaps(z.Polygon, other.Polygon) {
				return geomError("zone:"+z.ID, "zone overlaps zone:%s", other.ID)
			}
		}
	}
	for i := range region.Zones {
		z := region.Zones[i]
		for j, dep := range z.DependsOn {
			if _, ok := zoneIndex[dep]; !ok {
				return schemaError(fmt.Sprintf("zones[%d].depends_on[%d]", i, j), "unknown dependency zone %q", dep)
			}
		}
	}
	if _, err := topologyOrder(region.Zones); err != nil {
		return err
	}

	for i := range region.NoCollect {
		nc := &region.NoCollect[i]
		path := fmt.Sprintf("no_collect[%d]", i)
		if nc.ID == "" {
			return schemaError(path+".id", "no-collect id is required")
		}
		if err := validatePolygon(nc.Polygon, "no_collect:"+nc.ID, path+".polygon"); err != nil {
			return err
		}
		for _, p := range nc.Polygon {
			if !pointInBounds(p, region.Bounds) {
				return geomError("no_collect:"+nc.ID, "no-collect vertex lies outside region bounds")
			}
		}
	}
	seenNoCollect := map[string]struct{}{}
	for i, nc := range region.NoCollect {
		if _, dup := seenNoCollect[nc.ID]; dup {
			return schemaError(fmt.Sprintf("no_collect[%d].id", i), "duplicate no-collect id %q", nc.ID)
		}
		seenNoCollect[nc.ID] = struct{}{}
	}

	cfg := region.Sampling
	if cfg.GridSpacing <= 0 {
		return schemaError("sampling_config.grid_spacing", "grid_spacing must be positive")
	}
	if cfg.MaxSamples <= 0 {
		return schemaError("sampling_config.max_samples", "max_samples must be positive")
	}
	if cfg.MinPerStratum < 0 {
		return schemaError("sampling_config.min_per_stratum", "min_per_stratum must be non-negative")
	}
	if cfg.PerThreadSampleBuffer <= 0 {
		return schemaError("sampling_config.per_thread_sample_buffer", "per_thread_sample_buffer must be positive")
	}
	if cfg.MaxParallel <= 0 {
		return schemaError("sampling_config.max_parallel", "max_parallel must be positive")
	}
	if cfg.DensityStep <= 0 {
		return schemaError("sampling_config.density_step", "density_step must be positive")
	}
	if cfg.RarityStep <= 0 || cfg.RarityStep > 1 {
		return schemaError("sampling_config.rarity_step", "rarity_step must be in (0,1]")
	}
	if cfg.DensityTolerance < 0 || cfg.CountTolerance < 0 || cfg.HeatmapTolerance < 0 {
		return schemaError("sampling_config.density_tolerance", "tolerances must be non-negative")
	}
	if cfg.BiasThreshold <= 0 {
		return schemaError("sampling_config.bias_threshold", "bias_threshold must be positive")
	}
	if cfg.HeatmapCellSize <= 0 {
		return schemaError("sampling_config.heatmap_cell_size", "heatmap_cell_size must be positive")
	}
	if cfg.MaxPointsPerZone < 0 {
		return schemaError("sampling_config.max_points_per_zone", "max_points_per_zone must be non-negative")
	}
	if len(region.SampleSites) == 0 {
		return schemaError("sample_sites", "sample_sites is required")
	}
	if len(region.SampleSites) > cfg.MaxSamples {
		return schemaError("sample_sites", "%d sample sites exceeds declared max_samples=%d",
			len(region.SampleSites), cfg.MaxSamples)
	}
	seenSites := map[string]struct{}{}
	noCollectPoints := collectNoCollect(region)
	for i := range region.SampleSites {
		site := &region.SampleSites[i]
		path := fmt.Sprintf("sample_sites[%d]", i)
		if site.ID == "" {
			return schemaError(path+".id", "sample site id is required")
		}
		if _, dup := seenSites[site.ID]; dup {
			return schemaError(path+".id", "duplicate sample site id %q", site.ID)
		}
		seenSites[site.ID] = struct{}{}
		if !finite(site.X) || !finite(site.Y) {
			return sampleError("sample:"+site.ID, site.Point(), "sample coordinate is not finite")
		}
		if !finite(site.ObservedDensity) || site.ObservedDensity < 0 {
			return schemaError(path+".observed_density", "observed_density must be finite and non-negative")
		}
		for rarity, count := range site.RarityCounts {
			if rarity == "" {
				return schemaError(path+".rarity_counts", "rarity name is required")
			}
			if count < 0 {
				return schemaError(fmt.Sprintf("%s.rarity_counts.%s", path, rarity), "count must be non-negative")
			}
		}
		if !pointInPolygon(site.Point(), region.Boundary) {
			return sampleError("region:"+region.RegionID, site.Point(), "sample site lies outside the region boundary")
		}
		for _, nc := range noCollectPoints {
			if pointInPolygon(site.Point(), nc.Polygon) {
				return sampleError("no_collect:"+nc.ID, site.Point(), "sample site lies inside a no-collect area")
			}
		}
		terrain, ok := terrainAt(region, site.Point())
		if !ok {
			return sampleError("region:"+region.RegionID, site.Point(), "sample site does not map to a terrain raster cell")
		}
		zone := zoneAt(region, site.Point())
		if zone == nil {
			return sampleError("zone:none", site.Point(), "sample site is not inside any zone")
		}
		if !containsString(zone.TerrainTypes, terrain) {
			return sampleError("zone:"+zone.ID, site.Point(), "sample site terrain %q is not valid for the zone", terrain)
		}
		if site.TerrainType != "" && site.TerrainType != terrain {
			return sampleError("terrain:"+terrain, site.Point(),
				"sample site declares terrain %q but raster contains %q", site.TerrainType, terrain)
		}
	}

	if len(region.Truth.ZoneTruth) == 0 {
		return schemaError("truth.zone_truth", "truth.zone_truth is required")
	}
	for zoneID, truth := range region.Truth.ZoneTruth {
		if _, ok := zoneIndex[zoneID]; !ok {
			return schemaError("truth.zone_truth", "truth references unknown zone %q", zoneID)
		}
		if !finite(truth.Density) || truth.Density < 0 {
			return schemaError("truth.zone_truth."+zoneID+".density", "truth density must be finite and non-negative")
		}
		if err := validateMix(truth.RarityMix, "truth.zone_truth."+zoneID+".rarity_mix"); err != nil {
			return err
		}
	}
	for i := range region.Zones {
		if _, ok := region.Truth.ZoneTruth[region.Zones[i].ID]; !ok {
			return schemaError("truth.zone_truth", "missing truth for zone %q", region.Zones[i].ID)
		}
	}
	if len(region.Truth.Points) == 0 {
		return schemaError("truth.points", "truth point list is required")
	}
	for i, point := range region.Truth.Points {
		if _, ok := zoneIndex[point.ZoneID]; !ok {
			return schemaError(fmt.Sprintf("truth.points[%d].zone_id", i), "unknown zone %q", point.ZoneID)
		}
		if !finite(point.X) || !finite(point.Y) {
			return schemaError(fmt.Sprintf("truth.points[%d]", i), "truth point coordinates must be finite")
		}
	}
	return nil
}

func validateMix(mix map[string]float64, path string) *Error {
	if len(mix) == 0 {
		return schemaError(path, "rarity mix must contain at least one rarity")
	}
	keys := sortedMapKeys(mix)
	sum := 0.0
	for _, key := range keys {
		value := mix[key]
		if key == "" {
			return schemaError(path, "rarity name is required")
		}
		if !finite(value) || value < 0 {
			return schemaError(path+"."+key, "rarity weight must be finite and non-negative")
		}
		sum += value
	}
	if sum <= 0 {
		return schemaError(path, "rarity mix total must be positive")
	}
	return nil
}

func normalizeRegion(region *Region) *Region {
	out := cloneJSON(*region)
	sort.Slice(out.Zones, func(i, j int) bool { return out.Zones[i].ID < out.Zones[j].ID })
	for i := range out.Zones {
		sort.Strings(out.Zones[i].DependsOn)
		out.Zones[i].TerrainTypes = sortedUniqueStrings(out.Zones[i].TerrainTypes)
	}
	sort.Slice(out.NoCollect, func(i, j int) bool { return out.NoCollect[i].ID < out.NoCollect[j].ID })
	sort.Slice(out.SampleSites, func(i, j int) bool { return out.SampleSites[i].ID < out.SampleSites[j].ID })
	sort.Slice(out.Truth.Points, func(i, j int) bool {
		if out.Truth.Points[i].ZoneID != out.Truth.Points[j].ZoneID {
			return out.Truth.Points[i].ZoneID < out.Truth.Points[j].ZoneID
		}
		if out.Truth.Points[i].X != out.Truth.Points[j].X {
			return out.Truth.Points[i].X < out.Truth.Points[j].X
		}
		return out.Truth.Points[i].Y < out.Truth.Points[j].Y
	})
	return &out
}

func terrainAt(region *Region, p Vec2) (string, bool) {
	if !pointInBounds(p, region.Bounds) {
		return "", false
	}
	r := region.TerrainRaster
	col := int(math.Floor((p.X - region.Bounds.MinX) / r.CellSize))
	row := int(math.Floor((p.Y - region.Bounds.MinY) / r.CellSize))
	if col == r.Cols && nearlyEqual(p.X, region.Bounds.MaxX) {
		col--
	}
	if row == r.Rows && nearlyEqual(p.Y, region.Bounds.MaxY) {
		row--
	}
	if col < 0 || col >= r.Cols || row < 0 || row >= r.Rows {
		return "", false
	}
	idx := r.Values[row*r.Cols+col]
	return region.TerrainTypes[idx], true
}

func zoneAt(region *Region, p Vec2) *Zone {
	for i := range region.Zones {
		if pointInPolygon(p, region.Zones[i].Polygon) {
			return &region.Zones[i]
		}
	}
	return nil
}

func findZone(region *Region, zoneID string) *Zone {
	for i := range region.Zones {
		if region.Zones[i].ID == zoneID {
			return &region.Zones[i]
		}
	}
	return nil
}

func collectNoCollect(region *Region) []NoCollect {
	out := append([]NoCollect(nil), region.NoCollect...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type zoneHit struct {
	ID     string
	Points []Vec2
}

func noCollectHit(noCollect []NoCollect, p Vec2) *zoneHit {
	sorted := append([]NoCollect(nil), noCollect...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for i := range sorted {
		if pointInPolygon(p, sorted[i].Polygon) {
			return &zoneHit{ID: sorted[i].ID, Points: sorted[i].Polygon}
		}
	}
	return nil
}

func topologyOrder(zones []Zone) ([]string, *Error) {
	byID := make(map[string]Zone, len(zones))
	indegree := make(map[string]int, len(zones))
	dependents := make(map[string][]string, len(zones))
	for _, zone := range zones {
		byID[zone.ID] = zone
		if _, ok := indegree[zone.ID]; !ok {
			indegree[zone.ID] = 0
		}
	}
	for _, zone := range zones {
		for _, dep := range sortedUniqueStrings(zone.DependsOn) {
			if _, ok := byID[dep]; !ok {
				return nil, schemaError("zones.depends_on", "unknown dependency %q for zone %q", dep, zone.ID)
			}
			indegree[zone.ID]++
			dependents[dep] = append(dependents[dep], zone.ID)
		}
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(zones))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		children := append([]string(nil), dependents[id]...)
		sort.Strings(children)
		for _, child := range children {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(zones) {
		return nil, schemaError("zones.depends_on", "zone dependency graph contains a cycle")
	}
	return order, nil
}

func downstreamZones(zones []Zone, roots []string) map[string]bool {
	children := make(map[string][]string)
	for _, zone := range zones {
		for _, dep := range zone.DependsOn {
			children[dep] = append(children[dep], zone.ID)
		}
	}
	out := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if out[id] {
			continue
		}
		out[id] = true
		next := append([]string(nil), children[id]...)
		sort.Strings(next)
		queue = append(queue, next...)
	}
	return out
}

func validateState(region *Region, state *WorkspaceState) *Error {
	if state == nil {
		return schemaError("state", "workspace state is required")
	}
	if len(state.ZoneParams) != len(region.Zones) {
		return schemaError("state.zone_params", "workspace state does not cover every zone")
	}
	for i := range region.Zones {
		zone := &region.Zones[i]
		params, ok := state.ZoneParams[zone.ID]
		if !ok {
			return schemaError("state.zone_params."+zone.ID, "missing zone parameters")
		}
		if !finite(params.DensityLimit) || params.DensityLimit < 0 {
			return schemaError("state.zone_params."+zone.ID+".density_limit", "invalid density limit")
		}
		if params.RarityMixOverride != nil {
			if err := validateMix(params.RarityMixOverride, "state.zone_params."+zone.ID+".rarity_mix_override"); err != nil {
				return err
			}
		}
	}
	seenNoCollect := make(map[string]struct{}, len(state.NoCollect))
	for i := range state.NoCollect {
		nc := &state.NoCollect[i]
		if nc.ID == "" {
			return schemaError(fmt.Sprintf("state.no_collect[%d].id", i), "no-collect id is required")
		}
		if _, duplicate := seenNoCollect[nc.ID]; duplicate {
			return schemaError(fmt.Sprintf("state.no_collect[%d].id", i), "duplicate no-collect id %q", nc.ID)
		}
		seenNoCollect[nc.ID] = struct{}{}
		if err := validatePolygon(nc.Polygon, "no_collect:"+nc.ID,
			fmt.Sprintf("state.no_collect[%d].polygon", i)); err != nil {
			return err
		}
		for _, point := range nc.Polygon {
			if !pointInBounds(point, region.Bounds) {
				return geomError("no_collect:"+nc.ID, "no-collect vertex lies outside region bounds")
			}
		}
		if math.Abs(polygonIntersectionArea(nc.Polygon, region.Boundary)-polygonArea(nc.Polygon)) > 1e-5 {
			return geomError("no_collect:"+nc.ID, "no-collect polygon extends outside the region boundary")
		}
	}
	for _, site := range region.SampleSites {
		if hit := noCollectHit(state.NoCollect, site.Point()); hit != nil {
			return sampleError("no_collect:"+hit.ID, site.Point(), "sample site lies inside a no-collect area")
		}
	}
	return nil
}

func defaultState(region *Region) WorkspaceState {
	state := WorkspaceState{
		ZoneParams: make(map[string]ZoneParams, len(region.Zones)),
		NoCollect:  cloneJSON(region.NoCollect),
	}
	for _, zone := range region.Zones {
		state.ZoneParams[zone.ID] = ZoneParams{DensityLimit: zone.DensityLimit}
	}
	if state.NoCollect == nil {
		state.NoCollect = []NoCollect{}
	}
	return state
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
