package silhouette

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

type TileSegment struct {
	SegmentID       string         `json:"segment_id"`
	MetadataVersion string         `json:"metadata_version"`
	Weight          int64          `json:"weight"`
	MaxChordHeight  int64          `json:"max_chord_height"`
	Vertices        []SampleVertex `json:"vertices"`
	CoverageNum     int64          `json:"coverage_num"`
	CoverageDen     int64          `json:"coverage_den"`
	Coverage        string         `json:"coverage"`
}

type TileAssembly struct {
	TileID           string         `json:"tile_id"`
	Bounds           Tile           `json:"bounds"`
	Segments         []TileSegment  `json:"segments"`
	Vertices         []SampleVertex `json:"vertices"`
	CoverageNum      int64          `json:"coverage_num"`
	CoverageDen      int64          `json:"coverage_den"`
	Coverage         string         `json:"coverage"`
	ProjectionDigest string         `json:"projection_digest"`
	InputHash        string         `json:"input_hash"`
	ContentHash      string         `json:"content_hash"`
	CacheKey         string         `json:"cache_key"`
	BoundaryRule     string         `json:"boundary_rule"`
}

type AssemblyResult struct {
	SceneID          string           `json:"scene_id"`
	ToleranceBySeg   map[string]int64 `json:"tolerance_by_segment"`
	SegmentSamples   []SegmentSample  `json:"segment_samples"`
	Tiles            []TileAssembly   `json:"tiles"`
	Budget           BudgetReport     `json:"budget"`
	AlgorithmVersion string           `json:"algorithm_version"`
	GeometryVersion  string           `json:"geometry_version"`
	WorkerEvents     []WorkerEvent    `json:"worker_events,omitempty"`
}

func containsPoint(tile Tile, p Vec2) bool {
	return p.X >= tile.Min.X && p.X < tile.Max.X && p.Y >= tile.Min.Y && p.Y < tile.Max.Y
}

func containsPointClosed(tile Tile, p Vec2) bool {
	return p.X >= tile.Min.X && p.X <= tile.Max.X && p.Y >= tile.Min.Y && p.Y <= tile.Max.Y
}

// OwnTile applies the declared deterministic ownership rule: prefer the
// lowest-order half-open tile containing the point; only the scene's outer
// maximum edge falls back to closed bounds. A vertex is therefore counted in at
// most one tile.
func OwnTile(tiles []Tile, p Vec2) (Tile, bool) {
	ordered := append([]Tile(nil), tiles...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Order == ordered[j].Order {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Order < ordered[j].Order
	})
	for _, tile := range ordered {
		if containsPoint(tile, p) {
			return tile, true
		}
	}
	for _, tile := range ordered {
		if containsPointClosed(tile, p) {
			return tile, true
		}
	}
	return Tile{}, false
}

func hashJSON(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashStrings(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type projectionPayload struct {
	GeometryVersion string          `json:"geometry_version"`
	Bounds          geometryBounds  `json:"bounds"`
	Segments        []projectionSeg `json:"segments"`
}

type geometryBounds struct {
	Min Vec2 `json:"min"`
	Max Vec2 `json:"max"`
}

type projectionSeg struct {
	ID       string         `json:"id"`
	Vertices []SampleVertex `json:"vertices"`
}

// ComputeCacheKey implements the documented cache-key contract.
func ComputeCacheKey(inputHash string, segmentIDs []string, toleranceVersion, budgetVersion, upstreamKey string) string {
	ids := append([]string(nil), segmentIDs...)
	sort.Strings(ids)
	payload := struct {
		InputHash        string   `json:"input_hash"`
		SegmentIDs       []string `json:"segment_ids"`
		ToleranceVersion string   `json:"tolerance_version"`
		BudgetVersion    string   `json:"budget_version"`
		UpstreamKey      string   `json:"upstream_key"`
	}{inputHash, ids, toleranceVersion, budgetVersion, upstreamKey}
	return hashJSON(payload)
}

func segmentUpstreamKey(scene *Scene, seg CurveSegment, tolerance int64) string {
	payload := struct {
		SegmentID        string  `json:"segment_id"`
		Controls         [4]Vec2 `json:"controls"`
		Weight           int64   `json:"weight"`
		MetadataVersion  string  `json:"metadata_version"`
		Tolerance        int64   `json:"tolerance"`
		ToleranceVersion string  `json:"tolerance_version"`
		BudgetVersion    string  `json:"budget_version"`
		AlgorithmVersion string  `json:"algorithm_version"`
	}{seg.ID, seg.Controls, seg.Weight, seg.MetadataVersion, tolerance, scene.Tolerance.ParamVersion, scene.Budget.ParamVersion, AlgorithmVersion}
	return hashJSON(payload)
}

// Assemble builds tile results from segment samples. Iteration and output are
// sorted by segment/tile ID, so parallel completion order cannot affect hashes.
func Assemble(scene *Scene, sampling *SamplingResult) (*AssemblyResult, error) {
	return AssembleContext(context.Background(), scene, sampling, 1, 0)
}

func AssembleContext(ctx context.Context, scene *Scene, sampling *SamplingResult, workers, verticesPerWorker int) (*AssemblyResult, error) {
	if scene == nil || sampling == nil {
		return nil, NewDomainError(CodeInternal, "$", "scene and sampling result are required")
	}
	samples := append([]SegmentSample(nil), sampling.Segments...)
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].SegmentID < samples[j].SegmentID })
	result := &AssemblyResult{
		SceneID: scene.ID, ToleranceBySeg: sampling.ToleranceBySeg,
		SegmentSamples: samples, Budget: sampling.Budget,
		AlgorithmVersion: AlgorithmVersion, GeometryVersion: GeometryVersion,
	}
	segmentByID := make(map[string]CurveSegment, len(scene.Segments))
	for _, seg := range scene.Segments {
		segmentByID[seg.ID] = seg
	}
	total := totalVertices(samples)
	if workers <= 1 || len(scene.Tiles) <= 1 {
		for _, tile := range scene.Tiles {
			assembly, err := assembleTile(scene, tile, samples, segmentByID, total)
			if err != nil {
				return nil, err
			}
			result.Tiles = append(result.Tiles, assembly)
		}
	} else {
		pool, err := NewWorkerPool(WorkerPoolConfig{Workers: workers, VerticesPerWorker: verticesPerWorker})
		if err != nil {
			return nil, err
		}
		tiles := append([]Tile(nil), scene.Tiles...)
		assemblies := make([]TileAssembly, len(tiles))
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex
		for i, tile := range tiles {
			i, tile := i, tile
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := pool.Do(ctx, "assemble:"+tile.ID, 0, func(workerID, bufferLimit int) (int, error) {
					assembly, err := assembleTile(scene, tile, samples, segmentByID, total)
					if err != nil {
						return 0, err
					}
					if len(assembly.Vertices) > bufferLimit {
						return len(assembly.Vertices), (&DomainError{Code: CodeBuffer, Path: "$.worker.buffer", Message: "per-worker assembly vertex buffer exhausted"}).
							WithDetail("worker_id", strconv.Itoa(workerID)).
							WithDetail("task", tile.ID).
							WithDetail("vertices", strconv.Itoa(len(assembly.Vertices))).
							WithDetail("capacity", strconv.Itoa(bufferLimit))
					}
					assemblies[i] = assembly
					return len(assembly.Vertices), nil
				})
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
		pool.Close()
		result.WorkerEvents = append(result.WorkerEvents, pool.Events()...)
		if firstErr != nil {
			return nil, firstErr
		}
		result.Tiles = append(result.Tiles, assemblies...)
	}
	sort.SliceStable(result.Tiles, func(i, j int) bool { return result.Tiles[i].TileID < result.Tiles[j].TileID })
	return result, nil
}

