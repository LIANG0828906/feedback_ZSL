package pipeline

import (
	"sort"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"
)

// SelectSampleCells applies the deterministic stratified layout rule.
//
// Valid cells are grouped by terrain stratum, and every stratum keeps a
// deterministic prefix of its lexicographically ordered cells. The same region
// and configuration always yields exactly the same ordered cell set, including
// across operating systems and worker scheduling choices.
func SelectSampleCells(region *model.Region, zoneID int) ([]model.SampleCell, error) {
	groups := make(map[int][]model.SampleCell)
	for _, cell := range region.SampleGrid {
		if cell.ZoneID != zoneID || !cell.Valid {
			continue
		}
		groups[cell.Terrain] = append(groups[cell.Terrain], cell)
	}

	strata := make([]int, 0, len(groups))
	for terrain := range groups {
		strata = append(strata, terrain)
	}
	sort.Ints(strata)

	selected := make([]model.SampleCell, 0)
	for _, terrain := range strata {
		cells := groups[terrain]
		sort.Slice(cells, func(i, j int) bool {
			if cells[i].Coord.X != cells[j].Coord.X {
				return cells[i].Coord.X < cells[j].Coord.X
			}
			return cells[i].Coord.Y < cells[j].Coord.Y
		})
		selected = append(selected, cells...)
	}
	if len(selected) == 0 {
		return nil, errors.WithContext(errors.EObs,
			map[string]any{"zoneId": zoneID},
			"zone %d has no valid sample cells", zoneID)
	}
	return selected, nil
}

// FindObservation returns the observation for a grid index or an E_OBS naming
// the missing coordinate.
func FindObservation(region *model.Region, cell model.SampleCell) (model.Observation, error) {
	for _, obs := range region.Observations {
		if obs.GridIndex == cell.Index {
			return obs, nil
		}
	}
	return model.Observation{}, errors.WithContext(errors.EObs,
		map[string]any{"gridIndex": cell.Index, "x": cell.Coord.X, "y": cell.Coord.Y},
		"observation missing at grid point (%g,%g)", cell.Coord.X, cell.Coord.Y)
}
