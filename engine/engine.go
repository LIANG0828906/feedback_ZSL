package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"maskqc/checkpoint"
	"maskqc/model"
	"maskqc/raster"
	"maskqc/topology"
)

type Engine struct {
	Project *model.Project
	config  Config
	store   *checkpoint.Store

	mu                  chan struct{}
	cache               map[int]*BlockResult
	last                *RunResult
	failHook            func(model.Stroke) error
	pendingInvalidation *InvalidationReport
}

func New(project *model.Project, config Config, store *checkpoint.Store) (*Engine, error) {
	if project == nil {
		return nil, &model.Error{Code: model.CodeSchema, Path: "$.project", Message: "project is nil"}
	}
	normalized, err := config.normalized()
	if err != nil {
		return nil, err
	}
	if project.Width <= 0 || project.Height <= 0 || len(project.CandidateMask) != project.Width*project.Height {
		return nil, &model.Error{
			Code: model.CodeSchema, Path: "$.candidateMask", ObjectID: project.ID,
			Message: "project canvas or candidate mask is inconsistent",
		}
	}
	for _, stroke := range project.Strokes {
		if err := raster.ValidateStroke(stroke); err != nil {
			return nil, err
		}
	}
	engine := &Engine{
		Project: project, config: normalized, store: store,
		mu: make(chan struct{}, 1), cache: map[int]*BlockResult{},
	}
	engine.mu <- struct{}{}
	return engine, nil
}

func (e *Engine) Config() Config {
	return e.config
}

func (e *Engine) Store() *checkpoint.Store {
	return e.store
}

func (e *Engine) SetStrokeFailureHook(hook func(model.Stroke) error) {
	<-e.mu
	defer func() { e.mu <- struct{}{} }()
	e.failHook = hook
}

func (e *Engine) LastResult() *RunResult {
	<-e.mu
	defer func() { e.mu <- struct{}{} }()
	return e.last
}

