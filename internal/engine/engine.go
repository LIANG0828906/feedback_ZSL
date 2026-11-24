package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"

	"cadlab/internal/deps"
	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/render"
	"cadlab/internal/sched"
	"cadlab/internal/store"
)

type Engine struct {
	mu      sync.Mutex
	dataset model.Dataset
	opts    Options
	state   model.SceneState
	exact   *render.Scene
	preview *render.Scene
	cache   map[int]render.TileResult
	store   *store.Store
	graph   deps.Graph
	report  deps.Report

	latestPreview     *render.Frame
	latestExact       *render.Frame
	latestIncremental *render.Frame
	latestResume      *render.Frame
	reference         *render.Frame
	queue             sched.Metrics
	failTile          int
	profileLocks      map[int64]ProfileResult
	lastCompare       *Comparison
	recoveredSnapshot bool
}

type Bootstrap struct {
	Seed          int64            `json:"seed"`
	Generator     string           `json:"generator"`
	Tolerance     float64          `json:"tolerance"`
	AngleTol      float64          `json:"angleToleranceDeg"`
	State         model.SceneState `json:"state"`
	Patches       []PatchView      `json:"patches"`
	Meshes        []MeshView       `json:"meshes"`
	Graph         deps.Graph       `json:"graph"`
	Options       Options          `json:"options"`
	HasCheckpoint bool             `json:"hasCheckpoint"`
	Frame         *FrameView       `json:"frame,omitempty"`
}

type PatchView struct {
	ID       int                  `json:"id"`
	Name     string               `json:"name"`
	UDegree  int                  `json:"uDegree"`
	VDegree  int                  `json:"vDegree"`
	UCount   int                  `json:"uCount"`
	VCount   int                  `json:"vCount"`
	Control  []model.ControlPoint `json:"controlPoints"`
	Trim     []model.Vec2         `json:"trimBoundary"`
	Density  int                  `json:"density"`
	Material model.Material       `json:"material"`
	BoundBox model.BBox           `json:"boundingBox"`
}

type MeshView struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Vertices  int    `json:"vertices"`
	Triangles int    `json:"triangles"`
}

type FrameView struct {
	PNG         string             `json:"pngBase64"`
	Version     int64              `json:"version"`
	Path        string             `json:"path"`
	LOD         int                `json:"lod"`
	Width       int                `json:"width"`
	Height      int                `json:"height"`
	Grid        model.TileGrid     `json:"grid"`
	Camera      model.Camera       `json:"camera"`
	Clip        model.ClipPlane    `json:"clipPlane"`
	DurationNS  int64              `json:"durationNs"`
	CacheHits   int                `json:"cacheHits"`
	Rendered    int                `json:"renderedTiles"`
	Algorithm   string             `json:"algorithmVersion"`
	ContentHash string             `json:"contentHash"`
	Tiles       []TileView         `json:"tiles"`
	Stats       []render.TileStats `json:"tileStats"`
	Queue       sched.Metrics      `json:"queue"`
}

type TileView struct {
	ID         int              `json:"id"`
	X          int              `json:"x"`
	Y          int              `json:"y"`
	W          int              `json:"w"`
	H          int              `json:"h"`
	Stats      render.TileStats `json:"stats"`
	DurationNS int64            `json:"durationNs"`
	Version    int64            `json:"version"`
	Path       string           `json:"path"`
}

