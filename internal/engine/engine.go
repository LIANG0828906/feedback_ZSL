package engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"terrainpatch/internal/boundary"
	"terrainpatch/internal/graph"
	"terrainpatch/internal/mesh"
	"terrainpatch/internal/model"
	"terrainpatch/internal/store"
)

type Options struct {
	Store                 store.Store
	Workers               int
	VertexBufferCap       int
	MemoryBudgetBytes     int64
	InterruptAfterCommits int
	ComputeHook           func(tileID string)
}

type Invalidation struct {
	TileID string `json:"tileId"`
	Reason string `json:"reason"`
	Reused bool   `json:"reused"`
}

type TileInfo struct {
	TileID        string          `json:"tileId"`
	Status        string          `json:"status"`
	Reason        string          `json:"reason,omitempty"`
	Reused        bool            `json:"reused"`
	DurationMS    int64           `json:"durationMs"`
	QueueWaitMS   int64           `json:"queueWaitMs"`
	QueueReasons  []string        `json:"queueReasons,omitempty"`
	MemoryBytes   int64           `json:"memoryBytes"`
	VertexCount   int             `json:"vertexCount"`
	TriangleCount int             `json:"triangleCount"`
	Depth         int             `json:"depth"`
	ContentHash   string          `json:"contentHash,omitempty"`
	CommitSeq     int64           `json:"commitSeq,omitempty"`
	Error         *model.AppError `json:"error,omitempty"`
}

type BudgetSnapshot struct {
	MaxParallelism    int   `json:"maxParallelism"`
	VertexBufferCap   int   `json:"vertexBufferCap"`
	MemoryBudgetBytes int64 `json:"memoryBudgetBytes"`
	MemoryPeakBytes   int64 `json:"memoryPeakBytes"`
	TaskMemoryBytes   int64 `json:"taskMemoryBytes"`
}

type QueueEvent struct {
	TileID  string   `json:"tileId"`
	Reason  string   `json:"reason"`
	Reasons []string `json:"reasons,omitempty"`
	WaitMS  int64    `json:"waitMs"`
}

type Report struct {
	Kind            string                 `json:"kind"`
	Generation      int64                  `json:"generation"`
	DurationMS      int64                  `json:"durationMs"`
	Invalidations   []Invalidation         `json:"invalidations"`
	Tiles           []TileInfo             `json:"tiles"`
	CommitSequence  []string               `json:"commitSequence"`
	WrittenSequence []store.CommitRecord   `json:"writtenSequence"`
	Boundaries      []boundary.Stat        `json:"boundaries"`
	Queue           []QueueEvent           `json:"queue"`
	Budget          BudgetSnapshot         `json:"budget"`
	Meshes          map[string]*model.Mesh `json:"meshes"`
	Reset           bool                   `json:"reset,omitempty"`
	ResetReason     string                 `json:"resetReason,omitempty"`
	Interrupted     bool                   `json:"interrupted,omitempty"`
}

type RecoveryResult struct {
	Reset     bool     `json:"reset"`
	Reason    string   `json:"reason,omitempty"`
	Recovered []string `json:"recovered"`
	Missing   []string `json:"missing"`
}

type CheckpointContent struct {
	Mesh         *model.Mesh     `json:"mesh"`
	Stats        []boundary.Stat `json:"stats"`
	Dependencies []string        `json:"dependencies"`
}

type Engine struct {
	mu         sync.RWMutex
	runMu      sync.Mutex
	proj       *model.Project
	g          *graph.Graph
	store      store.Store
	opts       Options
	meshes     map[string]*model.Mesh
	stats      map[string]boundary.Stat
	failures   map[string]*model.AppError
	checks     map[string]store.Checkpoint
	infos      map[string]TileInfo
	inject     map[string]string
	generation int64
	lastReport *Report

	memMu   sync.Mutex
	memUsed int64
	memPeak int64
	memCond *sync.Cond
}

func NewEngine(p *model.Project, opts Options) *Engine {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.VertexBufferCap <= 0 {
		opts.VertexBufferCap = 4096
	}
	if opts.MemoryBudgetBytes <= 0 {
		opts.MemoryBudgetBytes = 16 << 20
	}
	if opts.Store == nil {
		opts.Store = store.NewMemoryStore()
	}
	e := &Engine{
		proj: p, g: graph.Build(p), store: opts.Store, opts: opts,
		meshes: map[string]*model.Mesh{}, stats: map[string]boundary.Stat{},
		failures: map[string]*model.AppError{}, checks: map[string]store.Checkpoint{},
		infos: map[string]TileInfo{}, inject: map[string]string{},
	}
	e.memCond = sync.NewCond(&e.memMu)
	return e
}

