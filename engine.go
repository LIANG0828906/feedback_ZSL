package layercache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type RunOptions struct {
	// Workers may lower, but never raise, the project's declared budget.
	Workers int
	// Checkpoint, when non-nil, receives a complete snapshot after each tick.
	Checkpoint        *CheckpointStore
	CheckpointVersion string
	// StopAfterMS is optional and useful for crash/recovery exercises.
	StopAfterMS int64
}

type Engine struct {
	project *Project
	records map[string][]accessRecord
	lastRun *RunResult
}

func NewEngine(project *Project) (*Engine, error) {
	if err := project.Validate(); err != nil {
		return nil, err
	}
	return &Engine{project: project.DeepCopy(), records: collectAccessRecords(project)}, nil
}

func (e *Engine) Project() *Project {
	return e.project.DeepCopy()
}

type runtimeState struct {
	CurrentLayer       map[string]string    `json:"currentLayer"`
	Pending            map[string]Migration `json:"pending"`
	PendingBatchID     string               `json:"pendingBatchId"`
	PendingMigrations  []Migration          `json:"pendingMigrations"`
	PendingWrites      map[string]int64     `json:"pendingWrites"`
	NextIndex          int                  `json:"nextIndex"`
	NextTimeMS         int64                `json:"nextTimeMs"`
	LastCommittedTick  int64                `json:"lastCommittedTick"`
	LastCommittedBatch string               `json:"lastCommittedBatch"`
}

func newRuntime(p *Project) *runtimeState {
	state := &runtimeState{
		CurrentLayer:      make(map[string]string, len(p.Objects)),
		Pending:           map[string]Migration{},
		PendingWrites:     map[string]int64{},
		LastCommittedTick: -1,
	}
	for _, object := range p.Objects {
		state.CurrentLayer[object.ID] = object.InitialLayer
	}
	return state
}

