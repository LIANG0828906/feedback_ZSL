package maskqc

import (
	"context"
	"fmt"
	"image"
	"sort"
)

// ChunkCacheRecord is the persistent cache representation of one tile.
type ChunkCacheRecord struct {
	ProjectID    string     `json:"project_id"`
	Coord        ChunkCoord `json:"coord"`
	CacheKey     string     `json:"cache_key"`
	InputHash    string     `json:"input_hash"`
	OutputHash   string     `json:"output_hash"`
	OrderVersion string     `json:"order_version"`
	ParamsKey    string     `json:"params_key"`
	UpstreamKeys []string   `json:"upstream_keys"`
	Alpha        []byte     `json:"alpha"`
}

// PersistentCache combines atomic stroke checkpoints and tile cache access.
type PersistentCache interface {
	CheckpointSink
	PutChunkCache(ctx context.Context, record ChunkCacheRecord) error
	GetChunkCache(ctx context.Context, cacheKey string) (*ChunkCacheRecord, error)
}

// IncrementalResult contains the rebuilt result and precise invalidation list.
type IncrementalResult struct {
	Pipeline       *PipelineResult     `json:"pipeline"`
	Invalidation   *InvalidationReport `json:"invalidation"`
	PersistentHits []ChunkCoord        `json:"persistent_hits"`
}

// Engine keeps the last deterministic plan for real-time incremental edits.
type Engine struct {
	Params Params
	Cache  PersistentCache

	Project      *Project
	Result       *PipelineResult
	chunkByCoord map[ChunkCoord]ChunkResult
}

func NewEngine(p *Project, params Params, cache PersistentCache) *Engine {
	params = params.normalized()
	return &Engine{Params: params, Cache: cache, Project: p, chunkByCoord: map[ChunkCoord]ChunkResult{}}
}

// FullRebuild computes every tile and warms the persistent cache.
func (e *Engine) FullRebuild(ctx context.Context) (*PipelineResult, error) {
	result, err := RunPipeline(ctx, e.Project, e.Params, "realtime-full", e.Cache)
	if err != nil {
		if pe, ok := AsError(err); ok && pe.Code == CodeTop {
			// Topology mismatch is a diagnostic result, not an engine failure.
		} else {
			return nil, err
		}
	}
	e.Result = result
	e.chunkByCoord = map[ChunkCoord]ChunkResult{}
	for _, c := range result.Chunks {
		e.chunkByCoord[c.Coord] = c
		if e.Cache != nil {
			rec := ChunkCacheRecord{
				ProjectID: e.Project.ID, Coord: c.Coord, CacheKey: c.CacheKey,
				InputHash: c.InputHash, OutputHash: c.OutputHash,
				OrderVersion: c.OrderKey, ParamsKey: c.ParamsKey,
				UpstreamKeys: c.UpstreamKeys, Alpha: c.Alpha,
			}
			if perr := e.Cache.PutChunkCache(ctx, rec); perr != nil {
				return result, perr
			}
		}
	}
	return result, err
}