func (e *Engine) Graph() *graph.Graph { return e.g }
func (e *Engine) Store() store.Store  { return e.store }

func (e *Engine) Project() *model.Project { return e.proj }

func (e *Engine) LastReport() *Report {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastReport
}

func (e *Engine) RecomputeOnly(ctx context.Context, tileID, reason string) (*Report, *model.AppError) {
	return e.run(ctx, map[string]string{tileID: reason}, reason)
}

func (e *Engine) InjectFailure(tileID, code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inject[tileID] = code
}

func (e *Engine) ClearInjectedFailure(tileID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inject, tileID)
}

func (e *Engine) SetInterruptAfterCommits(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.InterruptAfterCommits = n
}

func (e *Engine) SnapshotReport(kind string) *Report {
	r := &Report{
		Kind: kind, Generation: e.generation, CommitSequence: append([]string{}, e.g.Tiles...),
		Budget: BudgetSnapshot{MaxParallelism: e.opts.Workers, VertexBufferCap: e.opts.VertexBufferCap, MemoryBudgetBytes: e.opts.MemoryBudgetBytes},
		Meshes: map[string]*model.Mesh{},
	}
	e.fillReportLocked(r, time.Now())
	return r
}

func (e *Engine) RebuildAll(ctx context.Context, kind string) (*Report, *model.AppError) {
	impact := map[string]string{}
	for _, id := range e.g.Tiles {
		impact[id] = "full"
	}
	return e.run(ctx, impact, kind)
}

type EditSpec struct {
	TileID   string `json:"tileId"`
	Field    string `json:"field"`
	IntValue int64  `json:"intValue"`
	GridX    int    `json:"gridX,omitempty"`
	GridY    int    `json:"gridY,omitempty"`
}

func (e *Engine) ApplyEdit(ctx context.Context, edit EditSpec) (*Report, *model.AppError) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if err := ApplyEditSpec(e.proj, edit); err != nil {
		return nil, err
	}
	impact := e.g.Impact(edit.TileID, edit.Field)
	return e.runLocked(ctx, impact, "edit:"+edit.Field)
}

func (e *Engine) RetryTile(ctx context.Context, tileID string) (*Report, *model.AppError) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.ClearInjectedFailure(tileID)
	impact := map[string]string{tileID: "retry"}
	return e.runLocked(ctx, impact, "retry")
}

func ApplyEditSpec(p *model.Project, edit EditSpec) *model.AppError {
	t := p.Tile(edit.TileID)
	if t == nil {
		return model.NewError(model.CodePara, "unknown tile").WithTile(edit.TileID, "tileId")
	}
	switch edit.Field {
	case "sampleBudget":
		if edit.IntValue <= 0 || edit.IntValue > 1_000_000 {
			return model.NewError(model.CodePara, "sampleBudget out of range").
				WithTile(edit.TileID, "sampleBudget").WithActual(edit.IntValue).WithLimit([2]int64{1, 1_000_000})
		}
		t.SampleBudget = int(edit.IntValue)
	case "tolerance":
		if edit.IntValue < 0 || edit.IntValue > 1_000_000_000 {
			return model.NewError(model.CodePara, "tolerance out of range").
				WithTile(edit.TileID, "tolerance").WithActual(edit.IntValue).WithLimit([2]int64{0, 1_000_000_000})
		}
		t.Tolerance = edit.IntValue
	case "height":
		gx, gy := edit.GridX, edit.GridY
		if gx == 0 && gy == 0 {
			gx, gy = t.Grid.Width/2, t.Grid.Height/2
		}
		if gy < 0 || gy >= t.Grid.Height || gx < 0 || gx >= t.Grid.Width {
			return model.NewError(model.CodePara, "height grid coordinate out of range").
				WithTile(edit.TileID, "height").WithActual([2]int{gx, gy})
		}
		if edit.IntValue < t.HeightMin || edit.IntValue > t.HeightMax {
			return model.NewError(model.CodePara, "height value outside declared bounds").
				WithTile(edit.TileID, "height").WithActual(edit.IntValue).WithLimit([2]int64{t.HeightMin, t.HeightMax})
		}
		t.Grid.Heights[gy][gx] = edit.IntValue
	default:
		return model.NewError(model.CodePara, "unsupported editable field").
			WithTile(edit.TileID, "field").WithActual(edit.Field)
	}
	t.ParamVersion++
	return nil
}

