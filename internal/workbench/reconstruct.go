package workbench

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

func reconstructZone(
	region *Region,
	noCollect []NoCollect,
	zone *Zone,
	estimate ZoneEstimate,
	baseHash, paramVersion string,
	topologyOrder int,
) (*ZoneResult, *Error) {
	target := int(math.Round(estimate.Density * estimate.EffectiveArea))
	if target < 0 {
		target = 0
	}
	if region.Sampling.MaxPointsPerZone > 0 && target > region.Sampling.MaxPointsPerZone {
		target = region.Sampling.MaxPointsPerZone
	}
	points := make([]Point, 0, target)
	maxAttempts := target*80 + 4096
	if maxAttempts < 8192 {
		maxAttempts = 8192
	}
	minX, minY, maxX, maxY := zoneBounds(zone.Polygon)
	rarityKeys := sortedMapKeys(estimate.RarityMix)
	if len(rarityKeys) == 0 {
		rarityKeys = []string{"common"}
		estimate.RarityMix = map[string]float64{"common": 1}
	}
	for attempt := 0; attempt < maxAttempts && len(points) < target; attempt++ {
		u, v := deterministicPair(baseHash, paramVersion, zone.ID, fmt.Sprintf("point/%d", attempt))
		p := Vec2{X: minX + u*(maxX-minX), Y: minY + v*(maxY-minY)}
		if !pointInPolygon(p, zone.Polygon) {
			continue
		}
		if !pointInPolygon(p, region.Boundary) {
			continue
		}
		if noCollectHit(noCollect, p) != nil {
			continue
		}
		terrain, ok := terrainAt(region, p)
		if !ok || !containsString(zone.TerrainTypes, terrain) {
			continue
		}
		rarity := pickWeighted(rarityKeys, estimate.RarityMix,
			deterministicUnit(baseHash, paramVersion, zone.ID, fmt.Sprintf("rarity/%d", len(points))))
		points = append(points, Point{
			ID:          fmt.Sprintf("%s:p:%06d", zone.ID, len(points)),
			ZoneID:      zone.ID,
			X:           p.X,
			Y:           p.Y,
			TerrainType: terrain,
			Rarity:      rarity,
		})
	}
	if len(points) < target {
		return nil, geomError("zone:"+zone.ID,
			"deterministic reconstruction could only place %d/%d points after %d attempts",
			len(points), target, maxAttempts)
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].X != points[j].X {
			return points[i].X < points[j].X
		}
		if points[i].Y != points[j].Y {
			return points[i].Y < points[j].Y
		}
		if points[i].Rarity != points[j].Rarity {
			return points[i].Rarity < points[j].Rarity
		}
		return points[i].ID < points[j].ID
	})
	heatLayer, err := buildHeatLayer(region, noCollect, zone, estimate.Density)
	if err != nil {
		return nil, err
	}
	result := &ZoneResult{
		ZoneID:        zone.ID,
		TopologyOrder: topologyOrder,
		Estimate:      estimate,
		Points:        points,
		PointCount:    len(points),
		PointDigest:   stableDigest(points),
		HeatLayer:     heatLayer,
		HeatDigest:    stableDigest(heatLayer),
		ParamVersion:  paramVersion,
	}
	result.Metrics = ZoneMetrics{
		DensityQuantizationError:   metricDensityQuantization(region, result),
		StandardError:              estimate.StandardError,
		RarityRoundingResidual:     estimate.RarityResidual,
		HeatmapReconstructionError: metricHeatError(region, result),
	}
	return result, nil
}

func zoneBounds(polygon []Vec2) (float64, float64, float64, float64) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range polygon {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
		maxX = math.Max(maxX, p.X)
		maxY = math.Max(maxY, p.Y)
	}
	return minX, minY, maxX, maxY
}

func deterministicPair(parts ...string) (float64, float64) {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	a := binary.BigEndian.Uint64(sum[0:8])
	b := binary.BigEndian.Uint64(sum[8:16])
	return float64(a>>11) / float64(uint64(1)<<53), float64(b>>11) / float64(uint64(1)<<53)
}

