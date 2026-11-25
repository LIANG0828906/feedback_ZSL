package acoustic

import "container/heap"

type PathSegment struct {
	From                 Point   `json:"from"`
	To                   Point   `json:"to"`
	LengthTicks          Fixed   `json:"lengthTicks"`
	RoundingResidualTick float64 `json:"roundingResidualTicks"`
}

type ChainSummary struct {
	SourceID             int           `json:"sourceId"`
	ListenerID           int           `json:"listenerId"`
	SourceRoomID         int           `json:"sourceRoomId"`
	ListenerRoomID       int           `json:"listenerRoomId"`
	RoomIDs              []int         `json:"roomIds"`
	OpeningIDs           []int         `json:"openingIds"`
	LengthTicks          Fixed         `json:"lengthTicks"`
	QuantizationResidual float64       `json:"quantizationResidualTicks"`
	Segments             []PathSegment `json:"segments"`
	RoomCount            int           `json:"roomCount"`
}

type openingLink struct {
	ID     int
	Center Point
	RoomA  int
	RoomB  int
	WallID int
}

type chainState struct {
	room         int
	entryOpening int
	rooms        []int
	openings     []int
	segments     []PathSegment
	length       int64
	hops         int
	serial       int64
	index        int
}

type chainQueue []*chainState

func (q chainQueue) Len() int {
	return len(q)
}

func (q chainQueue) Less(i, j int) bool {
	if q[i].hops != q[j].hops {
		return q[i].hops < q[j].hops
	}
	if q[i].length != q[j].length {
		return q[i].length < q[j].length
	}
	return q[i].serial < q[j].serial
}

func (q chainQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *chainQueue) Push(value any) {
	state := value.(*chainState)
	state.index = len(*q)
	*q = append(*q, state)
}

func (q *chainQueue) Pop() any {
	old := *q
	n := len(old)
	state := old[n-1]
	old[n-1] = nil
	state.index = -1
	*q = old[:n-1]
	return state
}

func FindChain(project *Project, sourceID, listenerID int) (ChainSummary, error) {
	if project == nil {
		return ChainSummary{}, &Error{Code: CodeInternal, Op: "chain", Message: "project is nil", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}}
	}
	source, ok := project.sourceByID(sourceID)
	if !ok {
		return ChainSummary{}, &Error{Code: CodeSchema, Op: "chain", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "source does not exist"}
	}
	listener, ok := project.listenerByID(listenerID)
	if !ok {
		return ChainSummary{}, &Error{Code: CodeSchema, Op: "chain", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "listener does not exist"}
	}
	if source.RoomID == listener.RoomID {
		length, residual := distanceTicks(source.Pos, listener.Pos)
		if length == 0 {
			return ChainSummary{}, &Error{Code: CodeGeometry, Op: "chain", RoomIDs: []int{source.RoomID}, SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "source and listener coincide"}
		}
		return ChainSummary{
			SourceID:             sourceID,
			ListenerID:           listenerID,
			SourceRoomID:         source.RoomID,
			ListenerRoomID:       listener.RoomID,
			RoomIDs:              []int{source.RoomID},
			LengthTicks:          Fixed(length),
			QuantizationResidual: residual,
			Segments: []PathSegment{{
				From:                 source.Pos,
				To:                   listener.Pos,
				LengthTicks:          Fixed(length),
				RoundingResidualTick: residual,
			}},
			RoomCount: 1,
		}, nil
	}

	linksByRoom := buildOpeningLinks(project)
	start := &chainState{
		room:   source.RoomID,
		rooms:  []int{source.RoomID},
		hops:   1,
		serial: 0,
		index:  -1,
	}
	queue := chainQueue{start}
	heap.Init(&queue)
	best := make(map[[2]int]*chainState)
	best[[2]int{source.RoomID, 0}] = start
	var serial int64 = 1
	var finished *chainState

	for queue.Len() > 0 {
		current := heap.Pop(&queue).(*chainState)
		key := [2]int{current.room, current.entryOpening}
		if candidate, ok := best[key]; !ok || candidate != current {
			continue
		}
		from := source.Pos
		if current.entryOpening != 0 {
			link := findOpeningLink(linksByRoom[current.room], current.entryOpening)
			if link == nil {
				continue
			}
			from = link.Center
		}
		for _, link := range linksByRoom[current.room] {
			nextRoom := link.RoomB
			if link.RoomB == current.room {
				nextRoom = link.RoomA
			}
			if containsInt(current.rooms, nextRoom) {
				continue
			}
			if !segmentClear(project, from, link.Center) {
				continue
			}
			distance, residual := distanceTicks(from, link.Center)
			next := &chainState{
				room:         nextRoom,
				entryOpening: link.ID,
				rooms:        appendInts(current.rooms, nextRoom),
				openings:     appendInts(current.openings, link.ID),
				segments:     appendSegments(current.segments, from, link.Center, distance, residual),
				length:       current.length + distance,
				hops:         current.hops + 1,
				serial:       serial,
				index:        -1,
			}
			serial++
			nextKey := [2]int{nextRoom, link.ID}
			if previous, ok := best[nextKey]; ok && !betterPartial(next, previous) {
				continue
			}
			best[nextKey] = next
			heap.Push(&queue, next)
			if nextRoom == listener.RoomID {
				finalDistance, finalResidual := distanceTicks(link.Center, listener.Pos)
				if finalDistance == 0 || !segmentClear(project, link.Center, listener.Pos) {
					continue
				}
				candidate := &chainState{
					room:         nextRoom,
					entryOpening: link.ID,
					rooms:        appendInts(next.rooms, []int{}...),
					openings:     appendInts(next.openings, []int{}...),
					segments:     appendSegments(next.segments, link.Center, listener.Pos, finalDistance, finalResidual),
					length:       next.length + finalDistance,
					hops:         next.hops,
					serial:       serial,
				}
				serial++
				if finished == nil || betterFinal(candidate, finished) {
					finished = candidate
				}
			}
		}
	}

	if finished == nil {
		return ChainSummary{}, &Error{Code: CodeNoChain, Op: "chain", SourceIDs: []int{sourceID}, ListenerIDs: []int{listenerID}, Message: "no door-connected wall-clear room chain exists"}
	}
	var totalResidual float64
	for _, segment := range finished.segments {
		totalResidual += segment.RoundingResidualTick
	}
	return ChainSummary{
		SourceID:             sourceID,
		ListenerID:           listenerID,
		SourceRoomID:         source.RoomID,
		ListenerRoomID:       listener.RoomID,
		RoomIDs:              finished.rooms,
		OpeningIDs:           finished.openings,
		LengthTicks:          Fixed(finished.length),
		QuantizationResidual: totalResidual,
		Segments:             finished.segments,
		RoomCount:            len(finished.rooms),
	}, nil
}

