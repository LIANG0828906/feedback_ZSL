package thermalcache

import (
	"encoding/json"
	"fmt"
	"sort"
)

type EditKind string

const (
	EditObjectAccess  EditKind = "object_access"
	EditLayerCapacity EditKind = "layer_capacity"
	EditAppendEvent   EditKind = "append_access_event"
	EditRemoveEvent   EditKind = "remove_access_event"
)

type Edit struct {
	Kind          EditKind    `json:"kind"`
	ObjectID      string      `json:"object_id,omitempty"`
	Counts        []int64     `json:"counts,omitempty"`
	AtMs          int64       `json:"at_ms,omitempty"`
	SetWatermark  bool        `json:"set_watermark,omitempty"`
	Watermark     *int64      `json:"watermark,omitempty"`
	SetLastAccess bool        `json:"set_last_access,omitempty"`
	LastAccessMs  int64       `json:"last_access_ms,omitempty"`
	NodeID        string      `json:"node_id,omitempty"`
	Tier          Tier        `json:"tier,omitempty"`
	CapacityBytes int64       `json:"capacity_bytes,omitempty"`
	Event         AccessEvent `json:"event,omitempty"`
	EventIndex    int         `json:"event_index,omitempty"`
}

type Invalidation struct {
	EditIndex       int      `json:"edit_index"`
	Kind            EditKind `json:"kind"`
	Source          string   `json:"source"`
	FromMs          int64    `json:"from_ms"`
	ReusedTicks     []int64  `json:"reused_ticks"`
	RecomputedTicks []int64  `json:"recomputed_ticks"`
	Objects         []string `json:"objects"`
	Tiers           []string `json:"tiers"`
	BatchIDs        []string `json:"batch_ids"`
	Closure         []string `json:"closure"`
}

type IncrementalResult struct {
	Result          *SimulationResult `json:"result"`
	Invalidations   []Invalidation    `json:"invalidations"`
	ReusedTicks     []int64           `json:"reused_ticks"`
	RecomputedTicks []int64           `json:"recomputed_ticks"`
	FullMatch       bool              `json:"full_match"`
}

type Simulator struct {
	project   *Project
	base      *SimulationResult
	snapshots []*Checkpoint
	ran       bool
}

func NewSimulator(p *Project) (*Simulator, error) {
	if p == nil {
		return nil, biz(CodeSchemaInvalid, "project is nil", nil)
	}
	cp := p.Clone()
	if err := cp.Validate(); err != nil {
		return nil, err
	}
	canonicalize(cp)
	return &Simulator{project: cp}, nil
}

func (s *Simulator) Project() *Project {
	return s.project.Clone()
}

func (s *Simulator) Run() (*SimulationResult, error) {
	if s.ran {
		return s.base, nil
	}
	eng, err := NewEngine(s.project)
	if err != nil {
		return nil, err
	}
	timeline := eng.Timeline()
	var ticks []TickResult
	for range timeline {
		tr, err := eng.RunOne()
		if err != nil {
			return nil, err
		}
		ticks = append(ticks, *tr)
		snap, err := eng.Snapshot()
		if err != nil {
			return nil, err
		}
		s.snapshots = append(s.snapshots, snap)
	}
	res, err := eng.Finish()
	if err != nil {
		return nil, err
	}
	res.Ticks = ticks
	s.base = res
	s.ran = true
	return res, nil
}

func (s *Simulator) Base() (*SimulationResult, error) {
	if !s.ran {
		return s.Run()
	}
	return s.base, nil
}

func engineFromSnapshot(p *Project, cp *Checkpoint) (*Engine, error) {
	eng, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if err := eng.restoreCheckpointLocked(cp, false, false); err != nil {
		return nil, err
	}
	return eng, nil
}

