// Package engine orchestrates deterministic room chunks, worker-pool
// reflection solves, invalidation reuse and SQLite checkpoint commits.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"roomacoustics/pkg/acoustics"
	"roomacoustics/pkg/checkpoint"
	"roomacoustics/pkg/fixedpoint"
)

var ErrRoomsFailed = errors.New("one or more rooms entered failed state")
var ErrInterrupted = errors.New("simulated interruption after durable commit")

type Config struct {
	Store            *checkpoint.Store
	Pool             *acoustics.Pool
	MaxWorkers       int
	BufferPerWorker  int
	ReuseReport      *acoustics.Report
	Invalidation     *acoustics.InvalidationReport
	StopAfterCommits int
}

type Engine struct {
	level  *acoustics.Level
	index  *acoustics.Index
	params acoustics.Params

	globalHash string
	roomHashes map[string]string

	machine  *acoustics.StateMachine
	pool     *acoustics.Pool
	ownsPool bool
	store    *checkpoint.Store

	reuse         map[string]acoustics.RoomResult
	invalidRooms  map[string]bool
	invalidation  *acoustics.InvalidationReport
	recovered     map[string]checkpoint.RoomCheckpoint
	windowStarted bool

	resultsMu sync.Mutex
	results   map[string]acoustics.RoomResult

	faultMu           sync.Mutex
	failBeforeReflect map[string]int

	stopAfterCommits int
	newCommits       int
}

func New(level *acoustics.Level, cfg Config) (*Engine, error) {
	if err := acoustics.Validate(level); err != nil {
		return nil, err
	}
	idx := acoustics.NewIndex(level)
	params := level.Params
	if params.ParamVersion == "" {
		params = acoustics.DefaultParams()
	}
	globalHash := acoustics.HashCanonical(struct {
		Level            *acoustics.Level `json:"level"`
		ParamVersion     string           `json:"param_version"`
		AlgorithmVersion string           `json:"algorithm_version"`
	}{Level: level, ParamVersion: params.ParamVersion, AlgorithmVersion: params.AlgorithmVersion})

	hashes := map[string]string{}
	for _, id := range idx.RoomOrder {
		hashes[id] = hashRoom(level, idx, id, params)
	}

	pool := cfg.Pool
	owns := false
	if pool == nil {
		pool = acoustics.NewPool(cfg.MaxWorkers, cfg.BufferPerWorker)
		owns = true
	}
	reuse := map[string]acoustics.RoomResult{}
	if cfg.ReuseReport != nil {
		for _, rr := range cfg.ReuseReport.Rooms {
			if rr.Committed && rr.State == acoustics.StateCommitted {
				reuse[rr.RoomID] = rr
			}
		}
	}
	invalid := map[string]bool{}
	if cfg.Invalidation != nil {
		for _, id := range cfg.Invalidation.InvalidRooms {
			invalid[id] = true
		}
	}

	return &Engine{
		level: level, index: idx, params: params,
		globalHash: globalHash, roomHashes: hashes,
		machine: acoustics.NewStateMachine(idx.RoomOrder),
		pool:    pool, ownsPool: owns, store: cfg.Store,
		reuse: reuse, invalidRooms: invalid, invalidation: cfg.Invalidation,
		results:           map[string]acoustics.RoomResult{},
		failBeforeReflect: map[string]int{},
		stopAfterCommits:  cfg.StopAfterCommits,
	}, nil
}

func (e *Engine) Close() {
	if e.ownsPool && e.pool != nil {
		e.pool.Close()
	}
}

func (e *Engine) GlobalHash() string                    { return e.globalHash }
func (e *Engine) StateMachine() *acoustics.StateMachine { return e.machine }

// FailNext injects n failures immediately before reflection solving.
func (e *Engine) FailNext(roomID string, n int) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	e.failBeforeReflect[roomID] = n
}

