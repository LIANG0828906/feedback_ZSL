package fog

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Workbench struct {
	mu          sync.Mutex
	dbPath      string
	store       *CheckpointStore
	sample      *GeneratedSample
	graph       *DependencyGraph
	scheduler   *Scheduler
	tiles       map[string]TileResult
	statuses    map[string]string
	lastQueue   []QueueRecord
	resource    ResourceSnapshot
	invalidated *InvalidationReport
	threeWay    *ThreeWaySummary
	preview     *PreviewSummary
	anomaly     *Error
	info        string
}

type TileDiagnostic struct {
	Tile              TileCoord `json:"tile"`
	Status            string    `json:"status"`
	Transmittance     float64   `json:"transmittance"`
	Scattering        float64   `json:"scattering"`
	AverageScattering float64   `json:"average_scattering"`
	CacheKey          string    `json:"cache_key"`
	ContentHash       string    `json:"content_hash"`
	ShadowSamples     int       `json:"shadow_samples"`
	RayBufferUsed     int       `json:"ray_buffer_used"`
	CommitOrder       int       `json:"commit_order"`
	AverageColor      [3]uint8  `json:"average_color"`
	PixelCount        int       `json:"pixel_count"`
}

type GraphSummary struct {
	NodeCount    int `json:"node_count"`
	EdgeCount    int `json:"edge_count"`
	VoxelNodes   int `json:"voxel_nodes"`
	SegmentNodes int `json:"segment_nodes"`
	TileNodes    int `json:"tile_nodes"`
}

type ThreeWaySummary struct {
	Equal            bool              `json:"equal"`
	FirstDiff        *Error            `json:"first_diff,omitempty"`
	Sequence         []TileCoord       `json:"sequence"`
	ResumeReused     []TileCoord       `json:"resume_reused"`
	ResumeRecomputed []TileCoord       `json:"resume_recomputed"`
	Stale            []StaleCheckpoint `json:"stale"`
}

type PreviewSummary struct {
	StepBudget      int              `json:"step_budget_per_ray"`
	ShadowBudget    int              `json:"shadow_budget_per_ray"`
	RayCount        int              `json:"ray_count"`
	ActualSteps     int              `json:"actual_steps_total"`
	ActualShadow    int              `json:"actual_shadow_total"`
	AverageSteps    float64          `json:"average_steps_per_ray"`
	AverageShadow   float64          `json:"average_shadow_per_ray"`
	RecipeVersion   int64            `json:"recipe_version"`
	CameraVersion   int64            `json:"camera_version"`
	MediumVersions  map[string]int64 `json:"medium_versions"`
	ElapsedMS       int64            `json:"elapsed_ms"`
	DelayLimitMS    int64            `json:"delay_limit_ms"`
	WithinDelay     bool             `json:"within_delay"`
	MaxPixelDelta   float64          `json:"max_pixel_delta"`
	WithinTolerance bool             `json:"within_tolerance"`
	Judgement       string           `json:"judgement"`
	Divergence      *Error           `json:"divergence,omitempty"`
	Pixels          []RenderPixel    `json:"pixels"`
	Width           int              `json:"width"`
	Height          int              `json:"height"`
}

type WorkbenchSnapshot struct {
	DBPath       string              `json:"db_path"`
	Seed         int64               `json:"seed"`
	Viewport     Viewport            `json:"viewport"`
	Media        []*Medium           `json:"media"`
	Recipe       Recipe              `json:"recipe"`
	Tolerances   Tolerances          `json:"tolerances"`
	Tiles        []TileDiagnostic    `json:"tiles"`
	Resource     ResourceSnapshot    `json:"resource"`
	Queue        []QueueRecord       `json:"queue"`
	Invalidation *InvalidationReport `json:"invalidation,omitempty"`
	Graph        GraphSummary        `json:"graph"`
	ThreeWay     *ThreeWaySummary    `json:"three_way,omitempty"`
	Preview      *PreviewSummary     `json:"preview,omitempty"`
	Anomaly      *Error              `json:"anomaly,omitempty"`
	Info         string              `json:"info,omitempty"`
}