func New(dataset model.Dataset, opts Options) (*Engine, error) {
	if d := model.ValidateDataset(dataset); d != nil {
		return nil, d
	}
	if opts.Width <= 0 || opts.Height <= 0 || opts.TileCols <= 0 || opts.TileRows <= 0 {
		return nil, model.Diag(model.CodePara, "invalid viewport or tile grid")
	}
	if _, err := sched.New(sched.Options{
		MaxParallel: opts.MaxParallel, PerTaskPixelBytes: opts.PerTaskPixelBytes,
		PerTaskMeshBytes: opts.PerTaskMeshBytes, TotalPixelBytes: opts.TotalPixelBytes,
		TotalMeshBytes: opts.TotalMeshBytes,
	}); err != nil {
		return nil, model.Diag(model.CodePara, err.Error())
	}
	e := &Engine{
		dataset: dataset, opts: opts, cache: map[int]render.TileResult{},
		failTile: opts.StageFailTile, profileLocks: map[int64]ProfileResult{},
	}
	st, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	e.store = st
	restored := false
	if raw, meta, _, err := st.LatestScene(); err == nil && len(raw) > 0 {
		var saved model.SceneState
		if json.Unmarshal(raw, &saved) == nil && saved.AlgorithmVersion == render.AlgorithmVersion &&
			saved.ContentHash == hashScene(saved) && len(saved.Patches) == len(dataset.Patches) {
			e.state = saved
			restored = true
			e.recoveredSnapshot = true
			_ = meta
		}
	}
	if !restored {
		e.state = initialState(dataset, opts)
	}
	e.state.AlgorithmVersion = render.AlgorithmVersion
	e.state.ContentHash = hashScene(e.state)
	exact, preview, diag := e.buildScenes(e.state, nil, nil, -1, true)
	if diag != nil {
		st.Close()
		return nil, diag
	}
	e.exact, e.preview = exact, preview
	e.graph = deps.Build(e.exact)
	return e, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.Close()
}

func (e *Engine) Bootstrap() Bootstrap {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := Bootstrap{
		Seed: e.dataset.Seed, Generator: e.dataset.Generator,
		Tolerance: e.dataset.Tolerance, AngleTol: e.dataset.AngleTol,
		State: e.state, Graph: e.graph, Options: e.opts,
	}
	for _, p := range e.state.Patches {
		bb := geom.Bounds(e.exactPatchMesh(p.Source.ID))
		b.Patches = append(b.Patches, PatchView{
			ID: p.Source.ID, Name: p.Source.Name, UDegree: p.Source.UDegree, VDegree: p.Source.VDegree,
			UCount: p.Source.UCount, VCount: p.Source.VCount,
			Control: append([]model.ControlPoint(nil), p.Source.Control...),
			Trim:    append([]model.Vec2(nil), p.Source.Trim...), Density: p.Density,
			Material: p.Material, BoundBox: bb,
		})
	}
	for _, m := range e.state.Meshes {
		b.Meshes = append(b.Meshes, MeshView{ID: m.ID, Name: m.Name, Vertices: len(m.Vertices), Triangles: len(m.Indices) / 3})
	}
	if e.latestExact != nil {
		v := e.frameView(e.latestExact, e.queue)
		b.Frame = &v
	}
	if _, _, complete, err := e.store.LatestScene(); err == nil {
		b.HasCheckpoint = complete
	}
	return b
}

func (e *Engine) exactPatchMesh(id int) *geom.SurfaceMesh {
	for _, p := range e.exact.Patches {
		if p.PatchID == id {
			return p.Mesh
		}
	}
	return nil
}

func initialState(ds model.Dataset, opts Options) model.SceneState {
	s := model.SceneState{
		Version: 1, AlgorithmVersion: render.AlgorithmVersion,
		Camera: model.Camera{
			Eye: model.Vec3{X: 0, Y: 8.4, Z: 12.8}, Target: model.Vec3{X: 0, Y: 0.05, Z: 0},
			Up: model.Vec3{Y: 1}, FovY: 42, Near: 0.1, Far: 80,
			Width: opts.Width, Height: opts.Height,
		},
		Clip:  model.ClipPlane{Normal: model.Vec3{Z: 1}, Offset: 20},
		Tiles: model.TileGrid{Cols: opts.TileCols, Rows: opts.TileRows, Width: opts.Width, Height: opts.Height},
		Config: model.RenderConfig{
			PreviewDensityCap: opts.PreviewDensity, CurvatureScale: opts.CurvatureScale,
			NormalTolerance: opts.NormalTolerance, MaxParallel: opts.MaxParallel,
			PerTaskPixelBytes: opts.PerTaskPixelBytes, PerTaskMeshBytes: opts.PerTaskMeshBytes,
			PixelBudget: opts.TotalPixelBytes, MeshBudget: opts.TotalMeshBytes,
		},
		Meshes: append([]model.SubdivMesh(nil), ds.Meshes...),
	}
	for i, p := range ds.Patches {
		s.Patches = append(s.Patches, model.PatchState{
			Source: p, Density: opts.ExactDensity, Material: defaultMaterial(i),
		})
	}
	return s
}

