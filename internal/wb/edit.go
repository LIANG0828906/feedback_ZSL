package wb

import (
	"context"
	"fmt"
	"sort"
)

type AddDoorRequest struct {
	ID     string `json:"id"`
	WallID string `json:"wallId"`
	Kind   string `json:"kind"`
	Center int    `json:"center"`
	Width  int    `json:"width"`
	RoomA  string `json:"roomA,omitempty"`
	RoomB  string `json:"roomB,omitempty"`
}

type BoundaryRequest struct {
	WallID string `json:"wallId"`
	Axis   string `json:"axis,omitempty"`
	Value  int    `json:"value"`
}

type ParamUpdateRequest struct {
	RoomID string     `json:"roomId"`
	Params RoomParams `json:"params"`
}

func (e *Engine) DragBoundary(ctx context.Context, req BoundaryRequest) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	old := e.sample
	anchor, ok := FindWall(old, req.WallID)
	if !ok {
		return nil, NewAPIError(CodeSchema, req.WallID, "wallId", "unknown wall")
	}
	vertical := anchor.Vertical()
	oldValue := anchor.X1
	if !vertical {
		oldValue = anchor.Y1
	}
	if req.Value == oldValue {
		return e.noOpReportLocked("boundary-unchanged"), nil
	}

	next := CloneSample(old)
	movingWalls := map[string]bool{}
	for _, w := range next.Walls {
		if vertical && w.Vertical() && w.X1 == oldValue {
			movingWalls[w.ID] = true
		}
		if !vertical && w.Horizontal() && w.Y1 == oldValue {
			movingWalls[w.ID] = true
		}
	}
	for i := range next.Walls {
		w := &next.Walls[i]
		if movingWalls[w.ID] {
			if vertical {
				w.X1, w.X2 = req.Value, req.Value
			} else {
				w.Y1, w.Y2 = req.Value, req.Value
			}
			continue
		}
		if vertical && w.Horizontal() {
			if w.X1 == oldValue {
				w.X1 = req.Value
			}
			if w.X2 == oldValue {
				w.X2 = req.Value
			}
		}
		if !vertical && w.Vertical() {
			if w.Y1 == oldValue {
				w.Y1 = req.Value
			}
			if w.Y2 == oldValue {
				w.Y2 = req.Value
			}
		}
	}
	for i := range next.Openings {
		o := &next.Openings[i]
		if !movingWalls[o.WallID] {
			continue
		}
		if vertical {
			o.X1, o.X2 = req.Value, req.Value
		} else {
			o.Y1, o.Y2 = req.Value, req.Value
		}
	}
	for i := range next.Rooms {
		r := &next.Rooms[i].Rect
		if vertical {
			if r.X1 == oldValue {
				r.X1 = req.Value
			}
			if r.X2 == oldValue {
				r.X2 = req.Value
			}
		} else {
			if r.Y1 == oldValue {
				r.Y1 = req.Value
			}
			if r.Y2 == oldValue {
				r.Y2 = req.Value
			}
		}
	}
	if apiErr := ValidateSample(next); apiErr != nil {
		return nil, apiErr
	}

	plan, apiErr := e.planMutationLocked(old, next, "boundary-dragged:"+req.WallID, req.WallID, "")
	if apiErr != nil {
		return nil, apiErr
	}
	e.sample = next
	e.revision++
	return e.recomputeLocked(ctx, plan)
}

func (e *Engine) UpdateRoomParams(ctx context.Context, req ParamUpdateRequest) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	room, ok := FindRoom(e.sample, req.RoomID)
	if !ok {
		return nil, NewAPIError(CodeSchema, req.RoomID, "roomId", "unknown room")
	}
	if apiErr := ValidateRoomParams(req.RoomID, req.Params); apiErr != nil {
		return nil, apiErr
	}
	if room.Params == req.Params {
		return e.noOpReportLocked("params-unchanged"), nil
	}

	next := CloneSample(e.sample)
	for i := range next.Rooms {
		if next.Rooms[i].ID == req.RoomID {
			next.Rooms[i].Params = req.Params
		}
	}
	if apiErr := ValidateSample(next); apiErr != nil {
		return nil, apiErr
	}
	plan, apiErr := e.planMutationLocked(e.sample, next, "reverb-param-changed:"+req.RoomID, req.RoomID, req.RoomID)
	if apiErr != nil {
		return nil, apiErr
	}
	e.sample = next
	e.revision++
	return e.recomputeLocked(ctx, plan)
}

