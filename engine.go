package thermalcache

import (
	"math"
	"sort"
	"sync"
	"time"
)

type ObjectState struct {
	ID              string `json:"id"`
	Tier            Tier   `json:"tier"`
	AccessTotal     int64  `json:"access_total"`
	LastAccessMs    int64  `json:"last_access_ms"`
	LastAppliedStep int    `json:"last_applied_step"`
	Score           int64  `json:"score"`
	Expired         bool   `json:"expired"`
	Saturated       bool   `json:"saturated"`
	Overflowed      bool   `json:"overflowed"`
	SourceVersion   int64  `json:"source_version"`
	TargetVersion   int64  `json:"target_version"`
	PendingWrites   int64  `json:"pending_writes"`
	Migrating       bool   `json:"migrating"`
	TargetTier      Tier   `json:"target_tier,omitempty"`
	BatchID         string `json:"batch_id,omitempty"`
	GateDenials     int64  `json:"gate_denials"`
}

type Occupancy struct {
	HotCapacity  int64 `json:"hot_capacity"`
	ColdCapacity int64 `json:"cold_capacity"`
	HotUsed      int64 `json:"hot_used"`
	ColdUsed     int64 `json:"cold_used"`
	HotReserved  int64 `json:"hot_reserved"`
	ColdReserved int64 `json:"cold_reserved"`
}

func (o Occupancy) Used(t Tier) int64 {
	if t == TierHot {
		return o.HotUsed
	}
	return o.ColdUsed
}

func (o Occupancy) Capacity(t Tier) int64 {
	if t == TierHot {
		return o.HotCapacity
	}
	return o.ColdCapacity
}

type ObjectTick struct {
	ObjectID      string            `json:"object_id"`
	Score         int64             `json:"score"`
	RawScore      int64             `json:"raw_score"`
	AgeMs         int64             `json:"age_ms"`
	AccessTotal   int64             `json:"access_total"`
	LastAccessMs  int64             `json:"last_access_ms"`
	Tier          Tier              `json:"tier"`
	AnswerTier    Tier              `json:"answer_tier"`
	DesiredTier   Tier              `json:"desired_tier"`
	Direction     DecisionDirection `json:"direction"`
	Reason        ReasonCode        `json:"reason"`
	ReasonID      string            `json:"reason_id"`
	Threshold     int64             `json:"threshold"`
	Boundary      bool              `json:"boundary"`
	Expired       bool              `json:"expired"`
	Saturated     bool              `json:"saturated"`
	Overflowed    bool              `json:"overflowed"`
	Migrating     bool              `json:"migrating"`
	SourceTier    Tier              `json:"source_tier,omitempty"`
	TargetTier    Tier              `json:"target_tier,omitempty"`
	BatchID       string            `json:"batch_id,omitempty"`
	PendingWrites int64             `json:"pending_writes"`
	SourceVersion int64             `json:"source_version"`
	TargetVersion int64             `json:"target_version"`
	TargetVisible bool              `json:"target_visible"`
	GateDenials   int64             `json:"gate_denials"`
	DeferredCode  string            `json:"deferred_code,omitempty"`
}

type Backpressure struct {
	Layer      Tier   `json:"layer"`
	Code       string `json:"code"`
	Candidates int    `json:"candidates"`
	Limit      int    `json:"limit"`
}

type LayerMetric struct {
	Layer      Tier   `json:"layer"`
	Phase      string `json:"phase"`
	Objects    int    `json:"objects"`
	DurationNs int64  `json:"duration_ns"`
}

type TickResult struct {
	Index            int            `json:"index"`
	AtMs             int64          `json:"at_ms"`
	CommittedTick    int64          `json:"committed_tick"`
	Objects          []ObjectTick   `json:"objects"`
	BatchesStarted   []string       `json:"batches_started,omitempty"`
	BatchesCommitted []string       `json:"batches_committed,omitempty"`
	Gate             []GateResult   `json:"gate,omitempty"`
	Backpressure     []Backpressure `json:"backpressure,omitempty"`
	Occupancy        Occupancy      `json:"occupancy"`
	LayerMetrics     []LayerMetric  `json:"layer_metrics,omitempty"`
}

