package silhouette

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type RunPath string

const (
	PathRealtime      RunPath = "realtime"
	PathResume        RunPath = "resume"
	PathFullRecompute RunPath = "full_recompute"
	PathEdit          RunPath = "edit"
)

type RunConfig struct {
	Workers           int
	VerticesPerWorker int
	Sampling          SamplingOptions
	StopAfterCommits  int
}

type RunResult struct {
	Path         RunPath           `json:"path"`
	Scene        *Scene            `json:"scene"`
	Sampling     *SamplingResult   `json:"sampling"`
	Assembly     *AssemblyResult   `json:"assembly"`
	Dependencies *DependencyGraph  `json:"dependencies"`
	CommitOrder  []string          `json:"commit_order"`
	Commits      []CommitRecord    `json:"commits"`
	ReusedTiles  []string          `json:"reused_tiles"`
	Invalidation *InvalidationPlan `json:"invalidation,omitempty"`
	Interrupted  bool              `json:"interrupted"`
}

func normalizeRunConfig(cfg RunConfig) RunConfig {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.VerticesPerWorker <= 0 {
		cfg.VerticesPerWorker = DefaultVerticesPerWorker
	}
	if cfg.Sampling.Workers <= 0 {
		cfg.Sampling.Workers = cfg.Workers
	}
	if cfg.Sampling.VerticesPerWorker <= 0 {
		cfg.Sampling.VerticesPerWorker = cfg.VerticesPerWorker
	}
	return cfg
}

func orderedTileAssemblies(scene *Scene, assembly *AssemblyResult) []TileAssembly {
	byID := make(map[string]TileAssembly, len(assembly.Tiles))
	for _, tile := range assembly.Tiles {
		byID[tile.TileID] = tile
	}
	order := make([]TileAssembly, 0, len(scene.Tiles))
	for _, tile := range scene.Tiles {
		if item, ok := byID[tile.ID]; ok {
			order = append(order, item)
		}
	}
	return order
}

func tileOrderIDs(scene *Scene, assembly *AssemblyResult) []string {
	tiles := orderedTileAssemblies(scene, assembly)
	out := make([]string, 0, len(tiles))
	for _, tile := range tiles {
		out = append(out, tile.TileID)
	}
	return out
}

func setAssemblyTile(assembly *AssemblyResult, replacement TileAssembly) {
	for i := range assembly.Tiles {
		if assembly.Tiles[i].TileID == replacement.TileID {
			assembly.Tiles[i] = replacement
			return
		}
	}
	assembly.Tiles = append(assembly.Tiles, replacement)
	sort.SliceStable(assembly.Tiles, func(i, j int) bool { return assembly.Tiles[i].TileID < assembly.Tiles[j].TileID })
}

func runPipeline(ctx context.Context, path RunPath, scene *Scene, store *CheckpointStore, cfg RunConfig, mode string) (*RunResult, error) {
	if scene == nil || store == nil {
		return nil, NewDomainError(CodeInternal, "$", "scene and checkpoint store are required")
	}
	cfg = normalizeRunConfig(cfg)
	if mode == "full" {
		if err := store.ClearScene(ctx, scene.ID); err != nil {
			return nil, err
		}
	}
	sampling, err := SampleSceneContext(ctx, scene, cfg.Sampling)
	if err != nil {
		return nil, err
	}
	assembly, err := AssembleContext(ctx, scene, sampling, cfg.Workers, cfg.VerticesPerWorker)
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		Path: path, Scene: scene, Sampling: sampling, Assembly: assembly,
		CommitOrder: tileOrderIDs(scene, assembly),
	}
	existing := make(map[string]Checkpoint)
	if mode == "resume" {
		list, err := store.ListCheckpoints(ctx, scene.ID)
		if err != nil {
			return nil, err
		}
		for _, cp := range list {
			existing[cp.TileID] = cp
		}
	}
	committed := 0
	for _, tile := range orderedTileAssemblies(scene, assembly) {
		if cfg.StopAfterCommits > 0 && committed >= cfg.StopAfterCommits {
			result.Interrupted = true
			break
		}
		if mode == "resume" {
			if cp, ok := existing[tile.TileID]; ok {
				if reason := checkpointMismatchReason(&cp, tile, scene.Tolerance.ParamVersion, scene.Budget.ParamVersion); reason == "" {
					setAssemblyTile(assembly, cp.Tile)
					result.ReusedTiles = append(result.ReusedTiles, tile.TileID)
					continue
				}
			}
		}
		record, err := store.CommitTile(ctx, scene.ID, tile, scene.Tolerance.ParamVersion, scene.Budget.ParamVersion)
		if err != nil {
			return nil, err
		}
		result.Commits = append(result.Commits, record)
		committed++
	}
	sort.Strings(result.ReusedTiles)
	graph, err := BuildDependencyGraph(scene, assembly)
	if err != nil {
		return nil, err
	}
	result.Dependencies = graph
	return result, nil
}

func RunRealtime(ctx context.Context, scene *Scene, store *CheckpointStore, cfg RunConfig) (*RunResult, error) {
	return runPipeline(ctx, PathRealtime, scene, store, cfg, "realtime")
}

