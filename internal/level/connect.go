package level

import "sort"

// Directions: 0=N, 1=E, 2=S, 3=W. Opposite direction is d^2.
var dirDelta = [4][2]int{{-1, 0}, {0, 1}, {1, 0}, {0, -1}}

func gridIndex(proj *Project) map[[2]int]int {
	idx := make(map[[2]int]int, len(proj.Partitions))
	for _, pc := range proj.Partitions {
		idx[[2]int{pc.Row, pc.Col}] = pc.ID
	}
	return idx
}

func nodePassable(proj *Project, parts map[int]*PartitionResult, id int) bool {
	pc := proj.PartitionByID(id)
	if pc == nil {
		return false
	}
	res, ok := parts[id]
	if !ok || res.Failed {
		return false
	}
	return res.PassablePct >= pc.MinPassablePct
}

func edgeOpen(parts map[int]*PartitionResult, a, b, dir int) bool {
	ra, oka := parts[a]
	rb, okb := parts[b]
	if !oka || !okb {
		return false
	}
	return ra.Edges[dir] && rb.Edges[dir^2]
}

func bfs(proj *Project, parts map[int]*PartitionResult, idx map[[2]int]int, start int) []int {
	visited := map[int]bool{}
	if !nodePassable(proj, parts, start) {
		return nil
	}
	queue := []int{start}
	visited[start] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		pc := proj.PartitionByID(cur)
		for dir := 0; dir < 4; dir++ {
			nr, nc := pc.Row+dirDelta[dir][0], pc.Col+dirDelta[dir][1]
			nid, ok := idx[[2]int{nr, nc}]
			if !ok || visited[nid] {
				continue
			}
			if !edgeOpen(parts, cur, nid, dir) || !nodePassable(proj, parts, nid) {
				continue
			}
			visited[nid] = true
			queue = append(queue, nid)
		}
	}
	out := make([]int, 0, len(visited))
	for id := range visited {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// EnsureConnectivity checks spawn-to-target connectivity over the
// partition graph and repairs broken adjacencies in a controlled way:
// only the boundary passable band of two adjacent partitions may be
// carved, and neither side may leave its difficulty budget range. The
// function is a deterministic fixpoint: applying it to an already
// repaired state is a no-op, which is what makes the incremental, replay
// and full-regeneration paths agree byte-for-byte.
func EnsureConnectivity(proj *Project, parts map[int]*PartitionResult) (*ConnectivityResult, error) {
	idx := gridIndex(proj)
	spawnID, ok := idx[[2]int{proj.Spawn.Row, proj.Spawn.Col}]
	if !ok {
		return nil, paraErr("spawn", -1, "spawn partition not found in grid")
	}
	res := &ConnectivityResult{}
	maxIters := len(parts)*2 + 8
	for iter := 0; iter < maxIters; iter++ {
		reachable := bfs(proj, parts, idx, spawnID)
		unreachable := unreachableTargets(proj, reachable)
		if len(unreachable) == 0 {
			res.Connected = true
			res.Reachable = reachable
			res.UnreachableTargets = nil
			return res, nil
		}
		if !repairFrontier(proj, parts, idx, reachable, res) {
			return res, connErr(unreachable, reachable)
		}
	}
	reachable := bfs(proj, parts, idx, spawnID)
	return res, connErr(unreachableTargets(proj, reachable), reachable)
}

func unreachableTargets(proj *Project, reachable []int) []string {
	in := make(map[int]bool, len(reachable))
	for _, id := range reachable {
		in[id] = true
	}
	var out []string
	for _, t := range proj.Targets {
		if !in[t.Partition] {
			out = append(out, t.ID)
		}
	}
	sort.Strings(out)
	return out
}

// repairFrontier scans edges from the reachable set outward in a fixed
// order and repairs the first repairable closed edge. Returns false when
// no frontier edge can be repaired.
func repairFrontier(proj *Project, parts map[int]*PartitionResult, idx map[[2]int]int, reachable []int, res *ConnectivityResult) bool {
	in := make(map[int]bool, len(reachable))
	for _, id := range reachable {
		in[id] = true
	}
	ids := append([]int(nil), reachable...)
	sort.Ints(ids)
	for _, a := range ids {
		pa := proj.PartitionByID(a)
		for dir := 0; dir < 4; dir++ {
			nid, ok := idx[[2]int{pa.Row + dirDelta[dir][0], pa.Col + dirDelta[dir][1]}]
			if !ok || in[nid] {
				continue
			}
			if edgeOpen(parts, a, nid, dir) {
				continue
			}
			rec, err := repairEdge(proj, parts, a, nid, dir)
			if err == nil {
				res.Repairs = append(res.Repairs, *rec)
				return true
			}
		}
	}
	return false
}

func bandCells(dir int) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < RefDensity; i++ {
		switch dir {
		case 0:
			out[i] = true
		case 1:
			out[i*RefDensity+RefDensity-1] = true
		case 2:
			out[(RefDensity-1)*RefDensity+i] = true
		case 3:
			out[i*RefDensity] = true
		}
	}
	return out
}

