package iconmask

import (
	"fmt"
	"math"
	"sort"
)

type RadiusConfig struct {
	Min            int `json:"min"`
	Max            int `json:"max"`
	CurvatureBands int `json:"curvatureBands"`
}

type SegmentRadius struct {
	SegmentID       string  `json:"segmentId"`
	ComponentID     string  `json:"componentId"`
	Loop            string  `json:"loop"`
	Curvature       float64 `json:"curvature"`
	CurvatureRadius float64 `json:"curvatureRadius,omitempty"`
	Normalized      float64 `json:"normalized"`
	Bucket          int     `json:"bucket"`
	ErosionRadius   int     `json:"erosionRadius"`
	DilationRadius  int     `json:"dilationRadius"`
}

type RadiusTable struct {
	Config           RadiusConfig    `json:"config"`
	CurvatureMin     float64         `json:"curvatureMin"`
	CurvatureMax     float64         `json:"curvatureMax"`
	Segments         []SegmentRadius `json:"segments"`
	AlgorithmVersion string          `json:"algorithmVersion"`
}

func ValidateRadiusConfig(imageID string, cfg RadiusConfig) *Error {
	if cfg.Min <= 0 {
		return radiusError(imageID, "radius.min", "radius band lower bound must be a positive integer", nil)
	}
	if cfg.Max <= 0 {
		return radiusError(imageID, "radius.max", "radius band upper bound must be a positive integer", nil)
	}
	if cfg.Min > cfg.Max {
		return radiusError(imageID, "radius.min", "radius band lower bound must not exceed upper bound", map[string]any{
			"min": cfg.Min,
			"max": cfg.Max,
		})
	}
	if cfg.CurvatureBands <= 0 {
		return radiusError(imageID, "radius.curvatureBands", "curvature bands must be a positive integer", nil)
	}
	return nil
}

func radiusError(imageID, field, message string, details map[string]any) *Error {
	e := domainError(CodeRadius, imageID, field, message)
	e.Details = details
	return e
}

func ResolveRadii(imageID string, segments []BoundarySegment, cfg RadiusConfig) (*RadiusTable, *Error) {
	if err := ValidateRadiusConfig(imageID, cfg); err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, radiusError(imageID, "truth.segments", "cannot resolve radii without boundary segments", nil)
	}
	curvatureMin := math.Inf(1)
	curvatureMax := math.Inf(-1)
	for _, segment := range segments {
		if !finite(segment.Curvature) || segment.Curvature < 0 {
			return nil, radiusError(imageID, "truth.segments."+segment.ID+".curvature",
				"segment curvature must be finite and non-negative", map[string]any{
					"segmentId": segment.ID,
					"curvature": segment.Curvature,
				})
		}
		curvatureMin = math.Min(curvatureMin, segment.Curvature)
		curvatureMax = math.Max(curvatureMax, segment.Curvature)
	}
	table := &RadiusTable{
		Config:           cfg,
		CurvatureMin:     curvatureMin,
		CurvatureMax:     curvatureMax,
		AlgorithmVersion: AlgorithmVersion,
		Segments:         make([]SegmentRadius, 0, len(segments)),
	}
	for _, segment := range segments {
		normalized := normalizeCurvature(segment.Curvature, curvatureMin, curvatureMax)
		radius, bucket := MapRadius(segment.Curvature, curvatureMin, curvatureMax, cfg)
		entry := SegmentRadius{
			SegmentID:      segment.ID,
			ComponentID:    segment.ComponentID,
			Loop:           segment.Loop,
			Curvature:      segment.Curvature,
			Normalized:     normalized,
			Bucket:         bucket,
			ErosionRadius:  radius,
			DilationRadius: radius,
		}
		if segment.Curvature > 0 {
			entry.CurvatureRadius = 1 / segment.Curvature
		}
		table.Segments = append(table.Segments, entry)
	}
	sort.SliceStable(table.Segments, func(i, j int) bool {
		return table.Segments[i].SegmentID < table.Segments[j].SegmentID
	})
	if err := ValidateRadiusTable(imageID, table, cfg); err != nil {
		return nil, err
	}
	return table, nil
}

// MapRadius is the pure mapping used both at runtime and by recomputation
// tests. Curvature is bucketed from low to high; higher curvature maps to a
// smaller (or equal) integer radius.
func MapRadius(curvature, curvatureMin, curvatureMax float64, cfg RadiusConfig) (int, int) {
	if cfg.CurvatureBands <= 1 || curvatureMax-curvatureMin <= 1e-12 {
		return clampInt(cfg.Max, cfg.Min, cfg.Max), 0
	}
	normalized := (curvature - curvatureMin) / (curvatureMax - curvatureMin)
	normalized = math.Max(0, math.Min(1, normalized))
	bucket := int(math.Floor(normalized * float64(cfg.CurvatureBands)))
	bucket = clampInt(bucket, 0, cfg.CurvatureBands-1)
	step := float64(cfg.Max-cfg.Min) / float64(cfg.CurvatureBands-1)
	radius := cfg.Max - int(math.Round(float64(bucket)*step))
	return clampInt(radius, cfg.Min, cfg.Max), bucket
}

