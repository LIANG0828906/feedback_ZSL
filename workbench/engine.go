package workbench

import (
	"context"
	"fmt"
	"sync"
	"time"

	"yuvench/errs"
	"yuvench/filters"
	"yuvench/yuv"
)

type Engine struct {
	mu          sync.Mutex
	store       *Store
	initialized bool
	cache       map[string]*CacheEntry
	activeMeta  SnapshotMeta
	persisted   map[string]bool
	loadReason  string
}

type nodeResult struct {
	nodeID   string
	output   *NodeOutput
	entry    *CacheEntry
	duration time.Duration
	err      error
}

func NewEngine(store *Store) *Engine {
	return &Engine{
		store:     store,
		cache:     make(map[string]*CacheEntry),
		persisted: make(map[string]bool),
	}
}

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	return e.store.Close()
}

func (e *Engine) initialize(image *yuv.Image, params filters.Params, forceFull bool) error {
	if e.initialized {
		return nil
	}
	e.initialized = true
	expected := snapshotMetaFor(image, params)
	loaded, err := e.store.Load(expected)
	if err != nil {
		return err
	}
	if forceFull {
		e.loadReason = "forced-full"
		return nil
	}
	if loaded.HardReason != "" {
		e.loadReason = loaded.HardReason
		return nil
	}
	if loaded.Exists {
		e.activeMeta = loaded.Meta
		e.cache = loaded.Stages
		for node := range loaded.Stages {
			e.persisted[node] = true
		}
	}
	return nil
}

func snapshotMetaFor(image *yuv.Image, params filters.Params) SnapshotMeta {
	meta := SnapshotMeta{
		SchemaVersion:    SchemaVersion,
		AlgorithmVersion: AlgorithmVersion,
		ParamVersion:     params.ImageParamsHash(),
		ChromaVersion:    image.ChromaVersion,
		InputHash:        image.InputHash,
		ManifestHash:     image.ManifestHash,
	}
	meta.RunID = ComputeRunID(meta)
	return meta
}

