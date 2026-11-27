package terrainbake

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

type EngineConfig struct {
	MaxParallelism       int   `json:"max_parallelism"`
	MaxVerticesPerThread int   `json:"max_vertices_per_thread"`
	MeshMemoryBudget     int64 `json:"mesh_memory_budget"`
}

func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		MaxParallelism:       3,
		MaxVerticesPerThread: 4096,
		MeshMemoryBudget:     8 << 20,
	}
}

func (c EngineConfig) validate() error {
	if c.MaxParallelism < 1 || c.MaxParallelism > 256 {
		return errPara("max_parallelism", "[1,256]", "worker count is outside the legal interval")
	}
	if c.MaxVerticesPerThread < 16 || c.MaxVerticesPerThread > 1_000_000 {
		return errPara("max_vertices_per_thread", "[16,1000000]", "per-thread vertex buffer cap is outside the legal interval")
	}
	if c.MeshMemoryBudget < 64<<10 || c.MeshMemoryBudget > 1<<40 {
		return errPara("mesh_memory_budget", "[65536,1099511627776]", "mesh memory budget is outside the legal interval")
	}
	return nil
}

type Edit struct {
	BlockID       string  `json:"block_id"`
	HeightDeltaMM *int64  `json:"height_delta_mm,omitempty"`
	Budget        *Budget `json:"budget,omitempty"`
	ToleranceUM   *int64  `json:"tolerance_um,omitempty"`
}

type Invalidation struct {
	BlockID string `json:"block_id"`
	Reason  string `json:"reason"`
	Source  string `json:"source,omitempty"`
	Via     string `json:"via,omitempty"`
	Field   string `json:"field,omitempty"`
}

type QueueEvent struct {
	BlockID        string `json:"block_id"`
	Reason         string `json:"reason"`
	RequestedBytes int64  `json:"requested_bytes"`
	WaitMS         int64  `json:"wait_ms"`
	Acquired       bool   `json:"acquired"`
}

type BudgetUsage struct {
	MaxParallelism int   `json:"max_parallelism"`
	MemoryBudget   int64 `json:"memory_budget"`
	PeakInUse      int64 `json:"peak_in_use"`
	QueuedCount    int   `json:"queued_count"`
	TotalWaitMS    int64 `json:"total_wait_ms"`
}

type BakeResult struct {
	Blocks         []string          `json:"blocks"`
	Meshes         map[string]*Mesh  `json:"meshes"`
	Seams          []SeamReport      `json:"seams"`
	Invalidations  []Invalidation    `json:"invalidations"`
	Invalidated    []string          `json:"invalidated"`
	Reused         []string          `json:"reused"`
	Recomputed     []string          `json:"recomputed"`
	WrittenBlocks  []string          `json:"written_blocks"`
	CommitSequence []string          `json:"commit_sequence"`
	Queue          []QueueEvent      `json:"queue"`
	Budget         BudgetUsage       `json:"budget"`
	BlockErrors    map[string]*Error `json:"block_errors,omitempty"`
	TruthCheck     *TruthCheck       `json:"truth_check,omitempty"`
	AttributionErr *Error            `json:"attribution_error,omitempty"`
	ReusedCount    int               `json:"reused_count"`
	Config         EngineConfig      `json:"config"`
}

func (r *BakeResult) BlockErrorIDs() []string {
	ids := make([]string, 0, len(r.BlockErrors))
	for id := range r.BlockErrors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type Engine struct {
	mu      sync.Mutex
	project *Project
	gen     *Generator
	config  EngineConfig
	cache   *CheckpointStore
	states  map[string]BlockState
	invalid map[string]Invalidation
	order   []string
	adjVer  string
}

func NewEngine(p *Project, cfg EngineConfig, cacheDir string) (*Engine, error) {
	if p == nil {
		return nil, errSchema("project", "nil project")
	}
	if err := p.ValidateTopology(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cache, err := OpenCheckpoint(cacheDir, p)
	if err != nil {
		return nil, err
	}
	states, _ := cache.StateSnapshot()
	e := &Engine{
		project: p.Clone(),
		gen:     NewGenerator(p.World.Seed),
		config:  cfg,
		cache:   cache,
		states:  map[string]BlockState{},
		invalid: map[string]Invalidation{},
		order:   dependencyOrder(p),
		adjVer:  adjacencyVersion(p),
	}
	for id, st := range states {
		if _, ok := p.BlockByID(id); ok {
			st.Fault = ""
			e.states[id] = st
		}
	}
	return e, nil
}

func (e *Engine) Project() *Project { return e.project.Clone() }

func (e *Engine) State(blockID string) BlockState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.states[blockID].clone()
}

func (e *Engine) States() map[string]BlockState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]BlockState, len(e.states))
	for k, v := range e.states {
		out[k] = v.clone()
	}
	return out
}