func (e *Engine) run(ctx context.Context, impact map[string]string, kind string) (*Report, *model.AppError) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return e.runLocked(ctx, impact, kind)
}

type workResult struct {
	index int
	info  TileInfo
	mesh  *model.Mesh
	err   *model.AppError
}

func (e *Engine) runLocked(ctx context.Context, impact map[string]string, kind string) (*Report, *model.AppError) {
	start := time.Now()
	e.generation++
	gen := e.generation
	order := make([]string, 0, len(impact))
	for _, id := range e.g.TopoOrderAll() {
		if _, ok := impact[id]; ok {
			order = append(order, id)
		}
	}

	rep := &Report{
		Kind: kind, Generation: gen, CommitSequence: append([]string{}, e.g.Tiles...),
		Budget: BudgetSnapshot{MaxParallelism: e.opts.Workers, VertexBufferCap: e.opts.VertexBufferCap, MemoryBudgetBytes: e.opts.MemoryBudgetBytes},
		Meshes: map[string]*model.Mesh{},
	}
	defer func() {
		e.mu.Lock()
		e.lastReport = rep
		e.mu.Unlock()
	}()
	for _, id := range e.g.Tiles {
		reason, impacted := impact[id]
		rep.Invalidations = append(rep.Invalidations, Invalidation{TileID: id, Reason: reason, Reused: !impacted})
	}

	workerSem := make(chan struct{}, e.opts.Workers)
	results := make([]workResult, len(order))
	var wg sync.WaitGroup
	for i, id := range order {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i] = e.computeOne(ctx, i, id, impact[id], workerSem)
		}(i, id)
	}
	wg.Wait()

	e.mu.Lock()
	for _, id := range e.g.Tiles {
		if _, ok := impact[id]; !ok {
			info := TileInfo{TileID: id, Status: "cache-hit", Reused: true, Reason: "unaffected"}
			if cp, ok := e.checks[id]; ok {
				info.ContentHash = cp.ContentHash
				info.CommitSeq = cp.CommitSeq
			}
			e.infoLocked(id, info)
		}
	}
	for _, res := range results {
		id := res.info.TileID
		if res.err != nil {
			e.failures[id] = res.err
			e.infoLocked(id, res.info)
			continue
		}
		e.meshes[id] = res.mesh
		delete(e.failures, id)
		res.info.Status = "computed"
		e.infoLocked(id, res.info)
	}
	e.recomputeStatsLocked()
	e.mu.Unlock()

	committed := 0
	for _, res := range results {
		if res.err != nil || res.mesh == nil {
			continue
		}
		id := res.info.TileID
		cp, apErr := e.checkpointFor(id, gen, committed)
		if apErr != nil {
			res.info.Status = "failed"
			res.info.Error = apErr
			e.mu.Lock()
			e.failures[id] = apErr
			e.infoLocked(id, res.info)
			e.mu.Unlock()
			continue
		}
		rec, err := e.store.Save(ctx, cp, impact[id])
		if err != nil {
			apErr = model.NewError(model.CodeInternal, "sqlite checkpoint save failed: "+err.Error()).WithTile(id, "checkpoint")
			res.info.Status = "failed"
			res.info.Error = apErr
			e.mu.Lock()
			e.failures[id] = apErr
			e.infoLocked(id, res.info)
			e.mu.Unlock()
			continue
		}
		committed++
		res.info.ContentHash = cp.ContentHash
		res.info.CommitSeq = rec.Seq
		res.info.Status = "committed"
		e.mu.Lock()
		e.checks[id] = cp
		cp.CommitSeq = rec.Seq
		e.checks[id] = cp
		e.infoLocked(id, res.info)
		e.mu.Unlock()
		rep.WrittenSequence = append(rep.WrittenSequence, rec)

		e.mu.Lock()
		interrupt := e.opts.InterruptAfterCommits > 0 && committed >= e.opts.InterruptAfterCommits
		e.mu.Unlock()
		if interrupt {
			rep.Interrupted = true
			e.fillReportLocked(rep, start)
			return rep, model.NewError(model.CodeInternal, "simulated process interruption after durable commit").
				WithTile(id, "interruptAfterCommits").WithActual(committed)
		}
	}

	e.fillReportLocked(rep, start)
	return rep, nil
}

