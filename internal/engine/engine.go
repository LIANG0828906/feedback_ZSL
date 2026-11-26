package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/checkpoint"
	"layerforge/internal/model"
	"layerforge/internal/sqlitefile"
)

const (
	maxThreadLimit     = 256
	maxBufferPerThread = 1024
	maxMemoryBudget    = int64(1) << 40
	maxRoundLimit      = 100000
)

type Engine struct {
	Project    model.Project
	Graph      *build.Graph
	Derivation *build.Derivation
	Store      *checkpoint.Store
}

func New(project model.Project, store *checkpoint.Store) (*Engine, error) {
	if store == nil || store.Path == "" {
		return nil, apperr.New(apperr.CheckpointIO, "new-engine", "store", "checkpoint store path is required")
	}
	project = project.Clone()
	graph, err := build.BuildGraph(project)
	if err != nil {
		return nil, err
	}
	derivation, err := build.Derive(graph)
	if err != nil {
		return nil, err
	}
	return &Engine{Project: project, Graph: graph, Derivation: derivation, Store: store}, nil
}

func (e *Engine) Build(ctx context.Context, options BuildOptions) (BuildResult, error) {
	budget, err := effectiveBudget(e.Project.Budget, options.Budget)
	if err != nil {
		return BuildResult{}, err
	}
	result := BuildResult{
		ProjectID:    e.Project.ID,
		ProjectHash:  e.Derivation.ProjectHash,
		Decisions:    make([]Decision, 0, len(e.Graph.Steps)),
		Rounds:       make([]RoundReport, 0),
		Degradations: make([]Degradation, 0),
		Backpressure: make([]BackpressureEvent, 0),
		Codes:        make([]apperr.Code, 0),
		FinalDigests: make(map[string]string),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		result.Interrupted = true
		return result, apperr.New(apperr.Interrupted, "build", "context", "interrupted before checkpoint load")
	default:
	}

	snapshot, err := e.Store.Load()
	switch {
	case errors.Is(err, checkpoint.ErrNoCheckpoint):
		snapshot = checkpoint.NewSnapshot(e.Project.ID)
	case apperr.IsCode(err, apperr.CheckpointVersion):
		result.Recovery = Recovery{Code: apperr.CheckpointVersion, Reason: err.Error(), Reset: true}
		if resetErr := e.Store.Reset(e.Project.ID); resetErr != nil {
			return result, resetErr
		}
		snapshot = checkpoint.NewSnapshot(e.Project.ID)
	case err != nil:
		return result, err
	case snapshot.ProjectID != e.Project.ID:
		return result, apperr.New(apperr.RecoveryMismatch, "recover-checkpoint", e.Store.Path,
			fmt.Sprintf("persisted project %q does not match %q", snapshot.ProjectID, e.Project.ID))
	}
	if snapshot.Version != model.CheckpointVersion {
		result.Recovery = Recovery{Code: apperr.CheckpointVersion, Reason: "snapshot version mismatch", Reset: true}
		if err := e.Store.Reset(e.Project.ID); err != nil {
			return result, err
		}
		snapshot = checkpoint.NewSnapshot(e.Project.ID)
	}
	baseChanged := ensureBaseLayers(snapshot, e.Project)

	decisions := make(map[int]Decision, len(e.Graph.Topo))
	misses := make(map[int]bool)
	direct := directEdits(options.PreviousProject, e.Project)
	for _, number := range sortedNumbers(e.Graph.Steps) {
		step := e.Graph.Steps[number]
		derivation := e.Derivation.Steps[number]
		decision := Decision{
			Step:        number,
			Name:        step.Name,
			LayerID:     derivation.LayerID,
			CacheKey:    derivation.CacheKey,
			Digest:      derivation.LayerDigest,
			SizeBytes:   derivation.SizeBytes,
			BuildCostMS: step.BuildCost().Milliseconds(),
			MemoryBytes: step.MinimumMemoryBytes(),
		}
		if e.cacheHit(snapshot, derivation) {
			decision.Hit = true
			decision.Source = "cache"
			result.HitCount++
		} else {
			misses[number] = true
			decision.Source = missSource(e.Graph, direct, options.PreviousProject != nil, number)
		}
		decisions[number] = decision
	}

	if options.CommitHook != nil {
		e.Store.SetFaultHook(func(stage sqlitefile.CommitStage) error {
			return options.CommitHook(snapshot.LastRound, stage)
		})
		defer e.Store.SetFaultHook(nil)
	}

	if len(misses) == 0 {
		result.Decisions = orderedDecisions(decisions)
		result.FinalDigests = finalDigests(e.Derivation)
		if baseChanged {
			snapshot.Generation++
			if err := e.Store.CommitRound(snapshot); err != nil {
				return result, err
			}
		}
		return result, nil
	}

	built := make(map[int]bool)
	serialMode := false
	memoryDegraded := false
	roundDegraded := false
	latencyDegraded := false
	safety := 0
	maxSafety := len(misses)*max(4, budget.BufferPerThread) + budget.MaxRounds + 16

	for len(misses) > 0 {
		safety++
		if safety > maxSafety {
			return result, apperr.New(apperr.Internal, "build", "rounds", "scheduler made no progress")
		}
		select {
		case <-ctx.Done():
			result.Interrupted = true
			result.Decisions = orderedDecisions(decisions)
			result.FinalDigests = finalDigests(e.Derivation)
			return result, apperr.New(apperr.Interrupted, "build", "context", "interrupted before next round")
		default:
		}

		ready := readySteps(e.Graph, misses, built, decisions)
		if len(ready) == 0 {
			return result, apperr.New(apperr.GraphCycle, "schedule", "steps", "no ready layer although work remains")
		}
		if !serialMode {
			var memoryImpact []string
			for _, number := range ready {
				step := e.Graph.Steps[number]
				if step.MinimumMemoryBytes() > budget.MemoryPerThread {
					memoryImpact = append(memoryImpact, step.Output.ID)
				}
			}
			if len(memoryImpact) > 0 {
				serialMode = true
				if !memoryDegraded {
					memoryDegraded = true
					result.Degradations = append(result.Degradations, Degradation{
						Code:         apperr.BudgetMemory,
						Strategy:     "serial-streaming",
						Reason:       "per-thread memory budget is below one or more layer minimums",
						Round:        len(snapshot.Rounds) + 1,
						ImpactLayers: sortedStrings(memoryImpact),
					})
				}
			}
		}

		workerLimit := budget.ThreadLimit
		if serialMode {
			workerLimit = 1
		}
		capacity := workerLimit * budget.BufferPerThread
		if capacity <= 0 {
			return result, apperr.New(apperr.BackpressureQueue, "schedule", "bufferPerThread", "worker buffer capacity is exhausted")
		}
		workerLoads := make([]time.Duration, workerLimit)
		selected := make([]int, 0, min(capacity, len(ready)))
		deferredNumbers := make([]int, 0)
		for _, number := range ready {
			if len(selected) >= capacity {
				deferredNumbers = append(deferredNumbers, number)
				continue
			}
			cost := e.Graph.Steps[number].BuildCost()
			worker := chooseWorker(workerLoads, cost, budget.Latency)
			if worker < 0 {
				deferredNumbers = append(deferredNumbers, number)
				continue
			}
			workerLoads[worker] += cost
			selected = append(selected, number)
		}
		if len(selected) == 0 {
			if !latencyDegraded {
				latencyDegraded = true
				impact := make([]string, 0, len(ready))
				for _, number := range ready {
					impact = append(impact, e.Graph.Steps[number].Output.ID)
				}
				result.Degradations = append(result.Degradations, Degradation{
					Code:         apperr.BudgetLatency,
					Strategy:     "uninterruptible-single-layer",
					Reason:       "interaction latency budget is smaller than every ready layer",
					Round:        len(snapshot.Rounds) + 1,
					ImpactLayers: sortedStrings(impact),
				})
			}
			selected = append(selected, ready[0])
			workerLoads[0] = e.Graph.Steps[ready[0]].BuildCost()
			for _, number := range ready[1:] {
				deferredNumbers = append(deferredNumbers, number)
			}
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i] < selected[j] })
		effectiveWorkers := min(workerLimit, len(selected))

		builtLayers, err := e.materializeRound(ctx, selected, budget, serialMode)
		if err != nil {
			return result, err
		}
		roundNumber := len(snapshot.Rounds) + 1
		startSnapshot := snapshot.Clone()
		// Layer state is staged before the cache index, and Validate enforces
		// that every index entry references a committed layer.
		for _, layer := range builtLayers {
			snapshot.Layers[layer.LayerID] = checkpoint.LayerRecord{
				LayerID:      layer.LayerID,
				Digest:       layer.Digest,
				SizeBytes:    layer.SizeBytes,
				CacheKey:     layer.CacheKey,
				ProducerStep: layer.Step,
				State:        checkpoint.LayerStateCommitted,
				Round:        roundNumber,
			}
		}
		for _, layer := range builtLayers {
			for cacheKey, entry := range snapshot.Index {
				if entry.LayerID == layer.LayerID && cacheKey != layer.CacheKey {
					delete(snapshot.Index, cacheKey)
				}
			}
			snapshot.Index[layer.CacheKey] = checkpoint.IndexRecord{
				CacheKey:     layer.CacheKey,
				LayerID:      layer.LayerID,
				Digest:       layer.Digest,
				SizeBytes:    layer.SizeBytes,
				ProducerStep: layer.Step,
				Round:        roundNumber,
			}
		}

		selectedSet := make(map[int]bool, len(selected))
		for _, number := range selected {
			selectedSet[number] = true
		}
		selectedSetCopy := make(map[int]bool, len(selectedSet))
		for number := range selectedSet {
			selectedSetCopy[number] = true
		}
		deferred := make([]Deferral, 0, len(deferredNumbers))
		rank := 0
		for _, number := range ready {
			if selectedSet[number] {
				continue
			}
			rank++
			reason := "latency"
			code := apperr.BudgetLatency
			if len(selectedSetCopy) >= capacity {
				reason = "threads"
				code = apperr.BudgetThreads
				if capacity > workerLimit {
					reason = "buffer"
					code = apperr.BackpressureQueue
					result.Backpressure = append(result.Backpressure, BackpressureEvent{
						Code:    code,
						Round:   roundNumber,
						Step:    number,
						LayerID: e.Graph.Steps[number].Output.ID,
						Rank:    rank,
					})
				}
			}
			deferred = append(deferred, Deferral{
				Step:    number,
				LayerID: e.Graph.Steps[number].Output.ID,
				Reason:  reason,
				Code:    code,
				Rank:    rank,
				Round:   roundNumber,
			})
		}
		sort.Slice(deferred, func(i, j int) bool { return deferred[i].Rank < deferred[j].Rank })

		for _, number := range selected {
			built[number] = true
			delete(misses, number)
			decision := decisions[number]
			result.BuildCount++
			decisions[number] = decision
		}
		report := RoundReport{
			Round:           roundNumber,
			Built:           builtLayers,
			Deferred:        deferred,
			Remaining:       len(misses),
			LatencyBudgetMS: budget.Latency.Milliseconds(),
			LatencyUsedMS:   maxDuration(workerLoads).Milliseconds(),
			WorkerLimit:     workerLimit,
			WorkersUsed:     effectiveWorkers,
			MemoryPerThread: budget.MemoryPerThread,
			PeakMemoryBytes: peakMemory(builtLayers),
		}
		report.LatencyRemainingMS = report.LatencyBudgetMS - report.LatencyUsedMS
		result.Rounds = append(result.Rounds, report)
		deferredJSON, _ := json.Marshal(deferred)
		degradedJSON := ""
		if len(result.Degradations) > 0 {
			degradedJSONBytes, _ := json.Marshal(result.Degradations)
			degradedJSON = string(degradedJSONBytes)
		}
		builtIDs := make([]string, 0, len(builtLayers))
		for _, layer := range builtLayers {
			builtIDs = append(builtIDs, layer.LayerID)
		}
		deferredIDs := make([]string, 0, len(deferred))
		for _, item := range deferred {
			deferredIDs = append(deferredIDs, item.LayerID)
		}
		snapshot.Rounds = append(snapshot.Rounds, checkpoint.RoundRecord{
			Round:           roundNumber,
			BuiltLayers:     sortedStrings(builtIDs),
			DeferredLayers:  sortedStrings(deferredIDs),
			LatencyUsedMS:   report.LatencyUsedMS,
			LatencyBudgetMS: report.LatencyBudgetMS,
			WorkersUsed:     report.WorkersUsed,
			PeakMemoryBytes: report.PeakMemoryBytes,
			DeferredJSON:    string(deferredJSON),
			DegradedJSON:    degradedJSON,
		})
		snapshot.LastRound = roundNumber
		snapshot.Generation++
		if err := snapshot.Validate(); err != nil {
			*snapshot = *startSnapshot
			return result, err
		}
		if err := e.Store.CommitRound(snapshot); err != nil {
			*snapshot = *startSnapshot
			return result, err
		}

		if len(misses) > 0 && !roundDegraded && snapshot.LastRound >= budget.MaxRounds {
			roundDegraded = true
			impact := make([]string, 0, len(misses))
			for number := range misses {
				impact = append(impact, e.Graph.Steps[number].Output.ID)
			}
			result.Degradations = append(result.Degradations, Degradation{
				Code:         apperr.BudgetLatency,
				Strategy:     "batch-continue",
				Reason:       "declared interaction rounds cannot cover all remaining layers",
				Round:        snapshot.LastRound,
				ImpactLayers: sortedStrings(impact),
			})
		}
		if options.InterruptAfterRound > 0 && snapshot.LastRound >= options.InterruptAfterRound && len(misses) > 0 {
			result.Interrupted = true
			result.Decisions = orderedDecisions(decisions)
			result.FinalDigests = finalDigests(e.Derivation)
			result.Codes = resultCodes(result)
			return result, apperr.New(apperr.Interrupted, "build", "round", "interrupted after complete checkpoint round")
		}
	}

	result.Decisions = orderedDecisions(decisions)
	result.FinalDigests = finalDigests(e.Derivation)
	result.Codes = resultCodes(result)
	if err := e.verifyFinal(snapshot); err != nil {
		return result, err
	}
	return result, nil
}

