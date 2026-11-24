package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/checkpoint"
	"terrainnormalverify/internal/dependency"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/projectio"
	"terrainnormalverify/internal/seam"
)

type Config struct {
	Budget            Budget
	FaultBlockID      int
	CrashAfterCommits int
}

type Edit struct {
	Kind     string  `json:"kind"`
	BlockID  int     `json:"blockId,omitempty"`
	LayerID  string  `json:"layerId,omitempty"`
	X        int     `json:"x,omitempty"`
	Y        int     `json:"y,omitempty"`
	Value    float64 `json:"value,omitempty"`
	BiasX    float64 `json:"biasX,omitempty"`
	BiasY    float64 `json:"biasY,omitempty"`
	BiasZ    float64 `json:"biasZ,omitempty"`
	Strength float64 `json:"strength,omitempty"`
	Operator string  `json:"operator,omitempty"`
}

type BlockStat struct {
	BlockID     int     `json:"blockId"`
	Rebaked     bool    `json:"rebaked"`
	CacheHit    bool    `json:"cacheHit"`
	BakeCount   int     `json:"bakeCount"`
	DurationMS  float64 `json:"durationMs"`
	ContentHash string  `json:"contentHash"`
	NormalHash  string  `json:"normalHash"`
	SlopeHash   string  `json:"slopeHash"`
	Failure     string  `json:"failure,omitempty"`
}

type RunReport struct {
	Mode                string                         `json:"mode"`
	Revision            int64                          `json:"revision"`
	ParameterVersion    string                         `json:"parameterVersion"`
	AlgorithmVersion    string                         `json:"algorithmVersion"`
	Affected            []int                          `json:"affected"`
	Invalidation        []dependency.InvalidationEntry `json:"invalidation"`
	CommitOrder         []int                          `json:"commitOrder"`
	SnapshotOrder       []int                          `json:"snapshotOrder"`
	Seams               []seam.SeamReport              `json:"seams"`
	Queue               []QueueRecord                  `json:"queue"`
	Blocks              []BlockStat                    `json:"blocks"`
	CacheHits           int                            `json:"cacheHits"`
	Rebaked             int                            `json:"rebaked"`
	Failed              int                            `json:"failed"`
	PeakMemoryBytes     int64                          `json:"peakMemoryBytes"`
	WorkerCount         int                            `json:"workerCount"`
	SnapshotInvalidated bool                           `json:"snapshotInvalidated"`
	TotalDurationMS     float64                        `json:"totalDurationMs"`
}

type Snapshot struct {
	Revision         int64                     `json:"revision"`
	ParameterVersion string                    `json:"parameterVersion"`
	AlgorithmVersion string                    `json:"algorithmVersion"`
	Blocks           map[int]*bake.BlockResult `json:"blocks"`
	Seams            []seam.SeamReport         `json:"seams"`
	CommitOrder      []int                     `json:"commitOrder"`
	SnapshotOrder    []int                     `json:"snapshotOrder"`
}

type Engine struct {
	project       *model.Project
	graph         *dependency.Graph
	store         *checkpoint.Store
	config        Config
	cache         map[int]*checkpoint.Checkpoint
	pending       map[int]*bake.BlockResult
	bakeCounts    map[int]int
	failures      map[int]error
	seams         []seam.SeamReport
	lastOrder     []int
	snapshotOrder []int
}

func New(project *model.Project, store *checkpoint.Store, config Config) (*Engine, error) {
	if project == nil {
		return nil, model.NewError(model.ErrSchema, -1, "project", "project is nil")
	}
	if store == nil {
		return nil, model.NewError(model.ErrIO, -1, "store", "checkpoint store is nil")
	}
	if err := projectio.Validate(project); err != nil {
		return nil, err
	}
	if err := bake.ValidateParameters(project.Parameters); err != nil {
		return nil, err
	}
	if err := config.Budget.Validate(); err != nil {
		return nil, err
	}
	graph := dependency.Build(project)
	return &Engine{
		project:    project.Clone(),
		graph:      graph,
		store:      store,
		config:     config,
		cache:      make(map[int]*checkpoint.Checkpoint),
		pending:    make(map[int]*bake.BlockResult),
		bakeCounts: make(map[int]int),
		failures:   make(map[int]error),
	}, nil
}

