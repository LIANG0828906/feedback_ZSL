package tactical

import (
	"container/heap"
	"fmt"
)

const (
	DefaultMaxExpansions = 5_000_000
	DefaultHeuristic     = 1000
)

type HeuristicFunc func(pos, goal GridPoint, exactRemaining int64) int64

type SearchOptions struct {
	Neighbors            int
	HeuristicWeightMilli int64
	MaxExpansions        int
	MaxCost              int64
	Heuristic            HeuristicFunc
}

type searchModel struct {
	def       MapDef
	width     int
	height    int
	neighbors int
	moves     []GridPoint
}

func newSearchModel(def MapDef, neighbors int) (*searchModel, error) {
	if neighbors != 4 && neighbors != 8 {
		return nil, schemaError("neighbors", "neighbors must be 4 or 8")
	}
	if len(def.Cells) != def.Width*def.Height {
		return nil, schemaError("map.cells", "cell count does not match width*height")
	}
	moves := []GridPoint{
		{X: 0, Y: -1},
		{X: 1, Y: 0},
		{X: 0, Y: 1},
		{X: -1, Y: 0},
	}
	if neighbors == 8 {
		moves = append(moves,
			GridPoint{X: 1, Y: -1},
			GridPoint{X: 1, Y: 1},
			GridPoint{X: -1, Y: 1},
			GridPoint{X: -1, Y: -1},
		)
	}
	return &searchModel{
		def:       def,
		width:     def.Width,
		height:    def.Height,
		neighbors: neighbors,
		moves:     moves,
	}, nil
}

func (m *searchModel) index(p GridPoint) int {
	return p.Y*m.width + p.X
}

func (m *searchModel) point(index int) GridPoint {
	return GridPoint{X: index % m.width, Y: index / m.width}
}

func (m *searchModel) passable(p GridPoint) bool {
	return p.InBounds(m.width, m.height) && m.def.Cells[m.index(p)].Passable
}

func (m *searchModel) forwardEdge(from, to GridPoint) (int64, bool) {
	if !to.InBounds(m.width, m.height) {
		return 0, false
	}
	target := m.def.Cells[m.index(to)]
	if !target.Passable {
		return 0, false
	}
	dx := to.X - from.X
	dy := to.Y - from.Y
	if dx != 0 && dy != 0 {
		if !m.passable(GridPoint{X: from.X + dx, Y: from.Y}) ||
			!m.passable(GridPoint{X: from.X, Y: from.Y + dy}) {
			return 0, false
		}
	}
	return target.Cost, true
}

// exactRemaining computes an admissible heuristic table by running Dijkstra
// backwards from the goal. Diagonal moves obey the same corner-cut rule as
// forward search.
func (m *searchModel) exactRemaining(goal GridPoint) ([]int64, error) {
	const unreachable = int64(-1)
	dist := make([]int64, len(m.def.Cells))
	for i := range dist {
		dist[i] = unreachable
	}
	if !m.passable(goal) {
		return dist, nil
	}
	goalIdx := m.index(goal)
	dist[goalIdx] = 0
	pq := &nodeHeap{}
	heap.Init(pq)
	heap.Push(pq, searchNode{index: goalIdx, cost: 0, heuristic: 0, priority: 0})
	for pq.Len() > 0 {
		item := heap.Pop(pq).(searchNode)
		if item.cost != dist[item.index] {
			continue
		}
		current := m.point(item.index)
		for _, move := range m.moves {
			predecessor := GridPoint{X: current.X - move.X, Y: current.Y - move.Y}
			if !predecessor.InBounds(m.width, m.height) {
				continue
			}
			cost, ok := m.forwardEdge(predecessor, current)
			if !ok {
				continue
			}
			candidate := item.cost + cost
			if candidate < 0 || candidate > MaxPathCost {
				return nil, memberError(CodeHeur, "", "path cost exceeded the fixed integer overflow boundary")
			}
			predIdx := m.index(predecessor)
			if dist[predIdx] == unreachable || candidate < dist[predIdx] {
				dist[predIdx] = candidate
				heap.Push(pq, searchNode{
					index: predIdx, cost: candidate, heuristic: 0,
					priority: candidate,
				})
			}
		}
	}
	return dist, nil
}

