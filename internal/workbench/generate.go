package workbench

import (
	"fmt"
	"math"
	"sort"
)

// GenerateSample returns the deterministic local validation region. The hidden
// truth is produced from the same geometry, but never read by the estimator.
func GenerateSample(seed uint64) (*Region, *Error) {
	region := &Region{
		SchemaVersion: SchemaVersion,
		RegionID:      "verdant-reach",
		DisplayName:   "Verdant Reach Resource Survey",
		Seed:          seed,
		Bounds:        Bounds{MinX: 0, MinY: 0, MaxX: 160, MaxY: 120},
		Boundary:      closedRect(0, 0, 160, 120),
		TerrainTypes:  []string{"grass", "forest", "rock", "crystal"},
		TerrainRaster: TerrainRaster{
			CellSize: 10,
			Cols:     16,
			Rows:     12,
			Values:   make([]int, 16*12),
		},
		Zones: []Zone{
			{
				ID:               "zone-1-west",
				Name:             "Western Meadow",
				Polygon:          closedRect(0, 0, 70, 120),
				TerrainTypes:     []string{"grass", "forest"},
				DensityLimit:     0.20,
				RarityMix:        map[string]float64{"common": 0.70, "uncommon": 0.22, "rare": 0.08},
				SampleCountLimit: 10,
			},
			{
				ID:               "zone-2-northeast",
				Name:             "North East Forest",
				Polygon:          closedRect(70, 0, 160, 60),
				TerrainTypes:     []string{"forest", "rock"},
				DensityLimit:     0.15,
				RarityMix:        map[string]float64{"common": 0.55, "uncommon": 0.30, "rare": 0.15},
				SampleCountLimit: 8,
			},
			{
				ID:               "zone-3-southeast",
				Name:             "South East Crystal Scar",
				Polygon:          closedRect(70, 60, 160, 120),
				TerrainTypes:     []string{"rock", "crystal"},
				DensityLimit:     0.10,
				RarityMix:        map[string]float64{"common": 0.40, "uncommon": 0.35, "rare": 0.25},
				SampleCountLimit: 7,
				DependsOn:        []string{"zone-2-northeast"},
			},
		},
		NoCollect: []NoCollect{
			{ID: "nc-grove", Name: "Protected Grove", Polygon: closedRect(45, 35, 60, 50)},
			{ID: "nc-scar", Name: "Unstable Scar", Polygon: closedRect(115, 85, 130, 100)},
		},
		Sampling: SamplingConfig{
			GridSpacing:           5,
			MaxSamples:            40,
			MinPerStratum:         1,
			PerThreadSampleBuffer: 16,
			MaxParallel:           4,
			DensityStep:           0.001,
			RarityStep:            0.01,
			DensityTolerance:      0.02,
			CountTolerance:        0.02,
			HeatmapTolerance:      0.02,
			BiasThreshold:         0.50,
			HeatmapCellSize:       5,
			MaxPointsPerZone:      5000,
		},
		Truth: Truth{ZoneTruth: map[string]ZoneTruth{}},
	}
	for row := 0; row < region.TerrainRaster.Rows; row++ {
		for col := 0; col < region.TerrainRaster.Cols; col++ {
			value := 0
			if col < 7 {
				if row < 6 {
					value = 0
				} else {
					value = 1
				}
			} else if row < 6 {
				if (col+row)%2 == 0 {
					value = 1
				} else {
					value = 2
				}
			} else {
				if (col+row)%2 == 0 {
					value = 2
				} else {
					value = 3
				}
			}
			region.TerrainRaster.Values[row*region.TerrainRaster.Cols+col] = value
		}
	}
	truths := map[string]ZoneTruth{
		"zone-1-west":      {Density: 0.12, RarityMix: map[string]float64{"common": 0.70, "uncommon": 0.22, "rare": 0.08}},
		"zone-2-northeast": {Density: 0.08, RarityMix: map[string]float64{"common": 0.55, "uncommon": 0.30, "rare": 0.15}},
		"zone-3-southeast": {Density: 0.05, RarityMix: map[string]float64{"common": 0.40, "uncommon": 0.35, "rare": 0.25}},
	}
	region.Truth.ZoneTruth = truths

	candidateTargets := map[string]int{
		"zone-1-west":      16,
		"zone-2-northeast": 12,
		"zone-3-southeast": 12,
	}
	for _, zoneID := range sortedZoneIDs(region.Zones) {
		zone := findZone(region, zoneID)
		candidates := make([]Vec2, 0)
		for y := region.Bounds.MinY + region.Sampling.GridSpacing/2; y < region.Bounds.MaxY; y += region.Sampling.GridSpacing {
			for x := region.Bounds.MinX + region.Sampling.GridSpacing/2; x < region.Bounds.MaxX; x += region.Sampling.GridSpacing {
				p := Vec2{X: x, Y: y}
				if !pointInPolygon(p, zone.Polygon) || noCollectHit(region.NoCollect, p) != nil {
					continue
				}
				terrain, ok := terrainAt(region, p)
				if !ok || !containsString(zone.TerrainTypes, terrain) {
					continue
				}
				candidates = append(candidates, p)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			left := rankKey(fmt.Sprint(seed), "candidate", zoneID, fmt.Sprintf("%.8f/%.8f", candidates[i].X, candidates[i].Y))
			right := rankKey(fmt.Sprint(seed), "candidate", zoneID, fmt.Sprintf("%.8f/%.8f", candidates[j].X, candidates[j].Y))
			if left != right {
				return left < right
			}
			if candidates[i].X != candidates[j].X {
				return candidates[i].X < candidates[j].X
			}
			return candidates[i].Y < candidates[j].Y
		})
		count := candidateTargets[zoneID]
		if count > len(candidates) {
			count = len(candidates)
		}
		for i := 0; i < count; i++ {
			p := candidates[i]
			terrain, _ := terrainAt(region, p)
			truth := truths[zoneID]
			jitter := (float64(hashByte(seed, zoneID, fmt.Sprintf("jitter/%d", i))%9) - 4) * 0.0004
			density := truth.Density + jitter
			if density < 0 {
				density = 0
			}
			counts := integerMix(truth.RarityMix, 100)
			region.SampleSites = append(region.SampleSites, SampleSite{
				ID:              fmt.Sprintf("site-%s-%03d", zoneID, i+1),
				X:               p.X,
				Y:               p.Y,
				TerrainType:     terrain,
				ObservedDensity: density,
				RarityCounts:    counts,
			})
		}
	}
	sort.Slice(region.SampleSites, func(i, j int) bool { return region.SampleSites[i].ID < region.SampleSites[j].ID })
	if len(region.SampleSites) > region.Sampling.MaxSamples {
		return nil, schemaError("sample_sites", "generated candidate count exceeds max_samples")
	}

	for order, zoneID := range sortedZoneIDs(region.Zones) {
		zone := findZone(region, zoneID)
		model := buildZoneModel(region, region.NoCollect, zone)
		truth := truths[zoneID]
		estimate := ZoneEstimate{
			Density:       truth.Density,
			RarityMix:     truth.RarityMix,
			EffectiveArea: model.EffectiveArea,
		}
		result, err := reconstructZone(region, region.NoCollect, zone, estimate,
			fmt.Sprintf("truth-%d-%s", seed, region.RegionID), "truth-v1", order)
		if err != nil {
			return nil, err
		}
		for _, point := range result.Points {
			region.Truth.Points = append(region.Truth.Points, TruthPoint{
				ZoneID:      zoneID,
				X:           point.X,
				Y:           point.Y,
				TerrainType: point.TerrainType,
				Rarity:      point.Rarity,
			})
		}
	}
	sort.Slice(region.Truth.Points, func(i, j int) bool {
		if region.Truth.Points[i].ZoneID != region.Truth.Points[j].ZoneID {
			return region.Truth.Points[i].ZoneID < region.Truth.Points[j].ZoneID
		}
		if region.Truth.Points[i].X != region.Truth.Points[j].X {
			return region.Truth.Points[i].X < region.Truth.Points[j].X
		}
		return region.Truth.Points[i].Y < region.Truth.Points[j].Y
	})
	if err := ValidateRegion(region); err != nil {
		return nil, err
	}
	return normalizeRegion(region), nil
}