func (e *Engine) UpdatePrecision(ctx context.Context, precision PrecisionSpec) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if apiErr := ValidatePrecision(precision); apiErr != nil {
		return nil, apiErr
	}
	next := CloneSample(e.sample)
	next.Precision = precision
	plan := &recomputePlan{
		reason:          "precision-changed",
		dirtyRooms:      map[string]string{},
		targetListeners: []string{},
	}
	for _, room := range next.Rooms {
		plan.dirtyRooms[room.ID] = "precision-parameters-changed"
	}
	for _, listener := range next.Listeners {
		plan.targetListeners = append(plan.targetListeners, listener.ID)
	}
	plan.items = e.allStatusItemsLocked(next, plan.dirtyRooms, plan.targetListeners, nil)
	e.sample = next
	e.sched = NewScheduler(precision)
	e.revision++
	return e.recomputeLocked(ctx, plan)
}

func (e *Engine) AddDoor(ctx context.Context, req AddDoorRequest) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	wall, ok := FindWall(e.sample, req.WallID)
	if !ok {
		return nil, NewAPIError(CodeSchema, req.WallID, "wallId", "unknown wall")
	}
	if req.Kind == "" {
		req.Kind = "door"
	}
	if req.Kind != "door" && req.Kind != "opening" {
		return nil, NewAPIError(CodeSchema, req.ID, "kind", "kind must be door or opening")
	}
	width := req.Width
	if width == 0 {
		width = 2
	}
	if width <= 0 {
		return nil, NewAPIError(CodeGeom, req.ID, "width", "opening width must be positive")
	}
	center := req.Center
	if center == 0 {
		center = freeOpeningCenter(e.sample, wall, width)
	}
	id := req.ID
	if id == "" {
		id = fmt.Sprintf("D_EDIT_%d", e.revision+1)
	}
	if _, exists := FindOpening(e.sample, id); exists {
		return nil, NewAPIError(CodeSchema, id, "id", "opening id already exists")
	}
	opening := Opening{ID: id, WallID: wall.ID, Kind: req.Kind, RoomA: req.RoomA, RoomB: req.RoomB}
	if opening.RoomA == "" {
		opening.RoomA = wall.RoomA
	}
	if opening.RoomB == "" {
		opening.RoomB = wall.RoomB
	}
	start := center - width/2
	if wall.Horizontal() {
		opening.Y1, opening.Y2 = wall.Y1, wall.Y1
		opening.X1, opening.X2 = start, start+width
	} else {
		opening.X1, opening.X2 = wall.X1, wall.X1
		opening.Y1, opening.Y2 = start, start+width
	}

	next := CloneSample(e.sample)
	next.Openings = append(next.Openings, opening)
	if apiErr := ValidateSample(next); apiErr != nil {
		return nil, apiErr
	}
	plan, apiErr := e.planMutationLocked(e.sample, next, "door-added:"+id, id, "")
	if apiErr != nil {
		return nil, apiErr
	}
	e.sample = next
	e.revision++
	return e.recomputeLocked(ctx, plan)
}

func freeOpeningCenter(s *Sample, wall Wall, width int) int {
	lo, hi := minMaxInt(wall.X1, wall.X2)
	if wall.Vertical() {
		lo, hi = minMaxInt(wall.Y1, wall.Y2)
	}
	mid := (lo + hi) / 2
	for delta := 0; delta <= hi-lo; delta++ {
		for _, center := range []int{mid - delta, mid + delta} {
			start := center - width/2
			end := start + width
			if start < lo || end > hi {
				continue
			}
			conflict := false
			for _, o := range s.Openings {
				if o.WallID != wall.ID {
					continue
				}
				olo, ohi := openingInterval(o)
				if maxInt(start, olo) < minInt(end, ohi) {
					conflict = true
					break
				}
			}
			if !conflict {
				return center
			}
		}
	}
	return mid
}

