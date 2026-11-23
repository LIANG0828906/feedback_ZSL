package stage

import "fmt"

type Params struct {
	WindowMS             int64   `json:"window_ms"`
	PreviewFPS           int     `json:"preview_fps"`
	MaxExactCues         int     `json:"max_exact_cues"`
	MaxExactOps          int64   `json:"max_exact_ops"`
	MaxWorkers           int     `json:"max_workers"`
	PerWorkerBufferBytes int64   `json:"per_worker_buffer_bytes"`
	MemoryBudgetBytes    int64   `json:"memory_budget_bytes"`
	ColorTolerance       float64 `json:"color_tolerance"`
	PositionTolerance    float64 `json:"position_tolerance"`
	OtherTolerance       float64 `json:"other_tolerance"`
	ForceMode            Mode    `json:"force_mode,omitempty"`
}

func DefaultParams() Params {
	return Params{
		PreviewFPS:           24,
		MaxExactCues:         8000,
		MaxExactOps:          2_000_000,
		MaxWorkers:           4,
		PerWorkerBufferBytes: 16 << 20,
		ColorTolerance:       DefaultTolerance().Color,
		PositionTolerance:    DefaultTolerance().Position,
		OtherTolerance:       DefaultTolerance().Intensity,
	}
}

func (p Params) WithDefaults(project *Project) Params {
	defaults := DefaultParams()
	if p.WindowMS == 0 && project != nil {
		p.WindowMS = project.WindowMS
	}
	if p.PreviewFPS == 0 {
		p.PreviewFPS = defaults.PreviewFPS
	}
	if p.MaxExactCues == 0 {
		p.MaxExactCues = defaults.MaxExactCues
	}
	if p.MaxExactOps == 0 {
		p.MaxExactOps = defaults.MaxExactOps
	}
	if p.MaxWorkers == 0 {
		p.MaxWorkers = defaults.MaxWorkers
	}
	if p.PerWorkerBufferBytes == 0 {
		p.PerWorkerBufferBytes = defaults.PerWorkerBufferBytes
	}
	if p.MemoryBudgetBytes == 0 {
		p.MemoryBudgetBytes = p.PerWorkerBufferBytes * int64(p.MaxWorkers)
	}
	if p.ColorTolerance == 0 {
		p.ColorTolerance = defaults.ColorTolerance
	}
	if p.PositionTolerance == 0 {
		p.PositionTolerance = defaults.PositionTolerance
	}
	if p.OtherTolerance == 0 {
		p.OtherTolerance = defaults.OtherTolerance
	}
	return p
}

func (p Params) Validate() error {
	if p.WindowMS <= 0 {
		return newError(CodeParameter, "window_ms must be positive", "params", "window_ms")
	}
	if p.PreviewFPS <= 0 || p.PreviewFPS > 240 {
		return withDetail(newError(CodeParameter, "preview_fps must be 1..240", "params", "preview_fps"), ErrorDetail{Actual: fmt.Sprint(p.PreviewFPS)})
	}
	if p.MaxExactCues <= 0 {
		return newError(CodeParameter, "max_exact_cues must be positive", "params", "max_exact_cues")
	}
	if p.MaxExactOps <= 0 {
		return newError(CodeParameter, "max_exact_ops must be positive", "params", "max_exact_ops")
	}
	if p.MaxWorkers <= 0 || p.MaxWorkers > 128 {
		return newError(CodeParameter, "max_workers must be 1..128", "params", "max_workers")
	}
	if p.PerWorkerBufferBytes <= 0 {
		return newError(CodeParameter, "per_worker_buffer_bytes must be positive", "params", "per_worker_buffer_bytes")
	}
	if p.MemoryBudgetBytes <= 0 {
		return newError(CodeParameter, "memory_budget_bytes must be positive", "params", "memory_budget_bytes")
	}
	if p.ColorTolerance < 0 || p.PositionTolerance < 0 || p.OtherTolerance < 0 {
		return newError(CodeParameter, "tolerances must be non-negative", "params", "tolerance")
	}
	switch p.ForceMode {
	case "", ModeExact, ModeMerged:
	default:
		return withDetail(newError(CodeParameter, "unsupported forced mode", "params", "force_mode"), ErrorDetail{Actual: string(p.ForceMode)})
	}
	return nil
}

func SelectMode(project *Project, params Params) ModeDecision {
	params = params.WithDefaults(project)
	lights := len(project.Lights)
	cues := len(project.Cues)
	frames := FrameCount(project.DurationMS, project.FPS)
	exactOps := int64(cues*lights + frames*lights)
	durationSeconds := float64(project.DurationMS) / 1000
	previewCapacity := int64((durationSeconds*float64(params.PreviewFPS) + 1) * float64(lights) * 8)
	decision := ModeDecision{
		Mode:            ModeExact,
		CueCount:        cues,
		LightCount:      lights,
		FrameCount:      frames,
		EstimatedOps:    exactOps,
		PreviewCapacity: previewCapacity,
		MaxExactCues:    params.MaxExactCues,
		MaxExactOps:     params.MaxExactOps,
		PreviewFPS:      params.PreviewFPS,
	}
	if params.ForceMode != "" {
		decision.Mode = params.ForceMode
		decision.Forced = true
		decision.Reasons = append(decision.Reasons, "mode forced by params.force_mode="+string(params.ForceMode))
		return decision
	}
	if cues > params.MaxExactCues {
		decision.Mode = ModeMerged
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("cue_count=%d exceeds exact cue limit=%d", cues, params.MaxExactCues))
	}
	if exactOps > params.MaxExactOps {
		decision.Mode = ModeMerged
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("estimated_ops=%d exceeds exact operation budget=%d", exactOps, params.MaxExactOps))
	}
	if exactOps > previewCapacity {
		decision.Mode = ModeMerged
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("estimated_ops=%d exceeds preview_fps capacity=%d at %d fps", exactOps, previewCapacity, params.PreviewFPS))
	}
	if len(decision.Reasons) == 0 {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("cue_count=%d, estimated_ops=%d within exact budget=%d and preview capacity=%d",
				cues, exactOps, params.MaxExactOps, previewCapacity))
	}
	return decision
}
