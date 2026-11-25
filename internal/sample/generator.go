package sample

import (
	"fmt"
	"math"
	"sort"

	"atelier/internal/geometry"
	"atelier/internal/hashutil"
	"atelier/internal/model"
)

const (
	DefaultSeed int64 = 2042
	worldWidth        = 240.0
	worldHeight       = 160.0
)

var terrainDensityMultiplier = map[string]float64{
	"forest": 1.10,
	"meadow": 0.96,
	"ridge":  0.84,
	"marsh":  0.76,
}

type zoneSpec struct {
	ID          string
	Name        string
	Ordinal     int
	BaseDensity float64
	Rarity      map[string]float64
	Vertices    []model.Point
}

func Generate(seed int64) model.Scenario {
	if seed == 0 {
		seed = DefaultSeed
	}
	profile := profileForSeed(seed)
	region := buildRegion(profile)
	cfg := model.SamplingConfig{
		SampleGridStep:      4,
		SamplesPerZone:      36,
		HeatmapCellSize:     4,
		HeatmapTileSize:     8,
		MaxParallelism:      4,
		PerWorkerBufferSize: 64,
		QuantizationStep:    0.005,
	}
	observations := buildObservations(region, cfg, seed, profile)
	truth := buildTruth(region, seed, profile)
	return model.Scenario{
		SchemaVersion: model.SchemaVersion,
		Seed:          seed,
		Region:        region,
		Config:        cfg,
		Observations:  observations,
		Truth:         truth,
	}
}

func buildRegion(profile int) model.Region {
	if profile != 0 {
		return buildVariantRegion(profile)
	}
	boundary := polygon(
		model.Point{X: 0, Y: 0},
		model.Point{X: worldWidth, Y: 0},
		model.Point{X: worldWidth, Y: worldHeight},
		model.Point{X: 0, Y: worldHeight},
		model.Point{X: 0, Y: 0},
	)
	specs := []zoneSpec{
		{
			ID: "west-woods", Name: "西侧林地", Ordinal: 0,
			BaseDensity: 0.105,
			Rarity:      map[string]float64{"common": 0.62, "uncommon": 0.27, "rare": 0.11},
			Vertices: []model.Point{
				{X: 0, Y: 0}, {X: 88, Y: 0}, {X: 82, Y: 72}, {X: 0, Y: 80}, {X: 0, Y: 0},
			},
		},
		{
			ID: "central-meadow", Name: "中央草甸", Ordinal: 1,
			BaseDensity: 0.136,
			Rarity:      map[string]float64{"common": 0.70, "uncommon": 0.22, "rare": 0.08},
			Vertices: []model.Point{
				{X: 88, Y: 0}, {X: 160, Y: 0}, {X: 150, Y: 78}, {X: 82, Y: 72}, {X: 88, Y: 0},
			},
		},
		{
			ID: "east-ridge", Name: "东部山脊", Ordinal: 2,
			BaseDensity: 0.082,
			Rarity:      map[string]float64{"common": 0.55, "uncommon": 0.30, "rare": 0.15},
			Vertices: []model.Point{
				{X: 160, Y: 0}, {X: 240, Y: 0}, {X: 240, Y: 88}, {X: 150, Y: 78}, {X: 160, Y: 0},
			},
		},
		{
			ID: "south-marsh", Name: "南部湿地", Ordinal: 3,
			BaseDensity: 0.094,
			Rarity:      map[string]float64{"common": 0.66, "uncommon": 0.24, "rare": 0.10},
			Vertices: []model.Point{
				{X: 0, Y: 80}, {X: 82, Y: 72}, {X: 150, Y: 78}, {X: 240, Y: 88},
				{X: 240, Y: 160}, {X: 0, Y: 160}, {X: 0, Y: 80},
			},
		},
	}
	zones := make([]model.Zone, 0, len(specs))
	for _, spec := range specs {
		zones = append(zones, model.Zone{
			ID:      spec.ID,
			Name:    spec.Name,
			Ordinal: spec.Ordinal,
			Polygon: polygon(spec.Vertices...),
			Params:  model.ZoneParams{DensityScale: 1, RarityShift: 0},
		})
	}
	exclusions := []model.Exclusion{
		{
			ID: "mirror-lake", Name: "镜湖", Reason: "水域禁采",
			Polygon: polygon(
				model.Point{X: 86, Y: 104}, model.Point{X: 126, Y: 98},
				model.Point{X: 136, Y: 126}, model.Point{X: 104, Y: 138},
				model.Point{X: 80, Y: 122}, model.Point{X: 86, Y: 104},
			),
		},
		{
			ID: "old-ruins", Name: "旧遗迹", Reason: "保护遗迹",
			Polygon: polygon(
				model.Point{X: 110, Y: 18}, model.Point{X: 141, Y: 20},
				model.Point{X: 138, Y: 46}, model.Point{X: 107, Y: 44},
				model.Point{X: 110, Y: 18},
			),
		},
		{
			ID: "sacred-spring", Name: "圣泉", Reason: "仪式区域",
			Polygon: polygon(
				model.Point{X: 30, Y: 27}, model.Point{X: 56, Y: 25},
				model.Point{X: 60, Y: 48}, model.Point{X: 34, Y: 50},
				model.Point{X: 30, Y: 27},
			),
		},
	}
	return model.Region{
		ID:         "frontier-basin",
		Name:       "边境盆地",
		Boundary:   boundary,
		Zones:      zones,
		Exclusions: exclusions,
		Terrain:    buildTerrain(boundary.Bounds(), profile),
	}
}