// WeightedHeuristic applies a fixed-point weight in [0,1000] without overflow.
func WeightedHeuristic(exact, weightMilli int64) int64 {
	if exact <= 0 {
		return 0
	}
	if weightMilli <= 0 {
		return 0
	}
	if weightMilli > 1000 {
		weightMilli = 1000
	}
	whole := (exact / 1000) * weightMilli
	frac := ((exact % 1000) * weightMilli) / 1000
	return whole + frac
}

func SearchMember(s Scenario, member Member, opts SearchOptions) (*MemberPath, error) {
	if opts.Neighbors == 0 {
		opts.Neighbors = member.Neighbors
	}
	if opts.HeuristicWeightMilli < 0 {
		opts.HeuristicWeightMilli = member.HeuristicWeightMilli
	}
	if opts.MaxExpansions <= 0 {
		opts.MaxExpansions = DefaultMaxExpansions
	}
	if opts.MaxCost <= 0 {
		opts.MaxCost = MaxPathCost
	}
	if opts.MaxCost > MaxPathCost {
		opts.MaxCost = MaxPathCost
	}
	if opts.HeuristicWeightMilli < 0 || opts.HeuristicWeightMilli > 1000 {
		return nil, memberError(CodeHeur, member.ID, "heuristic weight must be in 0..1000")
	}
	effective := EffectiveMap(s.Map)
	model, err := newSearchModel(effective, opts.Neighbors)
	if err != nil {
		return nil, err
	}
	if !member.Start.InBounds(model.width, model.height) || !member.End.InBounds(model.width, model.height) {
		return nil, memberError(CodeSchema, member.ID, "start or end is outside map bounds")
	}
	if !model.passable(member.Start) {
		return nil, &Error{Code: CodeSchema, MemberID: member.ID, Field: "start", Message: fmt.Sprintf("start cell (%d,%d) is impassable", member.Start.X, member.Start.Y)}
	}
	if !model.passable(member.End) {
		return nil, &Error{Code: CodeSchema, MemberID: member.ID, Field: "end", Message: fmt.Sprintf("end cell (%d,%d) is impassable", member.End.X, member.End.Y)}
	}

	exact, err := model.exactRemaining(member.End)
	if err != nil {
		target, ok := AsError(err)
		if ok {
			target.MemberID = member.ID
		}
		return nil, err
	}
	heuristic := opts.Heuristic
	if heuristic == nil {
		heuristic = func(_, _ GridPoint, remaining int64) int64 { return remaining }
	}
	heuristicValues := make([]int64, len(exact))
	for i, remaining := range exact {
		if remaining < 0 {
			heuristicValues[i] = 0
			continue
		}
		value := heuristic(model.point(i), member.End, remaining)
		if value < 0 || value > remaining {
			return nil, memberError(CodeHeur, member.ID,
				fmt.Sprintf("heuristic %d overestimates exact remaining cost %d at cell %d,%d", value, remaining, i%model.width, i/model.width))
		}
		heuristicValues[i] = value
	}
	startIdx := model.index(member.Start)
	if exact[startIdx] < 0 {
		return nil, memberError(CodeHeur, member.ID, "no passable route exists between start and end")
	}

	const infinity = int64(1<<63 - 1)
	gScore := make([]int64, len(model.def.Cells))
	previous := make([]int, len(model.def.Cells))
	for i := range gScore {
		gScore[i] = infinity
		previous[i] = -1
	}
	gScore[startIdx] = 0
	pq := &nodeHeap{}
	heap.Init(pq)
	startH := WeightedHeuristic(heuristicValues[startIdx], opts.HeuristicWeightMilli)
	lineTie := func(p GridPoint) int64 {
		return lineDeviation(member.Start, member.End, p)
	}
	heap.Push(pq, searchNode{
		index: startIdx, cost: 0, heuristic: heuristicValues[startIdx],
		priority: startH, tie: lineTie(member.Start),
	})
	goalIdx := model.index(member.End)
	expanded := 0
	found := false
	for pq.Len() > 0 {
		item := heap.Pop(pq).(searchNode)
		if item.cost != gScore[item.index] {
			continue
		}
		expanded++
		if expanded > opts.MaxExpansions {
			return nil, memberError(CodeHeur, member.ID, "search expansion budget exhausted")
		}
		if item.index == goalIdx {
			found = true
			break
		}
		current := model.point(item.index)
		for _, move := range model.moves {
			next := GridPoint{X: current.X + move.X, Y: current.Y + move.Y}
			cost, ok := model.forwardEdge(current, next)
			if !ok {
				continue
			}
			if item.cost > opts.MaxCost-cost {
				return nil, memberError(CodeHeur, member.ID, "accumulated path cost exceeded the fixed integer overflow boundary")
			}
			candidate := item.cost + cost
			nextIdx := model.index(next)
			if candidate >= gScore[nextIdx] {
				continue
			}
			gScore[nextIdx] = candidate
			previous[nextIdx] = item.index
			weighted := WeightedHeuristic(heuristicValues[nextIdx], opts.HeuristicWeightMilli)
			priority := candidate + weighted
			if priority < candidate || priority > MaxPathCost*2 {
				return nil, memberError(CodeHeur, member.ID, "priority exceeded the fixed integer overflow boundary")
			}
			heap.Push(pq, searchNode{
				index: nextIdx, cost: candidate,
				heuristic: heuristicValues[nextIdx], priority: priority,
				tie: lineTie(next),
			})
		}
	}
	if !found {
		return nil, memberError(CodeHeur, member.ID, "no passable route exists between start and end")
	}
	indices := make([]int, 0, 64)
	for at := goalIdx; at >= 0; at = previous[at] {
		indices = append(indices, at)
		if at == startIdx {
			break
		}
	}
	if len(indices) == 0 || indices[len(indices)-1] != startIdx {
		return nil, memberError(CodeHeur, member.ID, "failed to reconstruct a complete path")
	}
	cells := make([]GridPoint, 0, len(indices))
	for i := len(indices) - 1; i >= 0; i-- {
		cells = append(cells, model.point(indices[i]))
	}
	deps := pathDependencies(effective, cells)
	return &MemberPath{
		MemberID:             member.ID,
		Cells:                cells,
		Cost:                 gScore[goalIdx],
		Expanded:             expanded,
		HeuristicWeightMilli: opts.HeuristicWeightMilli,
		Neighbors:            opts.Neighbors,
		Dependencies:         deps,
	}, nil
}

