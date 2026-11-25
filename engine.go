package acousticfield

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var ErrAborted = errors.New("rebuild aborted after requested commit count")

type EditKind string

const (
	EditAbsorption       EditKind = "absorption"
	EditReflector        EditKind = "reflector"
	EditSourceEmission   EditKind = "source_emission"
	EditBoundaryTransfer EditKind = "boundary_transfer"
)

type Edit struct {
	Kind        EditKind
	ChunkID     int
	BandID      string
	Coefficient int64
	Reflector   *Reflector
	SourceID    string
	Emission    []BandEnergy
	TransferID  string
}

type InvalidatedChunk struct {
	ChunkID  int      `json:"chunk_id"`
	Distance int      `json:"distance"`
	Reasons  []string `json:"reasons"`
}

type BuildOptions struct {
	AbortAfterCommits int
	ForceFull         bool
}

type ConservationBand struct {
	SourceID    string `json:"source_id"`
	BandID      string `json:"band_id"`
	Emission    int64  `json:"emission"`
	ChunkEnergy int64  `json:"chunk_energy"`
	BoundaryNet int64  `json:"boundary_net_flow"`
	Escape      int64  `json:"escape"`
	Residual    int64  `json:"residual"`
}

type ConservationReport struct {
	OK          bool               `json:"ok"`
	MaxResidual int64              `json:"max_residual"`
	Bands       []ConservationBand `json:"bands"`
	Error       string             `json:"error,omitempty"`
}

type Submission struct {
	Sequence int        `json:"sequence"`
	ChunkID  int        `json:"chunk_id"`
	Reused   bool       `json:"reused"`
	State    ChunkState `json:"state"`
}

type RunReport struct {
	RoundID         int64              `json:"round_id"`
	SnapshotID      string             `json:"snapshot_id"`
	Results         []ChunkResult      `json:"results"`
	Invalidations   []InvalidatedChunk `json:"invalidations"`
	Submissions     []Submission       `json:"submissions"`
	QueueEvents     []QueueEvent       `json:"queue_events"`
	WorkerErrors    []WorkError        `json:"worker_errors"`
	Conservation    ConservationReport `json:"conservation"`
	TruthDiff       *Diff              `json:"truth_diff,omitempty"`
	Recomputed      int                `json:"recomputed_chunks"`
	CacheHits       int                `json:"cache_hits"`
	CommittedChunks int                `json:"committed_chunks"`
	CommittedRounds int                `json:"committed_rounds"`
	Aborted         bool               `json:"aborted"`
}

type RestoreReport struct {
	SnapshotID      string `json:"snapshot_id"`
	Loaded          []int  `json:"loaded_chunks"`
	InvalidSnapshot bool   `json:"invalid_snapshot"`
	Reason          string `json:"reason,omitempty"`
}

type Engine struct {
	mu              sync.Mutex
	level           *Level
	options         Options
	truth           *Truth
	store           *Store
	snapshotID      string
	state           *StateMachine
	results         map[int]ChunkResult
	plan            *EnergyPlan
	roundSeq        int64
	invalidations   []InvalidatedChunk
	queueEvents     []QueueEvent
	workerErrors    []WorkError
	cacheHits       int
	recomputed      int
	conservation    ConservationReport
	truthDiff       *Diff
	lastSubmissions []Submission
}

func NewEngine(level *Level, truth *Truth, options Options, store *Store) (*Engine, error) {
	if err := ValidateLevel(level); err != nil {
		return nil, err
	}
	options = options.Normalized()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	clone := CloneLevel(level)
	computed := ComputeTruth(clone, options)
	if truth == nil || truth.Fingerprint == "" {
		truth = &computed
	} else if truth.Fingerprint != computed.Fingerprint {
		return nil, &FieldError{
			Code: CodeValue, ObjectID: "truth", FieldPath: "fingerprint",
			Measured: truth.Fingerprint, Min: computed.Fingerprint, Max: computed.Fingerprint,
			Message: "supplied truth does not match the current level/options",
		}
	} else {
		cloned := cloneTruth(*truth)
		truth = &cloned
	}
	ids := chunkIDsSorted(clone)
	e := &Engine{
		level: clone, options: options, truth: truth, store: store,
		state: NewStateMachine(ids), results: make(map[int]ChunkResult),
		snapshotID: SnapshotID(clone, options),
	}
	return e, nil
}