func assembleTile(scene *Scene, tile Tile, samples []SegmentSample, segmentByID map[string]CurveSegment, total int) (TileAssembly, error) {
	assembly := TileAssembly{
		TileID: tile.ID, Bounds: tile, BoundaryRule: BoundaryRule,
		CoverageDen: int64(total),
	}
	var projection projectionPayload
	projection.GeometryVersion = GeometryVersion
	projection.Bounds = geometryBounds{Min: tile.Min, Max: tile.Max}
	var allSegmentIDs []string
	var upstreamParts []string
	for _, sample := range samples {
		seg, ok := segmentByID[sample.SegmentID]
		if !ok {
			continue
		}
		owned := make([]SampleVertex, 0)
		for _, vertex := range sample.Vertices {
			owner, ok := OwnTile(scene.Tiles, Vec2{X: vertex.X, Y: vertex.Y})
			if ok && owner.ID == tile.ID {
				owned = append(owned, vertex)
			}
		}
		if len(owned) == 0 {
			continue
		}
		coverageDen := int64(len(sample.Vertices))
		if coverageDen < 1 {
			coverageDen = 1
		}
		ts := TileSegment{
			SegmentID: sample.SegmentID, MetadataVersion: seg.MetadataVersion,
			Weight: seg.Weight, MaxChordHeight: sample.MaxChordHeight, Vertices: owned,
			CoverageNum: int64(len(owned)), CoverageDen: coverageDen,
			Coverage: fmt.Sprintf("%d/%d", len(owned), coverageDen),
		}
		assembly.Segments = append(assembly.Segments, ts)
		assembly.Vertices = append(assembly.Vertices, owned...)
		assembly.CoverageNum += int64(len(owned))
		allSegmentIDs = append(allSegmentIDs, seg.ID)
		upstreamParts = append(upstreamParts, segmentUpstreamKey(scene, seg, sample.Tolerance))
		projection.Segments = append(projection.Segments, projectionSeg{
			ID: seg.ID, Vertices: owned,
		})
	}
	sort.Strings(upstreamParts)
	upstreamKey := hashStrings(upstreamParts...)
	projectionBytes, _ := json.Marshal(projection)
	assembly.ProjectionDigest = hashJSON(projection)
	assembly.InputHash = hashJSON(struct {
		Bounds     geometryBounds `json:"bounds"`
		Segments   []string       `json:"segments"`
		Projection string         `json:"projection"`
	}{projection.Bounds, allSegmentIDs, assembly.ProjectionDigest})
	meta := make([]string, 0, len(assembly.Segments))
	for _, ts := range assembly.Segments {
		meta = append(meta, ts.SegmentID+"@"+ts.MetadataVersion)
	}
	sort.Strings(meta)
	sum := sha256.Sum256(append(projectionBytes, []byte("\x00"+joinStrings(meta, "\x00"))...))
	assembly.ContentHash = hex.EncodeToString(sum[:])
	assembly.CacheKey = ComputeCacheKey(assembly.InputHash, allSegmentIDs, scene.Tolerance.ParamVersion, scene.Budget.ParamVersion, upstreamKey)
	if assembly.CoverageDen > 0 {
		assembly.Coverage = fmt.Sprintf("%d/%d", assembly.CoverageNum, assembly.CoverageDen)
	} else {
		assembly.Coverage = "0/1"
	}
	return assembly, nil
}

func totalVertices(samples []SegmentSample) int {
	total := 0
	for _, sample := range samples {
		total += len(sample.Vertices)
	}
	return total
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += sep
		}
		out += item
	}
	return out
}
