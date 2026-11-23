package spatialpick

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
)

type state struct {
	locals         map[string]FixedMatrix
	localVersions  map[string]uint64
	worldVersions  map[string]uint64
	derived        map[string]*Derived
	geometries     map[string]*Geometry
	index          *Index
	revision       uint64
	indexVersion   uint64
	appliedBatches []string
	contentHash    string
}

func (s *state) clone() *state {
	c := &state{
		locals:         make(map[string]FixedMatrix, len(s.locals)),
		localVersions:  make(map[string]uint64, len(s.localVersions)),
		worldVersions:  make(map[string]uint64, len(s.worldVersions)),
		derived:        make(map[string]*Derived, len(s.derived)),
		geometries:     make(map[string]*Geometry, len(s.geometries)),
		index:          s.index,
		revision:       s.revision,
		indexVersion:   s.indexVersion,
		appliedBatches: append([]string(nil), s.appliedBatches...),
		contentHash:    s.contentHash,
	}
	for k, v := range s.locals {
		c.locals[k] = v
	}
	for k, v := range s.localVersions {
		c.localVersions[k] = v
	}
	for k, v := range s.worldVersions {
		c.worldVersions[k] = v
	}
	for k, v := range s.derived {
		c.derived[k] = v
	}
	for k, v := range s.geometries {
		c.geometries[k] = v
	}
	return c
}

type Engine struct {
	mu         sync.RWMutex
	st         *state
	scene      *Scene
	baseHash   string
	nodeByID   map[string]Node
	nodeIDs    []string
	children   map[string][]string
	topo       []string
	depths     map[string]int
	snapshotMu sync.Mutex
}

func NewEngine(scene *Scene) (*Engine, error) {
	if scene == nil {
		return nil, coded(CodeSchema, "new_engine", "nil scene")
	}
	baseHash, err := ComputeSceneHash(scene)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		scene:    CloneScene(scene),
		baseHash: baseHash,
		nodeByID: make(map[string]Node, len(scene.Nodes)),
		children: make(map[string][]string),
		depths:   make(map[string]int),
	}
	for _, n := range e.scene.Nodes {
		e.nodeByID[n.ID] = n
		e.nodeIDs = append(e.nodeIDs, n.ID)
	}
	sort.Strings(e.nodeIDs)
	for _, id := range e.nodeIDs {
		n := e.nodeByID[id]
		if n.ParentID != "" {
			e.children[n.ParentID] = append(e.children[n.ParentID], id)
		}
	}
	for id := range e.children {
		sort.Strings(e.children[id])
	}
	if err := e.computeTopology(); err != nil {
		return nil, err
	}
	s, err := e.buildInitialState()
	if err != nil {
		return nil, err
	}
	e.st = s
	return e, nil
}

func (e *Engine) computeTopology() error {
	var visit func(string, map[string]bool) error
	visit = func(id string, stack map[string]bool) error {
		if stack[id] {
			return &CodedError{Code: CodeCycle, Op: "topology", NodeID: id, Field: "parent_id", Message: "cycle detected"}
		}
		stack[id] = true
		n := e.nodeByID[id]
		if n.ParentID != "" {
			if _, ok := e.nodeByID[n.ParentID]; !ok {
				return &CodedError{Code: CodeRef, Op: "topology", NodeID: id, Field: "parent_id", Message: "unknown parent"}
			}
			if err := visit(n.ParentID, stack); err != nil {
				return err
			}
			e.depths[id] = e.depths[n.ParentID] + 1
		}
		delete(stack, id)
		return nil
	}
	for _, id := range e.nodeIDs {
		if err := visit(id, make(map[string]bool)); err != nil {
			return err
		}
	}
	e.topo = append([]string(nil), e.nodeIDs...)
	sort.SliceStable(e.topo, func(i, j int) bool {
		if e.depths[e.topo[i]] != e.depths[e.topo[j]] {
			return e.depths[e.topo[i]] < e.depths[e.topo[j]]
		}
		return e.topo[i] < e.topo[j]
	})
	for i, id := range e.topo {
		if e.depths[id] == 0 {
			continue
		}
		pi := -1
		for j := 0; j < i; j++ {
			if e.topo[j] == e.nodeByID[id].ParentID {
				pi = j
				break
			}
		}
		if pi < 0 {
			return &CodedError{Code: CodeCycle, Op: "topology", NodeID: id, Field: "parent_id", Message: "parent does not precede child"}
		}
	}
	return nil
}