func (e *Engine) Project() *model.Project {
	return e.project.Clone()
}

func (e *Engine) Store() *checkpoint.Store {
	return e.store
}

func (e *Engine) SetFaultBlock(id int) {
	e.config.FaultBlockID = id
}

func (e *Engine) Initialize() (*RunReport, error) {
	started := time.Now()
	e.failures = make(map[int]error)
	e.pending = make(map[int]*bake.BlockResult)
	expected, err := e.expectedHashes(e.project, e.graph)
	if err != nil {
		return nil, err
	}
	paramVersion := model.ParameterVersion(e.project.Parameters)
	invalidated := false
	meta, err := e.store.LoadMeta()
	if err != nil {
		return nil, err
	}
	var loaded map[int]*checkpoint.Checkpoint
	if meta.Complete && (meta.AlgorithmVersion != "" && meta.AlgorithmVersion != model.AlgorithmVersion ||
		meta.ParameterVersion != "" && meta.ParameterVersion != paramVersion) {
		if err := e.store.Wipe(); err != nil {
			return nil, err
		}
		invalidated = true
		loaded = make(map[int]*checkpoint.Checkpoint)
	} else {
		loaded, err = e.store.LoadAll(expected, paramVersion, model.AlgorithmVersion)
		if err != nil {
			var modelErr *model.Error
			if ok := asModelError(err, &modelErr); ok && modelErr.Code == model.ErrSnapshot {
				invalidated = true
				loaded = make(map[int]*checkpoint.Checkpoint)
			} else {
				return nil, err
			}
		}
	}
	e.cache = loaded
	e.failures = make(map[int]error)
	var affected = make(map[int]string)
	for _, block := range e.project.SortedBlocks() {
		if _, ok := e.cache[block.ID]; !ok {
			affected[block.ID] = "checkpoint missing after resume"
		}
	}
	order := e.graph.TopologicalOrder(affected)
	report := &RunReport{
		Mode:                "resume",
		Revision:            e.project.Revision,
		ParameterVersion:    paramVersion,
		AlgorithmVersion:    model.AlgorithmVersion,
		Affected:            order,
		SnapshotInvalidated: invalidated,
	}
	if len(loaded) == 0 {
		report.Mode = "full"
	}
	if len(affected) > 0 {
		if err := e.executeBake(report, e.project, e.graph, affected, order); err != nil {
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, err
		}
		if report.Failed > 0 {
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, firstFailure(e.failures)
		}
		workingResults := e.workingResults()
		reports, err := seam.Apply(e.project, workingResults)
		if err != nil {
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, err
		}
		e.seams = reports
		cps, err := e.buildCheckpoints(e.project, e.graph, affected, workingResults, reports, e.project.Revision)
		if err != nil {
			return report, err
		}
		snapshotOrder := allTopo(e.project, e.graph)
		if err := e.commit(report, snapshotOrder, cps, e.project.Revision); err != nil {
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, err
		}
		e.adoptResults(workingResults, cps, affected)
	} else {
		workingResults := e.workingResults()
		reports, err := seam.Apply(e.project, workingResults)
		if err != nil {
			return report, err
		}
		e.seams = reports
		e.adoptResults(workingResults, nil, affected)
	}
	e.snapshotOrder = e.allBlocksTopo()
	report.SnapshotOrder = append([]int(nil), e.snapshotOrder...)
	if err := e.store.Finalize(checkpoint.Meta{
		Revision:         e.project.Revision,
		ProjectHash:      e.project.CanonicalHash(),
		ParameterVersion: paramVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Complete:         true,
		SnapshotOrder:    e.snapshotOrder,
	}); err != nil {
		return report, err
	}
	e.fillReport(report, e.project, e.graph, affected, order)
	report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
	return report, nil
}