func buildTerrain(b model.Bounds, profile int) model.TerrainRaster {
	const cell = 4.0
	width := int(math.Ceil(b.Width() / cell))
	height := int(math.Ceil(b.Height() / cell))
	types := make([]string, 0, width*height)
	for row := 0; row < height; row++ {
		for col := 0; col < width; col++ {
			x := b.MinX + (float64(col)+0.5)*cell
			y := b.MinY + (float64(row)+0.5)*cell
			types = append(types, terrainAt(x, y, profile))
		}
	}
	return model.TerrainRaster{
		CellSize: cell,
		OriginX:  b.MinX,
		OriginY:  b.MinY,
		Width:    width,
		Height:   height,
		Types:    types,
	}
}

func terrainAt(x, y float64, profile int) string {
	if profile != 0 {
		return variantTerrainAt(x, y, profile)
	}
	switch {
	case x < 78:
		return "forest"
	case x < 150:
		return "meadow"
	case x < 198:
		return "ridge"
	default:
		return "marsh"
	}
}

func buildTruth(region model.Region, seed int64, profile int) model.TruthManifest {
	specs := truthSpecs(profile)
	zones := orderedZones(region)
	manifest := model.TruthManifest{RegionID: region.ID}
	for _, zone := range zones {
		spec := specs[zone.ID]
		areas := zoneTerrainAreas(region, zone)
		zt := model.ZoneTruth{
			ZoneID:       zone.ID,
			BaseDensity:  spec.BaseDensity,
			Rarity:       cloneRatios(spec.Rarity),
			TerrainCount: map[string]int{},
		}
		terrains := sortedKeys(areas)
		for _, terrain := range terrains {
			area := areas[terrain]
			if area <= 0 {
				continue
			}
			density := spec.BaseDensity * terrainDensityMultiplier[terrain]
			count := int(math.Round(density * area))
			points := sampleTruthPoints(region, zone, terrain, count, density, seed, spec.Rarity)
			manifest.Points = append(manifest.Points, points...)
			zt.TerrainCount[terrain] = count
			zt.PointCount += count
		}
		manifest.Zones = append(manifest.Zones, zt)
	}
	sort.SliceStable(manifest.Points, func(i, j int) bool {
		a, b := manifest.Points[i], manifest.Points[j]
		oa, ob := zoneOrdinal(region, a.ZoneID), zoneOrdinal(region, b.ZoneID)
		if oa != ob {
			return oa < ob
		}
		if a.X != b.X {
			return a.X < b.X
		}
		return a.Y < b.Y
	})
	counters := map[string]int{}
	for i := range manifest.Points {
		p := &manifest.Points[i]
		counters[p.ZoneID]++
		p.ID = fmt.Sprintf("truth-%s-%04d", p.ZoneID, counters[p.ZoneID])
	}
	return manifest
}