func SearchGrid(def MapDef, from, end GridPoint, opts SearchOptions) (*MemberPath, error) {
	scenario := Scenario{Map: EffectiveMap(def)}
	member := Member{
		ID: "probe", Start: from, End: end,
		Neighbors:            4,
		HeuristicWeightMilli: 1000,
	}
	if opts.Neighbors == 0 {
		opts.Neighbors = 4
	}
	if opts.HeuristicWeightMilli == 0 {
		opts.HeuristicWeightMilli = 1000
	}
	return SearchMember(scenario, member, opts)
}

func pathDependencies(def MapDef, cells []GridPoint) PathDependencies {
	terrains := make([]string, 0, len(cells))
	regions := make([]string, 0, len(cells))
	for _, point := range cells {
		cell, ok := CellAt(def, point)
		if !ok {
			continue
		}
		terrains = append(terrains, cell.Terrain)
		if cell.BlockedBy != "" {
			regions = append(regions, cell.BlockedBy)
		}
	}
	return PathDependencies{
		Cells:          uniqueSortedPoints(cells),
		Terrains:       uniqueSorted(terrains),
		BlockedRegions: uniqueSorted(regions),
	}
}

type searchNode struct {
	index     int
	cost      int64
	heuristic int64
	priority  int64
	tie       int64
	seq       uint64
}

type nodeHeap []searchNode

func (h nodeHeap) Len() int { return len(h) }
func (h nodeHeap) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority < h[j].priority
	}
	if h[i].tie != h[j].tie {
		return h[i].tie < h[j].tie
	}
	if h[i].heuristic != h[j].heuristic {
		return h[i].heuristic < h[j].heuristic
	}
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	if h[i].index != h[j].index {
		return h[i].index < h[j].index
	}
	return h[i].seq < h[j].seq
}

func lineDeviation(start, goal, point GridPoint) int64 {
	dx := int64(goal.X - start.X)
	dy := int64(goal.Y - start.Y)
	if dx == 0 && dy == 0 {
		return 0
	}
	px := int64(point.X - start.X)
	py := int64(point.Y - start.Y)
	cross := dx*py - dy*px
	if cross < 0 {
		cross = -cross
	}
	return cross
}
func (h nodeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)   { *h = append(*h, x.(searchNode)) }
func (h *nodeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