func (e *Engine) BakeAll(reason string) (*RunReport, error) {
	started := time.Now()
	e.failures = make(map[int]error)
	e.pending = make(map[int]*bake.BlockResult)
	if reason == "" {
		reason = "full rebuild requested"
	}
	if err := e.store.Wipe(); err != nil {
		return nil, err
	}
	e.cache = make(map[int]*checkpoint.Checkpoint)
	e.failures = make(map[int]error)
	e.bakeCounts = make(map[int]int)
	affected := e.graph.InvalidateAll(e.project, reason)
	order := e.graph.TopologicalOrder(affected)
	report := &RunReport{
		Mode:             "full",
		Revision:         e.project.Revision,
		ParameterVersion: model.ParameterVersion(e.project.Parameters),
		AlgorithmVersion: model.AlgorithmVersion,
		Affected:         order,
	}
	if err := e.executeBake(report, e.project, e.graph, affected, order); err != nil {
		return report, err
	}
	if report.Failed > 0 {
		return report, firstFailure(e.failures)
	}
	workingResults := e.workingResults()
	reports, err := seam.Apply(e.project, workingResults)
	if err != nil {
		return report, err
	}
	e.seams = reports
	cps, err := e.buildCheckpoints(e.project, e.graph, affected, workingResults, reports, e.project.Revision)
	if err != nil {
		return report, err
	}
	snapshotOrder := allTopo(e.project, e.graph)
	if err := e.commit(report, snapshotOrder, cps, e.project.Revision); err != nil {
		return report, err
	}
	e.adoptResults(workingResults, cps, affected)
	e.snapshotOrder = e.allBlocksTopo()
	report.SnapshotOrder = append([]int(nil), e.snapshotOrder...)
	if err := e.store.Finalize(checkpoint.Meta{
		Revision:         e.project.Revision,
		ProjectHash:      e.project.CanonicalHash(),
		ParameterVersion: report.ParameterVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Complete:         true,
		SnapshotOrder:    e.snapshotOrder,
	}); err != nil {
		return report, err
	}
	e.fillReport(report, e.project, e.graph, affected, order)
	report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
	return report, nil
}

