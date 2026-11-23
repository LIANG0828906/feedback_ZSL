package gait

import (
	"errors"
	"fmt"
	"strings"
)

func ValidateDataset(ds Dataset) error {
	if ds.SchemaVersion != DatasetSchemaVersion {
		return errAt(CodeSchema, fmt.Sprintf("schema_version must be %q", DatasetSchemaVersion), 0)
	}
	if err := ValidateParams(ds.Params); err != nil {
		return err
	}
	if len(ds.Sources) == 0 {
		return errAt(CodeSchema, "sources must not be empty", 0)
	}
	sourceIDs := make(map[string]bool)
	sourceByID := make(map[string]SourceLoop)
	for _, source := range ds.Sources {
		if err := validateSource(source, ds.Params); err != nil {
			return err
		}
		if sourceIDs[source.ID] {
			return errAt(CodeSchema, fmt.Sprintf("duplicate source id %q", source.ID), 0)
		}
		sourceIDs[source.ID] = true
		sourceByID[source.ID] = source
	}
	phaseIDs := make(map[string]bool)
	phaseBySource := make(map[string]PhaseTrack)
	for _, track := range ds.Phases {
		if err := validatePhaseTrack(track); err != nil {
			var app *AppError
			if errors.As(err, &app) && app.Cycle < 0 && app.Frame >= 0 {
				if source, ok := sourceByID[track.SourceID]; ok {
					app.Cycle = cycleForFrame(source.Cycles, app.Frame)
				}
			}
			return err
		}
		if phaseIDs[track.SourceID] {
			return errAt(CodePhase, fmt.Sprintf("duplicate phase track for source %q", track.SourceID), 0)
		}
		phaseIDs[track.SourceID] = true
		phaseBySource[track.SourceID] = track
	}
	truthIDs := make(map[string]bool)
	truthBySource := make(map[string]Truth)
	for _, truth := range ds.Truths {
		if err := validateTruth(truth); err != nil {
			return err
		}
		if truthIDs[truth.SourceID] {
			return errAt(CodeSchema, fmt.Sprintf("duplicate truth for source %q", truth.SourceID), 0)
		}
		truthIDs[truth.SourceID] = true
		truthBySource[truth.SourceID] = truth
	}
	for _, source := range ds.Sources {
		id := source.ID
		if !phaseIDs[id] {
			return errAt(CodePhase, fmt.Sprintf("phase track for source %q is missing", id), 0)
		}
		if !truthIDs[id] {
			return errAt(CodeSchema, fmt.Sprintf("truth for source %q is missing", id), 0)
		}
		track := phaseBySource[id]
		if len(track.Frames) != source.FrameCount {
			frame := min(len(track.Frames), source.FrameCount)
			e := errAt(CodePhase, fmt.Sprintf("phase coverage for source %q has %d frames, expected %d",
				id, len(track.Frames), source.FrameCount), frame)
			e.Cycle = cycleForFrame(source.Cycles, frame)
			if e.Cycle < 0 && len(source.Cycles) > 0 {
				e.Cycle = len(source.Cycles) - 1
			}
			return e
		}
		truth := truthBySource[id]
		if truth.FrameCount != source.FrameCount {
			e := errAt(CodeSchema, fmt.Sprintf("truth frame_count %d for source %q does not match %d",
				truth.FrameCount, id, source.FrameCount), source.FrameCount)
			e.Field = "truth.frame_count"
			return e
		}
		if truth.CycleCount != len(source.Cycles) {
			e := errCycle(CodeSchema, fmt.Sprintf("truth cycle_count %d for source %q does not match %d",
				truth.CycleCount, id, len(source.Cycles)), min(truth.CycleCount, len(source.Cycles)))
			e.Field = "truth.cycle_count"
			return e
		}
	}
	targetIDs := make(map[string]bool)
	for _, target := range ds.Targets {
		if err := validateTarget(target); err != nil {
			return err
		}
		if targetIDs[target.ID] {
			return errAt(CodeSchema, fmt.Sprintf("duplicate target id %q", target.ID), 0)
		}
		targetIDs[target.ID] = true
	}
	if len(ds.Targets) == 0 {
		return errAt(CodeSchema, "targets must not be empty", 0)
	}
	return nil
}