func effectiveBudget(declared model.Budget, override *BudgetOverride) (model.Budget, error) {
	budget := declared
	if override != nil {
		budget = model.Budget{
			Latency:         override.Latency,
			MaxRounds:       override.MaxRounds,
			ThreadLimit:     override.ThreadLimit,
			MemoryPerThread: override.MemoryPerThread,
			BufferPerThread: override.BufferPerThread,
		}
	}
	if budget.ThreadLimit <= 0 || budget.ThreadLimit > maxThreadLimit {
		return model.Budget{}, apperr.New(apperr.BudgetThreads, "validate-budget", "threadLimit", "thread limit must be between 1 and 256")
	}
	if budget.MemoryPerThread <= 0 || budget.MemoryPerThread > maxMemoryBudget {
		return model.Budget{}, apperr.New(apperr.BudgetMemory, "validate-budget", "memoryPerThread", "memory budget is outside the supported range")
	}
	if budget.BufferPerThread <= 0 || budget.BufferPerThread > maxBufferPerThread {
		return model.Budget{}, apperr.New(apperr.BackpressureQueue, "validate-budget", "bufferPerThread", "buffer capacity is zero or outside the supported range")
	}
	if budget.Latency <= 0 || budget.Latency > 24*time.Hour {
		return model.Budget{}, apperr.New(apperr.BudgetLatency, "validate-budget", "latency", "interaction latency budget is outside the supported range")
	}
	if budget.MaxRounds <= 0 || budget.MaxRounds > maxRoundLimit {
		return model.Budget{}, apperr.New(apperr.BudgetLatency, "validate-budget", "maxRounds", "max rounds is outside the supported range")
	}
	return budget, nil
}