func (e *Engine) ApplyEdits(edits []Edit) (*RunReport, error) {
	started := time.Now()
	e.failures = make(map[int]error)
	e.pending = make(map[int]*bake.BlockResult)
	if len(edits) == 0 {
		report, err := e.Initialize()
		if report != nil {
			report.Mode = "incremental"
		}
		return report, err
	}
	working := e.project.Clone()
	working.Revision = e.project.Revision + 1
	graph := dependency.Build(working)
	affected := make(map[int]string)
	for _, edit := range edits {
		if err := applyEdit(working, edit); err != nil {
			return nil, err
		}
		var delta map[int]string
		switch edit.Kind {
		case "height_delta", "height_set":
			delta = graph.InvalidateHeight(edit.BlockID, fmt.Sprintf("%s edit on block %d", edit.Kind, edit.BlockID))
		case "layer_bias":
			delta = graph.InvalidateLayer(working, edit.LayerID, fmt.Sprintf("material layer %s parameter edit", edit.LayerID))
		case "difference_operator", "scale":
			delta = graph.InvalidateAll(working, fmt.Sprintf("%s parameter edit", edit.Kind))
		default:
			return nil, model.NewError(model.ErrSchema, edit.BlockID, "edit.kind", "unsupported edit kind")
		}
		for id, reason := range delta {
			if _, exists := affected[id]; !exists {
				affected[id] = reason
			}
		}
	}
	expected, err := e.expectedHashes(working, graph)
	if err != nil {
		return nil, err
	}
	var mismatchSeeds []int
	for _, block := range working.SortedBlocks() {
		if _, invalid := affected[block.ID]; invalid {
			continue
		}
		cp, ok := e.cache[block.ID]
		if !ok {
			affected[block.ID] = "cache miss"
			continue
		}
		if cp.ContentHash != expected[block.ID] {
			affected[block.ID] = "cache content hash mismatch"
			mismatchSeeds = append(mismatchSeeds, block.ID)
		}
	}
	for _, seed := range mismatchSeeds {
		for id := range graph.InvalidateHeight(seed, "downstream of cache hash mismatch") {
			if _, invalid := affected[id]; !invalid {
				affected[id] = "downstream of cache hash mismatch"
			}
		}
	}
	order := graph.TopologicalOrder(affected)
	report := &RunReport{
		Mode:             "incremental",
		Revision:         working.Revision,
		ParameterVersion: model.ParameterVersion(working.Parameters),
		AlgorithmVersion: model.AlgorithmVersion,
		Affected:         order,
	}
	if len(order) > 0 {
		if err := e.executeBake(report, working, graph, affected, order); err != nil {
			return report, err
		}
		if report.Failed > 0 {
			e.project = working
			e.graph = graph
			e.fillReport(report, working, graph, affected, order)
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, firstFailure(e.failures)
		}
		workingResults := e.workingResults()
		reports, err := seam.Apply(working, workingResults)
		if err != nil {
			e.project = working
			e.graph = graph
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, err
		}
		e.seams = reports
		cps, err := e.buildCheckpoints(working, graph, affected, workingResults, reports, working.Revision)
		if err != nil {
			return report, err
		}
		snapshotOrder := allTopo(working, graph)
		if err := e.commit(report, snapshotOrder, cps, working.Revision); err != nil {
			e.project = working
			e.graph = graph
			report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
			return report, err
		}
		e.adoptResults(workingResults, cps, affected)
	}
	e.project = working
	e.graph = graph
	e.snapshotOrder = e.allBlocksTopo()
	report.SnapshotOrder = append([]int(nil), e.snapshotOrder...)
	if err := e.store.Finalize(checkpoint.Meta{
		Revision:         working.Revision,
		ProjectHash:      working.CanonicalHash(),
		ParameterVersion: report.ParameterVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Complete:         true,
		SnapshotOrder:    e.snapshotOrder,
	}); err != nil {
		return report, err
	}
	e.fillReport(report, working, graph, affected, order)
	report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
	return report, nil
}

func (e *Engine) RetryFailed() (*RunReport, error) {
	started := time.Now()
	failed := make(map[int]bool, len(e.failures))
	for id := range e.failures {
		failed[id] = true
	}
	e.failures = make(map[int]error)
	e.pending = make(map[int]*bake.BlockResult)
	expected, err := e.expectedHashes(e.project, e.graph)
	if err != nil {
		return nil, err
	}
	affected := make(map[int]string)
	var seeds []int
	for id := range failed {
		affected[id] = "retry previously failed block"
		seeds = append(seeds, id)
	}
	for _, block := range e.project.SortedBlocks() {
		cp, ok := e.cache[block.ID]
		if !ok {
			affected[block.ID] = "retry cache miss"
			seeds = append(seeds, block.ID)
			continue
		}
		if cp.ContentHash != expected[block.ID] {
			affected[block.ID] = "retry content hash mismatch"
			seeds = append(seeds, block.ID)
		}
	}
	sort.Ints(seeds)
	for _, seed := range seeds {
		for id := range e.graph.InvalidateHeight(seed, "retry dependency closure") {
			if _, exists := affected[id]; !exists {
				affected[id] = "retry dependency closure"
			}
		}
	}
	return e.retryAffected(affected, started)
}

func (e *Engine) RetryBlock(blockID int) (*RunReport, error) {
	started := time.Now()
	if _, ok := e.project.Block(blockID); !ok {
		return nil, model.NewError(model.ErrSchema, blockID, "retry.blockId", "block does not exist")
	}
	e.failures = make(map[int]error)
	e.pending = make(map[int]*bake.BlockResult)
	affected := e.graph.InvalidateHeight(blockID, fmt.Sprintf("manual retry of block %d", blockID))
	return e.retryAffected(affected, started)
}