func (e *Engine) Run(ctx context.Context, mode Mode) (*RunResult, error) {
	<-e.mu
	defer func() { e.mu <- struct{}{} }()
	pendingInvalidation := e.pendingInvalidation
	e.pendingInvalidation = nil
	if mode != ModeIncremental && mode != ModeResume && mode != ModeFull {
		return nil, &model.Error{
			Code: model.CodeParam, Path: "mode", Field: "mode",
			Message: "unknown verification mode", Allowed: "incremental,resume,full",
		}
	}
	started := time.Now()
	result := &RunResult{
		Mode: mode, ProjectID: e.Project.ID, Width: e.Project.Width, Height: e.Project.Height,
		CandidateHash: model.HashBytes(e.Project.CandidateMask),
		Order:         e.Project.StrokeIDsInOrder(), OrderVersion: model.OrderVersion(e.Project.Strokes),
		ConversionResidual: e.config.ResidualDigest,
	}
	stamp := e.stamp()
	restored := (*checkpoint.RestoreResult)(nil)
	if e.store != nil && mode != ModeFull {
		var err error
		restored, err = e.store.Restore(stamp)
		if err != nil {
			return nil, err
		}
		if !restored.Valid {
			result.CheckpointInvalid = restored.Invalidated
			result.CheckpointReason = restored.Reason
		}
	}
	plans, err := e.planBlocks()
	if err != nil {
		return nil, err
	}
	missing := make([]blockJob, 0)
	cachedBlocks := make(map[int]*BlockResult)
	if mode != ModeFull {
		for _, plan := range plans {
			if cached, ok := e.cachedBlock(plan, restored); ok {
				cachedBlocks[plan.job.index] = cached
			} else {
				missing = append(missing, plan.job)
			}
		}
	} else {
		for _, plan := range plans {
			missing = append(missing, plan.job)
		}
	}
	planByIndex := make(map[int]*blockPlan, len(plans))
	for index := range plans {
		planByIndex[plans[index].job.index] = &plans[index]
	}
	computed := runScheduler(ctx, missing, e.config, func(ctx context.Context, job blockJob) (*BlockResult, error) {
		return e.computeBlock(ctx, *planByIndex[job.index])
	})
	allBlocks := make(map[int]*BlockResult, len(plans))
	for index, block := range cachedBlocks {
		allBlocks[index] = block
	}
	for index, block := range computed.blocks {
		allBlocks[index] = block
	}
	result.Budget = computed.budget
	sort.Slice(result.Budget.Events, func(i, j int) bool {
		if result.Budget.Events[i].BlockIndex != result.Budget.Events[j].BlockIndex {
			return result.Budget.Events[i].BlockIndex < result.Budget.Events[j].BlockIndex
		}
		if result.Budget.Events[i].Stage != result.Budget.Events[j].Stage {
			return result.Budget.Events[i].Stage < result.Budget.Events[j].Stage
		}
		return result.Budget.Events[i].WaitMicros < result.Budget.Events[j].WaitMicros
	})
	if len(computed.failures) > 0 {
		for _, stroke := range model.OrderedStrokes(e.Project.Strokes) {
			if failure, failed := computed.failures[stroke.ID]; failed {
				result.StrokeFailures = append(result.StrokeFailures, stroke.ID)
				if e.store != nil {
					_ = e.store.MarkStrokeFailure(stamp, stroke.ID, stroke.Seq, strokeInputHash(e.Project, stroke), failure.Error(), stroke.Seq)
				}
			}
		}
		if e.store != nil {
			if err := e.commitSuccessfulStrokes(stamp, allBlocks, computed.failures); err != nil {
				return result, err
			}
			if err := e.commitBlocks(stamp, allBlocks); err != nil {
				return result, err
			}
		}
		e.finishBlockResults(result, allBlocks, pendingInvalidation)
		first := model.Errors{}
		ids := make([]string, 0, len(computed.failures))
		for id := range computed.failures {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			first = append(first, &model.Error{
				Code: model.CodeSchema, Path: "stroke." + id, ObjectID: id,
				Message: "stroke rasterization failed: " + computed.failures[id].Error(),
			})
		}
		return result, first
	}
	accumulated, err := assembleMask(e.Project.Width, e.Project.Height, allBlocks)
	if err != nil {
		return result, err
	}
	analysis, err := topology.Analyze(accumulated, e.Project.Width, e.Project.Height, e.Project.Strokes)
	if err != nil {
		return result, err
	}
	issues := topology.Compare(analysis, e.Project.Truth, e.config.Tolerance)
	result.Accumulated = accumulated
	result.AccumulatedHash = model.HashBytes(accumulated)
	result.Analysis = analysis
	result.Issues = issues
	e.finishBlockResults(result, allBlocks, pendingInvalidation)
	result.Graph = e.buildGraph(result)
	if e.store != nil {
		if err := e.commitSuccessfulStrokes(stamp, allBlocks, nil); err != nil {
			return result, err
		}
		if err := e.commitBlocks(stamp, allBlocks); err != nil {
			return result, err
		}
	}
	e.cache = make(map[int]*BlockResult, len(allBlocks))
	for index, block := range allBlocks {
		e.cache[index] = block
	}
	e.last = result
	result.ElapsedMillis = time.Since(started).Milliseconds()
	if len(issues) > 0 {
		return result, issues
	}
	return result, nil
}

type blockPlan struct {
	job           blockJob
	rect          model.Rect
	contentHash   string
	orderVersion  string
	cacheKey      string
	participants  []Participant
	componentKeys map[string]string
	strokeIDs     []string
}

