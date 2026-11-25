package fogkernel

import (
	"math"
	"sort"
	"sync"
	"time"
)

type Path string

const (
	PathExact      Path = "exact"
	PathPreview    Path = "preview"
	PathDiagnostic Path = "diagnostic"
)

type Budget struct {
	Workers         int `json:"workers"`
	PerWorkerBuffer int `json:"per_worker_buffer"`
}

func DefaultBudget(s *Scene) Budget {
	return Budget{Workers: s.Recipe.DefaultWorkers, PerWorkerBuffer: s.Recipe.DefaultBuffer}
}

func (b Budget) WithSceneDefaults(s *Scene) (Budget, error) {
	if b.Workers == 0 {
		b.Workers = s.Recipe.DefaultWorkers
	}
	if b.PerWorkerBuffer == 0 {
		b.PerWorkerBuffer = s.Recipe.DefaultBuffer
	}
	if b.Workers == 0 {
		b.Workers = 1
	}
	if b.PerWorkerBuffer == 0 {
		b.PerWorkerBuffer = 16
	}
	return b.Validate()
}

func (b Budget) Validate() (Budget, error) {
	if b.Workers < 1 || b.Workers > MaxDeclaredWorkers {
		return b, NewBizError(CodeBudgetWorkers, "$.budget.workers", "worker count %d outside 1..%d", b.Workers, MaxDeclaredWorkers).
			WithContext("workers", b.Workers, "min", 1, "max", MaxDeclaredWorkers)
	}
	if b.PerWorkerBuffer < 1 || b.PerWorkerBuffer > MaxDeclaredBuffer {
		return b, NewBizError(CodeBudgetBuffer, "$.budget.per_worker_buffer", "block buffer %d outside 1..%d", b.PerWorkerBuffer, MaxDeclaredBuffer).
			WithContext("per_worker_buffer", b.PerWorkerBuffer, "min", 1, "max", MaxDeclaredBuffer)
	}
	return b, nil
}

func (b Budget) Capacity() int { return b.Workers * b.PerWorkerBuffer }

// AdmitBlockBatch is the non-blocking admission boundary. It returns a stable
// E_BACKPRESSURE instead of growing an unbounded queue or dropping blocks.
func AdmitBlockBatch(b Budget, blocks int) error {
	if _, err := b.Validate(); err != nil {
		return err
	}
	if blocks < 0 {
		return NewBizError(CodeBudgetBuffer, "$.blocks", "block count must be non-negative")
	}
	if blocks > b.Capacity() {
		return NewBizError(CodeBackpressure, "$.admission", "block batch of %d exceeds bounded capacity %d", blocks, b.Capacity()).
			WithContext("requested_blocks", blocks, "capacity", b.Capacity(),
				"workers", b.Workers, "per_worker_buffer", b.PerWorkerBuffer,
				"retry_hint", "split_batch_or_increase_budget")
	}
	return nil
}

type SolveOptions struct {
	Path               Path   `json:"path"`
	Workers            int    `json:"workers"`
	Buffer             int    `json:"buffer"`
	Order              string `json:"order"`
	BatchSize          int    `json:"batch_size"`
	StrictBackpressure bool   `json:"strict_backpressure"`
}

type BlockResult struct {
	BlockIndex          int            `json:"block_index"`
	ChunkBin            int64          `json:"chunk_bin"`
	T0                  float64        `json:"t0"`
	T1                  float64        `json:"t1"`
	TauA                float64        `json:"tau_a"`
	TauS                float64        `json:"tau_s"`
	Tau                 float64        `json:"tau"`
	Albedo              float64        `json:"albedo"`
	Phase               float64        `json:"phase"`
	TBlock              float64        `json:"t_block"`
	TIn                 float64        `json:"t_in"`
	TOut                float64        `json:"t_out"`
	Energy              float64        `json:"energy"`
	Terminal            bool           `json:"terminal"`
	StopReason          string         `json:"stop_reason,omitempty"`
	PreviewSampled      bool           `json:"preview_sampled,omitempty"`
	CacheStatus         string         `json:"cache_status,omitempty"`
	InvalidationSources []string       `json:"invalidation_sources,omitempty"`
	PropagationClosure  []string       `json:"propagation_closure,omitempty"`
	VolumeIDs           []string       `json:"volume_ids"`
	Segments            []VoxelSegment `json:"segments,omitempty"`
}

