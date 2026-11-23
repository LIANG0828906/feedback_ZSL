package retarget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

func Run(clip model.Clip, target model.TargetSkeleton, cfg model.Config) (*model.Animation, error) {
	if err := loader.ValidateClip(clip); err != nil {
		return nil, err
	}
	if err := loader.ValidateTarget(target, clip.Skeleton); err != nil {
		return nil, err
	}
	if err := ValidateContactMarkers(clip, target); err != nil {
		return nil, err
	}
	plan, err := ChoosePlan(clip, target, cfg)
	if err != nil {
		return nil, err
	}
	mapping, err := BuildMapping(clip.Skeleton, target)
	if err != nil {
		return nil, err
	}
	paramVersion := ParameterVersion(clip, target, cfg)

	keySet := make(map[int]bool, len(plan.SolveFrameIndices))
	for _, index := range plan.SolveFrameIndices {
		keySet[index] = true
	}
	solved := make(map[int]*pose, len(plan.SolveFrameIndices))
	for _, index := range plan.SolveFrameIndices {
		p, solveErr := solvePose(mapping, clip.Frames[index], cfg)
		if solveErr != nil {
			return nil, solveErr
		}
		solved[index] = p
	}
	smoothPoses(solved, plan.SolveFrameIndices, plan.SmoothWindow)

	base := make([]*pose, len(clip.Frames))
	if plan.Mode == "exact" {
		for i := range clip.Frames {
			base[i] = solved[i]
		}
	} else {
		base = interpolatePoses(clip.Frames, solved, plan.SolveFrameIndices, len(target.Joints))
	}
	// Hard projection after smoothing/interpolation is deterministic and is the
	// constraint-priority half of the smoothing trade-off.
	preClamp := make([][]model.Quat, len(base))
	for i, p := range base {
		preClamp[i] = append([]model.Quat(nil), p.rotations...)
		clampPose(mapping, p)
	}

	contactSpans := sample.ContactIntervals(clip.Frames, sortedFeet(target))
	anchors := contactAnchors(mapping, clip, target, base, contactSpans, cfg.GroundY)
	frames := make([]model.FrameResult, len(clip.Frames))
	maxDrift := 0.0
	totalDecisions := 0
	tolerance := clip.Truth.Tolerances.ContactDrift
	for f := range clip.Frames {
		p := base[f]
		decisions := clampDecisions(mapping, f, preClamp[f], p.rotations, tolerance)
		contacts := activeContacts(clip.Frames[f], target)
		drift, contactDecision := correctContact(mapping, p, f, contacts, anchors, contactSpans, cfg)
		if contactDecision != nil {
			decisions = append(decisions, *contactDecision)
		}
		angleViolation, reachViolation, _ := evaluateConstraints(mapping, p.rotations, p.root, cfg)
		residual := normalizedResidual(angleViolation, reachViolation, cfg)
		p.stats.AngleViolationDeg = angleViolation
		p.stats.ReachViolation = reachViolation
		p.stats.Residual = residual
		p.stats.ContactDrift = drift
		if drift > maxDrift {
			maxDrift = drift
		}
		totalDecisions += len(decisions)
		frames[f] = model.FrameResult{
			Frame: f, Rotations: append([]model.Quat(nil), p.rotations...),
			RootTranslation: p.root, Contacts: contacts, Stats: p.stats,
			Decisions: decisions, State: "committed",
		}
	}
	if maxDrift > tolerance+1e-9 {
		return nil, &model.AppError{
			Code: model.ErrMark, Object: clip.ID, Frame: firstDriftFrame(frames), Field: "contact_drift",
			Message: fmt.Sprintf("support foot drift %.9f exceeds declared upper bound %.9f", maxDrift, tolerance),
		}
	}
	if badFrame, delta := contactSetMismatch(clip, target); delta > clip.Truth.Tolerances.ContactFrameSlack {
		return nil, &model.AppError{
			Code: model.ErrMark, Object: clip.ID, Frame: badFrame, Field: "contacts",
			Message: fmt.Sprintf("contact frame set differs from truth by %d frames", delta),
		}
	}
	animation := &model.Animation{
		ClipID: clip.ID, TargetID: target.ID,
		AlgorithmVersion: model.AlgorithmVersion, ParameterVersion: paramVersion,
		Plan: plan, Mapping: mapping.Trace, Frames: frames,
		ContactSpans: contactSpans, ContactFrames: contactFrameIndices(frames),
		RootStart: frames[0].RootTranslation, RootEnd: frames[len(frames)-1].RootTranslation,
		SnapshotAction: "none",
	}
	animation.Summary = summarize(frames, totalDecisions)
	return animation, nil
}