func NewWorkbench(dbPath string) (*Workbench, error) {
	store, err := OpenCheckpointStore(dbPath)
	if err != nil {
		return nil, err
	}
	sample, err := GenerateSample()
	if err != nil {
		return nil, err
	}
	wb := &Workbench{
		dbPath:    dbPath,
		store:     store,
		sample:    sample,
		graph:     BuildDependencyGraph(&sample.Scene),
		scheduler: NewScheduler(SchedulerConfig{MaxParallelTiles: 3, RayBufferPerWorker: 48, ShadowBudgetTotal: 220_000}),
		tiles:     map[string]TileResult{},
		statuses:  map[string]string{},
	}
	if err := wb.runScheduledLocked(context.Background()); err != nil {
		return nil, err
	}
	preview, err := RenderPreview(&wb.sample.Scene)
	if err != nil {
		return nil, err
	}
	exact, err := RenderExact(&wb.sample.Scene)
	if err != nil {
		return nil, err
	}
	EvaluatePreview(&wb.sample.Scene, preview, exact)
	wb.preview = previewSummary(preview)
	return wb, nil
}

func (w *Workbench) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.store.Close()
}

func (w *Workbench) Snapshot() WorkbenchSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked()
}

func (w *Workbench) snapshotLocked() WorkbenchSnapshot {
	scene := &w.sample.Scene
	tiles := make([]TileDiagnostic, 0, len(w.tiles))
	for _, coord := range TileGrid(scene.Viewport, scene.Recipe) {
		key := tileKey(coord)
		status := w.statuses[key]
		if status == "" {
			status = "pending"
		}
		dto := TileDiagnostic{Tile: coord, Status: status}
		if result, ok := w.tiles[key]; ok {
			dto.Transmittance = result.Transmittance
			dto.Scattering = result.Scattering
			dto.AverageScattering = result.AverageScattering
			dto.CacheKey = result.CacheKey
			dto.ContentHash = result.ContentHash
			dto.ShadowSamples = result.ShadowSamples
			dto.RayBufferUsed = result.MaxRayBufferUsed
			dto.CommitOrder = result.CommitOrder
			dto.PixelCount = len(result.Pixels)
			dto.AverageColor = averageColor(result.Pixels)
		}
		tiles = append(tiles, dto)
	}
	return WorkbenchSnapshot{
		DBPath: w.dbPath, Seed: scene.Seed, Viewport: scene.Viewport, Media: scene.Media,
		Recipe: scene.Recipe, Tolerances: scene.Tolerances, Tiles: tiles,
		Resource: w.resource, Queue: append([]QueueRecord(nil), w.lastQueue...),
		Invalidation: w.invalidated, Graph: graphSummary(w.graph), ThreeWay: w.threeWay,
		Preview: w.preview, Anomaly: w.anomaly, Info: w.info,
	}
}

func (w *Workbench) ClearAnomaly() {
	w.mu.Lock()
	w.anomaly = nil
	w.mu.Unlock()
}

func (w *Workbench) RunAction(ctx context.Context, action string, query url.Values) (WorkbenchSnapshot, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.anomaly = nil
	w.info = ""
	var err error
	switch action {
	case "scheduled", "exact":
		err = w.runScheduledLocked(ctx)
	case "preview":
		steps := queryInt(query, "steps", w.sample.Scene.Recipe.PreviewStepsPerRay)
		shadows := queryInt(query, "shadow", w.sample.Scene.Recipe.PreviewShadowBudget)
		err = w.runPreviewLocked(steps, shadows)
	case "threeway":
		err = w.runThreeWayLocked(ctx)
	case "inject":
		err = w.injectLocked(ctx, query.Get("kind"), queryTile(query))
	case "retry":
		err = w.retryLocked(ctx, queryTile(query))
	case "editVoxel":
		err = w.editVoxelLocked(query)
	case "editScattering":
		err = w.editScatteringLocked(query)
	case "reset":
		err = w.resetLocked(ctx)
	default:
		err = newError(CodeSchema, "unknown workbench action %q", action)
	}
	if fogErr := AsError(err); fogErr != nil {
		w.anomaly = fogErr
		w.info = fogErr.Error()
		err = nil
	} else if err != nil {
		return w.Snapshot(), err
	}
	return w.snapshotLocked(), nil
}

