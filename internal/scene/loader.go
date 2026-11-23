package scene

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"occlbench/internal/core"
)

// Load 从工程目录读取 world.json 并执行完整结构校验。
func Load(dir string) (*World, error) {
	path := filepath.Join(dir, "world.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, core.NewError(core.EIO, "cannot read world.json", map[string]any{"path": path, "err": err.Error()})
	}
	var w World
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, core.NewError(core.ESchema, "invalid json", map[string]any{"err": err.Error()})
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Save 将工程确定性落盘（缩进 JSON）。
func (w *World) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return core.NewError(core.EIO, "mkdir failed", map[string]any{"dir": dir, "err": err.Error()})
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return core.NewError(core.EIO, "marshal failed", map[string]any{"err": err.Error()})
	}
	if err := os.WriteFile(filepath.Join(dir, "world.json"), data, 0o644); err != nil {
		return core.NewError(core.EIO, "write failed", map[string]any{"err": err.Error()})
	}
	return nil
}

// Validate 执行结构校验，错误码遵循 E_SCHEMA / E_BOX / E_REF 约定。
func (w *World) Validate() error {
	if w.SchemaVersion == "" {
		return core.NewError(core.ESchema, "missing schemaVersion", nil)
	}
	if len(w.Partitions) == 0 {
		return core.NewError(core.ESchema, "no partitions", nil)
	}

	chunkIDs := map[int]bool{}
	maxChunkID := -1
	for p := range w.Partitions {
		part := &w.Partitions[p]
		if part.ID != p {
			return core.NewError(core.ESchema, "partition id mismatch",
				map[string]any{"partition": p, "declared": part.ID})
		}
		if len(part.Chunks) == 0 {
			return core.NewError(core.ESchema, "partition has no chunks",
				map[string]any{"partition": part.ID})
		}
		for c := range part.Chunks {
			ch := &part.Chunks[c]
			loc := map[string]any{"partition": part.ID, "chunk": ch.ID, "localIndex": c}
			if ch.LocalIndex != c {
				return core.NewError(core.ESchema, "chunk localIndex mismatch", loc)
			}
			if ch.Partition != part.ID {
				return core.NewError(core.ESchema, "chunk partition mismatch", loc)
			}
			if chunkIDs[ch.ID] {
				return core.NewError(core.ERef, "duplicate chunk id", loc)
			}
			chunkIDs[ch.ID] = true
			if ch.ID > maxChunkID {
				maxChunkID = ch.ID
			}
			// E_BOX：数值有限且最小角 <= 最大角。
			if !ch.Box.Valid() {
				return core.NewError(core.EBox, "invalid chunk aabb", loc)
			}
			if ch.LoadCost <= 0 || ch.MemoryCost <= 0 || ch.Complexity <= 0 {
				return core.NewError(core.ESchema, "non-positive cost/complexity", loc)
			}
			// E_REF：遮挡体引用存在性（遮挡体表先于引用校验时无法判断下标，先记录范围）。
			for _, ref := range ch.OccluderRefs {
				if ref < 0 || ref >= len(w.Occluders) {
					return core.NewError(core.ERef, "occluder reference out of range",
						map[string]any{"partition": part.ID, "chunk": ch.ID, "ref": ref, "occluderCount": len(w.Occluders)})
				}
			}
		}
	}

	occIDs := map[int]bool{}
	for i := range w.Occluders {
		oc := &w.Occluders[i]
		if oc.ID != i {
			return core.NewError(core.ESchema, "occluder id mismatch", map[string]any{"occluder": i, "declared": oc.ID})
		}
		if occIDs[oc.ID] {
			return core.NewError(core.ERef, "duplicate occluder id", map[string]any{"occluder": oc.ID})
		}
		occIDs[oc.ID] = true
		if !oc.Box.Valid() {
			return core.NewError(core.EBox, "invalid occluder aabb", map[string]any{"occluder": oc.ID})
		}
		if !chunkIDs[oc.RefChunk] {
			return core.NewError(core.ERef, "occluder references missing chunk",
				map[string]any{"occluder": oc.ID, "refChunk": oc.RefChunk})
		}
	}

	if err := w.validateTrajectory(); err != nil {
		return err
	}
	return w.validateGroundTruth(chunkIDs, maxChunkID)
}

func (w *World) validateTrajectory() error {
	if len(w.Trajectory.Samples) == 0 {
		return core.NewError(core.ESchema, "empty trajectory", nil)
	}
	prev := math.Inf(-1)
	for i := range w.Trajectory.Samples {
		s := &w.Trajectory.Samples[i]
		loc := map[string]any{"sample": i, "t": s.T}
		if !s.Pos.IsFinite() || !s.Forward.IsFinite() || !s.Up.IsFinite() {
			return core.NewError(core.ESchema, "non-finite camera vector", loc)
		}
		if s.Forward.Length() == 0 || s.Up.Length() == 0 {
			return core.NewError(core.ESchema, "zero camera basis vector", loc)
		}
		if s.FOVy <= 0 || s.FOVy >= 180 || s.Aspect <= 0 || s.Near <= 0 || s.Far <= s.Near {
			return core.NewError(core.EPara, "invalid frustum parameter",
				map[string]any{"sample": i, "field": "fovY/aspect/near/far"})
		}
		// 采样时刻严格单调递增。
		if s.T <= prev {
			return core.NewError(core.ESchema, "trajectory time not strictly increasing", loc)
		}
		prev = s.T
	}
	return nil
}

func (w *World) validateGroundTruth(chunkIDs map[int]bool, maxChunkID int) error {
	if len(w.GroundTruth.Visible) != len(w.Trajectory.Samples) {
		return core.NewError(core.ESchema, "ground truth visible length mismatch",
			map[string]any{"visible": len(w.GroundTruth.Visible), "samples": len(w.Trajectory.Samples)})
	}
	for i, list := range w.GroundTruth.Visible {
		for _, id := range list {
			if id < 0 || id > maxChunkID || !chunkIDs[id] {
				return core.NewError(core.ERef, "ground truth visible chunk missing",
					map[string]any{"sample": i, "chunk": id})
			}
		}
	}
	for chunkID, refs := range w.GroundTruth.OccluderBasis {
		if !chunkIDs[chunkID] {
			return core.NewError(core.ERef, "occluder basis references missing chunk",
				map[string]any{"chunk": chunkID})
		}
		for _, oid := range refs {
			if oid < 0 || oid >= len(w.Occluders) {
				return core.NewError(core.ERef, "occluder basis reference out of range",
					map[string]any{"chunk": chunkID, "occluder": oid})
			}
		}
	}
	for _, sc := range w.GroundTruth.ScreenChecks {
		if sc.SampleIndex < 0 || sc.SampleIndex >= len(w.Trajectory.Samples) {
			return core.NewError(core.ERef, "screen check sample out of range",
				map[string]any{"sample": sc.SampleIndex})
		}
		if !chunkIDs[sc.ChunkID] {
			return core.NewError(core.ERef, "screen check chunk missing",
				map[string]any{"chunk": sc.ChunkID})
		}
	}
	return nil
}
