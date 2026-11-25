package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"maskqa/internal/checkpoint"
	"maskqa/internal/deps"
	"maskqa/internal/engine"
	"maskqa/internal/model"
	"maskqa/internal/topology"
)

var ErrForceTerminated = errors.New("forced termination boundary: committed stroke checkpoint reached")

type Config struct {
	Mode                 string
	MaxParallel          int
	PixelBufferPixels    int
	QueueBudget          int
	KillAfterCommits     int
	ReusePersistedChunks bool
}

func DefaultConfig(mode string) Config {
	return Config{Mode: mode, MaxParallel: 2, PixelBufferPixels: 1024, QueueBudget: 8, ReusePersistedChunks: true}
}

type task struct {
	stroke    model.Stroke
	chunk     model.Chunk
	inputHash string
	key       string
}

type taskResult struct {
	strokeID  string
	chunkID   string
	component []byte
	key       string
	waitMs    int64
	err       error
}

type preparedComponent struct {
	bytes []byte
	key   string
}

func Run(ctx context.Context, p *model.Project, store *checkpoint.Store, cfg Config, plan *model.InvalidationPlan) (*model.RunResult, error) {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 2
	}
	if cfg.PixelBufferPixels <= 0 {
		cfg.PixelBufferPixels = 1024
	}
	if cfg.QueueBudget <= 0 {
		cfg.QueueBudget = 8
	}
	ordered := engine.OrderedStrokes(p.Strokes)
	chunkByID := map[string]model.Chunk{}
	inputHash := map[string]string{}
	for _, c := range p.Chunks {
		chunkByID[c.ID] = c
		inputHash[c.ID] = deps.ChunkInputHash(p.Candidate, c)
	}

	result := &model.RunResult{Mode: cfg.Mode, ProjectID: p.ID, Invalidation: plan}
	affectedChunks := map[string]bool{}
	reusedCoverage := map[string][]byte{}
	if plan != nil && cfg.ReusePersistedChunks && store != nil {
		for _, id := range plan.AffectedChunks {
			affectedChunks[id] = true
		}
		for _, id := range plan.ReusedChunks {
			c, ok := chunkByID[id]
			if !ok {
				affectedChunks[id] = true
				continue
			}
			expectedKey := expectedChunkKey(p, c, ordered, inputHash[id])
			rec, err := store.LoadChunkOutput(ctx, p.ID, id)
			if err != nil {
				return result, err
			}
			if rec == nil || rec.CacheKey != expectedKey || rec.InputHash != inputHash[id] || len(rec.Coverage) != c.Width()*c.Height() {
				affectedChunks[id] = true
				plan.Items = append(plan.Items, model.InvalidationItem{Kind: "chunk_cache_miss", ID: id, Reason: "persisted_chunk_key_missing_or_mismatch_recomputed"})
				continue
			}
			cp := append([]byte(nil), rec.Coverage...)
			reusedCoverage[id] = cp
			result.Chunks = append(result.Chunks, model.ChunkResult{
				ChunkID: id, CacheKey: rec.CacheKey, InputHash: rec.InputHash, OutputHash: rec.OutputHash,
				Coverage: cp, Width: c.Width(), Height: c.Height(), Reused: true,
			})
		}
	} else {
		for _, c := range p.Chunks {
			affectedChunks[c.ID] = true
		}
	}

	var state *checkpoint.CommittedState
	if store != nil {
		var err error
		state, err = store.LoadCommittedState(ctx, p.ID, model.OrderVersion, model.AlgorithmVersion, model.ParamsVersion)
		if err != nil {
			return result, err
		}
		result.CommitSequence = append(result.CommitSequence, state.Sequence...)
	} else {
		state = &checkpoint.CommittedState{Commits: map[string]int{}, Components: map[string]map[string]checkpoint.CommittedComponent{}}
	}

	componentData := map[string]map[string]preparedComponent{}
	var tasks []task
	for _, s := range ordered {
		if componentData[s.ID] == nil {
			componentData[s.ID] = map[string]preparedComponent{}
		}
		for _, c := range p.Chunks {
			if !affectedChunks[c.ID] || !engine.StrokeIntersectsChunk(s, c) {
				continue
			}
			key := deps.StrokeComponentKey(inputHash[c.ID], c.ID, s, "candidate:"+inputHash[c.ID])
			if committed := findCommitted(state, s.ID, c.ID, key); committed != nil {
				componentData[s.ID][c.ID] = preparedComponent{bytes: append([]byte(nil), committed.Component...), key: key}
				continue
			}
			tasks = append(tasks, task{stroke: s, chunk: c, inputHash: inputHash[c.ID], key: key})
		}
	}

	results, queueEvents, isolationEvents := runScheduler(ctx, tasks, cfg)
	result.QueueEvents = append(result.QueueEvents, queueEvents...)
	result.IsolationEvents = append(result.IsolationEvents, isolationEvents...)
	taskByStrokeChunk := map[string]map[string]taskResult{}
	var firstErr error
	for _, tr := range results {
		if tr.err != nil && firstErr == nil {
			firstErr = tr.err
		}
		if taskByStrokeChunk[tr.strokeID] == nil {
			taskByStrokeChunk[tr.strokeID] = map[string]taskResult{}
		}
		taskByStrokeChunk[tr.strokeID][tr.chunkID] = tr
	}
	if firstErr != nil {
		return result, firstErr
	}
	for strokeID, m := range taskByStrokeChunk {
		for chunkID, tr := range m {
			if tr.err != nil {
				continue
			}
			if componentData[strokeID] == nil {
				componentData[strokeID] = map[string]preparedComponent{}
			}
			componentData[strokeID][chunkID] = preparedComponent{bytes: tr.component, key: tr.key}
		}
	}

	committedThisRun := 0
	lastCommitIndex := 0
	if len(result.CommitSequence) > 0 {
		lastCommitIndex = state.Commits[result.CommitSequence[len(result.CommitSequence)-1]]
	}
	for _, s := range ordered {
		var rows []checkpoint.ComponentRow
		needsCommit := false
		for _, c := range p.Chunks {
			if !affectedChunks[c.ID] || !engine.StrokeIntersectsChunk(s, c) {
				continue
			}
			pc, ok := componentData[s.ID][c.ID]
			if !ok || len(pc.bytes) == 0 {
				continue
			}
			committed := findCommitted(state, s.ID, c.ID, pc.key)
			if committed == nil {
				needsCommit = true
			}
			rows = append(rows, checkpoint.ComponentRow{
				ChunkID: c.ID, Component: pc.bytes, ComponentKey: pc.key, CacheKey: pc.key,
				ContentHash: s.ContentHash, ResidualDigest: ResidualDigest(s),
			})
		}
		if needsCommit {
			if store == nil {
				result.CommitSequence = append(result.CommitSequence, s.ID)
				committedThisRun++
				continue
			}
			idx, err := store.CommitStroke(ctx, checkpoint.StrokeCommit{
				ProjectID: p.ID, StrokeID: s.ID, StrokeNo: s.No, ContentHash: s.ContentHash,
				ResidualDigest: ResidualDigest(s), OrderVersion: model.OrderVersion,
				AlgoVersion: model.AlgorithmVersion, ParamsVersion: model.ParamsVersion, Components: rows,
			})
			if err != nil {
				return result, err
			}
			lastCommitIndex = idx
			result.CommitSequence = append(result.CommitSequence, s.ID)
			committedThisRun++
			if cfg.KillAfterCommits > 0 && committedThisRun >= cfg.KillAfterCommits {
				result.KilledAfter = committedThisRun
				return result, ErrForceTerminated
			}
		}
	}

	finalCoverage := model.NewMask(p.Width, p.Height)
	var chunkResults []model.ChunkResult
	chunkOutputKeys := map[string]string{}
	for _, c := range p.Chunks {
		if reused, ok := reusedCoverage[c.ID]; ok {
			if err := engine.WriteChunkIntoMask(&finalCoverage, c, reused); err != nil {
				return result, err
			}
			chunkResults = append(chunkResults, *findChunkResult(result.Chunks, c.ID))
			chunkOutputKeys[c.ID] = findChunkResult(result.Chunks, c.ID).CacheKey
			continue
		}
		coverage := engine.ChunkCoverageFromMask(p.Candidate, c)
		var participating []string
		var numbers []int
		var orderIDs []string
		var componentKeys []string
		winner := ""
		for _, s := range ordered {
			if !engine.StrokeIntersectsChunk(s, c) {
				continue
			}
			participating = append(participating, s.ID)
			numbers = append(numbers, s.No)
			orderIDs = append(orderIDs, s.ID)
			pc, ok := componentData[s.ID][c.ID]
			if !ok {
				return result, model.NewError(model.CodeState, "component", s.ID, "missing component for chunk %s", c.ID)
			}
			if err := engine.ApplyComponent(coverage, c, pc.bytes); err != nil {
				return result, err
			}
			componentKeys = append(componentKeys, pc.key)
			if anyNonZero(pc.bytes) {
				winner = s.ID
			}
		}
		chunkKey := deps.ChunkOutputKey(c, inputHash[c.ID], componentKeys)
		outputHash := model.HashBytes("chunk-output-v1", coverage)
		if err := engine.WriteChunkIntoMask(&finalCoverage, c, coverage); err != nil {
			return result, err
		}
		cr := model.ChunkResult{
			ChunkID: c.ID, CacheKey: chunkKey, InputHash: inputHash[c.ID], OutputHash: outputHash,
			Coverage: append([]byte(nil), coverage...), Width: c.Width(), Height: c.Height(),
		}
		chunkResults = append(chunkResults, cr)
		chunkOutputKeys[c.ID] = chunkKey
		if store != nil {
			if err := store.SaveChunkOutput(ctx, p.ID, checkpoint.ChunkOutput{
				ChunkID: c.ID, CacheKey: chunkKey, InputHash: inputHash[c.ID], OutputHash: outputHash,
				Coverage: cr.Coverage,
			}, lastCommitIndex); err != nil {
				return result, err
			}
		}
		result.OrderDecisions = append(result.OrderDecisions, model.OrderDecision{
			ChunkID:       c.ID,
			Basis:         "sort process_priority ASC, then stroke_no ASC; map iteration and worker id are never used",
			Participating: participating, ParticipatingNumbers: numbers, ResolvedOrder: orderIDs,
			Winner: winner, WinnerBasis: "last nonzero source-over component in resolved order is the final operator for overlapping alpha",
		})
	}
	sort.Slice(chunkResults, func(i, j int) bool { return chunkResults[i].ChunkID < chunkResults[j].ChunkID })
	result.Chunks = chunkResults
	result.FinalCoverage = finalCoverage
	result.FinalMask = finalCoverage.Clone()
	for i, v := range result.FinalMask.Pix {
		if v >= model.AlphaThreshold {
			result.FinalMask.Pix[i] = 255
		} else {
			result.FinalMask.Pix[i] = 0
		}
	}

	topo := topology.Analyze(finalCoverage)
	result.Topology = topo
	if err := topology.VerifyAgainstTruth(topo, p.Truth, topology.DefaultTolerances()); err != nil {
		return result, err
	}
	if store != nil {
		topoKey := deps.TopologyKey(chunkOutputKeys)
		raw, _ := json.Marshal(topo)
		if err := store.SaveTopology(ctx, p.ID, topoKey, raw, model.HashCanonical(topo), lastCommitIndex); err != nil {
			return result, err
		}
	}
	return result, nil
}