func RunResume(ctx context.Context, scene *Scene, store *CheckpointStore, cfg RunConfig) (*RunResult, error) {
	return runPipeline(ctx, PathResume, scene, store, cfg, "resume")
}

func RunFullRecompute(ctx context.Context, scene *Scene, store *CheckpointStore, cfg RunConfig) (*RunResult, error) {
	return runPipeline(ctx, PathFullRecompute, scene, store, cfg, "full")
}

// RunEdit applies an edit, reuses persisted tiles proven unaffected, commits
// only invalidated tiles, and returns the precise invalidation list.
func RunEdit(ctx context.Context, scene *Scene, oldAssembly *AssemblyResult, edit Edit, store *CheckpointStore, cfg RunConfig) (*RunResult, error) {
	cfg = normalizeRunConfig(cfg)
	next, err := applyEdit(scene, edit)
	if err != nil {
		return nil, err
	}
	samplingOpts := cfg.Sampling
	if samplingOpts.Workers <= 0 {
		samplingOpts.Workers = cfg.Workers
	}
	if samplingOpts.VerticesPerWorker <= 0 {
		samplingOpts.VerticesPerWorker = cfg.VerticesPerWorker
	}
	plan, err := PlanInvalidation(scene, oldAssembly, edit, samplingOpts)
	if err != nil {
		return nil, err
	}
	assembly := plan.NewAssembly
	list, err := store.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		return nil, err
	}
	checkpoints := make(map[string]Checkpoint, len(list))
	for _, cp := range list {
		checkpoints[cp.TileID] = cp
	}
	result := &RunResult{
		Path: PathEdit, Scene: next, Sampling: plan.NewSampling, Assembly: assembly,
		Invalidation: plan,
	}
	invalidated := make(map[string]bool, len(plan.InvalidatedTiles))
	for _, id := range plan.InvalidatedTiles {
		invalidated[id] = true
	}
	for _, tile := range orderedTileAssemblies(next, assembly) {
		if !invalidated[tile.TileID] {
			cp, ok := checkpoints[tile.TileID]
			if !ok {
				return nil, NewDomainError(CodeInternal, "$.store", "expected reusable checkpoint is missing").
					WithDetail("tile_id", tile.TileID)
			}
			setAssemblyTile(assembly, cp.Tile)
			result.ReusedTiles = append(result.ReusedTiles, tile.TileID)
			result.CommitOrder = append(result.CommitOrder, tile.TileID)
			continue
		}
		record, err := store.CommitTile(ctx, next.ID, tile, next.Tolerance.ParamVersion, next.Budget.ParamVersion)
		if err != nil {
			return nil, err
		}
		result.Commits = append(result.Commits, record)
		result.CommitOrder = append(result.CommitOrder, tile.TileID)
	}
	sort.Strings(result.ReusedTiles)
	graph, err := BuildDependencyGraph(next, assembly)
	if err != nil {
		return nil, err
	}
	result.Dependencies = graph
	return result, nil
}

type ConsistencyReport struct {
	Consistent          bool     `json:"consistent"`
	ComparedTiles       int      `json:"compared_tiles"`
	ComparedVertices    int      `json:"compared_vertices"`
	FirstMismatchTile   string   `json:"first_mismatch_tile,omitempty"`
	FirstMismatchVertex int      `json:"first_mismatch_vertex,omitempty"`
	FirstMismatchKind   string   `json:"first_mismatch_kind,omitempty"`
	Details             []string `json:"details,omitempty"`
	LeftPath            RunPath  `json:"left_path"`
	RightPath           RunPath  `json:"right_path"`
}

