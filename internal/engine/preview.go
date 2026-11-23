package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/editing"
	"roomreverb/internal/errs"
	"roomreverb/internal/fsm"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
	"roomreverb/internal/validate"
)

type DependencyGraph struct {
	RoomToListeners   map[string][]string `json:"room_to_listeners"`
	PortalToListeners map[string][]string `json:"portal_to_listeners"`
	ListenerToRoom    map[string]string   `json:"listener_to_room"`
}

func BuildDependencies(level model.Level, rooms map[string]solver.RoomResult) *DependencyGraph {
	dep := &DependencyGraph{
		RoomToListeners:   map[string][]string{},
		PortalToListeners: map[string][]string{},
		ListenerToRoom:    map[string]string{},
	}
	roomSets := map[string]map[string]bool{}
	portalSets := map[string]map[string]bool{}
	for _, l := range level.Listeners {
		for _, r := range level.Rooms {
			if r.Rect.ContainsInclusive(l.Pos) {
				dep.ListenerToRoom[l.ID] = r.ID
				break
			}
		}
	}
	for roomID, result := range rooms {
		for _, p := range result.Pairs {
			dep.ListenerToRoom[p.ListenerID] = roomID
			if !p.Reachable {
				continue
			}
			for _, rid := range p.RoomPath {
				if roomSets[rid] == nil {
					roomSets[rid] = map[string]bool{}
				}
				roomSets[rid][p.ListenerID] = true
			}
			for _, pid := range p.PortalPath {
				if portalSets[pid] == nil {
					portalSets[pid] = map[string]bool{}
				}
				portalSets[pid][p.ListenerID] = true
			}
		}
	}
	for k, set := range roomSets {
		dep.RoomToListeners[k] = sortedSet(set)
	}
	for k, set := range portalSets {
		dep.PortalToListeners[k] = sortedSet(set)
	}
	return dep
}

type ChangeReport struct {
	Edit           model.Edit           `json:"edit"`
	Invalidations  []InvalidationRecord `json:"invalidations"`
	CandidateRooms []string             `json:"candidate_rooms"`
	Dependencies   *DependencyGraph     `json:"dependencies"`
}

type PreviewSession struct {
	level         model.Level
	cfg           Config
	store         *checkpoint.Store
	tracker       *fsm.Tracker
	rooms         map[string]solver.RoomResult
	metrics       solver.Metrics
	paramsVersion int
	invalidations []InvalidationRecord
	edits         []model.Edit
}

