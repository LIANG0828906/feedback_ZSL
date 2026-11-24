package workbench

import (
	"math"
	"sort"
)

type zoneModel struct {
	Zone          *Zone
	TerrainAreas  map[string]float64
	EffectiveArea float64
}

func buildZoneModel(region *Region, noCollect []NoCollect, zone *Zone) zoneModel {
	r := region.TerrainRaster
	cellArea := r.CellSize * r.CellSize
	allowed := make(map[string]bool, len(zone.TerrainTypes))
	for _, terrain := range zone.TerrainTypes {
		allowed[terrain] = true
	}
	areas := make(map[string]float64)
	total := 0.0
	for row := 0; row < r.Rows; row++ {
		for col := 0; col < r.Cols; col++ {
			p := Vec2{
				X: region.Bounds.MinX + (float64(col)+0.5)*r.CellSize,
				Y: region.Bounds.MinY + (float64(row)+0.5)*r.CellSize,
			}
			if !pointInPolygon(p, zone.Polygon) {
				continue
			}
			if noCollectHit(noCollect, p) != nil {
				continue
			}
			terrain := region.TerrainTypes[r.Values[row*r.Cols+col]]
			if !allowed[terrain] {
				continue
			}
			areas[terrain] += cellArea
			total += cellArea
		}
	}
	return zoneModel{Zone: zone, TerrainAreas: areas, EffectiveArea: total}
}

func (e *Engine) sampleZone(zone *Zone, model zoneModel) (ZoneEstimate, SampleSelection, *Error) {
	selection := SampleSelection{Indices: []int{}, SiteIDs: []string{}, Strata: []StratumSelection{}}
	if model.EffectiveArea <= 0 {
		return ZoneEstimate{}, selection, geomError("zone:"+zone.ID, "zone has no usable terrain area")
	}
	byTerrain := make(map[string][]int)
	for idx, site := range e.region.SampleSites {
		if noCollectHit(e.state.NoCollect, site.Point()) != nil {
			return ZoneEstimate{}, selection, sampleError("no_collect", site.Point(), "sample site is inside a no-collect area")
		}
		if !pointInPolygon(site.Point(), zone.Polygon) {
			continue
		}
		terrain, ok := terrainAt(e.region, site.Point())
		if !ok {
			return ZoneEstimate{}, selection, sampleError("region:"+e.region.RegionID, site.Point(), "sample site has no raster cell")
		}
		if !containsString(zone.TerrainTypes, terrain) {
			continue
		}
		byTerrain[terrain] = append(byTerrain[terrain], idx)
	}
	if len(byTerrain) == 0 {
		return ZoneEstimate{}, selection, sampleError("zone:"+zone.ID, Vec2{}, "no valid sample sites are available for zone")
	}
	terrainKeys := make([]string, 0, len(byTerrain))
	for terrain, indices := range byTerrain {
		if len(indices) == 0 {
			continue
		}
		sort.Ints(indices)
		byTerrain[terrain] = indices
		terrainKeys = append(terrainKeys, terrain)
	}
	sort.Strings(terrainKeys)
	zoneTarget, err := e.allocateZoneSamples(zone, byTerrain)
	if err != nil {
		return ZoneEstimate{}, selection, err
	}
	if zoneTarget > len(e.region.SampleSites) {
		zoneTarget = len(e.region.SampleSites)
	}
	stratumWeights := make(map[string]float64, len(terrainKeys))
	stratumCaps := make(map[string]int, len(terrainKeys))
	for _, terrain := range terrainKeys {
		stratumWeights[terrain] = model.TerrainAreas[terrain]
		stratumCaps[terrain] = len(byTerrain[terrain])
	}
	stratumCounts := allocateStrata(terrainKeys, stratumWeights, stratumCaps, zoneTarget, e.region.Sampling.MinPerStratum)
	selected := make([]int, 0, zoneTarget)
	strata := make([]StratumSelection, 0, len(terrainKeys))
	for _, terrain := range terrainKeys {
		count := stratumCounts[terrain]
		if count <= 0 {
			continue
		}
		indices := append([]int(nil), byTerrain[terrain]...)
		sort.Slice(indices, func(i, j int) bool {
			left := e.region.SampleSites[indices[i]]
			right := e.region.SampleSites[indices[j]]
			lk := rankKey(e.baseHash, "sample", zone.ID, terrain, left.ID)
			rk := rankKey(e.baseHash, "sample", zone.ID, terrain, right.ID)
			if lk != rk {
				return lk < rk
			}
			return left.ID < right.ID
		})
		if count > len(indices) {
			count = len(indices)
		}
		chosen := indices[:count]
		sort.Ints(chosen)
		selected = append(selected, chosen...)
		siteIDs := make([]string, 0, len(chosen))
		for _, idx := range chosen {
			siteIDs = append(siteIDs, e.region.SampleSites[idx].ID)
		}
		sort.Strings(siteIDs)
		strata = append(strata, StratumSelection{
			TerrainType: terrain,
			Area:        model.TerrainAreas[terrain],
			SiteIDs:     siteIDs,
		})
	}
	sort.Ints(selected)
	siteIDs := make([]string, 0, len(selected))
	for _, idx := range selected {
		siteIDs = append(siteIDs, e.region.SampleSites[idx].ID)
	}
	selection = SampleSelection{Indices: selected, SiteIDs: siteIDs, Strata: strata}

	estimate := estimateZone(e.region, model, selection, zone, e.state.ZoneParams[zone.ID])
	return estimate, selection, nil
}