// ApplyEdit invalidates only geometry/dependency-affected chunks and topo checks.
func (e *Engine) ApplyEdit(ctx context.Context, edit Edit) (*IncrementalResult, error) {
	if e.Project == nil || e.Result == nil {
		return nil, &Error{Code: CodeState, Field: "engine", Message: "call FullRebuild before ApplyEdit"}
	}
	idx := -1
	for i := range e.Project.Strokes {
		if e.Project.Strokes[i].Seq == edit.StrokeSeq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, schemaError("", fmt.Sprintf("/strokes?seq=%d", edit.StrokeSeq), "stroke_seq", "edited stroke not found")
	}

	oldStroke := e.Project.Strokes[idx]
	oldBounds := StrokeBounds(oldStroke, e.Project.Width, e.Project.Height)
	fields, applyErr := applyEditToStroke(&e.Project.Strokes[idx], edit)
	if applyErr != nil {
		return nil, applyErr
	}
	newStroke := e.Project.Strokes[idx]
	if verr := ValidateStrokeParameters(newStroke); verr != nil {
		return nil, verr
	}
	newBounds := StrokeBounds(newStroke, e.Project.Width, e.Project.Height)
	union := unionRect(oldBounds, newBounds)
	affected := ChunksForRect(union, e.Project.Width, e.Project.Height, e.Params.ChunkSize)
	affectedSet := map[ChunkCoord]bool{}
	for _, c := range affected {
		affectedSet[c] = true
	}

	allCoords := ChunkGrid(e.Project.Width, e.Project.Height, e.Params.ChunkSize)
	reused := []ChunkCoord{}
	persistentHits := []ChunkCoord{}
	for _, c := range allCoords {
		if affectedSet[c] {
			continue
		}
		reused = append(reused, c)
		if e.Cache != nil {
			old, ok := e.chunkByCoord[c]
			if ok {
				rec, err := e.Cache.GetChunkCache(ctx, old.CacheKey)
				if err != nil {
					return nil, err
				}
				if rec != nil {
					persistentHits = append(persistentHits, c)
					cached := old
					cached.Alpha = append([]byte(nil), rec.Alpha...)
					e.chunkByCoord[c] = cached
				}
			}
		}
	}

	oldTopology := e.Result.Topology
	invalidTopo := []int{}
	for _, d := range oldTopology {
		for _, c := range affected {
			cr := ChunkRect(c, e.Project.Width, e.Project.Height, e.Params.ChunkSize)
			if !cr.Intersect(d.Bounds).Empty() {
				invalidTopo = append(invalidTopo, d.DomainID)
				break
			}
		}
	}

	ordered, orderTrail := AdjudicateOrder(e.Project.Strokes)
	orderVersion := CompositionOrderVersion(ordered)
	paramsKey := e.Params.ParamsKey()
	rootHash := ProjectContentHash(e.Project)
	compByChunk := map[string]ComponentRecord{}
	for _, rec := range e.Result.StrokeCommits {
		compByChunk[strokeChunkKey(rec.StrokeSeq, rec.Chunk)] = rec
	}
	for _, c := range affected {
		delete(compByChunk, strokeChunkKey(edit.StrokeSeq, c))
	}

	jobs := make([]Job, 0, len(affected))
	newComponents := make([][]ComponentRecord, len(affected))
	for ji, coord := range affected {
		ji, coord := ji, coord
		region := ChunkRect(coord, e.Project.Width, e.Project.Height, e.Params.ChunkSize).Intersect(newBounds)
		if region.Empty() {
			continue
		}
		pixels := region.Width() * region.Height()
		jobs = append(jobs, Job{
			ID: fmt.Sprintf("incremental-raster/%d/%s", edit.StrokeSeq, coord), Chunk: coord, Pixels: pixels,
			Run: func(buffer []byte) error {
				target := buffer[:pixels]
				if err := RasterizeStrokeInto(newStroke, region, target); err != nil {
					return err
				}
				alpha := append([]byte(nil), target...)
				contentHash := hashBytes([]byte(fmt.Sprintf("component:%s:%d:%s", AlgorithmVersion, newStroke.Seq, coord.String())), alpha)
				_, residual, _ := ConvertScale(newStroke.WeightPermille, WeightMaxInternal, WeightMaxV2, fmt.Sprintf("stroke/%d/weight", newStroke.Seq))
				digest := ResidualDigest(newStroke.Seq, "weight->v2", residual)
				key := hashJSON("component-cache-key-v1", map[string]any{
					"project": e.Project.ID, "stroke": newStroke.Seq, "chunk": coord,
					"bounds": region, "content": contentHash,
					"params": paramsKey, "residual": digest, "algorithm": AlgorithmVersion,
				})
				newComponents[ji] = []ComponentRecord{{
					ProjectID: e.Project.ID, OrderIndex: strokeOrderIndex(ordered, newStroke.Seq),
					StrokeSeq: newStroke.Seq, Chunk: coord, Bounds: region, Alpha: alpha,
					CacheKey: key, ContentHash: contentHash, ResidualDigest: digest,
					OrderVersion: OrderRulesVersion, AlgorithmVersion: AlgorithmVersion,
				}}
				return nil
			},
		})
	}
	rasterResults := RunJobs(ctx, jobs, e.Params.Scheduler)
	if jerr := AllJobsFailed(rasterResults); jerr != nil {
		return nil, jerr
	}
	editedRecords := []ComponentRecord{}
	for _, group := range newComponents {
		for _, rec := range group {
			compByChunk[strokeChunkKey(rec.StrokeSeq, rec.Chunk)] = rec
			editedRecords = append(editedRecords, rec)
		}
	}
	if e.Cache != nil && len(editedRecords) > 0 {
		cp := StrokeCheckpoint{
			ProjectID: e.Project.ID, SchemaVersion: e.Project.SourceVersion, OrderIndex: strokeOrderIndex(ordered, edit.StrokeSeq),
			StrokeSeq: edit.StrokeSeq, RootContentHash: rootHash,
			OrderVersion: OrderRulesVersion, ParamsVersion: paramsKey, Components: editedRecords,
		}
		if _, err := e.Cache.CommitStrokeCheckpoint(ctx, cp); err != nil {
			return nil, err
		}
	}

	composeJobs := make([]Job, 0, len(affected))
	composed := make([]ChunkResult, len(affected))
	for ai, coord := range affected {
		ai, coord := ai, coord
		rect := ChunkRect(coord, e.Project.Width, e.Project.Height, e.Params.ChunkSize)
		pixels := rect.Width() * rect.Height()
		composeJobs = append(composeJobs, Job{
			ID: fmt.Sprintf("incremental-compose/%s", coord), Chunk: coord, Pixels: pixels,
			Run: func(buffer []byte) error {
				alpha := buffer[:pixels]
				for y := rect.MinY; y < rect.MaxY; y++ {
					srcStart := y*e.Project.Width + rect.MinX
					dstStart := (y - rect.MinY) * rect.Width()
					copy(alpha[dstStart:dstStart+rect.Width()], e.Project.Candidate.Pix[srcStart:srcStart+rect.Width()])
				}
				seqs := []int{}
				upstream := []string{}
				for _, stroke := range ordered {
					rec, ok := compByChunk[strokeChunkKey(stroke.Seq, coord)]
					if !ok {
						continue
					}
					seqs = append(seqs, stroke.Seq)
					upstream = append(upstream, rec.CacheKey)
					ComposeRegionOver(alpha, rect, rec.Bounds, rec.Alpha)
				}
				inputHash := hashBytes(e.Project.Candidate.Pix[rect.MinY*e.Project.Width+rect.MinX : rect.MinY*e.Project.Width+rect.MinX+pixels])
				localOrderKey := hashJSON("chunk-order-v1", map[string]any{"rules": OrderRulesVersion, "seqs": seqs})
				cacheKey := hashJSON("chunk-cache-key-v1", map[string]any{
					"input": inputHash, "chunk": coord, "seqs": seqs,
					"order_version": OrderRulesVersion, "order_key": localOrderKey,
					"params": paramsKey, "upstream": upstream,
				})
				cr := ChunkResult{
					Coord: coord, Bounds: rect, Alpha: append([]byte(nil), alpha...),
					InputHash: inputHash, OrderKey: localOrderKey, ParamsKey: paramsKey,
					UpstreamKeys: upstream, CacheKey: cacheKey,
					OutputHash: hashBytes([]byte("chunk-output"), alpha),
				}
				composed[ai] = cr
				return nil
			},
		})
	}
	composeResults := RunJobs(ctx, composeJobs, e.Params.Scheduler)
	if jerr := AllJobsFailed(composeResults); jerr != nil {
		return nil, jerr
	}

	waitRecords := []WaitRecord{}
	for _, jr := range append(append([]JobResult{}, rasterResults...), composeResults...) {
		waitRecords = append(waitRecords, jr.Waits...)
	}

	invalidItems := []InvalidationItem{}
	for _, coord := range affected {
		invalidItems = append(invalidItems, InvalidationItem{
			Chunk: coord, Reason: string(edit.Kind) + " changes stroke geometry/parameters/order dependency",
			StrokeSeqs: []int{edit.StrokeSeq}, Fields: fields,
		})
	}
	report := &InvalidationReport{
		InvalidatedChunks: invalidItems, ReusedChunks: reused,
		InvalidatedTopo: invalidTopo, WaitRecords: waitRecords,
	}

	for _, cr := range composed {
		e.chunkByCoord[cr.Coord] = cr
		if e.Cache != nil {
			rec := ChunkCacheRecord{
				ProjectID: e.Project.ID, Coord: cr.Coord, CacheKey: cr.CacheKey,
				InputHash: cr.InputHash, OutputHash: cr.OutputHash,
				OrderVersion: cr.OrderKey, ParamsKey: cr.ParamsKey,
				UpstreamKeys: cr.UpstreamKeys, Alpha: cr.Alpha,
			}
			if err := e.Cache.PutChunkCache(ctx, rec); err != nil {
				return nil, err
			}
		}
	}

	chunks := make([]ChunkResult, 0, len(allCoords))
	for _, coord := range allCoords {
		chunks = append(chunks, e.chunkByCoord[coord])
	}
	mask := image.NewGray(image.Rect(0, 0, e.Project.Width, e.Project.Height))
	for _, cr := range chunks {
		rect := cr.Bounds
		for y := rect.MinY; y < rect.MaxY; y++ {
			dst := y*e.Project.Width + rect.MinX
			src := (y - rect.MinY) * rect.Width()
			copy(mask.Pix[dst:dst+rect.Width()], cr.Alpha[src:src+rect.Width()])
		}
	}
	topo, terr := AnalyzeGrayMask(mask, e.Params.AlphaThreshold)
	var topoErr *Error
	if e.Project.Truth != nil {
		topoErr = VerifyTopology(topo, e.Project.Truth.Domains, e.Params.Tolerance)
		if topoErr != nil && terr == nil {
			terr = topoErr
		}
	}
	graph := BuildGraph(e.Project, allCoords, ordered, chunks, topo)
	e.Result.Mask = mask
	e.Result.Path = "realtime-incremental"
	e.Result.Chunks = chunks
	e.Result.OrderedStrokes = ordered
	e.Result.OrderVersion = orderVersion
	e.Result.OrderTrail = orderTrail
	updatedCommits := make([]ComponentRecord, 0, len(e.Result.StrokeCommits)+len(editedRecords))
	for _, rec := range e.Result.StrokeCommits {
		if rec.StrokeSeq != edit.StrokeSeq {
			rec.OrderIndex = strokeOrderIndex(ordered, rec.StrokeSeq)
			updatedCommits = append(updatedCommits, rec)
		}
	}
	updatedCommits = append(updatedCommits, editedRecords...)
	sort.SliceStable(updatedCommits, func(i, j int) bool {
		if updatedCommits[i].OrderIndex != updatedCommits[j].OrderIndex {
			return updatedCommits[i].OrderIndex < updatedCommits[j].OrderIndex
		}
		if updatedCommits[i].Chunk.Y != updatedCommits[j].Chunk.Y {
			return updatedCommits[i].Chunk.Y < updatedCommits[j].Chunk.Y
		}
		return updatedCommits[i].Chunk.X < updatedCommits[j].Chunk.X
	})
	e.Result.StrokeCommits = updatedCommits
	e.Result.Topology = topo
	e.Result.TopologyError = topoErr
	e.Result.Graph = graph
	e.Result.Invalidation = report
	e.Result.WaitRecords = waitRecords
	e.Result.RootHash = rootHash
	e.Result.Identity = SnapshotIdentity{ProjectID: e.Project.ID, RootContentHash: rootHash, OrderVersion: OrderRulesVersion, ParamsVersion: paramsKey, AlgorithmVersion: AlgorithmVersion, SchemaVersion: e.Project.SourceVersion}

	return &IncrementalResult{Pipeline: e.Result, Invalidation: report, PersistentHits: persistentHits}, terr
}