func (e *Engine) SetFault(blockID, fault string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.project.BlockByID(blockID); !ok {
		return errAdj("edit.block_id", "block_id", "unknown block "+blockID)
	}
	st := e.states[blockID]
	st.Fault = fault
	e.states[blockID] = st
	e.invalid[blockID] = Invalidation{BlockID: blockID, Reason: "fault_injection", Field: "fault"}
	return nil
}

func (e *Engine) ApplyEdit(edit Edit) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.project.BlockByID(edit.BlockID)
	if !ok {
		return nil, errAdj("edit.block_id", "block_id", "unknown block "+edit.BlockID)
	}
	st := e.states[edit.BlockID]
	var fields []string
	changed := false
	if edit.HeightDeltaMM != nil && st.HeightDeltaMM != *edit.HeightDeltaMM {
		st.HeightDeltaMM = *edit.HeightDeltaMM
		fields = append(fields, "height")
		changed = true
	}
	if edit.Budget != nil {
		if edit.Budget.MaxVertices <= 0 || edit.Budget.MaxTriangles <= 0 {
			return nil, errPara("budget", "max_vertices>0,max_triangles>0", "invalid budget override")
		}
		if st.Budget == nil || *st.Budget != *edit.Budget {
			cp := *edit.Budget
			st.Budget = &cp
			fields = append(fields, "budget")
			changed = true
		}
	}
	if edit.ToleranceUM != nil && (st.ToleranceUM == nil || *st.ToleranceUM != *edit.ToleranceUM) {
		if *edit.ToleranceUM <= 0 {
			return nil, errPara("tolerance_um", ">0", "invalid tolerance override")
		}
		v := *edit.ToleranceUM
		st.ToleranceUM = &v
		fields = append(fields, "tolerance")
		changed = true
	}
	if !changed {
		return nil, nil
	}
	e.states[edit.BlockID] = st
	var out []Invalidation
	reason := "edit:" + joinStrings(fields, "+")
	iv := Invalidation{BlockID: edit.BlockID, Reason: reason, Source: edit.BlockID, Field: joinStrings(fields, ",")}
	out = append(out, iv)
	e.invalid[edit.BlockID] = iv
	for _, n := range e.project.AdjacentBlocks(edit.BlockID) {
		niv := Invalidation{
			BlockID: n,
			Reason:  "seam_realignment",
			Source:  edit.BlockID,
			Via:     EdgeID(edit.BlockID, n),
			Field:   joinStrings(fields, ","),
		}
		out = append(out, niv)
		e.invalid[n] = niv
	}
	if err := e.cache.SaveEdits(e.states); err != nil {
		return nil, &Error{Code: CodeBlock, BlockID: edit.BlockID, Field: "checkpoint", Message: err.Error()}
	}
	_ = b
	return out, nil
}

func joinStrings(in []string, sep string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func (e *Engine) Bake(ctx context.Context) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bakeLocked(ctx, false)
}

func (e *Engine) BakeFull(ctx context.Context) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bakeLocked(ctx, true)
}

func (e *Engine) effectiveBlock(id string) (BlockSpec, BlockState) {
	b, _ := e.project.BlockByID(id)
	spec := *b
	st := e.states[id].clone()
	if st.Budget != nil {
		spec.Budget = *st.Budget
	}
	if st.ToleranceUM != nil {
		spec.ToleranceUM = *st.ToleranceUM
	}
	return spec, st
}

func (e *Engine) localHeight(id string) MeshHeightFunc {
	spec, st := e.effectiveBlock(id)
	return func(x, y int64) int64 {
		return e.gen.HeightMM(spec.ID, spec.Rect, st.HeightDeltaMM, x, y)
	}
}