func (e *Engine) DeleteDoor(ctx context.Context, doorID string) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := FindOpening(e.sample, doorID); !ok {
		return nil, NewAPIError(CodeSchema, doorID, "doorId", "unknown opening")
	}
	next := CloneSample(e.sample)
	kept := next.Openings[:0]
	for _, o := range next.Openings {
		if o.ID != doorID {
			kept = append(kept, o)
		}
	}
	next.Openings = kept
	if apiErr := ValidateSample(next); apiErr != nil {
		return nil, apiErr
	}
	plan, apiErr := e.planMutationLocked(e.sample, next, "door-deleted:"+doorID, doorID, "")
	if apiErr != nil {
		return nil, apiErr
	}
	e.sample = next
	e.revision++
	return e.recomputeLocked(ctx, plan)
}

func FindWall(s *Sample, id string) (Wall, bool) {
	for _, w := range s.Walls {
		if w.ID == id {
			return w, true
		}
	}
	return Wall{}, false
}

func FindOpening(s *Sample, id string) (Opening, bool) {
	for _, o := range s.Openings {
		if o.ID == id {
			return o, true
		}
	}
	return Opening{}, false
}

func (e *Engine) noOpReportLocked(reason string) *InvalidationReport {
	items := []InvalidationItem{}
	for _, room := range e.sortedRoomsLocked() {
		items = append(items, InvalidationItem{
			Kind: "room", ID: room.ID, RoomID: room.ID, Reason: reason,
			Status: "reused-persisted-cache", Reused: true,
		})
	}
	for _, listener := range e.sample.Listeners {
		items = append(items, InvalidationItem{
			Kind: "listener", ID: listener.ID, RoomID: e.results[listener.ID].RoomID,
			Reason: reason, Status: "reused-persisted-cache", Reused: true,
		})
	}
	deps := e.buildDependenciesLocked()
	topo, _ := TopologicalOrder(deps)
	return &InvalidationReport{Reason: reason, Items: items, TopoOrder: topo}
}

func (e *Engine) planMutationLocked(old, next *Sample, reason, mutationID, forceAudioRoom string) (*recomputePlan, *APIError) {
	changedRooms := map[string]string{}
	changedOpenings := map[string]string{}

	oldRoom := map[string]Room{}
	for _, room := range old.Rooms {
		oldRoom[room.ID] = room
	}
	for _, room := range next.Rooms {
		before := oldRoom[room.ID]
		if before.Rect != room.Rect {
			changedRooms[room.ID] = "boundary-geometry-changed"
		}
		if before.Params != room.Params {
			changedRooms[room.ID] = "reverb-param-changed"
		}
	}

	oldOpening := map[string]Opening{}
	for _, o := range old.Openings {
		oldOpening[o.ID] = o
	}
	newOpening := map[string]Opening{}
	for _, o := range next.Openings {
		newOpening[o.ID] = o
	}
	for id, o := range newOpening {
		before, existed := oldOpening[id]
		if !existed {
			changedOpenings[id] = "opening-added"
			e.addOpeningRooms(changedRooms, o, "connectivity-edge-added")
			continue
		}
		if before != o {
			changedOpenings[id] = "opening-changed"
			e.addOpeningRooms(changedRooms, o, "door-snapshot-changed")
		}
	}
	for id, o := range oldOpening {
		if _, exists := newOpening[id]; !exists {
			changedOpenings[id] = "opening-deleted"
			e.addOpeningRooms(changedRooms, o, "connectivity-edge-deleted")
		}
	}

	oldGraph := BuildGraph(old)
	newGraph := BuildGraph(next)
	for _, room := range next.Rooms {
		if !sameStringSet(neighborIDs(oldGraph, room.ID), neighborIDs(newGraph, room.ID)) {
			if _, exists := changedRooms[room.ID]; !exists {
				changedRooms[room.ID] = "connectivity-neighbor-set-changed"
			}
		}
	}

	candidateListeners := map[string]string{}
	addCandidate := func(listenerID, why string) {
		if _, exists := candidateListeners[listenerID]; !exists {
			candidateListeners[listenerID] = why
		}
	}

	if len(e.results) == 0 {
		for _, l := range next.Listeners {
			addCandidate(l.ID, "empty-cache")
		}
		for _, room := range next.Rooms {
			if _, ok := changedRooms[room.ID]; !ok {
				changedRooms[room.ID] = "empty-cache"
			}
		}
	} else {
		for listenerID, result := range e.results {
			for _, pair := range result.Pairs {
				depHit := false
				for _, roomID := range pair.Path.RoomChain {
					if _, hit := changedRooms[roomID]; hit {
						depHit = true
					}
				}
				for _, doorID := range pair.Path.DoorChain {
					if _, hit := changedOpenings[doorID]; hit {
						depHit = true
					}
				}
				if depHit {
					addCandidate(listenerID, "dependency-path-hit")
				}
			}
			if _, hit := changedRooms[result.RoomID]; hit {
				addCandidate(listenerID, "listener-room-dependency-hit")
			}
		}
		for _, l := range next.Listeners {
			roomID, apiErr := RoomAtPoint(next, Point{X: l.X, Y: l.Y}, l.ID, "listener")
			if apiErr != nil {
				return nil, apiErr
			}
			if _, hit := changedRooms[roomID]; hit {
				addCandidate(l.ID, "listener-located-in-changed-room")
			}
		}
		if reason == "door-added:"+mutationID {
			if opening, ok := newOpening[mutationID]; ok {
				reachable := reachableRooms(newGraph, []string{opening.RoomA, opening.RoomB})
				for _, l := range next.Listeners {
					roomID, _ := RoomAtPoint(next, Point{X: l.X, Y: l.Y}, l.ID, "listener")
					if reachable[roomID] {
						addCandidate(l.ID, "new-door-reachable-component")
					}
				}
			}
		}
		if forceAudioRoom != "" {
			for _, l := range next.Listeners {
				roomID, _ := RoomAtPoint(next, Point{X: l.X, Y: l.Y}, l.ID, "listener")
				if roomID == forceAudioRoom {
					addCandidate(l.ID, "room-reverb-param-changed")
				}
			}
		}
	}

	targetListeners := []string{}
	reusedListeners := []string{}
	newGraphForCheck := newGraph
	for listenerID := range candidateListeners {
		changed, apiErr := e.listenerResultActuallyChanged(old, next, newGraphForCheck, listenerID, forceAudioRoom)
		if apiErr != nil {
			return nil, apiErr
		}
		if changed {
			targetListeners = append(targetListeners, listenerID)
		} else {
			reusedListeners = append(reusedListeners, listenerID)
		}
	}
	sort.Strings(targetListeners)
	sort.Strings(reusedListeners)

	for _, listenerID := range targetListeners {
		l, _ := FindListener(next, listenerID)
		roomID, _ := RoomAtPoint(next, Point{X: l.X, Y: l.Y}, l.ID, "listener")
		if _, ok := changedRooms[roomID]; !ok {
			changedRooms[roomID] = "downstream-listener-result-changed"
		}
	}

	items := e.allStatusItemsLocked(next, changedRooms, targetListeners, candidateListeners)
	plan := &recomputePlan{
		reason:          reason,
		dirtyRooms:      changedRooms,
		targetListeners: targetListeners,
		items:           items,
	}
	return plan, nil
}

