package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"cadlab/internal/deps"
	"cadlab/internal/model"
	"cadlab/internal/render"
	"cadlab/internal/sched"
)

type EditOutcome struct {
	Frame        *render.Frame `json:"-"`
	Reference    *render.Frame `json:"-"`
	Invalidation deps.Report   `json:"invalidation"`
	Comparison   *Comparison   `json:"comparison"`
	Queue        sched.Metrics `json:"queue"`
}

type ResumeOutcome struct {
	Frame          *render.Frame     `json:"-"`
	Diagnostic     *model.Diagnostic `json:"diagnostic,omitempty"`
	LoadedTiles    int               `json:"loadedTiles"`
	FullRerendered bool              `json:"fullRerendered"`
}

func (e *Engine) RenderPreview(ctx context.Context) (*render.Frame, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureContext(ctx); err != nil {
		return nil, err
	}
	frame, err := render.RenderFull(ctx, e.preview, render.PathPreview)
	if err != nil {
		return nil, err
	}
	frame.CacheHits = 0
	frame.Rendered = e.state.Tiles.Count()
	e.latestPreview = frame
	return frame, nil
}

func (e *Engine) RenderExact(ctx context.Context) (*render.Frame, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.renderExactLocked(ctx)
}

func (e *Engine) renderExactLocked(ctx context.Context) (*render.Frame, error) {
	if err := e.ensureContext(ctx); err != nil {
		return nil, err
	}
	start := time.Now()
	tiles, metrics, err := e.renderTileSet(ctx, e.exact, allTileIDs(e.state.Tiles.Count()), render.PathExact, nil)
	if err != nil {
		return nil, err
	}
	frame := render.NewFrame(e.exact, render.PathExact, time.Since(start), mapTiles(tiles))
	frame.Rendered = len(tiles)
	e.queue = metrics
	e.cache = tiles
	e.latestExact = frame
	e.reference = frame
	if e.latestIncremental != nil && e.latestIncremental.Version != frame.Version {
		e.latestIncremental = nil
	}
	if e.latestResume != nil && e.latestResume.Version != frame.Version {
		e.latestResume = nil
	}
	if err := e.persistFrameLocked(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (e *Engine) ApplyControlPoint(ctx context.Context, patchID, row, col int, p model.Vec3) (*EditOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyEditLocked(ctx, deps.Edit{
		Kind: deps.EditControl, PatchID: patchID, Row: row, Col: col, Position: p,
	})
}

func (e *Engine) ApplyMaterial(ctx context.Context, patchID int, m model.Material) (*EditOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyEditLocked(ctx, deps.Edit{Kind: deps.EditMaterial, PatchID: patchID, Material: m})
}

func (e *Engine) ApplyDensity(ctx context.Context, patchID, density int) (*EditOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyEditLocked(ctx, deps.Edit{Kind: deps.EditDensity, PatchID: patchID, Density: density})
}

func (e *Engine) applyEditLocked(ctx context.Context, edit deps.Edit) (*EditOutcome, error) {
	if err := e.ensureContext(ctx); err != nil {
		return nil, err
	}
	if len(e.cache) != e.state.Tiles.Count() {
		return nil, model.Diag(model.CodeStage, "exact cache is not initialized; render an exact frame first")
	}
	next := e.state.Clone()
	ps, ok := next.PatchByID(edit.PatchID)
	if !ok {
		return nil, model.DiagPatch(model.CodePara, edit.PatchID, "patchId", "surface patch does not exist")
	}
	retess := false
	switch edit.Kind {
	case deps.EditControl:
		if edit.Row < 0 || edit.Row >= ps.Source.VCount || edit.Col < 0 || edit.Col >= ps.Source.UCount {
			return nil, model.DiagPatch(model.CodePara, edit.PatchID, "controlPoint", "control point index out of range")
		}
		if !validateFinite(edit.Position) {
			return nil, model.DiagPatch(model.CodePara, edit.PatchID, "position", "control point coordinate must be finite")
		}
		idx := edit.Row*ps.Source.UCount + edit.Col
		ps.Source.Control[idx].Position = edit.Position
		retess = true
	case deps.EditMaterial:
		if err := validateMaterial(edit.Material); err != nil {
			err.PatchID = edit.PatchID
			return nil, err
		}
		ps.Material = edit.Material
	case deps.EditDensity:
		if edit.Density < 2 || edit.Density > 128 {
			return nil, model.DiagPatch(model.CodePara, edit.PatchID, "density", "density must be within [2,128]")
		}
		ps.Density = edit.Density
		retess = true
	default:
		return nil, model.Diag(model.CodePara, "unknown edit kind")
	}
	next.Version++
	exact, preview, diag := e.buildScenes(next, e.exact, e.preview, edit.PatchID, retess)
	if diag != nil {
		return nil, diag
	}
	next.ContentHash = hashScene(next)
	exact.ContentHash, preview.ContentHash = next.ContentHash, next.ContentHash
	report := deps.Compute(e.exact, exact, edit)
	start := time.Now()
	newTiles := map[int]render.TileResult{}
	for id, t := range e.cache {
		newTiles[id] = t
	}
	invalidSet := map[int]bool{}
	for _, id := range report.InvalidatedTiles {
		invalidSet[id] = true
	}
	rendered, metrics, err := e.renderTileSet(ctx, exact, report.InvalidatedTiles, render.PathIncremental, nil)
	if err != nil {
		return nil, err
	}
	for id, t := range rendered {
		newTiles[id] = t
	}
	for id, t := range newTiles {
		t.Version = next.Version
		t.Path = render.PathIncremental
		newTiles[id] = t
	}
	incFrame := render.NewFrame(exact, render.PathIncremental, time.Since(start), mapTiles(newTiles))
	incFrame.CacheHits = len(newTiles) - len(rendered)
	incFrame.Rendered = len(rendered)

	refStart := time.Now()
	refTiles, refMetrics, err := e.renderTileSet(ctx, exact, allTileIDs(next.Tiles.Count()), render.PathExact, nil)
	if err != nil {
		return nil, err
	}
	refFrame := render.NewFrame(exact, render.PathExact, time.Since(refStart), mapTiles(refTiles))
	refFrame.Rendered = len(refTiles)
	refMetrics.Elapsed = metrics.Elapsed + refMetrics.Elapsed

	cmp := e.compareIncrementalToReference(incFrame, refFrame, invalidSet)
	if cmp.FullVsIncremental.Diagnostic != nil {
		return nil, cmp.FullVsIncremental.Diagnostic
	}
	e.state = next
	e.exact, e.preview = exact, preview
	e.cache = newTiles
	e.latestIncremental = incFrame
	e.reference = refFrame
	e.latestExact = refFrame
	e.latestResume = nil
	e.report = report
	e.graph = report.Graph
	e.queue = mergeMetrics(metrics, refMetrics)
	e.lastCompare = cmp
	if err := e.persistFrameLocked(incFrame); err != nil {
		return nil, err
	}
	return &EditOutcome{
		Frame: incFrame, Reference: refFrame, Invalidation: report,
		Comparison: cmp, Queue: e.queue,
	}, nil
}

func validateMaterial(m model.Material) *model.Diagnostic {
	if m.Ambient < 0 || m.Ambient > 1 || m.Diffuse < 0 || m.Diffuse > 1 ||
		m.Specular < 0 || m.Specular > 1 || m.Shininess <= 0 || m.Shininess > 512 {
		return model.Diag(model.CodePara, "material parameters out of range")
	}
	return nil
}

func (e *Engine) renderTileSet(ctx context.Context, scene *render.Scene, ids []int, path string, _ map[int]render.TileResult) (map[int]render.TileResult, sched.Metrics, error) {
	ids = model.SortTiles(ids)
	if len(ids) == 0 {
		return map[int]render.TileResult{}, sched.Metrics{}, nil
	}
	meshBytes := sceneMeshBytes(scene)
	pixelLimit := e.opts.PerTaskPixelBytes
	meshLimit := e.opts.PerTaskMeshBytes
	sch, err := sched.New(sched.Options{
		MaxParallel: e.opts.MaxParallel, PerTaskPixelBytes: pixelLimit,
		PerTaskMeshBytes: meshLimit, TotalPixelBytes: e.opts.TotalPixelBytes,
		TotalMeshBytes: e.opts.TotalMeshBytes,
	})
	if err != nil {
		return nil, sched.Metrics{}, model.Diag(model.CodePara, err.Error())
	}
	tasks := make([]sched.Task, 0, len(ids))
	for _, id := range ids {
		tileID := id
		w, h := scene.Tiles.TileSize(tileID)
		tasks = append(tasks, sched.Task{
			ID: tileID, Label: "exact-tile", PixelBytes: int64(w*h*8 + 256), MeshBytes: meshBytes,
			Run: func(runCtx context.Context) (any, error) {
				t, err := render.RenderTile(scene, tileID, render.RenderOptions{Context: runCtx, FailTile: e.failTile})
				if err != nil {
					return nil, err
				}
				t.Path = path
				return t, nil
			},
		})
	}
	results, metrics, runErr := sch.Run(ctx, tasks)
	out := map[int]render.TileResult{}
	for _, r := range results {
		if r.Err == nil {
			if t, ok := r.Value.(render.TileResult); ok {
				out[t.ID] = t
			}
		}
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return out, metrics, &model.Diagnostic{
				Code: model.CodeRecover, PatchID: -1, TileID: -1, Field: "render.cancel",
				Message: "render cancelled; only committed checkpoints are resumable",
			}
		}
		if d := diagnosticsFromErr(runErr); d != nil {
			return out, metrics, d
		}
		return out, metrics, runErr
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return out, metrics, model.DiagTile(model.CodeStage, -1, id, "tile", "renderer produced no tile")
		}
	}
	return out, metrics, nil
}