func (e *Engine) boundaryHeight(x, y int64) int64 {
	return e.gen.BaseHeightMM(x, y)
}

type jobResult struct {
	id    string
	mesh  *Mesh
	err   *Error
	queue QueueEvent
}

func (e *Engine) bakeLocked(ctx context.Context, forceAll bool) (*BakeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &Error{Code: CodeBlock, Field: "context", Message: err.Error()}
	}
	result := &BakeResult{
		Blocks:      append([]string(nil), e.order...),
		Meshes:      map[string]*Mesh{},
		BlockErrors: map[string]*Error{},
		Config:      e.config,
		Budget: BudgetUsage{
			MaxParallelism: e.config.MaxParallelism,
			MemoryBudget:   e.config.MeshMemoryBudget,
		},
	}
	specs := map[string]BlockSpec{}
	states := map[string]BlockState{}
	contents := map[string]string{}
	params := map[string]string{}
	for _, id := range e.order {
		spec, st := e.effectiveBlock(id)
		if err := validateMeshParameters(spec); err != nil {
			result.BlockErrors[id] = asError(err)
			continue
		}
		specs[id] = spec
		states[id] = st
		contents[id] = contentHash(spec, e.localHeight(id))
		params[id] = parameterVersion(spec, e.project.Seams)
	}
	keys := map[string]string{}
	baseKeys := map[string]string{}
	upstreams := map[string][]string{}
	for _, id := range e.order {
		if _, ok := specs[id]; !ok {
			continue
		}
		baseKeys[id] = cacheKey(contents[id], e.adjVer, params[id], AlgorithmVersion, nil)
	}
	for _, id := range e.order {
		if _, ok := specs[id]; !ok {
			continue
		}
		deps := append([]string(nil), specs[id].Neighbors...)
		sort.Strings(deps)
		var upstream []string
		for _, dep := range deps {
			// Upstream keys are the direct neighbour's content/parameter
			// cache key. Using the fully composed key here would create a
			// transitive invalidation chain and re-bake blocks that are not
			// adjacent to the edit.
			upstream = append(upstream, baseKeys[dep])
		}
		upstreams[id] = append([]string(nil), upstream...)
		keys[id] = cacheKey(contents[id], e.adjVer, params[id], AlgorithmVersion, upstream)
	}

	reusedSeams := []SeamReport{}
	jobs := make([]string, 0)
	for _, id := range e.order {
		spec, ok := specs[id]
		if !ok {
			continue
		}
		if !forceAll {
			if _, marked := e.invalid[id]; !marked {
				if rec, ok := e.cache.Record(id); ok && e.cache.ValidateRecord(rec, keys[id], contents[id], e.adjVer, params[id]) {
					result.Meshes[id] = rec.Mesh
					result.Reused = append(result.Reused, id)
					reusedSeams = append(reusedSeams, rec.Seams...)
					continue
				}
			}
		}
		if rec, ok := e.cache.Record(id); ok {
			_ = rec
			if iv, marked := e.invalid[id]; marked {
				result.Invalidations = append(result.Invalidations, iv)
			} else {
				result.Invalidations = append(result.Invalidations, Invalidation{BlockID: id, Reason: "cache_invalid", Field: "cache"})
			}
		} else if iv, marked := e.invalid[id]; marked {
			result.Invalidations = append(result.Invalidations, iv)
		} else {
			result.Invalidations = append(result.Invalidations, Invalidation{BlockID: id, Reason: "cache_miss", Field: "cache"})
		}
		jobs = append(jobs, id)
		result.Recomputed = append(result.Recomputed, id)
		_ = spec
	}
	result.Invalidated = invalidatedIDs(result.Invalidations)

	if len(jobs) > 0 {
		jobResults, budget, queue, err := e.runJobs(ctx, jobs, specs, states)
		if err != nil {
			return nil, err
		}
		result.Budget = budget
		result.Queue = queue
		for _, jr := range jobResults {
			if jr.err != nil {
				result.BlockErrors[jr.id] = jr.err
				_ = e.cache.InvalidateBlock(jr.id)
				continue
			}
			result.Meshes[jr.id] = jr.mesh
		}
	}

	// Analyze only seams whose two meshes are available; a failed block must
	// not prevent already-committed neighbours from remaining valid.
	analysisProject := e.project.Clone()
	analysisProject.Seams = nil
	for _, s := range e.project.Seams {
		if result.Meshes[s.A] != nil && result.Meshes[s.B] != nil {
			analysisProject.Seams = append(analysisProject.Seams, s)
		}
	}
	seams, attrErr := AnalyzeSeams(analysisProject, result.Meshes)
	if attrErr != nil {
		result.AttributionErr = asError(attrErr)
	}
	result.Seams = mergeSeamReports(reusedSeams, seams)
	filteredSeams := result.Seams[:0]
	for _, s := range result.Seams {
		if result.Meshes[s.BlockA] != nil && result.Meshes[s.BlockB] != nil {
			filteredSeams = append(filteredSeams, s)
		}
	}
	result.Seams = filteredSeams
	sort.Slice(result.Seams, func(i, j int) bool { return result.Seams[i].ID < result.Seams[j].ID })

	allOK := len(result.BlockErrors) == 0
	if allOK {
		check, err := VerifyTruth(e.project, result.Meshes, result.Seams)
		result.TruthCheck = check
		if err != nil {
			return result, err
		}
	}

	// Commit in canonical dependency order. This is the only writer: each
	// record is serialized as part of an atomic whole-checkpoint rename.
	for _, id := range e.order {
		mesh := result.Meshes[id]
		if mesh == nil {
			continue
		}
		if containsString(result.Reused, id) {
			result.CommitSequence = append(result.CommitSequence, id)
			continue
		}
		var blockSeams []SeamReport
		for _, s := range result.Seams {
			if s.BlockA == id || s.BlockB == id {
				blockSeams = append(blockSeams, s)
			}
		}
		attrs := attributionLabels(blockSeams)
		rec := BlockRecord{
			BlockID:           id,
			CacheKey:          keys[id],
			ContentHash:       contents[id],
			AdjacencyVersion:  e.adjVer,
			ParamVersion:      params[id],
			AlgorithmVersion:  AlgorithmVersion,
			Dependencies:      append([]string(nil), specs[id].Neighbors...),
			UpstreamKeys:      append([]string(nil), upstreams[id]...),
			VertexDigest:      mesh.VertexDigest,
			TriangleDigest:    mesh.TriangleDigest,
			FoldDigest:        digestJSON(blockSeams),
			AttributionDigest: digestJSON(attrs),
			Mesh:              mesh,
			Seams:             blockSeams,
			Attribution:       attrs,
		}
		committed, err := e.cache.CommitRecord(rec)
		if err != nil {
			return nil, &Error{Code: CodeBlock, BlockID: id, Field: "checkpoint", Message: err.Error()}
		}
		result.WrittenBlocks = append(result.WrittenBlocks, id)
		result.CommitSequence = append(result.CommitSequence, id)
		_ = committed
	}
	result.ReusedCount = len(result.Reused)
	// A successful bake clears the invalidation markers it consumed.
	for _, id := range append(append([]string(nil), result.Reused...), result.Recomputed...) {
		delete(e.invalid, id)
	}
	return result, nil
}

