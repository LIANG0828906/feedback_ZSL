package engine

import (
	"fmt"
	"sort"
	"sync"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
)

// Edit is one deterministic timeline mutation.
type Edit struct {
	Op      string `json:"op"` // time|amount|fault|set_flag|add_sync|remove_sync
	EventID string `json:"event_id,omitempty"`
	At      int64  `json:"at,omitempty"`
	Amount  int64  `json:"amount,omitempty"`
	Bool    *bool  `json:"bool,omitempty"`
	Flag    string `json:"flag,omitempty"`
	SyncID  string `json:"sync_id,omitempty"`
	SyncAt  int64  `json:"sync_at,omitempty"`
}

// Manifest is the invalidation/reuse manifest for one edit.
type Manifest struct {
	Edit            Edit         `json:"edit"`
	Items           []ChangeItem `json:"items"`
	RecomputedEvent []string     `json:"recomputed_events"`
	ReusedEvents    []string     `json:"reused_events"`
	DirtySegments   []int        `json:"dirty_segments"`
	ReusedSegments  []int        `json:"reused_segments"`
}

// IncrementalResult bundles report + manifest for an edit.
type IncrementalResult struct {
	Report   *model.Report `json:"report"`
	Manifest *Manifest     `json:"manifest"`
}

// Service owns the editable replay, outcome cache and dependency graph.
type Service struct {
	mu       sync.Mutex
	params   model.Params
	replay   *model.Replay
	cache    map[string]model.Outcome
	attempts map[string]int
	report   *model.Report
}

// NewService creates an incremental service. Call RunFull first to warm cache.
func NewService(r *model.Replay, p model.Params) *Service {
	return &Service{
		params:   p,
		replay:   r,
		cache:    map[string]model.Outcome{},
		attempts: map[string]int{},
	}
}

// Replay exposes the owned replay (callers must not mutate without ApplyEdit).
func (s *Service) Replay() *model.Replay { return s.replay }

// LastReport returns the latest settled report.
func (s *Service) LastReport() *model.Report { return s.report }

// RunFull warms the cache and settles everything sequentially.
func (s *Service) RunFull() (*model.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, err := SettleFull(s.replay, s.params)
	if rep != nil {
		s.storeOutcomesFromReport(rep)
		s.report = rep
	}
	return rep, err
}

// RunFullPooled warms the cache using the bounded worker pool.
func (s *Service) RunFullPooled() (*model.Report, int, []BlockRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, peak, blocks, err := SettleFullPooled(s.replay, s.params)
	if rep != nil {
		s.storeOutcomesFromReport(rep)
		s.report = rep
	}
	return rep, peak, blocks, err
}

func (s *Service) storeOutcomesFromReport(rep *model.Report) {
	// Reconstruct cacheable outcomes from report rows.
	for _, res := range rep.EventResults {
		s.cache[res.EventID] = model.Outcome{
			Status: res.Status, Reason: res.Reason, Effects: res.Effects,
		}
	}
}