func defaultMaterial(i int) model.Material {
	colors := []model.Color{
		{R: 77, G: 157, B: 191},
		{R: 216, G: 143, B: 68},
		{R: 87, G: 177, B: 143},
	}
	c := colors[i%len(colors)]
	return model.Material{
		BaseColor: c, Ambient: 0.20, Diffuse: 0.78, Specular: 0.34, Shininess: 28,
	}
}

func (e *Engine) buildScenes(state model.SceneState, oldExact, oldPreview *render.Scene, changedPatch int, retess bool) (*render.Scene, *render.Scene, *model.Diagnostic) {
	exact := &render.Scene{
		Version: state.Version, ContentHash: state.ContentHash, AlgorithmVersion: state.AlgorithmVersion,
		Camera: state.Camera, Clip: state.Clip, Tiles: state.Tiles,
		Meshes: meshDraws(state.Meshes),
	}
	preview := &render.Scene{
		Version: state.Version, ContentHash: state.ContentHash, AlgorithmVersion: state.AlgorithmVersion,
		Camera: state.Camera, Clip: state.Clip, Tiles: state.Tiles,
		Meshes: meshDraws(state.Meshes),
	}
	for _, ps := range state.Patches {
		opts := geom.TessOptions{
			Density: ps.Density, CurvatureScale: state.Config.CurvatureScale,
			NormalTolDegrees: state.Config.NormalTolerance, MaxDensity: 128,
		}
		var exactMesh, previewMesh *geom.SurfaceMesh
		reuse := oldExact != nil && changedPatch >= 0 && ps.Source.ID != changedPatch
		if reuse {
			exactMesh = findMesh(oldExact, ps.Source.ID)
			previewMesh = findMesh(oldPreview, ps.Source.ID)
		}
		if exactMesh == nil || !reuse || retess {
			m, d := geom.Tessellate(ps.Source, opts)
			if d != nil {
				return nil, nil, d
			}
			exactMesh = m
		}
		if previewMesh == nil || !reuse || retess {
			popts := opts
			if popts.Density > state.Config.PreviewDensityCap {
				popts.Density = state.Config.PreviewDensityCap
			}
			if popts.Density < geom.MinDensity {
				popts.Density = geom.MinDensity
			}
			m, d := geom.Tessellate(ps.Source, popts)
			if d != nil {
				return nil, nil, d
			}
			previewMesh = m
		}
		exact.Patches = append(exact.Patches, render.PatchDraw{
			PatchID: ps.Source.ID, Source: ps.Source, Mesh: exactMesh, Material: ps.Material,
		})
		preview.Patches = append(preview.Patches, render.PatchDraw{
			PatchID: ps.Source.ID, Source: ps.Source, Mesh: previewMesh, Material: ps.Material,
		})
	}
	exact.LOD = maxMeshDensity(exact)
	preview.LOD = maxMeshDensity(preview)
	return exact, preview, nil
}

func meshDraws(meshes []model.SubdivMesh) []render.MeshDraw {
	out := make([]render.MeshDraw, 0, len(meshes))
	for _, m := range meshes {
		out = append(out, render.MeshDraw{
			Mesh: m,
			Material: model.Material{
				BaseColor: m.Color, Ambient: 0.18, Diffuse: 0.68, Specular: 0.1, Shininess: 12,
			},
		})
	}
	return out
}

func findMesh(scene *render.Scene, id int) *geom.SurfaceMesh {
	if scene == nil {
		return nil
	}
	for _, p := range scene.Patches {
		if p.PatchID == id {
			return p.Mesh
		}
	}
	return nil
}

