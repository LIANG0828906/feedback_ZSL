package retarget

import (
	"fmt"
	"math"

	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

type pose struct {
	rotations []model.Quat
	root      model.Vec3
	stats     model.FrameStats
}

func solvePose(mapping *Mapping, source model.Frame, cfg model.Config) (*pose, error) {
	rotations := mapping.FrameRotations(source)
	root := mapping.RootTranslation(source.RootTranslation)
	result := &pose{rotations: rotations, root: root}
	if effector, required, limit := unreachableEffector(mapping, source, cfg.ReachTolerance); effector != "" {
		return nil, &model.AppError{
			Code: model.ErrInfeas, Object: mapping.TargetSkeleton.ID,
			Frame: source.Index, Joint: effector, Field: "reach",
			Message: fmt.Sprintf("mapped end-effector reach %.6f m exceeds target chain length %.6f m", required, limit),
		}
	}
	for iteration := 1; iteration <= cfg.MaxIterations; iteration++ {
		changed := false
		for i, joint := range mapping.TargetSkeleton.Joints {
			limit := joint.MaxAngleDeg * math.Pi / 180
			clamped, didClamp := model.ClampQuatAngle(result.rotations[i], limit)
			if didClamp {
				result.rotations[i] = clamped
				changed = true
			}
		}
		angleViolation, reachViolation, _ := evaluateConstraints(mapping, result.rotations, result.root, cfg)
		residual := normalizedResidual(angleViolation, reachViolation, cfg)
		if residual <= 1 {
			result.stats = model.FrameStats{
				Iterations:        iteration,
				AngleViolationDeg: angleViolation,
				ReachViolation:    reachViolation,
				Residual:          residual,
			}
			return result, nil
		}
		if reachViolation > cfg.ReachTolerance {
			projectReach(mapping, result.rotations)
			changed = true
		}
		if !changed {
			break
		}
	}
	angleViolation, reachViolation, effector := evaluateConstraints(mapping, result.rotations, result.root, cfg)
	residual := normalizedResidual(angleViolation, reachViolation, cfg)
	result.stats = model.FrameStats{
		Iterations:        cfg.MaxIterations,
		AngleViolationDeg: angleViolation,
		ReachViolation:    reachViolation,
		Residual:          residual,
	}
	if residual > 1 {
		return nil, &model.AppError{
			Code: model.ErrInfeas, Object: mapping.TargetSkeleton.ID,
			Frame: source.Index, Joint: effector, Field: "constraints",
			Message: fmt.Sprintf("constraints did not converge after %d iterations (angle=%.6f deg reach=%.6f m)", cfg.MaxIterations, angleViolation, reachViolation),
		}
	}
	return result, nil
}

// unreachableEffector checks the proportion-normalized source target point
// against the target kinematic chain. Rotation alone cannot repair this class
// of infeasibility, so it is reported before iterative projection starts.
func unreachableEffector(mapping *Mapping, source model.Frame, tolerance float64) (string, float64, float64) {
	sourcePositions, _ := sample.ForwardKinematics(mapping.SourceSkeleton, source)
	rootIndex := mapping.SourceIndex[rootName(mapping.SourceSkeleton.Joints)]
	sourceRoot := sourcePositions[rootIndex]
	targetJoints := make(map[string]model.Joint, len(mapping.TargetSkeleton.Joints))
	for _, joint := range mapping.TargetSkeleton.Joints {
		targetJoints[joint.Name] = joint
	}
	for _, targetJoint := range mapping.TargetSkeleton.Joints {
		if !targetJoint.EndEffector {
			continue
		}
		mapped, ok := mapping.ByTarget[targetJoint.Name]
		if !ok {
			continue
		}
		sourceIndex, ok := mapping.SourceIndex[mapped.Source]
		if !ok {
			continue
		}
		required := sourcePositions[sourceIndex].Sub(sourceRoot).Length() * mapping.RootScale
		limit := chainLength(targetJoint.Name, targetJoints)
		if required > limit+tolerance {
			return targetJoint.Name, required, limit
		}
	}
	return "", 0, 0
}

func evaluateConstraints(mapping *Mapping, rotations []model.Quat, root model.Vec3, cfg model.Config) (float64, float64, string) {
	maxAngle := 0.0
	for i, joint := range mapping.TargetSkeleton.Joints {
		limit := joint.MaxAngleDeg
		angle := rotations[i].AngleDeg()
		if angle-limit > maxAngle {
			maxAngle = angle - limit
		}
	}
	frame := model.Frame{RootTranslation: root, Rotations: rotations}
	positions, _ := sample.ForwardKinematics(mapping.TargetSkeleton.Skeleton(), frame)
	rootPos := positions[mapping.TargetIndex[rootName(mapping.TargetSkeleton.Joints)]]
	maxReach := 0.0
	effector := ""
	byName := make(map[string]model.Joint, len(mapping.TargetSkeleton.Joints))
	for _, joint := range mapping.TargetSkeleton.Joints {
		byName[joint.Name] = joint
	}
	for i, joint := range mapping.TargetSkeleton.Joints {
		if !joint.EndEffector {
			continue
		}
		chainLen := chainLength(joint.Name, byName)
		actual := positions[i].Sub(rootPos).Length()
		violation := actual - chainLen
		if violation > maxReach {
			maxReach = violation
			effector = joint.Name
		}
	}
	if maxReach < 0 {
		maxReach = 0
	}
	return maxAngle, maxReach, effector
}

func chainLength(name string, joints map[string]model.Joint) float64 {
	total := 0.0
	for {
		joint, ok := joints[name]
		if !ok || joint.Parent == "" {
			return total
		}
		total += joint.Offset.Length()
		name = joint.Parent
	}
}

func rootName(joints []model.Joint) string {
	for _, joint := range joints {
		if joint.Parent == "" {
			return joint.Name
		}
	}
	return ""
}

func projectReach(mapping *Mapping, rotations []model.Quat) {
	byName := make(map[string]model.Joint, len(mapping.TargetSkeleton.Joints))
	for _, joint := range mapping.TargetSkeleton.Joints {
		byName[joint.Name] = joint
	}
	for _, joint := range mapping.TargetSkeleton.Joints {
		if !joint.EndEffector {
			continue
		}
		name := joint.Name
		for {
			current, ok := byName[name]
			if !ok || current.Parent == "" {
				break
			}
			idx := mapping.TargetIndex[name]
			rotations[idx] = rotations[idx].Slerp(model.QuatIdentity, 0.22)
			name = current.Parent
		}
	}
}

func normalizedResidual(angleViolation, reachViolation float64, cfg model.Config) float64 {
	angle := 0.0
	if cfg.AngleToleranceDeg > 0 {
		angle = angleViolation / cfg.AngleToleranceDeg
	}
	reach := 0.0
	if cfg.ReachTolerance > 0 {
		reach = reachViolation / cfg.ReachTolerance
	}
	if reach > angle {
		return reach
	}
	return angle
}
