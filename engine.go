package terrainbake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Engine owns a project, dependency graph, resource policy and checkpoint
// directory. Public methods are serialized; block work inside one bake is the
// only parallel section.
type Engine struct {
	mu sync.Mutex

	project *Project
	graph   *DependencyGraph
	opts    EngineOptions
	store   *Store

	outputs map[string]*BlockOutput
	attempt map[string]int

	snapshotReset    bool
	snapshotResetWhy string
}

func NewEngine(p *Project, opts EngineOptions) (*Engine, error) {
	var err error
	opts, err = normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	store, err := OpenStore(opts.StoreDir)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		project: p,
		graph:   BuildDependencyGraph(p),
		opts:    opts,
		store:   store,
		outputs: make(map[string]*BlockOutput),
		attempt: make(map[string]int),
	}
	m := store.Manifest()
	reason, err := e.snapshotMismatchLocked(m)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		if err := store.Reset(); err != nil {
			return nil, err
		}
		e.snapshotReset = true
		e.snapshotResetWhy = reason
		return e, nil
	}
	for id, cp := range m.Blocks {
		if containsString(m.EpochDirty, id) {
			continue
		}
		out, err := store.LoadBlock(cp)
		if err != nil {
			// A corrupt non-dirty block invalidates the whole snapshot. This
			// prevents accidental partial reuse.
			if resetErr := store.Reset(); resetErr != nil {
				return nil, resetErr
			}
			e.outputs = make(map[string]*BlockOutput)
			e.snapshotReset = true
			e.snapshotResetWhy = fmt.Sprintf("block %s cache validation failed: %v", id, err)
			return e, nil
		}
		e.outputs[id] = out
	}
	return e, nil
}

func (e *Engine) snapshotMismatchLocked(m Manifest) (string, error) {
	if m.TargetContentHash == "" && len(m.Blocks) == 0 {
		return "", nil
	}
	if m.AlgorithmVersion != AlgorithmVersion {
		return fmt.Sprintf("algorithm version mismatch: snapshot=%s current=%s", m.AlgorithmVersion, AlgorithmVersion), nil
	}
	if m.ParamsVersion != ParamsHash(e.opts.Params) {
		return fmt.Sprintf("bake parameter version mismatch: snapshot=%s current=%s", m.ParamsVersion, ParamsHash(e.opts.Params)), nil
	}
	want := ProjectHash(e.project)
	if m.TargetContentHash != want {
		return fmt.Sprintf("global content hash mismatch: snapshot=%s current=%s", m.TargetContentHash, want), nil
	}
	for id, cp := range m.Blocks {
		if containsString(m.EpochDirty, id) {
			continue
		}
		if cp.AlgorithmVersion != AlgorithmVersion {
			return fmt.Sprintf("block %s algorithm version mismatch", id), nil
		}
		if cp.ParamsVersion != ParamsHash(e.opts.Params) {
			return fmt.Sprintf("block %s parameter version mismatch", id), nil
		}
		wantHash := BlockContentHash(e.project, id, e.opts.Params, AlgorithmVersion)
		if cp.ContentHash != wantHash {
			return fmt.Sprintf("block %s content hash mismatch", id), nil
		}
		if _, err := e.store.LoadBlock(cp); err != nil {
			return fmt.Sprintf("block %s cache validation failed: %v", id, err), nil
		}
	}
	return "", nil
}

func (e *Engine) ProjectHash() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ProjectHash(e.project)
}

func (e *Engine) DependencyGraph() *DependencyGraph {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneGraph(e.graph)
}

func cloneGraph(g *DependencyGraph) *DependencyGraph {
	out := &DependencyGraph{
		Blocks:        append([]string(nil), g.Blocks...),
		SeamEdges:     make(map[string][]string, len(g.SeamEdges)),
		MaterialUsers: make(map[string][]string, len(g.MaterialUsers)),
	}
	for k, v := range g.SeamEdges {
		out.SeamEdges[k] = append([]string(nil), v...)
	}
	for k, v := range g.MaterialUsers {
		out.MaterialUsers[k] = append([]string(nil), v...)
	}
	return out
}

func (e *Engine) CheckpointManifest() Manifest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.Manifest()
}