func (e *Engine) infoLocked(id string, info TileInfo) {
	if existing, ok := e.infos[id]; ok {
		if info.DurationMS == 0 {
			info.DurationMS = existing.DurationMS
		}
		if info.QueueWaitMS == 0 {
			info.QueueWaitMS = existing.QueueWaitMS
		}
	}
	e.infos[id] = info
}

func (e *Engine) computeOne(ctx context.Context, index int, id, reason string, sem chan struct{}) workResult {
	tile := *e.proj.Tile(id)
	info := TileInfo{TileID: id, Reason: reason}
	start := time.Now()

	select {
	case sem <- struct{}{}:
	default:
		info.QueueReasons = append(info.QueueReasons, "worker-slot")
		queueStart := time.Now()
		select {
		case sem <- struct{}{}:
			info.QueueWaitMS += time.Since(queueStart).Milliseconds()
		case <-ctx.Done():
			err := model.NewError(model.CodeInternal, "canceled while waiting for worker slot").WithTile(id, "worker")
			info.Status = "failed"
			info.Error = err
			return workResult{index: index, info: info, err: err}
		}
	}
	defer func() { <-sem }()

	upperVertices := int64(tile.Grid.Width * tile.Grid.Height)
	if upperVertices > int64(e.opts.VertexBufferCap) {
		err := model.NewError(model.CodeResource, "tile vertex buffer exceeds per-worker cap").
			WithTile(id, "vertexBufferCap").WithActual(upperVertices).WithLimit(e.opts.VertexBufferCap)
		info.Status = "failed"
		info.Error = err
		return workResult{index: index, info: info, err: err}
	}

	need := estimateMemory(tile)
	info.MemoryBytes = need
	if !e.tryAcquireMemory(need) {
		info.QueueReasons = append(info.QueueReasons, "memory-budget")
		queueStart := time.Now()
		e.acquireMemory(need)
		info.QueueWaitMS += time.Since(queueStart).Milliseconds()
	}
	defer e.releaseMemory(need)
	if e.opts.ComputeHook != nil {
		e.opts.ComputeHook(id)
	}

	e.mu.RLock()
	inject := e.inject[id]
	e.mu.RUnlock()
	if inject != "" {
		err := model.NewError(inject, "injected single-tile failure").WithTile(id, "injectFailure")
		info.Status = "failed"
		info.Error = err
		info.DurationMS = time.Since(start).Milliseconds()
		return workResult{index: index, info: info, err: err}
	}

	m, err := mesh.Build(tile)
	info.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		info.Status = "failed"
		info.Error = err
		return workResult{index: index, info: info, err: err}
	}
	if len(m.Vertices) > e.opts.VertexBufferCap {
		err := model.NewError(model.CodeResource, "actual vertex buffer exceeds per-worker cap").
			WithTile(id, "vertexBufferCap").WithActual(len(m.Vertices)).WithLimit(e.opts.VertexBufferCap)
		info.Status = "failed"
		info.Error = err
		return workResult{index: index, info: info, err: err}
	}
	info.VertexCount = len(m.Vertices)
	info.TriangleCount = len(m.Triangles)
	info.Depth = m.Depth
	return workResult{index: index, info: info, mesh: m}
}

func estimateMemory(t model.Tile) int64 {
	grid := int64(t.Grid.Width * t.Grid.Height)
	return 4096 + grid*96 + grid*2*64
}

func (e *Engine) acquireMemory(need int64) {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	for e.memUsed+need > e.opts.MemoryBudgetBytes {
		e.memCond.Wait()
	}
	e.memUsed += need
	if e.memUsed > e.memPeak {
		e.memPeak = e.memUsed
	}
}

func (e *Engine) tryAcquireMemory(need int64) bool {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	if e.memUsed+need > e.opts.MemoryBudgetBytes {
		return false
	}
	e.memUsed += need
	if e.memUsed > e.memPeak {
		e.memPeak = e.memUsed
	}
	return true
}