func sceneMeshBytes(scene *render.Scene) int64 {
	var n int64
	for _, p := range scene.Patches {
		if p.Mesh != nil {
			n += p.Mesh.EstimatedBytes
		}
	}
	for _, m := range scene.Meshes {
		n += int64(len(m.Mesh.Vertices)*9*8 + len(m.Mesh.Indices)*4)
	}
	if n < 1 {
		n = 1
	}
	return n
}

func allTileIDs(count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = i
	}
	return out
}

func mapTiles(m map[int]render.TileResult) []render.TileResult {
	out := make([]render.TileResult, 0, len(m))
	for _, t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mergeMetrics(a, b sched.Metrics) sched.Metrics {
	out := a
	out.Elapsed += b.Elapsed
	out.Completed += b.Completed
	out.Cancelled += b.Cancelled
	if b.PeakParallel > out.PeakParallel {
		out.PeakParallel = b.PeakParallel
	}
	if b.PeakPixelBytes > out.PeakPixelBytes {
		out.PeakPixelBytes = b.PeakPixelBytes
	}
	if b.PeakMeshBytes > out.PeakMeshBytes {
		out.PeakMeshBytes = b.PeakMeshBytes
	}
	out.Queue = append(append([]sched.QueueRecord(nil), a.Queue...), b.Queue...)
	sort.Slice(out.Queue, func(i, j int) bool { return out.Queue[i].TaskID < out.Queue[j].TaskID })
	return out
}

func (e *Engine) persistFrameLocked(frame *render.Frame) error {
	if e.store == nil {
		return nil
	}
	meta := e.stateMeta(frame.Path)
	sceneJSON, err := json.Marshal(e.state)
	if err != nil {
		return model.Diag(model.CodeRecover, err.Error())
	}
	if err := e.store.Begin(meta, sceneJSON); err != nil {
		return model.Diag(model.CodeRecover, err.Error())
	}
	tiles := append([]render.TileResult(nil), frame.Tiles...)
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].ID < tiles[j].ID })
	for ordinal, t := range tiles {
		if err := e.store.CommitTile(meta, t, ordinal); err != nil {
			return model.DiagTile(model.CodeRecover, -1, t.ID, "checkpoint", err.Error())
		}
	}
	if err := e.store.Finalize(meta); err != nil {
		return model.Diag(model.CodeRecover, err.Error())
	}
	return nil
}