func ValidateParams(params Params) error {
	para := func(field, reason string) error {
		e := appErr(CodePara, reason)
		e.Field = field
		return e
	}
	if strings.TrimSpace(params.ParameterVersion) == "" {
		e := errAt(CodeSchema, "parameter_version is required", 0)
		e.Field = "parameter_version"
		return e
	}
	if strings.TrimSpace(params.AlgorithmVersion) == "" {
		e := errAt(CodeSchema, "algorithm_version is required", 0)
		e.Field = "algorithm_version"
		return e
	}
	if !isFinite(params.ContactHeight) || params.ContactHeight <= 0 {
		return para("contact_height", "contact_height must be finite and greater than zero")
	}
	if !isFinite(params.SwingHeightMin) || params.SwingHeightMin <= params.ContactHeight {
		return para("swing_height_min", "swing_height_min must be finite and greater than contact_height")
	}
	if !isFinite(params.SwingSpeedMin) || params.SwingSpeedMin < 0 {
		return para("swing_speed_min", "swing_speed_min must be finite and non-negative")
	}
	if !isFinite(params.SwingLiftScale) || params.SwingLiftScale < 0.1 || params.SwingLiftScale > 4 {
		if params.SwingLiftScale != 0 {
			return para("swing_lift_scale", "swing_lift_scale must be zero or finite in [0.1, 4]")
		}
	}
	if !isFinite(params.StanceSlideMax) || params.StanceSlideMax < 0 {
		return para("stance_slide_max", "stance_slide_max must be finite and non-negative")
	}
	if !isFinite(params.FootJumpMax) || params.FootJumpMax <= 0 {
		return para("foot_jump_max", "foot_jump_max must be finite and greater than zero")
	}
	if params.LandingToleranceFrames < 0 {
		return para("landing_tolerance_frames", "landing_tolerance_frames must be non-negative")
	}
	if !isFinite(params.PhaseRatioTolerance) || params.PhaseRatioTolerance < 0 || params.PhaseRatioTolerance >= 1 {
		return para("phase_ratio_tolerance", "phase_ratio_tolerance must be in [0, 1)")
	}
	if params.MaxParallelism < 1 {
		return para("max_parallelism", "max_parallelism must be at least one")
	}
	if params.CycleBufferLimit < 4 {
		return para("cycle_buffer_limit", "cycle_buffer_limit must be at least four")
	}
	return nil
}

