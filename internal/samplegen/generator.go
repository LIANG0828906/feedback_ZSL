package samplegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"

	levelacoustic "levelacoustic"
)

const DefaultSeed int64 = 42

type chainNode struct {
	room    int
	prior   int
	opening int
	center  levelacoustic.Point
}

type analyticPath struct {
	SourceID        int
	ReflectorID     int
	ReflectionPoint levelacoustic.Point
	PathLengthTicks int64
	IncidenceCos    int64
	SPLDB           float64
}

func Sample(seed int64) (*levelacoustic.Project, error) {
	if seed < 0 {
		return nil, &levelacoustic.Error{Code: levelacoustic.CodeSchema, Op: "sample", Message: "seed must be non-negative"}
	}
	project := &levelacoustic.Project{
		SchemaVersion:    levelacoustic.SchemaVersionCurrent,
		ParamsVersion:    3 + seed%5,
		AlgorithmVersion: levelacoustic.AlgorithmVersionCurrent,
		Rooms: []levelacoustic.Room{
			{ID: 1, Rect: levelacoustic.Rect{MinX: 0, MinY: 0, MaxX: 4000, MaxY: 3000}},
			{ID: 2, Rect: levelacoustic.Rect{MinX: 4000, MinY: 0, MaxX: 8000, MaxY: 3000}},
			{ID: 3, Rect: levelacoustic.Rect{MinX: 8000, MinY: 0, MaxX: 12000, MaxY: 3000}},
		},
		Walls: []levelacoustic.Wall{
			{ID: 101, RoomA: 1, RoomB: 2, A: levelacoustic.Point{X: 4000, Y: 0}, B: levelacoustic.Point{X: 4000, Y: 3000}},
			{ID: 102, RoomA: 2, RoomB: 3, A: levelacoustic.Point{X: 8000, Y: 0}, B: levelacoustic.Point{X: 8000, Y: 3000}},
		},
		Openings: []levelacoustic.Opening{
			{ID: 201, WallID: 101, Start: 1000, Width: 1000},
			{ID: 202, WallID: 102, Start: 1400, Width: 1400},
		},
		Sources: []levelacoustic.Source{
			{ID: 11, RoomID: 1, Pos: levelacoustic.Point{X: 800, Y: 800}, LevelDB: 80},
			{ID: 12, RoomID: 2, Pos: levelacoustic.Point{X: 5000, Y: 2200}, LevelDB: 82},
			{ID: 13, RoomID: 3, Pos: levelacoustic.Point{X: 9000, Y: 2000}, LevelDB: 78},
		},
		Reflectors: []levelacoustic.Reflector{
			{ID: 31, RoomID: 1, A: levelacoustic.Point{X: 1800, Y: 800}, B: levelacoustic.Point{X: 1800, Y: 2600}, NX: 1000, NY: 0, Absorption: 0.05},
			{ID: 32, RoomID: 1, A: levelacoustic.Point{X: 400, Y: 1000}, B: levelacoustic.Point{X: 2600, Y: 1000}, NX: 0, NY: 1000, Absorption: 0.08},
			{ID: 33, RoomID: 2, A: levelacoustic.Point{X: 7100, Y: 700}, B: levelacoustic.Point{X: 7100, Y: 2900}, NX: 1000, NY: 0, Absorption: 0.06},
			{ID: 34, RoomID: 2, A: levelacoustic.Point{X: 4500, Y: 1600}, B: levelacoustic.Point{X: 7500, Y: 1600}, NX: 0, NY: 1000, Absorption: 0.04},
			{ID: 35, RoomID: 3, A: levelacoustic.Point{X: 10600, Y: 1000}, B: levelacoustic.Point{X: 10600, Y: 2600}, NX: 1000, NY: 0, Absorption: 0.07},
			{ID: 36, RoomID: 3, A: levelacoustic.Point{X: 8500, Y: 1800}, B: levelacoustic.Point{X: 11500, Y: 1800}, NX: 0, NY: 1000, Absorption: 0.03},
		},
		Listeners: []levelacoustic.Listener{
			{ID: 21, RoomID: 1, Pos: levelacoustic.Point{X: 1200, Y: 2200}},
			{ID: 22, RoomID: 2, Pos: levelacoustic.Point{X: 6500, Y: 2500}},
			{ID: 23, RoomID: 3, Pos: levelacoustic.Point{X: 10000, Y: 2200}},
		},
		Tolerance: levelacoustic.Tolerance{
			CoordinateTicks: 2,
			PathTicks:       4,
			IncidenceCos:    1000,
			SPLDB:           0.05,
		},
	}
	truth, err := buildTruth(project)
	if err != nil {
		return nil, err
	}
	project.Truth = truth
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return project, nil
}

