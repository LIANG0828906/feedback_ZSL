package solver

import (
	"container/heap"
	"fmt"
	"strings"

	"levelacoustic/internal/fixed"
	"levelacoustic/internal/geom"
	"levelacoustic/internal/model"
)

type searchLabel struct {
	cost     int64
	roomID   string
	lastDoor string
	rooms    []string
	doors    []string
}

type labelHeap []searchLabel

func (h labelHeap) Len() int { return len(h) }
func (h labelHeap) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	ki := geom.ChainKey(h[i].rooms) + "\x01" + geom.DoorKey(h[i].doors)
	kj := geom.ChainKey(h[j].rooms) + "\x01" + geom.DoorKey(h[j].doors)
	return ki < kj
}
func (h labelHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *labelHeap) Push(x any)   { *h = append(*h, x.(searchLabel)) }
func (h *labelHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type Connector struct {
	Graph *geom.Graph
}

func NewConnector(g *geom.Graph) *Connector {
	return &Connector{Graph: g}
}

// ShortestChain runs Dijkstra over (room, lastDoor) states. A state records
// the last portal used, so the geometric cost of the next inter-door segment
// is exact. The graph edge exists only for a real door on a shared wall.
func (c *Connector) ShortestChain(source, listener model.Point) (geom.Path, error) {
	g := c.Graph
	sourceRoom, ok := g.RoomForPoint(source.X, source.Y)
	if !ok {
		return geom.Path{}, model.GeometryError("", "", "source_position", "source %s is outside all rooms", source.ID)
	}
	listenerRoom, ok := g.RoomForPoint(listener.X, listener.Y)
	if !ok {
		return geom.Path{}, model.GeometryError("", "", "listener_position", "listener %s is outside all rooms", listener.ID)
	}
	if sourceRoom == listenerRoom {
		return geom.DirectPath(g, source, listener)
	}

	start := searchLabel{roomID: sourceRoom, rooms: []string{sourceRoom}}
	pq := &labelHeap{start}
	heap.Init(pq)
	bestCost := map[string]int64{stateKey(sourceRoom, ""): 0}
	bestPrefix := map[string]string{stateKey(sourceRoom, ""): ""}

	var best *geom.Path
	var bestTotal int64
	for pq.Len() > 0 {
		label := heap.Pop(pq).(searchLabel)
		if best != nil && label.cost > bestTotal {
			break
		}
		key := stateKey(label.roomID, label.lastDoor)
		if label.cost > bestCost[key] {
			continue
		}

		if label.roomID == listenerRoom && label.lastDoor != "" {
			candidate, err := geom.BuildPath(g, source, listener, label.doors)
			if err != nil {
				return geom.Path{}, err
			}
			if best == nil || geom.BetterPath(candidate, *best) {
				copyPath := candidate
				best = &copyPath
				bestTotal = candidate.PathLengthMicro
			}
			continue
		}

		var prev model.Point
		if label.lastDoor == "" {
			prev = source
		} else {
			prev = g.Doors[label.lastDoor].Center
		}
		for _, doorID := range g.RoomDoors[label.roomID] {
			if doorID == label.lastDoor {
				continue
			}
			link := g.Doors[doorID]
			nextRoom, ok := link.OtherRoom(label.roomID)
			if !ok {
				continue
			}
			step, _ := fixed.SegmentLength(int64(link.Center.X-prev.X), int64(link.Center.Y-prev.Y))
			nextCost := label.cost + step
			nextKey := stateKey(nextRoom, doorID)
			nextRooms := appendCopy(label.rooms, nextRoom)
			nextDoors := appendCopy(label.doors, doorID)
			nextLabel := searchLabel{
				cost:     nextCost,
				roomID:   nextRoom,
				lastDoor: doorID,
				rooms:    nextRooms,
				doors:    nextDoors,
			}
			prefix := labelPrefix(nextLabel)
			if old, seen := bestCost[nextKey]; seen {
				if nextCost > old || (nextCost == old && prefix >= bestPrefix[nextKey]) {
					continue
				}
			}
			bestCost[nextKey] = nextCost
			bestPrefix[nextKey] = prefix
			heap.Push(pq, nextLabel)
		}
	}
	if best == nil {
		return geom.Path{Reachable: false, ListenerRoomID: listenerRoom}, nil
	}
	return *best, nil
}

func (c *Connector) ChainSummary(source, listener model.Point) (model.ChainSummary, error) {
	path, err := c.ShortestChain(source, listener)
	if err != nil {
		return model.ChainSummary{}, err
	}
	return model.ChainSummary{
		SourceID:        source.ID,
		ListenerID:      listener.ID,
		ListenerRoomID:  path.ListenerRoomID,
		Reachable:       path.Reachable,
		RoomChain:       append([]string(nil), path.RoomChain...),
		DoorChain:       append([]string(nil), path.DoorChain...),
		PathLengthMicro: path.PathLengthMicro,
		Segments:        append([]model.TruthSegment(nil), path.Segments...),
		Truncations:     append([]model.Truncation(nil), path.Truncations...),
	}, nil
}

func stateKey(roomID, lastDoor string) string {
	return roomID + "\x00" + lastDoor
}

func labelPrefix(l searchLabel) string {
	return geom.ChainKey(l.rooms) + "\x01" + geom.DoorKey(l.doors)
}

func appendCopy[T any](src []T, value T) []T {
	out := make([]T, len(src)+1)
	copy(out, src)
	out[len(src)] = value
	return out
}

func FirstChainDivergence(want, got []string) (int, string) {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			return i, want[i]
		}
	}
	if len(want) != len(got) {
		if len(want) > n {
			return n, want[n]
		}
		return n, got[n]
	}
	return -1, ""
}