func validateSource(source SourceLoop, params Params) error {
	if strings.TrimSpace(source.ID) == "" {
		e := errAt(CodeSchema, "source.id is required", 0)
		e.Field = "source.id"
		return e
	}
	if source.FrameCount <= 0 {
		e := errAt(CodeSchema, "frame_count must be positive", 0)
		e.Field = "frame_count"
		return e
	}
	if err := validateSkeleton(source.Skeleton); err != nil {
		return err
	}
	if !isFinite(source.Skeleton.GroundPlane) {
		e := errAt(CodeSchema, "skeleton.ground_plane must be finite", 0)
		e.Field = "skeleton.ground_plane"
		return e
	}
	if len(source.Root) != source.FrameCount {
		frame := len(source.Root)
		e := errAt(CodeSchema, fmt.Sprintf("root has %d frames, expected %d", len(source.Root), source.FrameCount), frame)
		e.Field = "root"
		return e
	}
	for frame, root := range source.Root {
		if err := validateFiniteVec(root, "root", frame); err != nil {
			return err
		}
	}
	if err := validateCycles(source); err != nil {
		return err
	}
	if err := validateLegs(source.Skeleton, source.Legs); err != nil {
		return err
	}
	footSet := make(map[string]bool)
	for _, foot := range source.Feet {
		if strings.TrimSpace(foot.Foot) == "" {
			e := errAt(CodeSchema, "foot id is required", 0)
			e.Field = "feet.foot"
			return e
		}
		if footSet[foot.Foot] {
			return errAt(CodeSchema, fmt.Sprintf("duplicate foot track %q", foot.Foot), 0)
		}
		footSet[foot.Foot] = true
		if len(foot.Frames) != source.FrameCount {
			frame := len(foot.Frames)
			e := errAt(CodePhase, fmt.Sprintf("foot %s has %d frames, expected %d", foot.Foot, len(foot.Frames), source.FrameCount), frame)
			e.Foot = foot.Foot
			e.Field = "feet.frames"
			return e
		}
		for frame, sample := range foot.Frames {
			if err := validateFiniteVec(sample.Position, "feet.position", frame); err != nil {
				e := err.(*AppError)
				e.Foot = foot.Foot
				return e
			}
			if err := validateFiniteVec(sample.Velocity, "feet.velocity", frame); err != nil {
				e := err.(*AppError)
				e.Foot = foot.Foot
				return e
			}
			if frame > 0 {
				delta := sample.Position.Sub(foot.Frames[frame-1].Position).Length()
				if delta > params.FootJumpMax {
					e := errAt(CodeJump, fmt.Sprintf("foot %s jumps %.6f between frames (limit %.6f)", foot.Foot, delta, params.FootJumpMax), frame)
					e.Cycle = cycleForFrame(source.Cycles, frame)
					e.Foot = foot.Foot
					e.Field = "feet.position"
					return e
				}
			}
		}
	}
	for _, leg := range source.Legs {
		if !footSet[leg.Foot] {
			return errAt(CodeSchema, fmt.Sprintf("foot track for leg %q is missing", leg.Foot), 0)
		}
	}
	return nil
}

func validateSkeleton(skeleton Skeleton) error {
	if strings.TrimSpace(skeleton.ID) == "" {
		e := errAt(CodeSchema, "skeleton.id is required", 0)
		e.Field = "skeleton.id"
		return e
	}
	if len(skeleton.Nodes) == 0 {
		e := errAt(CodeSchema, "skeleton.nodes must not be empty", 0)
		e.Field = "skeleton.nodes"
		return e
	}
	nodes := make(map[string]Bone, len(skeleton.Nodes))
	for _, node := range skeleton.Nodes {
		if strings.TrimSpace(node.Name) == "" {
			e := errAt(CodeSchema, "bone name is required", 0)
			e.Field = "nodes.name"
			return e
		}
		if _, exists := nodes[node.Name]; exists {
			e := errAt(CodeSchema, fmt.Sprintf("duplicate bone %q", node.Name), 0)
			e.Field = "nodes.name"
			return e
		}
		if !isFinite(node.LocalOffset.X) || !isFinite(node.LocalOffset.Y) || !isFinite(node.LocalOffset.Z) {
			e := errAt(CodeSchema, fmt.Sprintf("bone %s local_offset is not finite", node.Name), 0)
			e.Field = "nodes.local_offset"
			return e
		}
		nodes[node.Name] = node
	}
	rootCount := 0
	for _, node := range skeleton.Nodes {
		if node.Parent == "" {
			rootCount++
			continue
		}
		if _, ok := nodes[node.Parent]; !ok {
			e := errAt(CodeSchema, fmt.Sprintf("bone %s references missing parent %s", node.Name, node.Parent), 0)
			e.Field = "nodes.parent"
			return e
		}
	}
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(nodes))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case visiting:
			return errAt(CodeCycle, fmt.Sprintf("skeleton parent relation contains a cycle at bone %s", name), 0)
		case done:
			return nil
		}
		state[name] = visiting
		node := nodes[name]
		if node.Parent != "" {
			if err := visit(node.Parent); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for name := range nodes {
		if err := visit(name); err != nil {
			return err
		}
	}
	if rootCount == 0 {
		e := errAt(CodeSchema, "skeleton must have a root bone", 0)
		e.Field = "nodes.parent"
		return e
	}
	return nil
}