func (e *Engine) retryAffected(affected map[int]string, started time.Time) (*RunReport, error) {
	order := e.graph.TopologicalOrder(affected)
	report := &RunReport{
		Mode:             "retry",
		Revision:         e.project.Revision,
		ParameterVersion: model.ParameterVersion(e.project.Parameters),
		AlgorithmVersion: model.AlgorithmVersion,
		Affected:         order,
	}
	if len(order) == 0 {
		e.fillReport(report, e.project, e.graph, affected, order)
		report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
		return report, nil
	}
	if err := e.executeBake(report, e.project, e.graph, affected, order); err != nil {
		return report, err
	}
	if report.Failed > 0 {
		e.fillReport(report, e.project, e.graph, affected, order)
		return report, firstFailure(e.failures)
	}
	workingResults := e.workingResults()
	reports, err := seam.Apply(e.project, workingResults)
	if err != nil {
		return report, err
	}
	e.seams = reports
	cps, err := e.buildCheckpoints(e.project, e.graph, affected, workingResults, reports, e.project.Revision)
	if err != nil {
		return report, err
	}
	snapshotOrder := allTopo(e.project, e.graph)
	if err := e.commit(report, snapshotOrder, cps, e.project.Revision); err != nil {
		return report, err
	}
	e.adoptResults(workingResults, cps, affected)
	e.snapshotOrder = e.allBlocksTopo()
	report.SnapshotOrder = append([]int(nil), e.snapshotOrder...)
	if err := e.store.Finalize(checkpoint.Meta{
		Revision:         e.project.Revision,
		ProjectHash:      e.project.CanonicalHash(),
		ParameterVersion: report.ParameterVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Complete:         true,
		SnapshotOrder:    e.snapshotOrder,
	}); err != nil {
		return report, err
	}
	e.fillReport(report, e.project, e.graph, affected, order)
	report.TotalDurationMS = float64(time.Since(started).Microseconds()) / 1000
	return report, nil
}

func (e *Engine) executeBake(report *RunReport, project *model.Project, graph *dependency.Graph, affected map[int]string, order []int) error {
	results := Schedule(project, order, e.config.Budget, e.config.FaultBlockID)
	for id, err := range results.Failures {
		e.failures[id] = err
	}
	report.Queue = results.Queue
	report.PeakMemoryBytes = results.PeakMemory
	report.WorkerCount = results.WorkerCount
	for _, id := range order {
		if result, ok := results.Results[id]; ok {
			e.pending[id] = result
			e.bakeCounts[id]++
			report.Rebaked++
			report.Blocks = append(report.Blocks, BlockStat{
				BlockID:    id,
				Rebaked:    true,
				BakeCount:  e.bakeCounts[id],
				DurationMS: float64(results.Durations[id].Microseconds()) / 1000,
			})
		} else if err, failed := results.Failures[id]; failed {
			report.Failed++
			report.Blocks = append(report.Blocks, BlockStat{
				BlockID:   id,
				Rebaked:   true,
				BakeCount: e.bakeCounts[id],
				Failure:   err.Error(),
			})
		}
	}
	return nil
}

func (e *Engine) commit(report *RunReport, order []int, cps []checkpoint.Checkpoint, revision int64) error {
	if err := e.store.MarkIncomplete(revision); err != nil {
		return err
	}
	limit := len(cps)
	if e.config.CrashAfterCommits > 0 && e.config.CrashAfterCommits < limit {
		limit = e.config.CrashAfterCommits
	}
	if err := e.store.ReplaceBlocks(order, cps[:limit]); err != nil {
		return err
	}
	for i := 0; i < limit; i++ {
		report.CommitOrder = append(report.CommitOrder, cps[i].BlockID)
	}
	e.lastOrder = append([]int(nil), report.CommitOrder...)
	if limit < len(cps) {
		err := model.NewError(model.ErrIO, cps[limit].BlockID, "simulatedCrash", "simulated termination after partial checkpoint commit")
		return err
	}
	return nil
}

