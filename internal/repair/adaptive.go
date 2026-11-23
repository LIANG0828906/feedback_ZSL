package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"meshforge/internal/errs"
	"meshforge/internal/mesh"
)

const AlgorithmVersion = "meshforge-repair/1.0.0"

type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeExact   Mode = "exact"
	ModeSpatial Mode = "spatial"
)

type Config struct {
	Mode               Mode    `json:"mode"`
	WeldTolerance      float64 `json:"weld_tolerance"`
	MinComponentPct    float64 `json:"min_component_pct"`
	PreferConvexFan    bool    `json:"prefer_convex_fan"`
	SmoothWeight       float64 `json:"smooth_weight"`
	SmoothIterations   int     `json:"smooth_iterations"`
	FineFaceLimit      int     `json:"fine_face_limit"`
	FineComponentLimit int     `json:"fine_component_limit"`
	ChunkTargetFaces   int     `json:"chunk_target_faces"`
	TimeBudgetMS       int64   `json:"time_budget_ms"`
	MemoryBudgetMB     int64   `json:"memory_budget_mb"`
	MaxParallel        int     `json:"max_parallel"`
	PerWorkerBufferMB  int64   `json:"per_worker_buffer_mb"`
}

func DefaultConfig() Config {
	return Config{
		Mode:               ModeAuto,
		WeldTolerance:      1e-9,
		MinComponentPct:    0.05,
		PreferConvexFan:    true,
		SmoothWeight:       0.52,
		SmoothIterations:   8,
		FineFaceLimit:      50000,
		FineComponentLimit: 256,
		ChunkTargetFaces:   8192,
		TimeBudgetMS:       10000,
		MemoryBudgetMB:     384,
		MaxParallel:        4,
		PerWorkerBufferMB:  32,
	}
}

func (c Config) Normalized() Config {
	d := DefaultConfig()
	if c.Mode == "" {
		c.Mode = d.Mode
	}
	if c.MinComponentPct <= 0 || c.MinComponentPct >= 1 {
		c.MinComponentPct = d.MinComponentPct
	}
	if c.FineFaceLimit <= 0 {
		c.FineFaceLimit = d.FineFaceLimit
	}
	if c.FineComponentLimit <= 0 {
		c.FineComponentLimit = d.FineComponentLimit
	}
	if c.ChunkTargetFaces <= 0 {
		c.ChunkTargetFaces = d.ChunkTargetFaces
	}
	if c.TimeBudgetMS <= 0 {
		c.TimeBudgetMS = d.TimeBudgetMS
	}
	if c.MemoryBudgetMB <= 0 {
		c.MemoryBudgetMB = d.MemoryBudgetMB
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = d.MaxParallel
	}
	if c.MaxParallel > 64 {
		c.MaxParallel = 64
	}
	if c.PerWorkerBufferMB <= 0 {
		c.PerWorkerBufferMB = d.PerWorkerBufferMB
	}
	return c
}

func (c Config) Validate() *errs.CodedError {
	if e := ValidateSmoothOptions(SmoothOptions{Weight: c.SmoothWeight, Iterations: c.SmoothIterations}); e != nil {
		return e
	}
	if c.WeldTolerance < 0 || math.IsNaN(c.WeldTolerance) || math.IsInf(c.WeldTolerance, 0) {
		return errs.New(errs.CodeTolo, "weld tolerance must be finite and non-negative")
	}
	switch c.Mode {
	case ModeAuto, ModeExact, ModeSpatial:
	default:
		return errs.New(errs.CodePara, "unknown mode %q", c.Mode)
	}
	return nil
}