type RayResult struct {
	RayIndex      int           `json:"ray_index"`
	RayID         string        `json:"ray_id"`
	CameraIndex   int           `json:"camera_index"`
	Hit           bool          `json:"hit"`
	Path          Path          `json:"path"`
	Transmittance float64       `json:"transmittance"`
	TerminalBlock int           `json:"terminal_block"`
	StopReason    string        `json:"stop_reason"`
	PlannedBlocks int           `json:"planned_blocks"`
	Blocks        []BlockResult `json:"blocks"`
	DurationNS    int64         `json:"duration_ns"`
}

type SolveReport struct {
	OK           bool        `json:"ok"`
	Path         Path        `json:"path"`
	SceneVersion string      `json:"scene_version"`
	Budget       Budget      `json:"budget"`
	Order        string      `json:"order"`
	Rays         []RayResult `json:"rays"`
}

func SolveScene(scene *Scene, opts SolveOptions) (*SolveReport, error) {
	if scene == nil {
		return nil, NewBizError(CodeEditTarget, "$.scene", "nil scene")
	}
	path := opts.Path
	if path == "" {
		path = PathExact
	}
	if path != PathExact && path != PathPreview && path != PathDiagnostic {
		return nil, NewBizError(CodeSchemaField, "$.path", "unknown render path %q", path)
	}
	budget, err := Budget{Workers: opts.Workers, PerWorkerBuffer: opts.Buffer}.WithSceneDefaults(scene)
	if err != nil {
		return nil, err
	}
	order := opts.Order
	if order == "" {
		order = "forward"
	}
	if order != "forward" && order != "reverse" {
		return nil, NewBizError(CodeSchemaField, "$.order", "order must be forward or reverse, got %q", order)
	}
	if opts.StrictBackpressure {
		totalBlocks := 0
		for _, ray := range scene.Rays {
			planned := PlanRay(scene, ray)
			totalBlocks += len(planned.Blocks)
			if len(planned.Blocks) > budget.PerWorkerBuffer {
				return nil, NewBizError(CodeBackpressure, "$.budget.per_worker_buffer",
					"ray %d needs %d block slots but per-worker buffer is %d", ray.Index, len(planned.Blocks), budget.PerWorkerBuffer).
					WithContext("ray_index", ray.Index, "planned_blocks", len(planned.Blocks), "per_worker_buffer", budget.PerWorkerBuffer)
			}
		}
		if err := AdmitBlockBatch(budget, totalBlocks); err != nil {
			return nil, err
		}
	}

	indices := make([]int, len(scene.Rays))
	for i := range indices {
		indices[i] = i
	}
	if order == "reverse" {
		sort.Sort(sort.Reverse(sort.IntSlice(indices)))
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 || batchSize > len(indices) {
		batchSize = len(indices)
	}
	results := make([]RayResult, len(scene.Rays))
	workerSem := make(chan struct{}, budget.Workers)
	blockSlots := make(chan struct{}, budget.Capacity())

	for start := 0; start < len(indices); start += batchSize {
		end := start + batchSize
		if end > len(indices) {
			end = len(indices)
		}
		var wg sync.WaitGroup
		for _, rayIndex := range indices[start:end] {
			rayIndex := rayIndex
			wg.Add(1)
			go func() {
				defer wg.Done()
				workerSem <- struct{}{}
				defer func() { <-workerSem }()
				results[rayIndex] = solveRay(scene, scene.Rays[rayIndex], path, true, blockSlots)
			}()
		}
		wg.Wait()
	}
	return &SolveReport{
		OK: true, Path: path, SceneVersion: scene.Version(), Budget: budget,
		Order: order, Rays: results,
	}, nil
}

func solveRay(scene *Scene, ray Ray, path Path, includeDiagnostic bool, blockSlots chan struct{}) RayResult {
	start := time.Now()
	plan := PlanRay(scene, ray)
	result := RayResult{
		RayIndex: ray.Index, RayID: ray.ID, CameraIndex: ray.CameraIndex,
		Path: path, TerminalBlock: -1,
	}
	if !plan.Hit || len(plan.Blocks) == 0 {
		result.Hit = false
		result.Transmittance = 1
		result.StopReason = StopNoIntersection
		result.DurationNS = time.Since(start).Nanoseconds()
		return result
	}
	result.Hit = true
	result.PlannedBlocks = len(plan.Blocks)
	physics := make([]BlockPhysics, len(plan.Blocks))
	sampled := make([]bool, len(plan.Blocks))

	if path == PathPreview {
		stride := scene.Recipe.PreviewStride
		if stride < 1 {
			stride = 2
		}
		for i := range plan.Blocks {
			if i%stride == 0 {
				acquireBlockSlot(blockSlots)
				physics[i] = ComputeBlockPhysics(scene, ray, plan.Blocks[i])
				releaseBlockSlot(blockSlots)
				sampled[i] = true
			}
		}
		for i := range plan.Blocks {
			if !sampled[i] {
				physics[i] = approximatePreviewPhysics(plan.Blocks, physics, sampled, i, scene.Recipe.ChunkMeters())
			}
		}
	} else {
		for i, bp := range plan.Blocks {
			acquireBlockSlot(blockSlots)
			physics[i] = ComputeBlockPhysics(scene, ray, bp)
			releaseBlockSlot(blockSlots)
			sampled[i] = true
		}
	}

	t := 1.0
	threshold := scene.Recipe.TerminationThreshold()
	limit := scene.Recipe.BlockSampleLimit
	for i, bp := range plan.Blocks {
		if len(result.Blocks) >= limit {
			result.StopReason = StopBlockLimit
			break
		}
		ph := physics[i]
		tBlock := math.Exp(-ph.Tau)
		energy := blockEnergy(t, ph)
		tOut := t * tBlock
		br := BlockResult{
			BlockIndex: i, ChunkBin: bp.Bin, T0: bp.T0, T1: bp.T1,
			TauA: ph.TauA, TauS: ph.TauS, Tau: ph.Tau, Albedo: ph.Albedo,
			Phase: ph.Phase, TBlock: tBlock, TIn: t, TOut: tOut, Energy: energy,
			PreviewSampled: sampled[i] && path == PathPreview,
			VolumeIDs:      ph.VolumeIDs,
		}
		if path == PathDiagnostic || includeDiagnostic && path == PathDiagnostic {
			br.Segments = append([]VoxelSegment(nil), bp.Segments...)
		}
		t = tOut
		result.Transmittance = t
		result.TerminalBlock = i
		result.Blocks = append(result.Blocks, br)
		if t < threshold {
			result.StopReason = StopThreshold
			result.Blocks[len(result.Blocks)-1].Terminal = true
			result.Blocks[len(result.Blocks)-1].StopReason = StopThreshold
			break
		}
	}
	if result.StopReason == "" {
		if len(plan.Blocks) > limit && len(result.Blocks) == limit {
			result.StopReason = StopBlockLimit
		} else {
			result.StopReason = StopCompleted
		}
	}
	result.DurationNS = time.Since(start).Nanoseconds()
	return result
}

func acquireBlockSlot(ch chan struct{}) {
	if ch == nil {
		return
	}
	ch <- struct{}{}
}

func releaseBlockSlot(ch chan struct{}) {
	if ch == nil {
		return
	}
	<-ch
}

func approximatePreviewPhysics(blocks []BlockPlan, known []BlockPhysics, sampled []bool, idx int, chunkM float64) BlockPhysics {
	prev := idx - 1
	for prev >= 0 && !sampled[prev] {
		prev--
	}
	next := idx + 1
	for next < len(blocks) && !sampled[next] {
		next++
	}
	length := blocks[idx].T1 - blocks[idx].T0
	if length <= 0 {
		length = chunkM
	}
	scaleTau := func(p BlockPhysics, blockIdx int) BlockPhysics {
		l := blocks[blockIdx].T1 - blocks[blockIdx].T0
		if l <= 1e-12 {
			l = chunkM
		}
		ratio := length / l
		p.Tau *= ratio
		p.TauA *= ratio
		p.TauS *= ratio
		return p
	}
	switch {
	case prev >= 0 && next < len(blocks):
		a := scaleTau(known[prev], prev)
		b := scaleTau(known[next], next)
		weight := float64(idx-prev) / float64(next-prev)
		mix := a
		mix.Tau = a.Tau*(1-weight) + b.Tau*weight
		mix.TauA = a.TauA*(1-weight) + b.TauA*weight
		mix.TauS = a.TauS*(1-weight) + b.TauS*weight
		mix.Albedo = a.Albedo*(1-weight) + b.Albedo*weight
		mix.Phase = a.Phase*(1-weight) + b.Phase*weight
		mix.SegmentCount = 0
		return mix
	case prev >= 0:
		return scaleTau(known[prev], prev)
	case next < len(blocks):
		return scaleTau(known[next], next)
	default:
		return BlockPhysics{Albedo: 1, Phase: 1}
	}
}

func SolveRayDirect(scene *Scene, rayIndex int, path Path) RayResult {
	return solveRay(scene, scene.Rays[rayIndex], path, true, nil)
}
