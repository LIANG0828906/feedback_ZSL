// Package generator builds the deterministic local sample dataset. Every value
// derives analytically from a fixed seed; there is no external data source.
package generator

import (
	"math"

	"collectionrecon/internal/geom"
	"collectionrecon/internal/model"
	"collectionrecon/internal/rand"
)

const (
	regionW     = 200.0
	regionH     = 160.0
	numZones    = 4
	zoneW       = regionW / numZones
	rasterCell  = 4.0
	gridSpacing = 10.0
	quantStep   = 0.001
	observeR    = 12.0
)

// Config controls the fixed shape of the generated dataset.
type Config struct {
	Seed       uint64
	RasterCell float64
	GridStep   float64
	QuantStep  float64
	ObserveR   float64
}

// DefaultConfig returns the fixed-seed configuration used by the example.
func DefaultConfig() Config {
	return Config{
		Seed:       0xA11CE5,
		RasterCell: rasterCell,
		GridStep:   gridSpacing,
		QuantStep:  quantStep,
		ObserveR:   observeR,
	}
}

var zoneDensities = [numZones]float64{0.030, 0.045, 0.020, 0.035}

var zoneTerrainWeights = [numZones][model.NumRarity]float64{
	{0.60, 0.30, 0.10},
	{0.40, 0.40, 0.20},
	{0.20, 0.50, 0.30},
	{0.30, 0.20, 0.50},
}

var zoneRarityMix = [numZones][model.NumRarity]float64{
	{0.70, 0.20, 0.10},
	{0.60, 0.30, 0.10},
	{0.55, 0.30, 0.15},
	{0.50, 0.35, 0.15},
}

// Generate produces the full region dataset from cfg.
func Generate(cfg Config) *model.Region {
	nx := int(regionW / cfg.RasterCell)
	ny := int(regionH / cfg.RasterCell)
	region := &model.Region{
		ID:          1,
		Name:        "seed-region-1",
		Seed:        cfg.Seed,
		RasterCell:  cfg.RasterCell,
		RasterNX:    nx,
		RasterNY:    ny,
		QuantStep:   cfg.QuantStep,
		GridSpacing: cfg.GridStep,
		BBox:        model.Rect{MinX: 0, MinY: 0, MaxX: regionW, MaxY: regionH},
		Boundary: model.Polygon{Vertices: []model.Point{
			{0, 0}, {regionW, 0}, {regionW, regionH}, {0, regionH}, {0, 0},
		}},
	}

	region.Zones = make([]model.Zone, numZones)
	for z := 0; z < numZones; z++ {
		region.Zones[z] = model.Zone{
			ID:             z + 1,
			Bounds:         model.Rect{MinX: float64(z) * zoneW, MinY: 0, MaxX: float64(z+1) * zoneW, MaxY: regionH},
			TerrainWeights: append([]float64(nil), zoneTerrainWeights[z][:]...),
			TrueDensity:    zoneDensities[z],
			RarityMix:      append([]float64(nil), zoneRarityMix[z][:]...),
			ParamVersion:   1,
		}
	}

	region.NoCollect = []model.NoCollectZone{
		{ID: 1, ZoneID: 1, Bounds: model.Rect{MinX: 8, MinY: 8, MaxX: 38, MaxY: 42}},
		{ID: 2, ZoneID: 3, Bounds: model.Rect{MinX: 112, MinY: 98, MaxX: 142, MaxY: 138}},
	}

	region.TerrainRaster = buildRaster(region, cfg.Seed)
	fillActualTerrainWeights(region)
	region.Truth = buildTruth(region, cfg.Seed)
	region.SampleGrid = buildSampleGrid(region, cfg.GridStep)
	region.Observations = buildObservations(region, cfg)
	return region
}

func buildRaster(region *model.Region, seed uint64) [][]int {
	raster := make([][]int, region.RasterNX)
	for ix := 0; ix < region.RasterNX; ix++ {
		raster[ix] = make([]int, region.RasterNY)
		rng := rand.Derive(seed, uint64(ix)+1)
		for iy := 0; iy < region.RasterNY; iy++ {
			zoneID := zoneAtX(region, float64(ix)*region.RasterCell+region.RasterCell/2)
			weights := region.Zones[zoneID].TerrainWeights
			draw := rng.Float64()
			cum := 0.0
			terrain := model.NumRarity - 1
			for t, w := range weights {
				cum += w
				if draw < cum {
					terrain = t
					break
				}
			}
			raster[ix][iy] = terrain
		}
	}
	return raster
}

