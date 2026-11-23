package tactical

import (
	"fmt"
	"sort"
	"strings"
)

// BuildDependencyGraph records concrete path-cell dependencies and formation
// coupling intervals. The graph is persisted with the checkpoint.
func BuildDependencyGraph(s Scenario, paths map[string]MemberPath, smooth map[string]SmoothedPath, traces map[string]Trace) DependencyGraph {
	order := SortedMemberIDs(s)
	groups := make(map[string][]Member)
	graph := DependencyGraph{
		CellToMembers: make(map[string][]string),
	}
	for _, member := range s.Formation.Members {
		groups[member.Group] = append(groups[member.Group], member)
	}
	for _, id := range order {
		path, ok := paths[id]
		dep := MemberDependency{MemberID: id}
		if ok {
			effective := EffectiveMap(s.Map)
			allCells := append([]GridPoint(nil), path.Cells...)
			if smoothed, exists := smooth[id]; exists {
				for i := 0; i < len(smoothed.Corners)-1; i++ {
					allCells = append(allCells, lineCells(smoothed.Corners[i].Grid, smoothed.Corners[i+1].Grid)...)
				}
			}
			dep.PathCells = uniqueSortedPoints(allCells)
			terrains := make([]string, 0)
			regions := make([]string, 0)
			for _, point := range dep.PathCells {
				cell, cellOK := CellAt(effective, point)
				if !cellOK {
					continue
				}
				terrains = append(terrains, cell.Terrain)
				if cell.BlockedBy != "" {
					regions = append(regions, cell.BlockedBy)
				}
			}
			dep.Terrains = uniqueSorted(terrains)
			dep.BlockedRegions = append([]string(nil), path.Dependencies.BlockedRegions...)
			dep.BlockedRegions = uniqueSorted(append(dep.BlockedRegions, regions...))
			for _, cell := range dep.PathCells {
				key := cell.Key()
				graph.CellToMembers[key] = append(graph.CellToMembers[key], id)
			}
		}
		member, _ := MemberByID(s, id)
		for _, other := range groups[member.Group] {
			if other.ID != id {
				dep.CoupledMembers = append(dep.CoupledMembers, other.ID)
			}
		}
		dep.CoupledMembers = uniqueSorted(dep.CoupledMembers)
		graph.Members = append(graph.Members, dep)
	}
	for key, ids := range graph.CellToMembers {
		graph.CellToMembers[key] = uniqueSorted(ids)
	}
	for _, member := range s.Formation.Members {
		slot, ok := SlotByID(s, member.SlotID)
		if !ok || slot.Anchor == member.ID {
			continue
		}
		anchorLen := len(traces[slot.Anchor].States)
		memberLen := len(traces[member.ID].States)
		last := anchorLen
		if memberLen > last {
			last = memberLen
		}
		if last == 0 {
			continue
		}
		graph.Couplings = append(graph.Couplings, CouplingInterval{
			MemberID:        slot.Anchor,
			CoupledMemberID: member.ID,
			Interval:        TickInterval{Start: 0, End: last - 1},
			Reason:          "formation slot " + slot.ID,
		})
	}
	sort.SliceStable(graph.Couplings, func(i, j int) bool {
		if graph.Couplings[i].MemberID != graph.Couplings[j].MemberID {
			return graph.Couplings[i].MemberID < graph.Couplings[j].MemberID
		}
		return graph.Couplings[i].CoupledMemberID < graph.Couplings[j].CoupledMemberID
	})
	return graph
}

// TopoMemberOrder orders explicit member dependencies before slot anchors and
// then uses scenario order as the stable tie-breaker.
func TopoMemberOrder(s Scenario) ([]string, error) {
	order := SortedMemberIDs(s)
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	indegree := make(map[string]int, len(order))
	edges := make(map[string][]string, len(order))
	addEdge := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		edges[from] = append(edges[from], to)
		indegree[to]++
	}
	for _, member := range s.Formation.Members {
		for _, dependency := range member.DependsOn {
			addEdge(dependency, member.ID)
		}
		if slot, ok := SlotByID(s, member.SlotID); ok {
			addEdge(slot.Anchor, member.ID)
		}
	}
	ready := make([]string, 0, len(order))
	for _, id := range order {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return position[ready[i]] < position[ready[j]] })
	out := make([]string, 0, len(order))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		next := edges[id]
		for _, target := range next {
			indegree[target]--
			if indegree[target] == 0 {
				ready = append(ready, target)
				sort.Slice(ready, func(i, j int) bool { return position[ready[i]] < position[ready[j]] })
			}
		}
	}
	if len(out) != len(order) {
		return nil, schemaError("formation.members.depends_on", "member dependency graph contains a cycle")
	}
	return out, nil
}