func (e *Engine) Run(ctx context.Context) (*acoustics.Report, error) {
	if e.store != nil && !e.windowStarted {
		ws, err := e.store.InitializeWindow(ctx, checkpoint.Signature{
			GlobalHash: e.globalHash, ParamVersion: e.params.ParamVersion, AlgorithmVersion: e.params.AlgorithmVersion,
		}, e.index.RoomOrder)
		if err != nil {
			return nil, err
		}
		e.windowStarted = true
		e.recovered = ws.Recovered
	}

	var failedRooms []string
	var interrupted bool
	for commitIndex, roomID := range e.index.RoomOrder {
		select {
		case <-ctx.Done():
			return e.buildReport(), ctx.Err()
		default:
		}

		if _, exists := e.getResult(roomID); exists {
			continue
		}

		if cp, ok := e.recovered[roomID]; ok {
			rr, err := decodeRoomResult(cp.ResultJSON)
			if err != nil {
				return e.buildReport(), err
			}
			rr.RoomID = roomID
			rr.State = acoustics.StateCommitted
			rr.Committed = true
			rr.Reused = true
			rr.CommitIndex = commitIndex
			e.machine.Restore(roomID, acoustics.StateCommitted)
			e.setResult(roomID, rr)
			continue
		}

		if old, ok := e.reuse[roomID]; ok && !e.invalidRooms[roomID] {
			old.State = acoustics.StateCommitted
			old.Committed = true
			old.Reused = true
			old.CommitIndex = commitIndex
			old.ContentHash = e.roomHashes[roomID]
			if err := e.machine.Transition(roomID, acoustics.StateConnecting); err != nil {
				return e.buildReport(), err
			}
			if err := e.machine.Transition(roomID, acoustics.StateReflecting); err != nil {
				return e.buildReport(), err
			}
			if err := e.commitRoom(ctx, old, commitIndex); err != nil {
				_ = e.machine.Transition(roomID, acoustics.StateFailed)
				e.setResult(roomID, acoustics.RoomResult{RoomID: roomID, State: acoustics.StateFailed, CommitIndex: commitIndex, ContentHash: e.roomHashes[roomID]})
				failedRooms = append(failedRooms, roomID)
				continue
			}
			_ = e.machine.Transition(roomID, acoustics.StateCommitted)
			e.setResult(roomID, old)
			e.newCommits++
			if e.stopAfterCommits > 0 && e.newCommits >= e.stopAfterCommits {
				interrupted = true
				break
			}
			continue
		}

		rr, err := e.solveRoom(ctx, roomID, commitIndex)
		if err != nil {
			_ = e.machine.Transition(roomID, acoustics.StateFailed)
			failed := acoustics.RoomResult{RoomID: roomID, State: acoustics.StateFailed, CommitIndex: commitIndex, ContentHash: e.roomHashes[roomID]}
			var de *acoustics.DomainError
			if errors.As(err, &de) {
				// Keep the structured cause available to diagnostics via error return.
				_ = de
			}
			e.setResult(roomID, failed)
			failedRooms = append(failedRooms, roomID)
			continue
		}
		rr.State = acoustics.StateCommitted
		rr.Committed = true
		if err := e.commitRoom(ctx, rr, commitIndex); err != nil {
			_ = e.machine.Transition(roomID, acoustics.StateFailed)
			e.setResult(roomID, acoustics.RoomResult{RoomID: roomID, State: acoustics.StateFailed, CommitIndex: commitIndex, ContentHash: e.roomHashes[roomID]})
			failedRooms = append(failedRooms, roomID)
			continue
		}
		if err := e.machine.Transition(roomID, acoustics.StateCommitted); err != nil {
			return e.buildReport(), err
		}
		e.setResult(roomID, rr)
		e.newCommits++
		if e.stopAfterCommits > 0 && e.newCommits >= e.stopAfterCommits {
			interrupted = true
			break
		}
	}

	report := e.buildReport()
	if interrupted {
		return report, ErrInterrupted
	}
	if len(failedRooms) > 0 {
		return report, errors.Join(append([]error{ErrRoomsFailed}, namedErrors(failedRooms)...)...)
	}
	return report, nil
}

