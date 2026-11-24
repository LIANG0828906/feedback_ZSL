package pipeline

import (
	"math"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

// BuildBoundsReport derives per-zone and cumulative bias upper bounds using
// only observed quantities: number of observed cells, zone area, observed
// density values and the quantization step. Truth values are never consulted.
//
// The four cumulative bounds are:
//  1. quantization error bound (density units),
//  2. within-stratum standard error bound (density units),
//  3. rarity apportionment residual (integer milli-units),
//  4. heatmap reconstruction error bound (density units),
//
// and each accumulates monotonically along zone order.
func BuildBoundsReport(region *model.Region, results []model.ZoneResult) ([]model.BoundRecord, error) {
	records := make([]model.BoundRecord, 0, len(results))
	var cumQuant, cumStd, cumHeat float64
	var cumRarity int

	for _, res := range results {
		cells := make([]model.SampleCell, 0)
		for _, cell := range region.SampleGrid {
			if cell.ZoneID == res.ZoneID && cell.Valid {
				cells = append(cells, cell)
			}
		}
		if len(cells) == 0 {
			return nil, errors.WithContext(errors.EBound,
				map[string]any{"zoneId": res.ZoneID, "missing": "observed sample cells"},
				"cannot derive bounds for zone %d: no observed cells", res.ZoneID)
		}

		values := make([]float64, 0, len(cells))
		step := region.QuantStep
		for _, cell := range cells {
			obs, err := FindObservation(region, cell)
			if err != nil {
				return nil, errors.WithContext(errors.EBound,
					map[string]any{"zoneId": res.ZoneID, "gridIndex": cell.Index,
						"x": cell.Coord.X, "y": cell.Coord.Y},
					"missing observation required for bound derivation at (%g,%g)",
					cell.Coord.X, cell.Coord.Y)
			}
			values = append(values, obs.Density)
			if obs.QuantStep > 0 {
				step = obs.QuantStep
			}
		}

		n := len(values)
		mean := res.Estimate.Density
		quantBound := step / 2.0

		variance := 0.0
		maxObs := 0.0
		for _, v := range values {
			d := v - mean
			variance += d * d
			if v > maxObs {
				maxObs = v
			}
		}
		if n > 1 {
			variance /= float64(n - 1)
		}
		stdBound := math.Sqrt(variance/float64(n)) + quantBound/math.Sqrt(float64(n))

		rarityResidual := 0
		for _, r := range res.Estimate.RarityResid {
			rarityResidual += absInt(r)
		}

		zone := findZone(region, res.ZoneID)
		area := effectiveArea(region, zone)
		// KDE truncation and quantization can move a heat value by at most the
		// largest observed density scaled by the observed-cell coverage ratio.
		coverage := float64(n) * region.GridSpacing * region.GridSpacing / area
		if coverage > 1 {
			coverage = 1
		}
		heatBound := maxObs*(1-coverage) + quantBound

		cumQuant += quantBound
		cumStd += stdBound
		cumRarity += rarityResidual
		cumHeat += heatBound

		records = append(records, model.BoundRecord{
			ZoneID:          res.ZoneID,
			QuantErrorBound: quantBound,
			StdErrorBound:   stdBound,
			RarityResidual:  rarityResidual,
			HeatErrorBound:  heatBound,
			CumQuantBound:   cumQuant,
			CumStdBound:     cumStd,
			CumRarityBound:  cumRarity,
			CumHeatBound:    cumHeat,
		})
	}
	return records, nil
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