func ensureBaseLayers(snapshot *checkpoint.Snapshot, project model.Project) bool {
	changed := false
	for _, base := range project.Bases {
		record, ok := snapshot.Layers[base.ID]
		want := checkpoint.LayerRecord{
			LayerID:      base.ID,
			Digest:       base.Digest,
			SizeBytes:    base.SizeBytes,
			ProducerStep: 0,
			State:        checkpoint.LayerStateBase,
			Round:        0,
		}
		if !ok || record != want {
			snapshot.Layers[base.ID] = want
			changed = true
		}
	}
	return changed
}

func (e *Engine) cacheHit(snapshot *checkpoint.Snapshot, derivation build.StepDerivation) bool {
	entry, ok := snapshot.Index[derivation.CacheKey]
	if !ok || entry.LayerID != derivation.LayerID || entry.Digest != derivation.LayerDigest ||
		entry.SizeBytes != derivation.SizeBytes || entry.ProducerStep != derivation.Step {
		return false
	}
	layer, ok := snapshot.Layers[entry.LayerID]
	return ok &&
		layer.State == checkpoint.LayerStateCommitted &&
		layer.Digest == derivation.LayerDigest &&
		layer.SizeBytes == derivation.SizeBytes &&
		layer.CacheKey == derivation.CacheKey &&
		layer.ProducerStep == derivation.Step
}