func Marshal(project *levelacoustic.Project) ([]byte, error) {
	return json.MarshalIndent(project, "", "  ")
}

func WriteJSON(path string, project *levelacoustic.Project) error {
	data, err := Marshal(project)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func buildTruth(project *levelacoustic.Project) ([]levelacoustic.ListenerTruth, error) {
	sources := append([]levelacoustic.Source(nil), project.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	listeners := append([]levelacoustic.Listener(nil), project.Listeners...)
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	reflectors := append([]levelacoustic.Reflector(nil), project.Reflectors...)
	sort.Slice(reflectors, func(i, j int) bool { return reflectors[i].ID < reflectors[j].ID })

	truth := make([]levelacoustic.ListenerTruth, 0, len(listeners))
	for _, listener := range listeners {
		listenerTruth := levelacoustic.ListenerTruth{ListenerID: listener.ID, RoomID: listener.RoomID}
		for _, source := range sources {
			roomIDs, openingIDs := analyticChain(project, source.ID, listener.ID)
			chainLength := analyticChainLength(project, source.ID, listener.ID, roomIDs, openingIDs)
			sourceTruth := levelacoustic.TruthSource{
				SourceID:         source.ID,
				ChainRoomIDs:     roomIDs,
				ChainOpeningIDs:  openingIDs,
				ChainLengthTicks: levelacoustic.Fixed(chainLength),
				Paths:            []levelacoustic.TruthPath{},
			}
			if len(roomIDs) == 0 {
				listenerTruth.Sources = append(listenerTruth.Sources, sourceTruth)
				continue
			}
			for _, reflector := range reflectors {
				if reflector.RoomID != listener.RoomID {
					continue
				}
				path, ok := analyticReflection(project, source, listener, reflector)
				if !ok {
					continue
				}
				sourceTruth.Paths = append(sourceTruth.Paths, levelacoustic.TruthPath{
					SourceID:        source.ID,
					ReflectorID:     reflector.ID,
					ReflectionPoint: path.ReflectionPoint,
					PathLengthTicks: levelacoustic.Fixed(path.PathLengthTicks),
					IncidenceCos:    path.IncidenceCos,
					SPLDB:           path.SPLDB,
				})
			}
			sourceTruth.ValidPathCount = len(sourceTruth.Paths)
			listenerTruth.ValidPathCount += sourceTruth.ValidPathCount
			listenerTruth.Sources = append(listenerTruth.Sources, sourceTruth)
		}
		truth = append(truth, listenerTruth)
	}
	return truth, nil
}

func analyticReflection(project *levelacoustic.Project, source levelacoustic.Source, listener levelacoustic.Listener, reflector levelacoustic.Reflector) (analyticPath, bool) {
	if reflector.RoomID != listener.RoomID {
		return analyticPath{}, false
	}
	line := levelacoustic.Point{X: reflector.B.X - reflector.A.X, Y: reflector.B.Y - reflector.A.Y}
	if line == (levelacoustic.Point{}) {
		return analyticPath{}, false
	}
	d := reflector.NX*(source.Pos.X-reflector.A.X) + reflector.NY*(source.Pos.Y-reflector.A.Y)
	e := reflector.NX*(listener.Pos.X-reflector.A.X) + reflector.NY*(listener.Pos.Y-reflector.A.Y)
	if d == 0 || e == 0 || (d > 0) != (e > 0) {
		return analyticPath{}, false
	}
	point, ok := analyticMirrorPoint(source.Pos, listener.Pos, reflector)
	if !ok {
		return analyticPath{}, false
	}
	if !pointOnSegment(point, reflector.A, reflector.B, 2) {
		return analyticPath{}, false
	}
	room := findRoom(project, listener.RoomID)
	if room == nil || !containsClosed(room.Rect, point) {
		return analyticPath{}, false
	}
	if !analyticSegmentClear(project, source.Pos, point) || !analyticSegmentClear(project, point, listener.Pos) {
		return analyticPath{}, false
	}
	d1 := roundedDistance(source.Pos, point)
	d2 := roundedDistance(point, listener.Pos)
	if d1 <= 0 || d2 <= 0 {
		return analyticPath{}, false
	}
	length := d1 + d2
	cosine := analyticCosine(source.Pos, point, reflector)
	alpha := math.Round(reflector.Absorption*1_000_000) / 1_000_000
	world := float64(length) / 1000
	spl := source.LevelDB - 20*math.Log10(world) - 8.685889638*alpha*world
	return analyticPath{
		SourceID:        source.ID,
		ReflectorID:     reflector.ID,
		ReflectionPoint: point,
		PathLengthTicks: length,
		IncidenceCos:    cosine,
		SPLDB:           spl,
	}, true
}

func analyticMirrorPoint(source, listener levelacoustic.Point, reflector levelacoustic.Reflector) (levelacoustic.Point, bool) {
	if reflector.A.X == reflector.B.X {
		c := reflector.A.X
		imageX := 2*c - source.X
		dx := listener.X - imageX
		if dx == 0 {
			return levelacoustic.Point{}, false
		}
		t := float64(c-imageX) / float64(dx)
		y := float64(source.Y) + t*float64(listener.Y-source.Y)
		return levelacoustic.Point{X: c, Y: int64(math.Round(y))}, true
	}
	if reflector.A.Y == reflector.B.Y {
		c := reflector.A.Y
		imageY := 2*c - source.Y
		dy := listener.Y - imageY
		if dy == 0 {
			return levelacoustic.Point{}, false
		}
		t := float64(c-imageY) / float64(dy)
		x := float64(source.X) + t*float64(listener.X-source.X)
		return levelacoustic.Point{X: int64(math.Round(x)), Y: c}, true
	}
	nx := float64(reflector.NX) / 1000
	ny := float64(reflector.NY) / 1000
	d := nx*float64(source.X-reflector.A.X) + ny*float64(source.Y-reflector.A.Y)
	ix := float64(source.X) - 2*d*nx
	iy := float64(source.Y) - 2*d*ny
	dt := nx*float64(listener.X-reflector.A.X) + ny*float64(listener.Y-reflector.A.Y)
	den := d + dt
	if den == 0 {
		return levelacoustic.Point{}, false
	}
	t := d / den
	return levelacoustic.Point{
		X: int64(math.Round(ix + t*float64(listener.X) - t*ix)),
		Y: int64(math.Round(iy + t*float64(listener.Y) - t*iy)),
	}, true
}

func analyticChain(project *levelacoustic.Project, sourceID, listenerID int) ([]int, []int) {
	source := findSource(project, sourceID)
	listener := findListener(project, listenerID)
	if source == nil || listener == nil {
		return nil, nil
	}
	if source.RoomID == listener.RoomID {
		return []int{source.RoomID}, nil
	}
	type node struct {
		room    int
		prior   int
		opening int
		roomIDs []int
		doors   []int
	}
	queue := []node{{room: source.RoomID, prior: -1, roomIDs: []int{source.RoomID}}}
	visited := map[int]bool{source.RoomID: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, opening := range project.Openings {
			wall := findWall(project, opening.WallID)
			if wall == nil {
				continue
			}
			next := wall.RoomA
			if next == current.room {
				next = wall.RoomB
			} else if wall.RoomB != current.room {
				continue
			}
			if visited[next] {
				continue
			}
			rooms := append(append([]int(nil), current.roomIDs...), next)
			doors := append(append([]int(nil), current.doors...), opening.ID)
			if next == listener.RoomID {
				return rooms, doors
			}
			visited[next] = true
			queue = append(queue, node{room: next, opening: opening.ID, roomIDs: rooms, doors: doors})
		}
	}
	return nil, nil
}

func analyticChainLength(project *levelacoustic.Project, sourceID, listenerID int, roomIDs, openingIDs []int) int64 {
	if len(roomIDs) == 0 {
		return 0
	}
	source := findSource(project, sourceID)
	listener := findListener(project, listenerID)
	if source == nil || listener == nil {
		return 0
	}
	from := source.Pos
	var total int64
	for _, openingID := range openingIDs {
		opening := findOpening(project, openingID)
		if opening == nil {
			return 0
		}
		wall := findWall(project, opening.WallID)
		center := analyticDoorCenter(*wall, *opening)
		if !analyticSegmentClear(project, from, center) {
			return 0
		}
		total += roundedDistance(from, center)
		from = center
	}
	if !analyticSegmentClear(project, from, listener.Pos) {
		return 0
	}
	return total + roundedDistance(from, listener.Pos)
}

func analyticSegmentClear(project *levelacoustic.Project, from, to levelacoustic.Point) bool {
	if from == to {
		return false
	}
	for _, wall := range project.Walls {
		if crossesWall(from, to, wall) && !crossesAtDoor(project, from, to, wall) {
			return false
		}
	}
	return true
}

func crossesWall(from, to levelacoustic.Point, wall levelacoustic.Wall) bool {
	if wall.A.X == wall.B.X {
		if from.X == to.X {
			if from.X != wall.A.X {
				return false
			}
			return intervalsOverlap(from.Y, to.Y, wall.A.Y, wall.B.Y)
		}
		if wall.A.X < min(from.X, to.X) || wall.A.X > max(from.X, to.X) {
			return false
		}
		t := float64(wall.A.X-from.X) / float64(to.X-from.X)
		y := float64(from.Y) + t*float64(to.Y-from.Y)
		return y >= float64(min(wall.A.Y, wall.B.Y)) && y <= float64(max(wall.A.Y, wall.B.Y))
	}
	if from.Y == to.Y {
		if from.Y != wall.A.Y {
			return false
		}
		return intervalsOverlap(from.X, to.X, wall.A.X, wall.B.X)
	}
	if wall.A.Y < min(from.Y, to.Y) || wall.A.Y > max(from.Y, to.Y) {
		return false
	}
	t := float64(wall.A.Y-from.Y) / float64(to.Y-from.Y)
	x := float64(from.X) + t*float64(to.X-from.X)
	return x >= float64(min(wall.A.X, wall.B.X)) && x <= float64(max(wall.A.X, wall.B.X))
}

func crossesAtDoor(project *levelacoustic.Project, from, to levelacoustic.Point, wall levelacoustic.Wall) bool {
	var crossX, crossY float64
	if wall.A.X == wall.B.X {
		crossX = float64(wall.A.X)
		if from.X == to.X {
			crossY = float64(from.Y)
		} else {
			t := float64(wall.A.X-from.X) / float64(to.X-from.X)
			crossY = float64(from.Y) + t*float64(to.Y-from.Y)
		}
	} else {
		crossY = float64(wall.A.Y)
		if from.Y == to.Y {
			crossX = float64(from.X)
		} else {
			t := float64(wall.A.Y-from.Y) / float64(to.Y-from.Y)
			crossX = float64(from.X) + t*float64(to.X-from.X)
		}
	}
	for _, opening := range project.Openings {
		if opening.WallID != wall.ID {
			continue
		}
		door := analyticDoorSegment(wall, opening)
		if wall.A.X == wall.B.X {
			lo, hi := float64(min(door.A.Y, door.B.Y)), float64(max(door.A.Y, door.B.Y))
			if crossX == float64(wall.A.X) && crossY >= lo && crossY <= hi {
				return true
			}
		} else {
			lo, hi := float64(min(door.A.X, door.B.X)), float64(max(door.A.X, door.B.X))
			if crossY == float64(wall.A.Y) && crossX >= lo && crossX <= hi {
				return true
			}
		}
	}
	return false
}

func analyticDoorSegment(wall levelacoustic.Wall, opening levelacoustic.Opening) struct{ A, B levelacoustic.Point } {
	if wall.A.X == wall.B.X {
		lo := min(wall.A.Y, wall.B.Y)
		return struct{ A, B levelacoustic.Point }{
			A: levelacoustic.Point{X: wall.A.X, Y: lo + opening.Start},
			B: levelacoustic.Point{X: wall.A.X, Y: lo + opening.Start + opening.Width},
		}
	}
	lo := min(wall.A.X, wall.B.X)
	return struct{ A, B levelacoustic.Point }{
		A: levelacoustic.Point{X: lo + opening.Start, Y: wall.A.Y},
		B: levelacoustic.Point{X: lo + opening.Start + opening.Width, Y: wall.A.Y},
	}
}

func analyticDoorCenter(wall levelacoustic.Wall, opening levelacoustic.Opening) levelacoustic.Point {
	door := analyticDoorSegment(wall, opening)
	return levelacoustic.Point{X: (door.A.X + door.B.X) / 2, Y: (door.A.Y + door.B.Y) / 2}
}

func roundedDistance(a, b levelacoustic.Point) int64 {
	return int64(math.Round(math.Hypot(float64(a.X-b.X), float64(a.Y-b.Y))))
}

func analyticCosine(source, reflection levelacoustic.Point, reflector levelacoustic.Reflector) int64 {
	vx := float64(source.X - reflection.X)
	vy := float64(source.Y - reflection.Y)
	length := math.Hypot(vx, vy)
	normalLength := math.Hypot(float64(reflector.NX), float64(reflector.NY))
	if length == 0 || normalLength == 0 {
		return 0
	}
	cosine := math.Abs(vx*float64(reflector.NX)+vy*float64(reflector.NY)) / (length * normalLength)
	return int64(math.Round(math.Min(1, cosine) * 1000))
}

func pointOnSegment(point, a, b levelacoustic.Point, tolerance float64) bool {
	abx := float64(b.X - a.X)
	aby := float64(b.Y - a.Y)
	apx := float64(point.X - a.X)
	apy := float64(point.Y - a.Y)
	lengthSquared := abx*abx + aby*aby
	if lengthSquared == 0 {
		return false
	}
	projection := apx*abx + apy*aby
	if projection < 0 || projection > lengthSquared {
		return false
	}
	perp := math.Abs(apx*aby-apy*abx) / math.Sqrt(lengthSquared)
	return perp <= tolerance
}

func containsClosed(rect levelacoustic.Rect, point levelacoustic.Point) bool {
	return point.X >= rect.MinX && point.X <= rect.MaxX && point.Y >= rect.MinY && point.Y <= rect.MaxY
}

func findRoom(project *levelacoustic.Project, id int) *levelacoustic.Room {
	for i := range project.Rooms {
		if project.Rooms[i].ID == id {
			return &project.Rooms[i]
		}
	}
	return nil
}

func findWall(project *levelacoustic.Project, id int) *levelacoustic.Wall {
	for i := range project.Walls {
		if project.Walls[i].ID == id {
			return &project.Walls[i]
		}
	}
	return nil
}

func findOpening(project *levelacoustic.Project, id int) *levelacoustic.Opening {
	for i := range project.Openings {
		if project.Openings[i].ID == id {
			return &project.Openings[i]
		}
	}
	return nil
}

func findSource(project *levelacoustic.Project, id int) *levelacoustic.Source {
	for i := range project.Sources {
		if project.Sources[i].ID == id {
			return &project.Sources[i]
		}
	}
	return nil
}

func findListener(project *levelacoustic.Project, id int) *levelacoustic.Listener {
	for i := range project.Listeners {
		if project.Listeners[i].ID == id {
			return &project.Listeners[i]
		}
	}
	return nil
}

func intervalsOverlap(a0, a1, b0, b1 int64) bool {
	loA, hiA := min(a0, a1), max(a0, a1)
	loB, hiB := min(b0, b1), max(b0, b1)
	return hiA >= loB && hiB >= loA
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func Decode(data []byte) (*levelacoustic.Project, error) {
	project, err := levelacoustic.LoadProject(data)
	if err != nil {
		return nil, err
	}
	return project, nil
}

func Pretty(project *levelacoustic.Project) ([]byte, error) {
	data, err := Marshal(project)
	if err != nil {
		return nil, err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, data, "", "  "); err != nil {
		return nil, fmt.Errorf("indent sample: %w", err)
	}
	return indented.Bytes(), nil
}