func sampleTruthPoints(region model.Region, zone model.Zone, terrain string, count int, density float64, seed int64, rarity map[string]float64) []model.TruthPoint {
	if count <= 0 {
		return nil
	}
	b := region.Boundary.Bounds()
	points := make([]model.TruthPoint, 0, count)
	labels := rarityLabels(count, rarity)
	attempt := uint64(1)
	rejects := 0
	for len(points) < count {
		u := halton(attempt, 2)
		v := halton(attempt, 3)
		x := b.MinX + u*b.Width()
		y := b.MinY + v*b.Height()
		attempt++
		p := model.Point{X: x, Y: y}
		if !geometry.StrictlyInside(p, zone.Polygon) || region.Terrain.AtXY(x, y) != terrain || excluded(region, p) {
			rejects++
			if rejects > 5_000_000 {
				break
			}
			continue
		}
		points = append(points, model.TruthPoint{
			ZoneID:  zone.ID,
			X:       round6(x),
			Y:       round6(y),
			Terrain: terrain,
			Rarity:  labels[len(points)],
			Density: density,
		})
	}
	return points
}

func buildObservations(region model.Region, cfg model.SamplingConfig, seed int64, profile int) []model.Observation {
	b := region.Boundary.Bounds()
	zones := orderedZones(region)
	var observations []model.Observation
	cols := int(math.Floor(b.Width() / cfg.SampleGridStep))
	rows := int(math.Floor(b.Height() / cfg.SampleGridStep))
	specs := truthSpecs(profile)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x := b.MinX + (float64(col)+0.5)*cfg.SampleGridStep
			y := b.MinY + (float64(row)+0.5)*cfg.SampleGridStep
			p := model.Point{X: x, Y: y}
			if excluded(region, p) {
				continue
			}
			zone, ok := zoneAt(zones, p)
			if !ok {
				continue
			}
			terrain := region.Terrain.AtXY(x, y)
			spec := specs[zone.ID]
			raw := spec.BaseDensity * terrainDensityMultiplier[terrain]
			jitter := (hashUnit(hashutil.Uint64(seed, "obs-jitter", zone.ID, row, col)) - 0.5) * cfg.QuantizationStep * 0.9
			quantized := model.RoundTo(raw+jitter, cfg.QuantizationStep)
			counts := rarityCounts(100, spec.Rarity, hashutil.Uint64(seed, "obs-rarity", zone.ID, row, col))
			observations = append(observations, model.Observation{
				ID:               fmt.Sprintf("obs-%03d-%03d", row, col),
				ZoneID:           zone.ID,
				X:                x,
				Y:                y,
				GridRow:          row,
				GridCol:          col,
				Terrain:          terrain,
				Density:          quantized,
				QuantizedDensity: quantized,
				RarityCounts:     counts,
			})
		}
	}
	sort.Slice(observations, func(i, j int) bool {
		a, b := observations[i], observations[j]
		if a.GridRow != b.GridRow {
			return a.GridRow < b.GridRow
		}
		return a.GridCol < b.GridCol
	})
	return observations
}

func zoneTerrainAreas(region model.Region, zone model.Zone) map[string]float64 {
	areas := map[string]float64{}
	r := region.Terrain
	cellArea := r.CellSize * r.CellSize
	for row := 0; row < r.Height; row++ {
		for col := 0; col < r.Width; col++ {
			x := r.OriginX + (float64(col)+0.5)*r.CellSize
			y := r.OriginY + (float64(row)+0.5)*r.CellSize
			p := model.Point{X: x, Y: y}
			if !geometry.StrictlyInside(p, zone.Polygon) || excluded(region, p) {
				continue
			}
			areas[r.At(row, col)] += cellArea
		}
	}
	return areas
}

func ZoneTerrainAreas(region model.Region, zone model.Zone) map[string]float64 {
	return zoneTerrainAreas(region, zone)
}

func excluded(region model.Region, p model.Point) bool {
	for _, ex := range region.Exclusions {
		if inside, _ := geometry.PointInPolygon(p, ex.Polygon); inside {
			return true
		}
	}
	return false
}