func changedMapCells(oldMap, newMap MapDef) []CellChange {
	if oldMap.Width != newMap.Width || oldMap.Height != newMap.Height {
		changes := make([]CellChange, 0)
		for y := 0; y < newMap.Height; y++ {
			for x := 0; x < newMap.Width; x++ {
				changes = append(changes, CellChange{
					Point:  GridPoint{X: x, Y: y},
					Reason: "map dimensions changed",
				})
			}
		}
		return changes
	}
	changes := make([]CellChange, 0)
	for y := 0; y < oldMap.Height; y++ {
		for x := 0; x < oldMap.Width; x++ {
			oldCell := oldMap.Cells[y*oldMap.Width+x]
			newCell := newMap.Cells[y*newMap.Width+x]
			if oldCell.Cost == newCell.Cost && oldCell.Passable == newCell.Passable && oldCell.BlockedBy == newCell.BlockedBy {
				continue
			}
			reason := "cell cost"
			if oldCell.Passable != newCell.Passable {
				reason = "cell passability"
			}
			if oldCell.BlockedBy != newCell.BlockedBy {
				reason = "blocked region"
			}
			changes = append(changes, CellChange{
				Point: GridPoint{X: x, Y: y}, OldCost: oldCell.Cost, NewCost: newCell.Cost,
				OldPassable: oldCell.Passable, NewPassable: newCell.Passable,
				OldBlockedBy: oldCell.BlockedBy, NewBlockedBy: newCell.BlockedBy,
				Reason: reason,
			})
		}
	}
	return changes
}

// PlanInvalidation reports only members whose concrete path cell set touches a
// changed cell, plus the formation-coupled simulation intervals that depend on
// those members.
func PlanInvalidation(oldScenario, newScenario Scenario, snapshot *Snapshot) (InvalidationReport, []string) {
	oldMap := EffectiveMap(oldScenario.Map)
	newMap := EffectiveMap(newScenario.Map)
	changes := changedMapCells(oldMap, newMap)
	report := InvalidationReport{
		ChangedCells: changes,
		Topology:     mustTopo(newScenario),
	}
	if snapshot == nil || len(changes) == 0 {
		return report, nil
	}
	direct := make(map[string]struct{})
	directReason := make(map[string]string)
	for _, change := range changes {
		for _, memberID := range snapshot.Dependencies.CellToMembers[change.Point.Key()] {
			direct[memberID] = struct{}{}
			directReason[memberID] = "path depends on edited map cell"
		}
		if expandsRoutingOptions(change) {
			for _, member := range newScenario.Formation.Members {
				direct[member.ID] = struct{}{}
				directReason[member.ID] = "cell re-opened or became cheaper; an alternative optimal route is possible"
			}
		}
	}
	order := SortedMemberIDs(newScenario)
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	pathInvalid := make([]string, 0, len(direct))
	for id := range direct {
		pathInvalid = append(pathInvalid, id)
	}
	sort.Slice(pathInvalid, func(i, j int) bool { return position[pathInvalid[i]] < position[pathInvalid[j]] })
	simulationInvalid := make(map[string]string)
	maxTick := make(map[string]int)
	for _, member := range newScenario.Formation.Members {
		if trace, ok := snapshot.Traces[member.ID]; ok && len(trace.States) > 0 {
			maxTick[member.ID] = len(trace.States) - 1
		}
	}
	for _, id := range pathInvalid {
		last := maxTick[id]
		reason := directReason[id]
		if reason == "" {
			reason = "path depends on edited map cell"
		}
		report.Entries = append(report.Entries, Invalidation{
			MemberID: id, Kind: InvalidatePath,
			Interval: TickInterval{Start: 0, End: last}, Reason: reason,
		})
		simulationInvalid[id] = reason
		member, _ := MemberByID(newScenario, id)
		for _, other := range newScenario.Formation.Members {
			if other.ID == id || other.Group != member.Group {
				continue
			}
			simulationInvalid[other.ID] = "formation coupling with " + id
		}
	}
	simIDs := make([]string, 0, len(simulationInvalid))
	for id := range simulationInvalid {
		simIDs = append(simIDs, id)
	}
	sort.Slice(simIDs, func(i, j int) bool { return position[simIDs[i]] < position[simIDs[j]] })
	for _, id := range simIDs {
		last := maxTick[id]
		report.Entries = append(report.Entries, Invalidation{
			MemberID: id, Kind: InvalidateSimulation,
			Interval: TickInterval{Start: 0, End: last},
			Reason:   simulationInvalid[id],
		})
	}
	sort.SliceStable(report.Entries, func(i, j int) bool {
		a, b := report.Entries[i], report.Entries[j]
		if position[a.MemberID] != position[b.MemberID] {
			return position[a.MemberID] < position[b.MemberID]
		}
		if a.Kind != b.Kind {
			return a.Kind == InvalidatePath
		}
		if a.Interval.Start != b.Interval.Start {
			return a.Interval.Start < b.Interval.Start
		}
		if a.Interval.End != b.Interval.End {
			return a.Interval.End < b.Interval.End
		}
		return a.Reason < b.Reason
	})
	return report, pathInvalid
}

func expandsRoutingOptions(change CellChange) bool {
	if !change.OldPassable && change.NewPassable {
		return true
	}
	if change.OldPassable && change.NewPassable && change.NewCost < change.OldCost {
		return true
	}
	return change.OldBlockedBy != "" && change.NewBlockedBy == ""
}

func mustTopo(s Scenario) []string {
	order, err := TopoMemberOrder(s)
	if err != nil {
		return nil
	}
	return order
}

func invalidationFingerprint(entries []Invalidation) string {
	var b strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&b, "%s|%s|%d|%d|%s;", entry.MemberID, entry.Kind, entry.Interval.Start, entry.Interval.End, entry.Reason)
	}
	return b.String()
}
