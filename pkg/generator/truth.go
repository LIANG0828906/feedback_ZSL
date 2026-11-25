package generator

import (
	"math"
	"sort"

	"roomacoustics/pkg/acoustics"
)

type truthEdge struct {
	other  string
	doorID string
}

func BuildTruth(level *acoustics.Level) (*acoustics.TruthFile, error) {
	doors := map[string]acoustics.Door{}
	walls := map[string]acoustics.Wall{}
	reflectors := map[string][]acoustics.Reflector{}
	for _, d := range level.Doors {
		doors[d.ID] = d
	}
	for _, w := range level.Walls {
		walls[w.ID] = w
	}
	adj := map[string][]truthEdge{}
	for _, d := range level.Doors {
		if !d.IsOpen() {
			continue
		}
		w, ok := walls[d.WallID]
		if !ok {
			continue
		}
		adj[w.RoomA] = append(adj[w.RoomA], truthEdge{other: w.RoomB, doorID: d.ID})
		adj[w.RoomB] = append(adj[w.RoomB], truthEdge{other: w.RoomA, doorID: d.ID})
	}
	for room := range adj {
		sort.Slice(adj[room], func(i, j int) bool {
			if adj[room][i].other != adj[room][j].other {
				return adj[room][i].other < adj[room][j].other
			}
			return adj[room][i].doorID < adj[room][j].doorID
		})
	}
	for _, r := range level.Reflectors {
		reflectors[r.RoomID] = append(reflectors[r.RoomID], r)
	}
	for room := range reflectors {
		sort.Slice(reflectors[room], func(i, j int) bool { return reflectors[room][i].ID < reflectors[room][j].ID })
	}

	tf := &acoustics.TruthFile{
		SchemaVersion:    acoustics.TruthSchemaVersion,
		Seed:             level.Seed,
		ParamVersion:     level.Params.ParamVersion,
		AlgorithmVersion: level.Params.AlgorithmVersion,
		Tolerances:       level.Params,
	}

	for _, lis := range sortedListeners(level) {
		lt := acoustics.ListenerTruth{ListenerID: lis.ID, RoomID: lis.RoomID}
		for _, src := range sortedSources(level) {
			st := acoustics.SourceTruth{SourceID: src.ID, SourceRoomID: src.RoomID}
			rooms, doorIDs, visited, ok := bfsTruthChain(adj, src.RoomID, lis.RoomID)
			st.ReachableRooms = visited
			if !ok {
				st.Connected = false
				st.ErrorCode = acoustics.CodeUnreach
				lt.Sources = append(lt.Sources, st)
				continue
			}
			st.Connected = true
			st.RoomChain = rooms
			st.DoorChain = doorIDs

			doorPoints := make([]acoustics.Point, 0, len(doorIDs))
			for _, id := range doorIDs {
				doorPoints = append(doorPoints, doors[id].Center)
			}
			st.PathLengthM = round6(polylineLength(src.Position, doorPoints, lis.Position))

			input := src.Position
			prefix := 0.0
			if len(doorPoints) > 0 {
				input = doorPoints[len(doorPoints)-1]
				prefix = pathLengthToLastDoor(src.Position, doorPoints)
			}

			for _, ref := range reflectors[lis.RoomID] {
				rp, valid := mirrorTruthPoint(input, lis.Position, ref.A, ref.B)
				if !valid {
					continue
				}
				local := distTruth(input, rp) + distTruth(rp, lis.Position)
				total := prefix + local
				angle := incidenceTruthDeg(input, rp, ref.Normal)
				spl := src.PowerDB - 20*math.Log10(math.Max(total, 1e-6)) + 10*math.Log10(math.Max(1-ref.Absorption, 1e-12))
				st.Reflections = append(st.Reflections, acoustics.ReflectionTruth{
					ReflectorID:       ref.ID,
					ReflectionPoint:   acoustics.Point{X: round6(rp.X), Y: round6(rp.Y)},
					PathLengthM:       round6(total),
					IncidenceAngleDeg: round6(angle),
					SPLDB:             round6(spl),
				})
			}
			st.ValidPathCount = len(st.Reflections)
			levels := make([]float64, 0, len(st.Reflections))
			for _, r := range st.Reflections {
				levels = append(levels, r.SPLDB)
			}
			st.ExpectedSPLDB = aggregateTruthSPL(levels)
			lt.Sources = append(lt.Sources, st)
		}
		tf.ListenerTruths = append(tf.ListenerTruths, lt)
	}
	return tf, nil
}