func readySteps(graph *build.Graph, misses, built map[int]bool, decisions map[int]Decision) []int {
	var ready []int
	for _, number := range graph.Topo {
		if !misses[number] {
			continue
		}
		ok := true
		for _, dependency := range graph.Steps[number].DependsOn {
			producer := graph.Layers[dependency].ProducerStep
			if producer == 0 {
				continue
			}
			if !built[producer] && !decisions[producer].Hit {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, number)
		}
	}
	sort.Ints(ready)
	return ready
}

func chooseWorker(loads []time.Duration, cost, budget time.Duration) int {
	best := -1
	var bestLoad time.Duration
	for index, load := range loads {
		if load+cost > budget {
			continue
		}
		if best < 0 || load < bestLoad {
			best = index
			bestLoad = load
		}
	}
	return best
}

func (e *Engine) materializeRound(ctx context.Context, selected []int, budget model.Budget, serial bool) ([]BuiltLayer, error) {
	type job struct {
		index int
		step  int
	}
	type completed struct {
		index int
		layer BuiltLayer
		err   error
	}
	jobs := make(chan job, len(selected))
	results := make(chan completed, len(selected))
	var wg sync.WaitGroup
	workers := min(budget.ThreadLimit, len(selected))
	if serial {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				step := e.Graph.Steps[item.step]
				derivation := e.Derivation.Steps[item.step]
				if err := ctx.Err(); err != nil {
					results <- completed{index: item.index, err: err}
					continue
				}
				computed := model.HashFields(
					"layerforge-layer/v1",
					derivation.LayerID,
					strconv.FormatInt(derivation.SizeBytes, 10),
					derivation.CacheKey,
				)
				layer := BuiltLayer{
					Step:        item.step,
					LayerID:     derivation.LayerID,
					CacheKey:    derivation.CacheKey,
					Digest:      computed,
					SizeBytes:   derivation.SizeBytes,
					BuildCostMS: step.BuildCost().Milliseconds(),
					MemoryBytes: step.MinimumMemoryBytes(),
					Streaming:   serial && step.MinimumMemoryBytes() > budget.MemoryPerThread,
				}
				if layer.Streaming {
					layer.MemoryBytes = budget.MemoryPerThread
				}
				if computed != derivation.LayerDigest {
					results <- completed{index: item.index, err: apperr.New(apperr.Internal, "materialize", derivation.LayerID, "computed digest differs from derivation")}
					continue
				}
				results <- completed{index: item.index, layer: layer}
			}
		}()
	}
	for index, step := range selected {
		jobs <- job{index: index, step: step}
	}
	close(jobs)
	wg.Wait()
	close(results)
	layers := make([]BuiltLayer, 0, len(selected))
	for item := range results {
		if item.err != nil {
			code := apperr.Internal
			if errors.Is(item.err, context.Canceled) || errors.Is(item.err, context.DeadlineExceeded) {
				code = apperr.Interrupted
			}
			return nil, apperr.Wrap(code, "materialize", "workers", item.err)
		}
		layers = append(layers, item.layer)
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].Step < layers[j].Step })
	return layers, nil
}