func validateLegs(skeleton Skeleton, legs []LegChain) error {
	if len(legs) == 0 {
		return errAt(CodeSchema, "legs must not be empty", 0)
	}
	nodes := make(map[string]Bone, len(skeleton.Nodes))
	for _, n := range skeleton.Nodes {
		nodes[n.Name] = n
	}
	seen := make(map[string]bool)
	for _, leg := range legs {
		if leg.Foot == "" || leg.Hip == "" || leg.Knee == "" || leg.Ankle == "" {
			e := errAt(CodeSchema, "leg chain fields foot/hip/knee/ankle are required", 0)
			e.Field = "legs"
			return e
		}
		if seen[leg.Foot] {
			return errAt(CodeSchema, fmt.Sprintf("duplicate leg foot %q", leg.Foot), 0)
		}
		seen[leg.Foot] = true
		for _, ref := range []string{leg.Hip, leg.Knee, leg.Ankle} {
			if _, ok := nodes[ref]; !ok {
				e := errAt(CodeSchema, fmt.Sprintf("leg %s references missing bone %s", leg.Foot, ref), 0)
				e.Field = "legs"
				return e
			}
		}
		for _, toe := range leg.Toes {
			if _, ok := nodes[toe]; !ok {
				e := errAt(CodeSchema, fmt.Sprintf("leg %s references missing toe bone %s", leg.Foot, toe), 0)
				e.Field = "legs.toes"
				return e
			}
		}
	}
	return nil
}

func validateCycles(source SourceLoop) error {
	if len(source.Cycles) == 0 {
		return errCycle(CodeDuration, "cycles must not be empty", 0)
	}
	expectedStart := 0
	for i, c := range source.Cycles {
		if c.Index != i {
			return errCycle(CodeSchema, fmt.Sprintf("cycle index is %d, expected %d", c.Index, i), i)
		}
		if c.StartFrame != expectedStart {
			return errCycle(CodeSchema, fmt.Sprintf("cycle starts at frame %d, expected %d", c.StartFrame, expectedStart), i)
		}
		if c.EndFrame < c.StartFrame {
			e := errCycle(CodeDuration, fmt.Sprintf("cycle duration is zero or negative (%d..%d)", c.StartFrame, c.EndFrame), i)
			e.Frame = c.StartFrame
			return e
		}
		if c.EndFrame >= source.FrameCount {
			e := errCycle(CodeSchema, fmt.Sprintf("cycle end frame %d exceeds frame count %d", c.EndFrame, source.FrameCount), i)
			e.Frame = c.EndFrame
			return e
		}
		expectedStart = c.EndFrame + 1
	}
	if expectedStart != source.FrameCount {
		e := errCycle(CodeSchema, fmt.Sprintf("cycles cover %d frames, expected %d", expectedStart, source.FrameCount), len(source.Cycles)-1)
		e.Frame = expectedStart
		return e
	}
	return nil
}

var allowedGlobalTransition = map[[2]string]bool{
	{PhaseDoubleSupport, PhaseDoubleSupport}: true,
	{PhaseDoubleSupport, PhaseStance}:        true,
	{PhaseDoubleSupport, PhaseSwing}:         true,
	{PhaseStance, PhaseDoubleSupport}:        true,
	{PhaseStance, PhaseStance}:               true,
	{PhaseSwing, PhaseDoubleSupport}:         true,
	{PhaseSwing, PhaseSwing}:                 true,
}