// RetryFailed increments the attempt counter for failed events and reruns the
// incremental pipeline. A persistent deterministic fault fails again without
// polluting other events; fixing the event via ApplyEdit then succeeds.
func (s *Service) RetryFailed() (*IncrementalResult, error) {
	s.mu.Lock()
	var ids []string
	for id, oc := range s.cache {
		if oc.Status == StatusFailed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	s.mu.Unlock()
	if len(ids) == 0 {
		return &IncrementalResult{Report: s.report, Manifest: &Manifest{}}, nil
	}
	target := ids[0]
	return s.ApplyEdit(Edit{Op: "retry", EventID: target})
}

// ApplyEdit mutates the replay, plans precise invalidation and recomputes.
func (s *Service) ApplyEdit(edit Edit) (*IncrementalResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seeds, explicitDirtySegs, err := ApplyEdit(s.replay, s.params, edit)
	if err != nil {
		return nil, err
	}

	g := BuildGraph(s.replay, s.params)
	reached := g.Propagate(seeds)

	order := CanonicalOrder(s.replay.Events)
	manifest := &Manifest{Edit: edit}
	dirtyEvents := map[string]string{}

	for _, e := range order {
		nid := NodeID(NodeEvent, e.ID)
		reason, bad := reached[nid]
		_, cached := s.cache[e.ID]
		item := ChangeItem{ID: e.ID, Kind: NodeEvent}
		if edit.Op == "retry" && e.ID == edit.EventID {
			reason = "event_retry"
			bad = true
		}
		if bad {
			item.Invalidated = true
			item.Reason = reason
			dirtyEvents[e.ID] = reason
			manifest.RecomputedEvent = append(manifest.RecomputedEvent, e.ID)
		} else {
			item.Reused = cached
			if cached {
				manifest.ReusedEvents = append(manifest.ReusedEvents, e.ID)
			} else {
				item.Invalidated = true
				item.Reason = "cache_miss"
				dirtyEvents[e.ID] = "cache_miss"
				manifest.RecomputedEvent = append(manifest.RecomputedEvent, e.ID)
			}
		}
		manifest.Items = append(manifest.Items, item)
	}

	// Constraint / flag / sync nodes reached.
	otherNodes := make([]string, 0)
	for nid := range reached {
		kind, id, ok := splitNode(nid)
		if !ok || kind == NodeEvent || kind == NodeSegment {
			continue
		}
		otherNodes = append(otherNodes, nid)
		_ = id
	}
	sort.Strings(otherNodes)
	for _, nid := range otherNodes {
		kind, id, _ := splitNode(nid)
		manifest.Items = append(manifest.Items, ChangeItem{
			ID: id, Kind: kind, Invalidated: true, Reason: reached[nid],
		})
	}

	// Segment invalidation: reached segment nodes + explicit old segments.
	dirtySegSet := map[int]string{}
	for nid, reason := range reached {
		kind, raw, ok := splitNode(nid)
		if !ok || kind != NodeSegment {
			continue
		}
		var seg int
		if _, scanErr := fmt.Sscanf(raw, "%d", &seg); scanErr == nil {
			dirtySegSet[seg] = reason
		}
	}
	for _, seg := range explicitDirtySegs {
		if _, ok := dirtySegSet[seg]; !ok {
			dirtySegSet[seg] = "event_moved_out"
		}
	}

	// Recompute dirty outcomes and atomically replace cache entries.
	flags, _, flagErr := ResolveFlags(s.replay)
	if flagErr != nil {
		return nil, flagErr
	}
	mutexDec, _, mutexErr := adjudicateMutex(s.replay, order, flags)
	if mutexErr != nil {
		return nil, mutexErr
	}
	outcomes := make(map[string]model.Outcome, len(order))
	for _, e := range order {
		if reason, dirty := dirtyEvents[e.ID]; dirty {
			oc := EvaluateOutcome(e, flags, mutexDec[e.ID])
			s.cache[e.ID] = oc // atomic whole-value replacement under lock
			if edit.Op == "retry" && e.ID == edit.EventID {
				s.attempts[e.ID]++
			}
			_ = reason
		}
		outcomes[e.ID] = s.cache[e.ID]
	}

	rep, settleErr := settle(s.replay, s.params, outcomes, s.attempts)

	maxSeg := 0
	if rep != nil {
		s.report = rep
		for _, c := range rep.CommitSequence {
			if c.Segment > maxSeg {
				maxSeg = c.Segment
			}
		}
	}
	for seg := 0; seg <= maxSeg; seg++ {
		if reason, dirty := dirtySegSet[seg]; dirty {
			manifest.DirtySegments = append(manifest.DirtySegments, seg)
			manifest.Items = append(manifest.Items, ChangeItem{
				ID: fmt.Sprintf("%d", seg), Kind: NodeSegment, Invalidated: true, Reason: reason,
			})
		} else {
			manifest.ReusedSegments = append(manifest.ReusedSegments, seg)
			manifest.Items = append(manifest.Items, ChangeItem{
				ID: fmt.Sprintf("%d", seg), Kind: NodeSegment, Reused: true,
			})
		}
	}
	sort.Ints(manifest.DirtySegments)
	sort.Ints(manifest.ReusedSegments)
	sort.Strings(manifest.RecomputedEvent)
	sort.Strings(manifest.ReusedEvents)
	return &IncrementalResult{Report: rep, Manifest: manifest}, settleErr
}

func splitNode(nid string) (kind, id string, ok bool) {
	for i := 0; i < len(nid); i++ {
		if nid[i] == ':' {
			return nid[:i], nid[i+1:], true
		}
	}
	return "", "", false
}

func syncNodeID(at int64, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return fmt.Sprintf("sync@%d", at)
}

// ApplyEdit applies an edit directly to a replay and returns propagation seeds
// plus any segment indices that must be explicitly dirtied. It is used by the
// incremental service and by the other two paths so all share one mutation.
func ApplyEdit(r *model.Replay, p model.Params, edit Edit) (map[string]string, []int, error) {
	seeds := map[string]string{}
	var extraSegs []int
	idx := r.EventIndex()

	switch edit.Op {
	case "time", "amount", "fault", "retry":
		e, ok := idx[edit.EventID]
		if !ok {
			return nil, nil, errs.New(errs.ESchema, "edit.event_id", edit.EventID, "unknown event")
		}
		switch edit.Op {
		case "time":
			if edit.At < 0 {
				return nil, nil, errs.New(errs.ESchema, "edit.at", edit.EventID, "time must be >= 0")
			}
			oldSeg := SegmentOf(e.At, p.SegmentUnits)
			newSeg := SegmentOf(edit.At, p.SegmentUnits)
			if oldSeg != newSeg {
				extraSegs = append(extraSegs, oldSeg)
			}
			e.At = edit.At
			seeds[NodeID(NodeEvent, e.ID)] = "event_time_changed"
		case "amount":
			if edit.Amount < 0 {
				return nil, nil, errs.New(errs.ESchema, "edit.amount", edit.EventID, "amount must be >= 0")
			}
			e.Amount = edit.Amount
			seeds[NodeID(NodeEvent, e.ID)] = "event_amount_changed"
		case "fault":
			if edit.Bool == nil {
				return nil, nil, errs.New(errs.ESchema, "edit.bool", edit.EventID, "fault edit requires bool")
			}
			e.Fault = *edit.Bool
			seeds[NodeID(NodeEvent, e.ID)] = "event_fault_changed"
		case "retry":
			seeds[NodeID(NodeEvent, e.ID)] = "event_retry"
		}

	case "set_flag":
		if edit.Flag == "" {
			return nil, nil, errs.New(errs.ESchema, "edit.flag", "", "flag edit requires name")
		}
		if edit.Bool == nil {
			return nil, nil, errs.New(errs.ESchema, "edit.bool", "", "flag edit requires bool")
		}
		found := false
		for i := range r.Flags {
			if r.Flags[i].Name == edit.Flag {
				r.Flags[i].Value = *edit.Bool
				found = true
				break
			}
		}
		if !found {
			r.Flags = append(r.Flags, model.FlagDef{
				ID: "flagdef:" + edit.Flag, Name: edit.Flag, Value: *edit.Bool, Priority: 0,
			})
		}
		seeds[NodeID(NodeFlag, edit.Flag)] = "flag_value_changed"

	case "add_sync":
		id := syncNodeID(edit.SyncAt, edit.SyncID)
		if edit.SyncID == "" {
			// keep generated id in sync with engine convention
			id = syncNodeID(edit.SyncAt, "")
		}
		exists := false
		for i := range r.Sync {
			if r.Sync[i].ID == id || r.Sync[i].At == edit.SyncAt {
				r.Sync[i].At = edit.SyncAt
				exists = true
				break
			}
		}
		if !exists {
			r.Sync = append(r.Sync, model.SyncPoint{ID: id, At: edit.SyncAt})
		}
		seeds[NodeID(NodeSync, id)] = "sync_added"

	case "remove_sync":
		removedAt := int64(-1)
		kept := r.Sync[:0]
		for _, s := range r.Sync {
			if (edit.SyncID != "" && s.ID == edit.SyncID) || (edit.SyncAt != 0 && s.At == edit.SyncAt) {
				removedAt = s.At
				seeds[NodeID(NodeSync, s.ID)] = "sync_removed"
				continue
			}
			kept = append(kept, s)
		}
		r.Sync = kept
		if removedAt >= 0 {
			extraSegs = append(extraSegs, SegmentOf(removedAt, p.SegmentUnits))
		}
		if len(seeds) == 0 {
			return nil, nil, errs.New(errs.ESchema, "edit.sync_id", edit.SyncID, "sync point not found")
		}

	default:
		return nil, nil, errs.New(errs.ESchema, "edit.op", "", "unknown edit op %q", edit.Op)
	}
	return seeds, extraSegs, nil
}

// SettleFullPooled evaluates pure outcomes on the bounded pool, then runs the
// exact same ordered fold as SettleFull.
func SettleFullPooled(r *model.Replay, p model.Params) (*model.Report, int, []BlockRecord, error) {
	flags, _, flagErr := ResolveFlags(r)
	if flagErr != nil {
		rep, err := settle(r, p, nil, nil)
		return rep, 0, nil, err
	}
	order := CanonicalOrder(r.Events)
	decisions, _, mutexErr := adjudicateMutex(r, order, flags)
	if mutexErr != nil {
		rep, err := settle(r, p, nil, nil)
		return rep, 0, nil, err
	}

	pool := NewPool(p.MaxParallel, p.WorkerBuffer)
	outcomes := make([]model.Outcome, len(order))
	for i, e := range order {
		i, e := i, e
		pool.Submit(e.ID, func() {
			outcomes[i] = EvaluateOutcome(e, flags, decisions[e.ID])
		})
	}
	pool.Close()

	m := make(map[string]model.Outcome, len(order))
	for i, e := range order {
		m[e.ID] = outcomes[i]
	}
	rep, err := settle(r, p, m, nil)
	return rep, pool.PeakParallel(), pool.Blocks(), err
}