func (c Config) ParameterVersion() string {
	payload := []any{
		"p1", c.Mode, c.WeldTolerance, c.MinComponentPct, c.PreferConvexFan,
		c.SmoothWeight, c.SmoothIterations, c.FineFaceLimit,
		c.FineComponentLimit, c.ChunkTargetFaces, c.TimeBudgetMS,
		c.MemoryBudgetMB, c.PerWorkerBufferMB,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "p1-" + hex.EncodeToString(sum[:8])
}

type ModeCandidate struct {
	Mode           Mode     `json:"mode"`
	Feasible       bool     `json:"feasible"`
	EstimateTimeMS int64    `json:"estimate_time_ms"`
	EstimateMemMB  int64    `json:"estimate_memory_mb"`
	Reasons        []string `json:"reasons"`
}

type ChunkPlan struct {
	Grid              int     `json:"grid"`
	Edge              float64 `json:"edge"`
	TargetFaces       int     `json:"target_faces"`
	MaxFacesPerChunk  int     `json:"max_faces_per_chunk"`
	WorkerBufferFaces int     `json:"worker_buffer_faces"`
	BoundaryOverlap   int     `json:"boundary_overlap"`
}

type Decision struct {
	Requested         Mode            `json:"requested"`
	Selected          Mode            `json:"selected"`
	FaceCount         int             `json:"face_count"`
	ComponentCount    int             `json:"component_count"`
	Rule              string          `json:"rule"`
	Reasons           []string        `json:"reasons"`
	Candidates        []ModeCandidate `json:"candidates"`
	Chunk             ChunkPlan       `json:"chunk"`
	TimeBudgetMS      int64           `json:"time_budget_ms"`
	MemoryBudgetMB    int64           `json:"memory_budget_mb"`
	PerWorkerBufferMB int64           `json:"per_worker_buffer_mb"`
	MaxParallel       int             `json:"max_parallel"`
}

func Decide(m mesh.Mesh, cfg Config) (Decision, *errs.CodedError) {
	cfg = cfg.Normalized()
	if e := cfg.Validate(); e != nil {
		return Decision{}, e
	}
	comps := mesh.FaceComponents(m, mesh.ComponentAllSharedEdges)
	faceCount := len(m.Faces)
	compCount := len(comps)
	exactTime := int64(math.Ceil(float64(faceCount)*0.045 + float64(compCount)*2.0))
	exactMem := int64(math.Ceil(float64(faceCount)*1600/1024/1024)) + 8
	approxTime := int64(math.Ceil(float64(faceCount)*0.038 + float64(compCount)*1.5))
	approxMem := int64(math.Ceil(float64(faceCount)*2200/1024/1024)) + 8
	exact := ModeCandidate{Mode: ModeExact, EstimateTimeMS: exactTime, EstimateMemMB: exactMem}
	approx := ModeCandidate{Mode: ModeSpatial, EstimateTimeMS: approxTime, EstimateMemMB: approxMem}

	if faceCount <= cfg.FineFaceLimit {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("faces %d <= fine limit %d", faceCount, cfg.FineFaceLimit))
	} else {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("faces %d > fine limit %d", faceCount, cfg.FineFaceLimit))
	}
	if compCount <= cfg.FineComponentLimit {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("components %d <= fine limit %d", compCount, cfg.FineComponentLimit))
	} else {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("components %d > fine limit %d", compCount, cfg.FineComponentLimit))
	}
	if exactTime <= cfg.TimeBudgetMS {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("estimated %dms <= %dms", exactTime, cfg.TimeBudgetMS))
	} else {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("estimated %dms > %dms", exactTime, cfg.TimeBudgetMS))
	}
	if exactMem <= cfg.MemoryBudgetMB {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("estimated %dMiB <= %dMiB", exactMem, cfg.MemoryBudgetMB))
	} else {
		exact.Reasons = append(exact.Reasons, fmt.Sprintf("estimated %dMiB > %dMiB", exactMem, cfg.MemoryBudgetMB))
	}
	exact.Feasible = exactTime <= cfg.TimeBudgetMS && exactMem <= cfg.MemoryBudgetMB
	if cfg.Mode == ModeAuto {
		exact.Feasible = exact.Feasible &&
			faceCount <= cfg.FineFaceLimit && compCount <= cfg.FineComponentLimit
	}

	if approxTime <= cfg.TimeBudgetMS {
		approx.Reasons = append(approx.Reasons, fmt.Sprintf("estimated %dms <= %dms", approxTime, cfg.TimeBudgetMS))
	} else {
		approx.Reasons = append(approx.Reasons, fmt.Sprintf("estimated %dms > %dms", approxTime, cfg.TimeBudgetMS))
	}
	if approxMem <= cfg.MemoryBudgetMB {
		approx.Reasons = append(approx.Reasons, fmt.Sprintf("estimated %dMiB <= %dMiB", approxMem, cfg.MemoryBudgetMB))
	} else {
		approx.Reasons = append(approx.Reasons, fmt.Sprintf("estimated %dMiB > %dMiB", approxMem, cfg.MemoryBudgetMB))
	}
	approx.Feasible = approxTime <= cfg.TimeBudgetMS && approxMem <= cfg.MemoryBudgetMB

	selected := Mode("")
	var rule string
	switch cfg.Mode {
	case ModeExact:
		if exact.Feasible {
			selected = ModeExact
			rule = "explicit exact mode satisfies hard time/memory budgets; auto face/component thresholds are bypassed"
		}
	case ModeSpatial:
		if approx.Feasible {
			selected = ModeSpatial
			rule = "explicit spatial mode satisfies hard time/memory budgets"
		}
	case ModeAuto:
		if exact.Feasible {
			selected = ModeExact
			rule = "exact preferred when both candidates are feasible; otherwise first feasible policy candidate"
		} else if approx.Feasible {
			selected = ModeSpatial
			rule = "exact is not admissible under declared limits; spatial chunk mode selected"
		}
	}
	if selected == "" {
		e := errs.New(errs.CodeScale, "no repair mode is feasible under the current budgets")
		e.Item = "mode_feasibility"
		e.Left = exact
		e.Right = approx
		e.Tolerance = cfg
		return Decision{}, e
	}

	chunk := ChunkPlan{}
	if selected == ModeSpatial {
		grid := int(math.Ceil(math.Cbrt(float64(faceCount) / float64(max(1, cfg.ChunkTargetFaces)))))
		if grid < 1 {
			grid = 1
		}
		bufferFaces := int(cfg.PerWorkerBufferMB * 1024 * 1024 / 420)
		if bufferFaces < cfg.ChunkTargetFaces {
			bufferFaces = cfg.ChunkTargetFaces
		}
		maxPerChunk := int(math.Ceil(float64(faceCount) / math.Pow(float64(grid), 3)))
		for maxPerChunk > bufferFaces {
			grid++
			maxPerChunk = int(math.Ceil(float64(faceCount) / math.Pow(float64(grid), 3)))
		}
		min, maxV := m.Bounds()
		diag := maxV.Sub(min).Len()
		chunk = ChunkPlan{
			Grid:              grid,
			Edge:              diag / float64(grid),
			TargetFaces:       cfg.ChunkTargetFaces,
			MaxFacesPerChunk:  maxPerChunk,
			WorkerBufferFaces: bufferFaces,
			BoundaryOverlap:   1,
		}
	}
	d := Decision{
		Requested:         cfg.Mode,
		Selected:          selected,
		FaceCount:         faceCount,
		ComponentCount:    compCount,
		Rule:              rule,
		Candidates:        []ModeCandidate{exact, approx},
		Chunk:             chunk,
		TimeBudgetMS:      cfg.TimeBudgetMS,
		MemoryBudgetMB:    cfg.MemoryBudgetMB,
		PerWorkerBufferMB: cfg.PerWorkerBufferMB,
		MaxParallel:       cfg.MaxParallel,
	}
	d.Reasons = append(d.Reasons,
		fmt.Sprintf("faces=%d", faceCount),
		fmt.Sprintf("components=%d", compCount),
		"candidate order=exact,spatial",
	)
	sort.Slice(d.Candidates, func(i, j int) bool { return d.Candidates[i].Mode < d.Candidates[j].Mode })
	return d, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