func fillActualTerrainWeights(region *model.Region) {
	counts := make([][model.NumRarity]int, numZones)
	total := make([]int, numZones)
	for ix := 0; ix < region.RasterNX; ix++ {
		for iy := 0; iy < region.RasterNY; iy++ {
			zoneID := zoneAtX(region, float64(ix)*region.RasterCell+region.RasterCell/2)
			counts[zoneID][region.TerrainRaster[ix][iy]]++
			total[zoneID]++
		}
	}
	for z := range region.Zones {
		weights := make([]float64, model.NumRarity)
		for t := range weights {
			weights[t] = float64(counts[z][t]) / float64(total[z])
		}
		region.Zones[z].TerrainWeights = weights
	}
}

func buildTruth(region *model.Region, seed uint64) []model.TruthPoint {
	truth := make([]model.TruthPoint, 0, 1024)
	for z := range region.Zones {
		zone := region.Zones[z]
		target := int(math.Round(zone.TrueDensity * geom.RectArea(zone.Bounds)))
		rng := rand.Derive(seed, uint64(z)*7919+17)
		placed := 0
		for placed < target {
			px := zone.Bounds.MinX + rng.Float64()*(zone.Bounds.MaxX-zone.Bounds.MinX)
			py := zone.Bounds.MinY + rng.Float64()*(zone.Bounds.MaxY-zone.Bounds.MinY)
			coord := model.Point{X: px, Y: py}
			if inAnyNoCollect(coord, region.NoCollect) {
				continue
			}
			terrain := terrainAt(region, coord)
			rarity := pickWeighted(rng, zone.RarityMix)
			truth = append(truth, model.TruthPoint{
				Coord: coord, ZoneID: zone.ID, Terrain: terrain, Rarity: rarity,
			})
			placed++
		}
	}
	return truth
}

func buildSampleGrid(region *model.Region, step float64) []model.SampleCell {
	cells := make([]model.SampleCell, 0)
	idx := 0
	for x := step / 2; x < regionW; x += step {
		for y := step / 2; y < regionH; y += step {
			coord := model.Point{X: x, Y: y}
			zoneID := zoneAtX(region, x)
			noCollect := inAnyNoCollect(coord, region.NoCollect)
			valid := geom.PointInPolygon(coord, region.Boundary) && zoneID >= 0 && !noCollect
			cells = append(cells, model.SampleCell{
				Index:       idx,
				Coord:       coord,
				ZoneID:      zoneID + 1,
				Terrain:     terrainAt(region, coord),
				InNoCollect: noCollect,
				Valid:       valid,
			})
			idx++
		}
	}
	return cells
}

func buildObservations(region *model.Region, cfg Config) []model.Observation {
	obs := make([]model.Observation, 0, len(region.SampleGrid))
	area := math.Pi * cfg.ObserveR * cfg.ObserveR
	for _, cell := range region.SampleGrid {
		if !cell.Valid {
			continue
		}
		var totals [model.NumRarity]float64
		for _, tp := range region.Truth {
			dx, dy := tp.Coord.X-cell.Coord.X, tp.Coord.Y-cell.Coord.Y
			if dx*dx+dy*dy <= cfg.ObserveR*cfg.ObserveR {
				totals[tp.Rarity]++
			}
		}
		density := 0.0
		for _, c := range totals {
			density += c
		}
		density /= area
		rarityDensity := make([]float64, model.NumRarity)
		for r := range rarityDensity {
			rarityDensity[r] = rand.Quantize(totals[r]/area, cfg.QuantStep)
		}
		obs = append(obs, model.Observation{
			GridIndex:     cell.Index,
			Coord:         cell.Coord,
			ZoneID:        cell.ZoneID,
			Terrain:       cell.Terrain,
			Density:       rand.Quantize(density, cfg.QuantStep),
			RarityDensity: rarityDensity,
			QuantStep:     cfg.QuantStep,
		})
	}
	return obs
}

func zoneAtX(region *model.Region, x float64) int {
	for z := range region.Zones {
		if geom.PointInRect(model.Point{X: x, Y: 0}, region.Zones[z].Bounds) {
			return z
		}
	}
	return -1
}

func inAnyNoCollect(p model.Point, zones []model.NoCollectZone) bool {
	for _, nz := range zones {
		if geom.PointInRect(p, nz.Bounds) {
			return true
		}
	}
	return false
}

func terrainAt(region *model.Region, p model.Point) int {
	ix := int((p.X - region.BBox.MinX) / region.RasterCell)
	iy := int((p.Y - region.BBox.MinY) / region.RasterCell)
	if ix < 0 || ix >= region.RasterNX || iy < 0 || iy >= region.RasterNY {
		return -1
	}
	return region.TerrainRaster[ix][iy]
}

func pickWeighted(rng *rand.RNG, weights []float64) int {
	draw := rng.Float64()
	cum := 0.0
	for i, w := range weights {
		cum += w
		if draw < cum {
			return i
		}
	}
	return len(weights) - 1
}