func maxMeshDensity(scene *render.Scene) int {
	max := 0
	for _, p := range scene.Patches {
		if p.Mesh != nil && p.Mesh.DensityU > max {
			max = p.Mesh.DensityU
		}
	}
	if max == 0 {
		max = geom.MinDensity
	}
	return max
}

func hashScene(s model.SceneState) string {
	copy := s.Clone()
	copy.ContentHash = ""
	copy.Version = 0
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) stateMeta(path string) store.SnapshotMeta {
	return store.SnapshotMeta{
		Version: e.state.Version, ContentHash: e.state.ContentHash,
		AlgorithmVersion: e.state.AlgorithmVersion, Width: e.state.Camera.Width,
		Height: e.state.Camera.Height, TileCols: e.state.Tiles.Cols,
		TileRows: e.state.Tiles.Rows, Path: path,
	}
}

func (e *Engine) frameView(f *render.Frame, q sched.Metrics) FrameView {
	v := FrameView{
		PNG: f.PNGBase64(), Version: f.Version, Path: f.Path, LOD: f.LOD,
		Width: f.Width, Height: f.Height, Grid: f.Grid, Camera: f.Camera, Clip: f.Clip,
		DurationNS: f.Duration.Nanoseconds(), CacheHits: f.CacheHits, Rendered: f.Rendered,
		Algorithm: f.Algorithm, ContentHash: f.ContentHash, Stats: f.Stats, Queue: q,
	}
	for _, t := range f.Tiles {
		v.Tiles = append(v.Tiles, TileView{
			ID: t.ID, X: t.X, Y: t.Y, W: t.W, H: t.H, Stats: t.Stats,
			DurationNS: t.Duration.Nanoseconds(), Version: t.Version, Path: t.Path,
		})
	}
	return v
}

func (e *Engine) SceneState() model.SceneState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Clone()
}

func (e *Engine) defaultMaterialFor(id int) model.Material {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ps, ok := e.state.PatchByID(id); ok {
		return ps.Material
	}
	return defaultMaterial(id)
}

func validateFinite(v model.Vec3) bool { return v.IsFinite() }

func (e *Engine) sceneSizeBytes() int64 {
	var n int64
	for _, p := range e.exact.Patches {
		if p.Mesh != nil {
			n += p.Mesh.EstimatedBytes
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

func (e *Engine) snapshotSummary() map[string]any {
	return map[string]any{
		"version": e.state.Version, "contentHash": e.state.ContentHash,
		"algorithmVersion": e.state.AlgorithmVersion, "camera": e.state.Camera,
		"clipPlane": e.state.Clip, "cachedTiles": len(e.cache),
	}
}

func (e *Engine) sortedCacheTiles() []render.TileResult {
	out := make([]render.TileResult, 0, len(e.cache))
	for _, t := range e.cache {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (e *Engine) ExportState() (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]any{
		"generatedBy":      "cadlab diagnostic workbench",
		"snapshot":         e.snapshotSummary(),
		"scene":            e.state,
		"dependencyGraph":  e.graph,
		"lastInvalidation": e.report,
		"lastQueue":        e.queue,
	}
	if e.lastCompare != nil {
		out["comparison"] = e.lastCompare
	}
	if e.latestExact != nil {
		out["exactFrame"] = e.frameView(e.latestExact, e.queue)
	}
	if e.latestIncremental != nil {
		out["incrementalFrame"] = e.frameView(e.latestIncremental, e.queue)
	}
	if e.latestResume != nil {
		out["resumeFrame"] = e.frameView(e.latestResume, e.queue)
	}
	return out, nil
}

func (e *Engine) WriteExport(path string) error {
	data, err := e.ExportState()
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func round(v float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(v*p) / p
}

func (e *Engine) ensureContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func diagnosticsFromErr(err error) *model.Diagnostic {
	if err == nil {
		return nil
	}
	if d, ok := err.(*model.Diagnostic); ok {
		return d
	}
	return model.Diag(model.CodeStage, err.Error())
}

func (e *Engine) String() string {
	return fmt.Sprintf("cadlab engine v%d %s", e.state.Version, e.state.ContentHash[:min(10, len(e.state.ContentHash))])
}