func (e *Engine) planBlocks() ([]blockPlan, error) {
	size := e.config.BlockSize
	width, height := e.Project.Width, e.Project.Height
	columns := (width + size - 1) / size
	rows := (height + size - 1) / size
	plans := make([]blockPlan, 0, columns*rows)
	orderedStrokes := model.OrderedStrokes(e.Project.Strokes)
	for blockY := 0; blockY < rows; blockY++ {
		for blockX := 0; blockX < columns; blockX++ {
			rect := model.Rect{
				MinX: blockX * size, MinY: blockY * size,
				MaxX: min(width-1, (blockX+1)*size-1),
				MaxY: min(height-1, (blockY+1)*size-1),
			}
			tile := candidateTile(e.Project.CandidateMask, width, rect)
			contentHash := model.HashStrings(
				"block-input", model.HashBytes(tile),
				fmt.Sprint(rect.MinX), fmt.Sprint(rect.MinY), fmt.Sprint(rect.MaxX), fmt.Sprint(rect.MaxY),
				fmt.Sprint(width), fmt.Sprint(height),
			)
			participants := make([]Participant, 0)
			strokeIDs := make([]string, 0)
			for _, stroke := range orderedStrokes {
				if !stroke.Bounds().Intersects(rect) {
					continue
				}
				strokeIDs = append(strokeIDs, stroke.ID)
				participants = append(participants, Participant{
					StrokeID: stroke.ID, Seq: stroke.Seq, Priority: stroke.Priority,
					CompositeOrder: stroke.CompositeOrder,
					Basis:          fmt.Sprintf("priority=%d,compositeOrder=%d,seq=%d", stroke.Priority, stroke.CompositeOrder, stroke.Seq),
				})
			}
			orderParts := make([]string, 0, len(participants)+2)
			orderParts = append(orderParts, "order", string(model.SchemaV3))
			for _, participant := range participants {
				orderParts = append(orderParts, participant.StrokeID)
			}
			orderVersion := model.HashStrings(orderParts...)
			componentKeys := make(map[string]string, len(participants))
			componentKeyOrder := make([]string, 0, len(participants))
			for index := range participants {
				stroke, _ := e.Project.StrokeByID(participants[index].StrokeID)
				key := model.HashStrings(
					"stroke-component", contentHash, stroke.ID, stroke.RasterSignature(),
					orderVersion, model.ConversionVersion, e.config.ResidualDigest, model.AlgorithmVersion,
				)
				participants[index].ComponentKey = key
				componentKeys[stroke.ID] = key
				componentKeyOrder = append(componentKeyOrder, key)
			}
			cacheParts := []string{
				"block-output", contentHash, orderVersion, model.ConversionVersion,
				model.AlgorithmVersion, fmt.Sprint(e.config.ResidualDigest),
			}
			cacheParts = append(cacheParts, componentKeyOrder...)
			cacheKey := model.HashStrings(cacheParts...)
			index := blockY*columns + blockX
			plans = append(plans, blockPlan{
				job: blockJob{
					index: index, rect: rect, width: rect.Width(), height: rect.Height(),
					area: rect.Width() * rect.Height(),
				},
				rect: rect, contentHash: contentHash, orderVersion: orderVersion,
				cacheKey: cacheKey, participants: participants, componentKeys: componentKeys,
				strokeIDs: strokeIDs,
			})
		}
	}
	return plans, nil
}

func (e *Engine) cachedBlock(plan blockPlan, restored *checkpoint.RestoreResult) (*BlockResult, bool) {
	if block, ok := e.cache[plan.job.index]; ok {
		if block.CacheKey == plan.cacheKey && block.ContentHash == plan.contentHash &&
			block.OrderVersion == plan.orderVersion && sameStrings(block.StrokeIDs, plan.strokeIDs) &&
			block.CompositeDigest == model.HashBytes(block.Mask) {
			copyBlock := *block
			copyBlock.Reused = true
			return &copyBlock, true
		}
	}
	if restored == nil || !restored.Valid {
		return nil, false
	}
	record, ok := restored.Blocks[plan.job.index]
	if !ok || record.CacheKey != plan.cacheKey || record.ContentHash != plan.contentHash ||
		record.OrderVersion != plan.orderVersion || !sameStrings(record.StrokeIDs, plan.strokeIDs) ||
		len(record.Mask) != plan.job.area || record.CompositeDigest != model.HashBytes(record.Mask) {
		return nil, false
	}
	components := make([]BlockComponent, 0, len(plan.participants))
	for _, participant := range plan.participants {
		strokeRecord, ok := restored.Strokes[participant.StrokeID]
		if !ok || strokeRecord.Status != "committed" {
			return nil, false
		}
		found := false
		for _, component := range strokeRecord.Components {
			if component.BlockIndex != plan.job.index {
				continue
			}
			if component.CacheKey != plan.componentKeys[participant.StrokeID] ||
				component.ContentHash != plan.contentHash ||
				component.ComponentDigest != model.HashBytes(component.Coverage) ||
				len(component.Coverage) != plan.job.area {
				return nil, false
			}
			components = append(components, BlockComponent{
				StrokeID: participant.StrokeID, BlockIndex: plan.job.index,
				BlockX:   plan.job.rect.MinX / e.config.BlockSize,
				BlockY:   plan.job.rect.MinY / e.config.BlockSize,
				CacheKey: component.CacheKey, ContentHash: component.ContentHash,
				ComponentDigest: component.ComponentDigest, Coverage: component.Coverage,
			})
			found = true
			break
		}
		if !found {
			return nil, false
		}
	}
	return &BlockResult{
		Index: plan.job.index, BlockX: plan.rect.MinX / e.config.BlockSize,
		BlockY: plan.rect.MinY / e.config.BlockSize, Width: plan.job.width, Height: plan.job.height,
		Rect: plan.rect, ContentHash: record.ContentHash, CacheKey: record.CacheKey,
		OrderVersion: record.OrderVersion, CompositeDigest: record.CompositeDigest,
		Mask: record.Mask, StrokeIDs: record.StrokeIDs, Participants: plan.participants,
		Components: components, Reused: true,
	}, true
}