func (e *Engine) buildCheckpoints(project *model.Project, graph *dependency.Graph, _ map[int]string, results map[int]*bake.BlockResult, reports []seam.SeamReport, revision int64) ([]checkpoint.Checkpoint, error) {
	byBlock := seam.AggregateByBlock(reports)
	allAffected := make(map[int]string, len(project.Blocks))
	for _, block := range project.Blocks {
		allAffected[block.ID] = "snapshot"
	}
	order := graph.TopologicalOrder(allAffected)
	cps := make([]checkpoint.Checkpoint, 0, len(order))
	paramVersion := model.ParameterVersion(project.Parameters)
	for _, id := range order {
		result, ok := results[id]
		if !ok || result == nil {
			return nil, model.NewError(model.ErrBlock, id, "checkpoint.result", "missing computed block result")
		}
		contentHash, err := e.contentHash(project, graph, id)
		if err != nil {
			return nil, err
		}
		deps, _ := json.Marshal(graph.DependencySet(id))
		stats, _ := json.Marshal(byBlock[id])
		cp := checkpoint.Checkpoint{
			BlockID:          id,
			Revision:         revision,
			ContentHash:      contentHash,
			NormalHash:       model.HashNormals(result.Normals),
			SlopeHash:        model.HashFloat64(result.Slopes),
			ParameterVersion: paramVersion,
			AlgorithmVersion: model.AlgorithmVersion,
			Dependencies:     deps,
			SeamStats:        stats,
			Result:           result,
		}
		cps = append(cps, cp)
	}
	return cps, nil
}

func (e *Engine) expectedHashes(project *model.Project, graph *dependency.Graph) (map[int]string, error) {
	out := make(map[int]string, len(project.Blocks))
	for _, block := range project.Blocks {
		hash, err := e.contentHash(project, graph, block.ID)
		if err != nil {
			return nil, err
		}
		out[block.ID] = hash
	}
	return out, nil
}