// Run executes the filter DAG. Unchanged stages are loaded from the content
// addressed cache; invalid stages and their downstream closure are recomputed.
func (e *Engine) Run(ctx context.Context, image *yuv.Image, params filters.Params, limits filters.Limits, options RunOptions) (*RunResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if image == nil {
		return nil, errs.New(errs.CodePlane, "image is nil")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if limits.MaxParallel == 0 {
		limits = filters.DefaultLimits()
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	forceFull := options.ForceFull || options.Mode == "full"
	if err := e.initialize(image, params, forceFull); err != nil {
		return nil, err
	}
	graph, err := BuildGraph(image, params)
	if err != nil {
		return nil, err
	}
	original, err := yuv.Upsample420(image.Meta, image.Y, image.U, image.V)
	if err != nil {
		return nil, &errs.Error{Code: errs.CodePlane, Message: "interleave original image", Cause: err.Error()}
	}
	expectedMeta := snapshotMetaFor(image, params)
	hardReason := ""
	if forceFull {
		hardReason = "forced-full"
	} else if e.loadReason != "" && len(e.cache) == 0 {
		hardReason = e.loadReason
	}
	invalidations, invalid := EvaluateInvalidations(graph, e.cache, hardReason)
	invalidReason := make(map[string]string, len(invalidations))
	for _, item := range invalidations {
		invalidReason[item.Node] = item.Reason
	}

	resumeSame := e.activeMeta.RunID == expectedMeta.RunID && e.activeMeta.RunID != ""
	if !resumeSame || forceFull {
		if err := e.store.BeginSnapshot(expectedMeta); err != nil {
			return nil, err
		}
		e.activeMeta = expectedMeta
		e.persisted = make(map[string]bool)
		e.loadReason = ""
	}

	started := time.Now()
	result := &RunResult{
		OK:              true,
		Mode:            normalizedMode(options.Mode, forceFull),
		RunID:           expectedMeta.RunID,
		ImageName:       image.Meta.Name,
		Image:           image.Meta,
		Params:          params,
		ParamsVersion:   params.FullParamsHash(),
		InputHash:       image.InputHash,
		OriginalSummary: original.Summary(),
		Invalidations:   invalidations,
		Graph:           graph,
		Original:        original,
		Nodes:           make(map[string]*NodeOutput),
		Snapshot:        expectedMeta,
		SnapshotReason:  hardReason,
	}
	result.Stats = Stats{Mode: result.Mode, Stages: make([]StageStat, len(graph.Nodes))}
	for index, node := range graph.Nodes {
		result.Stats.Stages[index] = StageStat{
			NodeID:        node.ID,
			Plane:         node.Plane,
			Stage:         node.Stage,
			Seq:           node.Seq,
			InvalidReason: invalidReason[node.ID],
		}
	}

	done := make(map[string]bool, len(graph.Nodes))
	launched := make(map[string]bool, len(graph.Nodes))
	failed := make(map[string]bool, len(graph.Nodes))
	outputs := make(map[string]*NodeOutput, len(graph.Nodes))
	entries := make(map[string]*CacheEntry, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if !invalid[node.ID] {
			if entry := e.cache[node.ID]; entry != nil && entry.Output != nil {
				done[node.ID] = true
				outputs[node.ID] = entry.Output
				entries[node.ID] = entry
				updateStat(&result.Stats, node.ID, func(stat *StageStat) {
					stat.CacheHit = true
				})
				result.Stats.CacheHits++
			}
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan nodeResult, len(graph.Nodes))
	sem := make(chan struct{}, limits.MaxParallel)
	running := 0
	var firstErr error
	var firstErrNode string
	var statsMu sync.Mutex
	var outputsMu sync.RWMutex
	var wg sync.WaitGroup
	nextCommit := 0
	paused := false
	cancelReason := error(nil)

	runtime := filters.Runtime{
		Limits: limits,
		OnBufferWait: func(reason string) {
			statsMu.Lock()
			result.Stats.BufferWaitCount++
			result.Stats.BufferWaits = append(result.Stats.BufferWaits, reason)
			statsMu.Unlock()
		},
	}

	launchReady := func() {
		if firstErr != nil || paused || runCtx.Err() != nil {
			return
		}
		for _, node := range graph.Nodes {
			if launched[node.ID] || done[node.ID] || failed[node.ID] || !invalid[node.ID] {
				continue
			}
			ready := true
			for _, dep := range node.Deps {
				if !done[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			select {
			case sem <- struct{}{}:
			default:
				return
			}
			launched[node.ID] = true
			running++
			wg.Add(1)
			go func(node *Node) {
				defer wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				if options.Fault != nil {
					if fault := options.Fault(node.ID); fault != nil {
						results <- nodeResult{nodeID: node.ID, duration: time.Since(start), err: fault}
						return
					}
				}
				output, computeErr := computeNode(runCtx, image, graph, node, outputs, &outputsMu, params, runtime)
				if computeErr != nil {
					results <- nodeResult{nodeID: node.ID, duration: time.Since(start), err: computeErr}
					return
				}
				upstreamKeys := make([]string, 0, len(node.Deps))
				for _, dep := range node.Deps {
					upstreamKeys = append(upstreamKeys, graph.ByID[dep].CacheKey)
				}
				entry := &CacheEntry{
					NodeID:        node.ID,
					CacheKey:      node.CacheKey,
					InputHash:     node.InputHash,
					ParamsHash:    node.ParamsHash,
					ParamVersion:  expectedMeta.ParamVersion,
					ChromaVersion: node.ChromaVersion,
					UpstreamKeys:  upstreamKeys,
					Summary:       output.Summary,
					Output:        output,
				}
				results <- nodeResult{nodeID: node.ID, output: output, entry: entry, duration: time.Since(start)}
			}(node)
		}
	}

	for {
		if !paused && options.Pause != nil {
			select {
			case <-options.Pause:
				paused = true
				cancel()
			default:
			}
		}
		if paused {
			break
		}
		launchReady()
		committedSomething := false
		for nextCommit < len(graph.Nodes) {
			node := graph.Nodes[nextCommit]
			if !done[node.ID] || failed[node.ID] {
				break
			}
			if options.PauseAfterCommits > 0 && result.Stats.Committed >= options.PauseAfterCommits {
				paused = true
				cancel()
				break
			}
			entry := entries[node.ID]
			outputsMu.RLock()
			output := outputs[node.ID]
			outputsMu.RUnlock()
			if entry == nil || output == nil {
				break
			}
			stat := findStat(&result.Stats, node.ID)
			persistedNow := false
			if stat.CacheHit {
				if !e.persisted[node.ID] {
					if err := e.store.CommitStage(expectedMeta, node, entry, output); err != nil {
						firstErr = err
						firstErrNode = node.ID
						break
					}
					e.persisted[node.ID] = true
					result.Stats.ReusedCommitted++
					persistedNow = true
				}
			} else {
				if err := e.store.CommitStage(expectedMeta, node, entry, output); err != nil {
					firstErr = err
					firstErrNode = node.ID
					break
				}
				e.persisted[node.ID] = true
				stat.Committed = true
				result.Stats.Committed++
				persistedNow = true
			}
			stat.Persisted = persistedNow
			stat.Reused = stat.CacheHit
			e.cache[node.ID] = entry
			result.Nodes[node.ID] = output
			result.CommitSequence = append(result.CommitSequence, StageCommit{
				Seq:      node.Seq,
				NodeID:   node.ID,
				Plane:    node.Plane,
				Stage:    node.Stage,
				CacheKey: node.CacheKey,
				Summary:  output.Summary,
				Reused:   stat.CacheHit,
			})
			nextCommit++
			committedSomething = true
		}
		if paused {
			break
		}
		if firstErr != nil {
			cancel()
			break
		}
		if nextCommit == len(graph.Nodes) {
			break
		}
		if running == 0 {
			launchReady()
			if running == 0 {
				if runCtx.Err() != nil {
					cancelReason = &errs.Error{Code: errs.CodeCancel, Message: "run cancelled", Cause: runCtx.Err().Error()}
					break
				}
				cancelReason = &errs.Error{Code: errs.CodeStage, Message: "dependency graph stalled", Node: graph.Nodes[nextCommit].ID}
				break
			}
		}
		if committedSomething {
			continue
		}
		select {
		case item := <-results:
			running--
			if item.err != nil {
				code := errs.CodeStage
				if business, ok := errs.As(item.err); ok {
					code = business.Code
				}
				wrapped := &errs.Error{Code: code, Message: "stage failed", Node: item.nodeID, Cause: item.err.Error()}
				failed[item.nodeID] = true
				if firstErr == nil {
					firstErr = wrapped
					firstErrNode = item.nodeID
					updateStat(&result.Stats, item.nodeID, func(stat *StageStat) {
						stat.Failed = true
						stat.Error = item.err.Error()
						stat.DurationMS = float64(item.duration.Microseconds()) / 1000.0
					})
					result.Stats.Failed++
				}
				cancel()
				continue
			}
			done[item.nodeID] = true
			outputsMu.Lock()
			outputs[item.nodeID] = item.output
			outputsMu.Unlock()
			entries[item.nodeID] = item.entry
			updateStat(&result.Stats, item.nodeID, func(stat *StageStat) {
				stat.Recomputed = true
				stat.DurationMS = float64(item.duration.Microseconds()) / 1000.0
			})
			result.Stats.Recomputed++
		case <-runCtx.Done():
			cancelReason = &errs.Error{Code: errs.CodeCancel, Message: "run cancelled", Cause: runCtx.Err().Error()}
		}
		if cancelReason != nil {
			break
		}
	}

	cancel()
	wg.Wait()
	statsMu.Lock()
	result.Stats.TotalDurationMS = NowMS(started)
	statsMu.Unlock()

	if paused {
		result.Complete = false
		result.Paused = true
		result.OK = true
		result.Snapshot.Complete = false
		_ = e.store.SetComplete(expectedMeta, false)
		return result, nil
	}
	e.activeMeta = expectedMeta
	if firstErr != nil {
		result.OK = false
		result.Complete = false
		result.Snapshot.Complete = false
		_ = e.store.SetComplete(expectedMeta, false)
		if firstErrNode != "" {
			if business, ok := errs.As(firstErr); ok {
				business.Message = "stage execution failed: " + business.Message
			}
		}
		return result, firstErr
	}
	if cancelReason != nil {
		result.OK = false
		result.Complete = false
		result.Snapshot.Complete = false
		_ = e.store.SetComplete(expectedMeta, false)
		return result, cancelReason
	}
	if nextCommit != len(graph.Nodes) {
		result.OK = false
		return result, errs.New(errs.CodeStage, "pipeline did not commit every stage")
	}
	if err := e.store.SetComplete(expectedMeta, true); err != nil {
		result.OK = false
		return result, err
	}
	result.Complete = true
	result.Snapshot.Complete = true
	outputsMu.RLock()
	finalOutput := outputs[graph.FinalID]
	outputsMu.RUnlock()
	if finalOutput == nil {
		result.OK = false
		return result, errs.New(errs.CodeStage, "final stage output is missing")
	}
	result.Final = finalOutput.Composite
	result.FinalSummary = finalOutput.Summary
	result.FinalSize = [2]int{result.Final.Width, result.Final.Height}
	return result, nil
}

func computeNode(ctx context.Context, image *yuv.Image, graph *Graph, node *Node, outputs map[string]*NodeOutput, outputsMu *sync.RWMutex, params filters.Params, runtime filters.Runtime) (*NodeOutput, error) {
	if node.ID == filters.NodeComposite {
		outputsMu.RLock()
		yc := outputs[filters.NodeYContrast]
		ug := outputs[filters.NodeUGain]
		vg := outputs[filters.NodeVGain]
		outputsMu.RUnlock()
		if yc == nil || ug == nil || vg == nil || yc.Plane == nil || ug.Plane == nil || vg.Plane == nil {
			return nil, errs.New(errs.CodeStage, "missing interleave dependencies")
		}
		composite, err := yuv.Upsample420(image.Meta, yc.Plane, ug.Plane, vg.Plane)
		if err != nil {
			return nil, &errs.Error{Code: errs.CodeStage, Message: "interleave enhanced image", Node: node.ID, Cause: err.Error()}
		}
		if composite.Width != image.Meta.Width || composite.Height != image.Meta.Height {
			return nil, &errs.Error{Code: errs.CodeStage, Message: "interleave changed image dimensions", Node: node.ID, Expected: fmt.Sprintf("%dx%d", image.Meta.Width, image.Meta.Height), Actual: fmt.Sprintf("%dx%d", composite.Width, composite.Height)}
		}
		return &NodeOutput{Composite: composite, Summary: composite.Summary()}, nil
	}
	var input *yuv.Plane
	if len(node.Deps) == 0 {
		switch node.Plane {
		case "Y":
			input = image.Y
		case "U":
			input = image.U
		case "V":
			input = image.V
		}
	} else {
		outputsMu.RLock()
		dep := outputs[node.Deps[0]]
		outputsMu.RUnlock()
		if dep != nil {
			input = dep.Plane
		}
	}
	if input == nil {
		return nil, &errs.Error{Code: errs.CodeStage, Message: "missing stage input", Node: node.ID}
	}
	plane, err := filters.Apply(ctx, input, node.ID, params, runtime)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", node.ID, err)
	}
	if plane.Width != input.Width || plane.Height != input.Height || plane.BitDepth != input.BitDepth {
		return nil, &errs.Error{Code: errs.CodeStage, Message: "stage changed plane shape", Node: node.ID}
	}
	return &NodeOutput{Plane: plane, Summary: plane.Summary()}, nil
}

func updateStat(stats *Stats, nodeID string, mutate func(*StageStat)) {
	if stat := findStat(stats, nodeID); stat != nil {
		mutate(stat)
	}
}

func findStat(stats *Stats, nodeID string) *StageStat {
	for index := range stats.Stages {
		if stats.Stages[index].NodeID == nodeID {
			return &stats.Stages[index]
		}
	}
	return nil
}

func normalizedMode(mode string, forceFull bool) string {
	if forceFull {
		return "full"
	}
	switch mode {
	case "full", "incremental", "resume":
		return mode
	default:
		return "incremental"
	}
}