func cloneProject(p *Project) (*Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, &Error{Code: CodeBake, Object: "project", Field: "clone", Message: err.Error()}
	}
	var out Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &Error{Code: CodeBake, Object: "project", Field: "clone", Message: err.Error()}
	}
	return &out, nil
}

func (e *Engine) baselineReadyLocked(m Manifest) bool {
	return m.Complete && m.TargetContentHash == ProjectHash(e.project) &&
		m.AlgorithmVersion == AlgorithmVersion && m.ParamsVersion == ParamsHash(e.opts.Params) &&
		len(m.Blocks) == len(e.project.Blocks) && len(m.Pending) == 0
}

// ApplyEdits clones the project, applies all edits atomically, propagates
// invalidation, and writes a durable pending plan. No block is changed until
// BakeInvalidated/IncrementalBake is called.
func (e *Engine) ApplyEdits(edits []Edit) ([]InvalidationItem, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.store.Manifest()
	if !e.baselineReadyLocked(m) {
		if !m.Complete && len(m.Pending) != 0 {
			return nil, &Error{Code: CodeState, Object: "checkpoint", Field: "pending", Message: "an unfinished bake must be resumed before new edits"}
		}
		return nil, &Error{Code: CodeState, Object: "engine", Field: "baseline", Message: "a complete baseline bake is required before incremental edits"}
	}
	next, err := cloneProject(e.project)
	if err != nil {
		return nil, err
	}
	inv, err := ApplyEdits(next, e.graph, edits)
	if err != nil {
		return nil, err
	}
	if len(inv) == 0 {
		return nil, nil
	}
	if err := ValidateProject(next); err != nil {
		return nil, err
	}
	dirty := make([]string, 0, len(inv))
	for _, item := range inv {
		dirty = append(dirty, item.BlockID)
	}
	dirty = e.graph.TopologicalOrder(dirty)
	order := e.graph.TopologicalOrder(canonicalBlockIDs(next))
	m.TargetContentHash = ProjectHash(next)
	m.AlgorithmVersion = AlgorithmVersion
	m.ParamsVersion = ParamsHash(e.opts.Params)
	m.Complete = false
	m.EpochDirty = append([]string(nil), dirty...)
	m.Pending = append([]string(nil), dirty...)
	m.CommitOrder = order
	m.Invalidation = append([]InvalidationItem(nil), inv...)
	if err := e.store.WriteManifest(m); err != nil {
		return nil, err
	}
	e.project = next
	e.graph = BuildDependencyGraph(next)
	return inv, nil
}

func (e *Engine) FullBake(ctx context.Context) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fullBakeLocked(ctx)
}

func (e *Engine) fullBakeLocked(ctx context.Context) (*BakeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := e.store.Reset(); err != nil {
		return nil, err
	}
	e.outputs = make(map[string]*BlockOutput)
	all := e.graph.TopologicalOrder(canonicalBlockIDs(e.project))
	inv := make([]InvalidationItem, 0, len(all))
	for _, id := range all {
		inv = append(inv, InvalidationItem{BlockID: id, Reason: "full bake requested", Chain: []string{id}})
	}
	m := emptyManifest()
	m.TargetContentHash = ProjectHash(e.project)
	m.AlgorithmVersion = AlgorithmVersion
	m.ParamsVersion = ParamsHash(e.opts.Params)
	m.Complete = false
	m.EpochDirty = append([]string(nil), all...)
	m.Pending = append([]string(nil), all...)
	m.CommitOrder = append([]string(nil), all...)
	m.Invalidation = append([]InvalidationItem(nil), inv...)
	if err := e.store.WriteManifest(m); err != nil {
		return nil, err
	}
	return e.runLocked(ctx, m, all, all, inv)
}

// IncrementalBake applies edits and bakes only the affected seam-connected
// component, reusing all unaffected checkpoint blocks.
func (e *Engine) IncrementalBake(ctx context.Context, edits []Edit) (*BakeResult, error) {
	inv, err := e.ApplyEdits(edits)
	if err != nil {
		return nil, err
	}
	if len(inv) == 0 {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.reuseResultLocked()
	}
	return e.BakeInvalidated(ctx)
}