func zoneAt(zones []model.Zone, p model.Point) (model.Zone, bool) {
	for _, zone := range zones {
		if inside, _ := geometry.PointInPolygon(p, zone.Polygon); inside {
			return zone, true
		}
	}
	return model.Zone{}, false
}

func orderedZones(region model.Region) []model.Zone {
	zones := append([]model.Zone(nil), region.Zones...)
	sort.SliceStable(zones, func(i, j int) bool {
		if zones[i].Ordinal == zones[j].Ordinal {
			return zones[i].ID < zones[j].ID
		}
		return zones[i].Ordinal < zones[j].Ordinal
	})
	return zones
}

func zoneOrdinal(region model.Region, zoneID string) int {
	for _, z := range region.Zones {
		if z.ID == zoneID {
			return z.Ordinal
		}
	}
	return 1 << 30
}

func truthSpecs(profile int) map[string]zoneSpec {
	if profile != 0 {
		return variantTruthSpecs(profile)
	}
	return map[string]zoneSpec{
		"west-woods": {
			BaseDensity: 0.105,
			Rarity:      map[string]float64{"common": 0.62, "uncommon": 0.27, "rare": 0.11},
		},
		"central-meadow": {
			BaseDensity: 0.136,
			Rarity:      map[string]float64{"common": 0.70, "uncommon": 0.22, "rare": 0.08},
		},
		"east-ridge": {
			BaseDensity: 0.082,
			Rarity:      map[string]float64{"common": 0.55, "uncommon": 0.30, "rare": 0.15},
		},
		"south-marsh": {
			BaseDensity: 0.094,
			Rarity:      map[string]float64{"common": 0.66, "uncommon": 0.24, "rare": 0.10},
		},
	}
}

func polygon(points ...model.Point) model.Polygon {
	return model.Polygon{Vertices: points}
}

func halton(index uint64, base int) float64 {
	f := 1.0
	r := 0.0
	i := index
	for i > 0 {
		f /= float64(base)
		r += f * float64(i%uint64(base))
		i /= uint64(base)
	}
	return r
}

func hashUnit(h uint64) float64 {
	return float64(h>>11) / float64(1<<53)
}

func rarityCounts(total int, ratios map[string]float64, salt uint64) map[string]int {
	counts := map[string]int{}
	remaining := total
	type rem struct {
		kind  string
		value float64
	}
	var rems []rem
	for _, kind := range model.RarityKinds {
		raw := ratios[kind] * float64(total)
		n := int(math.Floor(raw))
		if n > remaining {
			n = remaining
		}
		counts[kind] = n
		remaining -= n
		rems = append(rems, rem{kind: kind, value: raw - math.Floor(raw)})
	}
	sort.SliceStable(rems, func(i, j int) bool {
		if math.Abs(rems[i].value-rems[j].value) < 1e-12 {
			return hashutil.Uint64(salt, rems[i].kind) < hashutil.Uint64(salt, rems[j].kind)
		}
		return rems[i].value > rems[j].value
	})
	for i := 0; remaining > 0; i = (i + 1) % len(rems) {
		counts[rems[i].kind]++
		remaining--
	}
	return counts
}

func weightedRarity(salt uint64, ratios map[string]float64) string {
	u := hashUnit(salt)
	var cumulative float64
	for _, kind := range model.RarityKinds {
		cumulative += ratios[kind]
		if u <= cumulative {
			return kind
		}
	}
	return model.RarityKinds[len(model.RarityKinds)-1]
}

func rarityLabels(total int, ratios map[string]float64) []string {
	counts := rarityCounts(total, ratios, 0)
	labels := make([]string, 0, total)
	for _, kind := range model.RarityKinds {
		for i := 0; i < counts[kind]; i++ {
			labels = append(labels, kind)
		}
	}
	if len(labels) > total {
		labels = labels[:total]
	}
	for len(labels) < total {
		labels = append(labels, model.RarityKinds[0])
	}
	return labels
}

func cloneRatios(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

func ZoneTruthBase(seed int64, zoneID string) float64 {
	return truthSpecs(profileForSeed(seed))[zoneID].BaseDensity
}