func CloneLevel(level *Level) *Level {
	clone := *level
	clone.Bands = append([]Band(nil), level.Bands...)
	clone.Chunks = append([]Chunk(nil), level.Chunks...)
	for i := range clone.Chunks {
		clone.Chunks[i].Absorption = append([]Absorption(nil), level.Chunks[i].Absorption...)
		clone.Chunks[i].Reflectors = append([]Reflector(nil), level.Chunks[i].Reflectors...)
	}
	clone.Sources = append([]Source(nil), level.Sources...)
	for i := range clone.Sources {
		clone.Sources[i].Emission = append([]BandEnergy(nil), level.Sources[i].Emission...)
	}
	clone.Listeners = append([]Listener(nil), level.Listeners...)
	return &clone
}

func (e *Engine) Level() *Level {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CloneLevel(e.level)
}

func (e *Engine) Truth() Truth {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneTruth(*e.truth)
}

func cloneTruth(in Truth) Truth {
	out := in
	out.Bands = append([]string(nil), in.Bands...)
	out.Chunks = make([]TruthChunk, len(in.Chunks))
	for i, c := range in.Chunks {
		out.Chunks[i] = c
		out.Chunks[i].Energy = cloneBandEnergy(c.Energy)
		out.Chunks[i].SourceEnergy = make([]SourceEnergy, len(c.SourceEnergy))
		for j, se := range c.SourceEnergy {
			out.Chunks[i].SourceEnergy[j] = SourceEnergy{SourceID: se.SourceID, Energy: cloneBandEnergy(se.Energy)}
		}
	}
	out.Paths = make([]TruthPath, len(in.Paths))
	for i, p := range in.Paths {
		out.Paths[i] = p
		out.Paths[i].Reflectors = append([]string(nil), p.Reflectors...)
	}
	out.Listeners = make([]TruthListener, len(in.Listeners))
	for i, l := range in.Listeners {
		out.Listeners[i] = l
		out.Listeners[i].BandEnergy = cloneBandEnergy(l.BandEnergy)
	}
	out.Sources = make([]TruthSource, len(in.Sources))
	for i, s := range in.Sources {
		out.Sources[i] = s
		out.Sources[i].Emission = cloneBandEnergy(s.Emission)
		out.Sources[i].Escape = cloneBandEnergy(s.Escape)
	}
	return out
}

func (e *Engine) SnapshotID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotID
}

// Initialize persists/validates snapshot identity and restores durable commits.
func (e *Engine) Initialize(ctx context.Context) (RestoreReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	report := RestoreReport{SnapshotID: e.snapshotID}
	if e.store == nil {
		return report, nil
	}
	meta := SnapshotMeta{
		ID: e.snapshotID, LevelID: e.level.ID, LevelHash: LevelContentHash(e.level),
		TruthFingerprint: e.truth.Fingerprint, ParamsVersion: e.options.Version(),
		AlgorithmVersion: AlgorithmVersion,
	}
	reset, err := e.store.EnsureSnapshot(ctx, meta)
	if err != nil {
		return report, err
	}
	if reset {
		report.InvalidSnapshot = true
		report.Reason = "snapshot identity/version changed and was invalidated"
		e.state.Restore(nil, 0, false)
		return report, nil
	}
	meta, err = e.store.SnapshotMeta(ctx, e.snapshotID)
	if errors.Is(err, ErrNoSnapshot) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	loaded, valid, err := e.store.ValidateLoadedSnapshot(ctx, e.snapshotID, e.level, e.options, e.truth.Fingerprint)
	if err != nil || !valid {
		report.InvalidSnapshot = true
		report.Reason = fmt.Sprint(err)
		e.state.Restore(nil, meta.LastRound, false)
		e.roundSeq = meta.LastRound
		return report, nil
	}
	ids := make([]int, 0, len(loaded))
	for _, res := range loaded {
		e.results[res.ChunkID] = res
		ids = append(ids, res.ChunkID)
	}
	sort.Ints(ids)
	report.Loaded = append([]int(nil), ids...)
	e.roundSeq = meta.LastRound
	e.state.Restore(ids, meta.LastRound, meta.Complete)
	return report, nil
}