// BakeInvalidated continues the durable pending plan created by ApplyEdits.
func (e *Engine) BakeInvalidated(ctx context.Context) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.store.Manifest()
	if m.Complete || len(m.Pending) == 0 {
		return e.reuseResultLocked()
	}
	if m.TargetContentHash != ProjectHash(e.project) {
		return nil, &Error{Code: CodeState, Object: "checkpoint", Field: "target_content_hash", Message: "pending checkpoint targets a different project content"}
	}
	if containsString(m.EpochDirty, "") || len(m.EpochDirty) == 0 {
		m.EpochDirty = append([]string(nil), m.Pending...)
	}
	return e.runLocked(ctx, m, m.EpochDirty, m.Pending, m.Invalidation)
}

// Resume restarts from a durable checkpoint. If the engine detected a global
// hash/version mismatch, it performs a complete re-bake instead of mixing old
// and new blocks.
func (e *Engine) Resume(ctx context.Context) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.store.Manifest()
	if e.snapshotReset {
		res, err := e.fullBakeLocked(ctx)
		if res != nil {
			res.SnapshotReset = true
			res.SnapshotResetWhy = e.snapshotResetWhy
		}
		return res, err
	}
	if m.AlgorithmVersion != AlgorithmVersion || m.ParamsVersion != ParamsHash(e.opts.Params) {
		e.snapshotReset = true
		e.snapshotResetWhy = "checkpoint algorithm or parameter version mismatch"
		res, err := e.fullBakeLocked(ctx)
		if res != nil {
			res.SnapshotReset = true
			res.SnapshotResetWhy = e.snapshotResetWhy
		}
		return res, err
	}
	if m.TargetContentHash == "" && len(m.Blocks) == 0 {
		return e.fullBakeLocked(ctx)
	}
	if m.TargetContentHash != ProjectHash(e.project) {
		e.snapshotReset = true
		e.snapshotResetWhy = "global content hash mismatch"
		res, err := e.fullBakeLocked(ctx)
		if res != nil {
			res.SnapshotReset = true
			res.SnapshotResetWhy = e.snapshotResetWhy
		}
		return res, err
	}
	if m.Complete && len(m.Pending) == 0 {
		return e.reuseResultLocked()
	}
	if len(m.Pending) == 0 {
		return nil, &Error{Code: CodeState, Object: "checkpoint", Field: "pending", Message: "checkpoint is incomplete but has no pending blocks"}
	}
	if len(m.EpochDirty) == 0 {
		m.EpochDirty = append([]string(nil), m.Pending...)
	}
	return e.runLocked(ctx, m, m.EpochDirty, m.Pending, m.Invalidation)
}

func (e *Engine) reuseResultLocked() (*BakeResult, error) {
	m := e.store.Manifest()
	res := e.baseResultLocked(m)
	for _, id := range m.CommitOrder {
		cp, ok := m.Blocks[id]
		if !ok {
			if m.Complete {
				return nil, &Error{Code: CodeState, Object: "checkpoint", Field: "blocks", BlockID: id, Message: "complete checkpoint is missing a block"}
			}
			continue
		}
		out, ok := e.outputs[id]
		if !ok {
			var err error
			out, err = e.store.LoadBlock(cp)
			if err != nil {
				return nil, err
			}
			e.outputs[id] = out
		}
		res.CommitSequence = append(res.CommitSequence, CommitEvent{BlockID: id, Kind: "reused"})
		res.Reused = append(res.Reused, id)
		res.SeamStats[id] = out.Seam
	}
	return res, nil
}

func (e *Engine) baseResultLocked(m Manifest) *BakeResult {
	return &BakeResult{
		AlgorithmVersion: AlgorithmVersion,
		ParamsVersion:    ParamsHash(e.opts.Params),
		ContentHash:      m.TargetContentHash,
		Invalidation:     append([]InvalidationItem(nil), m.Invalidation...),
		Failed:           make(map[string]string),
		SeamStats:        make(map[string]SeamStats),
		SnapshotReset:    e.snapshotReset,
		SnapshotResetWhy: e.snapshotResetWhy,
	}
}