// RetryFailed moves failed rooms back to pending and solves them in room order.
func (e *Engine) RetryFailed(ctx context.Context) (*acoustics.Report, error) {
	var retryIDs []string
	for _, id := range e.index.RoomOrder {
		if st, ok := e.machine.State(id); ok && st == acoustics.StateFailed {
			retryIDs = append(retryIDs, id)
		}
	}
	var failedRooms []string
	for commitIndex, roomID := range retryIDs {
		if err := e.machine.Transition(roomID, acoustics.StatePending); err != nil {
			return e.buildReport(), err
		}
		rr, err := e.solveRoom(ctx, roomID, commitIndex)
		if err != nil {
			_ = e.machine.Transition(roomID, acoustics.StateFailed)
			e.setResult(roomID, acoustics.RoomResult{RoomID: roomID, State: acoustics.StateFailed, ContentHash: e.roomHashes[roomID]})
			failedRooms = append(failedRooms, roomID)
			continue
		}
		stableIndex := indexOf(e.index.RoomOrder, roomID)
		rr.CommitIndex = stableIndex
		rr.State = acoustics.StateCommitted
		rr.Committed = true
		if err := e.commitRoom(ctx, rr, stableIndex); err != nil {
			_ = e.machine.Transition(roomID, acoustics.StateFailed)
			e.setResult(roomID, acoustics.RoomResult{RoomID: roomID, State: acoustics.StateFailed, ContentHash: e.roomHashes[roomID]})
			failedRooms = append(failedRooms, roomID)
			continue
		}
		if err := e.machine.Transition(roomID, acoustics.StateCommitted); err != nil {
			return e.buildReport(), err
		}
		e.setResult(roomID, rr)
	}
	report := e.buildReport()
	if len(failedRooms) > 0 {
		return report, ErrRoomsFailed
	}
	return report, nil
}

func (e *Engine) solveRoom(ctx context.Context, roomID string, commitIndex int) (acoustics.RoomResult, error) {
	start := time.Now()
	if err := e.machine.Transition(roomID, acoustics.StateConnecting); err != nil {
		return acoustics.RoomResult{}, err
	}
	room, _ := e.index.RoomByID[roomID]
	listeners := append([]acoustics.Listener{}, e.index.Level.Listeners...)
	listeners = filterListeners(listeners, roomID)
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	sources := append([]acoustics.Source{}, e.index.Level.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	type pairJob struct {
		src acoustics.Source
		lis acoustics.Listener
		pr  acoustics.PairResult
	}
	var jobs []*pairJob
	var summaries []acoustics.Chain
	for _, lis := range listeners {
		for _, src := range sources {
			pr := acoustics.PairResult{SourceID: src.ID, ListenerID: lis.ID, ListenerRoomID: roomID, Reflections: []acoustics.ReflectionHit{}}
			chain, err := e.index.SolvePointChain(src.ID, lis.ID, src.Position, src.RoomID, lis.Position, lis.RoomID)
			if err != nil {
				if de, ok := acoustics.AsDomainError(err); ok && de.Code == acoustics.CodeUnreach {
					pr.Status = "unreachable"
					pr.Error = de
					jobs = append(jobs, &pairJob{src: src, lis: lis, pr: pr})
					continue
				}
				return acoustics.RoomResult{}, err
			}
			pr.Status = "solved"
			pr.Chain = chain
			summaries = append(summaries, *chain)
			jobs = append(jobs, &pairJob{src: src, lis: lis, pr: pr})
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].SourceID+"|"+summaries[i].ListenerID < summaries[j].SourceID+"|"+summaries[j].ListenerID
	})
	if summaries == nil {
		summaries = []acoustics.Chain{}
	}

	if err := e.machine.Transition(roomID, acoustics.StateReflecting); err != nil {
		return acoustics.RoomResult{}, err
	}
	if err := e.takeFault(roomID); err != nil {
		return acoustics.RoomResult{}, err
	}

	tasks := make([]acoustics.Task, 0, len(jobs))
	for _, job := range jobs {
		job := job
		tasks = append(tasks, acoustics.Task{ID: roomID + ":" + job.pr.SourceID + "->" + job.pr.ListenerID, Fn: func() error {
			if job.pr.Chain == nil {
				return nil
			}
			pr, err := solveReflections(e.index, job.src, job.lis, job.pr.Chain, job.pr)
			if err != nil {
				return err
			}
			job.pr = pr
			return nil
		}})
	}
	errs := e.pool.Run(ctx, tasks)
	for _, err := range errs {
		if err != nil {
			return acoustics.RoomResult{}, err
		}
	}

	pairs := make([]acoustics.PairResult, 0, len(jobs))
	for _, job := range jobs {
		pairs = append(pairs, job.pr)
	}
	refs := append([]acoustics.Reflector{}, e.index.ReflectorsRoom[roomID]...)
	doors := doorsForRoom(e.index, roomID)
	walls := wallsForRoom(e.index, roomID)
	rr := acoustics.RoomResult{
		RoomID: roomID, State: acoustics.StateReflecting, CommitIndex: commitIndex,
		ContentHash: e.roomHashes[roomID], SurfaceSnapshot: refs, DoorSnapshot: doors, WallSnapshot: walls,
		PairResults: pairs, ChainSummaries: summaries, Committed: false,
		SolveMilliseconds: time.Since(start).Milliseconds(),
	}
	_ = room
	return rr, nil
}