func runScheduler(ctx context.Context, tasks []task, cfg Config) ([]taskResult, []model.QueueEvent, []model.IsolationEvent) {
	if len(tasks) == 0 {
		return nil, nil, nil
	}
	queue := make(chan task, cfg.QueueBudget)
	results := make(chan taskResult, len(tasks))
	var queueMu sync.Mutex
	var events []model.QueueEvent
	var isoMu sync.Mutex
	var isolations []model.IsolationEvent
	var wg sync.WaitGroup
	for w := 0; w < cfg.MaxParallel; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			scratch := make([]byte, 2*cfg.PixelBufferPixels)
			for tk := range queue {
				tr := taskResult{strokeID: tk.stroke.ID, chunkID: tk.chunk.ID, key: tk.key}
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							ierr := model.NewError(model.CodeState, "task_panic", tk.stroke.ID, "worker %d isolated panic: %v", workerID, rec)
							tr.err = ierr
							isoMu.Lock()
							isolations = append(isolations, model.IsolationEvent{TaskID: tk.stroke.ID + "/" + tk.chunk.ID, Code: string(model.CodeState), Message: ierr.Error()})
							isoMu.Unlock()
						}
					}()
					n := tk.chunk.Width() * tk.chunk.Height()
					if n > cfg.PixelBufferPixels {
						tr.err = model.NewError(model.CodePara, "pixel_buffer", tk.stroke.ID, "chunk pixels %d exceed per-thread cap %d", n, cfg.PixelBufferPixels)
						isoMu.Lock()
						isolations = append(isolations, model.IsolationEvent{TaskID: tk.stroke.ID + "/" + tk.chunk.ID, Code: string(model.CodePara), Message: tr.err.Error()})
						isoMu.Unlock()
						return
					}
					component, err := engine.RasterizeStrokeChunkBuffered(tk.stroke, tk.chunk, scratch[:2*n])
					if err != nil {
						tr.err = err
						if me, ok := err.(*model.Error); ok {
							isoMu.Lock()
							isolations = append(isolations, model.IsolationEvent{TaskID: tk.stroke.ID + "/" + tk.chunk.ID, Code: string(me.Code), Message: me.Error()})
							isoMu.Unlock()
						}
						return
					}
					tr.component = append([]byte(nil), component...)
				}()
				results <- tr
			}
		}(w)
	}

	go func() {
		for _, tk := range tasks {
			if len(queue) == cap(queue) {
				start := time.Now()
				select {
				case queue <- tk:
				case <-ctx.Done():
					results <- taskResult{strokeID: tk.stroke.ID, chunkID: tk.chunk.ID, key: tk.key, err: ctx.Err()}
					continue
				}
				elapsed := time.Since(start)
				queueMu.Lock()
				events = append(events, model.QueueEvent{ChunkID: tk.chunk.ID, StrokeID: tk.stroke.ID, Reason: "pending_chunk_queue_budget_exhausted_blocked_before_enqueue", WaitNanos: elapsed.Nanoseconds(), WaitMillis: elapsed.Milliseconds()})
				queueMu.Unlock()
			} else {
				select {
				case queue <- tk:
				case <-ctx.Done():
					results <- taskResult{strokeID: tk.stroke.ID, chunkID: tk.chunk.ID, key: tk.key, err: ctx.Err()}
				}
			}
		}
		close(queue)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var out []taskResult
	for tr := range results {
		out = append(out, tr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].strokeID != out[j].strokeID {
			return out[i].strokeID < out[j].strokeID
		}
		return out[i].chunkID < out[j].chunkID
	})
	return out, events, isolations
}