func (e *Engine) runLocked(ctx context.Context, m Manifest, bakeIDs, commitIDs []string, inv []InvalidationItem) (*BakeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	res := e.baseResultLocked(m)
	res.Failed = make(map[string]string)
	if m.Failures == nil {
		m.Failures = make(map[string]string)
	}
	res.Invalidation = append([]InvalidationItem(nil), inv...)
	if err := e.store.CleanTemps(); err != nil {
		return nil, err
	}
	defer func() { _ = e.store.CleanTemps() }()
	rawPaths, queue, failures, err := bakeRawTasks(ctx, e.project, e.opts.Params, bakeIDs, e.opts, e.store)
	res.Queue = append(res.Queue, queue...)
	for id, msg := range failures {
		res.Failed[id] = msg
	}
	if err != nil {
		m.Pending = append([]string(nil), commitIDs...)
		m.Complete = false
		m.Failures = cloneFailures(res.Failed)
		if werr := e.store.WriteManifest(m); werr != nil {
			return res, werr
		}
		return res, err
	}

	edgeRaw, err := loadEdgeNormals(e.project, e.opts.Params, bakeIDs, rawPaths)
	if err != nil {
		m.Pending = append([]string(nil), commitIDs...)
		m.Complete = false
		m.Failures = cloneFailures(res.Failed)
		_ = e.store.WriteManifest(m)
		return res, err
	}
	seamRes, err := resolveSeamsEdge(e.project, func(k pixelKey) (Vec3, bool) {
		v, ok := edgeRaw[k]
		return v, ok
	})
	if err != nil {
		m.Pending = append([]string(nil), commitIDs...)
		m.Complete = false
		m.Failures = cloneFailures(res.Failed)
		_ = e.store.WriteManifest(m)
		return res, err
	}
	for id := range seamRes.Stats {
		res.SeamStats[id] = seamRes.Stats[id]
	}

	commitSet := make(map[string]bool, len(commitIDs))
	for _, id := range commitIDs {
		commitSet[id] = true
	}
	limiter := newMemLimiter(e.opts.NormalMapMemoryBytes)
	remaining := append([]string(nil), commitIDs...)
	bakedCount := 0
	paused := false
	for _, id := range m.CommitOrder {
		if ctxErr := contextError(ctx); ctxErr != nil {
			paused = true
			break
		}
		if !commitSet[id] {
			cp, ok := m.Blocks[id]
			if !ok {
				continue
			}
			out, ok := e.outputs[id]
			if !ok {
				out, err = e.store.LoadBlock(cp)
				if err != nil {
					return res, err
				}
				e.outputs[id] = out
			}
			res.CommitSequence = append(res.CommitSequence, CommitEvent{BlockID: id, Kind: "reused"})
			res.Reused = append(res.Reused, id)
			res.SeamStats[id] = out.Seam
			delete(m.Failures, id)
			continue
		}
		rawPath, ok := rawPaths[id]
		if !ok {
			res.Failed[id] = "raw output is missing"
			m.Failures[id] = res.Failed[id]
			continue
		}
		pixels := 0
		if b, ok := FindBlock(e.project, id); ok {
			pixels = b.Width * b.Height
		}
		reserve := estimateFinalBytes(pixels)
		waited, aerr := limiter.acquire(ctx, reserve)
		if aerr != nil {
			res.Failed[id] = aerr.Error()
			m.Failures[id] = res.Failed[id]
			paused = true
			break
		}
		if waited {
			res.Queue = append(res.Queue, QueueEvent{BlockID: id, Reason: "normal_map_finalize_wait"})
		}
		var out *BlockOutput
		func() {
			defer limiter.release(reserve)
			e.attempt[id]++
			if e.opts.FaultInjector != nil {
				if ferr := e.opts.FaultInjector(id, e.attempt[id]); ferr != nil {
					res.Failed[id] = ferr.Error()
					return
				}
			}
			data, rerr := readFile(rawPath)
			if rerr != nil {
				res.Failed[id] = rerr.Error()
				return
			}
			raw, derr := decodeRawBinary(data)
			if derr != nil {
				res.Failed[id] = derr.Error()
				return
			}
			raw.BlockID = id
			stats := seamRes.Stats[id]
			stats.BlockID = id
			cpHash := BlockContentHash(e.project, id, e.opts.Params, AlgorithmVersion)
			out, derr = finalizeBlock(raw, seamRes.Overrides, cpHash, stats)
			if derr != nil {
				res.Failed[id] = derr.Error()
				out = nil
			}
		}()
		if out == nil {
			m.Failures[id] = res.Failed[id]
			continue
		}
		cp := BlockCheckpoint{
			BlockID: id, Width: out.Width, Height: out.Height,
			NormalDigest: out.NormalDigest, SlopeDigest: out.SlopeDigest, DataDigest: out.DataDigest,
			ContentHash: out.ContentHash, ParamsVersion: ParamsHash(e.opts.Params),
			AlgorithmVersion: AlgorithmVersion, Seam: out.Seam,
		}
		remaining = removeString(remaining, id)
		m.Pending = append([]string(nil), remaining...)
		m.Blocks[id] = cp
		if err := e.store.CommitBlock(out, cp, m); err != nil {
			return res, err
		}
		e.outputs[id] = out
		res.Baked = append(res.Baked, id)
		res.CommitSequence = append(res.CommitSequence, CommitEvent{BlockID: id, Kind: "baked"})
		res.SeamStats[id] = out.Seam
		delete(m.Failures, id)
		bakedCount++
		if e.opts.PauseAfter > 0 && bakedCount >= e.opts.PauseAfter {
			paused = true
			break
		}
	}
	res.Paused = len(res.Failed) != 0 || len(remaining) != 0
	if paused && len(remaining) == 0 && len(res.Failed) == 0 {
		// A pause that happened exactly after the last commit is not a
		// checkpoint pause: the snapshot is complete.
		res.Paused = false
	}
	if !res.Paused {
		m.Pending = nil
		m.Complete = true
		m.EpochDirty = nil
		m.Failures = nil
	} else {
		m.Complete = false
		m.Pending = append([]string(nil), remaining...)
		if len(res.Failed) == 0 {
			m.Failures = nil
		} else {
			m.Failures = cloneFailures(res.Failed)
		}
	}
	if err := e.store.WriteManifest(m); err != nil {
		return res, err
	}
	if !res.Paused {
		all, err := e.loadAllOutputsLocked()
		if err != nil {
			return res, err
		}
		e.outputs = all
		res.SeamStats = make(map[string]SeamStats, len(all))
		for id, out := range all {
			res.SeamStats[id] = out.Seam
		}
	}
	if len(res.Failed) != 0 {
		ids := make([]string, 0, len(res.Failed))
		for id := range res.Failed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return res, &Error{
			Code: CodeBake, Object: "block:" + ids[0], Field: "bake", BlockID: ids[0],
			Message: "one or more blocks failed; successful blocks were committed and failed blocks remain retryable",
			Details: map[string]any{"failed": res.Failed},
		}
	}
	return res, nil
}