func (e *Engine) buildInitialState() (*state, error) {
	s := &state{
		locals:         make(map[string]FixedMatrix, len(e.nodeIDs)),
		localVersions:  make(map[string]uint64, len(e.nodeIDs)),
		worldVersions:  make(map[string]uint64, len(e.nodeIDs)),
		derived:        make(map[string]*Derived, len(e.nodeIDs)),
		geometries:     make(map[string]*Geometry, len(e.nodeIDs)),
		indexVersion:   1,
		revision:       0,
		appliedBatches: []string{},
	}
	for _, id := range e.nodeIDs {
		n := e.nodeByID[id]
		fm, err := QuantizeMatrix(Mat4(n.LocalMatrix), e.scene.Precision)
		if err != nil {
			var ce *CodedError
			if IsCoded(err, &ce) {
				ce.NodeID = id
			}
			return nil, err
		}
		s.locals[id] = fm
		s.localVersions[id] = 0
		s.worldVersions[id] = 1
		if n.GeometryIndex >= 0 {
			s.geometries[id] = cloneGeometry(&e.scene.Geometries[n.GeometryIndex])
		}
	}
	for _, id := range e.topo {
		n := e.nodeByID[id]
		var parent *Derived
		if n.ParentID != "" {
			parent = s.derived[n.ParentID]
		}
		var g *Geometry
		if g = s.geometries[id]; g != nil {
		}
		d, err := BuildDerived(&n, s.locals[id], 0, 1, parent, g, e.scene.Precision)
		if err != nil {
			return nil, err
		}
		s.derived[id] = d
	}
	var entries []IndexEntry
	for _, id := range e.nodeIDs {
		g := s.geometries[id]
		if g == nil {
			continue
		}
		entry := MakeIndexEntry(s.derived[id], g, e.scene.Precision)
		if entry.NodeID != id {
			return nil, coded(CodeSchema, "new_engine", "index entry node mismatch")
		}
		entries = append(entries, entry)
	}
	s.index = BuildIndex(entries, s.indexVersion)
	s.contentHash = ComputeStateHashWithGeometry(e.baseHash, e.scene.Precision, s.locals, s.appliedBatches, s.geometries)
	return s, nil
}

func (e *Engine) currentState() *state {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st
}

func (e *Engine) Revision() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.revision
}

func (e *Engine) IndexVersion() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.indexVersion
}

func (e *Engine) ContentHash() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.contentHash
}

func (e *Engine) AppliedBatches() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.st.appliedBatches...)
}

func (e *Engine) Index() *Index {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.st.index
}

func (e *Engine) GetDerived(nodeID string, expectedVersion uint64) (*Derived, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.st.derived[nodeID]
	if !ok {
		return nil, &CodedError{Code: CodeRef, Op: "get_derived", NodeID: nodeID, Field: "node_id", Message: "unknown node"}
	}
	if expectedVersion != 0 && d.Version != expectedVersion {
		return nil, &CodedError{Code: CodeStale, Op: "get_derived", NodeID: nodeID, ExpectedVersion: expectedVersion, HasExpectedVersion: true, Message: "derived cache version does not match expected version"}
	}
	if d.Failed {
		return nil, &CodedError{Code: CodeStale, Op: "get_derived", NodeID: nodeID, ExpectedVersion: d.Version, HasExpectedVersion: true, Message: "derived cache entry failed; retry rebuild"}
	}
	return d, nil
}

func (e *Engine) ApplyLocalTransforms(plan *InvalidationPlan, changes map[string]FixedMatrix) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if plan == nil {
		return nil, coded(CodeDependency, "apply", "nil invalidation plan")
	}
	if plan.BaseRevision != e.st.revision || plan.BaseIndexVersion != e.st.indexVersion {
		return nil, &CodedError{Code: CodeTransaction, Op: "apply", ExpectedVersion: plan.BaseRevision, HasExpectedVersion: true, Message: "invalidation plan is based on a stale scene revision"}
	}
	if err := e.validatePlan(plan, changes, e.st.geometries); err != nil {
		return nil, err
	}
	return e.applyPlanLocked(plan, changes, "")
}