func (e *Engine) computeBlock(ctx context.Context, plan blockPlan) (*BlockResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	width := e.Project.Width
	tile := candidateTile(e.Project.CandidateMask, width, plan.rect)
	components := make([]BlockComponent, 0, len(plan.participants))
	for _, participant := range plan.participants {
		stroke, ok := e.Project.StrokeByID(participant.StrokeID)
		if !ok {
			return nil, &blockFailure{StrokeID: participant.StrokeID, Err: fmt.Errorf("stroke disappeared during run")}
		}
		if e.failHook != nil {
			if err := e.failHook(stroke); err != nil {
				return nil, &blockFailure{StrokeID: stroke.ID, Err: err}
			}
		}
		coverage, err := raster.CoverageRect(stroke, plan.rect, e.Project.Width, e.Project.Height)
		if err != nil {
			return nil, &blockFailure{StrokeID: stroke.ID, Err: err}
		}
		for index := range tile {
			tile[index] = raster.AlphaOver(tile[index], coverage[index])
		}
		components = append(components, BlockComponent{
			StrokeID: stroke.ID, BlockIndex: plan.job.index,
			BlockX: plan.rect.MinX / e.config.BlockSize, BlockY: plan.rect.MinY / e.config.BlockSize,
			CacheKey: plan.componentKeys[stroke.ID], ContentHash: plan.contentHash,
			ComponentDigest: model.HashBytes(coverage), Coverage: coverage,
		})
	}
	return &BlockResult{
		Index: plan.job.index, BlockX: plan.rect.MinX / e.config.BlockSize,
		BlockY: plan.rect.MinY / e.config.BlockSize, Width: plan.job.width, Height: plan.job.height,
		Rect: plan.rect, ContentHash: plan.contentHash, CacheKey: plan.cacheKey,
		OrderVersion: plan.orderVersion, CompositeDigest: model.HashBytes(tile),
		Mask: tile, StrokeIDs: plan.strokeIDs, Participants: plan.participants,
		Components: components,
	}, nil
}

func (e *Engine) finishBlockResults(result *RunResult, blocks map[int]*BlockResult, invalidation *InvalidationReport) {
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	result.Blocks = make([]BlockResult, 0, len(indices))
	result.CommitSequence = make([]int, 0, len(indices))
	for sequence, index := range indices {
		block := blocks[index]
		block.CommitSequence = sequence + 1
		if block.Reused {
			result.ReusedBlocks = append(result.ReusedBlocks, index)
		}
		result.Blocks = append(result.Blocks, *block)
		result.CommitSequence = append(result.CommitSequence, index)
	}
	if invalidation != nil {
		result.InvalidatedBlocks = append([]int(nil), invalidation.InvalidatedBlocks...)
		result.Invalidation = invalidation
	}
}

