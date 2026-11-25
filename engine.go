package textureconv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

type EngineOptions struct {
	ImageID              string
	SourceHash           string
	ParamsVersion        string
	TileSize             int
	Workers              int
	BufferBytesPerThread int
	Store                *Store
	FaultInjector        func(entry, tile int) error
}

func normalizeOptions(o EngineOptions) (EngineOptions, error) {
	if o.TileSize == 0 {
		o.TileSize = DefaultTileSize
	}
	if o.Workers == 0 {
		o.Workers = DefaultWorkers
	}
	if o.BufferBytesPerThread == 0 {
		o.BufferBytesPerThread = DefaultBufferBytesPerThread
	}
	if o.ParamsVersion == "" {
		o.ParamsVersion = "v1"
	}
	if o.ImageID == "" {
		o.ImageID = "image"
	}
	if o.TileSize < 1 || o.TileSize > 8192 {
		return o, paramErr(0, "tileSize", "must be in [1,8192]")
	}
	if o.Workers < 1 || o.Workers > 256 {
		return o, paramErr(0, "workers", "must be in [1,256]")
	}
	maxTileBytes := o.TileSize * o.TileSize * 4 * 2 * 2
	if o.BufferBytesPerThread < maxTileBytes {
		return o, paramErr(0, "bufferBytesPerThread",
			"must be at least %d bytes for tileSize=%d", maxTileBytes, o.TileSize)
	}
	return o, nil
}

type Submission struct {
	Tile        int    `json:"tile"`
	Entry       int    `json:"entry"`
	CacheKey    string `json:"cacheKey"`
	ContentHash string `json:"contentHash"`
}