func (e *Engine) verifyFinal(snapshot *checkpoint.Snapshot) error {
	for _, step := range e.Project.Steps {
		derivation := e.Derivation.Steps[step.Number]
		layer, ok := snapshot.Layers[step.Output.ID]
		if !ok {
			return apperr.WithLayers(apperr.WithSteps(apperr.New(apperr.CheckpointIntegrity, "verify-final", "layers", "final layer is missing"), step.Number), step.Output.ID)
		}
		if layer.State != checkpoint.LayerStateCommitted || layer.Digest != derivation.LayerDigest || layer.SizeBytes != derivation.SizeBytes {
			return apperr.WithLayers(apperr.WithSteps(apperr.New(apperr.CheckpointIntegrity, "verify-final", "layers", "final layer digest differs from deterministic truth"), step.Number), step.Output.ID)
		}
	}
	return nil
}

func directEdits(previous *model.Project, current model.Project) map[int]string {
	if previous == nil {
		return nil
	}
	oldSteps := previous.StepSnapshots()
	newSteps := current.StepSnapshots()
	out := make(map[int]string)
	for number, now := range newSteps {
		before, ok := oldSteps[number]
		if !ok {
			out[number] = "step-added"
			continue
		}
		switch {
		case !before.InputsEqual(now):
			out[number] = "inputs"
		case before.CommandFingerprint != now.CommandFingerprint:
			out[number] = "command"
		case !before.DependsEqual(now):
			out[number] = "dependencies"
		case before.Output != now.Output:
			out[number] = "output"
		}
	}
	for number := range oldSteps {
		if _, ok := newSteps[number]; !ok {
			out[number] = "step-removed"
		}
	}
	return out
}