func invalidatedIDs(in []Invalidation) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v.BlockID] {
			seen[v.BlockID] = true
			out = append(out, v.BlockID)
		}
	}
	sort.Strings(out)
	return out
}

func asError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: CodeBlock, Message: err.Error()}
}

func mergeSeamReports(a, b []SeamReport) []SeamReport {
	byID := map[string]SeamReport{}
	for _, s := range a {
		byID[s.ID] = s
	}
	for _, s := range b {
		byID[s.ID] = s
	}
	out := make([]SeamReport, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	return out
}

func containsString(in []string, v string) bool {
	for _, s := range in {
		if s == v {
			return true
		}
	}
	return false
}

func (e *Engine) runJobs(ctx context.Context, jobs []string, specs map[string]BlockSpec, states map[string]BlockState) ([]jobResult, BudgetUsage, []QueueEvent, error) {
	workers := e.config.MaxParallelism
	if workers > len(jobs) {
		workers = len(jobs)
	}
	sem := newWeightedSem(e.config.MeshMemoryBudget)
	type indexedJob struct {
		index int
		id    string
	}
	input := make(chan indexedJob)
	outputs := make([]jobResult, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range input {
				outputs[job.index] = e.runOneJob(ctx, sem, job.id, specs[job.id], states[job.id])
			}
		}()
	}
	for i, id := range jobs {
		input <- indexedJob{index: i, id: id}
	}
	close(input)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, BudgetUsage{}, nil, &Error{Code: CodeBlock, Field: "context", Message: err.Error()}
	}
	queue := make([]QueueEvent, 0, len(outputs))
	peak := sem.peakUsed()
	var totalWait int64
	queued := 0
	for _, out := range outputs {
		queue = append(queue, out.queue)
		totalWait += out.queue.WaitMS
		if out.queue.WaitMS > 0 || !out.queue.Acquired && out.err != nil {
			queued++
		}
	}
	return outputs, BudgetUsage{
		MaxParallelism: e.config.MaxParallelism,
		MemoryBudget:   e.config.MeshMemoryBudget,
		PeakInUse:      peak,
		QueuedCount:    queued,
		TotalWaitMS:    totalWait,
	}, queue, nil
}