func (w *Workbench) runScheduledLocked(ctx context.Context) error {
	scene := &w.sample.Scene
	report := w.scheduler.Run(ctx, scene, TileGrid(scene.Viewport, scene.Recipe), IntegrationOptions{})
	w.lastQueue = report.Queue
	w.resource = report.Resource
	for key, fe := range report.Failures {
		w.statuses[key] = "failed:" + fe.Code
	}
	ordered := OrderedTiles(report.Tiles)
	for _, result := range ordered {
		result := result
		result.Status = "committed"
		if err := w.store.CommitTile(ctx, result); err != nil {
			return err
		}
		key := tileKey(result.Tile)
		w.tiles[key] = result
		w.statuses[key] = "committed"
	}
	if first := firstSchedulerFailure(report.Failures); first != nil {
		w.anomaly = first
	}
	exact := FrameFromTiles(scene, ordered, "exact")
	preview, err := RenderPreview(scene)
	if err == nil {
		EvaluatePreview(scene, preview, exact)
		w.preview = previewSummary(preview)
	}
	w.info = fmt.Sprintf("scheduled %d committed tiles, %d failures", len(ordered), len(report.Failures))
	return nil
}

func (w *Workbench) runPreviewLocked(steps, shadows int) error {
	scene := &w.sample.Scene
	if steps <= 0 || shadows <= 0 {
		return schemaError("budget", [2]int{steps, shadows}, "preview budgets must be positive")
	}
	options := IntegrationOptions{StepBudget: steps, ShadowBudget: shadows}
	start := nowMillis()
	frame, err := RenderDirectFrame(scene, options, "preview-drag")
	if err != nil {
		return err
	}
	exact, err := RenderExact(scene)
	if err != nil {
		return err
	}
	elapsed := nowMillis() - start
	rayCount := frame.Width * frame.Height
	summary := &PreviewSummary{
		StepBudget: steps, ShadowBudget: shadows, RayCount: rayCount,
		ActualSteps: frame.Steps, ActualShadow: frame.ShadowSamples,
		AverageSteps:  float64(frame.Steps) / float64(rayCount),
		AverageShadow: float64(frame.ShadowSamples) / float64(rayCount),
		ElapsedMS:     elapsed, DelayLimitMS: scene.Recipe.PreviewDelayMS, WithinDelay: elapsed <= scene.Recipe.PreviewDelayMS,
		Pixels: frame.Pixels, Width: frame.Width, Height: frame.Height,
	}
	summary.MaxPixelDelta, summary.Divergence = ComparePreviewExact(scene, frame, exact)
	summary.WithinTolerance = summary.Divergence == nil
	summary.Judgement = fmt.Sprintf("step_budget=%d shadow_budget=%d elapsed=%dms max_pixel_delta=%.5f tolerance=%.3f", steps, shadows, elapsed, summary.MaxPixelDelta, scene.Tolerances.PreviewPixel)
	if summary.Divergence != nil {
		w.anomaly = summary.Divergence
	}
	w.preview = summary
	w.info = "preview evaluated"
	return nil
}

func (w *Workbench) runThreeWayLocked(ctx context.Context) error {
	dir := filepath.Join(filepath.Dir(w.dbPath), "threeway")
	if w.dbPath == ":memory:" {
		dir = filepath.Join(".", "artifacts", "workbench-threeway")
	}
	report, err := RunThreeWayConsistency(ctx, &w.sample.Scene, dir, SchedulerConfig{MaxParallelTiles: 2, RayBufferPerWorker: 48, ShadowBudgetTotal: 220_000})
	if err != nil {
		return err
	}
	w.threeWay = &ThreeWaySummary{
		Equal: report.Equal, FirstDiff: report.FirstDiff, Sequence: report.Realtime.Sequence,
		ResumeReused: report.Resume.ReusedTiles, ResumeRecomputed: report.Resume.RecomputedTiles,
		Stale: report.Resume.StaleCheckpoints,
	}
	w.info = "three-path comparison completed"
	return nil
}

