package segment

import (
	"fmt"

	"iconflow/errs"
	"iconflow/model"
)

func ValidateBlock(stats *BlockStats, truth *model.BlockTruth, tolerances model.Tolerances) *errs.Error {
	if truth == nil {
		return nil
	}
	actualRegions := len(stats.Components)
	expectedRegions := len(truth.Components)
	if actualRegions != expectedRegions {
		e := errs.New(errs.ERegion, stats.Coord.Key(), "region count mismatch: actual=%d expected=%d", actualRegions, expectedRegions)
		e.Details = map[string]any{
			"coord": stats.Coord.Key(), "actual_regions": actualRegions,
			"expected_regions":  expectedRegions,
			"actual_foreground": stats.Foreground, "expected_foreground": truth.Foreground,
		}
		return e
	}

	issues := []*errs.Error{}
	matched := map[int]bool{}
	for expectedIndex, expected := range truth.Components {
		best := -1
		bestDistance := int64(1 << 62)
		for actualIndex, actual := range stats.Components {
			if matched[actualIndex] {
				continue
			}
			dx := actual.Centroid.XMilli - expected.Centroid.XMilli
			dy := actual.Centroid.YMilli - expected.Centroid.YMilli
			if dx < 0 {
				dx = -dx
			}
			if dy < 0 {
				dy = -dy
			}
			distance := dx + dy
			if distance < bestDistance {
				bestDistance = distance
				best = actualIndex
			}
		}
		if best < 0 {
			issue := errs.New(errs.ETop, stats.Coord.Key(), "missing component %d", expectedIndex)
			issue.Details = map[string]any{"coord": stats.Coord.Key(), "expected": expected}
			issues = append(issues, issue)
			continue
		}
		matched[best] = true
		actual := stats.Components[best]
		areaDiff := actual.Area - expected.Area
		if areaDiff < 0 {
			areaDiff = -areaDiff
		}
		dx := actual.Centroid.XMilli - expected.Centroid.XMilli
		dy := actual.Centroid.YMilli - expected.Centroid.YMilli
		if dx < 0 {
			dx = -dx
		}
		if dy < 0 {
			dy = -dy
		}
		centroidDiff := dx
		if dy > centroidDiff {
			centroidDiff = dy
		}
		if areaDiff > tolerances.AreaAbs || centroidDiff > tolerances.CentroidMilliAbs {
			issue := errs.New(errs.ETop, stats.Coord.Key(),
				"topology tolerance exceeded: area_diff=%d/%d centroid_diff_milli=%d/%d",
				areaDiff, tolerances.AreaAbs, centroidDiff, tolerances.CentroidMilliAbs)
			issue.Details = map[string]any{
				"coord":         stats.Coord.Key(),
				"expected_area": expected.Area, "actual_area": actual.Area,
				"area_diff": areaDiff, "area_tolerance": tolerances.AreaAbs,
				"expected_centroid_milli":  expected.Centroid,
				"actual_centroid_milli":    actual.Centroid,
				"centroid_diff_milli":      centroidDiff,
				"centroid_tolerance_milli": tolerances.CentroidMilliAbs,
			}
			issues = append(issues, issue)
		}
	}
	if len(issues) > 0 {
		first := issues[0]
		first.Issues = issues
		if first.Details == nil {
			first.Details = map[string]any{}
		}
		first.Details["issue_count"] = len(issues)
		return first
	}
	return nil
}

func FormatMask(mask []bool, width int) string {
	out := ""
	for i, value := range mask {
		if value {
			out += "█"
		} else {
			out += "·"
		}
		if (i+1)%width == 0 {
			out += "\n"
		}
	}
	return fmt.Sprint(out)
}