func validatePhaseTrack(track PhaseTrack) error {
	if strings.TrimSpace(track.SourceID) == "" {
		e := errAt(CodePhase, "phase source_id is required", 0)
		e.Field = "source_id"
		return e
	}
	if len(track.Frames) == 0 {
		return errAt(CodePhase, "phase frames are missing", 0)
	}
	for frame, phase := range track.Frames {
		if !validGlobalPhase(phase.Global) {
			return errAt(CodePhase, fmt.Sprintf("global phase %q is invalid", phase.Global), frame)
		}
		if !validFootPhase(phase.Left) {
			e := errAt(CodePhase, fmt.Sprintf("left phase %q is invalid", phase.Left), frame)
			e.Foot = "left"
			return e
		}
		if !validFootPhase(phase.Right) {
			e := errAt(CodePhase, fmt.Sprintf("right phase %q is invalid", phase.Right), frame)
			e.Foot = "right"
			return e
		}
		if frame > 0 {
			key := [2]string{track.Frames[frame-1].Global, phase.Global}
			if !allowedGlobalTransition[key] {
				return errAt(CodePhase, fmt.Sprintf("illegal phase transition %s -> %s", key[0], key[1]), frame)
			}
		}
	}
	return nil
}

func validGlobalPhase(phase string) bool {
	return phase == PhaseStance || phase == PhaseSwing || phase == PhaseDoubleSupport
}

func validFootPhase(phase string) bool {
	return phase == PhaseStance || phase == PhaseSwing
}

func validateTruth(truth Truth) error {
	if strings.TrimSpace(truth.SourceID) == "" {
		return errAt(CodeSchema, "truth source_id is required", 0)
	}
	if truth.FrameCount <= 0 || truth.CycleCount <= 0 {
		e := errAt(CodeSchema, "truth frame_count and cycle_count must be positive", 0)
		e.Field = "truth"
		return e
	}
	if len(truth.StrideLengths) != truth.CycleCount {
		e := errCycle(CodeSchema, fmt.Sprintf("truth stride_lengths has %d entries, expected cycle_count %d",
			len(truth.StrideLengths), truth.CycleCount), min(len(truth.StrideLengths), truth.CycleCount))
		e.Field = "truth.stride_lengths"
		return e
	}
	for i, stride := range truth.StrideLengths {
		if stride.Cycle < 0 || stride.Cycle >= truth.CycleCount {
			return errCycle(CodeSchema, fmt.Sprintf("stride cycle %d is outside truth cycle_count", stride.Cycle), i)
		}
	}
	for _, pos := range truth.StanceFootPositions {
		if pos.Cycle < 0 || pos.Cycle >= truth.CycleCount {
			return errCycle(CodeSchema, fmt.Sprintf("stance position cycle %d is outside truth cycle_count", pos.Cycle), pos.Cycle)
		}
	}
	for _, k := range truth.RootKeyframes {
		if k.Frame < 0 || k.Frame >= truth.FrameCount {
			return errAt(CodeSchema, fmt.Sprintf("root keyframe %d is outside frame_count", k.Frame), k.Frame)
		}
	}
	for _, transition := range truth.PhaseTransitions {
		if transition.Frame < 0 || transition.Frame >= truth.FrameCount {
			return errAt(CodeSchema, fmt.Sprintf("phase transition frame %d is outside frame_count", transition.Frame), transition.Frame)
		}
	}
	return nil
}