func solveReflections(idx *acoustics.Index, src acoustics.Source, lis acoustics.Listener, chain *acoustics.Chain, pr acoustics.PairResult) (acoustics.PairResult, error) {
	input := src.Position
	prefixFixed := int64(0)
	if len(chain.DoorIDs) > 0 {
		lastDoor := idx.DoorByID[chain.DoorIDs[len(chain.DoorIDs)-1]]
		lastCenter := fixedpoint.FromMeters(lastDoor.Center.X, lastDoor.Center.Y)
		listenerFixed := fixedpoint.FromMeters(lis.Position.X, lis.Position.Y)
		lastLeg := fixedpoint.Distance(lastCenter, listenerFixed)
		prefixFixed = chain.LengthFixed - lastLeg
		input = lastDoor.Center
	}

	hits := []acoustics.ReflectionHit{}
	for _, ref := range idx.ReflectorsRoom[lis.RoomID] {
		hit, err := acoustics.SolveReflection(input, lis.Position, ref)
		if err != nil {
			return pr, err
		}
		if hit == nil {
			continue
		}
		hits = append(hits, hit.WithPrefixAndLevel(prefixFixed, src.PowerDB))
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ReflectorID < hits[j].ReflectorID })
	pr.Reflections = hits
	pr.ValidPathCount = len(hits)
	levels := make([]float64, 0, len(hits))
	for _, h := range hits {
		levels = append(levels, h.SPLDB)
	}
	pr.ExpectedSPLDB = acoustics.AggregateSPL(levels)
	return pr, nil
}

func (e *Engine) commitRoom(ctx context.Context, rr acoustics.RoomResult, commitIndex int) error {
	if e.store == nil {
		return nil
	}
	surfaceJSON, err := json.Marshal(rr.SurfaceSnapshot)
	if err != nil {
		return err
	}
	doorJSON, err := json.Marshal(rr.DoorSnapshot)
	if err != nil {
		return err
	}
	chainJSON, err := json.Marshal(rr.ChainSummaries)
	if err != nil {
		return err
	}
	resultJSON, err := json.Marshal(rr)
	if err != nil {
		return err
	}
	return e.store.SaveRoom(ctx, checkpoint.RoomCheckpoint{
		GlobalHash: e.globalHash, RoomID: rr.RoomID, CommitIndex: commitIndex,
		ContentHash: e.roomHashes[rr.RoomID], ParamVersion: e.params.ParamVersion, AlgorithmVersion: e.params.AlgorithmVersion,
		SurfaceSnapshot: surfaceJSON, DoorSnapshot: doorJSON, ChainSummary: chainJSON, ResultJSON: resultJSON,
		Status: "committed",
	})
}