func (e *Engine) runOneJob(ctx context.Context, sem *weightedSem, id string, spec BlockSpec, st BlockState) jobResult {
	out := jobResult{id: id}
	estimate := int64(spec.Budget.MaxVertices)*64 + int64(spec.Budget.MaxTriangles)*48
	out.queue = QueueEvent{BlockID: id, RequestedBytes: estimate, Reason: "mesh_memory"}
	if estimate > e.config.MeshMemoryBudget {
		out.queue.Reason = "request_exceeds_global_mesh_budget"
		out.queue.Acquired = false
		out.err = &Error{
			Code:     CodeBlock,
			BlockID:  id,
			Field:    "mesh_memory_budget",
			OperandA: fmt.Sprintf("request=%d", estimate),
			OperandB: fmt.Sprintf("budget=%d", e.config.MeshMemoryBudget),
			Message:  "single block cannot fit the global mesh memory budget",
		}
		return out
	}
	start := time.Now()
	if err := sem.acquire(ctx, estimate); err != nil {
		out.queue.WaitMS = time.Since(start).Milliseconds()
		out.err = &Error{Code: CodeBlock, BlockID: id, Field: "queue", Message: err.Error()}
		return out
	}
	out.queue.WaitMS = time.Since(start).Milliseconds()
	out.queue.Acquired = true
	defer sem.release(estimate)
	if st.Fault == "block" {
		out.err = &Error{Code: CodeBlock, BlockID: id, Field: "block", Message: "injected single-block failure"}
		return out
	}
	if st.Fault == "decode" {
		out.err = &Error{Code: CodeDecode, BlockID: id, Field: "height", Message: "injected height data corruption"}
		return out
	}
	mesh, err := BuildMesh(e.project, spec, e.localHeight(id), e.boundaryHeight)
	if err != nil {
		out.err = asError(err)
		if out.err.BlockID == "" {
			out.err.BlockID = id
		}
		return out
	}
	if len(mesh.Vertices) > e.config.MaxVerticesPerThread {
		out.err = &Error{
			Code:     CodeBlock,
			BlockID:  id,
			Field:    "max_vertices_per_thread",
			OperandA: fmt.Sprintf("vertices=%d", len(mesh.Vertices)),
			OperandB: fmt.Sprintf("limit=%d", e.config.MaxVerticesPerThread),
			Message:  "per-thread vertex buffer cap would be exceeded",
		}
		return out
	}
	out.mesh = mesh
	return out
}

type weightedSem struct {
	mu   sync.Mutex
	used int64
	max  int64
	peak int64
}

func newWeightedSem(max int64) *weightedSem {
	return &weightedSem{max: max}
}