func collectTimes(p *Project) []int64 {
	seen := map[int64]bool{0: true}
	for _, object := range p.Objects {
		for _, sample := range object.Samples {
			seen[sample.AtMS] = true
		}
	}
	for _, event := range p.Events {
		seen[event.AtMS] = true
	}
	out := make([]int64, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (e *Engine) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workers := opts.Workers
	if workers == 0 {
		workers = e.project.Config.Workers
	}
	if workers < 1 || workers > e.project.Config.Workers {
		return nil, bizError(CodeBackpressureThreads, e.project.SchemaVersion, "config.workers", "requested workers %d exceed declared budget %d", workers, e.project.Config.Workers)
	}
	state := newRuntime(e.project)
	timings := map[string]*LayerTiming{}
	for _, layer := range e.project.SortedLayers() {
		timings[layer.ID] = &LayerTiming{LayerID: layer.ID}
	}
	times := collectTimes(e.project)
	result := &RunResult{ProjectKey: e.project.SemanticKey(), Metrics: RunMetrics{}}
	for tickIndex, now := range times {
		if err := ctx.Err(); err != nil {
			result.Interrupted = true
			break
		}
		if opts.StopAfterMS > 0 && now > opts.StopAfterMS {
			result.Interrupted = true
			break
		}
		heats, err := computeHeatParallel(ctx, e.project, e.records, state, now, workers, timings)
		if err != nil {
			return nil, err
		}
		if err := checkCandidateBuffers(ctx, e.project, state, heats, workers, timings); err != nil {
			return nil, err
		}
		var batches []BatchResult
		var gates []GateObservation
		var commits []CommitResult
		var aborts []AbortResult
		events := e.project.SortedEvents()
		for _, event := range events {
			if event.AtMS != now {
				continue
			}
			switch event.Kind {
			case EventAccess:
				// All access events at this logical instant are folded into the
				// deterministic heat calculation above.
			case EventRebalance:
				if state.PendingBatchID != "" {
					return nil, bizError(CodeInvalidEvent, e.project.SchemaVersion, fmt.Sprintf("events[order=%d]", event.Order), "rebalance while batch %q is pending", state.PendingBatchID)
				}
				batch, err := solveMigrationBatch(e.project, state.CurrentLayer, heats, event)
				if err != nil {
					return nil, err
				}
				batches = append(batches, batch)
				moves := append(append([]Migration{}, batch.Demote...), batch.Promote...)
				if err := stageBatch(state, batch.BatchID, moves); err != nil {
					return nil, err
				}
			case EventMigrate:
				if state.PendingBatchID != "" {
					return nil, bizError(CodeInvalidEvent, e.project.SchemaVersion, fmt.Sprintf("events[order=%d]", event.Order), "migrate while batch %q is pending", state.PendingBatchID)
				}
				occupancy := currentOccupancy(e.project, state.CurrentLayer)
				batch, err := directMigration(e.project, state.CurrentLayer, occupancy, event)
				if err != nil {
					return nil, err
				}
				batches = append(batches, batch)
				moves := append(append([]Migration{}, batch.Demote...), batch.Promote...)
				if err := stageBatch(state, batch.BatchID, moves); err != nil {
					return nil, err
				}
			case EventRead:
				gates = append(gates, observeRead(state, event))
			case EventWrite:
				gates = append(gates, observeWrite(state, event))
			case EventCommit:
				commit, err := commitBatch(e.project, state, event, now)
				if err != nil {
					return nil, err
				}
				commits = append(commits, commit)
			case EventAbort:
				abort, err := abortBatch(state, event, now)
				if err != nil {
					return nil, err
				}
				aborts = append(aborts, abort)
			default:
				return nil, bizError(CodeInvalidEvent, e.project.SchemaVersion, fmt.Sprintf("events[order=%d]", event.Order), "unsupported event")
			}
		}
		tick, err := buildTick(e.project, state, heats, tickIndex, now, batches, gates, commits, aborts)
		if err != nil {
			return nil, err
		}
		result.Ticks = append(result.Ticks, tick)
		state.NextIndex = tickIndex + 1
		state.NextTimeMS = now
		if len(commits) > 0 {
			state.LastCommittedTick = now
			state.LastCommittedBatch = commits[len(commits)-1].BatchID
		}
		if opts.Checkpoint != nil {
			fullState := checkpointPayload(e.project, state, heats, now, tickIndex)
			if err := opts.Checkpoint.Save(fullState, opts.CheckpointVersion); err != nil {
				return nil, err
			}
		}
	}
	result.Interrupted = result.Interrupted || ctx.Err() != nil
	result.LastCommittedTick = state.LastCommittedTick
	result.LastCommittedBatch = state.LastCommittedBatch
	for _, layer := range e.project.SortedLayers() {
		t := timings[layer.ID]
		result.Metrics.Layer = append(result.Metrics.Layer, *t)
	}
	e.lastRun = cloneRunResult(result)
	return result, nil
}

func computeHeatParallel(ctx context.Context, p *Project, records map[string][]accessRecord, state *runtimeState, now int64, workers int, timings map[string]*LayerTiming) (map[string]heatPoint, error) {
	layers := p.SortedLayers()
	jobs := make([]struct {
		layer   string
		objects []Object
	}, len(layers))
	for i, layer := range layers {
		jobs[i].layer = layer.ID
	}
	for _, object := range p.Objects {
		layerID := state.CurrentLayer[object.ID]
		for i := range jobs {
			if jobs[i].layer == layerID {
				jobs[i].objects = append(jobs[i].objects, object)
				break
			}
		}
	}
	if len(jobs) > workers*4 {
		return nil, bizError(CodeBackpressureThreads, p.SchemaVersion, "layers", "layer task queue exceeds worker budget")
	}
	type heatResult struct {
		point heatPoint
		err   error
	}
	results := make([]heatResult, len(p.Objects))
	objectIndex := make(map[string]int, len(p.Objects))
	for i, object := range p.Objects {
		objectIndex[object.ID] = i
	}
	jobsCh := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for jobIndex := range jobsCh {
				job := jobs[jobIndex]
				start := time.Now()
				for _, object := range job.objects {
					watermark := sampleWatermark(object, now)
					point, err := heatAt(p, records[object.ID], now, watermark)
					if err != nil {
						if be, ok := err.(*BizError); ok {
							be.Location = fmt.Sprintf("object %q at %dms", object.ID, now)
						}
						results[objectIndex[object.ID]] = heatResult{err: err}
						continue
					}
					results[objectIndex[object.ID]] = heatResult{point: point}
				}
				if timing := timings[job.layer]; timing != nil {
					timing.HeatNanos += time.Since(start).Nanoseconds()
					timing.LastWorker = workerID
				}
			}
		}(worker)
	}
	go func() {
		defer close(jobsCh)
		for i := range jobs {
			select {
			case <-ctx.Done():
				return
			case jobsCh <- i:
			}
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	heats := make(map[string]heatPoint, len(p.Objects))
	var firstErr error
	for i, object := range p.Objects {
		if results[i].err != nil && firstErr == nil {
			firstErr = results[i].err
		}
		heats[object.ID] = results[i].point
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return heats, nil
}

func checkCandidateBuffers(ctx context.Context, p *Project, state *runtimeState, heats map[string]heatPoint, workers int, timings map[string]*LayerTiming) error {
	layers := p.SortedLayers()
	type result struct {
		layer string
		count int
		nanos int64
		err   error
	}
	results := make([]result, len(layers))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				start := time.Now()
				layer := layers[i]
				count := 0
				for _, object := range p.SortedObjects() {
					if state.CurrentLayer[object.ID] != layer.ID {
						continue
					}
					heat := heats[object.ID]
					if layer.Kind == LayerCold && heat.level == HeatHot && heat.score >= p.Config.MigrateHotLimit {
						count++
					}
					if layer.Kind == LayerHot && heat.level == HeatCold && heat.score <= p.Config.MigrateColdLimit {
						count++
					}
				}
				res := result{layer: layer.ID, count: count, nanos: time.Since(start).Nanoseconds()}
				if count > p.Config.PerLayerBuffer {
					res.err = bizError(CodeBackpressureBuffer, p.SchemaVersion, "layers."+layer.ID+".pending", "candidate buffer %d exceeds limit %d", count, p.Config.PerLayerBuffer)
				}
				results[i] = res
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range layers {
			select {
			case <-ctx.Done():
				return
			case jobs <- i:
			}
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	var firstErr error
	for i := range results {
		if t := timings[results[i].layer]; t != nil {
			t.CandidateNanos += results[i].nanos
		}
		if results[i].err != nil && firstErr == nil {
			firstErr = results[i].err
		}
	}
	return firstErr
}

func stageBatch(state *runtimeState, batchID string, moves []Migration) error {
	for _, move := range moves {
		if _, exists := state.Pending[move.ObjectID]; exists {
			return bizError(CodeInvalidEvent, "", "migration", "object %q is already pending", move.ObjectID)
		}
	}
	for _, move := range moves {
		state.Pending[move.ObjectID] = move
		state.PendingMigrations = append(state.PendingMigrations, move)
	}
	state.PendingBatchID = batchID
	return nil
}

func commitBatch(p *Project, state *runtimeState, event Event, now int64) (CommitResult, error) {
	batchID := event.BatchID
	if batchID == "" {
		batchID = state.PendingBatchID
	}
	if state.PendingBatchID == "" {
		return CommitResult{}, bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[order=%d]", event.Order), "no pending migration batch")
	}
	if batchID != state.PendingBatchID {
		return CommitResult{}, bizError(CodeInvalidEvent, p.SchemaVersion, fmt.Sprintf("events[order=%d]", event.Order), "commit batch %q does not match pending %q", batchID, state.PendingBatchID)
	}
	occupancy := currentOccupancy(p, state.CurrentLayer)
	moves := append([]Migration(nil), state.PendingMigrations...)
	for _, move := range moves {
		if state.CurrentLayer[move.ObjectID] != move.SourceLayer {
			return CommitResult{}, bizError(CodeInvalidEvent, p.SchemaVersion, "migration", "object %q source changed before commit", move.ObjectID)
		}
	}
	layerByID := map[string]Layer{}
	for _, layer := range p.SortedLayers() {
		layerByID[layer.ID] = layer
	}
	for _, move := range moves {
		target := layerByID[move.TargetLayer]
		if occupancy[target.ID]+move.SizeBytes > target.CapacityBytes {
			return CommitResult{}, bizError(CodeCapacityExceeded, p.SchemaVersion, "migration", "commit would exceed layer %q capacity", target.ID)
		}
		occupancy[move.SourceLayer] -= move.SizeBytes
		occupancy[move.TargetLayer] += move.SizeBytes
		state.CurrentLayer[move.ObjectID] = move.TargetLayer
		delete(state.Pending, move.ObjectID)
	}
	state.PendingMigrations = nil
	state.PendingBatchID = ""
	return CommitResult{
		BatchID: batchID, TimeMS: now, Migrations: moves,
		OccupancyAfter: occupancySnapshot(p.SortedLayers(), occupancy),
	}, nil
}

func abortBatch(state *runtimeState, event Event, now int64) (AbortResult, error) {
	batchID := event.BatchID
	if batchID == "" {
		batchID = state.PendingBatchID
	}
	if state.PendingBatchID == "" {
		return AbortResult{}, bizError(CodeInvalidEvent, "", fmt.Sprintf("events[order=%d]", event.Order), "no pending migration batch to abort")
	}
	if batchID != state.PendingBatchID {
		return AbortResult{}, bizError(CodeInvalidEvent, "", fmt.Sprintf("events[order=%d]", event.Order), "abort batch does not match pending batch")
	}
	moves := append([]Migration(nil), state.PendingMigrations...)
	for _, move := range moves {
		delete(state.Pending, move.ObjectID)
	}
	state.PendingMigrations = nil
	state.PendingBatchID = ""
	return AbortResult{BatchID: batchID, TimeMS: now, Migrations: moves}, nil
}

func observeRead(state *runtimeState, event Event) GateObservation {
	gate := GateObservation{EventOrder: event.Order, ObjectID: event.ObjectID, Operation: "read", AnswerLayer: state.CurrentLayer[event.ObjectID], TargetVisible: true}
	if move, ok := state.Pending[event.ObjectID]; ok {
		gate.Pending = true
		gate.AnswerLayer = move.SourceLayer
		gate.SourceLayer = move.SourceLayer
		gate.TargetLayer = move.TargetLayer
		gate.TargetVisible = false
	}
	return gate
}

func observeWrite(state *runtimeState, event Event) GateObservation {
	gate := GateObservation{EventOrder: event.Order, ObjectID: event.ObjectID, Operation: "write", AnswerLayer: state.CurrentLayer[event.ObjectID], TargetVisible: true}
	if move, ok := state.Pending[event.ObjectID]; ok {
		gate.Pending = true
		gate.AnswerLayer = move.SourceLayer
		gate.SourceLayer = move.SourceLayer
		gate.TargetLayer = move.TargetLayer
		gate.TargetVisible = false
		gate.Rejected = true
		gate.Queued = true
		gate.Reason = "pending migration; write queued for compensating write"
		state.PendingWrites[event.ObjectID]++
	}
	return gate
}

func currentOccupancy(p *Project, current map[string]string) map[string]int64 {
	occupancy := map[string]int64{}
	for _, object := range p.Objects {
		occupancy[current[object.ID]] += object.SizeBytes
	}
	return occupancy
}

func buildTick(p *Project, state *runtimeState, heats map[string]heatPoint, index int, now int64, batches []BatchResult, gates []GateObservation, commits []CommitResult, aborts []AbortResult) (TickResult, error) {
	occupancy := currentOccupancy(p, state.CurrentLayer)
	layers := p.SortedLayers()
	tick := TickResult{Index: index, TimeMS: now, Batches: batches, Gates: gates, Commits: commits, Aborts: aborts}
	for _, layer := range layers {
		objectIDs := []string{}
		for _, object := range p.SortedObjects() {
			if state.CurrentLayer[object.ID] == layer.ID {
				objectIDs = append(objectIDs, object.ID)
			}
		}
		snapshot := LayerSnapshot{
			ID: layer.ID, Kind: layer.Kind, CapacityBytes: layer.CapacityBytes,
			Occupancy: occupancy[layer.ID], ObjectCount: len(objectIDs), Objects: objectIDs,
		}
		for _, move := range state.PendingMigrations {
			if move.TargetLayer == layer.ID {
				snapshot.PendingBytes += move.SizeBytes
			}
		}
		if snapshot.Occupancy > snapshot.CapacityBytes {
			return TickResult{}, bizError(CodeCapacityExceeded, p.SchemaVersion, "layers."+layer.ID, "committed occupancy exceeds capacity")
		}
		tick.Layers = append(tick.Layers, snapshot)
	}
	for _, object := range p.SortedObjects() {
		point := heats[object.ID]
		watermark := sampleWatermark(object, now)
		snapshot := ObjectSnapshot{
			ID: object.ID, LayerID: state.CurrentLayer[object.ID], AnswerLayer: state.CurrentLayer[object.ID],
			PendingWrites: state.PendingWrites[object.ID], SizeBytes: object.SizeBytes,
			Heat: point.score, HeatLevel: point.level, LastAccessMS: point.lastAccessMS,
			CountFromMS: point.countFromMS, CountToMS: point.countToMS, CountInWindow: point.countInWindow,
			Watermark: watermark, WatermarkValid: watermark != nil,
			OverflowDetected: point.overflowDetected, Saturated: point.saturated,
		}
		if move, ok := state.Pending[object.ID]; ok {
			snapshot.Pending = true
			snapshot.AnswerLayer = move.SourceLayer
			snapshot.TargetLayer = move.TargetLayer
		}
		tick.Objects = append(tick.Objects, snapshot)
	}
	tick.Revision = tickRevision(tick)
	return tick, nil
}

func tickRevision(tick TickResult) string {
	tick.Revision = ""
	raw, _ := json.Marshal(tick)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cloneRunResult(in *RunResult) *RunResult {
	var out RunResult
	raw, _ := json.Marshal(in)
	_ = json.Unmarshal(raw, &out)
	return &out
}

func checkpointPayload(p *Project, state *runtimeState, heats map[string]heatPoint, now int64, nextIndex int) *CheckpointPayload {
	payload := &CheckpointPayload{
		ProjectKey:    p.SemanticKey(),
		SchemaVersion: p.SchemaVersion,
		CommittedSeq:  state.LastCommittedTick,
		NextIndex:     nextIndex + 1,
		NextTimeMS:    now,
		Runtime:       *state,
		Objects:       make([]PersistedObject, 0, len(p.Objects)),
		Layers:        make([]PersistedLayer, 0, len(p.Layers)),
	}
	for _, object := range p.SortedObjects() {
		point := heats[object.ID]
		payload.Objects = append(payload.Objects, PersistedObject{
			ID: object.ID, LayerID: state.CurrentLayer[object.ID], Heat: point.score, HeatLevel: point.level,
			LastAccessMS: point.lastAccessMS, PendingWrites: state.PendingWrites[object.ID],
		})
	}
	occupancy := currentOccupancy(p, state.CurrentLayer)
	for _, layer := range p.SortedLayers() {
		payload.Layers = append(payload.Layers, PersistedLayer{ID: layer.ID, Occupancy: occupancy[layer.ID]})
	}
	return payload
}
