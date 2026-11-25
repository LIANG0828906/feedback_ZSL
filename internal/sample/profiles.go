package sample

import "atelier/internal/model"

const (
	profileFrontier = 0
	profileEmber    = 1
	profileTidal    = 2
)

func profileForSeed(seed int64) int {
	switch seed {
	case 7183:
		return profileEmber
	case 9911:
		return profileTidal
	default:
		return profileFrontier
	}
}

func buildVariantRegion(profile int) model.Region {
	identity := map[int]struct {
		ID   string
		Name string
	}{
		profileEmber: {ID: "ember-rift", Name: "赤谷裂原"},
		profileTidal: {ID: "tidal-reach", Name: "潮汐边缘"},
	}[profile]
	boundary := polygon(
		model.Point{X: 0, Y: 0},
		model.Point{X: worldWidth, Y: 0},
		model.Point{X: worldWidth, Y: worldHeight},
		model.Point{X: 0, Y: worldHeight},
		model.Point{X: 0, Y: 0},
	)
	specs := variantZoneSpecs(profile)
	zones := make([]model.Zone, 0, len(specs))
	for _, spec := range specs {
		zones = append(zones, model.Zone{
			ID: spec.ID, Name: spec.Name, Ordinal: spec.Ordinal,
			Polygon: polygon(spec.Vertices...),
			Params:  model.ZoneParams{DensityScale: 1, RarityShift: 0},
		})
	}
	return model.Region{
		ID: identity.ID, Name: identity.Name,
		Boundary: boundary, Zones: zones,
		Exclusions: variantExclusions(profile),
		Terrain:    buildTerrain(boundary.Bounds(), profile),
	}
}

func variantZoneSpecs(profile int) []zoneSpec {
	switch profile {
	case profileEmber:
		return []zoneSpec{
			{
				ID: "north-scrub", Name: "北境灌丛", Ordinal: 0,
				BaseDensity: 0.112,
				Rarity:      map[string]float64{"common": 0.60, "uncommon": 0.28, "rare": 0.12},
				Vertices: []model.Point{
					{X: 0, Y: 0}, {X: 240, Y: 0}, {X: 240, Y: 54}, {X: 0, Y: 54}, {X: 0, Y: 0},
				},
			},
			{
				ID: "rift-floor", Name: "裂谷底", Ordinal: 1,
				BaseDensity: 0.078,
				Rarity:      map[string]float64{"common": 0.52, "uncommon": 0.31, "rare": 0.17},
				Vertices: []model.Point{
					{X: 0, Y: 54}, {X: 240, Y: 54}, {X: 240, Y: 108}, {X: 0, Y: 108}, {X: 0, Y: 54},
				},
			},
			{
				ID: "south-basin", Name: "南盐盆", Ordinal: 2,
				BaseDensity: 0.101,
				Rarity:      map[string]float64{"common": 0.68, "uncommon": 0.23, "rare": 0.09},
				Vertices: []model.Point{
					{X: 0, Y: 108}, {X: 240, Y: 108}, {X: 240, Y: 160}, {X: 0, Y: 160}, {X: 0, Y: 108},
				},
			},
		}
	case profileTidal:
		return []zoneSpec{
			{
				ID: "west-canopy", Name: "西岸林冠", Ordinal: 0,
				BaseDensity: 0.099,
				Rarity:      map[string]float64{"common": 0.64, "uncommon": 0.26, "rare": 0.10},
				Vertices: []model.Point{
					{X: 0, Y: 0}, {X: 120, Y: 0}, {X: 132, Y: 78}, {X: 0, Y: 84}, {X: 0, Y: 0},
				},
			},
			{
				ID: "east-crags", Name: "东岸岩台", Ordinal: 1,
				BaseDensity: 0.086,
				Rarity:      map[string]float64{"common": 0.57, "uncommon": 0.29, "rare": 0.14},
				Vertices: []model.Point{
					{X: 120, Y: 0}, {X: 240, Y: 0}, {X: 240, Y: 86}, {X: 132, Y: 78}, {X: 120, Y: 0},
				},
			},
			{
				ID: "tidal-flats", Name: "潮间浅滩", Ordinal: 2,
				BaseDensity: 0.118,
				Rarity:      map[string]float64{"common": 0.72, "uncommon": 0.20, "rare": 0.08},
				Vertices: []model.Point{
					{X: 0, Y: 84}, {X: 132, Y: 78}, {X: 142, Y: 160}, {X: 0, Y: 160}, {X: 0, Y: 84},
				},
			},
			{
				ID: "south-shoal", Name: "南部沙洲", Ordinal: 3,
				BaseDensity: 0.091,
				Rarity:      map[string]float64{"common": 0.63, "uncommon": 0.27, "rare": 0.10},
				Vertices: []model.Point{
					{X: 132, Y: 78}, {X: 240, Y: 86}, {X: 240, Y: 160}, {X: 142, Y: 160}, {X: 132, Y: 78},
				},
			},
		}
	default:
		return nil
	}
}