func missSource(graph *build.Graph, direct map[int]string, incremental bool, number int) string {
	if !incremental {
		return "cache-miss"
	}
	if reason, ok := direct[number]; ok {
		return "edit:" + reason
	}
	seen := make(map[int]bool)
	queue := append([]int(nil), directProducersOf(graph, number)...)
	sort.Ints(queue)
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if reason, ok := direct[candidate]; ok {
			return "upstream:" + strconv.Itoa(candidate) + ":edit:" + reason
		}
		queue = append(queue, directProducersOf(graph, candidate)...)
		sort.Ints(queue)
	}
	return "cache-miss"
}

func directProducersOf(graph *build.Graph, number int) []int {
	step, ok := graph.Steps[number]
	if !ok {
		return nil
	}
	var out []int
	for _, dependency := range step.DependsOn {
		producer := graph.Layers[dependency].ProducerStep
		if producer > 0 {
			out = append(out, producer)
		}
	}
	sort.Ints(out)
	return out
}

func orderedDecisions(decisions map[int]Decision) []Decision {
	numbers := make([]int, 0, len(decisions))
	for number := range decisions {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	out := make([]Decision, 0, len(numbers))
	for _, number := range numbers {
		out = append(out, decisions[number])
	}
	return out
}

func finalDigests(derivation *build.Derivation) map[string]string {
	out := make(map[string]string, len(derivation.Layers))
	for layerID, layer := range derivation.Layers {
		out[layerID] = layer.Digest
	}
	return out
}

func sortedNumbers(steps map[int]model.Step) []int {
	out := make([]int, 0, len(steps))
	for number := range steps {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func maxDuration(values []time.Duration) time.Duration {
	var result time.Duration
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func peakMemory(layers []BuiltLayer) int64 {
	var peak int64
	for _, layer := range layers {
		if layer.MemoryBytes > peak {
			peak = layer.MemoryBytes
		}
	}
	return peak
}

func resultCodes(result BuildResult) []apperr.Code {
	seen := make(map[apperr.Code]bool)
	codes := make([]apperr.Code, 0)
	add := func(code apperr.Code) {
		if code == "" || seen[code] {
			return
		}
		seen[code] = true
		codes = append(codes, code)
	}
	for _, event := range result.Backpressure {
		add(event.Code)
	}
	for _, degradation := range result.Degradations {
		add(degradation.Code)
	}
	for _, round := range result.Rounds {
		for _, deferred := range round.Deferred {
			add(deferred.Code)
		}
	}
	add(result.Recovery.Code)
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