func CompareRuns(left, right *RunResult) *ConsistencyReport {
	report := &ConsistencyReport{Consistent: true}
	if left == nil || right == nil {
		report.Consistent = false
		report.FirstMismatchKind = "missing_run"
		return report
	}
	report.LeftPath, report.RightPath = left.Path, right.Path
	if !equalStringSlices(left.CommitOrder, right.CommitOrder) {
		report.Consistent = false
		report.FirstMismatchKind = "commit_order"
		report.FirstMismatchTile = firstStringMismatch(left.CommitOrder, right.CommitOrder)
		report.Details = append(report.Details, fmt.Sprintf("commit order differs: %v vs %v", left.CommitOrder, right.CommitOrder))
	}
	leftTiles := tileMap(left.Assembly)
	rightTiles := tileMap(right.Assembly)
	ids := make([]string, 0, len(leftTiles))
	for id := range leftTiles {
		ids = append(ids, id)
	}
	for id := range rightTiles {
		if _, ok := leftTiles[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		lt, lok := leftTiles[id]
		rt, rok := rightTiles[id]
		if !lok || !rok {
			report.Consistent = false
			if report.FirstMismatchTile == "" {
				report.FirstMismatchTile = id
				report.FirstMismatchKind = "missing_tile"
			}
			report.Details = append(report.Details, "tile presence differs: "+id)
			continue
		}
		report.ComparedTiles++
		if lt.ContentHash != rt.ContentHash {
			report.Consistent = false
			if report.FirstMismatchTile == "" {
				report.FirstMismatchTile = id
				report.FirstMismatchKind = "tile_content_hash"
			}
			report.Details = append(report.Details, "tile content hash differs: "+id)
		}
		if lt.CacheKey != rt.CacheKey {
			report.Consistent = false
			if report.FirstMismatchTile == "" {
				report.FirstMismatchTile = id
				report.FirstMismatchKind = "tile_cache_key"
			}
		}
		if mismatch := compareTileVertices(lt, rt); mismatch >= 0 {
			report.Consistent = false
			if report.FirstMismatchTile == "" {
				report.FirstMismatchTile = id
				report.FirstMismatchVertex = mismatch
				report.FirstMismatchKind = "tile_vertex"
			}
			report.Details = append(report.Details, fmt.Sprintf("tile %s vertex %d differs", id, mismatch))
		} else {
			report.ComparedVertices += len(lt.Vertices)
		}
	}
	if mismatch := compareSegmentSamples(left.Sampling, right.Sampling); mismatch != "" {
		report.Consistent = false
		if report.FirstMismatchTile == "" {
			report.FirstMismatchKind = "segment_sample"
			parts := strings.SplitN(mismatch, ":", 2)
			if len(parts) == 2 {
				report.FirstMismatchTile = firstTileForSegment(left.Assembly, parts[0])
				fmt.Sscanf(parts[1], "%d", &report.FirstMismatchVertex)
			}
		}
		report.Details = append(report.Details, mismatch)
	}
	if !budgetEqual(left.Sampling, right.Sampling) {
		report.Consistent = false
		if report.FirstMismatchTile == "" {
			report.FirstMismatchKind = "budget_stats"
		}
		report.Details = append(report.Details, "budget/degradation statistics differ")
	}
	return report
}

func tileMap(assembly *AssemblyResult) map[string]TileAssembly {
	out := make(map[string]TileAssembly)
	if assembly == nil {
		return out
	}
	for _, tile := range assembly.Tiles {
		out[tile.TileID] = tile
	}
	return out
}

func compareTileVertices(a, b TileAssembly) int {
	if len(a.Segments) != len(b.Segments) {
		return 0
	}
	index := 0
	for i := range a.Segments {
		as, bs := a.Segments[i], b.Segments[i]
		if as.SegmentID != bs.SegmentID || as.MetadataVersion != bs.MetadataVersion || as.MaxChordHeight != bs.MaxChordHeight || len(as.Vertices) != len(bs.Vertices) {
			return index
		}
		for j := range as.Vertices {
			av, bv := as.Vertices[j], bs.Vertices[j]
			if av.Index != bv.Index || av.X != bv.X || av.Y != bv.Y || av.TNum != bv.TNum || av.TDen != bv.TDen {
				return index
			}
			index++
		}
	}
	if len(a.Vertices) != len(b.Vertices) {
		return 0
	}
	return -1
}

func compareSegmentSamples(a, b *SamplingResult) string {
	if a == nil || b == nil {
		if a != b {
			return "segment_samples:nil"
		}
		return ""
	}
	am := make(map[string]SegmentSample)
	bm := make(map[string]SegmentSample)
	for _, sample := range a.Segments {
		am[sample.SegmentID] = sample
	}
	for _, sample := range b.Segments {
		bm[sample.SegmentID] = sample
	}
	ids := make([]string, 0, len(am))
	for id := range am {
		ids = append(ids, id)
	}
	for id := range bm {
		if _, ok := am[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		as, aok := am[id]
		bs, bok := bm[id]
		if !aok || !bok {
			return id + ":0"
		}
		if len(as.Vertices) != len(bs.Vertices) || as.MaxChordHeight != bs.MaxChordHeight || as.Tolerance != bs.Tolerance || as.DepthReached != bs.DepthReached {
			return fmt.Sprintf("%s:0", id)
		}
		for i := range as.Vertices {
			av, bv := as.Vertices[i], bs.Vertices[i]
			if av.Index != bv.Index || av.X != bv.X || av.Y != bv.Y || av.TNum != bv.TNum || av.TDen != bv.TDen {
				return fmt.Sprintf("%s:%d", id, i)
			}
		}
	}
	return ""
}

func budgetEqual(a, b *SamplingResult) bool {
	if a == nil || b == nil {
		return a == b
	}
	aj, _ := json.Marshal(a.Budget)
	bj, _ := json.Marshal(b.Budget)
	return string(aj) == string(bj)
}

func firstTileForSegment(assembly *AssemblyResult, segmentID string) string {
	if assembly == nil {
		return ""
	}
	for _, tile := range assembly.Tiles {
		for _, ts := range tile.Segments {
			if ts.SegmentID == segmentID {
				return tile.TileID
			}
		}
	}
	return ""
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstStringMismatch(a, b []string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i]
		}
	}
	if len(a) > n {
		return a[n]
	}
	if len(b) > n {
		return b[n]
	}
	return ""
}

func IsNotFound(err error) bool { return err == sql.ErrNoRows }