func (e *Engine) releaseMemory(need int64) {
	e.memMu.Lock()
	e.memUsed -= need
	if e.memUsed < 0 {
		e.memUsed = 0
	}
	e.memCond.Broadcast()
	e.memMu.Unlock()
}

func (e *Engine) recomputeStatsLocked() {
	e.stats = map[string]boundary.Stat{}
	for _, b := range e.proj.Boundaries {
		ta := e.proj.Tile(b.SideA.TileID)
		tb := e.proj.Tile(b.SideB.TileID)
		ma := e.meshes[b.SideA.TileID]
		mb := e.meshes[b.SideB.TileID]
		if ta == nil || tb == nil || ma == nil || mb == nil {
			continue
		}
		st, _ := boundary.Diagnose(b, *ta, *tb, ma, mb)
		if st != nil {
			e.stats[b.ID] = *st
		}
	}
}

func (e *Engine) checkpointFor(id string, gen int64, order int) (store.Checkpoint, *model.AppError) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	m := e.meshes[id]
	if m == nil {
		return store.Checkpoint{}, model.NewError(model.CodeInternal, "mesh missing after successful compute").WithTile(id, "checkpoint")
	}
	deps := append([]string{}, e.g.Dependencies[id]...)
	states := []boundary.Stat{}
	for _, bid := range e.g.Boundaries[id] {
		if st, ok := e.stats[bid]; ok {
			states = append(states, st)
		}
	}
	content := CheckpointContent{Mesh: m, Stats: states, Dependencies: deps}
	contentJSON := model.CanonicalJSON(content)
	vertexDigest := model.DigestJSON(CanonicalMesh(m).Vertices)
	triDigest := model.DigestJSON(CanonicalMesh(m).Triangles)
	cp := store.Checkpoint{
		TileID: id, Generation: gen, Attempt: 1, ParamVersion: m.ParamVersion,
		AlgorithmVersion: model.AlgorithmVersion, VertexDigest: vertexDigest, TriangleDigest: triDigest,
		BoundaryStatsJSON: string(model.CanonicalJSON(states)),
		DependenciesJSON:  string(model.CanonicalJSON(deps)),
		ContentHash:       model.SHA256Hex(contentJSON), ContentJSON: contentJSON,
		VertexCount: len(m.Vertices), TriangleCount: len(m.Triangles), CommittedOrder: order,
	}
	return cp, nil
}

func (e *Engine) fillReportLocked(rep *Report, start time.Time) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rep.DurationMS = time.Since(start).Milliseconds()
	rep.Meshes = map[string]*model.Mesh{}
	rep.Tiles = []TileInfo{}
	for _, id := range e.g.Tiles {
		if m := e.meshes[id]; m != nil {
			mc := *m
			rep.Meshes[id] = &mc
		}
		info, ok := e.infos[id]
		if !ok {
			info = TileInfo{TileID: id, Status: "pending"}
		}
		if f, ok := e.failures[id]; ok {
			info.Status = "failed"
			info.Error = f
		}
		rep.Tiles = append(rep.Tiles, info)
		if info.QueueWaitMS > 0 || len(info.QueueReasons) > 0 {
			rep.Queue = append(rep.Queue, QueueEvent{
				TileID: info.TileID, Reason: info.Reason, Reasons: info.QueueReasons, WaitMS: info.QueueWaitMS,
			})
		}
	}
	for _, info := range rep.Tiles {
		rep.Budget.TaskMemoryBytes += info.MemoryBytes
	}
	e.memMu.Lock()
	rep.Budget.MemoryPeakBytes = e.memPeak
	e.memMu.Unlock()
	for _, b := range e.proj.Boundaries {
		if st, ok := e.stats[b.ID]; ok {
			rep.Boundaries = append(rep.Boundaries, st)
		}
	}
}