func buildOpeningLinks(project *Project) map[int][]*openingLink {
	links := make(map[int][]*openingLink)
	for _, opening := range project.Openings {
		wall, ok := project.wallByID(opening.WallID)
		if !ok {
			continue
		}
		center, ok := openingCenter(*wall, opening)
		if !ok {
			continue
		}
		link := &openingLink{
			ID:     opening.ID,
			Center: center,
			RoomA:  wall.RoomA,
			RoomB:  wall.RoomB,
			WallID: wall.ID,
		}
		links[wall.RoomA] = append(links[wall.RoomA], link)
		links[wall.RoomB] = append(links[wall.RoomB], link)
	}
	for roomID := range links {
		sortLinksByCenter(links[roomID])
	}
	return links
}

func sortLinksByCenter(links []*openingLink) {
	for i := 1; i < len(links); i++ {
		for j := i; j > 0; j-- {
			a, b := links[j-1], links[j]
			if a.ID <= b.ID {
				if a.Center == b.Center {
					break
				}
			}
			if a.ID < b.ID || (a.ID == b.ID && a.Center.Y < b.Center.Y) {
				break
			}
			links[j-1], links[j] = links[j], links[j-1]
		}
	}
}

func findOpeningLink(links []*openingLink, openingID int) *openingLink {
	for _, link := range links {
		if link.ID == openingID {
			return link
		}
	}
	return nil
}

func betterPartial(a, b *chainState) bool {
	if a.hops != b.hops {
		return a.hops < b.hops
	}
	if a.length != b.length {
		return a.length < b.length
	}
	return a.serial < b.serial
}

