package pipeline

import (
	"math"

	"collectionrecon/internal/model"
)

// EstimateDensity inverts the quantized observations of one zone. The zone
// density is the mean of its per-cell readings; rarity proportions come from
// the rarity-split densities. Rarity counts are allocated purely with integer
// arithmetic (largest remainder method) and the residual points are reported.
func EstimateDensity(region *model.Region, zoneID int, cells []model.SampleCell) (model.DensityEstimate, error) {
	est := model.DensityEstimate{
		ZoneID:       zoneID,
		RarityMix:    make([]float64, model.NumRarity),
		RarityCounts: make([]int, model.NumRarity),
		RarityResid:  make([]int, model.NumRarity),
		CellIndices:  make([]int, len(cells)),
	}

	sum := 0.0
	raritySums := make([]float64, model.NumRarity)
	values := make([]float64, 0, len(cells))
	for i, cell := range cells {
		obs, err := FindObservation(region, cell)
		if err != nil {
			return est, err
		}
		est.CellIndices[i] = cell.Index
		sum += obs.Density
		values = append(values, obs.Density)
		for r := range raritySums {
			raritySums[r] += obs.RarityDensity[r]
		}
	}

	n := float64(len(cells))
	est.CellCount = len(cells)
	est.Density = sum / n

	variance := 0.0
	for _, v := range values {
		d := v - est.Density
		variance += d * d
	}
	if len(values) > 1 {
		variance /= float64(len(values) - 1)
	}
	est.StdError = math.Sqrt(variance / n)

	rarityTotal := 0.0
	for _, s := range raritySums {
		rarityTotal += s
	}
	if rarityTotal > 0 {
		for r := range est.RarityMix {
			est.RarityMix[r] = raritySums[r] / rarityTotal
		}
	} else {
		est.RarityMix[0] = 1
	}

	zone := findZone(region, zoneID)
	targetPoints := int(math.Round(est.Density * effectiveArea(region, zone)))
	allocateRarity(targetPoints, est.RarityMix, est.RarityCounts, est.RarityResid)
	return est, nil
}

// allocateRarity performs integer largest-remainder apportionment. Integer
// counts and the remainder are handled with integer arithmetic only; the
// residual is the integer-allocated count minus the exact real-valued share,
// stored in milli-units so it stays an int per the reporting contract.
func allocateRarity(total int, mix []float64, counts, residualMilli []int) {
	if total <= 0 {
		return
	}
	type slot struct {
		idx int
		rem float64
	}
	assigned := 0
	slots := make([]slot, len(mix))
	for i, w := range mix {
		exact := w * float64(total)
		base := int(exact)
		counts[i] = base
		assigned += base
		slots[i] = slot{idx: i, rem: exact - float64(base)}
	}
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0 && (slots[j].rem > slots[j-1].rem ||
			(slots[j].rem == slots[j-1].rem && slots[j].idx < slots[j-1].idx)); j-- {
			slots[j], slots[j-1] = slots[j-1], slots[j]
		}
	}
	for k := 0; k < total-assigned; k++ {
		counts[slots[k].idx]++
	}
	for i, w := range mix {
		exact := w * float64(total)
		residualMilli[i] = int(math.Round((float64(counts[i]) - exact) * 1000))
	}
}

func sumInts(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

func effectiveArea(region *model.Region, zone model.Zone) float64 {
	area := (zone.Bounds.MaxX - zone.Bounds.MinX) * (zone.Bounds.MaxY - zone.Bounds.MinY)
	for _, nz := range region.NoCollect {
		if nz.ZoneID == zone.ID {
			area -= (nz.Bounds.MaxX - nz.Bounds.MinX) * (nz.Bounds.MaxY - nz.Bounds.MinY)
		}
	}
	return area
}