func bfsTruthChain(adj map[string][]truthEdge, start, target string) ([]string, []string, []string, bool) {
	if start == target {
		return []string{start}, []string{}, sortedKeys(adj, start), true
	}
	parentRoom := map[string]string{start: ""}
	parentDoor := map[string]string{start: ""}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range adj[cur] {
			if _, seen := parentRoom[e.other]; seen {
				continue
			}
			parentRoom[e.other] = cur
			parentDoor[e.other] = e.doorID
			queue = append(queue, e.other)
		}
	}
	visited := make([]string, 0, len(parentRoom))
	for r := range parentRoom {
		visited = append(visited, r)
	}
	sort.Strings(visited)
	if _, ok := parentRoom[target]; !ok {
		return nil, nil, visited, false
	}
	var rooms, doors []string
	cur := target
	for cur != start {
		rooms = append([]string{cur}, rooms...)
		doors = append([]string{parentDoor[cur]}, doors...)
		cur = parentRoom[cur]
	}
	rooms = append([]string{start}, rooms...)
	return rooms, doors, visited, true
}

func sortedKeys(adj map[string][]truthEdge, start string) []string {
	seen := map[string]bool{start: true}
	out := []string{start}
	for _, e := range adj[start] {
		if !seen[e.other] {
			seen[e.other] = true
			out = append(out, e.other)
		}
	}
	sort.Strings(out)
	return out
}

func mirrorTruthPoint(s, l, a, b acoustics.Point) (acoustics.Point, bool) {
	dx, dy := b.X-a.X, b.Y-a.Y
	den := dx*dx + dy*dy
	if den == 0 {
		return acoustics.Point{}, false
	}
	t := ((s.X-a.X)*dx + (s.Y-a.Y)*dy) / den
	fx, fy := a.X+t*dx, a.Y+t*dy
	spx, spy := 2*fx-s.X, 2*fy-s.Y
	ex, ey := l.X-spx, l.Y-spy
	numU := (a.X-spx)*dy - (a.Y-spy)*dx
	denU := ex*dy - ey*dx
	if math.Abs(denU) < 1e-15 {
		return acoustics.Point{}, false
	}
	u := numU / denU
	if u < -1e-9 || u > 1+1e-9 {
		return acoustics.Point{}, false
	}
	rx, ry := spx+u*ex, spy+u*ey
	v := ((rx-a.X)*dx + (ry-a.Y)*dy) / den
	if v < -1e-7 || v > 1+1e-7 {
		return acoustics.Point{}, false
	}
	return acoustics.Point{X: rx, Y: ry}, true
}

func incidenceTruthDeg(source, r, normal acoustics.Point) float64 {
	vx, vy := source.X-r.X, source.Y-r.Y
	vl := math.Hypot(vx, vy)
	nl := math.Hypot(normal.X, normal.Y)
	if vl == 0 || nl == 0 {
		return 0
	}
	cos := math.Abs(vx*normal.X+vy*normal.Y) / (vl * nl)
	cos = math.Max(0, math.Min(1, cos))
	return math.Acos(cos) * 180 / math.Pi
}

func polylineLength(src acoustics.Point, doors []acoustics.Point, dst acoustics.Point) float64 {
	points := append([]acoustics.Point{src}, doors...)
	points = append(points, dst)
	total := 0.0
	for i := 1; i < len(points); i++ {
		total += distTruth(points[i-1], points[i])
	}
	return total
}

func pathLengthToLastDoor(src acoustics.Point, doors []acoustics.Point) float64 {
	points := append([]acoustics.Point{src}, doors...)
	total := 0.0
	for i := 1; i < len(points); i++ {
		total += distTruth(points[i-1], points[i])
	}
	return total
}

func distTruth(a, b acoustics.Point) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }

func aggregateTruthSPL(levels []float64) *float64 {
	if len(levels) == 0 {
		return nil
	}
	max := levels[0]
	for _, v := range levels[1:] {
		if v > max {
			max = v
		}
	}
	sum := 0.0
	for _, v := range levels {
		sum += math.Pow(10, (v-max)/10)
	}
	out := max + 10*math.Log10(sum)
	return &out
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