func (e *Engine) allocateZoneSamples(zone *Zone, byTerrain map[string][]int) (int, *Error) {
	order, err := topologyOrder(e.region.Zones)
	if err != nil {
		return 0, err
	}
	desired := make(map[string]int, len(e.region.Zones))
	weights := make(map[string]float64, len(e.region.Zones))
	caps := make(map[string]int, len(e.region.Zones))
	for _, zoneID := range order {
		z := findZone(e.region, zoneID)
		model := buildZoneModel(e.region, e.state.NoCollect, z)
		candidateCount := 0
		if zoneID == zone.ID {
			for _, indices := range byTerrain {
				candidateCount += len(indices)
			}
		} else {
			for idx := range e.region.SampleSites {
				site := e.region.SampleSites[idx]
				if !pointInPolygon(site.Point(), z.Polygon) {
					continue
				}
				if noCollectHit(e.state.NoCollect, site.Point()) != nil {
					continue
				}
				terrain, ok := terrainAt(e.region, site.Point())
				if ok && containsString(z.TerrainTypes, terrain) {
					candidateCount++
				}
			}
		}
		capValue := z.SampleCountLimit
		if capValue <= 0 {
			capValue = candidateCount
		}
		if candidateCount < capValue {
			capValue = candidateCount
		}
		if capValue < 0 {
			capValue = 0
		}
		desired[zoneID] = capValue
		caps[zoneID] = capValue
		weights[zoneID] = model.EffectiveArea
	}
	totalDesired := 0
	for _, value := range desired {
		totalDesired += value
	}
	target := totalDesired
	if target > e.region.Sampling.MaxSamples {
		allocated := allocateProportional(order, weights, caps, e.region.Sampling.MaxSamples)
		target = allocated[zone.ID]
	} else {
		target = desired[zone.ID]
	}
	if target < 0 {
		target = 0
	}
	return target, nil
}

func allocateStrata(keys []string, weights map[string]float64, caps map[string]int, target, minPer int) map[string]int {
	out := make(map[string]int, len(keys))
	eligible := make([]string, 0, len(keys))
	for _, key := range keys {
		if caps[key] > 0 {
			eligible = append(eligible, key)
		}
	}
	sort.Strings(eligible)
	if target <= 0 || len(eligible) == 0 {
		return out
	}
	if minPer < 1 {
		minPer = 1
	}
	base := make(map[string]int, len(eligible))
	used := 0
	if target >= len(eligible)*minPer {
		for _, key := range eligible {
			n := minPer
			if n > caps[key] {
				n = caps[key]
			}
			base[key] = n
			used += n
		}
	} else {
		type areaEntry struct {
			key  string
			area float64
		}
		entries := make([]areaEntry, 0, len(eligible))
		for _, key := range eligible {
			entries = append(entries, areaEntry{key: key, area: weights[key]})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].area != entries[j].area {
				return entries[i].area > entries[j].area
			}
			return entries[i].key < entries[j].key
		})
		for i := 0; i < target && i < len(entries); i++ {
			base[entries[i].key] = 1
			used++
		}
	}
	remaining := target - used
	if remaining > 0 {
		extraCaps := make(map[string]int, len(eligible))
		for _, key := range eligible {
			extraCaps[key] = caps[key] - base[key]
		}
		extra := allocateProportional(eligible, weights, extraCaps, remaining)
		for _, key := range eligible {
			base[key] += extra[key]
		}
	}
	for _, key := range eligible {
		if base[key] > caps[key] {
			base[key] = caps[key]
		}
		out[key] = base[key]
	}
	return out
}