func NewPreview(ctx context.Context, level model.Level, store *checkpoint.Store, cfg Config) (*PreviewSession, error) {
	if err := validate.Validate(level); err != nil {
		return nil, err
	}
	s := &PreviewSession{
		level: level, cfg: cfg, store: store, tracker: fsm.New(SortedRoomIDs(level)),
		rooms: map[string]solver.RoomResult{}, paramsVersion: cfg.ParamsVersion,
	}
	ids := SortedRoomIDs(level)
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if store != nil && cfg.UseCache {
			hash := checkpoint.RoomInputHash(level, id)
			if res, ok, err := store.LoadCache(ctx, id, hash, cfg.ParamsVersion); err == nil && ok {
				s.rooms[id] = res
				continue
			}
		}
		missing = append(missing, id)
	}
	var batch solver.BatchResult
	if len(missing) > 0 {
		batch = solver.SolveRooms(ctx, level, missing, cfg.Solvers)
		s.metrics = batch.Metrics
	}
	for _, id := range ids {
		res, ok := s.rooms[id]
		if !ok {
			if solveErr, failed := batch.Failures[id]; failed {
				_ = s.tracker.Transition(id, fsm.Connectivity, "")
				s.tracker.Fail(id, solveErr)
				return nil, solveErr
			}
			var exists bool
			res, exists = batch.Rooms[id]
			if !exists {
				err := batch.Failures[id]
				if err == nil {
					err = &errs.Error{Code: errs.EInput, Room: id, Message: "room solve produced no result"}
				}
				_ = s.tracker.Transition(id, fsm.Connectivity, "")
				s.tracker.Fail(id, err)
				return nil, err
			}
		}
		if err := s.commitComputed(ctx, id, res, "persistent-cache"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *PreviewSession) Apply(ctx context.Context, edit model.Edit) (ChangeReport, error) {
	oldLevel := s.level
	applied, err := editing.Apply(oldLevel, edit)
	if err != nil {
		return ChangeReport{Edit: edit}, err
	}
	deps := BuildDependencies(oldLevel, s.rooms)
	candidates := candidateRooms(oldLevel, applied.Level, edit, deps, applied)
	oldRooms := make(map[string]solver.RoomResult, len(candidates))
	for _, id := range candidates {
		if r, ok := s.rooms[id]; ok {
			oldRooms[id] = r.Clone()
		}
	}
	batch := solver.SolveRooms(ctx, applied.Level, candidates, s.cfg.Solvers)
	candidateSet := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		candidateSet[id] = true
	}
	s.metrics = batch.Metrics
	s.paramsVersion++
	s.level = applied.Level
	s.edits = append(s.edits, edit)
	records := make([]InvalidationRecord, 0, len(SortedRoomIDs(s.level)))
	firstErr := error(nil)
	reason := editReason(edit)
	for _, id := range SortedRoomIDs(s.level) {
		oldRes, hadOld := oldRooms[id]
		newRes, hasNew := batch.Rooms[id]
		if solveErr, failed := batch.Failures[id]; failed {
			reason := editReason(edit)
			if state, ok := s.tracker.Get(id); ok {
				switch state.State {
				case fsm.Committed:
					_ = s.tracker.Invalidate(id, reason)
				case fsm.Failed:
					_ = s.tracker.Retry(id)
				}
			}
			_ = s.tracker.Transition(id, fsm.Connectivity, reason)
			s.tracker.Fail(id, solveErr)
			delete(s.rooms, id)
			records = append(records, InvalidationRecord{
				RoomID: id, Reason: reason, Invalidated: true, Reused: false,
			})
			if firstErr == nil {
				firstErr = solveErr
			}
			continue
		}
		if !hasNew {
			if candidateSet[id] {
				reason := editReason(edit)
				solveErr := batch.Failures[id]
				if solveErr == nil {
					solveErr = &errs.Error{Code: errs.EInput, Room: id, Message: "candidate room produced no result"}
				}
				if state, ok := s.tracker.Get(id); ok && state.State == fsm.Committed {
					_ = s.tracker.Invalidate(id, reason)
				}
				_ = s.tracker.Transition(id, fsm.Connectivity, reason)
				s.tracker.Fail(id, solveErr)
				delete(s.rooms, id)
				records = append(records, InvalidationRecord{
					RoomID: id, Reason: reason, Invalidated: true, Reused: false,
				})
				if firstErr == nil {
					firstErr = solveErr
				}
				continue
			}
			// Non-candidate rooms are deliberately reused untouched.
			if old, ok := s.rooms[id]; ok {
				records = append(records, InvalidationRecord{
					RoomID: id, Reason: "unchanged", Invalidated: false, Reused: true, ReusedFrom: "memory",
				})
				if err := s.bumpCache(ctx, id, old); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			continue
		}
		changed := !hadOld || !bytes.Equal(oldRes.CanonicalJSON(), newRes.CanonicalJSON())
		if !changed {
			records = append(records, InvalidationRecord{
				RoomID: id, Reason: "unchanged", Invalidated: false, Reused: true, ReusedFrom: "dependency-probe",
			})
			if err := s.bumpCache(ctx, id, newRes); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		listeners := changedListeners(oldRes, newRes)
		rec := InvalidationRecord{
			RoomID: id, Reason: reason, Invalidated: true, Reused: false,
			InvalidatedListeners: listeners,
		}
		records = append(records, rec)
		if state, ok := s.tracker.Get(id); ok && state.State == fsm.Committed {
			if err := s.tracker.Invalidate(id, reason); err != nil {
				return ChangeReport{Edit: edit, CandidateRooms: candidates, Dependencies: deps}, err
			}
		}
		if err := s.commitComputed(ctx, id, newRes, reason); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	s.invalidations = records
	report := ChangeReport{Edit: edit, Invalidations: records, CandidateRooms: candidates, Dependencies: deps}
	return report, firstErr
}

func (s *PreviewSession) commitComputed(ctx context.Context, id string, res solver.RoomResult, reason string) error {
	if err := ctx.Err(); err != nil {
		s.tracker.Fail(id, err)
		return &errs.Error{Code: errs.ECanceled, Room: id, Message: err.Error(), Wrapped: err}
	}
	if state, ok := s.tracker.Get(id); ok {
		switch state.State {
		case fsm.Committed:
			if err := s.tracker.Invalidate(id, reason); err != nil {
				return err
			}
		case fsm.Failed:
			if err := s.tracker.Retry(id); err != nil {
				return err
			}
		}
	}
	if err := s.tracker.Transition(id, fsm.Connectivity, reason); err != nil {
		return err
	}
	if err := s.tracker.Transition(id, fsm.Parameters, ""); err != nil {
		return err
	}
	_, contentHash := roomHashes(s.level, res)
	frozen := fsm.Frozen{
		Params: res.Params, Portals: clonePortals(res.Portals),
		Neighbors: append([]string(nil), res.Neighbors...),
		Pairs:     clonePairs(res.Pairs), Result: res.Clone(),
		ContentHash: contentHash, ParamsVersion: s.paramsVersion,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	if err := s.tracker.Commit(id, frozen); err != nil {
		return err
	}
	s.rooms[id] = res.Clone()
	if s.store != nil && s.cfg.UseCache {
		if err := s.store.SaveCache(ctx, res, checkpoint.RoomInputHash(s.level, id), s.paramsVersion); err != nil {
			return err
		}
	}
	return nil
}

func (s *PreviewSession) bumpCache(ctx context.Context, id string, res solver.RoomResult) error {
	if s.store == nil || !s.cfg.UseCache {
		return nil
	}
	return s.store.SaveCache(ctx, res, checkpoint.RoomInputHash(s.level, id), s.paramsVersion)
}

func (s *PreviewSession) RunResult() *RunResult {
	base := &RunResult{Invalidations: append([]InvalidationRecord(nil), s.invalidations...)}
	res := buildResult(s.level, s.tracker, s.rooms, s.metrics, base)
	res.ParamsVersion = s.paramsVersion
	return res
}

func (s *PreviewSession) Level() model.Level { return s.level.Clone() }

func (s *PreviewSession) Edits() []model.Edit {
	return append([]model.Edit(nil), s.edits...)
}

// Retry re-solves one failed room after the user has corrected the level or
// wants to retry a transient resource failure.
func (s *PreviewSession) Retry(ctx context.Context, roomID string) error {
	state, ok := s.tracker.Get(roomID)
	if !ok {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "unknown room"}
	}
	if state.State != fsm.Failed {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "retry requires a failed room"}
	}
	if err := s.tracker.Retry(roomID); err != nil {
		return err
	}
	batch := solver.SolveRooms(ctx, s.level, []string{roomID}, s.cfg.Solvers)
	if solveErr, failed := batch.Failures[roomID]; failed {
		_ = s.tracker.Transition(roomID, fsm.Connectivity, "")
		s.tracker.Fail(roomID, solveErr)
		return solveErr
	}
	res, ok := batch.Rooms[roomID]
	if !ok {
		err := batch.Failures[roomID]
		if err == nil {
			err = &errs.Error{Code: errs.EInput, Room: roomID, Message: "retry produced no result"}
		}
		_ = s.tracker.Transition(roomID, fsm.Connectivity, "")
		s.tracker.Fail(roomID, err)
		return err
	}
	s.metrics = batch.Metrics
	return s.commitComputed(ctx, roomID, res, "retry")
}

func (s *PreviewSession) Dependencies() *DependencyGraph {
	return BuildDependencies(s.level, s.rooms)
}

func candidateRooms(oldLevel, newLevel model.Level, edit model.Edit, deps *DependencyGraph, applied editing.Result) []string {
	set := map[string]bool{}
	for _, id := range applied.Rooms {
		set[id] = true
	}
	addListenerRooms := func(listenerIDs []string) {
		for _, l := range listenerIDs {
			if room := deps.ListenerToRoom[l]; room != "" {
				set[room] = true
			}
		}
	}
	switch edit.Kind {
	case model.EditSetRoomParams:
		addListenerRooms(deps.RoomToListeners[edit.RoomID])
		set[edit.RoomID] = true
	case model.EditAddPortal, model.EditRemovePortal:
		addListenerRooms(deps.PortalToListeners[edit.PortalID])
		start := append([]string(nil), applied.Rooms...)
		if edit.Kind == model.EditRemovePortal {
			start = append(start, wallRoomsForPortal(oldLevel, edit.PortalID)...)
		}
		for _, id := range downstreamRooms(oldLevel, newLevel, start) {
			set[id] = true
		}
	case model.EditMoveRoomEdge:
		for _, pid := range applied.Portals {
			addListenerRooms(deps.PortalToListeners[pid])
		}
		for _, id := range downstreamRooms(oldLevel, newLevel, applied.Rooms) {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		if _, ok := newLevel.Room(id); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func wallRoomsForPortal(level model.Level, portalID string) []string {
	o, ok := level.Opening(portalID)
	if !ok {
		return nil
	}
	w, ok := level.Wall(o.WallID)
	if !ok {
		return nil
	}
	return append([]string(nil), w.Rooms...)
}

func downstreamRooms(oldLevel, newLevel model.Level, starts []string) []string {
	adj := adjacency(oldLevel)
	for room, ns := range adjacency(newLevel) {
		adj[room] = append(adj[room], ns...)
	}
	seen := map[string]bool{}
	queue := append([]string(nil), starts...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, adj[id]...)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func adjacency(level model.Level) map[string][]string {
	out := map[string][]string{}
	for _, r := range level.Rooms {
		out[r.ID] = nil
	}
	for _, o := range level.Openings {
		if !o.Active {
			continue
		}
		w, ok := level.Wall(o.WallID)
		if !ok || len(w.Rooms) != 2 {
			continue
		}
		out[w.Rooms[0]] = append(out[w.Rooms[0]], w.Rooms[1])
		out[w.Rooms[1]] = append(out[w.Rooms[1]], w.Rooms[0])
	}
	return out
}

func changedListeners(old, new solver.RoomResult) []string {
	set := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range old.Pairs {
		ids[p.ListenerID+"\x00"+p.SourceID] = true
	}
	for _, p := range new.Pairs {
		ids[p.ListenerID+"\x00"+p.SourceID] = true
	}
	for key := range ids {
		var listener, source string
		for i := 0; i < len(key); i++ {
			if key[i] == 0 {
				listener, source = key[:i], key[i+1:]
				break
			}
		}
		a, oka := old.Pair(source, listener)
		b, okb := new.Pair(source, listener)
		if !oka || !okb || !bytes.Equal(a.CanonicalJSONBytes(), b.CanonicalJSONBytes()) {
			set[listener] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func editReason(edit model.Edit) string {
	switch edit.Kind {
	case model.EditSetRoomParams:
		return fmt.Sprintf("room_params:%s", edit.RoomID)
	case model.EditAddPortal:
		if edit.Portal != nil {
			return fmt.Sprintf("portal_added:%s", edit.Portal.ID)
		}
		return "portal_added"
	case model.EditRemovePortal:
		return fmt.Sprintf("portal_removed:%s", edit.PortalID)
	case model.EditMoveRoomEdge:
		return fmt.Sprintf("edge_moved:%s:%s", edit.RoomID, edit.Edge)
	default:
		return string(edit.Kind)
	}
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
