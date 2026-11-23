package tactical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// EffectiveMap applies active blocked regions to a copy of the base map. The
// returned map is what search, smoothing, and the UI should use.
func EffectiveMap(def MapDef) MapDef {
	out := def
	out.Cells = append([]Cell(nil), def.Cells...)
	for i := range out.Cells {
		out.Cells[i] = def.Cells[i]
	}
	for _, region := range def.BlockedRegions {
		if !region.Active {
			continue
		}
		for y := region.Y; y < region.Y+region.Height; y++ {
			for x := region.X; x < region.X+region.Width; x++ {
				idx := y*def.Width + x
				if idx < 0 || idx >= len(out.Cells) {
					continue
				}
				out.Cells[idx].Passable = false
				out.Cells[idx].BlockedBy = region.ID
			}
		}
	}
	return out
}

func CellAt(def MapDef, p GridPoint) (Cell, bool) {
	if !p.InBounds(def.Width, def.Height) {
		return Cell{}, false
	}
	return def.Cells[p.Y*def.Width+p.X], true
}

func SetCellCost(def *MapDef, p GridPoint, cost int64) error {
	if !p.InBounds(def.Width, def.Height) {
		return schemaError("map.cells", fmt.Sprintf("cell (%d,%d) is outside the map", p.X, p.Y))
	}
	if cost <= 0 || cost > MaxCellCost {
		return schemaError("map.cells.cost", fmt.Sprintf("cost %d is outside 1..%d", cost, MaxCellCost))
	}
	idx := p.Y*def.Width + p.X
	if !def.Cells[idx].Passable {
		return schemaError("map.cells.passable", "cannot assign a traversal cost to an impassable cell")
	}
	def.Cells[idx].Cost = cost
	return nil
}

func SetRegionActive(def *MapDef, regionID string, active bool) error {
	for i := range def.BlockedRegions {
		if def.BlockedRegions[i].ID == regionID {
			def.BlockedRegions[i].Active = active
			return nil
		}
	}
	return schemaError("map.blocked_regions.id", "unknown blocked region "+regionID)
}

// MapContentHash is stable for the complete base map plus region flags. It
// intentionally does not include formation slots or algorithm parameters.
func MapContentHash(def MapDef) string {
	canonical := struct {
		Width          int             `json:"width"`
		Height         int             `json:"height"`
		Cells          []Cell          `json:"cells"`
		BlockedRegions []BlockedRegion `json:"blocked_regions"`
	}{
		Width:          def.Width,
		Height:         def.Height,
		Cells:          def.Cells,
		BlockedRegions: def.BlockedRegions,
	}
	payload, _ := json.Marshal(canonical)
	sum := sha256.Sum256(append([]byte("tactical-map-v1\n"), payload...))
	return hex.EncodeToString(sum[:])
}

func SortedMemberIDs(s Scenario) []string {
	ids := make([]string, 0, len(s.Formation.Members))
	for _, member := range s.Formation.Members {
		ids = append(ids, member.ID)
	}
	return ids
}

func MemberByID(s Scenario, id string) (Member, bool) {
	for _, member := range s.Formation.Members {
		if member.ID == id {
			return member, true
		}
	}
	return Member{}, false
}

func SlotByID(s Scenario, id string) (Slot, bool) {
	for _, slot := range s.Formation.Slots {
		if slot.ID == id {
			return slot, true
		}
	}
	return Slot{}, false
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	out := copyValues[:0]
	for _, value := range copyValues {
		if value == "" {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func uniqueSortedPoints(points []GridPoint) []GridPoint {
	if len(points) == 0 {
		return nil
	}
	byKey := make(map[string]GridPoint, len(points))
	for _, point := range points {
		byKey[point.Key()] = point
	}
	keys := SortedKeys(byKey)
	out := make([]GridPoint, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out
}