func CompareTruth(p *model.Project, chains []model.ChainSummary) error {
	if p.Truth == nil {
		return nil
	}
	if !p.Truth.VerifyFingerprint(p) {
		return model.SchemaError("truth.project_fingerprint", "truth does not belong to the current project")
	}
	truthByPair := make(map[string]model.TruthEntry, len(p.Truth.Entries))
	for _, entry := range p.Truth.Entries {
		truthByPair[model.PairID(entry.SourceID, entry.ListenerID)] = entry
	}
	for _, chain := range chains {
		key := model.PairID(chain.SourceID, chain.ListenerID)
		truth, ok := truthByPair[key]
		if !ok {
			return &model.Error{
				Code:       model.ErrChain,
				Message:    fmt.Sprintf("truth is missing pair %s", key),
				SourceID:   chain.SourceID,
				ListenerID: chain.ListenerID,
			}
		}
		if truth.Reachable != chain.Reachable {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("reachability differs for %s", key),
				SourceID:        chain.SourceID,
				ListenerID:      chain.ListenerID,
				FirstDivergence: firstOrEmpty(truth.RoomChain),
			}
		}
		if !truth.Reachable {
			continue
		}
		if idx, room := FirstChainDivergence(truth.RoomChain, chain.RoomChain); idx >= 0 {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("room chain differs for %s at index %d: truth %s, solver %s", key, idx, strings.Join(truth.RoomChain, ">"), strings.Join(chain.RoomChain, ">")),
				SourceID:        chain.SourceID,
				ListenerID:      chain.ListenerID,
				FirstDivergence: room,
				Field:           "room_chain",
			}
		}
		if !sameStrings(truth.DoorChain, chain.DoorChain) {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("door chain differs for %s: truth %v, solver %v", key, truth.DoorChain, chain.DoorChain),
				SourceID:        chain.SourceID,
				ListenerID:      chain.ListenerID,
				FirstDivergence: firstOrEmpty(truth.RoomChain),
				Field:           "door_chain",
			}
		}
		diff := chain.PathLengthMicro - truth.PathLengthMicro
		if diff < 0 {
			diff = -diff
		}
		if diff > p.Tolerance.PathLengthMicro {
			divIdx, divRoom := firstSegmentDivergence(truth.Segments, chain.Segments)
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("path length differs for %s by %d micro-units (tolerance %d)", key, diff, p.Tolerance.PathLengthMicro),
				SourceID:        chain.SourceID,
				ListenerID:      chain.ListenerID,
				FirstDivergence: divRoom,
				Field:           fmt.Sprintf("segments[%d].length_micro", divIdx),
				Expected:        truth.PathLengthMicro,
				Actual:          chain.PathLengthMicro,
			}
		}
	}
	return nil
}

func firstSegmentDivergence(want, got []model.TruthSegment) (int, string) {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i].LengthMicro != got[i].LengthMicro || want[i].RoomID != got[i].RoomID {
			return i, want[i].RoomID
		}
	}
	if len(want) > n {
		return n, want[n].RoomID
	}
	if len(got) > n {
		return n, got[n].RoomID
	}
	return -1, ""
}

func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