func normalizeCurvature(curvature, min, max float64) float64 {
	if max-min <= 1e-12 {
		return 0
	}
	return math.Max(0, math.Min(1, (curvature-min)/(max-min)))
}

func ValidateRadiusTable(imageID string, table *RadiusTable, cfg RadiusConfig) *Error {
	if table == nil {
		return radiusError(imageID, "radiusTable", "radius table is nil", nil)
	}
	if err := ValidateRadiusConfig(imageID, cfg); err != nil {
		return err
	}
	if len(table.Segments) == 0 {
		return radiusError(imageID, "radiusTable.segments", "radius table must contain at least one segment", nil)
	}
	if table.Config != cfg {
		e := radiusError(imageID, "radiusTable.config", "radius table config differs from declared config", map[string]any{
			"tableMin":            table.Config.Min,
			"tableMax":            table.Config.Max,
			"tableCurvatureBands": table.Config.CurvatureBands,
			"configMin":           cfg.Min,
			"configMax":           cfg.Max,
			"configBands":         cfg.CurvatureBands,
		})
		return e
	}
	actualMin := math.Inf(1)
	actualMax := math.Inf(-1)
	for _, entry := range table.Segments {
		actualMin = math.Min(actualMin, entry.Curvature)
		actualMax = math.Max(actualMax, entry.Curvature)
	}
	if !approxEqual(actualMin, table.CurvatureMin) || !approxEqual(actualMax, table.CurvatureMax) {
		e := radiusError(imageID, "radiusTable.curvatureRange",
			"radius table curvature range cannot be reproduced from segment entries", map[string]any{
				"declaredMin": table.CurvatureMin,
				"declaredMax": table.CurvatureMax,
				"actualMin":   actualMin,
				"actualMax":   actualMax,
			})
		return e
	}
	for _, entry := range table.Segments {
		if entry.ErosionRadius <= 0 || entry.ErosionRadius < cfg.Min || entry.ErosionRadius > cfg.Max {
			e := radiusError(imageID, "radiusTable.segments."+entry.SegmentID+".erosionRadius",
				"erosion radius is outside the declared band", map[string]any{
					"segmentId": entry.SegmentID,
					"value":     entry.ErosionRadius,
					"min":       cfg.Min,
					"max":       cfg.Max,
				})
			e.SegmentID = entry.SegmentID
			return e
		}
		if entry.DilationRadius <= 0 || entry.DilationRadius < cfg.Min || entry.DilationRadius > cfg.Max {
			e := radiusError(imageID, "radiusTable.segments."+entry.SegmentID+".dilationRadius",
				"dilation radius is outside the declared band", map[string]any{
					"segmentId": entry.SegmentID,
					"value":     entry.DilationRadius,
					"min":       cfg.Min,
					"max":       cfg.Max,
				})
			e.SegmentID = entry.SegmentID
			return e
		}
	}
	if err := validateRadiusMonotonicity(imageID, table.Segments); err != nil {
		return err
	}
	for _, entry := range table.Segments {
		expected, bucket := MapRadius(entry.Curvature, table.CurvatureMin, table.CurvatureMax, cfg)
		if entry.ErosionRadius != expected || entry.DilationRadius != expected || entry.Bucket != bucket {
			e := radiusError(imageID, "radiusTable.segments."+entry.SegmentID,
				"radius cannot be recomputed from true curvature", map[string]any{
					"segmentId":      entry.SegmentID,
					"curvature":      entry.Curvature,
					"expectedRadius": expected,
					"actualErosion":  entry.ErosionRadius,
					"actualDilation": entry.DilationRadius,
					"expectedBucket": bucket,
					"actualBucket":   entry.Bucket,
				})
			e.SegmentID = entry.SegmentID
			return e
		}
	}
	return nil
}

func validateRadiusMonotonicity(imageID string, entries []SegmentRadius) *Error {
	sorted := append([]SegmentRadius(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Curvature == sorted[j].Curvature {
			return sorted[i].SegmentID < sorted[j].SegmentID
		}
		return sorted[i].Curvature < sorted[j].Curvature
	})
	for i := 1; i < len(sorted); i++ {
		left := sorted[i-1]
		right := sorted[i]
		if right.Curvature > left.Curvature &&
			(right.ErosionRadius > left.ErosionRadius || right.DilationRadius > left.DilationRadius) {
			details := map[string]any{
				"leftSegmentId":  left.SegmentID,
				"leftCurvature":  left.Curvature,
				"leftRadius":     left.ErosionRadius,
				"rightSegmentId": right.SegmentID,
				"rightCurvature": right.Curvature,
				"rightRadius":    right.ErosionRadius,
				"rule":           "higher curvature must map to a non-increasing radius",
			}
			e := radiusError(imageID, "radiusTable.monotonicity",
				fmt.Sprintf("radius monotonicity violated between %s and %s", left.SegmentID, right.SegmentID), details)
			e.SegmentID = right.SegmentID
			return e
		}
	}
	return nil
}

func (table *RadiusTable) Lookup(segmentID string) (SegmentRadius, bool) {
	if table == nil {
		return SegmentRadius{}, false
	}
	for _, entry := range table.Segments {
		if entry.SegmentID == segmentID {
			return entry, true
		}
	}
	return SegmentRadius{}, false
}