func (e *Engine) applyPlanLocked(plan *InvalidationPlan, changes map[string]FixedMatrix, batchID string) (*InvalidationPlan, error) {
	next := e.st.clone()
	for id, m := range changes {
		if !m.IsAffine(e.scene.Precision) {
			return nil, &CodedError{Code: CodeTransaction, Op: "commit_batch", BatchID: batchID, NodeID: id, Field: "local_matrix", Message: "changed local matrix is not affine"}
		}
		next.locals[id] = m
		next.localVersions[id]++
	}
	for _, id := range plan.AffectedNodes {
		next.worldVersions[id]++
	}
	for _, id := range e.topo {
		if _, ok := changes[id]; !ok {
			if !containsString(plan.AffectedNodes, id) {
				continue
			}
		}
		if !containsString(plan.AffectedNodes, id) {
			continue
		}
		n := e.nodeByID[id]
		var parent *Derived
		if n.ParentID != "" {
			var ok bool
			parent, ok = next.derived[n.ParentID]
			if !ok {
				return nil, &CodedError{Code: CodeTransaction, Op: "commit_batch", BatchID: batchID, NodeID: id, Field: "parent", Message: "parent derived cache is unavailable"}
			}
		}
		g := next.geometries[id]
		d, err := BuildDerived(&n, next.locals[id], next.localVersions[id], next.worldVersions[id], parent, g, e.scene.Precision)
		if err != nil {
			var ce *CodedError
			if IsCoded(err, &ce) {
				ce.Code = CodeTransaction
				ce.BatchID = batchID
				ce.NodeID = id
			}
			return nil, err
		}
		next.derived[id] = d
	}
	changedEntries := make(map[string]IndexEntry)
	for _, id := range plan.AffectedNodes {
		g := next.geometries[id]
		if g == nil {
			continue
		}
		changedEntries[id] = MakeIndexEntry(next.derived[id], g, e.scene.Precision)
	}
	next.revision++
	next.indexVersion++
	if len(changedEntries) > 0 {
		next.index = UpdateIndexEntries(next.index, changedEntries, next.indexVersion)
	} else {
		next.index = &Index{Version: next.indexVersion, Entries: next.index.Entries, Root: next.index.Root, MaxErrorBound: next.index.MaxErrorBound}
	}
	if batchID != "" {
		next.appliedBatches = append(next.appliedBatches, batchID)
	}
	next.contentHash = ComputeStateHashWithGeometry(e.baseHash, e.scene.Precision, next.locals, next.appliedBatches, next.geometries)
	e.st = next
	out := *plan
	out.BaseRevision = next.revision
	out.BaseIndexVersion = next.indexVersion
	return &out, nil
}

func containsString(list []string, want string) bool {
	i := sort.SearchStrings(list, want)
	return i < len(list) && list[i] == want
}

func (e *Engine) PickRay(req RayQuery) (RayPickResult, error) {
	idx := e.Index()
	res, err := idx.PickRay(req, e.scene.Camera)
	if err != nil {
		res.ErrorCode = errorCode(err)
	}
	return res, err
}

func (e *Engine) SelectBox(req BoxQuery) (BoxSelectResult, error) {
	idx := e.Index()
	res, err := idx.SelectBox(req, e.scene.Camera)
	if err != nil {
		res.ErrorCode = errorCode(err)
	}
	return res, err
}

func (e *Engine) ContainsPoint(req PointQuery) (PointSelectResult, error) {
	idx := e.Index()
	res, err := idx.ContainsPoint(req, e.scene.Camera)
	if err != nil {
		res.ErrorCode = errorCode(err)
	}
	return res, err
}

func errorCode(err error) string {
	var ce *CodedError
	if IsCoded(err, &ce) {
		return ce.Code
	}
	return ""
}

type BatchPlan struct {
	BatchID          string
	BaseRevision     uint64
	BaseIndexVersion uint64
	Selection        []string
	NewLocals        map[string]FixedMatrix
	Invalidation     *InvalidationPlan
}