func ValidateContactMarkers(clip model.Clip, target model.TargetSkeleton) error {
	if len(target.ContactJoints) == 0 {
		return model.NewError(model.ErrMark, target.ID, "contact_joints", "target does not declare contact feet")
	}
	required := make(map[string]bool, len(target.ContactJoints))
	targets := make(map[string]bool, len(target.ContactJoints))
	for sourceFoot, targetFoot := range target.ContactJoints {
		required[sourceFoot] = true
		if targets[targetFoot] {
			return &model.AppError{
				Code: model.ErrMark, Object: target.ID, Frame: -1, Field: "contact_joints." + sourceFoot,
				Message: "multiple source feet map to one target foot", Details: []string{targetFoot},
			}
		}
		targets[targetFoot] = true
	}
	for f, frame := range clip.Frames {
		if frame.Contacts == nil {
			return model.FrameError(model.ErrMark, clip.ID, f, "contacts", "contact marker map is missing")
		}
		for foot := range frame.Contacts {
			if !required[foot] {
				return model.FrameError(model.ErrMark, clip.ID, f, "contacts."+foot, "unknown contact marker")
			}
		}
		for foot := range required {
			if _, ok := frame.Contacts[foot]; !ok {
				return model.FrameError(model.ErrMark, clip.ID, f, "contacts."+foot, "contact marker is missing")
			}
		}
	}
	return nil
}

func smoothPoses(poses map[int]*pose, indices []int, window int) {
	if window <= 1 || len(indices) == 0 {
		return
	}
	originals := make(map[int][]model.Quat, len(indices))
	for _, index := range indices {
		originals[index] = append([]model.Quat(nil), poses[index].rotations...)
	}
	radius := window / 2
	for _, index := range indices {
		// Endpoints carry root trajectory invariants across exact/sampled modes;
		// they are never allowed to drift through a mode-dependent neighbour set.
		if index == indices[0] || index == indices[len(indices)-1] {
			continue
		}
		jointCount := len(poses[index].rotations)
		for joint := 0; joint < jointCount; joint++ {
			var samples []model.Quat
			var weights []float64
			for _, other := range indices {
				delta := other - index
				if delta < -radius || delta > radius {
					continue
				}
				weight := float64(radius + 1 - absInt(delta))
				samples = append(samples, originals[other][joint])
				weights = append(weights, weight)
			}
			if len(samples) > 0 {
				poses[index].rotations[joint] = model.WeightedQuatAverage(samples, weights)
			}
		}
	}
}

func interpolatePoses(frames []model.Frame, solved map[int]*pose, keys []int, jointCount int) []*pose {
	out := make([]*pose, len(frames))
	for f := range frames {
		if p, ok := solved[f]; ok {
			clone := &pose{rotations: append([]model.Quat(nil), p.rotations...), root: p.root, stats: p.stats}
			out[f] = clone
			continue
		}
		prev, next := boundingKeys(keys, f)
		if prev < 0 {
			prev = next
		}
		if next < 0 {
			next = prev
		}
		a, b := solved[prev], solved[next]
		alpha := 0.0
		if next != prev {
			alpha = float64(f-prev) / float64(next-prev)
		}
		rotations := make([]model.Quat, jointCount)
		for j := 0; j < jointCount; j++ {
			rotations[j] = a.rotations[j].Slerp(b.rotations[j], alpha)
		}
		out[f] = &pose{
			rotations: rotations,
			root:      a.root.Lerp(b.root, alpha),
			stats:     model.FrameStats{Iterations: 0, Interpolated: true},
		}
	}
	return out
}

func boundingKeys(keys []int, frame int) (int, int) {
	i := sort.SearchInts(keys, frame)
	prev := -1
	next := -1
	if i < len(keys) {
		next = keys[i]
	}
	if i > 0 {
		prev = keys[i-1]
	}
	return prev, next
}

