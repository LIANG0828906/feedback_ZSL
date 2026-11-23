package bake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"terrain-bake/project"
)

type ManagerOptions struct {
	CacheDir               string
	OutputDir              string
	Params                 Params
	FailureInjector        func(blockID string) error
	DisableEditPersistence bool
}

type BlockState struct {
	BlockID        string         `json:"blockId"`
	Col            int            `json:"col"`
	Row            int            `json:"row"`
	Status         string         `json:"status"`
	CacheHit       bool           `json:"cacheHit"`
	LastError      *project.Error `json:"lastError,omitempty"`
	LastBakeMS     float64        `json:"lastBakeMs"`
	ArtifactHash   string         `json:"artifactHash,omitempty"`
	VertexCount    int            `json:"vertexCount"`
	InstanceCount  int            `json:"instanceCount"`
	Bounds         Bounds         `json:"bounds"`
	Seams          SeamStats      `json:"seams"`
	MaterialRatios []float64      `json:"materialRatios"`
	Dependencies   []string       `json:"dependencies"`
	Generation     uint64         `json:"generation"`
}

type ManagerStatus struct {
	ContentHash       string       `json:"contentHash"`
	AlgorithmVersion  string       `json:"algorithmVersion"`
	ParamsVersion     string       `json:"paramsVersion"`
	ParamsHash        string       `json:"paramsHash"`
	Generation        uint64       `json:"generation"`
	CachePresent      bool         `json:"cachePresent"`
	CacheValid        bool         `json:"cacheValid"`
	CacheReason       string       `json:"cacheReason"`
	CacheSnapshotPath string       `json:"cacheSnapshotPath"`
	CacheSnapshotHash string       `json:"cacheSnapshotHash"`
	OutputDir         string       `json:"outputDir"`
	TruthStale        bool         `json:"truthStale"`
	ArtifactCount     int          `json:"artifactCount"`
	States            []BlockState `json:"states"`
}

type CommitRecord struct {
	BlockID      string  `json:"blockId"`
	ArtifactHash string  `json:"artifactHash"`
	OldHash      string  `json:"oldHash,omitempty"`
	DurationMS   float64 `json:"durationMs"`
}