type TileFailure struct {
	Tile    int    `json:"tile"`
	Entry   int    `json:"entry"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

type RunReport struct {
	Progress        Progress      `json:"progress"`
	CacheHits       int           `json:"cacheHits"`
	CacheMisses     int           `json:"cacheMisses"`
	ReusedTiles     int           `json:"reusedTiles"`
	RecomputedTiles int           `json:"recomputedTiles"`
	Submitted       []Submission  `json:"submitted"`
	Failures        []TileFailure `json:"failures,omitempty"`
	BufferWaits     []BufferWait  `json:"bufferWaits,omitempty"`
	Invalidated     bool          `json:"invalidated,omitempty"`
	InvalidatedWhy  string        `json:"invalidatedWhy,omitempty"`
}

type EditOptions struct {
	// AllowUnfreeze makes the caller explicitly accept deletion of affected
	// committed chunks. Without it, edits touching frozen chunks fail E_STATE.
	AllowUnfreeze bool
}

type Engine struct {
	mu         sync.Mutex
	source     *Image
	plan       *Plan
	graph      *DependencyGraph
	tiles      []Tile
	state      *StateMachine
	results    map[stageTileID]StageRecord
	version    VersionInfo
	opts       EngineOptions
	limiter    *BufferLimiter
	processing bool

	loadedInvalidated bool
	loadedReason      string
}

func NewEngine(source *Image, plan *Plan, opts EngineOptions) (*Engine, error) {
	if source == nil {
		return nil, schemaErr(0, "image", "nil source image")
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, schemaErr(0, "plan", "nil plan")
	}
	if err := plan.ValidateForImage(source); err != nil {
		return nil, err
	}
	var err error
	opts, err = normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	hp := clonePlan(plan)
	graph, err := BuildDependencyGraph(hp, source, opts.TileSize)
	if err != nil {
		return nil, err
	}
	sourceHash := opts.SourceHash
	if sourceHash == "" {
		sourceHash = HashSourceImage(source)
	}
	version := VersionInfo{
		ImageID: opts.ImageID, SourceHash: sourceHash,
		PlanFingerprint: hp.Fingerprint(), ParamsVersion: opts.ParamsVersion,
		AlgorithmVersion: AlgorithmVersion,
		Geometry:         fmt.Sprintf("%dx%d/tile=%d/channels=4", source.Width, source.Height, opts.TileSize),
	}
	e := &Engine{
		source: source, plan: hp, graph: graph, tiles: graph.Tiles,
		results: map[stageTileID]StageRecord{}, opts: opts, version: version,
		state: NewStateMachine(planEntrySeqs(hp), len(graph.Tiles)),
	}
	e.limiter = NewBufferLimiter(opts.BufferBytesPerThread * opts.Workers)
	if opts.Store != nil {
		commits, invalidated, reason, err := opts.Store.Prepare(version)
		if err != nil {
			return nil, err
		}
		e.loadedInvalidated = invalidated
		e.loadedReason = reason
		if !invalidated {
			if err := e.loadCommits(commits); err != nil {
				e.loadedInvalidated = true
				e.loadedReason = err.Error()
				e.results = map[stageTileID]StageRecord{}
				e.state = NewStateMachine(planEntrySeqs(hp), len(graph.Tiles))
				if err := opts.Store.Wipe(); err != nil {
					return nil, err
				}
				if _, _, _, err := opts.Store.Prepare(version); err != nil {
					return nil, err
				}
			}
		}
	}
	return e, nil
}

func (e *Engine) loadCommits(commits []TileCommit) error {
	if len(commits) == 0 {
		return nil
	}
	byTile := map[int]TileCommit{}
	for _, c := range commits {
		if c.Tile < 0 || c.Tile >= len(e.tiles) {
			return fmt.Errorf("snapshot tile %d is outside geometry", c.Tile)
		}
		byTile[c.Tile] = c
	}
	for tileIdx, c := range byTile {
		t := e.tiles[tileIdx]
		if len(c.Stages) > len(e.plan.Entries) {
			return fmt.Errorf("tile %d has more stages than current plan", tileIdx)
		}
		prevHash := sourceTileHash(e.source, t)
		prevKey := ""
		for i, r := range c.Stages {
			entry := &e.plan.Entries[i]
			if r.Entry != entry.Seq {
				return fmt.Errorf("tile %d stage %d has entry %d, want %d", tileIdx, i, r.Entry, entry.Seq)
			}
			node := e.graph.Nodes[i]
			if r.X != t.X || r.Y != t.Y || r.W != t.W || r.H != t.H {
				return fmt.Errorf("tile %d stage %d geometry differs", tileIdx, entry.Seq)
			}
			if r.BitDepth != node.TargetDepth || r.Channels != node.TargetChannels || r.AlphaIndex != node.AlphaIndex {
				return fmt.Errorf("tile %d stage %d shape metadata differs", tileIdx, entry.Seq)
			}
			params := EffectiveEntryFingerprint(*entry, t)
			wantKey := CacheKey(entry.Seq, tileIdx, params, prevHash, prevKey)
			if r.CacheKey != wantKey {
				return fmt.Errorf("tile %d stage %d cache key mismatch", tileIdx, entry.Seq)
			}
			st := r.StageImage()
			if err := st.Validate(); err != nil {
				return fmt.Errorf("tile %d stage %d invalid: %w", tileIdx, entry.Seq, err)
			}
			e.results[stageTileID{Entry: r.Entry, Tile: tileIdx}] = r
			e.state.MarkCommitted(r.Entry, tileIdx)
			prevHash = r.ContentHash
			prevKey = r.CacheKey
		}
	}
	return nil
}

// HashSourceImage hashes semantic source content independent of the XIMG file
// container.
func HashSourceImage(im *Image) string {
	h := sha256.New()
	writeHashHeader(h, im.Width, im.Height, im.BitDepth, 4, 3)
	data, _ := json.Marshal(im.Layout)
	_, _ = h.Write(data)
	var b [2]byte
	for _, v := range im.Pixels {
		b[0] = byte(v)
		b[1] = byte(v >> 8)
		_, _ = h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func planEntrySeqs(p *Plan) []int {
	out := make([]int, len(p.Entries))
	for i, e := range p.Entries {
		out[i] = e.Seq
	}
	return out
}

func clonePlan(p *Plan) *Plan {
	if p == nil {
		return nil
	}
	cp := &Plan{SchemaVersion: p.SchemaVersion, Entries: make([]Entry, len(p.Entries))}
	for i, e := range p.Entries {
		cp.Entries[i] = normalizedEntry(e)
	}
	return cp
}

func (e *Engine) Plan() *Plan             { return clonePlan(e.plan) }
func (e *Engine) Graph() *DependencyGraph { return e.graph }
func (e *Engine) Tiles() []Tile           { return append([]Tile(nil), e.tiles...) }
func (e *Engine) Progress() Progress      { return e.state.Progress() }
func (e *Engine) Version() VersionInfo    { return e.version }
func (e *Engine) InvalidatedAtLoad() (bool, string) {
	return e.loadedInvalidated, e.loadedReason
}

func (e *Engine) State(entry, tile int) TileState { return e.state.State(entry, tile) }

func (e *Engine) Transition(entry, tile int, to TileState) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.processing {
		return stateErr(entry, tile, "manual transition is unavailable while processing")
	}
	return e.state.Transition(entry, tile, to)
}

func (e *Engine) tileNeedsWork(tile int) bool {
	for _, entry := range e.plan.Entries {
		if e.state.State(entry.Seq, tile) != StateCommitted {
			return true
		}
	}
	return false
}

type tileBuild struct {
	tile    Tile
	stages  map[int]StageRecord
	started []int
	hits    int
	misses  int
	err     error
}

func (e *Engine) getCommitted(entry, tile int) (StageRecord, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.results[stageTileID{Entry: entry, Tile: tile}]
	return r, ok
}

func (e *Engine) runBatch(ctx context.Context, batch []Tile) []tileBuild {
	out := make([]tileBuild, len(batch))
	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = e.buildTile(ctx, batch[i])
		}(i)
	}
	wg.Wait()
	return out
}

func (e *Engine) buildTile(ctx context.Context, t Tile) tileBuild {
	b := tileBuild{tile: t, stages: make(map[int]StageRecord, len(e.plan.Entries))}
	needed := t.W * t.H * 4 * 2 * 2
	if err := e.limiter.Acquire(ctx, needed, fmt.Sprintf("tile %d pixel input/output buffers", t.Index)); err != nil {
		b.err = &Error{Code: CodeProcess, Tile: t.Index, Field: "buffer", Msg: err.Error(), Err: err}
		return b
	}
	defer e.limiter.Release(needed)
	for i := range e.plan.Entries {
		entry := e.plan.Entries[i]
		if e.state.State(entry.Seq, t.Index) == StateCommitted {
			r, ok := e.getCommitted(entry.Seq, t.Index)
			if !ok {
				b.err = stateErr(entry.Seq, t.Index, "committed state has no cached result")
				e.failStarted(&b)
				return b
			}
			b.stages[entry.Seq] = r
			b.hits++
			continue
		}
		if err := ctx.Err(); err != nil {
			b.err = &Error{Code: CodeProcess, Entry: entry.Seq, Tile: t.Index, Field: "context", Msg: "run canceled", Err: err}
			e.failStarted(&b)
			return b
		}
		from := e.state.State(entry.Seq, t.Index)
		if from != StatePending && from != StateFailed {
			b.err = stateErr(entry.Seq, t.Index, "cannot start from state %s", from)
			e.failStarted(&b)
			return b
		}
		if err := e.state.Transition(entry.Seq, t.Index, StateProcessing); err != nil {
			b.err = err
			e.failStarted(&b)
			return b
		}
		b.started = append(b.started, entry.Seq)
		if e.opts.FaultInjector != nil {
			if err := e.opts.FaultInjector(entry.Seq, t.Index); err != nil {
				b.err = &Error{Code: CodeProcess, Entry: entry.Seq, Tile: t.Index, Field: "fault", Msg: err.Error(), Err: err}
				e.failStarted(&b)
				return b
			}
		}
		var input *StageImage
		inputHash := ""
		upstreamKey := ""
		if i == 0 {
			input = extractSourceTile(e.source, t)
			inputHash = sourceTileHash(e.source, t)
		} else {
			prev := b.stages[e.plan.Entries[i-1].Seq]
			input = prev.StageImage()
			inputHash = prev.ContentHash
			upstreamKey = prev.CacheKey
		}
		out, err := convertEntryTileLocal(input, e.source.Width, e.source.Height, t, entry)
		if err != nil {
			b.err = normalizeProcessError(err, entry.Seq, t.Index)
			e.failStarted(&b)
			return b
		}
		paramsHash := EffectiveEntryFingerprint(entry, t)
		key := CacheKey(entry.Seq, t.Index, paramsHash, inputHash, upstreamKey)
		rec := StageRecord{
			Tile: t.Index, Entry: entry.Seq, X: t.X, Y: t.Y, W: t.W, H: t.H,
			BitDepth: out.BitDepth, Channels: out.Channels, AlphaIndex: out.AlphaIndex,
			CacheKey: key, ContentHash: stageTileHash(out, t),
			PlanFingerprint: e.version.PlanFingerprint, ParamsVersion: e.version.ParamsVersion,
			AlgorithmVersion: e.version.AlgorithmVersion,
			Pixels:           out.Pixels, Dropped: out.Dropped,
		}
		b.stages[entry.Seq] = rec
		b.misses++
	}
	return b
}

func normalizeProcessError(err error, entry, tile int) error {
	var pe *Error
	if asError(err, &pe) {
		if pe.Entry == 0 {
			pe.Entry = entry
		}
		if pe.Tile < 0 {
			pe.Tile = tile
		}
		return pe
	}
	return &Error{Code: CodeProcess, Entry: entry, Tile: tile, Field: "convert", Msg: err.Error(), Err: err}
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func (e *Engine) failStarted(b *tileBuild) {
	for _, entry := range b.started {
		if e.state.State(entry, b.tile.Index) == StateProcessing {
			_ = e.state.Transition(entry, b.tile.Index, StateFailed)
		}
	}
}

// Process advances all noncommitted tiles once. Successful tiles are persisted
// and committed; failed tiles are isolated and may be retried by another call.
func (e *Engine) Process(ctx context.Context) (RunReport, error) {
	return e.process(ctx, 0)
}

// ProcessLimit processes at most maxTiles tiles that need work. It is useful
// for deterministic interruption and resume demonstrations.
func (e *Engine) ProcessLimit(ctx context.Context, maxTiles int) (RunReport, error) {
	if maxTiles <= 0 {
		return RunReport{}, paramErr(0, "maxTiles", "must be positive")
	}
	return e.process(ctx, maxTiles)
}

func (e *Engine) process(ctx context.Context, maxTiles int) (RunReport, error) {
	e.mu.Lock()
	if e.processing {
		e.mu.Unlock()
		return RunReport{}, stateErr(0, 0, "engine is already processing")
	}
	e.processing = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.processing = false
		e.mu.Unlock()
	}()
	report := RunReport{Invalidated: e.loadedInvalidated, InvalidatedWhy: e.loadedReason}
	var failures []TileFailure
	attempted := map[int]bool{}
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		var candidates []Tile
		for _, t := range e.tiles {
			if attempted[t.Index] {
				continue
			}
			if e.tileNeedsWork(t.Index) {
				candidates = append(candidates, t)
			}
		}
		if len(candidates) == 0 {
			break
		}
		batchN := min(e.opts.Workers, len(candidates))
		if maxTiles > 0 {
			batchN = min(batchN, maxTiles-len(attempted))
		}
		if batchN <= 0 {
			break
		}
		batch := candidates[:batchN]
		builds := e.runBatch(ctx, batch)
		sort.Slice(builds, func(i, j int) bool { return builds[i].tile.Index < builds[j].tile.Index })
		for i := range builds {
			b := &builds[i]
			attempted[b.tile.Index] = true
			report.CacheHits += b.hits
			report.CacheMisses += b.misses
			if b.err != nil {
				failures = append(failures, failureFromError(b.err, b.tile.Index))
				continue
			}
			if err := e.commitBuild(*b); err != nil {
				e.failStarted(b)
				failures = append(failures, failureFromError(err, b.tile.Index))
				continue
			}
			report.RecomputedTiles++
			for _, seq := range b.started {
				report.Submitted = append(report.Submitted, Submission{
					Tile: b.tile.Index, Entry: seq,
					CacheKey: b.stages[seq].CacheKey, ContentHash: b.stages[seq].ContentHash,
				})
			}
		}
		if ctx.Err() != nil {
			break
		}
		if maxTiles > 0 && len(attempted) >= maxTiles {
			break
		}
	}
	report.Progress = e.state.Progress()
	report.Failures = failures
	report.BufferWaits = e.limiter.Waits()
	// Reused tiles are those fully committed before this run.
	for _, t := range e.tiles {
		all := true
		for _, entry := range e.plan.Entries {
			if e.state.State(entry.Seq, t.Index) != StateCommitted {
				all = false
				break
			}
		}
		if all && !containsBuild(buildsOf(attempted), t.Index) {
			report.ReusedTiles++
		}
	}
	if len(failures) > 0 {
		return report, &Error{
			Code: CodeProcess, Entry: failures[0].Entry, Tile: failures[0].Tile,
			Field: "process", Msg: fmt.Sprintf("%d tile(s) failed; committed tiles remain durable", len(failures)),
		}
	}
	if err := ctx.Err(); err != nil {
		return report, &Error{Code: CodeProcess, Tile: -1, Field: "context", Msg: "run canceled", Err: err}
	}
	return report, nil
}

func buildsOf(attempted map[int]bool) []int {
	out := make([]int, 0, len(attempted))
	for k := range attempted {
		out = append(out, k)
	}
	return out
}

func containsBuild(v []int, tile int) bool {
	for _, x := range v {
		if x == tile {
			return true
		}
	}
	return false
}

func failureFromError(err error, tile int) TileFailure {
	f := TileFailure{Tile: tile, Code: CodeOf(err), Message: err.Error()}
	var e *Error
	if asError(err, &e) {
		if e.Tile >= 0 {
			f.Tile = e.Tile
		}
		f.Entry = e.Entry
		if f.Code == "" {
			f.Code = e.Code
		}
	}
	if f.Code == "" {
		f.Code = CodeProcess
	}
	return f
}

func (e *Engine) commitBuild(b tileBuild) error {
	c := TileCommit{Tile: b.tile.Index, Stages: make([]StageRecord, 0, len(e.plan.Entries))}
	for _, entry := range e.plan.Entries {
		rec, ok := b.stages[entry.Seq]
		if !ok {
			return stateErr(entry.Seq, b.tile.Index, "tile build is missing a stage")
		}
		if rec.PlanFingerprint != e.version.PlanFingerprint ||
			rec.ParamsVersion != e.version.ParamsVersion ||
			rec.AlgorithmVersion != e.version.AlgorithmVersion {
			return stateErr(entry.Seq, b.tile.Index, "stage build uses stale version metadata")
		}
		c.Stages = append(c.Stages, rec)
	}
	if e.opts.Store != nil {
		if _, err := e.opts.Store.CommitTile(e.version, c); err != nil {
			return err
		}
	}
	e.mu.Lock()
	for _, rec := range c.Stages {
		e.results[stageTileID{Entry: rec.Entry, Tile: rec.Tile}] = rec
	}
	e.mu.Unlock()
	for _, seq := range b.started {
		if err := e.state.Transition(seq, b.tile.Index, StateCommitted); err != nil {
			return err
		}
	}
	return nil
}

// ApplyEdit is the only supported live-plan mutation. Affected committed
// chunks are never rewritten unless AllowUnfreeze explicitly authorizes it.
func (e *Engine) ApplyEdit(newPlan *Plan, opts EditOptions) (*InvalidationManifest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.processing {
		return nil, stateErr(0, 0, "cannot edit a plan while processing")
	}
	if newPlan == nil {
		return nil, schemaErr(0, "plan", "nil plan")
	}
	if err := newPlan.ValidateForImage(e.source); err != nil {
		return nil, err
	}
	np := clonePlan(newPlan)
	newGraph, err := BuildDependencyGraph(np, e.source, e.opts.TileSize)
	if err != nil {
		return nil, err
	}
	manifest, err := ComputeInvalidation(e.plan, np, e.source, e.opts.TileSize)
	if err != nil {
		return nil, err
	}
	if manifest.FirstChangedPosition < 0 {
		return manifest, nil
	}
	affectedTiles := map[int]map[int]bool{}
	frozenTiles := map[int]bool{}
	for _, a := range manifest.Affected {
		if affectedTiles[a.Entry] == nil {
			affectedTiles[a.Entry] = map[int]bool{}
		}
		for _, tile := range a.Tiles {
			affectedTiles[a.Entry][tile] = true
			if e.state.State(a.Entry, tile) == StateCommitted {
				frozenTiles[tile] = true
			}
		}
	}
	if len(frozenTiles) > 0 && !opts.AllowUnfreeze {
		tiles := sortedKeys(frozenTiles)
		return manifest, stateErr(0, tiles[0], "edit touches frozen committed chunks %v; set AllowUnfreeze to discard them explicitly", tiles)
	}
	keep := map[int]bool{}
	for _, entry := range np.Entries {
		keep[entry.Seq] = true
	}
	nextResults := make(map[stageTileID]StageRecord, len(e.results))
	for id, rec := range e.results {
		if !keep[id.Entry] || frozenTiles[id.Tile] {
			continue
		}
		if affectedTiles[id.Entry] != nil && affectedTiles[id.Entry][id.Tile] {
			continue
		}
		nextResults[id] = rec
	}
	newVersion := e.version
	newVersion.PlanFingerprint = np.Fingerprint()
	if e.opts.Store != nil {
		if err := e.opts.Store.UnfreezeAndRebase(newVersion, sortedKeys(frozenTiles)); err != nil {
			return nil, err
		}
	}
	for id, rec := range nextResults {
		rec.PlanFingerprint = newVersion.PlanFingerprint
		rec.ParamsVersion = newVersion.ParamsVersion
		rec.AlgorithmVersion = newVersion.AlgorithmVersion
		nextResults[id] = rec
	}
	e.results = nextResults
	e.plan = np
	e.version = newVersion
	e.graph = newGraph
	newState := NewStateMachine(planEntrySeqs(np), len(e.tiles))
	for id := range e.results {
		newState.MarkCommitted(id.Entry, id.Tile)
	}
	e.state = newState
	return manifest, nil
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// FullRecompute explicitly discards every checkpoint and recomputes all tiles.
func (e *Engine) FullRecompute(ctx context.Context) (RunReport, error) {
	e.mu.Lock()
	if e.processing {
		e.mu.Unlock()
		return RunReport{}, stateErr(0, 0, "engine is already processing")
	}
	if e.opts.Store != nil {
		if err := e.opts.Store.Wipe(); err != nil {
			e.mu.Unlock()
			return RunReport{}, err
		}
		if _, _, _, err := e.opts.Store.Prepare(e.version); err != nil {
			e.mu.Unlock()
			return RunReport{}, err
		}
	}
	e.results = map[stageTileID]StageRecord{}
	e.state = NewStateMachine(planEntrySeqs(e.plan), len(e.tiles))
	e.loadedInvalidated = false
	e.loadedReason = ""
	e.mu.Unlock()
	return e.Process(ctx)
}

type EntrySnapshot struct {
	Entry   int          `json:"entry"`
	Summary ImageSummary `json:"summary"`
}

type RunSnapshot struct {
	Stages      []*StageImage   `json:"-"`
	Entries     []EntrySnapshot `json:"entries"`
	Final       *StageImage     `json:"-"`
	Submissions []Submission    `json:"submissions"`
	Tiles       []Tile          `json:"tiles"`
}

// Snapshot assembles all committed stage images and the final image.
func (e *Engine) Snapshot() (*RunSnapshot, error) {
	e.mu.Lock()
	results := make(map[stageTileID]StageRecord, len(e.results))
	for k, v := range e.results {
		results[k] = v
	}
	e.mu.Unlock()
	snap := &RunSnapshot{Tiles: append([]Tile(nil), e.tiles...)}
	for i, entry := range e.plan.Entries {
		node := e.graph.Nodes[i]
		out := &StageImage{
			Width: e.source.Width, Height: e.source.Height, BitDepth: node.TargetDepth,
			Channels: node.TargetChannels, AlphaIndex: node.AlphaIndex,
			Pixels: make([]uint16, e.source.Width*e.source.Height*node.TargetChannels),
		}
		for _, t := range e.tiles {
			r, ok := results[stageTileID{Entry: entry.Seq, Tile: t.Index}]
			if !ok {
				return nil, stateErr(entry.Seq, t.Index, "stage is not committed")
			}
			st := r.StageImage()
			if err := placeStageTile(out, t, st); err != nil {
				return nil, &Error{Code: CodeProcess, Entry: entry.Seq, Tile: t.Index, Field: "assemble", Msg: err.Error()}
			}
			if len(out.Dropped) == 0 {
				out.Dropped = append([]DroppedChannel(nil), st.Dropped...)
			}
		}
		if err := out.Validate(); err != nil {
			return nil, err
		}
		snap.Stages = append(snap.Stages, out)
		snap.Entries = append(snap.Entries, EntrySnapshot{Entry: entry.Seq, Summary: summarizeStage(out)})
	}
	if len(snap.Stages) > 0 {
		snap.Final = snap.Stages[len(snap.Stages)-1]
	}
	for tile := 0; tile < len(e.tiles); tile++ {
		for _, entry := range e.plan.Entries {
			r, ok := results[stageTileID{Entry: entry.Seq, Tile: tile}]
			if !ok {
				return nil, stateErr(entry.Seq, tile, "stage is not committed")
			}
			snap.Submissions = append(snap.Submissions, Submission{
				Tile: tile, Entry: entry.Seq, CacheKey: r.CacheKey, ContentHash: r.ContentHash,
			})
		}
	}
	sort.Slice(snap.Submissions, func(i, j int) bool {
		if snap.Submissions[i].Tile != snap.Submissions[j].Tile {
			return snap.Submissions[i].Tile < snap.Submissions[j].Tile
		}
		return snap.Submissions[i].Entry < snap.Submissions[j].Entry
	})
	return snap, nil
}

// FinalImage is a convenience wrapper around Snapshot.
func (e *Engine) FinalImage() (*StageImage, error) {
	s, err := e.Snapshot()
	if err != nil {
		return nil, err
	}
	return s.Final, nil
}