func (e *Engine) PlanPivotBatch(batch EditBatch) (*BatchPlan, error) {
	s := e.currentState()
	if batch.ID == "" {
		return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", Field: "batch.id", Message: "batch id must not be empty"}
	}
	if len(batch.Selection) == 0 {
		return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, Field: "selection", Message: "selection must not be empty"}
	}
	if !batch.Pivot.IsFinite() {
		return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, Field: "pivot", Message: "pivot must be finite"}
	}
	selSeen := make(map[string]struct{}, len(batch.Selection))
	for _, id := range batch.Selection {
		if _, ok := e.nodeByID[id]; !ok {
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, NodeID: id, Field: "selection", Message: "unknown selected node"}
		}
		if _, dup := selSeen[id]; dup {
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, NodeID: id, Field: "selection", Message: "duplicate selected node"}
		}
		selSeen[id] = struct{}{}
	}
	delta := IdentityMat4()
	for _, step := range batch.Steps {
		var local Mat4
		switch step.Kind {
		case StepTranslate:
			local = TranslationMat4(step.Value)
		case StepRotate:
			local = Mat4Multiply(TranslationMat4(batch.Pivot), Mat4Multiply(RotationEulerDegrees(step.Value), TranslationMat4(batch.Pivot.Mul(-1))))
		case StepScale:
			if step.Value.X == 0 || step.Value.Y == 0 || step.Value.Z == 0 {
				return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, Field: "steps.value", Message: "scale component must not be zero"}
			}
			local = Mat4Multiply(TranslationMat4(batch.Pivot), Mat4Multiply(ScaleMat4(step.Value), TranslationMat4(batch.Pivot.Mul(-1))))
		default:
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, Field: "steps.kind", Message: "unknown pivot step kind"}
		}
		delta = Mat4Multiply(local, delta)
	}
	newLocals := make(map[string]FixedMatrix, len(batch.Selection))
	for _, id := range batch.Selection {
		d := s.derived[id]
		if d == nil || d.Failed {
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, NodeID: id, Message: "selected node derived cache is unavailable"}
		}
		oldWorld := d.WorldFloat
		newWorld := Mat4Multiply(delta, oldWorld)
		parentWorld := IdentityMat4()
		if n := e.nodeByID[id]; n.ParentID != "" {
			parentWorld = s.derived[n.ParentID].WorldFloat
		}
		parentInv, ok := Mat4InverseAffine(parentWorld)
		if !ok {
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, NodeID: id, Message: "parent world transform is singular"}
		}
		newLocal := Mat4Multiply(parentInv, newWorld)
		fm, err := QuantizeMatrix(newLocal, e.scene.Precision)
		if err != nil {
			return nil, &CodedError{Code: CodeTransaction, Op: "plan_batch", BatchID: batch.ID, NodeID: id, Cause: err}
		}
		newLocals[id] = fm
	}
	inv, err := e.PlanTransformInvalidation(batch.Selection, "batch:"+batch.ID)
	if err != nil {
		return nil, err
	}
	return &BatchPlan{
		BatchID:          batch.ID,
		BaseRevision:     s.revision,
		BaseIndexVersion: s.indexVersion,
		Selection:        append([]string(nil), batch.Selection...),
		NewLocals:        newLocals,
		Invalidation:     inv,
	}, nil
}

func (e *Engine) CommitBatch(plan *BatchPlan) (*InvalidationPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if plan == nil {
		return nil, coded(CodeTransaction, "commit_batch", "nil batch plan")
	}
	if plan.BaseRevision != e.st.revision || plan.BaseIndexVersion != e.st.indexVersion {
		obj := ""
		if len(plan.Selection) > 0 {
			obj = plan.Selection[0]
		}
		return nil, &CodedError{Code: CodeTransaction, Op: "commit_batch", BatchID: plan.BatchID, ObjectID: obj, ExpectedVersion: plan.BaseRevision, HasExpectedVersion: true, Message: "batch base revision is stale; entire batch rolled back"}
	}
	if len(plan.NewLocals) != len(plan.Selection) {
		return nil, &CodedError{Code: CodeTransaction, Op: "commit_batch", BatchID: plan.BatchID, Field: "new_locals", Message: "batch working copy does not match selection size"}
	}
	for _, id := range plan.Selection {
		if _, ok := plan.NewLocals[id]; !ok {
			return nil, &CodedError{Code: CodeTransaction, Op: "commit_batch", BatchID: plan.BatchID, NodeID: id, Field: "new_locals", Message: "selected node is missing from batch working copy"}
		}
	}
	if err := e.validatePlan(plan.Invalidation, plan.NewLocals, e.st.geometries); err != nil {
		var ce *CodedError
		if IsCoded(err, &ce) {
			ce.Code = CodeTransaction
			ce.BatchID = plan.BatchID
		}
		return nil, err
	}
	return e.applyPlanLocked(plan.Invalidation, plan.NewLocals, plan.BatchID)
}