func (e *Engine) commitSuccessfulStrokes(stamp checkpoint.Stamp, blocks map[int]*BlockResult, failures map[string]error) error {
	ordered := model.OrderedStrokes(e.Project.Strokes)
	for sequence, stroke := range ordered {
		if _, failed := failures[stroke.ID]; failed {
			continue
		}
		components := make([]checkpoint.ComponentCommit, 0)
		indices := make([]int, 0, len(blocks))
		for index := range blocks {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		componentDigests := make([]string, 0)
		for _, index := range indices {
			for _, component := range blocks[index].Components {
				if component.StrokeID != stroke.ID {
					continue
				}
				components = append(components, checkpoint.ComponentCommit{
					BlockIndex: blocks[index].Index,
					BlockX:     blocks[index].BlockX, BlockY: blocks[index].BlockY,
					ContentHash: component.ContentHash, CacheKey: component.CacheKey,
					ComponentDigest: component.ComponentDigest, Coverage: component.Coverage,
					ResidualDigest: e.config.ResidualDigest, OrderVersion: blocks[index].OrderVersion,
				})
				componentDigests = append(componentDigests, component.ComponentDigest)
			}
		}
		commit := checkpoint.StrokeCommit{
			Stamp: stamp, StrokeID: stroke.ID, Seq: stroke.Seq, Status: "committed",
			InputHash: strokeInputHash(e.Project, stroke), CacheKey: strokeCacheKey(stroke, components),
			ComponentDigest: model.HashStrings(componentDigests...),
			ResidualDigest:  e.config.ResidualDigest, OrderVersion: model.OrderVersion(e.Project.Strokes),
			CommitSequence: sequence + 1, Components: components,
		}
		if err := e.store.CommitStroke(commit); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) commitBlocks(stamp checkpoint.Stamp, blocks map[int]*BlockResult) error {
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		block := blocks[index]
		commit := checkpoint.BlockCommit{
			Stamp: stamp, BlockIndex: block.Index, BlockX: block.BlockX, BlockY: block.BlockY,
			ContentHash: block.ContentHash, CacheKey: block.CacheKey,
			CompositeDigest: block.CompositeDigest, StrokeIDs: block.StrokeIDs,
			Mask: block.Mask, OrderVersion: block.OrderVersion, CommitSequence: index + 1,
		}
		if err := e.store.CommitBlock(commit); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) stamp() checkpoint.Stamp {
	return checkpoint.Stamp{
		ProjectID:         e.Project.ID,
		ContentHash:       canvasHash(e.Project),
		ConversionVersion: model.ConversionVersion,
		AlgorithmVersion:  model.AlgorithmVersion,
		Width:             e.Project.Width, Height: e.Project.Height,
	}
}

func canvasHash(project *model.Project) string {
	return model.HashStrings("canvas", model.HashBytes(project.CandidateMask), fmt.Sprint(project.Width), fmt.Sprint(project.Height))
}

func strokeInputHash(project *model.Project, stroke model.Stroke) string {
	return model.HashStrings("stroke-input", canvasHash(project), stroke.Signature(), model.ConversionVersion)
}

func strokeCacheKey(stroke model.Stroke, components []checkpoint.ComponentCommit) string {
	parts := []string{"stroke-output", stroke.Signature(), model.ConversionVersion, model.AlgorithmVersion}
	for _, component := range components {
		parts = append(parts, component.CacheKey, component.ComponentDigest)
	}
	return model.HashStrings(parts...)
}

func candidateTile(candidate []byte, width int, rect model.Rect) []byte {
	tileWidth := rect.Width()
	tileHeight := rect.Height()
	tile := make([]byte, tileWidth*tileHeight)
	for y := 0; y < tileHeight; y++ {
		copy(tile[y*tileWidth:(y+1)*tileWidth], candidate[(rect.MinY+y)*width+rect.MinX:(rect.MinY+y)*width+rect.MaxX+1])
	}
	return tile
}

func assembleMask(width, height int, blocks map[int]*BlockResult) ([]byte, error) {
	mask := make([]byte, width*height)
	columns := 0
	for _, block := range blocks {
		columns = max(columns, block.BlockX+1)
	}
	if columns == 0 {
		return nil, &model.Error{Code: model.CodeSchema, Path: "blocks", Message: "no block results"}
	}
	for _, block := range blocks {
		if len(block.Mask) != block.Width*block.Height {
			return nil, &model.Error{
				Code: model.CodeSchema, Path: fmt.Sprintf("block[%d].mask", block.Index),
				Message: "block mask length does not match block dimensions",
			}
		}
		for y := 0; y < block.Height; y++ {
			destination := (block.Rect.MinY+y)*width + block.Rect.MinX
			copy(mask[destination:destination+block.Width], block.Mask[y*block.Width:(y+1)*block.Width])
		}
	}
	return mask, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