func clampPose(mapping *Mapping, p *pose) {
	for i, joint := range mapping.TargetSkeleton.Joints {
		limit := joint.MaxAngleDeg * math.Pi / 180
		if clamped, changed := model.ClampQuatAngle(p.rotations[i], limit); changed {
			p.rotations[i] = clamped
		}
	}
}

func clampDecisions(mapping *Mapping, frame int, before, after []model.Quat, driftTolerance float64) []model.Decision {
	var decisions []model.Decision
	for i, joint := range mapping.TargetSkeleton.Joints {
		pre := before[i].AngleDeg()
		limit := joint.MaxAngleDeg
		if pre > limit+mappingTolerance(driftTolerance) {
			decisions = append(decisions, model.Decision{
				Frame: frame, Constraint: "angle_limit", Action: "constraint_priority",
				Reason: "temporal smoothing/interpolation exceeded the joint limit; hard projection retained",
				Before: pre, After: after[i].AngleDeg(), Joint: joint.Name,
			})
		}
	}
	return decisions
}

func mappingTolerance(v float64) float64 {
	if v <= 0 {
		return 1e-6
	}
	return v
}

type anchorKey struct {
	foot string
	span int
}

func contactAnchors(mapping *Mapping, clip model.Clip, target model.TargetSkeleton, poses []*pose, spans map[string][]model.ContactInterval, groundY float64) map[anchorKey]model.Vec3 {
	anchors := make(map[anchorKey]model.Vec3)
	for _, foot := range sortedFeet(target) {
		for spanIndex, span := range spans[foot] {
			if span.Start < 0 || span.Start >= len(poses) {
				continue
			}
			positions := targetWorld(mapping, poses[span.Start])
			targetFoot := target.ContactJoints[foot]
			pos := positions[mapping.TargetIndex[targetFoot]]
			pos.Y = groundY
			anchors[anchorKey{foot: foot, span: spanIndex}] = pos
		}
	}
	return anchors
}

func correctContact(mapping *Mapping, p *pose, frame int, contacts []string, anchors map[anchorKey]model.Vec3, spans map[string][]model.ContactInterval, cfg model.Config) (float64, *model.Decision) {
	if len(contacts) == 0 {
		return 0, nil
	}
	primary := contacts[0]
	spanIndex, ok := findSpan(spans[primary], frame)
	if !ok {
		return 0, nil
	}
	anchor, ok := anchors[anchorKey{foot: primary, span: spanIndex}]
	if !ok {
		return 0, nil
	}
	positions := targetWorld(mapping, p)
	footPos := positions[mapping.TargetIndex[mapping.FootJoints[primary]]]
	correction := anchor.Sub(footPos)
	if correction.Length() < 1e-15 {
		return 0, nil
	}
	p.root = p.root.Add(correction.Scale(cfg.ContactDamping))
	afterPositions := targetWorld(mapping, p)
	afterFoot := afterPositions[mapping.TargetIndex[mapping.FootJoints[primary]]]
	drift := afterFoot.Sub(anchor).Length()
	decision := &model.Decision{
		Frame: frame, Constraint: "foot_contact", Action: "constraint_priority",
		Reason: "support foot anchored to ground; root translation corrected after smoothing",
		Joint:  mapping.FootJoints[primary], Before: correction.Length(), After: drift,
	}
	return drift, decision
}

func findSpan(spans []model.ContactInterval, frame int) (int, bool) {
	for i, span := range spans {
		if frame >= span.Start && frame <= span.End {
			return i, true
		}
	}
	return 0, false
}

func targetWorld(mapping *Mapping, p *pose) []model.Vec3 {
	positions, _ := sample.ForwardKinematics(mapping.TargetSkeleton.Skeleton(), model.Frame{
		RootTranslation: p.root, Rotations: p.rotations,
	})
	return positions
}

func activeContacts(frame model.Frame, target model.TargetSkeleton) []string {
	var contacts []string
	for _, foot := range sortedFeet(target) {
		if frame.Contacts[foot] {
			contacts = append(contacts, foot)
		}
	}
	return contacts
}

func contactFrameIndices(frames []model.FrameResult) []int {
	var out []int
	for _, frame := range frames {
		if len(frame.Contacts) > 0 {
			out = append(out, frame.Frame)
		}
	}
	return out
}

func firstDriftFrame(frames []model.FrameResult) int {
	max := 0.0
	frame := 0
	for _, result := range frames {
		if result.Stats.ContactDrift > max {
			max = result.Stats.ContactDrift
			frame = result.Frame
		}
	}
	return frame
}