func (e *Engine) ApplyPivotBatch(batch EditBatch) (*InvalidationPlan, error) {
	p, err := e.PlanPivotBatch(batch)
	if err != nil {
		return nil, err
	}
	return e.CommitBatch(p)
}

func (e *Engine) ApplyAllBatches() error {
	for _, batch := range e.scene.EditSequence {
		if _, err := e.ApplyPivotBatch(batch); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Bundle() ResultBundle {
	e.mu.RLock()
	s := e.st
	scene := e.scene
	e.mu.RUnlock()
	b := ResultBundle{
		SceneVersion: scene.SceneVersion,
		Revision:     s.revision,
		Precision:    scene.Precision,
	}
	ids := append([]string(nil), e.nodeIDs...)
	for _, id := range ids {
		d := s.derived[id]
		nr := NodeResult{
			NodeID: id, Depth: d.Depth, WorldRaw: d.World, World: d.WorldFloat,
			AABB: d.AABB, Sphere: d.Sphere, Version: d.Version,
			GeometryID: d.GeometryID, ErrorBound: d.GeometryError,
		}
		b.Nodes = append(b.Nodes, nr)
	}
	for _, req := range scene.RayQueries {
		res, err := s.index.PickRay(req, scene.Camera)
		if err != nil {
			res.ErrorCode = errorCode(err)
		}
		b.Rays = append(b.Rays, res)
	}
	for _, req := range scene.BoxQueries {
		res, err := s.index.SelectBox(req, scene.Camera)
		if err != nil {
			res.ErrorCode = errorCode(err)
		}
		b.Boxes = append(b.Boxes, res)
	}
	for _, req := range scene.PointQueries {
		res, err := s.index.ContainsPoint(req, scene.Camera)
		if err != nil {
			res.ErrorCode = errorCode(err)
		}
		b.Points = append(b.Points, res)
	}
	return b
}

type RebuildOptions struct {
	MaxParallel     int
	MaxScratchBytes int64
	Fault           func(nodeID string) error
}

type RebuildReport struct {
	Rebuilt      []string `json:"rebuilt"`
	Failed       []string `json:"failed"`
	MaxParallel  int      `json:"max_parallel"`
	ScratchBytes int64    `json:"scratch_bytes"`
}

// RebuildSubtree recomputes selected nodes and descendants. Depth levels run in
// parallel under a hard worker and scratch-memory limit. A failed node is
// marked independently and leaves all other entries usable.
func (e *Engine) RebuildSubtree(ctx context.Context, nodeIDs []string, opts RebuildOptions) (RebuildReport, error) {
	e.mu.RLock()
	base := e.st
	e.mu.RUnlock()
	changed, affected, err := e.affectedClosure(nodeIDs)
	if err != nil {
		return RebuildReport{}, err
	}
	_ = changed
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = runtime.NumCPU()
	}
	if opts.MaxParallel > 32 {
		opts.MaxParallel = 32
	}
	if opts.MaxScratchBytes <= 0 {
		opts.MaxScratchBytes = 64 << 20
	}
	var scratch int64
	for _, id := range affected {
		if g := base.geometries[id]; g != nil {
			scratch += int64(len(g.Vertices)*3*8 + len(g.Triangles)*3*8 + 256)
		} else {
			scratch += 256
		}
	}
	if scratch > opts.MaxScratchBytes {
		return RebuildReport{}, &CodedError{Code: CodeResource, Op: "rebuild", Message: fmt.Sprintf("scratch estimate %d exceeds limit %d", scratch, opts.MaxScratchBytes)}
	}
	next := base.clone()
	affectedSet := make(map[string]struct{}, len(affected))
	for _, id := range affected {
		affectedSet[id] = struct{}{}
	}
	byDepth := make(map[int][]string)
	for _, id := range affected {
		byDepth[e.depths[id]] = append(byDepth[e.depths[id]], id)
	}
	depths := make([]int, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	report := RebuildReport{MaxParallel: opts.MaxParallel, ScratchBytes: scratch}
	failedSet := make(map[string]struct{})
	for _, depth := range depths {
		level := byDepth[depth]
		sort.Strings(level)
		type result struct {
			id  string
			d   *Derived
			err error
		}
		results := make([]result, len(level))
		sem := make(chan struct{}, opts.MaxParallel)
		var wg sync.WaitGroup
		for i, id := range level {
			n := e.nodeByID[id]
			if n.ParentID != "" {
				p := next.derived[n.ParentID]
				if p == nil || p.Failed {
					failed := *next.derived[id]
					failed.Failed = true
					failed.Failure = "parent_rebuild_failed"
					failed.ExpectedVersion = failed.Version
					next.derived[id] = &failed
					results[i] = result{id: id, err: fmt.Errorf("parent failed")}
					failedSet[id] = struct{}{}
					continue
				}
			}
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results[i] = result{id: id, err: ctx.Err()}
					return
				}
				if opts.Fault != nil {
					if err := opts.Fault(id); err != nil {
						results[i] = result{id: id, err: err}
						return
					}
				}
				n := e.nodeByID[id]
				var parent *Derived
				if n.ParentID != "" {
					parent = next.derived[n.ParentID]
					if parent == nil || parent.Failed {
						results[i] = result{id: id, err: fmt.Errorf("parent failed")}
						return
					}
				}
				g := next.geometries[id]
				d, err := BuildDerived(&n, next.locals[id], next.localVersions[id], next.worldVersions[id], parent, g, e.scene.Precision)
				results[i] = result{id: id, d: d, err: err}
			}(i, id)
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return RebuildReport{}, err
		}
		for _, r := range results {
			if r.err != nil {
				if d := next.derived[r.id]; d != nil {
					failed := *d
					failed.Failed = true
					failed.Failure = r.err.Error()
					failed.ExpectedVersion = failed.Version
					next.derived[r.id] = &failed
				}
				failedSet[r.id] = struct{}{}
				continue
			}
			if r.d != nil {
				r.d.Failed = false
				r.d.Failure = ""
				r.d.ExpectedVersion = 0
				next.derived[r.id] = r.d
				report.Rebuilt = append(report.Rebuilt, r.id)
			}
		}
	}
	report.Failed = sortedStringsFromSet(failedSet)
	sort.Strings(report.Rebuilt)
	next.revision++
	next.indexVersion++
	changedEntries := make(map[string]IndexEntry)
	for id := range affectedSet {
		g := next.geometries[id]
		if g == nil {
			continue
		}
		changedEntries[id] = MakeIndexEntry(next.derived[id], g, e.scene.Precision)
	}
	if len(changedEntries) > 0 {
		next.index = UpdateIndexEntries(next.index, changedEntries, next.indexVersion)
	} else {
		next.index = &Index{Version: next.indexVersion, Entries: next.index.Entries, Root: next.index.Root, MaxErrorBound: next.index.MaxErrorBound}
	}
	next.contentHash = ComputeStateHashWithGeometry(e.baseHash, e.scene.Precision, next.locals, next.appliedBatches, next.geometries)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st != base {
		return RebuildReport{}, &CodedError{Code: CodeTransaction, Op: "rebuild", Message: "scene changed during rebuild; retry against the new revision"}
	}
	e.st = next
	return report, nil
}

func (e *Engine) NodeIDs() []string { return append([]string(nil), e.nodeIDs...) }

func (e *Engine) Scene() *Scene { return CloneScene(e.scene) }

func CloneScene(scene *Scene) *Scene {
	if scene == nil {
		return nil
	}
	c := *scene
	c.Nodes = append([]Node(nil), scene.Nodes...)
	c.Geometries = make([]Geometry, len(scene.Geometries))
	for i := range scene.Geometries {
		c.Geometries[i] = *cloneGeometry(&scene.Geometries[i])
	}
	c.RayQueries = append([]RayQuery(nil), scene.RayQueries...)
	c.BoxQueries = append([]BoxQuery(nil), scene.BoxQueries...)
	c.PointQueries = append([]PointQuery(nil), scene.PointQueries...)
	c.EditSequence = make([]EditBatch, len(scene.EditSequence))
	for i, batch := range scene.EditSequence {
		c.EditSequence[i] = batch
		c.EditSequence[i].Selection = append([]string(nil), batch.Selection...)
		c.EditSequence[i].Steps = append([]PivotStep(nil), batch.Steps...)
	}
	return &c
}