// ApplyEdit records the dependency source and invalidates the edited chunk plus
// every downstream receiver within the finite propagation horizon.
func (e *Engine) ApplyEdit(edit Edit) ([]InvalidatedChunk, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := CloneLevel(e.level)
	seeds := map[int][]string{}
	switch edit.Kind {
	case EditAbsorption:
		c, ok := chunkByID(next, edit.ChunkID)
		if !ok {
			return nil, valueError("edit", "chunk_id", edit.ChunkID, 0, len(next.Chunks)-1)
		}
		found := false
		for i := range c.Absorption {
			if c.Absorption[i].BandID == edit.BandID {
				c.Absorption[i].Coefficient = edit.Coefficient
				found = true
				break
			}
		}
		if !found {
			return nil, schemaError(fmt.Sprintf("chunk-%d", edit.ChunkID), "absorption", "unknown band "+edit.BandID)
		}
		seeds[edit.ChunkID] = append(seeds[edit.ChunkID], fmt.Sprintf("absorption_changed:%s", edit.BandID))
	case EditReflector:
		if edit.Reflector == nil {
			return nil, schemaError("edit", "reflector", "reflector payload is required")
		}
		c, ok := chunkByID(next, edit.ChunkID)
		if !ok {
			return nil, valueError("edit", "chunk_id", edit.ChunkID, 0, len(next.Chunks)-1)
		}
		found := false
		for i := range c.Reflectors {
			if c.Reflectors[i].ID == edit.Reflector.ID {
				c.Reflectors[i] = *edit.Reflector
				found = true
				break
			}
		}
		if !found {
			return nil, schemaError(fmt.Sprintf("chunk-%d", edit.ChunkID), "reflector", "unknown reflector "+edit.Reflector.ID)
		}
		sortReflectors(c.Reflectors)
		seeds[edit.ChunkID] = append(seeds[edit.ChunkID], "reflector_geometry_changed:"+edit.Reflector.ID)
	case EditSourceEmission:
		src, ok := sourceByID(next, edit.SourceID)
		if !ok {
			return nil, schemaError("edit", "source_id", "unknown source "+edit.SourceID)
		}
		src.Emission = cloneBandEnergy(edit.Emission)
		seeds[src.ChunkID] = append(seeds[src.ChunkID], "source_emission_changed:"+edit.SourceID)
	case EditBoundaryTransfer:
		chunkID := edit.ChunkID
		if chunkID < 0 {
			for _, res := range e.results {
				for _, t := range res.Incoming {
					if t.ID == edit.TransferID {
						chunkID = res.ChunkID
						break
					}
				}
			}
		}
		if chunkID < 0 {
			return nil, schemaError("edit", "transfer_id", "unknown boundary transfer "+edit.TransferID)
		}
		if _, ok := chunkByID(next, chunkID); !ok {
			return nil, valueError("edit", "chunk_id", chunkID, 0, len(next.Chunks)-1)
		}
		seeds[chunkID] = append(seeds[chunkID], "boundary_transfer_changed:"+edit.TransferID)
	default:
		return nil, schemaError("edit", "kind", "unknown edit kind "+string(edit.Kind))
	}
	if err := ValidateLevel(next); err != nil {
		return nil, err
	}
	editedTruth := ComputeTruth(next, e.options)
	e.level = next
	e.truth = &editedTruth
	invalidated := e.invalidateFromSeeds(seeds)
	e.invalidations = mergeInvalidations(e.invalidations, invalidated)
	if e.store != nil {
		ids := make([]int, 0, len(invalidated))
		for _, item := range invalidated {
			ids = append(ids, item.ChunkID)
		}
		if err := e.store.InvalidateChunks(context.Background(), e.snapshotID, ids,
			LevelContentHash(e.level), e.truth.Fingerprint, e.options.Version(), AlgorithmVersion); err != nil && !errors.Is(err, ErrNoSnapshot) {
			return nil, err
		}
	}
	return invalidated, nil
}

