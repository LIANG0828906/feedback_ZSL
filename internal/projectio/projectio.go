package projectio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"terrain-occlusion/internal/model"
)

func LoadFile(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.ESchema, "file", "read project: %v", err)
	}
	if err := requirePresent(data); err != nil {
		return nil, err
	}
	var p model.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, model.NewError(model.ESchema, "json", "invalid JSON: %v", err)
	}
	if err := Validate(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func SaveFile(path string, p *model.Project) error {
	if err := Validate(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func requirePresent(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return model.NewError(model.ESchema, "json", "object required: %v", err)
	}
	if err := requireKeys(root, "", "schema_version", "seed", "parameters", "budgets", "terrain", "occluders", "chunks", "camera_track", "truth"); err != nil {
		return err
	}

	var terrain map[string]json.RawMessage
	_ = json.Unmarshal(root["terrain"], &terrain)
	if err := requireKeys(terrain, "terrain", "width_cells", "depth_cells", "cell_size", "heights"); err != nil {
		return err
	}

	var params map[string]json.RawMessage
	_ = json.Unmarshal(root["parameters"], &params)
	if err := requireKeys(params, "parameters", "shadow_resolution", "light_azimuth_deg", "light_elevation_deg", "camera_depth_resolution", "sample_spacing", "fixed_point_step", "viewport_width", "viewport_height", "max_workers", "depth_buffer_limit"); err != nil {
		return err
	}

	var budgets map[string]json.RawMessage
	_ = json.Unmarshal(root["budgets"], &budgets)
	if err := requireKeys(budgets, "budgets", "load_slots", "memory_bytes", "max_residency_delay_samples", "max_parallel_tasks", "depth_buffer_entries_per_thread"); err != nil {
		return err
	}

	var chunks []map[string]json.RawMessage
	_ = json.Unmarshal(root["chunks"], &chunks)
	for i, ch := range chunks {
		if err := requireKeys(ch, fmt.Sprintf("chunks[%d]", i), "id", "coverage", "geometry_complexity", "load_cost", "memory_bytes", "occluder_refs", "sample_points"); err != nil {
			return err
		}
		var coverage map[string]json.RawMessage
		_ = json.Unmarshal(ch["coverage"], &coverage)
		if err := requireKeys(coverage, fmt.Sprintf("chunks[%d].coverage", i), "min_x", "min_z", "max_x", "max_z"); err != nil {
			return err
		}
	}
	var cams []map[string]json.RawMessage
	_ = json.Unmarshal(root["camera_track"], &cams)
	for i, cam := range cams {
		if err := requireKeys(cam, fmt.Sprintf("camera_track[%d]", i), "index", "logical_time_ms", "position", "target", "fov_deg", "near", "far"); err != nil {
			return err
		}
		for _, nested := range []string{"position", "target"} {
			var vec map[string]json.RawMessage
			_ = json.Unmarshal(cam[nested], &vec)
			if err := requireKeys(vec, fmt.Sprintf("camera_track[%d].%s", i, nested), "x", "y", "z"); err != nil {
				return err
			}
		}
	}

	var occluders []map[string]json.RawMessage
	_ = json.Unmarshal(root["occluders"], &occluders)
	for i, occ := range occluders {
		if err := requireKeys(occ, fmt.Sprintf("occluders[%d]", i), "id", "bounds"); err != nil {
			return err
		}
		var bounds map[string]json.RawMessage
		_ = json.Unmarshal(occ["bounds"], &bounds)
		if err := requireKeys(bounds, fmt.Sprintf("occluders[%d].bounds", i), "min", "max"); err != nil {
			return err
		}
	}

	return nil
}

func requireKeys(m map[string]json.RawMessage, prefix string, keys ...string) error {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || string(v) == "null" {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
			return model.NewError(model.ESchema, field, "field is missing or null")
		}
	}
	return nil
}

func Validate(p *model.Project) error {
	if p == nil {
		return model.NewError(model.ESchema, "project", "nil project")
	}
	if p.SchemaVersion == "" {
		return model.NewError(model.ESchema, "schema_version", "field is missing")
	}
	t := p.Terrain
	if t.WidthCells <= 0 || t.DepthCells <= 0 {
		return model.NewError(model.ESchema, "terrain.dimensions", "width_cells and depth_cells must be positive")
	}
	if t.CellSize <= 0 {
		return model.NewError(model.ESchema, "terrain.cell_size", "must be positive")
	}
	wantHeights := (t.WidthCells + 1) * (t.DepthCells + 1)
	if len(t.Heights) != wantHeights {
		return model.NewError(model.EDim, "terrain.heights", "has %d values, want %d for %dx%d cells", len(t.Heights), wantHeights, t.WidthCells, t.DepthCells)
	}

	if len(p.Chunks) == 0 {
		return model.NewError(model.ESchema, "chunks", "at least one chunk is required")
	}
	worldW := float64(t.WidthCells) * t.CellSize
	worldD := float64(t.DepthCells) * t.CellSize
	seenIDs := map[int]bool{}
	for i, c := range p.Chunks {
		if c.ID != i+1 {
			return model.NewError(model.EDim, fmt.Sprintf("chunks[%d].id", i), "chunk IDs must be consecutive starting at 1; got %d want %d", c.ID, i+1)
		}
		if seenIDs[c.ID] {
			return model.NewError(model.EDim, "chunks.id", "duplicate id %d", c.ID)
		}
		seenIDs[c.ID] = true
		r := c.Rect
		if r.MinX < -1e-9 || r.MinZ < -1e-9 || r.MaxX > worldW+1e-9 || r.MaxZ > worldD+1e-9 {
			return model.NewError(model.EDim, fmt.Sprintf("chunks[%d].coverage", i), "coverage %+v exceeds terrain %vx%v", r, worldW, worldD)
		}
		if r.MaxX <= r.MinX || r.MaxZ <= r.MinZ {
			return model.NewError(model.EDim, fmt.Sprintf("chunks[%d].coverage", i), "coverage must have positive area: %+v", r)
		}
		if c.LoadCost <= 0 || c.MemoryBytes <= 0 || c.Complexity <= 0 {
			return model.NewError(model.ESchema, fmt.Sprintf("chunks[%d]", i), "complexity, load_cost and memory_bytes must be positive")
		}
		if len(c.SamplePoints) == 0 {
			return model.NewError(model.ESchema, fmt.Sprintf("chunks[%d].sample_points", i), "at least one sample point is required")
		}
	}

	for i := 0; i < len(p.Chunks); i++ {
		for j := i + 1; j < len(p.Chunks); j++ {
			a, b := p.Chunks[i].Rect, p.Chunks[j].Rect
			ox := minFloat(a.MaxX, b.MaxX) - maxFloat(a.MinX, b.MinX)
			oz := minFloat(a.MaxZ, b.MaxZ) - maxFloat(a.MinZ, b.MinZ)
			if ox > 1e-8 && oz > 1e-8 {
				return model.NewError(model.EDim, "chunks.coverage", "chunks %d and %d overlap by %gx%g", p.Chunks[i].ID, p.Chunks[j].ID, ox, oz)
			}
		}
	}

	var area float64
	for _, c := range p.Chunks {
		area += (c.Rect.MaxX - c.Rect.MinX) * (c.Rect.MaxZ - c.Rect.MinZ)
	}
	if diff := area - worldW*worldD; diff > 1e-6 || diff < -1e-6 {
		return model.NewError(model.EDim, "chunks.coverage", "coverage area %.6f does not equal terrain area %.6f", area, worldW*worldD)
	}

	occluderIDs := map[int]bool{}
	for _, o := range p.Occluders {
		if occluderIDs[o.ID] {
			return model.NewError(model.EDim, "occluders.id", "duplicate occluder id %d", o.ID)
		}
		occluderIDs[o.ID] = true
	}
	for _, c := range p.Chunks {
		for _, ref := range c.OccluderRefs {
			if !occluderIDs[ref] {
				return &model.Error{Code: model.ERef, Field: fmt.Sprintf("chunks[%d].occluder_refs", c.ID), Message: fmt.Sprintf("occluder reference %d does not exist", ref), Details: map[string]any{"chunk_id": c.ID, "reference": ref}}
			}
		}
	}

	if len(p.CameraTrack) == 0 {
		return model.NewError(model.ESchema, "camera_track", "at least one camera sample is required")
	}
	var lastTime int64
	for i, s := range p.CameraTrack {
		if s.Index != i {
			return model.NewError(model.EDim, fmt.Sprintf("camera_track[%d].index", i), "camera sample indices must be consecutive from 0")
		}
		if i > 0 && s.TimeMs <= lastTime {
			return model.NewError(model.EDim, "camera_track.logical_time_ms", "sample times must be strictly monotonic at index %d", i)
		}
		lastTime = s.TimeMs
		if s.FovDeg <= 0 || s.Near <= 0 || s.Far <= s.Near {
			return model.NewError(model.ESchema, fmt.Sprintf("camera_track[%d]", i), "fov, near and far must be valid")
		}
	}

	if len(p.Truth.Samples) != len(p.CameraTrack) {
		return model.NewError(model.EDim, "truth.samples", "truth sample count %d != camera count %d", len(p.Truth.Samples), len(p.CameraTrack))
	}
	for si, ts := range p.Truth.Samples {
		if ts.SampleIndex != si {
			return model.NewError(model.EDim, fmt.Sprintf("truth.samples[%d].sample_index", si), "truth indices must match camera track")
		}
		if len(ts.Chunks) != len(p.Chunks) {
			return model.NewError(model.EDim, fmt.Sprintf("truth.samples[%d].chunks", si), "truth chunk count %d != %d", len(ts.Chunks), len(p.Chunks))
		}
		for _, ct := range ts.Chunks {
			if ct.ChunkID < 1 || ct.ChunkID > len(p.Chunks) {
				return &model.Error{Code: model.ERef, Field: "truth.chunks.chunk_id", Message: "truth chunk id out of range", Details: map[string]any{"sample_index": si, "chunk_id": ct.ChunkID}}
			}
			if ct.ReferenceOccluderID != 0 && !occluderIDs[ct.ReferenceOccluderID] {
				return &model.Error{Code: model.ERef, Field: "truth.chunks.reference_occluder_id", Message: "truth references missing occluder", Details: map[string]any{"sample_index": si, "chunk_id": ct.ChunkID, "reference": ct.ReferenceOccluderID}}
			}
		}
	}

	if p.Budgets.LoadSlots <= 0 || p.Budgets.MemoryBytes <= 0 || p.Budgets.MaxResidencyDelay < 0 {
		return model.NewError(model.ESchema, "budgets", "load_slots, memory_bytes and max_residency_delay_samples are required/non-negative")
	}
	if p.Budgets.MaxParallelTasks < 1 || p.Budgets.MaxParallelTasks > 16 {
		return model.ParameterError("budgets.max_parallel_tasks", "[1,16]", "got %d", p.Budgets.MaxParallelTasks)
	}
	if p.Budgets.DepthBuffersPerTask < 16 || p.Budgets.DepthBuffersPerTask > 1_000_000 {
		return model.ParameterError("budgets.depth_buffer_entries_per_thread", "[16,1000000]", "got %d", p.Budgets.DepthBuffersPerTask)
	}
	return nil
}

func ValidateParameters(q model.QuantParameters) error {
	if q.ShadowResolution < 16 || q.ShadowResolution > 1024 {
		return model.ParameterError("parameters.shadow_resolution", "[16,1024]", "got %d", q.ShadowResolution)
	}
	if q.LightAzimuthDeg < -180 || q.LightAzimuthDeg > 180 {
		return model.ParameterError("parameters.light_azimuth_deg", "[-180,180]", "got %v", q.LightAzimuthDeg)
	}
	if q.LightElevationDeg < 10 || q.LightElevationDeg > 80 {
		return model.ParameterError("parameters.light_elevation_deg", "[10,80]", "got %v", q.LightElevationDeg)
	}
	if q.SampleSpacing <= 0 || q.SampleSpacing > 8 {
		return model.ParameterError("parameters.sample_spacing", "(0,8] world units", "got %v", q.SampleSpacing)
	}
	if q.FixedPointStep <= 0 || q.FixedPointStep >= 0.1 {
		return model.ParameterError("parameters.fixed_point_step", "(0,0.1)", "got %v", q.FixedPointStep)
	}
	if q.ViewportWidth < 160 || q.ViewportWidth > 7680 || q.ViewportHeight < 90 || q.ViewportHeight > 4320 {
		return model.ParameterError("parameters.viewport", "width [160,7680], height [90,4320]", "got %dx%d", q.ViewportWidth, q.ViewportHeight)
	}
	if q.MaxWorkers < 1 || q.MaxWorkers > 16 {
		return model.ParameterError("parameters.max_workers", "[1,16]", "got %d", q.MaxWorkers)
	}
	if q.DepthBufferLimit < 16 || q.DepthBufferLimit > 1_000_000 {
		return model.ParameterError("parameters.depth_buffer_limit", "[16,1000000]", "got %d", q.DepthBufferLimit)
	}
	return nil
}

func SortedChunkIDs(chunks []model.Chunk) []int {
	out := make([]int, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.ID)
	}
	sort.Ints(out)
	return out
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