func (s *weightedSem) acquire(ctx context.Context, n int64) error {
	for {
		s.mu.Lock()
		if s.used+n <= s.max {
			s.used += n
			if s.used > s.peak {
				s.peak = s.used
			}
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (s *weightedSem) release(n int64) {
	s.mu.Lock()
	s.used -= n
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

func (s *weightedSem) peakUsed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// CompareBakeResults locates the first difference in canonical block order.
// It never skips a block or relaxes a tolerance to make two paths agree.
func CompareBakeResults(a, b *BakeResult) error {
	if a == nil || b == nil {
		return &Error{Code: CodeDiff, Field: "result", LHS: fmtValue(a == nil), RHS: fmtValue(b == nil), Message: "nil bake result"}
	}
	if !reflect.DeepEqual(a.CommitSequence, b.CommitSequence) {
		return diffError("", "commit_sequence", a.CommitSequence, b.CommitSequence)
	}
	for _, id := range a.CommitSequence {
		ma, mb := a.Meshes[id], b.Meshes[id]
		if ma == nil || mb == nil {
			return &Error{Code: CodeDiff, BlockID: id, Field: "mesh", LHS: fmtValue(ma != nil), RHS: fmtValue(mb != nil), Message: "mesh presence differs"}
		}
		if !reflect.DeepEqual(ma.Vertices, mb.Vertices) {
			return diffError(id, "vertices", ma.Vertices, mb.Vertices)
		}
		if !reflect.DeepEqual(ma.Triangles, mb.Triangles) {
			return diffError(id, "triangles", ma.Triangles, mb.Triangles)
		}
		sa := seamsForBlock(a.Seams, id)
		sb := seamsForBlock(b.Seams, id)
		if !reflect.DeepEqual(sa, sb) {
			return diffError(id, "seams", sa, sb)
		}
	}
	return nil
}

func diffError(blockID, field string, lhs, rhs any) *Error {
	return &Error{
		Code:     CodeDiff,
		BlockID:  blockID,
		Field:    field,
		Mismatch: field,
		LHS:      fmtValue(lhs),
		RHS:      fmtValue(rhs),
		Message:  "first mismatch in canonical comparison",
	}
}

func seamsForBlock(in []SeamReport, id string) []SeamReport {
	var out []SeamReport
	for _, s := range in {
		if s.BlockA == id || s.BlockB == id {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func dependencyOrder(p *Project) []string {
	ids := p.BlockOrder()
	adj := map[string][]string{}
	for _, id := range ids {
		adj[id] = p.AdjacentBlocks(id)
	}
	// Tarjan SCC. Bidirectional seam dependencies form cycles, so the only
	// meaningful topological order is on the SCC condensation.
	index := 0
	var stack []string
	onStack := map[string]bool{}
	indices := map[string]int{}
	low := map[string]int{}
	comp := map[string]int{}
	var comps [][]string
	var strongConnect func(v string)
	strongConnect = func(v string) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, ok := indices[w]; !ok {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && indices[w] < low[v] {
				low[v] = indices[w]
			}
		}
		if low[v] == indices[v] {
			var c []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp[w] = len(comps)
				c = append(c, w)
				if w == v {
					break
				}
			}
			sort.Strings(c)
			comps = append(comps, c)
		}
	}
	for _, id := range ids {
		if _, ok := indices[id]; !ok {
			strongConnect(id)
		}
	}
	compAdj := make([]map[int]bool, len(comps))
	indeg := make([]int, len(comps))
	for i := range compAdj {
		compAdj[i] = map[int]bool{}
	}
	for _, id := range ids {
		for _, n := range adj[id] {
			a, b := comp[id], comp[n]
			if a != b && !compAdj[a][b] {
				compAdj[a][b] = true
				indeg[b]++
			}
		}
	}
	var ready []int
	for i := range comps {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return comps[ready[i]][0] < comps[ready[j]][0] })
	var out []string
	for len(ready) > 0 {
		c := ready[0]
		ready = ready[1:]
		out = append(out, comps[c]...)
		next := make([]int, 0, len(compAdj[c]))
		for n := range compAdj[c] {
			indeg[n]--
			if indeg[n] == 0 {
				next = append(next, n)
			}
		}
		ready = append(ready, next...)
		sort.Slice(ready, func(i, j int) bool { return comps[ready[i]][0] < comps[ready[j]][0] })
	}
	if len(out) != len(ids) {
		return ids
	}
	return out
}