func (w *Workbench) injectLocked(ctx context.Context, kind string, tile TileCoord) error {
	switch kind {
	case "decode":
		medium := w.sample.Scene.Media[0]
		document, err := EncodeMedium(medium)
		if err != nil {
			return err
		}
		corrupt, _, err := CorruptEncodedMedium(document)
		if err != nil {
			return err
		}
		_, err = DecodeMedium(corrupt)
		if fe := AsError(err); fe != nil {
			location := w.firstMediumTile(medium.ID)
			fe.Tile = &location
			return fe
		}
		return err
	case "order":
		bad := w.sample.Scene
		bad.CameraTrack.Samples = append([]CameraSample(nil), bad.CameraTrack.Samples...)
		if len(bad.CameraTrack.Samples) > 1 {
			bad.CameraTrack.Samples[1].LogicalTime = bad.CameraTrack.Samples[0].LogicalTime - 1
		}
		err := ValidateScene(&bad)
		if fe := AsError(err); fe != nil {
			location := TileCoord{X: 0, Y: 0}
			if len(w.sample.Truth.Tiles) > 1 {
				location = w.sample.Truth.Tiles[1].Tile
			}
			fe.Tile = &location
			return fe
		}
		return err
	case "density":
		voxel, location, found := w.firstAffectedVoxel("fog-a")
		if !found {
			voxel = [3]int{0, 0, 0}
			location = tile
		}
		_, err := PlanVoxelEdit(&w.sample.Scene, w.graph, "fog-a", voxel, MaxDensityVoxel+99)
		if fe := AsError(err); fe != nil {
			fe.Tile = &location
			return fe
		}
		return err
	case "block":
		w.scheduler.Faults.FailNext(tile, 1)
		err := w.runScheduledLocked(ctx)
		if w.anomaly != nil && w.anomaly.Code == CodeBlock {
			w.anomaly.MediumID = w.primaryMediumForTile(tile)
		}
		return err
	default:
		return schemaError("kind", kind, "unknown injection kind")
	}
}

func (w *Workbench) firstMediumTile(mediumID string) TileCoord {
	nodeID := "scattering:" + mediumID + ":v" + fmt.Sprint(w.sample.Scene.Medium(mediumID).Version)
	tiles := w.graph.AffectedTiles(nodeID)
	if len(tiles) > 0 {
		return tiles[0]
	}
	return TileCoord{X: 0, Y: 0}
}

func (w *Workbench) firstAffectedVoxel(mediumID string) ([3]int, TileCoord, bool) {
	for _, node := range w.graph.Nodes {
		if node.Kind != "voxel" || node.MediumID != mediumID || node.Voxel == nil {
			continue
		}
		tiles := w.graph.AffectedTiles(node.ID)
		if len(tiles) > 0 {
			return *node.Voxel, tiles[0], true
		}
	}
	return [3]int{}, TileCoord{}, false
}

func (w *Workbench) primaryMediumForTile(tile TileCoord) string {
	for _, ray := range RaysForTile(w.sample.Scene, tile) {
		hits, err := IntersectRayMedia(ray, w.sample.Scene.Media)
		if err != nil || len(hits) == 0 {
			continue
		}
		segments, err := BuildRaySegments(ray, hits)
		if err != nil || len(segments) == 0 || len(segments[0].MediumIDs) == 0 {
			continue
		}
		return segments[0].MediumIDs[0]
	}
	return ""
}

func (w *Workbench) retryLocked(ctx context.Context, tile TileCoord) error {
	result, err := RenderTile(&w.sample.Scene, tile, TileRenderConfig{Options: IntegrationOptions{}, RayBufferLimit: w.scheduler.Config.RayBufferPerWorker})
	if err != nil {
		return err
	}
	result.Status = "committed-retry"
	if err := w.store.CommitTile(ctx, *result); err != nil {
		return err
	}
	w.tiles[tileKey(tile)] = *result
	w.statuses[tileKey(tile)] = "committed-retry"
	w.info = fmt.Sprintf("tile %v retried and committed", tile)
	return nil
}

func (w *Workbench) editVoxelLocked(query url.Values) error {
	mediumID := query.Get("medium")
	if mediumID == "" {
		mediumID = "fog-a"
	}
	voxel := [3]int{queryInt(query, "vx", 0), queryInt(query, "vy", 0), queryInt(query, "vz", 0)}
	value := queryInt(query, "value", 4)
	report, err := ApplyVoxelEdit(&w.sample.Scene, w.graph, mediumID, voxel, value)
	if err != nil {
		return err
	}
	w.graph = BuildDependencyGraph(&w.sample.Scene)
	w.invalidated = report
	w.info = fmt.Sprintf("voxel edited; %d tiles invalidated, %d reused", len(report.AffectedTiles), len(report.ReusedTiles))
	return w.rerenderAffectedLocked(report.AffectedTiles)
}