func (e *Engine) Recover(ctx context.Context) (*RecoveryResult, *model.AppError) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	res := &RecoveryResult{Recovered: []string{}, Missing: []string{}}
	schemaMeta, err := e.store.GetMeta(ctx, "schema_version")
	if err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	algoMeta, err := e.store.GetMeta(ctx, "algorithm_version")
	if err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	if schemaMeta != "" && schemaMeta != model.SchemaVersion {
		return e.resetSnapshot(ctx, res, "schema version mismatch: "+schemaMeta)
	}
	if algoMeta != "" && algoMeta != model.AlgorithmVersion {
		return e.resetSnapshot(ctx, res, "algorithm version mismatch: "+algoMeta)
	}
	latest, err := e.store.Latest(ctx)
	if err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	for id, cp := range latest {
		if model.SHA256Hex(cp.ContentJSON) != cp.ContentHash {
			return e.resetSnapshot(ctx, res, "content hash mismatch for "+id)
		}
		if cp.AlgorithmVersion != model.AlgorithmVersion {
			return e.resetSnapshot(ctx, res, "checkpoint algorithm mismatch for "+id)
		}
		t := e.proj.Tile(id)
		if t == nil || cp.ParamVersion != t.ParamVersion {
			return e.resetSnapshot(ctx, res, "parameter version mismatch for "+id)
		}
		var content CheckpointContent
		if err := json.Unmarshal(cp.ContentJSON, &content); err != nil || content.Mesh == nil {
			return e.resetSnapshot(ctx, res, "corrupt checkpoint content for "+id)
		}
		e.mu.Lock()
		e.meshes[id] = content.Mesh
		for _, st := range content.Stats {
			e.stats[st.BoundaryID] = st
		}
		cpCopy := cp
		e.checks[id] = cpCopy
		e.infos[id] = TileInfo{
			TileID: id, Status: "cache-hit", Reused: true, Reason: "sqlite-version-match",
			ContentHash: cp.ContentHash, CommitSeq: cp.CommitSeq,
			VertexCount: cp.VertexCount, TriangleCount: cp.TriangleCount,
		}
		e.mu.Unlock()
		res.Recovered = append(res.Recovered, id)
	}
	for _, id := range e.g.Tiles {
		if _, ok := latest[id]; !ok {
			res.Missing = append(res.Missing, id)
		}
	}
	if err := e.store.SetMeta(ctx, "schema_version", model.SchemaVersion); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	if err := e.store.SetMeta(ctx, "algorithm_version", model.AlgorithmVersion); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	sortStrings(res.Recovered)
	sortStrings(res.Missing)
	snapshot := e.SnapshotReport("recovery")
	e.mu.Lock()
	e.lastReport = snapshot
	e.mu.Unlock()
	return res, nil
}

func (e *Engine) resetSnapshot(ctx context.Context, res *RecoveryResult, reason string) (*RecoveryResult, *model.AppError) {
	if err := e.store.Reset(ctx, reason); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	e.mu.Lock()
	e.meshes = map[string]*model.Mesh{}
	e.stats = map[string]boundary.Stat{}
	e.checks = map[string]store.Checkpoint{}
	e.failures = map[string]*model.AppError{}
	e.mu.Unlock()
	res.Reset = true
	res.Reason = reason
	res.Recovered = []string{}
	res.Missing = append([]string{}, e.g.Tiles...)
	if err := e.store.SetMeta(ctx, "schema_version", model.SchemaVersion); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	if err := e.store.SetMeta(ctx, "algorithm_version", model.AlgorithmVersion); err != nil {
		return nil, model.NewError(model.CodeInternal, err.Error())
	}
	return res, nil
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func CompareReports(expected, actual *Report) *Difference {
	if expected == nil || actual == nil {
		return &Difference{Field: "report", Expected: expected != nil, Actual: actual != nil}
	}
	if len(expected.CommitSequence) != len(actual.CommitSequence) {
		return &Difference{Field: "commitSequence.length", Expected: len(expected.CommitSequence), Actual: len(actual.CommitSequence)}
	}
	for i := range expected.CommitSequence {
		if expected.CommitSequence[i] != actual.CommitSequence[i] {
			return &Difference{Field: "commitSequence.item", Index: i, Expected: expected.CommitSequence[i], Actual: actual.CommitSequence[i]}
		}
	}
	for _, id := range expected.CommitSequence {
		d := CompareCanonicalMesh(id, expected.Meshes[id], actual.Meshes[id])
		if d != nil {
			return d
		}
	}
	expStats := map[string]boundary.Stat{}
	for _, st := range expected.Boundaries {
		expStats[st.BoundaryID] = st
	}
	for _, st := range actual.Boundaries {
		if es, ok := expStats[st.BoundaryID]; ok {
			if d := CompareBoundaryStat(es, st); d != nil {
				return d
			}
		}
	}
	return nil
}