func contactSetMismatch(clip model.Clip, target model.TargetSkeleton) (int, int) {
	truth := clip.Truth.ContactIntervals
	derived := sample.ContactIntervals(clip.Frames, sortedFeet(target))
	maxDelta := 0
	badFrame := 0
	for foot, spans := range truth {
		got := derived[foot]
		delta := intervalFrameDelta(spans, got)
		if delta > maxDelta {
			maxDelta = delta
			badFrame = firstIntervalDifference(spans, got)
		}
	}
	return badFrame, maxDelta
}

func intervalFrameDelta(a, b []model.ContactInterval) int {
	if len(a) != len(b) {
		delta := len(a) - len(b)
		if delta < 0 {
			delta = -delta
		}
		return delta + 1
	}
	max := 0
	for i := range a {
		if d := absInt(a[i].Start - b[i].Start); d > max {
			max = d
		}
		if d := absInt(a[i].End - b[i].End); d > max {
			max = d
		}
	}
	return max
}

func firstIntervalDifference(a, b []model.ContactInterval) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i].Start < b[i].Start {
				return a[i].Start
			}
			return b[i].Start
		}
	}
	if len(a) > n {
		return a[n].Start
	}
	if len(b) > n {
		return b[n].Start
	}
	return 0
}

func summarize(frames []model.FrameResult, decisions int) model.Summary {
	summary := model.Summary{DecisionCount: decisions}
	for _, frame := range frames {
		if frame.Stats.Residual > summary.MaxResidual {
			summary.MaxResidual = frame.Stats.Residual
		}
		if frame.Stats.AngleViolationDeg > summary.MaxAngleViolation {
			summary.MaxAngleViolation = frame.Stats.AngleViolationDeg
		}
		if frame.Stats.ReachViolation > summary.MaxReachViolation {
			summary.MaxReachViolation = frame.Stats.ReachViolation
		}
		if frame.Stats.ContactDrift > summary.MaxContactDrift {
			summary.MaxContactDrift = frame.Stats.ContactDrift
		}
	}
	return summary
}

func ParameterVersion(clip model.Clip, target model.TargetSkeleton, cfg model.Config) string {
	payload := struct {
		Algorithm string               `json:"algorithm"`
		Clip      model.Clip           `json:"clip"`
		Target    model.TargetSkeleton `json:"target"`
		Config    model.Config         `json:"config"`
	}{
		Algorithm: model.AlgorithmVersion, Clip: clip, Target: target, Config: cfg,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func FrameContentHash(frame model.FrameResult, parameterVersion string) string {
	payload := struct {
		Frame      model.FrameResult `json:"frame"`
		Parameters string            `json:"parameters"`
		Algorithm  string            `json:"algorithm"`
	}{Frame: frame, Parameters: parameterVersion, Algorithm: model.AlgorithmVersion}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CheckScaleConsistency(exact, sampled *model.Animation, tolerances model.Tolerances) error {
	exactSet := make(map[int]bool, len(exact.ContactFrames))
	sampledSet := make(map[int]bool, len(sampled.ContactFrames))
	for _, f := range exact.ContactFrames {
		exactSet[f] = true
	}
	for _, f := range sampled.ContactFrames {
		sampledSet[f] = true
	}
	delta := 0
	first := -1
	for f := range exactSet {
		if !sampledSet[f] {
			delta++
			if first < 0 || f < first {
				first = f
			}
		}
	}
	for f := range sampledSet {
		if !exactSet[f] {
			delta++
			if first < 0 || f < first {
				first = f
			}
		}
	}
	startDelta := exact.RootStart.Sub(sampled.RootStart).Length()
	endDelta := exact.RootEnd.Sub(sampled.RootEnd).Length()
	if delta > tolerances.ScaleContactSlack || startDelta > tolerances.ScaleRoot || endDelta > tolerances.ScaleRoot {
		return &model.AppError{
			Code: model.ErrScale, Object: exact.ClipID, Frame: maxInt(first, 0), Field: "mode_comparison",
			Message: fmt.Sprintf("contact_frame_delta=%d (limit %d) root_start_delta=%.9f root_end_delta=%.9f (limit %.9f)",
				delta, tolerances.ScaleContactSlack, startDelta, endDelta, tolerances.ScaleRoot),
		}
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
