// Package validate checks pipeline outputs against the hidden truth and the
// declared tolerance contract. It is the only package allowed to read truth.
package validate

import (
	"fmt"
	"math"

	"collectionrecon/internal/model"
	"collectionrecon/internal/pipeline"
)

// Tolerances bundles the declared acceptance margins.
type Tolerances struct {
	DensityRelative    float64
	PointCountRelative float64
	HeatAbsolute       float64
}

// DefaultTolerances returns the margins used by the example.
func DefaultTolerances() Tolerances {
	// Margins are declared from the deterministic sample's observable noise:
	// density 20% relative, point count 15% relative (small-sample integer
	// counts), heat 0.04 absolute which equals ~2 disc counts at r=12.
	return Tolerances{DensityRelative: 0.20, PointCountRelative: 0.15, HeatAbsolute: 0.04}
}

// ZoneCheck summarizes one zone's truth-vs-output comparison.
type ZoneCheck struct {
	ZoneID             int     `json:"zoneId"`
	TrueDensity        float64 `json:"trueDensity"`
	EstimatedDensity   float64 `json:"estimatedDensity"`
	DensityRelError    float64 `json:"densityRelError"`
	DensityWithin      bool    `json:"densityWithin"`
	TruePointCount     int     `json:"truePointCount"`
	ReconPointCount    int     `json:"reconPointCount"`
	PointCountRelError float64 `json:"pointCountRelError"`
	PointCountWithin   bool    `json:"pointCountWithin"`
	MaxHeatError       float64 `json:"maxHeatError"`
	HeatWithin         bool    `json:"heatWithin"`
}

// CheckRegion compares every zone's output with truth and observations.
func CheckRegion(region *model.Region, results []model.ZoneResult, tol Tolerances) []ZoneCheck {
	truthCount := make(map[int]int)
	trueDensity := make(map[int]float64)
	for _, tp := range region.Truth {
		truthCount[tp.ZoneID]++
	}
	for _, z := range region.Zones {
		trueDensity[z.ID] = z.TrueDensity
	}

	pointsByZone := make(map[int][]model.ReconstructedPoint)
	for _, r := range results {
		pointsByZone[r.ZoneID] = r.Points
	}

	checks := make([]ZoneCheck, 0, len(results))
	for _, r := range results {
		id := r.ZoneID
		est := r.Estimate.Density
		rel := math.Abs(est-trueDensity[id]) / trueDensity[id]
		reconCount := len(pointsByZone[id])
		countRel := math.Abs(float64(reconCount-truthCount[id])) / float64(truthCount[id])
		maxHeat := maxHeatError(region, pointsByZone[id], id)
		checks = append(checks, ZoneCheck{
			ZoneID: id, TrueDensity: trueDensity[id], EstimatedDensity: est,
			DensityRelError: rel, DensityWithin: rel <= tol.DensityRelative,
			TruePointCount: truthCount[id], ReconPointCount: reconCount,
			PointCountRelError: countRel, PointCountWithin: countRel <= tol.PointCountRelative,
			MaxHeatError: maxHeat, HeatWithin: maxHeat <= tol.HeatAbsolute,
		})
	}
	return checks
}

func maxHeatError(region *model.Region, points []model.ReconstructedPoint, zoneID int) float64 {
	worst := 0.0
	for _, obs := range region.Observations {
		if obs.ZoneID != zoneID {
			continue
		}
		heat := pipeline.HeatAt(points, obs.Coord)
		if delta := math.Abs(heat - obs.Density); delta > worst {
			worst = delta
		}
	}
	return worst
}

// CountDensityOutliers applies a relative tolerance and returns the number of
// zones whose estimate exceeds it. Tightening the tolerance must never reduce
// this count; the test suite verifies monotonicity directly.
func CountDensityOutliers(checks []ZoneCheck, tolerance float64) int {
	out := 0
	for _, c := range checks {
		if c.DensityRelError > tolerance {
			out++
		}
	}
	return out
}

// AllPassed reports whether every declared tolerance held for every zone.
func AllPassed(checks []ZoneCheck, tol Tolerances) bool {
	for _, c := range checks {
		if !c.DensityWithin || !c.PointCountWithin || !c.HeatWithin {
			return false
		}
	}
	return true
}

// Summary renders a compact human-readable verification line.
func Summary(checks []ZoneCheck) string {
	out := ""
	for _, c := range checks {
		out += fmt.Sprintf(
			"zone %d: density relErr=%.3f pointCount %d/%d relErr=%.3f maxHeatErr=%.4f\n",
			c.ZoneID, c.DensityRelError, c.ReconPointCount, c.TruePointCount,
			c.PointCountRelError, c.MaxHeatError)
	}
	return out
}