type SimulationResult struct {
	ProjectID     string        `json:"project_id"`
	SchemaVersion string        `json:"schema_version"`
	Fingerprint   string        `json:"fingerprint"`
	Timeline      []int64       `json:"timeline"`
	Ticks         []TickResult  `json:"ticks"`
	Batches       []BatchResult `json:"batches"`
	Final         Occupancy     `json:"final_occupancy"`
	Revision      int64         `json:"revision"`
	Finished      bool          `json:"finished"`
}

type Engine struct {
	mu               sync.Mutex
	project          *Project
	objects          map[string]*ObjectState
	objectOrder      []string
	batches          map[string]*Batch
	batchOrder       []string
	timeline         []int64
	nextTimeline     int
	committedTick    int64
	currentAtMs      int64
	revision         int64
	capacities       map[string]*runtimeCapacity
	reserved         map[Tier]int64
	batchSeq         int
	accessCursor     int
	opCursor         int
	capacityCursor   int
	migrationCursor  int
	finished         bool
	checkpointPath   string
	lastLayerMetrics []LayerMetric
	lastSolveMetrics []LayerMetric
	ticks            []TickResult
}

type runtimeCapacity struct {
	Bytes   int64
	Deleted bool
}

func NewEngine(p *Project) (*Engine, error) {
	if p == nil {
		return nil, biz(CodeSchemaInvalid, "project is nil", nil)
	}
	cp := p.Clone()
	if err := cp.Validate(); err != nil {
		return nil, err
	}
	canonicalize(cp)
	e := &Engine{
		project:    cp,
		objects:    make(map[string]*ObjectState, len(cp.Objects)),
		batches:    make(map[string]*Batch),
		capacities: make(map[string]*runtimeCapacity, len(cp.Nodes)),
		reserved:   map[Tier]int64{TierHot: 0, TierCold: 0},
	}
	for _, n := range cp.Nodes {
		e.capacities[n.ID] = &runtimeCapacity{Bytes: n.CapacityBytes}
	}
	for _, o := range cp.Objects {
		e.objects[o.ID] = &ObjectState{
			ID:              o.ID,
			Tier:            o.InitialTier,
			LastAccessMs:    o.LastAccessMs,
			LastAppliedStep: -1,
			TargetVersion:   0,
		}
		e.objectOrder = append(e.objectOrder, o.ID)
	}
	sort.Strings(e.objectOrder)
	e.timeline = buildTimeline(cp)
	return e, nil
}