func (e *Engine) buildReport() *acoustics.Report {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	rooms := make([]acoustics.RoomResult, 0, len(e.index.RoomOrder))
	pairs := []acoustics.PairResult{}
	committedSequence := []string{}
	for _, id := range e.index.RoomOrder {
		rr, ok := e.results[id]
		if !ok {
			st, _ := e.machine.State(id)
			rr = acoustics.RoomResult{RoomID: id, State: st, ContentHash: e.roomHashes[id]}
		}
		rooms = append(rooms, rr)
		pairs = append(pairs, rr.PairResults...)
		if rr.State == acoustics.StateCommitted {
			committedSequence = append(committedSequence, id)
		}
	}
	report := &acoustics.Report{
		SchemaVersion: acoustics.LevelSchemaVersion, Seed: e.level.Seed,
		GlobalHash: e.globalHash, RoomContentHashes: e.roomHashes, Params: e.params,
		Rooms: rooms, Pairs: pairs, Progress: e.machine.Progress(),
		CommitSequence: committedSequence, QueueEvents: e.pool.QueueEvents(),
		Invalidation: e.invalidation,
	}
	report.Dependencies = acoustics.BuildDependencyGraph(e.level, report)
	return report
}

func (e *Engine) setResult(id string, rr acoustics.RoomResult) {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	e.results[id] = rr
}

func (e *Engine) getResult(id string) (acoustics.RoomResult, bool) {
	e.resultsMu.Lock()
	defer e.resultsMu.Unlock()
	rr, ok := e.results[id]
	return rr, ok
}

func (e *Engine) takeFault(roomID string) error {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	if e.failBeforeReflect[roomID] > 0 {
		e.failBeforeReflect[roomID]--
		return errors.New("injected isolated reflection failure for " + roomID)
	}
	return nil
}

func decodeRoomResult(b []byte) (acoustics.RoomResult, error) {
	var rr acoustics.RoomResult
	if len(b) == 0 {
		return rr, errors.New("empty checkpoint result")
	}
	if err := json.Unmarshal(b, &rr); err != nil {
		return rr, err
	}
	return rr, nil
}

func hashRoom(level *acoustics.Level, idx *acoustics.Index, roomID string, params acoustics.Params) string {
	room, _ := idx.RoomByID[roomID]
	listeners := filterListeners(level.Listeners, roomID)
	doors := doorsForRoom(idx, roomID)
	walls := wallsForRoom(idx, roomID)
	refs := append([]acoustics.Reflector{}, idx.ReflectorsRoom[roomID]...)
	return acoustics.HashCanonical(struct {
		Room       acoustics.Room        `json:"room"`
		Walls      []acoustics.Wall      `json:"walls"`
		Doors      []acoustics.Door      `json:"doors"`
		Reflectors []acoustics.Reflector `json:"reflectors"`
		Listeners  []acoustics.Listener  `json:"listeners"`
		Sources    []acoustics.Source    `json:"sources"`
		Params     acoustics.Params      `json:"params"`
	}{Room: room, Walls: walls, Doors: doors, Reflectors: refs, Listeners: listeners, Sources: level.Sources, Params: params})
}

func filterListeners(in []acoustics.Listener, roomID string) []acoustics.Listener {
	var out []acoustics.Listener
	for _, l := range in {
		if l.RoomID == roomID {
			out = append(out, l)
		}
	}
	return out
}

func doorsForRoom(idx *acoustics.Index, roomID string) []acoustics.Door {
	seen := map[string]bool{}
	var out []acoustics.Door
	for _, d := range idx.DoorsByRoom[roomID] {
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func wallsForRoom(idx *acoustics.Index, roomID string) []acoustics.Wall {
	doorIDs := map[string]bool{}
	for _, d := range doorsForRoom(idx, roomID) {
		doorIDs[d.WallID] = true
	}
	var out []acoustics.Wall
	for wallID := range doorIDs {
		if w, ok := idx.WallByID[wallID]; ok {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return 0
}

func namedErrors(ids []string) []error {
	out := make([]error, 0, len(ids))
	for _, id := range ids {
		out = append(out, errors.New(id))
	}
	return out
}
