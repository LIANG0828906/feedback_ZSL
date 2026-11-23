package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"roomreverb/internal/errs"
	"roomreverb/internal/fixed"
	"roomreverb/internal/model"
)

type Bundle struct {
	Level model.Level
	Truth model.Truth
	Edits model.EditFile
}

type rng struct{ state uint64 }

func newRNG(seed int64) *rng {
	s := uint64(seed) + 0x9e3779b97f4a7c15
	if s == 0 {
		s = 1
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) float(lo, hi float64) float64 {
	u := float64(r.next()>>11) / float64(1<<53)
	return lo + u*(hi-lo)
}

// Generate creates the fixed local level. Room topology is stable while
// acoustic parameters, portal widths and portal positions are seed-dependent.
func Generate(seed int64) (Bundle, error) {
	r := newRNG(seed)
	rooms := []model.Room{
		{ID: "r0", Name: "Atrium", Rect: model.Rect{X: 600, Y: 600, W: 1800, H: 1400}},
		{ID: "r1", Name: "Gallery", Rect: model.Rect{X: 2400, Y: 600, W: 1600, H: 1400}},
		{ID: "r2", Name: "Control", Rect: model.Rect{X: 4000, Y: 600, W: 1400, H: 1400}},
		{ID: "r3", Name: "Machine", Rect: model.Rect{X: 800, Y: 2000, W: 1400, H: 1300}},
		{ID: "r4", Name: "Hall", Rect: model.Rect{X: 2200, Y: 2000, W: 1800, H: 1300}},
		{ID: "r5", Name: "Storage", Rect: model.Rect{X: 4000, Y: 2000, W: 1600, H: 1300}},
	}
	for i := range rooms {
		rooms[i].Params = model.RoomParams{
			T60MS:        round6(r.float(720, 1650)),
			EarlyDensity: round6(r.float(0.52, 0.92)),
			ReverbGain:   round6(r.float(0.86, 1.22)),
		}
	}
	walls := []model.Wall{
		{ID: "w01", Start: model.Point{X: 2400, Y: 600}, End: model.Point{X: 2400, Y: 2000}, Rooms: []string{"r0", "r1"}},
		{ID: "w12", Start: model.Point{X: 4000, Y: 600}, End: model.Point{X: 4000, Y: 2000}, Rooms: []string{"r1", "r2"}},
		{ID: "w03", Start: model.Point{X: 800, Y: 2000}, End: model.Point{X: 2200, Y: 2000}, Rooms: []string{"r0", "r3"}},
		{ID: "w14", Start: model.Point{X: 2400, Y: 2000}, End: model.Point{X: 4000, Y: 2000}, Rooms: []string{"r1", "r4"}},
		{ID: "w25", Start: model.Point{X: 4000, Y: 2000}, End: model.Point{X: 5400, Y: 2000}, Rooms: []string{"r2", "r5"}},
		{ID: "w34", Start: model.Point{X: 2200, Y: 2000}, End: model.Point{X: 2200, Y: 3300}, Rooms: []string{"r3", "r4"}},
		{ID: "w45", Start: model.Point{X: 4000, Y: 2000}, End: model.Point{X: 4000, Y: 3300}, Rooms: []string{"r4", "r5"}},
	}
	opening := func(id, wallID string, x, y, baseWidth float64) model.Opening {
		w, _ := findWall(walls, wallID)
		width := round6(baseWidth + r.float(-35, 35))
		center := model.Point{X: x, Y: y}
		jitter := r.float(-18, 18)
		if w.Start.X == w.End.X {
			center.Y += jitter
		} else {
			center.X += jitter
		}
		center.X, center.Y = round6(center.X), round6(center.Y)
		return model.Opening{ID: id, WallID: wallID, Center: center, Width: width, Active: true}
	}
	openings := []model.Opening{
		opening("o01", "w01", 2400, 1300, 420),
		opening("o12", "w12", 4000, 1450, 360),
		opening("o03", "w03", 1450, 2000, 400),
		opening("o14", "w14", 3250, 2000, 430),
		opening("o25", "w25", 4720, 2000, 380),
		opening("o34", "w34", 2200, 2720, 390),
		opening("o45", "w45", 4000, 2640, 350),
	}
	// Portal jitter must remain inside each wall interval.
	for i := range openings {
		w, _ := findWall(walls, openings[i].WallID)
		openings[i] = clampOpening(w, openings[i])
	}
	sources := []model.Source{
		{ID: "s0", Pos: model.Point{X: 1050, Y: 980}},
		{ID: "s1", Pos: model.Point{X: 3030, Y: 2740}},
		{ID: "s2", Pos: model.Point{X: 4650, Y: 1020}},
	}
	listeners := []model.Listener{
		{ID: "l0", Pos: model.Point{X: 1800, Y: 1450}},
		{ID: "l1", Pos: model.Point{X: 2860, Y: 1080}},
		{ID: "l2", Pos: model.Point{X: 4540, Y: 1550}},
		{ID: "l3", Pos: model.Point{X: 1320, Y: 2920}},
		{ID: "l4", Pos: model.Point{X: 3420, Y: 2500}},
		{ID: "l5", Pos: model.Point{X: 5060, Y: 2530}},
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
	sort.Slice(walls, func(i, j int) bool { return walls[i].ID < walls[j].ID })
	sort.Slice(openings, func(i, j int) bool { return openings[i].ID < openings[j].ID })
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	level := model.Level{
		SchemaVersion: model.SchemaVersion,
		ID:            fmt.Sprintf("level-seed-%d", seed),
		Seed:          seed,
		Units:         "mm",
		Rooms:         rooms, Walls: walls, Openings: openings,
		Sources: sources, Listeners: listeners,
	}
	truth, err := buildTruth(level)
	if err != nil {
		return Bundle{}, err
	}
	edits := model.EditFile{
		LevelID: level.ID, AlgorithmVersion: model.AlgorithmVersion,
		Edits: []model.Edit{
			{Kind: model.EditSetRoomParams, RoomID: "r1", Params: &model.RoomParams{T60MS: 940, EarlyDensity: 0.70, ReverbGain: 1.08}},
			{Kind: model.EditRemovePortal, PortalID: "o34"},
			{Kind: model.EditSetRoomParams, RoomID: "r5", Params: &model.RoomParams{T60MS: 1280, EarlyDensity: 0.64, ReverbGain: 1.02}},
			{Kind: model.EditAddPortal, Portal: &model.Opening{ID: "o34b", WallID: "w34", Center: model.Point{X: 2200, Y: 2860}, Width: 260, Active: true}},
			{Kind: model.EditMoveRoomEdge, RoomID: "r3", Edge: "bottom", DeltaMM: -80},
		},
	}
	return Bundle{Level: level, Truth: truth, Edits: edits}, nil
}

func buildTruth(level model.Level) (model.Truth, error) {
	adj, err := truthAdjacency(level)
	if err != nil {
		return model.Truth{}, err
	}
	truth := model.Truth{
		SchemaVersion: model.TruthSchema, AlgorithmVersion: model.AlgorithmVersion,
		Precision: model.Precision, SpeedOfSoundMMPS: model.SpeedOfSoundMMPS,
		Tolerance:     model.Tolerance{PathMM: 0.02, DecayMS: 0.02, EarlyEnergy: 0.000001},
		ListenerRooms: map[string]string{},
	}
	for _, l := range level.Listeners {
		room, ok := roomAtPoint(level, l.Pos)
		if !ok {
			return model.Truth{}, &errs.Error{Code: errs.ERoom, Listener: l.ID, Field: "pos", Message: "listener outside rooms"}
		}
		truth.ListenerRooms[l.ID] = room
	}
	for _, l := range level.Listeners {
		for _, s := range level.Sources {
			p, err := truthShortestPath(level, adj, s, l)
			if err != nil {
				if errs.IsCode(err, errs.EConn) {
					truth.Pairs = append(truth.Pairs, model.PairTruth{
						SourceID: s.ID, ListenerID: l.ID, Reachable: false,
						RoomPath: []string{}, DoorPath: []string{},
					})
					continue
				}
				return model.Truth{}, err
			}
			decay, energy := truthEstimate(level, p.rooms, p.length)
			truth.Pairs = append(truth.Pairs, model.PairTruth{
				SourceID: s.ID, ListenerID: l.ID, Reachable: true,
				RoomPath:    append([]string{}, p.rooms...),
				DoorPath:    append([]string{}, p.portals...),
				PathMM:      fixed.Value(p.length).Float(),
				DecayMS:     fixed.Value(decay).Float(),
				EarlyEnergy: fixed.Value(energy).Float(),
			})
		}
	}
	sort.Slice(truth.Pairs, func(i, j int) bool {
		if truth.Pairs[i].ListenerID != truth.Pairs[j].ListenerID {
			return truth.Pairs[i].ListenerID < truth.Pairs[j].ListenerID
		}
		return truth.Pairs[i].SourceID < truth.Pairs[j].SourceID
	})
	return truth, nil
}

type truthPortal struct {
	id     string
	wallID string
	rooms  [2]string
	center model.Point
}

type truthPath struct {
	rooms   []string
	portals []string
	length  fixed.Value
}

// truthAdjacency and the DFS below are the generator's independent reference
// implementation. They deliberately do not call the production Dijkstra solver.
func truthAdjacency(level model.Level) (map[string][]truthPortal, error) {
	adj := map[string][]truthPortal{}
	for _, r := range level.Rooms {
		adj[r.ID] = nil
	}
	walls := map[string]model.Wall{}
	for _, w := range level.Walls {
		walls[w.ID] = w
	}
	for _, o := range level.Openings {
		if !o.Active {
			continue
		}
		w, ok := walls[o.WallID]
		if !ok || len(w.Rooms) != 2 {
			continue
		}
		p := truthPortal{id: o.ID, wallID: o.WallID, rooms: [2]string{w.Rooms[0], w.Rooms[1]}, center: o.Center}
		adj[p.rooms[0]] = append(adj[p.rooms[0]], p)
		adj[p.rooms[1]] = append(adj[p.rooms[1]], p)
	}
	for room := range adj {
		sort.Slice(adj[room], func(i, j int) bool { return adj[room][i].id < adj[room][j].id })
	}
	return adj, nil
}

func truthShortestPath(level model.Level, adj map[string][]truthPortal, source model.Source, listener model.Listener) (truthPath, error) {
	srcRoom, ok := roomAtPoint(level, source.Pos)
	if !ok {
		return truthPath{}, &errs.Error{Code: errs.ERoom, Source: source.ID, Field: "pos", Message: "source outside rooms"}
	}
	lstRoom, ok := roomAtPoint(level, listener.Pos)
	if !ok {
		return truthPath{}, &errs.Error{Code: errs.ERoom, Listener: listener.ID, Field: "pos", Message: "listener outside rooms"}
	}
	if srcRoom == lstRoom {
		return truthPath{
			rooms: []string{srcRoom}, portals: []string{},
			length: fixed.Distance(source.Pos.X, source.Pos.Y, listener.Pos.X, listener.Pos.Y),
		}, nil
	}
	var best *truthPath
	visited := map[string]bool{srcRoom: true}
	var visit func(room string, pos model.Point, cost fixed.Value, rooms, portals []string)
	visit = func(room string, pos model.Point, cost fixed.Value, rooms, portals []string) {
		if best != nil && cost > best.length {
			return
		}
		if room == lstRoom {
			total := cost + fixed.Distance(pos.X, pos.Y, listener.Pos.X, listener.Pos.Y)
			if best == nil || total < best.length ||
				(total == best.length && truthLexLess(rooms, portals, best.rooms, best.portals)) {
				best = &truthPath{
					rooms: append([]string(nil), rooms...), portals: append([]string(nil), portals...),
					length: total,
				}
			}
			return
		}
		for _, p := range adj[room] {
			next := p.rooms[0]
			if next == room {
				next = p.rooms[1]
			}
			if visited[next] {
				continue
			}
			nextCost := cost + fixed.Distance(pos.X, pos.Y, p.center.X, p.center.Y)
			visited[next] = true
			visit(next, p.center, nextCost, append(rooms, next), append(portals, p.id))
			delete(visited, next)
		}
	}
	visit(srcRoom, source.Pos, 0, []string{srcRoom}, nil)
	if best == nil {
		return truthPath{}, &errs.Error{
			Code: errs.EConn, Source: source.ID, Listener: listener.ID, Field: "room_path",
			Message: fmt.Sprintf("truth enumeration found no portal chain from %s to %s", srcRoom, lstRoom),
		}
	}
	return *best, nil
}

func truthEstimate(level model.Level, roomPath []string, path fixed.Value) (fixed.Value, fixed.Value) {
	var t60, density, gain fixed.Value
	for _, id := range roomPath {
		r, ok := level.Room(id)
		if !ok {
			continue
		}
		t60 += fixed.FromFloat(r.Params.T60MS)
		density += fixed.FromFloat(r.Params.EarlyDensity)
		gain += fixed.FromFloat(r.Params.ReverbGain)
	}
	n := int64(len(roomPath))
	if n == 0 {
		return 0, 0
	}
	travel := fixed.DivInt(fixed.MulInt(path, 1000), model.SpeedOfSoundMMPS)
	decay := fixed.Add(travel, fixed.DivInt(t60, n))
	unit := fixed.Mul(fixed.DivInt(density, n), fixed.DivInt(gain, n))
	energy := fixed.Div(fixed.MulInt(unit, 1000), fixed.Add(fixed.FromInt(1000), path))
	return decay, energy
}

func roomAtPoint(level model.Level, p model.Point) (string, bool) {
	found := ""
	for _, r := range level.Rooms {
		if r.Rect.ContainsInclusive(p) {
			if found != "" {
				return "", false
			}
			found = r.ID
		}
	}
	return found, found != ""
}

func truthLexLess(roomsA, portalsA, roomsB, portalsB []string) bool {
	n := len(roomsA)
	if len(roomsB) < n {
		n = len(roomsB)
	}
	for i := 0; i < n; i++ {
		if roomsA[i] != roomsB[i] {
			return roomsA[i] < roomsB[i]
		}
	}
	if len(roomsA) != len(roomsB) {
		return len(roomsA) < len(roomsB)
	}
	n = len(portalsA)
	if len(portalsB) < n {
		n = len(portalsB)
	}
	for i := 0; i < n; i++ {
		if portalsA[i] != portalsB[i] {
			return portalsA[i] < portalsB[i]
		}
	}
	return len(portalsA) < len(portalsB)
}

func findWall(walls []model.Wall, id string) (model.Wall, bool) {
	for _, w := range walls {
		if w.ID == id {
			return w, true
		}
	}
	return model.Wall{}, false
}

func clampOpening(w model.Wall, o model.Opening) model.Opening {
	dx, dy := w.End.X-w.Start.X, w.End.Y-w.Start.Y
	length := fixed.Value(fixed.Distance(w.Start.X, w.Start.Y, w.End.X, w.End.Y)).Float()
	if length <= 0 {
		return o
	}
	ux, uy := dx/length, dy/length
	px, py := o.Center.X-w.Start.X, o.Center.Y-w.Start.Y
	t := px*ux + py*uy
	half := o.Width / 2
	if t < half {
		t = half
	}
	if t > length-half {
		t = length - half
	}
	o.Center = model.Point{X: round6(w.Start.X + ux*t), Y: round6(w.Start.Y + uy*t)}
	return o
}

func round6(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}

func WriteBundle(dir string, b Bundle) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "level.json"), b.Level); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), b.Truth); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "edits.json"), b.Edits)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