func estimateZone(region *Region, model zoneModel, selection SampleSelection, zone *Zone, params ZoneParams) ZoneEstimate {
	weightedSum := 0.0
	weightTotal := 0.0
	varianceSum := 0.0
	rarityCounts := make(map[string]int)
	strataEstimate := make([]StratumEstimate, 0, len(selection.Strata))
	for _, stratum := range selection.Strata {
		n := len(stratum.SiteIDs)
		if n == 0 {
			continue
		}
		mean := 0.0
		values := make([]float64, 0, n)
		for _, siteID := range stratum.SiteIDs {
			site := findSampleSite(region, siteID)
			if site == nil {
				continue
			}
			mean += site.ObservedDensity
			values = append(values, site.ObservedDensity)
			for rarity, count := range site.RarityCounts {
				rarityCounts[rarity] += count
			}
		}
		if len(values) == 0 {
			continue
		}
		mean /= float64(len(values))
		variance := 0.0
		if len(values) > 1 {
			for _, value := range values {
				delta := value - mean
				variance += delta * delta
			}
			variance /= float64(len(values) - 1)
		}
		se := math.Sqrt(variance / float64(len(values)))
		weight := stratum.Area
		if weight <= 0 {
			weight = float64(len(values))
		}
		weightedSum += weight * mean
		weightTotal += weight
		varianceSum += weight * weight * se * se
		strataEstimate = append(strataEstimate, StratumEstimate{
			TerrainType:   stratum.TerrainType,
			Area:          stratum.Area,
			SampleCount:   len(values),
			Density:       mean,
			StandardError: se,
		})
	}
	density := 0.0
	se := 0.0
	if weightTotal > 0 {
		density = weightedSum / weightTotal
		se = math.Sqrt(varianceSum) / weightTotal
	}
	observedMix := normalizedCounts(rarityCounts)
	if len(observedMix) == 0 {
		observedMix = normalizeMix(zone.RarityMix)
	}
	effectiveMix := observedMix
	if params.RarityMixOverride != nil {
		effectiveMix = normalizeMix(params.RarityMixOverride)
	}
	quantizedMix, residual := quantizeMix(effectiveMix, region.Sampling.RarityStep)
	rawDensity := density
	density = math.Min(density, params.DensityLimit)
	heatError := 0.0
	if len(selection.SiteIDs) > 0 {
		for _, siteID := range selection.SiteIDs {
			site := findSampleSite(region, siteID)
			if site != nil {
				heatError += math.Abs(density - site.ObservedDensity)
			}
		}
		heatError /= float64(len(selection.SiteIDs))
	}
	return ZoneEstimate{
		Density:        density,
		RawDensity:     rawDensity,
		DensityLimit:   params.DensityLimit,
		StandardError:  se,
		Strata:         strataEstimate,
		RarityMix:      quantizedMix,
		ObservedRarity: observedMix,
		RarityResidual: residual,
		EffectiveArea:  model.EffectiveArea,
		SampleCount:    len(selection.SiteIDs),
		// Reuse ZoneMetrics fields for observed diagnostics before reconstruction.
		// They are overwritten with the final heat error after reconstruction.
	}
}

func normalizedCounts(counts map[string]int) map[string]float64 {
	total := sumIntMap(counts)
	out := make(map[string]float64, len(counts))
	if total <= 0 {
		return out
	}
	for key, count := range counts {
		out[key] = float64(count) / float64(total)
	}
	return out
}

func findSampleSite(region *Region, id string) *SampleSite {
	for i := range region.SampleSites {
		if region.SampleSites[i].ID == id {
			return &region.SampleSites[i]
		}
	}
	return nil
}