func findCommitted(state *checkpoint.CommittedState, strokeID, chunkID, key string) *checkpoint.CommittedComponent {
	if state == nil || state.Components[strokeID] == nil {
		return nil
	}
	row, ok := state.Components[strokeID][chunkID]
	if !ok || row.CacheKey != key {
		return nil
	}
	return &row
}

func expectedChunkKey(p *model.Project, c model.Chunk, ordered []model.Stroke, inputHash string) string {
	var keys []string
	for _, s := range ordered {
		if engine.StrokeIntersectsChunk(s, c) {
			keys = append(keys, deps.StrokeComponentKey(inputHash, c.ID, s, "candidate:"+inputHash))
		}
	}
	return deps.ChunkOutputKey(c, inputHash, keys)
}

func anyNonZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

func findChunkResult(chunks []model.ChunkResult, id string) *model.ChunkResult {
	for i := range chunks {
		if chunks[i].ChunkID == id {
			return &chunks[i]
		}
	}
	return nil
}

func sortJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func ResidualDigest(s model.Stroke) string {
	type residual struct {
		Field      string `json:"field"`
		Residual   int    `json:"residual"`
		Conversion string `json:"conversion"`
	}
	var rows []residual
	for _, tr := range s.Traces {
		if tr.Residual != 0 || tr.Conversion != "canonical_identity" && tr.Conversion != "absolute_pixel_integer_pair" && tr.Conversion != "integer_identity_default_1_when_absent" {
			rows = append(rows, residual{Field: tr.Field, Residual: tr.Residual, Conversion: tr.Conversion})
		}
	}
	return model.HashCanonical(rows)
}