func deterministicUnit(parts ...string) float64 {
	u, _ := deterministicPair(parts...)
	return u
}

func pickWeighted(keys []string, weights map[string]float64, unit float64) string {
	total := 0.0
	for _, key := range keys {
		if weights[key] > 0 {
			total += weights[key]
		}
	}
	if total <= 0 {
		return keys[0]
	}
	target := unit * total
	acc := 0.0
	for _, key := range keys {
		weight := weights[key]
		if weight <= 0 {
			continue
		}
		acc += weight
		if target <= acc+1e-12 {
			return key
		}
	}
	return keys[len(keys)-1]
}

func buildHeatLayer(region *Region, noCollect []NoCollect, zone *Zone, density float64) (map[int]float64, *Error) {
	grid := heatGrid(region)
	layer := make(map[int]float64)
	for row := 0; row < grid.Rows; row++ {
		for col := 0; col < grid.Cols; col++ {
			x := grid.Bounds.MinX + (float64(col)+0.5)*grid.CellSize
			y := grid.Bounds.MinY + (float64(row)+0.5)*grid.CellSize
			p := Vec2{X: x, Y: y}
			if !pointInPolygon(p, zone.Polygon) {
				continue
			}
			if noCollectHit(noCollect, p) != nil {
				continue
			}
			terrain, ok := terrainAt(region, p)
			if !ok || !containsString(zone.TerrainTypes, terrain) {
				continue
			}
			idx := row*grid.Cols + col
			layer[idx] = density
		}
	}
	// Preserve observed sample cells at zone edges. A cell-center-only
	// rasterization can miss a narrow edge cell; the declared observations must
	// remain reproducible on the fixed heat grid.
	for _, site := range region.SampleSites {
		if !pointInPolygon(site.Point(), zone.Polygon) {
			continue
		}
		if noCollectHit(noCollect, site.Point()) != nil {
			continue
		}
		terrain, ok := terrainAt(region, site.Point())
		if !ok || !containsString(zone.TerrainTypes, terrain) {
			continue
		}
		col := int((site.X - grid.Bounds.MinX) / grid.CellSize)
		row := int((site.Y - grid.Bounds.MinY) / grid.CellSize)
		if col >= 0 && col < grid.Cols && row >= 0 && row < grid.Rows {
			layer[row*grid.Cols+col] = density
		}
	}
	return layer, nil
}

func heatGrid(region *Region) Heatmap {
	cell := region.Sampling.HeatmapCellSize
	cols := int(math.Ceil(region.Bounds.Width() / cell))
	rows := int(math.Ceil(region.Bounds.Height() / cell))
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	return Heatmap{
		Bounds:   region.Bounds,
		CellSize: cell,
		Cols:     cols,
		Rows:     rows,
		Values:   make([]float64, rows*cols),
	}
}

func mergeHeatmap(region *Region, zones []*ZoneResult) Heatmap {
	out := heatGrid(region)
	for _, zone := range zones {
		for idx, value := range zone.HeatLayer {
			if idx >= 0 && idx < len(out.Values) {
				out.Values[idx] = value
			}
		}
	}
	return out
}

func metricDensityQuantization(region *Region, result *ZoneResult) float64 {
	total := 0.0
	count := 0
	for _, siteID := range result.SampleSelection.SiteIDs {
		site := findSampleSite(region, siteID)
		if site == nil {
			continue
		}
		total += math.Abs(site.ObservedDensity - quantizeValue(site.ObservedDensity, region.Sampling.DensityStep))
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

func metricHeatError(region *Region, result *ZoneResult) float64 {
	total := 0.0
	count := 0
	for _, siteID := range result.SampleSelection.SiteIDs {
		site := findSampleSite(region, siteID)
		if site == nil {
			continue
		}
		total += math.Abs(result.Estimate.Density - site.ObservedDensity)
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}
