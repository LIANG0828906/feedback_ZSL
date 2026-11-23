package morph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

// DecideQuality selects fine or chunked-approximation mode from scene scale
// and the memory budget. The decision is deterministic and fully recorded.
func DecideQuality(scene *Scene, cfg Config) (Quality, error) {
	if scene == nil {
		return Quality{}, newError(CodeDecode, "scene is nil")
	}
	if cfg.Adaptive.MemoryBudgetBytes <= 0 {
		return Quality{}, withField(newError(CodeParam, "memory budget must be positive"), "memory_budget_bytes")
	}
	baseGrid := cfg.Render.Grid
	if cfg.Adaptive.BaseGrid > 0 {
		baseGrid = cfg.Adaptive.BaseGrid
	}
	if baseGrid < 1 || baseGrid > 8 {
		return Quality{}, withField(newError(CodeParam, "base grid must be in [1,8]"), "grid")
	}
	baseTol := cfg.Eval.MidTolerance
	if baseTol <= 0 {
		baseTol = cfg.Adaptive.BaseMidTolerance
	}
	if !finite(baseTol) || baseTol <= 0 {
		return Quality{}, withField(newError(CodeParam, "midpoint tolerance must be positive"), "mid_tolerance")
	}
	frameTol := cfg.Adaptive.BaseFrameTolerance
	if frameTol <= 0 {
		frameTol = 0.2
	}
	keyframeCount := 0
	for _, p := range scene.Paths {
		keyframeCount += len(p.Keyframes)
	}
	work := int64(len(scene.Paths)) * int64(keyframeCount) * int64(len(scene.SampleTimes))
	extent := scene.CoordMax - scene.CoordMin
	px := int64(math.Ceil(extent*cfg.Eval.Scale)) + int64(2*cfg.Render.PaddingPixels)
	if px < 1 {
		px = 1
	}
	if px > int64(cfg.Render.MaxFramePixels) {
		px = int64(cfg.Render.MaxFramePixels)
	}
	concurrent := int64(cfg.Runtime.Workers)
	if concurrent < 1 {
		concurrent = 1
	}
	// canvas float64 plus path component masks and color arrays.
	perFrame := px * px * 32
	est := perFrame*concurrent + int64(len(scene.Paths))*int64(len(scene.Paths[0].Keyframes))*1024
	q := Quality{
		Mode: "fine", MidTolerance: baseTol, FrameTolerance: frameTol, Grid: baseGrid,
		ChunkSize: cfg.Adaptive.BaseChunkSize, MemoryBudget: cfg.Adaptive.MemoryBudgetBytes,
		EstimatedBytes: est, WorkUnits: work, ForcedFine: cfg.Adaptive.ForceFine,
	}
	if q.ChunkSize <= 0 {
		q.ChunkSize = 32
	}
	if cfg.Runtime.MaxChunkPaths > 0 && q.ChunkSize > cfg.Runtime.MaxChunkPaths {
		q.ChunkSize = cfg.Runtime.MaxChunkPaths
	}
	q.Reasons = append(q.Reasons, fmt.Sprintf("work_units=%d threshold=%d", work, cfg.Adaptive.WorkThreshold))
	q.Reasons = append(q.Reasons, fmt.Sprintf("estimated_peak_bytes=%d budget_bytes=%d", est, cfg.Adaptive.MemoryBudgetBytes))
	if cfg.Adaptive.ForceFine {
		q.Reasons = append(q.Reasons, "forced_fine requested; fine parameters retained and consistency thresholds unchanged")
		return q, nil
	}
	if work <= cfg.Adaptive.WorkThreshold && est <= cfg.Adaptive.MemoryBudgetBytes {
		q.Reasons = append(q.Reasons, "fine mode selected: work and memory estimates fit budget")
		return q, nil
	}
	q.Mode = "approx"
	q.MidTolerance = baseTol * 4
	q.FrameTolerance = frameTol * 2.5
	q.Grid = maxInt(1, baseGrid/2)
	q.ChunkSize = maxInt(1, q.ChunkSize/2)
	if cfg.Runtime.MaxChunkPaths > 0 && q.ChunkSize > cfg.Runtime.MaxChunkPaths {
		q.ChunkSize = cfg.Runtime.MaxChunkPaths
	}
	if work > cfg.Adaptive.WorkThreshold {
		q.Reasons = append(q.Reasons, fmt.Sprintf("work threshold exceeded (%d > %d): midpoint tolerance relaxed x4 and grid reduced", work, cfg.Adaptive.WorkThreshold))
	}
	if est > cfg.Adaptive.MemoryBudgetBytes {
		q.Reasons = append(q.Reasons, fmt.Sprintf("memory budget exceeded (%d > %d): chunk size reduced to %d", est, cfg.Adaptive.MemoryBudgetBytes, q.ChunkSize))
	}
	q.Reasons = append(q.Reasons, fmt.Sprintf("approx parameters: mid_tolerance=%.6g frame_tolerance=%.6g grid=%d chunk=%d", q.MidTolerance, q.FrameTolerance, q.Grid, q.ChunkSize))
	return q, nil
}

func (q Quality) Version() string {
	type versionPayload struct {
		Mode           string
		MidTolerance   float64
		FrameTolerance float64
		Grid           int
		ChunkSize      int
	}
	data, _ := json.Marshal(versionPayload{q.Mode, q.MidTolerance, q.FrameTolerance, q.Grid, q.ChunkSize})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// ConfigVersion binds all algorithmic parameters that can affect pixels.
func ConfigVersion(cfg Config) string {
	type versionPayload struct {
		Algorithm string
		Eval      EvalConfig
		Render    RenderConfig
	}
	data, _ := json.Marshal(versionPayload{AlgorithmVersion, cfg.Eval, cfg.Render})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func SceneStyleVersion(scene *Scene) string {
	type stylePayload struct {
		Paths []Path
	}
	data, _ := json.Marshal(stylePayload{scene.Paths})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