func variantExclusions(profile int) []model.Exclusion {
	switch profile {
	case profileEmber:
		return []model.Exclusion{
			{
				ID: "scorch-pit", Name: "灼坑", Reason: "高温禁采",
				Polygon: polygon(
					model.Point{X: 98, Y: 20}, model.Point{X: 146, Y: 18},
					model.Point{X: 153, Y: 45}, model.Point{X: 103, Y: 52},
					model.Point{X: 94, Y: 37}, model.Point{X: 98, Y: 20},
				),
			},
			{
				ID: "ash-vent", Name: "灰烬喷口", Reason: "不稳定地形",
				Polygon: polygon(
					model.Point{X: 98, Y: 72}, model.Point{X: 125, Y: 68},
					model.Point{X: 133, Y: 94}, model.Point{X: 103, Y: 100},
					model.Point{X: 90, Y: 87}, model.Point{X: 98, Y: 72},
				),
			},
			{
				ID: "salt-crust", Name: "盐壳坑", Reason: "坍塌风险",
				Polygon: polygon(
					model.Point{X: 180, Y: 126}, model.Point{X: 214, Y: 122},
					model.Point{X: 221, Y: 148}, model.Point{X: 184, Y: 153},
					model.Point{X: 180, Y: 126},
				),
			},
		}
	case profileTidal:
		return []model.Exclusion{
			{
				ID: "tidepool", Name: "王潮池", Reason: "潮汐保护区",
				Polygon: polygon(
					model.Point{X: 24, Y: 22}, model.Point{X: 58, Y: 18},
					model.Point{X: 64, Y: 50}, model.Point{X: 30, Y: 54},
					model.Point{X: 24, Y: 22},
				),
			},
			{
				ID: "wreck-field", Name: "沉船区", Reason: "水下遗存",
				Polygon: polygon(
					model.Point{X: 156, Y: 96}, model.Point{X: 194, Y: 92},
					model.Point{X: 201, Y: 126}, model.Point{X: 162, Y: 133},
					model.Point{X: 156, Y: 96},
				),
			},
			{
				ID: "blue-sink", Name: "蓝洞", Reason: "深水禁采",
				Polygon: polygon(
					model.Point{X: 73, Y: 118}, model.Point{X: 111, Y: 114},
					model.Point{X: 118, Y: 144}, model.Point{X: 78, Y: 149},
					model.Point{X: 73, Y: 118},
				),
			},
		}
	default:
		return nil
	}
}

func variantTerrainAt(x, y float64, profile int) string {
	switch profile {
	case profileEmber:
		if x < 68 && y < 86 {
			return "forest"
		}
		if y < 56 {
			return "ridge"
		}
		if y < 110 {
			return "meadow"
		}
		return "marsh"
	case profileTidal:
		if y < 82 {
			if x < 122 {
				return "forest"
			}
			return "ridge"
		}
		if x < 128 {
			return "meadow"
		}
		return "marsh"
	default:
		return "meadow"
	}
}

func variantTruthSpecs(profile int) map[string]zoneSpec {
	specs := variantZoneSpecs(profile)
	out := make(map[string]zoneSpec, len(specs))
	for _, spec := range specs {
		out[spec.ID] = spec
	}
	return out
}