func buildTimeline(p *Project) []int64 {
	seen := map[int64]struct{}{0: {}}
	add := func(v int64) {
		if v >= 0 {
			seen[v] = struct{}{}
		}
	}
	for _, o := range p.Objects {
		for i := range o.AccessCounts {
			add(int64(i) * p.TickMs)
		}
	}
	for _, ev := range p.AccessEvents {
		add(ev.AtMs)
	}
	for _, ev := range p.OpEvents {
		add(ev.AtMs)
	}
	for _, ev := range p.CapacityEvents {
		add(ev.AtMs)
	}
	for _, ev := range p.Migrations {
		add(ev.AtMs)
	}
	out := make([]int64, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (e *Engine) Project() *Project {
	return e.project.Clone()
}

func (e *Engine) Timeline() []int64 {
	return append([]int64(nil), e.timeline...)
}

func (e *Engine) EnableAutoCheckpoint(path string) {
	e.checkpointPath = path
}

func (e *Engine) Run() (*SimulationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished {
		return e.resultLocked(true), nil
	}
	for e.nextTimeline < len(e.timeline) {
		if _, err := e.stepLocked(); err != nil {
			return nil, err
		}
		if e.checkpointPath != "" {
			if err := e.saveCheckpointLocked(e.checkpointPath); err != nil {
				return nil, err
			}
		}
	}
	e.finished = true
	return e.resultLocked(true), nil
}

func (e *Engine) RunOne() (*TickResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nextTimeline >= len(e.timeline) {
		e.finished = true
		return nil, biz(CodeSchemaInvalid, "timeline is exhausted", nil)
	}
	tr, err := e.stepLocked()
	if err != nil {
		return nil, err
	}
	if e.checkpointPath != "" {
		if err := e.saveCheckpointLocked(e.checkpointPath); err != nil {
			return nil, err
		}
	}
	return tr, nil
}

// Finish seals an engine that was advanced with RunOne and returns the final
// result. It does not add ticks or rerun any prior computation.
func (e *Engine) Finish() (*SimulationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nextTimeline != len(e.timeline) {
		return nil, biz(CodeSchemaInvalid, "timeline has not been fully advanced", map[string]any{
			"next_timeline": e.nextTimeline, "timeline_len": len(e.timeline),
		})
	}
	e.finished = true
	return e.resultLocked(true), nil
}

// Result returns the current engine result without advancing time.
func (e *Engine) Result() *SimulationResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resultLocked(e.finished)
}

func (e *Engine) stepLocked() (*TickResult, error) {
	now := e.timeline[e.nextTimeline]
	e.nextTimeline++
	e.currentAtMs = now
	started, committed, err := e.commitDueLocked(now)
	if err != nil {
		return nil, err
	}
	if err := e.applyCapacityEventsLocked(now); err != nil {
		return nil, err
	}
	e.applyBaselineAccessLocked(now)
	e.applyAccessEventsLocked(now)

	forcedStarted, err := e.applyForcedMigrationsLocked(now)
	if err != nil {
		return nil, err
	}
	started = append(started, forcedStarted...)

	decisionByID, metrics, err := e.evaluateLayersLocked(now)
	if err != nil {
		return nil, err
	}
	backpressure, autoStarted, deferred, err := e.solveAutoLocked(now, decisionByID)
	if err != nil {
		return nil, err
	}
	started = append(started, autoStarted...)
	metrics = append(metrics, e.lastSolveMetrics...)
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Layer != metrics[j].Layer {
			return metrics[i].Layer < metrics[j].Layer
		}
		return metrics[i].Phase < metrics[j].Phase
	})
	e.lastLayerMetrics = metrics

	gateEvents, err := e.applyOpEventsLocked(now)
	if err != nil {
		return nil, err
	}

	if e.project.Limits.CommitDelayMs == 0 {
		extra, _, err := e.commitDueLocked(now)
		if err != nil {
			return nil, err
		}
		committed = append(committed, extra...)
	}

	sort.Strings(started)
	sort.Strings(committed)
	tr := e.tickResultLocked(now, e.nextTimeline-1, started, committed, gateEvents, backpressure, decisionByID, deferred)
	e.revision++
	e.committedTick = now
	tr.CommittedTick = now
	e.ticks = append(e.ticks, *tr)
	return tr, nil
}

func (e *Engine) applyBaselineAccessLocked(now int64) {
	globalStep := now / e.project.TickMs
	if globalStep < 0 {
		return
	}
	for _, id := range e.objectOrder {
		st := e.objects[id]
		obj := e.objectByID(id)
		if obj == nil {
			continue
		}
		limit := len(obj.AccessCounts) - 1
		if limit < 0 {
			if st.LastAppliedStep < 0 {
				st.LastAppliedStep = 0
			}
			continue
		}
		step := int(globalStep)
		if step > limit {
			step = limit
		}
		for s := st.LastAppliedStep + 1; s <= step; s++ {
			count := obj.AccessCounts[s]
			if count > 0 {
				st.AccessTotal, _ = saturatingAddUnsigned(st.AccessTotal, count, math.MaxInt64)
				at := int64(s) * e.project.TickMs
				if at > st.LastAccessMs {
					st.LastAccessMs = at
				}
			}
		}
		if step > st.LastAppliedStep {
			st.LastAppliedStep = step
		}
	}
}