func (e *Engine) Resume(ctx context.Context) (*ResumeOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureContext(ctx); err != nil {
		return nil, err
	}
	meta := e.stateMeta(render.PathResume)
	required := allTileIDs(e.state.Tiles.Count())
	result, diag := e.store.Load(meta, required)
	if diag != nil {
		if diag.Code == model.CodeSnap {
			frame, err := e.renderExactLocked(ctx)
			if err != nil {
				return nil, err
			}
			return &ResumeOutcome{
				Frame: frame, Diagnostic: diag, FullRerendered: true,
				LoadedTiles: 0,
			}, nil
		}
		return &ResumeOutcome{Diagnostic: diag}, nil
	}
	for _, t := range result.Tiles {
		if t.Version != e.state.Version {
			d := &model.Diagnostic{
				Code: model.CodeSnap, Message: "tile version differs from current snapshot",
				PatchID: -1, TileID: t.ID, Field: "tile.version",
				VersionA: t.Version, VersionB: e.state.Version, ConflictingTiles: []int{t.ID},
			}
			return &ResumeOutcome{Diagnostic: d}, nil
		}
	}
	frame := render.NewFrame(e.exact, render.PathResume, 0, result.Tiles)
	frame.CacheHits = len(result.Tiles)
	frame.Rendered = 0
	e.cache = map[int]render.TileResult{}
	for _, t := range result.Tiles {
		t.Path = render.PathResume
		e.cache[t.ID] = t
	}
	e.latestResume = frame
	return &ResumeOutcome{Frame: frame, LoadedTiles: len(result.Tiles)}, nil
}

func (e *Engine) LatestPreview() *render.Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latestPreview
}

func (e *Engine) LatestExact() *render.Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latestExact
}

func (e *Engine) LatestIncremental() *render.Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latestIncremental
}

func (e *Engine) LatestResume() *render.Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.latestResume
}

func (e *Engine) LastInvalidation() deps.Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.report
}

func (e *Engine) FrameView(f *render.Frame) FrameView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.frameView(f, e.queue)
}