func (e *Engine) addOpeningRooms(rooms map[string]string, o Opening, reason string) {
	for _, roomID := range []string{o.RoomA, o.RoomB} {
		if roomID == "" {
			continue
		}
		if _, exists := rooms[roomID]; !exists {
			rooms[roomID] = reason
		}
	}
}

func neighborIDs(g *Graph, roomID string) []string {
	if g == nil {
		return nil
	}
	edges := g.Adj[roomID]
	ids := make([]string, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, edge.Other)
	}
	sort.Strings(ids)
	return ids
}

func reachableRooms(g *Graph, starts []string) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), starts...)
	for _, id := range queue {
		seen[id] = true
	}
	for len(queue) > 0 {
		roomID := queue[0]
		queue = queue[1:]
		for _, edge := range g.Adj[roomID] {
			if !seen[edge.Other] {
				seen[edge.Other] = true
				queue = append(queue, edge.Other)
			}
		}
	}
	return seen
}

func (e *Engine) listenerResultActuallyChanged(old, next *Sample, g *Graph, listenerID, forceAudioRoom string) (bool, *APIError) {
	oldResult, hadOld := e.results[listenerID]
	if !hadOld {
		return true, nil
	}
	listener, ok := FindListener(next, listenerID)
	if !ok {
		return false, NewAPIError(CodeSchema, listenerID, "listenerId", "unknown listener")
	}
	newRoom, apiErr := RoomAtPoint(next, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
	if apiErr != nil {
		return false, apiErr
	}
	if oldResult.RoomID != newRoom {
		return true, nil
	}
	if forceAudioRoom == newRoom {
		return true, nil
	}

	oldBySource := map[string]PairResult{}
	for _, pair := range oldResult.Pairs {
		oldBySource[pair.Path.SourceID] = pair
	}
	for _, source := range next.Sources {
		newPath, pathErr := SolvePairPath(next, g, source.ID, listenerID)
		oldPair := oldBySource[source.ID]
		if pathErr != nil {
			if oldPair.Error == nil || oldPair.Error.Code != pathErr.Code || oldPair.Error.Object != pathErr.Object {
				return true, nil
			}
			continue
		}
		if oldPair.Error != nil {
			return true, nil
		}
		if pathFingerprint(*newPath) != pathFingerprint(oldPair.Path) {
			return true, nil
		}
	}
	return false, nil
}

func pathFingerprint(p PathResult) string {
	return fmt.Sprintf("%s|%v|%v|%d|%d|%d",
		p.ListenerRoom, p.RoomChain, p.DoorChain, p.Length, p.OcclusionCount, p.OcclusionDB)
}

func (e *Engine) allStatusItemsLocked(next *Sample, changedRooms map[string]string, targetListeners []string, candidateListeners map[string]string) []InvalidationItem {
	items := []InvalidationItem{}
	for _, room := range next.Rooms {
		reason := changedRooms[room.ID]
		if reason == "" {
			reason = "dependency-unchanged"
		}
		deps := []string{"param:" + room.ID, "geom:" + room.ID, "door dependencies"}
		items = append(items, InvalidationItem{
			Kind: "room", ID: room.ID, RoomID: room.ID, Reason: reason,
			Status: "pending", Reused: false, DependsOn: deps,
		})
	}
	targetSet := map[string]bool{}
	for _, id := range targetListeners {
		targetSet[id] = true
	}
	for _, listener := range next.Listeners {
		roomID, _ := RoomAtPoint(next, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
		reason := "dependency-unchanged"
		if why, ok := candidateListeners[listener.ID]; ok {
			reason = why
		}
		if targetSet[listener.ID] {
			reason = "actual-result-changed:" + reason
		} else if candidateListeners[listener.ID] != "" {
			reason = "impact-checked-no-change"
		}
		items = append(items, InvalidationItem{
			Kind: "listener", ID: listener.ID, RoomID: roomID, Reason: reason,
			Status: "pending", Reused: false,
			DependsOn: []string{"param:" + roomID, "conn:" + roomID},
		})
	}
	return items
}

func (e *Engine) Resume(ctx context.Context) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	dirty := map[string]string{}
	targetListeners := []string{}
	for _, room := range e.sortedRoomsLocked() {
		state := e.states[room.ID]
		if state != nil && state.Status != StatusCommitted {
			dirty[room.ID] = "checkpoint-missing-or-incomplete"
		}
	}
	if len(dirty) == 0 {
		return e.noOpReportLocked("checkpoint-complete"), nil
	}
	for _, listener := range e.sample.Listeners {
		roomID, apiErr := RoomAtPoint(e.sample, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
		if apiErr != nil {
			return nil, apiErr
		}
		if dirty[roomID] != "" {
			targetListeners = append(targetListeners, listener.ID)
		}
	}
	plan := &recomputePlan{
		reason:          "checkpoint-resume",
		dirtyRooms:      dirty,
		targetListeners: targetListeners,
	}
	plan.items = e.allStatusItemsLocked(e.sample, dirty, targetListeners, nil)
	return e.recomputeLocked(ctx, plan)
}

func (e *Engine) InjectRoomFailure(ctx context.Context, roomID string) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.states[roomID]; !ok {
		return nil, NewAPIError(CodeSchema, roomID, "roomId", "unknown room for failure injection")
	}
	e.failRooms[roomID] = true
	dirty := map[string]string{roomID: "injected-single-room-internal-failure"}
	targetListeners := []string{}
	for _, listener := range e.sample.Listeners {
		roomIDAtPoint, apiErr := RoomAtPoint(e.sample, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
		if apiErr != nil {
			return nil, apiErr
		}
		if roomIDAtPoint == roomID {
			targetListeners = append(targetListeners, listener.ID)
		}
	}
	plan := &recomputePlan{
		reason:          "inject-room-failure:" + roomID,
		dirtyRooms:      dirty,
		targetListeners: targetListeners,
	}
	plan.items = e.allStatusItemsLocked(e.sample, dirty, targetListeners, nil)
	return e.recomputeLocked(ctx, plan)
}