func (e *Engine) applyAccessEventsLocked(now int64) {
	for e.accessCursor < len(e.project.AccessEvents) {
		ev := e.project.AccessEvents[e.accessCursor]
		if ev.AtMs > now {
			break
		}
		e.accessCursor++
		st := e.objects[ev.ObjectID]
		if st == nil || ev.Count == 0 {
			continue
		}
		st.AccessTotal, _ = saturatingAddUnsigned(st.AccessTotal, ev.Count, math.MaxInt64)
		if ev.AtMs > st.LastAccessMs {
			st.LastAccessMs = ev.AtMs
		}
	}
}

func (e *Engine) applyCapacityEventsLocked(now int64) error {
	for e.capacityCursor < len(e.project.CapacityEvents) {
		ev := e.project.CapacityEvents[e.capacityCursor]
		if ev.AtMs > now {
			break
		}
		e.capacityCursor++
		node := e.capacities[ev.NodeID]
		if node == nil {
			return biz(CodeNodeNotFound, "capacity event references unknown node", map[string]any{
				"node_id": ev.NodeID, "at_ms": ev.AtMs,
			})
		}
		if ev.Delete {
			node.Deleted = true
			node.Bytes = 0
			continue
		}
		if ev.CapacityBytes == nil {
			return biz(CodeSchemaInvalid, "capacity event has no capacity", map[string]any{"node_id": ev.NodeID})
		}
		if *ev.CapacityBytes < 0 {
			return biz(CodeCapacityNegative, "capacity event is negative", map[string]any{
				"node_id": ev.NodeID, "capacity_bytes": *ev.CapacityBytes,
			})
		}
		node.Bytes = *ev.CapacityBytes
		node.Deleted = false
	}
	return nil
}

func (e *Engine) objectByID(id string) *Object {
	for i := range e.project.Objects {
		if e.project.Objects[i].ID == id {
			return &e.project.Objects[i]
		}
	}
	return nil
}

func (e *Engine) evaluateLayersLocked(now int64) (map[string]Decision, []LayerMetric, error) {
	type layerJob struct {
		layer Tier
		ids   []string
	}
	jobs := []layerJob{{layer: TierHot}, {layer: TierCold}}
	for _, id := range e.objectOrder {
		st := e.objects[id]
		if st.Tier == TierHot {
			jobs[0].ids = append(jobs[0].ids, id)
		} else {
			jobs[1].ids = append(jobs[1].ids, id)
		}
	}
	decisions := make(map[string]Decision, len(e.objectOrder))
	var mu sync.Mutex
	var metrics []LayerMetric
	workers := e.project.Limits.WorkerBudget
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers < 1 {
		return nil, nil, biz(CodeThreadBudget, "no worker available", map[string]any{"worker_budget": workers})
	}
	work := make(chan layerJob)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range work {
				start := time.Now()
				local := make(map[string]Decision, len(job.ids))
				for _, id := range job.ids {
					st := e.objects[id]
					obj := e.objectByID(id)
					if obj == nil {
						continue
					}
					sr := ComputeScore(e.project.Score, st.AccessTotal, st.LastAccessMs, now, obj.TTLMs, e.project.TickMs)
					d := Decide(e.project.Score, st.Tier, sr, obj.Watermark)
					if st.Migrating {
						d = Decision{
							Direction: DirectionHold, Desired: st.TargetTier,
							Reason: ReasonMigrating, ReasonID: ReasonMigrating.String(),
						}
					}
					st.Score = sr.Score
					st.Expired = sr.Expired
					st.Saturated = sr.Saturated
					st.Overflowed = sr.Overflowed
					local[id] = d
				}
				mu.Lock()
				for id, d := range local {
					decisions[id] = d
				}
				metrics = append(metrics, LayerMetric{
					Layer: job.layer, Phase: "evaluate", Objects: len(job.ids),
					DurationNs: time.Since(start).Nanoseconds(),
				})
				mu.Unlock()
			}
		}()
	}
	for _, job := range jobs {
		work <- job
	}
	close(work)
	wg.Wait()
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Layer != metrics[j].Layer {
			return metrics[i].Layer < metrics[j].Layer
		}
		return metrics[i].Phase < metrics[j].Phase
	})
	return decisions, metrics, nil
}

