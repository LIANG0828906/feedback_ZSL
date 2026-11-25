package maskqa

import (
	"fmt"
	"sort"
)

const OrderPolicyPriorityThenIndex = "process_priority ASC, stroke_index ASC"

type TileOrder struct {
	TileID       int    `json:"tile_id"`
	Participants []int  `json:"participants"`
	Order        []int  `json:"order"`
	Basis        string `json:"basis"`
}

type OrderDecision struct {
	Policy         string      `json:"policy"`
	OrderedStrokes []int       `json:"ordered_strokes"`
	OrderVersion   string      `json:"order_version"`
	Tiles          []TileOrder `json:"tiles"`
}

// AdjudicateOrder resolves construction order independent of JSON array order.
func AdjudicateOrder(p *Project, tiles []Tile) (*OrderDecision, error) {
	byIndex := map[int]Stroke{}
	duplicates := map[int]int{}
	for _, s := range p.Strokes {
		if _, exists := byIndex[s.Index]; exists {
			duplicates[s.Index]++
		}
		byIndex[s.Index] = s
	}
	if len(duplicates) > 0 {
		conflicts := make([]int, 0, len(duplicates))
		for idx := range duplicates {
			conflicts = append(conflicts, idx)
		}
		sort.Ints(conflicts)
		e := &Error{Code: CodeORDER, Field: "strokes[].index", Path: "strokes", Conflicts: conflicts, Message: "duplicate stroke index prevents unique order adjudication"}
		e.SourceVersion = p.SourceVersion
		return nil, e
	}

	ordered := append([]Stroke(nil), p.Strokes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Index < ordered[j].Index
	})

	seenPair := map[string][]int{}
	ids := make([]int, 0, len(ordered))
	for _, s := range ordered {
		key := fmt.Sprintf("%d:%d", s.Priority, s.Index)
		seenPair[key] = append(seenPair[key], s.Index)
		ids = append(ids, s.Index)
	}
	var conflict []int
	for _, group := range seenPair {
		if len(group) > 1 {
			conflict = append(conflict, group...)
		}
	}
	if len(conflict) > 0 {
		sort.Ints(conflict)
		e := &Error{Code: CodeORDER, Field: "strokes[].process_priority", Path: "strokes", Conflicts: conflict, Message: "same priority and stroke index prevents unique order"}
		e.SourceVersion = p.SourceVersion
		return nil, e
	}

	decision := &OrderDecision{Policy: OrderPolicyPriorityThenIndex, OrderedStrokes: ids}
	decision.OrderVersion = "order-" + HashJSON(struct {
		Policy string
		Order  []int
	}{decision.Policy, ids})[:16]

	strokeByIndex := map[int]Stroke{}
	for _, s := range p.Strokes {
		strokeByIndex[s.Index] = s
	}
	for _, tile := range tiles {
		to := TileOrder{TileID: tile.ID, Basis: OrderPolicyPriorityThenIndex}
		for _, idx := range ids {
			s := strokeByIndex[idx]
			bounds := StrokeBounds(p, s)
			if !bounds.Intersect(tile.Rect).Empty() {
				to.Participants = append(to.Participants, idx)
				to.Order = append(to.Order, idx)
			}
		}
		decision.Tiles = append(decision.Tiles, to)
	}
	return decision, nil
}

func (o *OrderDecision) Tile(tileID int) (TileOrder, bool) {
	for _, t := range o.Tiles {
		if t.TileID == tileID {
			return t, true
		}
	}
	return TileOrder{}, false
}
