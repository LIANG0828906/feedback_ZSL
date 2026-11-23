package retarget

import (
	"fmt"
	"math"
	"sort"

	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

func ValidateConfig(cfg model.Config) error {
	switch cfg.Mode {
	case "auto", "exact", "sampled":
	default:
		return model.NewError(model.ErrParam, cfg.Mode, "mode", "mode must be auto, exact or sampled")
	}
	if cfg.MaxIterations <= 0 || cfg.MaxIterations > 256 {
		return model.NewError(model.ErrParam, "config", "max_iterations", "must be in [1,256]")
	}
	if cfg.SmoothWindow < 1 || cfg.SmoothWindow > cfg.Budget.MaxSmoothWindow {
		return model.NewError(model.ErrParam, "config", "smooth_window", fmt.Sprintf("must be in [1,%d]", cfg.Budget.MaxSmoothWindow))
	}
	if cfg.Budget.MinSmoothWindow < 1 || cfg.Budget.MaxSmoothWindow < cfg.Budget.MinSmoothWindow {
		return model.NewError(model.ErrParam, "config", "budget.smooth_window", "min_smooth_window must be >=1 and <= max_smooth_window")
	}
	if cfg.Budget.MaxExactFrames < 1 || cfg.Budget.MaxExactJoints < 1 || cfg.Budget.MaxExactWork < 1 || cfg.Budget.MaxSolveFrames < 3 {
		return model.NewError(model.ErrParam, "config", "budget", "all budget limits must be positive and max_solve_frames >= 3")
	}
	if cfg.AngleToleranceDeg <= 0 || math.IsNaN(cfg.AngleToleranceDeg) || math.IsInf(cfg.AngleToleranceDeg, 0) {
		return model.NewError(model.ErrParam, "config", "angle_tolerance_deg", "must be finite and positive")
	}
	if cfg.ReachTolerance <= 0 || math.IsNaN(cfg.ReachTolerance) || math.IsInf(cfg.ReachTolerance, 0) {
		return model.NewError(model.ErrParam, "config", "reach_tolerance", "must be finite and positive")
	}
	if cfg.ContactDamping <= 0 || cfg.ContactDamping > 1 || math.IsNaN(cfg.ContactDamping) {
		return model.NewError(model.ErrParam, "config", "contact_damping", "must be in (0,1]")
	}
	if math.IsNaN(cfg.GroundY) || math.IsInf(cfg.GroundY, 0) {
		return model.NewError(model.ErrParam, "config", "ground_y", "must be finite")
	}
	return nil
}

func ChoosePlan(clip model.Clip, target model.TargetSkeleton, cfg model.Config) (model.Plan, error) {
	if err := ValidateConfig(cfg); err != nil {
		return model.Plan{}, err
	}
	frameCount := len(clip.Frames)
	jointCount := len(target.Joints)
	work := frameCount * jointCount
	plan := model.Plan{
		RequestedMode: cfg.Mode,
		FrameCount:    frameCount, JointCount: jointCount, WorkUnits: work,
		BudgetWork: cfg.Budget.MaxExactWork, Stride: 1,
		SmoothWindow: cfg.SmoothWindow, Interpolation: "none",
	}
	mode := cfg.Mode
	if mode == "auto" {
		switch {
		case cfg.Preview:
			mode = "sampled"
			plan.Reason = append(plan.Reason, "preview requested: exact per-frame solve disabled to bound latency")
		case frameCount > cfg.Budget.MaxExactFrames:
			mode = "sampled"
			plan.Reason = append(plan.Reason, fmt.Sprintf("frame_count %d > max_exact_frames %d", frameCount, cfg.Budget.MaxExactFrames))
		case jointCount > cfg.Budget.MaxExactJoints:
			mode = "sampled"
			plan.Reason = append(plan.Reason, fmt.Sprintf("joint_count %d > max_exact_joints %d", jointCount, cfg.Budget.MaxExactJoints))
		case work > cfg.Budget.MaxExactWork:
			mode = "sampled"
			plan.Reason = append(plan.Reason, fmt.Sprintf("work_units %d > max_exact_work %d", work, cfg.Budget.MaxExactWork))
		default:
			mode = "exact"
			plan.Reason = append(plan.Reason,
				fmt.Sprintf("frame_count %d <= %d", frameCount, cfg.Budget.MaxExactFrames),
				fmt.Sprintf("joint_count %d <= %d", jointCount, cfg.Budget.MaxExactJoints),
				fmt.Sprintf("work_units %d <= %d", work, cfg.Budget.MaxExactWork),
			)
		}
	} else {
		plan.Reason = append(plan.Reason, "mode explicitly requested by caller")
	}
	plan.Mode = mode
	if mode == "exact" {
		plan.Strategy = "exact_per_frame"
		plan.SolveFrames = frameCount
		plan.Interpolation = "none"
		for i := 0; i < frameCount; i++ {
			plan.SolveFrameIndices = append(plan.SolveFrameIndices, i)
		}
		return plan, nil
	}

	plan.Strategy = "downsample_window_interpolate"
	maxSolve := cfg.Budget.MaxSolveFrames
	if cfg.Preview {
		if maxSolve > 48 {
			maxSolve = 48
			plan.Reason = append(plan.Reason, "preview reduced max solve frames to 48")
		}
	}
	stride := 1
	if frameCount > 1 {
		stride = int(math.Ceil(float64(frameCount-1) / float64(maxSolve-1)))
	}
	if stride < 1 {
		stride = 1
	}
	set := map[int]bool{0: true, frameCount - 1: true}
	for i := 0; i < frameCount; i += stride {
		set[i] = true
	}
	for _, spans := range sample.ContactIntervals(clip.Frames, sortedFeet(target)) {
		for _, span := range spans {
			set[span.Start] = true
			set[span.End] = true
		}
	}
	for index := range set {
		plan.SolveFrameIndices = append(plan.SolveFrameIndices, index)
	}
	sort.Ints(plan.SolveFrameIndices)
	plan.Stride = stride
	plan.SolveFrames = len(plan.SolveFrameIndices)
	plan.Interpolation = "quaternion_slerp + vector_lerp"
	plan.Reason = append(plan.Reason,
		fmt.Sprintf("stride=%d derived from ceil((frames-1)/(max_solve_frames-1))", stride),
		fmt.Sprintf("contact boundaries retained: %d solve frames (including 0 and %d)", plan.SolveFrames, frameCount-1),
	)
	if cfg.SmoothWindow > 5 {
		short := 5
		if short < cfg.Budget.MinSmoothWindow {
			short = cfg.Budget.MinSmoothWindow
		}
		if short%2 == 0 {
			short--
		}
		if short < cfg.Budget.MinSmoothWindow {
			short = cfg.Budget.MinSmoothWindow
		}
		plan.SmoothWindow = short
		plan.ShortenedSmoothing = true
		plan.Reason = append(plan.Reason, fmt.Sprintf("smoothing window shortened %d -> %d to limit sampled-mode lag and memory", cfg.SmoothWindow, short))
	}
	if plan.SolveFrames > maxSolve*2 {
		return model.Plan{}, model.NewError(model.ErrParam, "config", "budget.max_solve_frames", "contact boundaries make sampled solve budget infeasible")
	}
	return plan, nil
}

func sortedFeet(target model.TargetSkeleton) []string {
	feet := make([]string, 0, len(target.ContactJoints))
	for foot := range target.ContactJoints {
		feet = append(feet, foot)
	}
	sort.Strings(feet)
	return feet
}