func (e *Engine) layerCapacityLocked(tier Tier) int64 {
	var capacity int64
	for _, n := range e.project.Nodes {
		if n.Tier != tier {
			continue
		}
		rc := e.capacities[n.ID]
		if rc == nil || rc.Deleted {
			continue
		}
		if rc.Bytes > 0 && capacity > math.MaxInt64-rc.Bytes {
			return math.MaxInt64
		}
		capacity += rc.Bytes
	}
	return capacity
}

func (e *Engine) occupancyLocked() Occupancy {
	o := Occupancy{
		HotCapacity:  e.layerCapacityLocked(TierHot),
		ColdCapacity: e.layerCapacityLocked(TierCold),
		HotReserved:  e.reserved[TierHot],
		ColdReserved: e.reserved[TierCold],
	}
	for _, id := range e.objectOrder {
		st := e.objects[id]
		obj := e.objectByID(id)
		if obj == nil {
			continue
		}
		switch st.Tier {
		case TierHot:
			o.HotUsed += obj.SizeBytes
		case TierCold:
			o.ColdUsed += obj.SizeBytes
		}
	}
	return o
}

func (e *Engine) tickResultLocked(now int64, index int, started, committed []string, gate []GateResult, backpressure []Backpressure, decisions map[string]Decision, deferred map[string]string) *TickResult {
	tr := &TickResult{
		Index: index, AtMs: now, CommittedTick: e.committedTick,
		BatchesStarted: started, BatchesCommitted: committed,
		Gate: gate, Backpressure: backpressure,
		Occupancy:    e.occupancyLocked(),
		LayerMetrics: append([]LayerMetric(nil), e.lastLayerMetrics...),
	}
	for _, id := range e.objectOrder {
		st := e.objects[id]
		obj := e.objectByID(id)
		if obj == nil {
			continue
		}
		sr := ComputeScore(e.project.Score, st.AccessTotal, st.LastAccessMs, now, obj.TTLMs, e.project.TickMs)
		d := decisions[id]
		ot := ObjectTick{
			ObjectID: id, Score: sr.Score, RawScore: sr.RawScore, AgeMs: sr.AgeMs,
			AccessTotal: st.AccessTotal, LastAccessMs: st.LastAccessMs,
			Tier: st.Tier, AnswerTier: st.Tier, DesiredTier: d.Desired,
			Direction: d.Direction, Reason: d.Reason, ReasonID: d.ReasonID,
			Threshold: d.Threshold, Boundary: d.Boundary,
			Expired: sr.Expired, Saturated: sr.Saturated || st.Saturated,
			Overflowed: sr.Overflowed || st.Overflowed,
			Migrating:  st.Migrating, BatchID: st.BatchID,
			PendingWrites: st.PendingWrites, SourceVersion: st.SourceVersion,
			TargetVersion: st.TargetVersion, TargetVisible: !st.Migrating,
			GateDenials: st.GateDenials, DeferredCode: deferred[id],
		}
		if st.Migrating {
			ot.SourceTier = st.Tier
			ot.TargetTier = st.TargetTier
		}
		tr.Objects = append(tr.Objects, ot)
	}
	return tr
}

func (e *Engine) resultLocked(finished bool) *SimulationResult {
	out := &SimulationResult{
		ProjectID: e.project.ID, SchemaVersion: e.project.SchemaVersion,
		Fingerprint: e.project.Fingerprint(), Timeline: append([]int64(nil), e.timeline...),
		Final: e.occupancyLocked(), Revision: e.revision, Finished: finished,
		Ticks: append([]TickResult(nil), e.ticks...),
	}
	for _, id := range e.batchOrder {
		out.Batches = append(out.Batches, e.batches[id].snapshot(e.occupancyLocked()))
	}
	return out
}

func (e *Engine) State(objectID string) (ObjectState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.objects[objectID]
	if st == nil {
		return ObjectState{}, false
	}
	return *st, true
}

func (e *Engine) Occupancy() Occupancy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.occupancyLocked()
}