func cloneFailures(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func readFile(path string) ([]byte, error) {
	return osReadFile(path)
}

// osReadFile is a variable only to keep runLocked's failure paths easy to
// test without changing public APIs.
var osReadFile = func(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func removeString(in []string, v string) []string {
	out := in[:0]
	for _, x := range in {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func containsString(in []string, v string) bool {
	for _, x := range in {
		if x == v {
			return true
		}
	}
	return false
}

func (e *Engine) loadAllOutputsLocked() (map[string]*BlockOutput, error) {
	m := e.store.Manifest()
	out := make(map[string]*BlockOutput, len(m.Blocks))
	for id, cp := range m.Blocks {
		if cached, ok := e.outputs[id]; ok && cached.ContentHash == cp.ContentHash {
			out[id] = cached
			continue
		}
		block, err := e.store.LoadBlock(cp)
		if err != nil {
			return nil, err
		}
		out[id] = block
	}
	return out, nil
}

// Outputs returns a map of all committed outputs.
func (e *Engine) Outputs() (map[string]*BlockOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.store.Manifest()
	if !m.Complete {
		return nil, &Error{Code: CodeState, Object: "checkpoint", Field: "complete", Message: "outputs are unavailable until the checkpoint is complete"}
	}
	all, err := e.loadAllOutputsLocked()
	if err != nil {
		return nil, err
	}
	e.outputs = all
	return all, nil
}

// VerifyTruth checks the project's analytic truth list against committed PNG
// and float outputs.
func (e *Engine) VerifyTruth() (int, error) {
	all, err := e.Outputs()
	if err != nil {
		return 0, err
	}
	return VerifyTruth(e.project, all)
}

// Close releases no OS resources today, but provides a stable lifecycle API.
func (e *Engine) Close() error { return nil }