// Apply edits the project copy, reuses the cached prefix before the earliest
// dependency, and recomputes only the dependent suffix. It additionally runs a
// clean full recomputation as an audit bit; FullMatch must be true.
func (s *Simulator) Apply(edits ...Edit) (*IncrementalResult, error) {
	if _, err := s.Run(); err != nil {
		return nil, err
	}
	edited := s.project.Clone()
	fromMs := int64(0)
	resolvedFrom := make([]int64, len(edits))
	for i, edit := range edits {
		at, err := applyEdit(edited, edit)
		if err != nil {
			return nil, biz(CodeEditInvalid, "edit application failed", map[string]any{
				"edit_index": i, "kind": edit.Kind, "error": err.Error(),
			})
		}
		if i == 0 || at < fromMs {
			fromMs = at
		}
		resolvedFrom[i] = at
	}
	if err := edited.Validate(); err != nil {
		return nil, err
	}
	canonicalize(edited)

	cutoff := -1
	for i, t := range s.base.Timeline {
		if t < fromMs {
			cutoff = i
		} else {
			break
		}
	}
	var eng *Engine
	var err error
	if cutoff >= 0 {
		eng, err = engineFromSnapshot(edited, s.snapshots[cutoff])
		if err != nil {
			return nil, err
		}
	} else {
		eng, err = NewEngine(edited)
		if err != nil {
			return nil, err
		}
	}
	newTimeline := eng.Timeline()
	if cutoff >= 0 {
		if cutoff+1 > len(newTimeline) || !equalTimelinePrefix(s.base.Timeline, newTimeline, cutoff+1) {
			return nil, biz(CodeEditInvalid, "edit changed the timeline before its dependency cutoff", map[string]any{
				"from_ms": fromMs, "cutoff": cutoff,
			})
		}
	}

	ticks := make([]TickResult, 0, len(newTimeline))
	if cutoff >= 0 {
		ticks = append(ticks, s.base.Ticks[:cutoff+1]...)
	}
	for eng.nextTimeline < len(newTimeline) {
		tr, err := eng.RunOne()
		if err != nil {
			return nil, err
		}
		ticks = append(ticks, *tr)
	}
	res, err := eng.Finish()
	if err != nil {
		return nil, err
	}
	res.Ticks = ticks

	reused := append([]int64(nil), newTimeline[:cutoff+1]...)
	recomputed := append([]int64(nil), newTimeline[cutoff+1:]...)
	objects, tiers, batchIDs := diffClosure(s.base, res, recomputed)
	closure := make([]string, 0, len(objects)+len(tiers)+len(batchIDs))
	for _, id := range objects {
		closure = append(closure, "object:"+id)
	}
	for _, tier := range tiers {
		closure = append(closure, "tier:"+tier)
	}
	for _, id := range batchIDs {
		closure = append(closure, "batch:"+id)
	}
	sort.Strings(closure)

	inv := make([]Invalidation, 0, len(edits))
	for i, edit := range edits {
		source := editSource(edit)
		oneClosure := append([]string(nil), closure...)
		if len(oneClosure) == 0 {
			oneClosure = []string{source}
		}
		inv = append(inv, Invalidation{
			EditIndex: i, Kind: edit.Kind, Source: source, FromMs: resolvedFrom[i],
			ReusedTicks:     append([]int64(nil), reused...),
			RecomputedTicks: append([]int64(nil), recomputed...),
			Objects:         append([]string(nil), objects...),
			Tiers:           append([]string(nil), tiers...),
			BatchIDs:        append([]string(nil), batchIDs...),
			Closure:         oneClosure,
		})
	}

	fullSim, err := NewSimulator(edited)
	if err != nil {
		return nil, err
	}
	full, err := fullSim.Run()
	if err != nil {
		return nil, err
	}
	return &IncrementalResult{
		Result: res, Invalidations: inv,
		ReusedTicks: reused, RecomputedTicks: recomputed,
		FullMatch: simulationEqual(full, res),
	}, nil
}