func ValidateForRetarget(source SourceLoop, phases PhaseTrack, truth Truth, target TargetSkeleton, params Params) error {
	if err := ValidateParams(params); err != nil {
		return err
	}
	if source.ID != phases.SourceID || source.ID != truth.SourceID {
		e := errAt(CodeSchema, "source, phase track and truth IDs do not match", 0)
		e.Field = "source_id"
		return e
	}
	if len(phases.Frames) != source.FrameCount {
		frame := min(len(phases.Frames), source.FrameCount)
		e := errAt(CodePhase, fmt.Sprintf("phase coverage has %d frames, expected %d", len(phases.Frames), source.FrameCount), frame)
		e.Cycle = cycleForFrame(source.Cycles, frame)
		if e.Cycle < 0 && len(source.Cycles) > 0 {
			e.Cycle = len(source.Cycles) - 1
		}
		return e
	}
	if truth.FrameCount != source.FrameCount {
		e := errAt(CodeSchema, fmt.Sprintf("truth frame_count %d does not match source %d", truth.FrameCount, source.FrameCount), source.FrameCount)
		e.Field = "truth.frame_count"
		return e
	}
	if truth.CycleCount != len(source.Cycles) {
		e := errCycle(CodeSchema, fmt.Sprintf("truth cycle_count %d does not match source cycles %d", truth.CycleCount, len(source.Cycles)), min(truth.CycleCount, len(source.Cycles)))
		e.Field = "truth.cycle_count"
		return e
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	if len(target.CycleFrames) != len(source.Cycles) {
		e := errCycle(CodeAlign, fmt.Sprintf("target declares %d cycles, source has %d", len(target.CycleFrames), len(source.Cycles)), min(len(target.CycleFrames), len(source.Cycles)))
		e.Conflicts = nil
		return e
	}
	sourceFeet := make(map[string]bool)
	for _, f := range source.Feet {
		sourceFeet[f.Foot] = true
	}
	for _, leg := range target.Legs {
		if !sourceFeet[leg.Foot] {
			e := errAt(CodeSchema, fmt.Sprintf("target leg %q has no source foot track", leg.Foot), 0)
			e.Foot = leg.Foot
			e.Field = "target.legs"
			return e
		}
	}
	for i, n := range target.CycleFrames {
		if n <= 0 {
			e := errCycle(CodeDuration, fmt.Sprintf("target cycle duration is %d", n), i)
			e.Frame = source.Cycles[i].StartFrame
			return e
		}
		if n > params.CycleBufferLimit {
			e := errCycle(CodeBuffer, fmt.Sprintf("target cycle has %d frames, buffer limit is %d", n, params.CycleBufferLimit), i)
			e.Frame = n - 1
			return e
		}
	}
	return nil
}

func validateTarget(target TargetSkeleton) error {
	if strings.TrimSpace(target.ID) == "" {
		e := errAt(CodeSchema, "target.id is required", 0)
		e.Field = "target.id"
		return e
	}
	if err := validateSkeleton(target.Skeleton); err != nil {
		return err
	}
	if !isFinite(target.LegLengthRatio) || target.LegLengthRatio < 0.25 || target.LegLengthRatio > 4 {
		e := errCycle(CodeRatio, "leg_length_ratio must be finite and in [0.25, 4]", 0)
		e.Frame = 0
		e.Field = "leg_length_ratio"
		return e
	}
	if !isFinite(target.AnkleROM.Min) || !isFinite(target.AnkleROM.Max) || target.AnkleROM.Min >= target.AnkleROM.Max {
		e := errAt(CodeSchema, "ankle_rom must be finite with min < max", 0)
		e.Field = "ankle_rom"
		return e
	}
	if !isFinite(target.FootReachable.Min) || !isFinite(target.FootReachable.Max) ||
		target.FootReachable.Min < target.Skeleton.GroundPlane ||
		target.FootReachable.Min > target.FootReachable.Max {
		e := errAt(CodeSchema, "foot_reachable_height must satisfy ground_plane <= min <= max", 0)
		e.Field = "foot_reachable_height"
		return e
	}
	if len(target.CycleFrames) == 0 {
		e := errCycle(CodeDuration, "target cycle_frames must not be empty", 0)
		e.Field = "cycle_frames"
		return e
	}
	for i, n := range target.CycleFrames {
		if n <= 0 {
			e := errCycle(CodeDuration, fmt.Sprintf("target cycle duration is %d", n), i)
			e.Field = "cycle_frames"
			return e
		}
	}
	if err := validateLegs(target.Skeleton, target.Legs); err != nil {
		return err
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