func (w *Workbench) editScatteringLocked(query url.Values) error {
	mediumID := query.Get("medium")
	if mediumID == "" {
		mediumID = "fog-b"
	}
	value := FixedFromFloat(queryFloat(query, "value", 0.2))
	report, err := ApplyScatteringEdit(&w.sample.Scene, w.graph, mediumID, value)
	if err != nil {
		return err
	}
	w.graph = BuildDependencyGraph(&w.sample.Scene)
	w.invalidated = report
	w.info = fmt.Sprintf("scattering edited; %d tiles invalidated, %d reused", len(report.AffectedTiles), len(report.ReusedTiles))
	return w.rerenderAffectedLocked(report.AffectedTiles)
}

func (w *Workbench) rerenderAffectedLocked(tiles []TileCoord) error {
	if len(tiles) == 0 {
		return nil
	}
	report := w.scheduler.Run(context.Background(), &w.sample.Scene, tiles, IntegrationOptions{})
	for _, result := range OrderedTiles(report.Tiles) {
		result := result
		result.Status = "recomputed-after-invalidation"
		if err := w.store.CommitTile(context.Background(), result); err != nil {
			return err
		}
		w.tiles[tileKey(result.Tile)] = result
		w.statuses[tileKey(result.Tile)] = result.Status
	}
	w.lastQueue = append(w.lastQueue, report.Queue...)
	w.resource = report.Resource
	return firstSchedulerFailure(report.Failures)
}

func (w *Workbench) resetLocked(ctx context.Context) error {
	sample, err := GenerateSample()
	if err != nil {
		return err
	}
	w.sample = sample
	w.graph = BuildDependencyGraph(&sample.Scene)
	w.scheduler = NewScheduler(SchedulerConfig{MaxParallelTiles: 3, RayBufferPerWorker: 48, ShadowBudgetTotal: 220_000})
	w.tiles = map[string]TileResult{}
	w.statuses = map[string]string{}
	w.invalidated = nil
	w.threeWay = nil
	w.preview = nil
	return w.runScheduledLocked(ctx)
}

func previewSummary(result *PreviewResult) *PreviewSummary {
	if result == nil {
		return nil
	}
	return &PreviewSummary{
		StepBudget: result.StepBudget, ShadowBudget: result.ShadowBudget, RayCount: result.RayCount,
		ActualSteps: result.ActualSteps, ActualShadow: result.ActualShadow,
		AverageSteps: result.AverageSteps, AverageShadow: result.AverageShadow,
		RecipeVersion: result.RecipeVersion, CameraVersion: result.CameraVersion,
		MediumVersions: result.MediumVersions,
		ElapsedMS:      result.ElapsedMS, DelayLimitMS: result.DelayLimitMS,
		WithinDelay: result.WithinDelay, MaxPixelDelta: result.MaxPixelDelta, WithinTolerance: result.WithinTolerance,
		Judgement: result.JudgementBasis, Divergence: result.Divergence, Pixels: result.Frame.Pixels,
		Width: result.Frame.Width, Height: result.Frame.Height,
	}
}

func graphSummary(graph *DependencyGraph) GraphSummary {
	summary := GraphSummary{NodeCount: len(graph.Nodes), EdgeCount: len(graph.Edges)}
	for _, node := range graph.Nodes {
		switch node.Kind {
		case "voxel":
			summary.VoxelNodes++
		case "ray_segment":
			summary.SegmentNodes++
		case "tile":
			summary.TileNodes++
		}
	}
	return summary
}

func averageColor(pixels []RenderPixel) [3]uint8 {
	if len(pixels) == 0 {
		return [3]uint8{}
	}
	var sums [3]int
	for _, pixel := range pixels {
		for c := 0; c < 3; c++ {
			sums[c] += int(pixel.Color[c])
		}
	}
	return [3]uint8{uint8(sums[0] / len(pixels)), uint8(sums[1] / len(pixels)), uint8(sums[2] / len(pixels))}
}

func queryTile(query map[string][]string) TileCoord {
	return TileCoord{X: queryInt(query, "x", 1), Y: queryInt(query, "y", 1)}
}

func queryInt(query url.Values, key string, fallback int) int {
	values, ok := query[key]
	_ = ok
	if len(values) == 0 {
		return fallback
	}
	value, err := strconv.Atoi(values[0])
	if err != nil {
		return fallback
	}
	return value
}

func queryFloat(query url.Values, key string, fallback float64) float64 {
	values, ok := query[key]
	_ = ok
	if len(values) == 0 {
		return fallback
	}
	value, err := strconv.ParseFloat(values[0], 64)
	if err != nil {
		return fallback
	}
	return value
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}
