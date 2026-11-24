package workbench

import (
	"fmt"
	"math"
)

// buildBiasReport uses only observed sample statistics, usable area, the
// declared quantization steps and observed densities. The hidden truth is not
// consulted.
func buildBiasReport(region *Region, zones []*ZoneResult) (BiasReport, *Error) {
	report := BiasReport{
		Threshold: region.Sampling.BiasThreshold,
		Zones:     make([]ZoneBias, 0, len(zones)),
		Passed:    true,
	}
	var cumulative BiasBound
	for order, zone := range zones {
		observed := zone.Metrics
		rarityCount := len(zone.Estimate.RarityMix)
		if rarityCount == 0 {
			rarityCount = 1
		}
		// The standard-error term is based on observed sample variance only.
		// The two quantization terms are the maximum errors of the declared
		// rounding grids. The heat upper bound combines these terms and the
		// density-cap slack actually observed in this run.
		capSlack := math.Abs(zone.Estimate.RawDensity - zone.Estimate.Density)
		upper := BiasBound{
			DensityQuantization: region.Sampling.DensityStep / 2,
			StandardError:       zone.Estimate.StandardError,
			RarityRounding:      region.Sampling.RarityStep * float64(rarityCount) / 2,
			Heatmap: region.Sampling.DensityStep/2 +
				1.96*zone.Estimate.StandardError +
				region.Sampling.RarityStep +
				capSlack,
		}
		cumulative.DensityQuantization += upper.DensityQuantization
		cumulative.StandardError += upper.StandardError
		cumulative.RarityRounding += upper.RarityRounding
		cumulative.Heatmap += upper.Heatmap
		report.Zones = append(report.Zones, ZoneBias{
			ZoneID:     zone.ZoneID,
			Order:      order,
			Observed:   observed,
			Upper:      upper,
			Cumulative: cumulative,
		})
	}
	report.TotalUpper = cumulative
	for _, zone := range report.Zones {
		values := []struct {
			name  string
			value float64
		}{
			{name: "density_quantization", value: zone.Cumulative.DensityQuantization},
			{name: "standard_error", value: zone.Cumulative.StandardError},
			{name: "rarity_rounding", value: zone.Cumulative.RarityRounding},
			{name: "heatmap", value: zone.Cumulative.Heatmap},
		}
		for _, item := range values {
			if item.value > report.Threshold {
				report.Passed = false
				report.FirstViolationZone = zone.ZoneID
				report.FirstViolationValue = item.value
				value := item.value
				return report, &Error{
					Code:     CodeBias,
					ZoneID:   zone.ZoneID,
					DataItem: item.name,
					Value:    &value,
					Message: fmt.Sprintf("cumulative bias upper bound %.6f exceeds threshold %.6f",
						item.value, report.Threshold),
					Details: map[string]any{
						"metric":         item.name,
						"threshold":      report.Threshold,
						"upper_bound":    item.value,
						"topology_order": zone.Order,
					},
				}
			}
		}
	}
	return report, nil
}