func closedRect(minX, minY, maxX, maxY float64) []Vec2 {
	return []Vec2{
		{X: minX, Y: minY},
		{X: maxX, Y: minY},
		{X: maxX, Y: maxY},
		{X: minX, Y: maxY},
		{X: minX, Y: minY},
	}
}

func integerMix(mix map[string]float64, total int) map[string]int {
	keys := sortedMapKeys(mix)
	normalized := normalizeMix(mix)
	out := make(map[string]int, len(keys))
	used := 0
	type rem struct {
		key   string
		value float64
	}
	rems := make([]rem, 0, len(keys))
	for _, key := range keys {
		exact := normalized[key] * float64(total)
		n := int(math.Floor(exact))
		out[key] = n
		used += n
		rems = append(rems, rem{key: key, value: exact - float64(n)})
	}
	sort.Slice(rems, func(i, j int) bool {
		if rems[i].value != rems[j].value {
			return rems[i].value > rems[j].value
		}
		return rems[i].key < rems[j].key
	})
	for used < total {
		for _, item := range rems {
			if used >= total {
				break
			}
			out[item.key]++
			used++
		}
	}
	return out
}

func hashByte(seed uint64, parts ...string) byte {
	key := rankKey(fmt.Sprint(seed))
	for _, part := range parts {
		key = rankKey(key, part)
	}
	if key == "" {
		return 0
	}
	return key[0]
}