type BlockFailure struct {
	BlockID string         `json:"blockId"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Error   *project.Error `json:"error"`
}

type BakeReport struct {
	Generation   uint64               `json:"generation"`
	DurationMS   float64              `json:"durationMs"`
	Affected     []string             `json:"affected"`
	CacheHits    []string             `json:"cacheHits"`
	CommitOrder  []string             `json:"commitOrder"`
	Committed    []CommitRecord       `json:"committed"`
	Failures     []BlockFailure       `json:"failures,omitempty"`
	Invalidation *InvalidationReport  `json:"invalidation,omitempty"`
	Snapshot     *SnapshotWriteResult `json:"snapshot,omitempty"`
	Canceled     bool                 `json:"canceled"`
}

type Manager struct {
	opMu sync.Mutex
	mu   sync.Mutex

	p        *project.Project
	graph    *Graph
	opts     ManagerOptions
	snapshot *SnapshotStore
	cache    LoadedCache

	artifacts  map[string]*Artifact
	states     map[string]*BlockState
	generation uint64

	activeCancel context.CancelFunc
	activeRun    bool
}

func NewManager(p *project.Project, g *Graph, opts ManagerOptions) (*Manager, *project.Error) {
	if e := opts.Params.Validate(); e != nil {
		return nil, e
	}
	if opts.CacheDir == "" {
		opts.CacheDir = filepath.Join(p.Root, "..", "cache")
	}
	if abs, err := filepath.Abs(opts.CacheDir); err == nil {
		opts.CacheDir = abs
	}
	if opts.OutputDir == "" {
		opts.OutputDir = filepath.Join(p.Root, "..", "build")
	}
	if abs, err := filepath.Abs(opts.OutputDir); err == nil {
		opts.OutputDir = abs
	}
	store := NewSnapshotStore(opts.CacheDir)
	cache := store.Load(p, g, opts.Params)
	m := &Manager{
		p:         p,
		graph:     g,
		opts:      opts,
		snapshot:  store,
		cache:     cache,
		artifacts: make(map[string]*Artifact),
		states:    make(map[string]*BlockState, len(g.Nodes)),
	}
	if cache.Valid {
		for id, a := range cache.Artifacts {
			m.artifacts[id] = a
		}
		m.generation = cache.Generation
	}
	for _, id := range g.Nodes {
		bd := p.Blocks[id]
		st := &BlockState{
			BlockID:      id,
			Col:          bd.Def.Col,
			Row:          bd.Def.Row,
			Dependencies: g.BlockDependencies(id),
		}
		if a := m.artifacts[id]; a != nil {
			st.Status = "cached"
			st.CacheHit = true
			m.fillState(st, a, m.generation)
		} else {
			st.Status = "dirty"
		}
		m.states[id] = st
	}
	return m, nil
}

func (m *Manager) Project() *project.Project {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.p
}

func (m *Manager) Graph() *Graph {
	return m.graph
}

func (m *Manager) Params() Params {
	return m.opts.Params
}

func (m *Manager) Artifact(blockID string) *Artifact {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.artifacts[blockID]
}

func (m *Manager) Status() ManagerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := ManagerStatus{
		ContentHash:       m.p.ContentHash,
		AlgorithmVersion:  AlgorithmVersion,
		ParamsVersion:     m.opts.Params.ParamsVersion,
		ParamsHash:        m.opts.Params.ArtifactHash(),
		Generation:        m.generation,
		CachePresent:      m.cache.Present,
		CacheValid:        m.cache.Valid,
		CacheReason:       m.cache.Reason,
		CacheSnapshotPath: m.snapshot.Path,
		CacheSnapshotHash: m.cache.SnapshotHash,
		OutputDir:         m.opts.OutputDir,
		TruthStale:        m.p.TruthStale,
		ArtifactCount:     len(m.artifacts),
	}
	order := m.graph.OrderAll()
	for _, id := range order {
		st := m.states[id]
		if st == nil {
			continue
		}
		status.States = append(status.States, cloneState(st))
	}
	return status
}

func (m *Manager) InvalidateQuery(blockID, changeKind string) (*InvalidationReport, *project.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.p.Blocks[blockID]; !ok {
		return nil, project.NewError(project.CodeSchema, blockID, "block", "unknown block")
	}
	if changeKind != "height" && changeKind != "material" {
		return nil, project.NewError(project.CodeSchema, blockID, "kind", "change kind must be height or material")
	}
	return m.graph.Invalidate(blockID, changeKind), nil
}

func (m *Manager) BakeAll(ctx context.Context, force bool) (*BakeReport, *project.Error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	report := &BakeReport{}
	start := time.Now()
	runCtx, e := m.beginRun(ctx)
	if e != nil {
		return report, e
	}
	defer m.endRun()

	m.mu.Lock()
	if force {
		m.artifacts = make(map[string]*Artifact)
		for _, id := range m.graph.Nodes {
			m.states[id].Status = "dirty"
			m.states[id].CacheHit = false
			m.states[id].LastError = nil
			m.states[id].ArtifactHash = ""
		}
	}
	var dirty []string
	allOrder := m.graph.OrderAll()
	for _, id := range allOrder {
		if m.artifacts[id] == nil {
			dirty = append(dirty, id)
			m.states[id].Status = "dirty"
			m.states[id].CacheHit = false
		}
	}
	for _, id := range allOrder {
		if m.artifacts[id] != nil {
			m.states[id].Status = "cached"
			m.states[id].CacheHit = true
			report.CacheHits = append(report.CacheHits, id)
		}
	}
	m.mu.Unlock()

	report.Affected = m.graph.Order(dirty)
	report.CommitOrder = append([]string(nil), report.Affected...)
	if len(dirty) == 0 {
		report.Generation = m.generation
		report.DurationMS = float64(time.Since(start).Microseconds()) / 1000
		if e := m.writeBuildOutputs(); e != nil {
			return report, e
		}
		return report, nil
	}
	if e := m.applyBatch(runCtx, report, dirty, m.opts.FailureInjector); e != nil {
		m.finishReport(report, start)
		return report, e
	}
	m.finishReport(report, start)
	return report, firstFailureError(report)
}

func (m *Manager) EditHeight(ctx context.Context, blockID string, col, row int, value float64) (*BakeReport, *project.Error) {
	return m.applyEdit(ctx, blockID, "height", func(p *project.Project) *project.Error {
		return p.ApplyHeightEdit(blockID, col, row, value)
	})
}

func (m *Manager) EditMaterial(ctx context.Context, blockID string, col, row, layer, weight int) (*BakeReport, *project.Error) {
	return m.applyEdit(ctx, blockID, "material", func(p *project.Project) *project.Error {
		return p.ApplyMaterialEdit(blockID, col, row, layer, weight)
	})
}

func (m *Manager) applyEdit(ctx context.Context, blockID, kind string, apply func(*project.Project) *project.Error) (*BakeReport, *project.Error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	invalidation := m.graph.Invalidate(blockID, kind)
	report := &BakeReport{Invalidation: invalidation}
	start := time.Now()
	if ctx != nil && ctx.Err() != nil {
		return report, project.NewError(project.CodeCancel, blockID, "edit", "edit canceled before apply")
	}

	m.mu.Lock()
	if m.states[blockID] == nil {
		m.mu.Unlock()
		return report, project.NewError(project.CodeSchema, blockID, "block", "unknown block")
	}
	previous := m.p.Clone()
	m.mu.Unlock()
	if e := apply(previous); e != nil {
		return report, e
	}
	if !m.opts.DisableEditPersistence {
		if e := project.WriteEdits(previous.Root, previous.Edits); e != nil {
			return report, e
		}
	}

	runCtx, e := m.beginRun(ctx)
	if e != nil {
		return report, e
	}
	defer m.endRun()

	affected := make([]string, 0, len(invalidation.Affected))
	for _, entry := range invalidation.Affected {
		affected = append(affected, entry.BlockID)
	}
	order := m.graph.Order(affected)
	report.Affected = append([]string(nil), order...)
	report.CommitOrder = append([]string(nil), order...)
	report.CacheHits = append([]string(nil), invalidation.HitBlocks...)

	m.mu.Lock()
	m.p = previous
	for _, id := range order {
		delete(m.artifacts, id)
		m.states[id].Status = "dirty"
		m.states[id].CacheHit = false
		m.states[id].LastError = nil
		m.states[id].ArtifactHash = ""
	}
	for _, id := range report.CacheHits {
		if m.artifacts[id] != nil {
			m.states[id].Status = "cached"
			m.states[id].CacheHit = true
		}
	}
	report.Generation = m.generation
	m.mu.Unlock()

	if e := m.applyBatch(runCtx, report, order, m.opts.FailureInjector); e != nil {
		m.finishReport(report, start)
		return report, e
	}
	m.finishReport(report, start)
	return report, firstFailureError(report)
}

func (m *Manager) RetryFailed(ctx context.Context) (*BakeReport, *project.Error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	report := &BakeReport{}
	start := time.Now()

	m.mu.Lock()
	var failed []string
	for _, id := range m.graph.OrderAll() {
		st := m.states[id]
		if st != nil && st.Status == "failed" {
			failed = append(failed, id)
		}
	}
	for _, id := range m.graph.Nodes {
		if _, ok := m.states[id]; !ok {
			continue
		}
		if m.artifacts[id] != nil && m.states[id].Status != "failed" {
			report.CacheHits = append(report.CacheHits, id)
		}
	}
	report.Generation = m.generation
	m.mu.Unlock()
	if len(failed) == 0 {
		report.DurationMS = float64(time.Since(start).Microseconds()) / 1000
		return report, nil
	}
	runCtx, e := m.beginRun(ctx)
	if e != nil {
		return report, e
	}
	defer m.endRun()
	order := m.graph.Order(failed)
	report.Affected = append([]string(nil), order...)
	report.CommitOrder = append([]string(nil), order...)

	m.mu.Lock()
	for _, id := range order {
		m.states[id].Status = "dirty"
		m.states[id].CacheHit = false
	}
	m.mu.Unlock()
	if e := m.applyBatch(runCtx, report, order, m.opts.FailureInjector); e != nil {
		m.finishReport(report, start)
		return report, e
	}
	m.finishReport(report, start)
	return report, firstFailureError(report)
}

func (m *Manager) CancelActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeRun || m.activeCancel == nil {
		return false
	}
	m.activeCancel()
	return true
}

func (m *Manager) beginRun(ctx context.Context) (context.Context, *project.Error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.activeRun = true
	m.activeCancel = cancel
	m.mu.Unlock()
	return runCtx, nil
}

func (m *Manager) endRun() {
	m.mu.Lock()
	if m.activeCancel != nil {
		m.activeCancel()
	}
	m.activeCancel = nil
	m.activeRun = false
	m.mu.Unlock()
}

type blockResult struct {
	id       string
	artifact *Artifact
	err      *project.Error
	duration time.Duration
}

func (m *Manager) runBatch(ctx context.Context, ids []string, injector func(string) error) map[string]blockResult {
	order := m.graph.Order(ids)
	results := make(chan blockResult, len(order))
	sem := make(chan struct{}, m.opts.Params.MaxWorkers)
	for _, id := range order {
		id := id
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- m.bakeOne(ctx, id, injector)
		}()
	}
	out := make(map[string]blockResult, len(order))
	for i := 0; i < len(order); i++ {
		res := <-results
		out[res.id] = res
	}
	return out
}

func (m *Manager) bakeOne(ctx context.Context, id string, injector func(string) error) (result blockResult) {
	start := time.Now()
	result.id = id
	defer func() {
		if r := recover(); r != nil {
			result.artifact = nil
			result.err = project.NewError(project.CodeBake, id, "bake", "block bake panic: %v", r).
				WithDetail("panic", fmt.Sprintf("%v", r))
			result.duration = time.Since(start)
		}
	}()
	if err := ctx.Err(); err != nil {
		result.err = project.NewError(project.CodeCancel, id, "bake", "bake canceled")
		result.duration = time.Since(start)
		return result
	}
	if injector != nil {
		if err := injector(id); err != nil {
			result.err = toProjectError(err, id, "bake")
			result.duration = time.Since(start)
			return result
		}
	}
	artifact, err := BakeBlock(m.p, id, m.opts.Params)
	result.duration = time.Since(start)
	if err != nil {
		result.err = err
		return result
	}
	result.artifact = artifact
	return result
}

func (m *Manager) applyBatch(ctx context.Context, report *BakeReport, ids []string, injector func(string) error) *project.Error {
	order := m.graph.Order(ids)
	m.mu.Lock()
	for _, id := range order {
		if st := m.states[id]; st != nil {
			st.Status = "baking"
			st.CacheHit = false
			st.LastError = nil
		}
	}
	m.mu.Unlock()
	results := m.runBatch(ctx, order, injector)
	if ctx.Err() != nil {
		m.mu.Lock()
		for _, id := range order {
			if m.artifacts[id] == nil {
				m.states[id].Status = "dirty"
			}
		}
		report.Canceled = true
		report.Generation = m.generation
		m.mu.Unlock()
		return project.NewError(project.CodeCancel, "", "bake", "bake canceled; no batch artifacts were committed")
	}

	var oldHashes = make(map[string]string)
	m.mu.Lock()
	for _, id := range order {
		oldHashes[id] = m.states[id].ArtifactHash
	}
	m.mu.Unlock()

	committedAny := false
	for _, id := range order {
		res := results[id]
		if res.err != nil {
			m.mu.Lock()
			delete(m.artifacts, id)
			st := m.states[id]
			st.Status = "failed"
			st.CacheHit = false
			st.LastError = res.err
			st.ArtifactHash = ""
			st.LastBakeMS = float64(res.duration.Microseconds()) / 1000
			m.mu.Unlock()
			report.Failures = append(report.Failures, BlockFailure{
				BlockID: id, Code: res.err.Code, Message: res.err.Message, Error: res.err,
			})
			continue
		}
		committedAny = true
		m.mu.Lock()
		m.artifacts[id] = res.artifact
		st := m.states[id]
		st.Status = "committed"
		st.CacheHit = false
		st.LastError = nil
		m.fillState(st, res.artifact, m.generation+1)
		st.LastBakeMS = float64(res.duration.Microseconds()) / 1000
		m.mu.Unlock()
		report.Committed = append(report.Committed, CommitRecord{
			BlockID: id, ArtifactHash: res.artifact.Hash(), OldHash: oldHashes[id],
			DurationMS: float64(res.duration.Microseconds()) / 1000,
		})
	}
	m.mu.Lock()
	if committedAny {
		m.generation++
	}
	for _, rec := range report.Committed {
		m.states[rec.BlockID].Generation = m.generation
	}
	report.Generation = m.generation
	m.mu.Unlock()

	if e := m.persistCurrent(); e != nil {
		return e
	}
	return nil
}

func (m *Manager) persistCurrent() *project.Error {
	m.mu.Lock()
	order := m.graph.OrderAll()
	artifacts := make(map[string]*Artifact, len(m.artifacts))
	for id, a := range m.artifacts {
		artifacts[id] = a
	}
	generation := m.generation
	p := m.p
	m.mu.Unlock()
	write, e := m.snapshot.Write(p, m.graph, m.opts.Params, generation, order, artifacts)
	if e != nil {
		return e
	}
	m.mu.Lock()
	m.cache = LoadedCache{
		Valid: true, Present: true, Reason: "snapshot written",
		Generation: generation, SnapshotHash: write.Hash, Artifacts: artifacts,
	}
	m.mu.Unlock()
	return m.writeBuildOutputsWith(write)
}

func (m *Manager) writeBuildOutputs() *project.Error {
	m.mu.Lock()
	write := SnapshotWriteResult{
		Path: m.snapshot.Path, Hash: m.cache.SnapshotHash,
		EntryCount: len(m.artifacts), Generation: m.generation,
	}
	m.mu.Unlock()
	return m.writeBuildOutputsWith(write)
}

func (m *Manager) fillState(st *BlockState, a *Artifact, generation uint64) {
	if a == nil {
		return
	}
	st.VertexCount = len(a.Positions)
	st.InstanceCount = len(a.Instances)
	st.Bounds = a.Bounds
	st.Seams = a.Seams
	st.MaterialRatios = append([]float64(nil), a.MaterialRatios...)
	st.ArtifactHash = a.Hash()
	st.Generation = generation
}

func cloneState(st *BlockState) BlockState {
	out := *st
	out.MaterialRatios = append([]float64(nil), st.MaterialRatios...)
	out.Dependencies = append([]string(nil), st.Dependencies...)
	out.Seams.PerSeam = append([]SeamStat(nil), st.Seams.PerSeam...)
	return out
}

func (m *Manager) finishReport(report *BakeReport, start time.Time) {
	report.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	m.mu.Lock()
	report.Generation = m.generation
	m.mu.Unlock()
}

func firstFailureError(report *BakeReport) *project.Error {
	if len(report.Failures) == 0 {
		return nil
	}
	f := report.Failures[0]
	if f.Error != nil {
		return f.Error
	}
	return project.NewError(f.Code, f.BlockID, "bake", "%s", f.Message)
}

func toProjectError(err error, blockID, fieldName string) *project.Error {
	var pe *project.Error
	if errors.As(err, &pe) {
		return pe
	}
	return project.NewError(project.CodeBake, blockID, fieldName, "%v", err)
}

// ComputeFull bakes every block from scratch without consulting the cache or
// mutating manager state.
func (m *Manager) ComputeFull(ctx context.Context) (map[string]*Artifact, *project.Error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	runCtx, e := m.beginRun(ctx)
	if e != nil {
		return nil, e
	}
	defer m.endRun()
	order := m.graph.OrderAll()
	m.mu.Lock()
	for _, id := range order {
		m.states[id].Status = "baking"
	}
	m.mu.Unlock()
	results := m.runBatch(runCtx, order, nil)
	artifacts := make(map[string]*Artifact, len(order))
	var first *project.Error
	for _, id := range order {
		res := results[id]
		if res.err != nil {
			if first == nil {
				first = res.err
			}
			continue
		}
		artifacts[id] = res.artifact
	}
	m.mu.Lock()
	for _, id := range order {
		if m.artifacts[id] != nil {
			m.states[id].Status = "cached"
		} else if m.states[id].Status == "baking" {
			m.states[id].Status = "dirty"
		}
	}
	m.mu.Unlock()
	if runCtx.Err() != nil {
		return nil, project.NewError(project.CodeCancel, "", "compare", "full recompute canceled")
	}
	if first != nil {
		return nil, first
	}
	return artifacts, nil
}

func (m *Manager) CompareToFull(ctx context.Context) (*DiffReport, *project.Error) {
	full, e := m.ComputeFull(ctx)
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	actual := make(map[string]*Artifact, len(m.artifacts))
	for id, a := range m.artifacts {
		actual[id] = a
	}
	order := m.graph.OrderAll()
	m.mu.Unlock()
	return CompareArtifacts(actual, full, order)
}

func (m *Manager) SortedBlockIDs() []string {
	return append([]string(nil), m.graph.OrderAll()...)
}

func (m *Manager) RemoveOutput(blockID string) {
	_ = os.Remove(filepath.Join(m.opts.OutputDir, "blocks", blockID+".json"))
}

func (m *Manager) String() string {
	s := m.Status()
	return fmt.Sprintf("%s generation=%d cached=%d/%d", s.ContentHash, s.Generation, s.ArtifactCount, len(s.States))
}