type hashBlock struct {
	ID      int       `json:"id"`
	Name    string    `json:"name"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	OriginX int       `json:"originX"`
	OriginY int       `json:"originY"`
	Heights []float64 `json:"heights"`
	Weights [][]int   `json:"weights"`
}

type hashLayer struct {
	ID          string  `json:"id"`
	NormalBiasX float64 `json:"normalBiasX"`
	NormalBiasY float64 `json:"normalBiasY"`
	NormalBiasZ float64 `json:"normalBiasZ"`
	Strength    float64 `json:"strength"`
}

func (e *Engine) contentHash(project *model.Project, graph *dependency.Graph, id int) (string, error) {
	block, ok := project.Block(id)
	if !ok {
		return "", model.NewError(model.ErrSchema, id, "block", "block does not exist")
	}
	layerSet := make(map[string]bool)
	for _, layerID := range graph.BlockLayers[id] {
		layerSet[layerID] = true
	}
	for _, neighborID := range graph.Neighbors[id] {
		for _, layerID := range graph.BlockLayers[neighborID] {
			layerSet[layerID] = true
		}
	}
	var layers []hashLayer
	for _, layer := range project.Layers {
		if layerSet[layer.ID] {
			layers = append(layers, hashLayer{
				ID: layer.ID, NormalBiasX: layer.NormalBiasX, NormalBiasY: layer.NormalBiasY,
				NormalBiasZ: layer.NormalBiasZ, Strength: layer.Strength,
			})
		}
	}
	var neighbors []hashBlock
	for _, neighborID := range graph.Neighbors[id] {
		neighbor, ok := project.Block(neighborID)
		if !ok {
			return "", model.NewError(model.ErrSchema, neighborID, "block", "neighbor does not exist")
		}
		neighbors = append(neighbors, exportHashBlock(neighbor))
	}
	var seams []model.Seam
	for _, seam := range project.SortedSeams() {
		if seam.BlockA == id || seam.BlockB == id {
			seams = append(seams, seam)
		}
	}
	input := struct {
		Algorithm  string               `json:"algorithm"`
		Parameters model.BakeParameters `json:"parameters"`
		Block      hashBlock            `json:"block"`
		Neighbors  []hashBlock          `json:"neighbors"`
		Layers     []hashLayer          `json:"layers"`
		Seams      []model.Seam         `json:"seams"`
	}{
		Algorithm: model.AlgorithmVersion,
		Parameters: model.BakeParameters{
			DifferenceOperator: project.Parameters.DifferenceOperator,
			Scale:              project.Parameters.Scale,
			MaterialMix:        project.Parameters.MaterialMix,
			SeamRelaxation:     project.Parameters.SeamRelaxation,
		},
		Block:     exportHashBlock(block),
		Neighbors: neighbors,
		Layers:    layers,
		Seams:     seams,
	}
	return model.HashJSON(input), nil
}

func exportHashBlock(block *model.Block) hashBlock {
	return hashBlock{
		ID: block.ID, Name: block.Name, Width: block.Width, Height: block.Height,
		OriginX: block.OriginX, OriginY: block.OriginY,
		Heights: append([]float64(nil), block.Heights...),
		Weights: append([][]int(nil), block.Weights...),
	}
}

func applyEdit(project *model.Project, edit Edit) error {
	switch edit.Kind {
	case "height_delta", "height_set":
		block, ok := project.Block(edit.BlockID)
		if !ok {
			return model.NewError(model.ErrSchema, edit.BlockID, "edit.blockId", "block does not exist")
		}
		if !block.ValidXY(edit.X, edit.Y) {
			e := model.NewError(model.ErrSize, edit.BlockID, "edit.pixel", "pixel is outside block bounds")
			e.Value = [2]int{edit.X, edit.Y}
			return e
		}
		idx := block.Index(edit.X, edit.Y)
		if edit.Kind == "height_set" {
			block.Heights[idx] = edit.Value
		} else {
			block.Heights[idx] += edit.Value
		}
		if math.IsNaN(block.Heights[idx]) || math.IsInf(block.Heights[idx], 0) {
			return model.NewError(model.ErrSchema, edit.BlockID, "edit.value", "height must be finite")
		}
		return nil
	case "layer_bias":
		layer, ok := project.Layer(edit.LayerID)
		if !ok {
			return model.NewError(model.ErrSchema, -1, "edit.layerId", "material layer does not exist")
		}
		if math.Abs(edit.BiasX) > 2 || math.Abs(edit.BiasY) > 2 || math.Abs(edit.BiasZ) > 2 {
			return model.NewError(model.ErrPara, -1, "edit.layerBias", "normal bias must be in [-2,2]")
		}
		layer.NormalBiasX = edit.BiasX
		layer.NormalBiasY = edit.BiasY
		layer.NormalBiasZ = edit.BiasZ
		if edit.Strength > 0 {
			if edit.Strength > 4 {
				return model.NewError(model.ErrPara, -1, "edit.strength", "must be in (0,4]")
			}
			layer.Strength = edit.Strength
		}
		return nil
	case "difference_operator":
		candidate := project.Parameters
		candidate.DifferenceOperator = edit.Operator
		if err := bake.ValidateParameters(candidate); err != nil {
			return err
		}
		project.Parameters.DifferenceOperator = edit.Operator
		return nil
	case "scale":
		candidate := project.Parameters
		candidate.Scale = edit.Value
		if err := bake.ValidateParameters(candidate); err != nil {
			return err
		}
		project.Parameters.Scale = edit.Value
		return nil
	default:
		return model.NewError(model.ErrSchema, edit.BlockID, "edit.kind", "unsupported edit kind")
	}
}

func ApplyEditsToProject(project *model.Project, edits []Edit) (*model.Project, error) {
	working := project.Clone()
	if len(edits) > 0 {
		working.Revision = project.Revision + 1
	}
	for _, edit := range edits {
		if err := applyEdit(working, edit); err != nil {
			return nil, err
		}
	}
	return working, nil
}

func (e *Engine) workingResults() map[int]*bake.BlockResult {
	out := make(map[int]*bake.BlockResult, len(e.cache))
	for id, cp := range e.cache {
		if cp != nil && cp.Result != nil {
			out[id] = cp.Result.Clone()
		}
	}
	for id, result := range e.pending {
		out[id] = result.Clone()
	}
	return out
}

func (e *Engine) adoptResults(results map[int]*bake.BlockResult, cps []checkpoint.Checkpoint, affected map[int]string) {
	for i := range cps {
		cp := cps[i]
		if result, ok := results[cp.BlockID]; ok {
			cp.Result = result.Clone()
		}
		e.cache[cp.BlockID] = &cp
	}
	for id, result := range results {
		if _, invalid := affected[id]; invalid {
			continue
		}
		if cp, ok := e.cache[id]; ok {
			cp.Result = result.Clone()
		}
	}
	e.pending = make(map[int]*bake.BlockResult)
}

func (e *Engine) fillReport(report *RunReport, project *model.Project, graph *dependency.Graph, affected map[int]string, order []int) {
	baked := make(map[int]bool)
	for _, id := range order {
		baked[id] = true
	}
	report.Invalidation = graph.Manifest(project, affected, baked)
	report.Seams = e.seams
	report.SnapshotOrder = append([]int(nil), e.snapshotOrder...)
	report.CacheHits = 0
	statIndex := make(map[int]int, len(report.Blocks))
	for i := range report.Blocks {
		statIndex[report.Blocks[i].BlockID] = i
	}
	for _, block := range project.SortedBlocks() {
		cp, ok := e.cache[block.ID]
		if idx, exists := statIndex[block.ID]; exists {
			if ok && report.Blocks[idx].Failure == "" {
				report.Blocks[idx].ContentHash = cp.ContentHash
				report.Blocks[idx].NormalHash = cp.NormalHash
				report.Blocks[idx].SlopeHash = cp.SlopeHash
			}
			continue
		}
		if ok && !affectedHas(affected, block.ID) {
			report.CacheHits++
			report.Blocks = append(report.Blocks, BlockStat{
				BlockID:     block.ID,
				CacheHit:    true,
				BakeCount:   e.bakeCounts[block.ID],
				ContentHash: cp.ContentHash,
				NormalHash:  cp.NormalHash,
				SlopeHash:   cp.SlopeHash,
			})
		}
	}
	sort.Slice(report.Blocks, func(i, j int) bool { return report.Blocks[i].BlockID < report.Blocks[j].BlockID })
}

func (e *Engine) Snapshot() Snapshot {
	results := make(map[int]*bake.BlockResult, len(e.cache))
	for id, cp := range e.cache {
		if cp != nil && cp.Result != nil {
			results[id] = cp.Result.Clone()
		}
	}
	return Snapshot{
		Revision:         e.project.Revision,
		ParameterVersion: model.ParameterVersion(e.project.Parameters),
		AlgorithmVersion: model.AlgorithmVersion,
		Blocks:           results,
		Seams:            append([]seam.SeamReport(nil), e.seams...),
		CommitOrder:      append([]int(nil), e.lastOrder...),
		SnapshotOrder:    append([]int(nil), e.snapshotOrder...),
	}
}

func (e *Engine) allBlocksTopo() []int {
	return allTopo(e.project, e.graph)
}

func allTopo(project *model.Project, graph *dependency.Graph) []int {
	all := make(map[int]string, len(project.Blocks))
	for _, block := range project.Blocks {
		all[block.ID] = "snapshot"
	}
	return graph.TopologicalOrder(all)
}

func affectedHas(affected map[int]string, id int) bool {
	_, ok := affected[id]
	return ok
}

func asModelError(err error, target **model.Error) bool {
	for err != nil {
		if typed, ok := err.(*model.Error); ok {
			*target = typed
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

func firstFailure(failures map[int]error) error {
	ids := make([]int, 0, len(failures))
	for id := range failures {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	if len(ids) == 0 {
		return model.NewError(model.ErrBlock, -1, "bake", "unknown block failure")
	}
	return failures[ids[0]]
}