func equalTimelinePrefix(a, b []int64, n int) bool {
	if len(a) < n || len(b) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func applyEdit(p *Project, edit Edit) (int64, error) {
	switch edit.Kind {
	case EditObjectAccess:
		var obj *Object
		for i := range p.Objects {
			if p.Objects[i].ID == edit.ObjectID {
				obj = &p.Objects[i]
				break
			}
		}
		if obj == nil {
			return 0, biz(CodeObjectNotFound, "edit object does not exist", map[string]any{"object_id": edit.ObjectID})
		}
		if edit.Counts != nil {
			for i, count := range edit.Counts {
				if count < 0 {
					return 0, biz(CodeEditInvalid, "edited access count is negative", map[string]any{
						"object_id": edit.ObjectID, "index": i,
					})
				}
			}
			obj.AccessCounts = append([]int64(nil), edit.Counts...)
		}
		if edit.SetWatermark {
			if edit.Watermark == nil {
				obj.Watermark = nil
			} else {
				v := *edit.Watermark
				if v < 0 {
					return 0, biz(CodeEditInvalid, "edited watermark is negative", map[string]any{"object_id": edit.ObjectID})
				}
				obj.Watermark = &v
			}
		}
		if edit.SetLastAccess {
			if edit.LastAccessMs < 0 {
				return 0, biz(CodeEditInvalid, "edited last access is negative", map[string]any{"object_id": edit.ObjectID})
			}
			obj.LastAccessMs = edit.LastAccessMs
		}
		return editEarliest(edit)
	case EditLayerCapacity:
		at := edit.AtMs
		if at < 0 {
			return 0, biz(CodeEditInvalid, "capacity edit time is negative", nil)
		}
		nodeID := edit.NodeID
		if nodeID == "" {
			for _, n := range p.Nodes {
				if n.Tier == edit.Tier {
					nodeID = n.ID
					break
				}
			}
			if nodeID == "" {
				return 0, biz(CodeEditInvalid, "no node exists for layer capacity edit", map[string]any{"tier": edit.Tier})
			}
		}
		node, ok := p.nodeByID(nodeID)
		if !ok {
			return 0, biz(CodeNodeNotFound, "capacity edit node does not exist", map[string]any{"node_id": nodeID})
		}
		target := edit.CapacityBytes
		if edit.NodeID == "" {
			effective := map[string]int64{}
			for _, n := range p.Nodes {
				effective[n.ID] = n.CapacityBytes
			}
			for _, ce := range p.CapacityEvents {
				if ce.AtMs > at {
					break
				}
				if ce.Delete {
					effective[ce.NodeID] = 0
				} else if ce.CapacityBytes != nil {
					effective[ce.NodeID] = *ce.CapacityBytes
				}
			}
			var others int64
			for _, n := range p.Nodes {
				if n.Tier == node.Tier && n.ID != node.ID {
					others += effective[n.ID]
				}
			}
			target -= others
		}
		if target < 0 {
			return 0, biz(CodeCapacityNegative, "edited node capacity would be negative", map[string]any{
				"node_id": nodeID, "computed_capacity": target,
			})
		}
		v := target
		p.CapacityEvents = append(p.CapacityEvents, CapacityEvent{AtMs: at, NodeID: nodeID, CapacityBytes: &v})
		return at, nil
	case EditAppendEvent:
		if edit.Event.ObjectID == "" || edit.Event.AtMs < 0 || edit.Event.Count < 0 {
			return 0, biz(CodeEditInvalid, "invalid appended access event", nil)
		}
		if p.objectByID(edit.Event.ObjectID) == nil {
			return 0, biz(CodeObjectNotFound, "appended event object does not exist", map[string]any{"object_id": edit.Event.ObjectID})
		}
		p.AccessEvents = append(p.AccessEvents, edit.Event)
		return edit.Event.AtMs, nil
	case EditRemoveEvent:
		idx := edit.EventIndex
		if idx < 0 || idx >= len(p.AccessEvents) {
			for i, ev := range p.AccessEvents {
				if ev.AtMs == edit.Event.AtMs && ev.ObjectID == edit.Event.ObjectID {
					idx = i
					break
				}
			}
		}
		if idx < 0 || idx >= len(p.AccessEvents) {
			return 0, biz(CodeEditInvalid, "access event to remove was not found", map[string]any{
				"event_index": edit.EventIndex, "at_ms": edit.Event.AtMs, "object_id": edit.Event.ObjectID,
			})
		}
		at := p.AccessEvents[idx].AtMs
		p.AccessEvents = append(p.AccessEvents[:idx], p.AccessEvents[idx+1:]...)
		return at, nil
	default:
		return 0, biz(CodeEditInvalid, "unknown edit kind", map[string]any{"kind": edit.Kind})
	}
}

func editEarliest(edit Edit) (int64, error) {
	switch edit.Kind {
	case EditObjectAccess:
		if edit.AtMs < 0 {
			return 0, biz(CodeEditInvalid, "object edit time is negative", nil)
		}
		return edit.AtMs, nil
	case EditLayerCapacity:
		return edit.AtMs, nil
	case EditAppendEvent:
		return edit.Event.AtMs, nil
	case EditRemoveEvent:
		return edit.AtMs, nil
	default:
		return 0, biz(CodeEditInvalid, "unknown edit kind", map[string]any{"kind": edit.Kind})
	}
}

func editSource(edit Edit) string {
	switch edit.Kind {
	case EditObjectAccess:
		return "object:" + edit.ObjectID
	case EditLayerCapacity:
		if edit.NodeID != "" {
			return "node:" + edit.NodeID
		}
		return "layer:" + string(edit.Tier)
	case EditAppendEvent, EditRemoveEvent:
		return fmt.Sprintf("access_event:%s@%d", edit.Event.ObjectID, edit.Event.AtMs)
	default:
		return string(edit.Kind)
	}
}

func diffClosure(base, inc *SimulationResult, recomputed []int64) ([]string, []string, []string) {
	want := map[int64]struct{}{}
	for _, t := range recomputed {
		want[t] = struct{}{}
	}
	oldTicks := map[int64]TickResult{}
	for _, tick := range base.Ticks {
		oldTicks[tick.AtMs] = tick
	}
	objects := map[string]struct{}{}
	tiers := map[string]struct{}{}
	batches := map[string]struct{}{}
	for _, tick := range inc.Ticks {
		if _, ok := want[tick.AtMs]; !ok {
			continue
		}
		old, ok := oldTicks[tick.AtMs]
		if !ok || !objectTicksEqual(old.Objects, tick.Objects) {
			for _, obj := range tick.Objects {
				objects[obj.ObjectID] = struct{}{}
				tiers[string(obj.Tier)] = struct{}{}
				if obj.TargetTier != "" {
					tiers[string(obj.TargetTier)] = struct{}{}
				}
			}
		}
		for _, id := range tick.BatchesStarted {
			batches[id] = struct{}{}
		}
		for _, id := range tick.BatchesCommitted {
			batches[id] = struct{}{}
		}
	}
	oldBatches := map[string]BatchResult{}
	for _, b := range base.Batches {
		oldBatches[b.ID] = b
	}
	for _, b := range inc.Batches {
		old, ok := oldBatches[b.ID]
		if !ok || !batchEqual(old, b) {
			batches[b.ID] = struct{}{}
			tiers[string(b.Source)] = struct{}{}
			tiers[string(b.Target)] = struct{}{}
		}
	}
	return sortedKeys(objects), sortedKeys(tiers), sortedKeys(batches)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func objectTicksEqual(a, b []ObjectTick) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func batchEqual(a, b BatchResult) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func simulationEqual(a, b *SimulationResult) bool {
	ac := *a
	bc := *b
	ac.Ticks = append([]TickResult(nil), a.Ticks...)
	bc.Ticks = append([]TickResult(nil), b.Ticks...)
	for i := range ac.Ticks {
		ac.Ticks[i].LayerMetrics = nil
	}
	for i := range bc.Ticks {
		bc.Ticks[i].LayerMetrics = nil
	}
	aj, _ := json.Marshal(struct {
		Timeline []int64
		Ticks    []TickResult
		Batches  []BatchResult
		Final    Occupancy
	}{ac.Timeline, ac.Ticks, ac.Batches, ac.Final})
	bj, _ := json.Marshal(struct {
		Timeline []int64
		Ticks    []TickResult
		Batches  []BatchResult
		Final    Occupancy
	}{bc.Timeline, bc.Ticks, bc.Batches, bc.Final})
	return string(aj) == string(bj)
}