func applyEditToStroke(s *Stroke, edit Edit) ([]string, *Error) {
	switch edit.Kind {
	case EditMoveStart:
		s.Start = edit.Point
		return []string{"start.x", "start.y"}, nil
	case EditMoveEnd:
		s.End = edit.Point
		return []string{"end.x", "end.y"}, nil
	case EditWeight:
		s.WeightPermille = edit.Value
		return []string{"weight_permille"}, nil
	case EditPriority:
		s.OrderPriority = edit.Value
		return []string{"order_priority"}, nil
	case EditHardness:
		s.HardnessPermille = edit.Value
		return []string{"hardness_permille"}, nil
	case EditRadius:
		s.RadiusTick = edit.Value
		return []string{"radius_tick"}, nil
	default:
		return nil, &Error{Code: CodePara, Field: "kind", Message: "unsupported edit kind"}
	}
}

func unionRect(a, b Rect) Rect {
	if a.Empty() {
		return b
	}
	if b.Empty() {
		return a
	}
	return Rect{
		MinX: minInt(a.MinX, b.MinX), MinY: minInt(a.MinY, b.MinY),
		MaxX: maxInt(a.MaxX, b.MaxX), MaxY: maxInt(a.MaxY, b.MaxY),
	}
}

func strokeOrderIndex(strokes []Stroke, seq int) int {
	for i, s := range strokes {
		if s.Seq == seq {
			return i
		}
	}
	return -1
}

// CloneForEdit copies construction data while sharing read-only image/truth.
func CloneForEdit(p *Project) *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Strokes = append([]Stroke(nil), p.Strokes...)
	cp.ChannelCoverage = append([]int(nil), p.ChannelCoverage...)
	cp.Trail = append([]TransformRecord(nil), p.Trail...)
	return &cp
}

// ApplyEdits derives the final project used by the full recompute path.
func ApplyEdits(p *Project, edits []Edit) (*Project, error) {
	out := CloneForEdit(p)
	for _, edit := range edits {
		idx := -1
		for i := range out.Strokes {
			if out.Strokes[i].Seq == edit.StrokeSeq {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, schemaError("", fmt.Sprintf("/strokes?seq=%d", edit.StrokeSeq), "stroke_seq", "edited stroke not found")
		}
		if _, err := applyEditToStroke(&out.Strokes[idx], edit); err != nil {
			return nil, err
		}
		if err := ValidateStrokeParameters(out.Strokes[idx]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