func betterFinal(a, b *chainState) bool {
	if a.hops != b.hops {
		return a.hops < b.hops
	}
	if a.length != b.length {
		return a.length < b.length
	}
	if len(a.rooms) != len(b.rooms) {
		return len(a.rooms) < len(b.rooms)
	}
	for i := range a.rooms {
		if a.rooms[i] != b.rooms[i] {
			return a.rooms[i] < b.rooms[i]
		}
	}
	for i := range a.openings {
		if a.openings[i] != b.openings[i] {
			return a.openings[i] < b.openings[i]
		}
	}
	return a.serial < b.serial
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func appendInts(values []int, extra ...int) []int {
	result := make([]int, len(values), len(values)+len(extra))
	copy(result, values)
	return append(result, extra...)
}

func appendSegments(values []PathSegment, from, to Point, length int64, residual float64) []PathSegment {
	result := make([]PathSegment, len(values), len(values)+1)
	copy(result, values)
	return append(result, PathSegment{From: from, To: to, LengthTicks: Fixed(length), RoundingResidualTick: residual})
}

func segmentClear(project *Project, from, to Point) bool {
	if from == to {
		return false
	}
	for _, wall := range project.Walls {
		if segmentCrossesWall(from, to, wall) && !crossingAtOpening(project, from, to, wall) {
			return false
		}
	}
	return true
}

func segmentCrossesWall(from, to Point, wall Wall) bool {
	if wall.A.X == wall.B.X {
		if from.X == to.X {
			if from.X != wall.A.X {
				return false
			}
			return rangesOverlap(from.Y, to.Y, minInt64(wall.A.Y, wall.B.Y), maxInt64(wall.A.Y, wall.B.Y))
		}
		crossX := wall.A.X
		minX, maxX := minInt64(from.X, to.X), maxInt64(from.X, to.X)
		if crossX < minX || crossX > maxX {
			return false
		}
		den := to.X - from.X
		numY := from.Y*den + (crossX-from.X)*(to.Y-from.Y)
		return rationalBetween(numY, den, minInt64(wall.A.Y, wall.B.Y), maxInt64(wall.A.Y, wall.B.Y))
	}
	if from.Y == to.Y {
		if from.Y != wall.A.Y {
			return false
		}
		return rangesOverlap(from.X, to.X, minInt64(wall.A.X, wall.B.X), maxInt64(wall.A.X, wall.B.X))
	}
	crossY := wall.A.Y
	minY, maxY := minInt64(from.Y, to.Y), maxInt64(from.Y, to.Y)
	if crossY < minY || crossY > maxY {
		return false
	}
	den := to.Y - from.Y
	numX := from.X*den + (crossY-from.Y)*(to.X-from.X)
	return rationalBetween(numX, den, minInt64(wall.A.X, wall.B.X), maxInt64(wall.A.X, wall.B.X))
}

func rangesOverlap(a0, a1, b0, b1 int64) bool {
	loA, hiA := minInt64(a0, a1), maxInt64(a0, a1)
	loB, hiB := minInt64(b0, b1), maxInt64(b0, b1)
	return hiA >= loB && hiB >= loA
}

func crossingAtOpening(project *Project, from, to Point, wall Wall) bool {
	if wall.A.X == wall.B.X {
		if from.X == to.X {
			loSeg, hiSeg := minInt64(from.Y, to.Y), maxInt64(from.Y, to.Y)
			for _, opening := range project.Openings {
				if opening.WallID != wall.ID {
					continue
				}
				door, ok := openingSegment(wall, opening)
				if !ok {
					continue
				}
				loDoor, hiDoor := minInt64(door.A.Y, door.B.Y), maxInt64(door.A.Y, door.B.Y)
				if loSeg >= loDoor && hiSeg <= hiDoor {
					return true
				}
			}
			return false
		}
		den := to.X - from.X
		numY := from.Y*den + (wall.A.X-from.X)*(to.Y-from.Y)
		for _, opening := range project.Openings {
			if opening.WallID != wall.ID {
				continue
			}
			door, ok := openingSegment(wall, opening)
			if !ok {
				continue
			}
			lo, hi := minInt64(door.A.Y, door.B.Y), maxInt64(door.A.Y, door.B.Y)
			if rationalBetween(numY, den, lo, hi) {
				return true
			}
		}
		return false
	}
	if from.Y == to.Y {
		loSeg, hiSeg := minInt64(from.X, to.X), maxInt64(from.X, to.X)
		for _, opening := range project.Openings {
			if opening.WallID != wall.ID {
				continue
			}
			door, ok := openingSegment(wall, opening)
			if !ok {
				continue
			}
			loDoor, hiDoor := minInt64(door.A.X, door.B.X), maxInt64(door.A.X, door.B.X)
			if loSeg >= loDoor && hiSeg <= hiDoor {
				return true
			}
		}
		return false
	}
	den := to.Y - from.Y
	numX := from.X*den + (wall.A.Y-from.Y)*(to.X-from.X)
	for _, opening := range project.Openings {
		if opening.WallID != wall.ID {
			continue
		}
		door, ok := openingSegment(wall, opening)
		if !ok {
			continue
		}
		lo, hi := minInt64(door.A.X, door.B.X), maxInt64(door.A.X, door.B.X)
		if rationalBetween(numX, den, lo, hi) {
			return true
		}
	}
	return false
}

func rationalBetween(num, den, low, high int64) bool {
	if den == 0 {
		return num >= low && num <= high
	}
	if den < 0 {
		num = -num
		den = -den
	}
	return num >= low*den && num <= high*den
}