func borderCell(cell int) bool {
	r, c := cell/RefDensity, cell%RefDensity
	return r == 0 || c == 0 || r == RefDensity-1 || c == RefDensity-1
}

// repairEdge opens the shared edge between adjacent partitions a and b by
// carving the boundary passable band on both sides. Band obstacles are
// removed; if that would drop a side below its budget minimum,
// compensating obstacles are added from the deterministic reserve pool
// (never on border cells, so later repairs stay idempotent). Neither side
// may exceed its budget maximum. The repair is planned first and applied
// only when feasible for both sides.
func repairEdge(proj *Project, parts map[int]*PartitionResult, a, b, dir int) (*RepairRecord, error) {
	if !nodePassable(proj, parts, a) || !nodePassable(proj, parts, b) {
		return nil, connErr(nil, []int{a, b})
	}
	pca := proj.PartitionByID(a)
	pcb := proj.PartitionByID(b)
	ra := parts[a]
	rb := parts[b]

	type side struct {
		pc   *PartitionConstraint
		res  *PartitionResult
		band map[int]bool
		drop []int
		add  []int
	}
	sides := []side{
		{pc: pca, res: ra, band: bandCells(dir)},
		{pc: pcb, res: rb, band: bandCells(dir ^ 2)},
	}
	for s := range sides {
		sd := &sides[s]
		for i, ob := range sd.res.Obstacles {
			if sd.band[ob.Cell] {
				sd.drop = append(sd.drop, i)
			}
		}
		remaining := append([]Obstacle(nil), sd.res.Obstacles...)
		for k := len(sd.drop) - 1; k >= 0; k-- {
			remaining = append(remaining[:sd.drop[k]], remaining[sd.drop[k]+1:]...)
		}
		diff := int64(len(sd.res.Resources)) * ResourceDifficulty
		for _, ob := range remaining {
			diff += ob.Cost
		}
		used := map[int]bool{}
		for diff < sd.pc.BudgetMin {
			pick := -1
			for i, ob := range sd.res.Reserve {
				if used[i] || borderCell(ob.Cell) {
					continue
				}
				if pick < 0 || ob.Cost > sd.res.Reserve[pick].Cost {
					pick = i
				}
			}
			if pick < 0 {
				return nil, connErr(nil, []int{a, b})
			}
			used[pick] = true
			diff += sd.res.Reserve[pick].Cost
			if diff > sd.pc.BudgetMax {
				return nil, connErr(nil, []int{a, b})
			}
			sd.add = append(sd.add, pick)
		}
	}

	rec := &RepairRecord{Edge: [2]int{a, b}}
	for s := range sides {
		sd := &sides[s]
		for k := len(sd.drop) - 1; k >= 0; k-- {
			i := sd.drop[k]
			rec.RemovedObstacles = append(rec.RemovedObstacles, sd.res.Obstacles[i].ID)
			sd.res.Obstacles = append(sd.res.Obstacles[:i], sd.res.Obstacles[i+1:]...)
		}
		for _, ri := range sd.add {
			added := sd.res.Reserve[ri]
			rec.AddedObstacles = append(rec.AddedObstacles, added.ID)
			sd.res.Obstacles = append(sd.res.Obstacles, added)
		}
		if len(sd.add) > 0 {
			mark := map[int]bool{}
			for _, ri := range sd.add {
				mark[ri] = true
			}
			kept := sd.res.Reserve[:0]
			for i, ob := range sd.res.Reserve {
				if !mark[i] {
					kept = append(kept, ob)
				}
			}
			sd.res.Reserve = kept
		}
		sd.res.Difficulty = DifficultyOf(sd.res)
		recomputePassable(sd.res)
	}
	ra.Edges[dir] = true
	rb.Edges[dir^2] = true
	return rec, nil
}