func (e *Engine) truthValue() {
	t := ComputeTruth(e.level, e.options)
	e.truth = &t
}

func (e *Engine) invalidateFromSeeds(seeds map[int][]string) []InvalidatedChunk {
	type queueItem struct {
		id       int
		distance int
	}
	reasons := make(map[int][]string)
	seen := make(map[int]int)
	var q []queueItem
	seedIDs := make([]int, 0, len(seeds))
	for id := range seeds {
		seedIDs = append(seedIDs, id)
	}
	sort.Ints(seedIDs)
	for _, id := range seedIDs {
		q = append(q, queueItem{id: id})
		seen[id] = 0
		reasons[id] = append(reasons[id], seeds[id]...)
	}
	nb := gridNeighbors(e.level)
	for len(q) > 0 {
		item := q[0]
		q = q[1:]
		if item.distance >= e.options.PropagationRounds {
			continue
		}
		sides := make([]string, 0, len(nb[item.id]))
		for side := range nb[item.id] {
			sides = append(sides, side)
		}
		sort.Strings(sides)
		for _, side := range sides {
			next := nb[item.id][side]
			d := item.distance + 1
			if old, ok := seen[next]; ok && old <= d {
				continue
			}
			seen[next] = d
			reasons[next] = append(reasons[next], fmt.Sprintf("downstream_of_chunk:%d", item.id))
			q = append(q, queueItem{id: next, distance: d})
		}
	}
	ids := make([]int, 0, len(reasons))
	for id := range reasons {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var out []InvalidatedChunk
	for _, id := range ids {
		if e.state.ChunkState(id) == ChunkCommitted || e.state.ChunkState(id) == ChunkFailed {
			_ = e.state.InvalidateChunk(id)
		}
		delete(e.results, id)
		out = append(out, InvalidatedChunk{ChunkID: id, Distance: seen[id], Reasons: uniqueStrings(reasons[id])})
	}
	return out
}

func uniqueStrings(v []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(v))
	for _, s := range v {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func mergeInvalidations(dst, add []InvalidatedChunk) []InvalidatedChunk {
	index := make(map[int]int, len(dst))
	for i := range dst {
		index[dst[i].ChunkID] = i
	}
	for _, item := range add {
		if i, ok := index[item.ChunkID]; ok {
			dst[i].Reasons = uniqueStrings(append(dst[i].Reasons, item.Reasons...))
			if item.Distance < dst[i].Distance || dst[i].Distance == 0 {
				dst[i].Distance = item.Distance
			}
			continue
		}
		item.Reasons = uniqueStrings(item.Reasons)
		index[item.ChunkID] = len(dst)
		dst = append(dst, item)
	}
	sort.Slice(dst, func(i, j int) bool { return dst[i].ChunkID < dst[j].ChunkID })
	return dst
}

// Rebuild performs an incremental rebuild using durable/committed results.
// Failed chunks are retried by the same call because only committed chunks are
// eligible for reuse.
func (e *Engine) Rebuild(ctx context.Context, build BuildOptions) (*RunReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	roundID := e.roundSeq + 1
	if err := e.state.RegisterRound(roundID); err != nil {
		return nil, err
	}
	if err := e.state.TransitionRound(roundID, RoundRebuilding); err != nil {
		return nil, err
	}
	e.roundSeq = roundID
	e.truthValue()
	if err := ValidateLevel(e.level); err != nil {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		return nil, err
	}
	plan, err := buildEnergyPlan(e.level, e.options)
	if err != nil {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		return nil, err
	}
	e.plan = plan
	order := BoundaryTopologicalOrder(e.level)

	if build.ForceFull {
		for _, id := range order {
			if e.state.ChunkState(id) == ChunkCommitted || e.state.ChunkState(id) == ChunkFailed {
				_ = e.state.InvalidateChunk(id)
			}
			delete(e.results, id)
			e.invalidations = mergeInvalidations(e.invalidations, []InvalidatedChunk{{ChunkID: id, Reasons: []string{"full_rebuild_requested"}}})
		}
	}

	reused := make(map[int]bool, len(order))
	for _, id := range order {
		res, ok := e.results[id]
		if ok && e.state.ChunkState(id) == ChunkCommitted && resultReusable(e.level, e.options, plan, res) {
			reused[id] = true
			continue
		}
		if ok {
			delete(e.results, id)
			if e.state.ChunkState(id) == ChunkCommitted {
				_ = e.state.InvalidateChunk(id)
			}
			e.invalidations = mergeInvalidations(e.invalidations, []InvalidatedChunk{{
				ChunkID: id, Reasons: []string{"cache_key_or_boundary_transfer_version_changed"},
			}})
		}
	}

	computed := make([]*ChunkResult, len(order))
	var jobs []WorkItem
	for i, id := range order {
		if reused[id] {
			continue
		}
		index := i
		chunkID := id
		jobs = append(jobs, WorkItem{
			ChunkID: chunkID,
			Run: func(jobCtx context.Context, workerID int) error {
				if err := e.state.TransitionChunk(chunkID, ChunkFiltering); err != nil {
					return err
				}
				res, err := SolveChunk(e.level, e.options, plan, chunkID, e.options.PathBufferPerWorker)
				if err != nil {
					_ = e.state.TransitionChunk(chunkID, ChunkFailed)
					return err
				}
				if err := e.state.TransitionChunk(chunkID, ChunkTransferring); err != nil {
					return err
				}
				res.RoundID = roundID
				res.Reused = false
				computed[index] = &res
				return nil
			},
		})
	}
	scheduler := NewParallelScheduler(e.options.MaxParallel, e.options.QueueBudget)
	summary := scheduler.Run(ctx, jobs)
	e.queueEvents = append([]QueueEvent(nil), summary.QueueEvents...)
	e.workerErrors = append([]WorkError(nil), summary.Errors...)
	for i := range computed {
		if computed[i] != nil {
			e.results[computed[i].ChunkID] = *computed[i]
		}
	}

	allAvailable := len(e.results) == len(order)
	var conservation ConservationReport
	var truthDiff *Diff
	var consErr error
	if allAvailable {
		results := e.resultsSorted(order)
		conservation = conservationDetails(e.level, e.options, results, plan)
		if !conservation.OK {
			consErr = &FieldError{
				Code: CodeCons, ObjectID: "level", FieldPath: "conservation",
				Values:  map[string]int64{"max_residual": conservation.MaxResidual},
				Message: conservation.Error,
			}
		} else {
			truthDiff = reconcileTruth(e.level, *e.truth, e.options, results)
		}
	}
	e.cacheHits = len(reused)
	e.recomputed = len(jobs)
	if allAvailable && (consErr != nil || truthDiff != nil) {
		for _, id := range order {
			if reused[id] {
				continue
			}
			if e.state.ChunkState(id) == ChunkTransferring {
				_ = e.state.TransitionChunk(id, ChunkFailed)
			}
			delete(e.results, id)
		}
		e.conservation = conservation
		e.truthDiff = truthDiff
		report := e.runReport(order, nil, conservation, truthDiff, false)
		_ = e.state.TransitionRound(roundID, RoundFailed)
		if e.store != nil {
			_ = e.store.MarkComplete(ctx, e.snapshotID, false)
		}
		if consErr != nil {
			return report, consErr
		}
		return report, diffError(truthDiff)
	}

	// Persist a deterministic commit sequence. Reused chunks are re-recorded
	// with reused=true and unchanged physics payloads.
	if e.store != nil {
		if err := e.store.BeginCommitRound(ctx, e.snapshotID, roundID); err != nil {
			_ = e.state.TransitionRound(roundID, RoundFailed)
			return nil, err
		}
	}
	var submissions []Submission
	aborted := false
	for _, id := range order {
		res, ok := e.results[id]
		if !ok {
			continue
		}
		if !reused[id] && e.state.ChunkState(id) == ChunkTransferring {
			if err := e.state.TransitionChunk(id, ChunkCommitted); err != nil {
				return nil, err
			}
		}
		res.RoundID = roundID
		res.Reused = reused[id]
		res.State = ChunkCommitted
		e.results[id] = res
		seq := len(submissions)
		if e.store != nil {
			if err := e.store.CommitChunk(ctx, e.snapshotID, roundID, seq, res, reused[id]); err != nil {
				_ = e.state.TransitionRound(roundID, RoundFailed)
				return nil, err
			}
		}
		submissions = append(submissions, Submission{Sequence: seq, ChunkID: id, Reused: reused[id], State: ChunkCommitted})
		if build.AbortAfterCommits > 0 && len(submissions) >= build.AbortAfterCommits {
			aborted = true
			break
		}
	}
	e.lastSubmissions = append([]Submission(nil), submissions...)
	e.conservation = conservation
	e.truthDiff = truthDiff

	report := e.runReport(order, submissions, conservation, truthDiff, aborted)
	if aborted {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		if e.store != nil {
			_ = e.store.MarkComplete(ctx, e.snapshotID, false)
		}
		return report, ErrAborted
	}
	if len(summary.Errors) > 0 || !allAvailable {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		if e.store != nil {
			_ = e.store.MarkComplete(ctx, e.snapshotID, false)
		}
		if len(summary.Errors) > 0 {
			return report, fmt.Errorf("%w: %v", &FieldError{Code: CodeState, ObjectID: "rebuild", FieldPath: "worker", Message: "one or more chunks failed"}, summary.Errors[0].Err)
		}
		return report, &FieldError{Code: CodeState, ObjectID: "rebuild", FieldPath: "chunks", Message: "not all chunks produced a result"}
	}
	if consErr != nil {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		if e.store != nil {
			_ = e.store.MarkComplete(ctx, e.snapshotID, false)
		}
		return report, consErr
	}
	if truthDiff != nil {
		_ = e.state.TransitionRound(roundID, RoundFailed)
		if e.store != nil {
			_ = e.store.MarkComplete(ctx, e.snapshotID, false)
		}
		return report, diffError(truthDiff)
	}
	if err := e.state.TransitionRound(roundID, RoundCommitted); err != nil {
		return report, err
	}
	if e.store != nil {
		if err := e.store.MarkComplete(ctx, e.snapshotID, true); err != nil {
			return report, err
		}
	}
	report.CommittedChunks, report.CommittedRounds = e.state.Progress()
	return e.runReport(order, submissions, conservation, truthDiff, false), nil
}

func (e *Engine) resultsSorted(order []int) []ChunkResult {
	out := make([]ChunkResult, 0, len(order))
	for _, id := range order {
		if res, ok := e.results[id]; ok {
			out = append(out, cloneChunkResult(res))
		}
	}
	return out
}

func (e *Engine) runReport(order []int, submissions []Submission, cons ConservationReport, diff *Diff, aborted bool) *RunReport {
	committedChunks, committedRounds := e.state.Progress()
	inv := make([]InvalidatedChunk, len(e.invalidations))
	for i, item := range e.invalidations {
		inv[i] = item
		inv[i].Reasons = append([]string(nil), item.Reasons...)
	}
	cons.Bands = append([]ConservationBand(nil), cons.Bands...)
	return &RunReport{
		RoundID: e.roundSeq, SnapshotID: e.snapshotID, Results: e.resultsSorted(order),
		Invalidations: inv, Submissions: append([]Submission(nil), submissions...),
		QueueEvents:  append([]QueueEvent(nil), e.queueEvents...),
		WorkerErrors: append([]WorkError(nil), e.workerErrors...),
		Conservation: cons, TruthDiff: diff, Recomputed: e.recomputed, CacheHits: e.cacheHits,
		CommittedChunks: committedChunks, CommittedRounds: committedRounds, Aborted: aborted,
	}
}

func resultReusable(level *Level, options Options, plan *EnergyPlan, res ChunkResult) bool {
	c, ok := chunkByID(level, res.ChunkID)
	if !ok || res.ContentHash != ChunkContentHash(c) || res.ParamsVersion != options.Version() || res.AlgorithmVersion != AlgorithmVersion {
		return false
	}
	if PathDigest(res.Paths) != res.PathDigest {
		return false
	}
	src, ok := sourceByChunk(level, res.ChunkID)
	if !ok {
		return false
	}
	hasSourceDep := false
	for _, dep := range res.Dependencies {
		if dep.Kind == "source_emission" && dep.Ref == src.ID {
			hasSourceDep = dep.Version == canonicalHash(src.Emission)
			if !hasSourceDep {
				return false
			}
		}
	}
	if !hasSourceDep {
		return false
	}
	if canonicalHash(res.Incoming) != canonicalHash(plan.Incoming[res.ChunkID]) {
		return false
	}
	if canonicalHash(res.Outgoing) != canonicalHash(plan.Outgoing[res.ChunkID]) {
		return false
	}
	deps := buildDependencies(level, res.ContentHash, res.ParamsVersion, src, plan.Incoming[res.ChunkID])
	expected := canonicalHash(struct {
		Content string       `json:"content"`
		Params  string       `json:"params"`
		Algo    string       `json:"algo"`
		Deps    []Dependency `json:"deps"`
	}{res.ContentHash, res.ParamsVersion, AlgorithmVersion, deps})
	return expected == res.CacheKey
}

func conservationDetails(level *Level, options Options, results []ChunkResult, plan *EnergyPlan) ConservationReport {
	report := ConservationReport{OK: true}
	chunkEnergy := map[string]map[string]int64{}
	for _, res := range results {
		for _, se := range res.SourceEnergy {
			if chunkEnergy[se.SourceID] == nil {
				chunkEnergy[se.SourceID] = map[string]int64{}
			}
			for _, e := range se.Energy {
				chunkEnergy[se.SourceID][e.BandID] += e.Energy
			}
		}
	}
	incoming := map[string]map[string]int64{}
	outgoing := map[string]map[string]int64{}
	for _, t := range plan.Transfers {
		if incoming[t.SourceID] == nil {
			incoming[t.SourceID] = map[string]int64{}
			outgoing[t.SourceID] = map[string]int64{}
		}
		for _, e := range t.Energy {
			incoming[t.SourceID][e.BandID] += e.Energy
			outgoing[t.SourceID][e.BandID] += e.Energy
		}
	}
	escapes := map[string]map[string]int64{}
	for _, list := range plan.Escapes {
		for _, e := range list {
			if escapes[e.SourceID] == nil {
				escapes[e.SourceID] = map[string]int64{}
			}
			escapes[e.SourceID][e.BandID] += e.Energy
		}
	}
	for _, src := range level.Sources {
		for _, band := range level.Bands {
			emission := bandValue(src.Emission, band.ID)
			captured := chunkEnergy[src.ID][band.ID]
			net := outgoing[src.ID][band.ID] - incoming[src.ID][band.ID]
			esc := escapes[src.ID][band.ID]
			residual := emission - (captured + net + esc)
			absResidual := residual
			if absResidual < 0 {
				absResidual = -absResidual
			}
			if absResidual > report.MaxResidual {
				report.MaxResidual = absResidual
			}
			if absResidual > options.ConservationResidue {
				report.OK = false
				if report.Error == "" {
					report.Error = fmt.Sprintf("source=%s band=%s emission=%d chunk_energy=%d boundary_net=%d escape=%d residual=%d",
						src.ID, band.ID, emission, captured, net, esc, residual)
				}
			}
			report.Bands = append(report.Bands, ConservationBand{
				SourceID: src.ID, BandID: band.ID, Emission: emission, ChunkEnergy: captured,
				BoundaryNet: net, Escape: esc, Residual: residual,
			})
		}
	}
	sort.Slice(report.Bands, func(i, j int) bool {
		if report.Bands[i].SourceID != report.Bands[j].SourceID {
			return report.Bands[i].SourceID < report.Bands[j].SourceID
		}
		return report.Bands[i].BandID < report.Bands[j].BandID
	})
	return report
}

func reconcileTruth(level *Level, truth Truth, options Options, results []ChunkResult) *Diff {
	truthChunks := make(map[int]TruthChunk)
	for _, c := range truth.Chunks {
		truthChunks[c.ChunkID] = c
	}
	truthPaths := make(map[string]TruthPath)
	truthPathCount := make(map[int]int)
	for _, p := range truth.Paths {
		truthPaths[p.PathID] = p
		truthPathCount[p.ChunkID]++
	}
	truthListeners := make(map[int]TruthListener)
	for _, l := range truth.Listeners {
		truthListeners[l.ChunkID] = l
	}
	for _, res := range results {
		tc, ok := truthChunks[res.ChunkID]
		if !ok {
			return &Diff{ChunkID: res.ChunkID, Field: "chunk_truth", Reason: "missing truth chunk"}
		}
		for _, band := range level.Bands {
			want := bandValue(tc.Energy, band.ID)
			got := bandValue(res.ArrivalEnergy, band.ID)
			if got != want {
				return &Diff{ChunkID: res.ChunkID, BandID: band.ID, Field: "arrival_energy", Expected: want, Actual: got}
			}
			for _, tse := range tc.SourceEnergy {
				wantSource := bandValue(tse.Energy, band.ID)
				gotSource := int64(0)
				for _, se := range res.SourceEnergy {
					if se.SourceID == tse.SourceID {
						gotSource = bandValue(se.Energy, band.ID)
						break
					}
				}
				if gotSource != wantSource {
					return &Diff{ChunkID: res.ChunkID, BandID: band.ID, SourceID: tse.SourceID, Field: "source_energy", Expected: wantSource, Actual: gotSource}
				}
			}
		}
		if len(res.Paths) != truthPathCount[res.ChunkID] {
			return &Diff{ChunkID: res.ChunkID, Field: "path_count", Expected: truthPathCount[res.ChunkID], Actual: len(res.Paths)}
		}
		for _, p := range res.Paths {
			tp, ok := truthPaths[p.ID]
			if !ok {
				return &Diff{ChunkID: res.ChunkID, PathID: p.ID, Field: "path", Reason: "path not in truth"}
			}
			delta := tp.LengthMicros - p.LengthMicros
			if delta < 0 {
				delta = -delta
			}
			if delta > options.PathToleranceMicros {
				return &Diff{ChunkID: res.ChunkID, PathID: p.ID, Field: "path_length", Expected: tp.LengthMicros, Actual: p.LengthMicros}
			}
		}
		tl, ok := truthListeners[res.ChunkID]
		if ok {
			got := sumBandEnergy(res.ArrivalEnergy)
			if got != tl.TotalEnergy {
				return &Diff{ChunkID: res.ChunkID, Field: "listener_total_energy", Expected: tl.TotalEnergy, Actual: got}
			}
		}
	}
	return nil
}

func (e *Engine) Report() *RunReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	order := BoundaryTopologicalOrder(e.level)
	return e.runReport(order, e.lastSubmissions, e.conservation, e.truthDiff, false)
}

func (e *Engine) StateSnapshot() StateSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Snapshot()
}

func (e *Engine) ResultForChunk(id int) (ChunkResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.results[id]
	if !ok {
		return ChunkResult{}, false
	}
	return cloneChunkResult(r), true
}
