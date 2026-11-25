package core

import (
	"fmt"
	"math"
)

type SolveOutput struct {
	Local         []Quat
	Pose          *Pose
	Iterations    int
	Converged     bool
	AngleResidual map[string]float64
	ReachResidual map[string]float64
	MaxAngle      float64
	MaxReach      float64
}

func limitAxis(j Joint) Vec3 {
	if j.Limit != nil && j.Limit.Axis.Norm() > 1e-12 {
		return j.Limit.Axis.Normalize()
	}
	return Vec3{1, 0, 0}
}

func AngleViolation(j Joint, q Quat) float64 {
	if j.Limit == nil {
		return 0
	}
	axis := limitAxis(j)
	signed := SignedAngleAround(q, axis)
	violation := 0.0
	if signed < j.Limit.MinRadians {
		violation = j.Limit.MinRadians - signed
	}
	if signed > j.Limit.MaxRadians {
		violation = signed - j.Limit.MaxRadians
	}
	capAngle := math.Max(math.Abs(j.Limit.MinRadians), math.Abs(j.Limit.MaxRadians))
	if total := q.Angle(); total > capAngle {
		violation = math.Max(violation, total-capAngle)
	}
	return violation
}

func projectJointAngle(j Joint, q Quat) Quat {
	if j.Limit == nil {
		return q.Normalized()
	}
	axis := limitAxis(j)
	signed := SignedAngleAround(q, axis)
	if signed < j.Limit.MinRadians || signed > j.Limit.MaxRadians {
		clamped := math.Min(j.Limit.MaxRadians, math.Max(j.Limit.MinRadians, signed))
		q = AxisAngle(axis, clamped)
	}
	capAngle := math.Max(math.Abs(j.Limit.MinRadians), math.Abs(j.Limit.MaxRadians))
	return ProjectAngle(q, capAngle)
}

func SolveFrame(frame int, sk Skeleton, initial []Quat, params Parameters) (*SolveOutput, *Error) {
	idx, ierr := NewSkeletonIndex(sk)
	if ierr != nil {
		return nil, ierr
	}
	if len(initial) != len(sk.Joints) {
		return nil, &Error{Code: ECodeQuat, Frame: frame, Message: "internal initial rotation count mismatch"}
	}
	local := make([]Quat, len(initial))
	for i, q := range initial {
		local[i] = q.Normalized()
	}
	out := &SolveOutput{Local: local, AngleResidual: map[string]float64{}, ReachResidual: map[string]float64{}}
	maxIterations := params.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultParameters().MaxIterations
	}

	var pose *Pose
	for iter := 1; iter <= maxIterations; iter++ {
		out.Iterations = iter
		for i, j := range sk.Joints {
			local[i] = projectJointAngle(j, local[i])
		}
		var err *Error
		pose, err = ForwardKinematics(sk, Vec3{}, local)
		if err != nil {
			return nil, err
		}

		for i, j := range sk.Joints {
			if j.Reach == nil {
				continue
			}
			anchor, ok := idx.ByName[j.Reach.Anchor]
			if !ok {
				return nil, &Error{Code: ECodeSchema, Frame: frame, Joint: j.Name, Message: "reach anchor does not exist: " + j.Reach.Anchor}
			}
			distance := pose.WorldPos[i].Sub(pose.WorldPos[anchor]).Norm()
			if distance > j.Reach.MaxDistance+params.ReachTolerance {
				ccdFoldReach(sk, idx, local, pose, i, anchor, j.Reach.MaxDistance)
			}
		}

		pose, err = ForwardKinematics(sk, Vec3{}, local)
		if err != nil {
			return nil, err
		}
		out.MaxAngle = 0
		out.MaxReach = 0
		for name := range out.AngleResidual {
			out.AngleResidual[name] = 0
		}
		for name := range out.ReachResidual {
			out.ReachResidual[name] = 0
		}
		worstJoint := ""
		for i, j := range sk.Joints {
			av := AngleViolation(j, local[i])
			out.AngleResidual[j.Name] = av
			if av > out.MaxAngle {
				out.MaxAngle = av
				worstJoint = j.Name
			}
			if j.Reach != nil {
				anchor := idx.ByName[j.Reach.Anchor]
				rv := math.Max(0, pose.WorldPos[i].Sub(pose.WorldPos[anchor]).Norm()-j.Reach.MaxDistance)
				out.ReachResidual[j.Name] = rv
				if rv > out.MaxReach {
					out.MaxReach = rv
					if rv > out.MaxAngle {
						worstJoint = j.Name
					}
				}
			}
		}
		if out.MaxAngle <= params.AngleTolerance && out.MaxReach <= params.ReachTolerance {
			out.Converged = true
			out.Pose = pose
			return out, nil
		}
		_ = worstJoint
	}

	worst := ""
	worstValue := -1.0
	details := map[string]float64{}
	for _, j := range sk.Joints {
		av := out.AngleResidual[j.Name]
		rv := out.ReachResidual[j.Name]
		if av > worstValue {
			worstValue, worst = av, j.Name
		}
		if rv > worstValue {
			worstValue, worst = rv, j.Name
		}
		if av > params.AngleTolerance || rv > params.ReachTolerance {
			details[j.Name+".angle"] = av
			details[j.Name+".reach"] = rv
		}
	}
	return out, &Error{
		Code:    ECodeSolve,
		Frame:   frame,
		Joint:   worst,
		Field:   "constraint_projection",
		Message: fmt.Sprintf("constraint projection did not converge after %d iterations", maxIterations),
		Details: map[string]any{"iterations": maxIterations, "residual": worstValue, "violations": details},
	}
}

func ccdFoldReach(sk Skeleton, idx *SkeletonIndex, local []Quat, pose *Pose, endIndex, anchorIndex int, maxDistance float64) {
	path := []int{}
	cur := endIndex
	for {
		name := sk.Joints[cur].Parent
		if name == "" {
			break
		}
		cur = idx.ByName[name]
		if cur == anchorIndex {
			break
		}
		path = append(path, cur)
		if cur == anchorIndex {
			break
		}
	}
	// path currently parent(end), ..., joint immediately below anchor because anchor not appended.
	for _, k := range path {
		currentPose, err := ForwardKinematics(sk, Vec3{}, local)
		if err != nil {
			return
		}
		anchorPos := currentPose.WorldPos[anchorIndex]
		ep := currentPose.WorldPos[endIndex]
		kp := currentPose.WorldPos[k]
		if ep.Sub(anchorPos).Norm() <= maxDistance {
			return
		}
		direction := ep.Sub(anchorPos).Normalize()
		target := anchorPos.Add(direction.Scale(maxDistance))
		from := ep.Sub(kp)
		to := target.Sub(kp)
		delta := QuatFromVectors(from, to)
		parent := sk.Joints[k].Parent
		parentWorld := Quat{1, 0, 0, 0}
		if parent != "" {
			parentWorld = currentPose.WorldRot[idx.ByName[parent]]
		}
		newWorld := delta.Mul(currentPose.WorldRot[k]).Normalized()
		local[k] = parentWorld.Conjugate().Mul(newWorld).Normalized()
	}
}
