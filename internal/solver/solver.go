package solver

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"retargetlab/internal/geom"
	"retargetlab/internal/kinematics"
	"retargetlab/internal/model"
)

type assignment struct {
	ruleIndex int
	sourceIdx int
	weight    float64
}

type Solver struct {
	source    model.Skeleton
	clip      model.Clip
	target    model.Skeleton
	mapping   model.Mapping
	params    Params
	bodyScale float64

	order       []int
	assignment  []assignment
	hasAssign   []bool
	missingBy   map[string]model.MissingRule
	scaleValues map[string]float64
	maxCCDStep  float64
}

func New(source model.Skeleton, clip model.Clip, target model.Skeleton, mapping model.Mapping, params Params) (*Solver, error) {
	if err := model.ValidateSkeleton(source); err != nil {
		return nil, err
	}
	if err := model.ValidateClip(source, clip); err != nil {
		return nil, err
	}
	if err := model.ValidateSkeleton(target); err != nil {
		return nil, err
	}
	if err := model.ValidateMapping(source, target, mapping); err != nil {
		return nil, err
	}
	if params.ParamVersion < 0 {
		return nil, model.Err(model.E_PARAM, "solver", "paramVersion must be non-negative")
	}
	if strings.TrimSpace(params.AlgorithmVersion) == "" {
		return nil, model.Err(model.E_PARAM, "solver", "algorithmVersion is required")
	}
	if params.MaxCCDStepDeg == 0 {
		params.MaxCCDStepDeg = 30
	}
	if math.IsNaN(params.MaxCCDStepDeg) || params.MaxCCDStepDeg <= 0 {
		return nil, model.Err(model.E_PARAM, "solver", "maxCCDStepDeg must be positive")
	}

	scaleValues := make(map[string]float64, len(mapping.ScaleDefaults))
	for _, s := range mapping.ScaleDefaults {
		scaleValues[s.Key] = s.Value
	}
	for k, v := range params.ScaleOverrides {
		if _, ok := scaleValues[k]; !ok {
			return nil, model.Err(model.E_PARAM, "solver", fmt.Sprintf("unknown scale coefficient %q", k))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return nil, model.Err(model.E_PARAM, "solver", fmt.Sprintf("scale coefficient %q must be finite and positive", k))
		}
		scaleValues[k] = v
	}

	order, err := kinematics.TopologicalOrder(target)
	if err != nil {
		return nil, model.Err(model.E_SCHEMA, "solver", err.Error())
	}
	assignments := make([]assignment, len(target.Joints))
	hasAssign := make([]bool, len(target.Joints))
	for ri, r := range mapping.Rules {
		src := source.JointIndex(r.Source)
		for _, targetName := range r.Targets {
			ti := target.JointIndex(targetName)
			weight := r.Weight
			if weight <= 0 {
				weight = 1
			}
			assignments[ti] = assignment{ruleIndex: ri, sourceIdx: src, weight: weight}
			hasAssign[ti] = true
		}
	}
	missingBy := make(map[string]model.MissingRule, len(mapping.Missing))
	for _, mr := range mapping.Missing {
		missingBy[mr.Joint] = mr
	}
	return &Solver{
		source:      source,
		clip:        clip,
		target:      target,
		mapping:     mapping,
		params:      params,
		bodyScale:   target.Height() / source.Height(),
		order:       order,
		assignment:  assignments,
		hasAssign:   hasAssign,
		missingBy:   missingBy,
		scaleValues: scaleValues,
		maxCCDStep:  params.MaxCCDStepDeg * math.Pi / 180,
	}, nil
}

func (s *Solver) Snapshot() Snapshot {
	scale := make(map[string]float64, len(s.scaleValues))
	for k, v := range s.scaleValues {
		scale[k] = v
	}
	return Snapshot{
		SkeletonHash:      model.CombinedSkeletonHash(s.source, s.target),
		MappingHash:       s.mapping.Hash(),
		ParamVersion:      s.params.ParamVersion,
		AlgorithmVersion:  s.params.AlgorithmVersion,
		BodyScale:         s.bodyScale,
		ScaleCoefficients: scale,
		Mapping:           s.mapping.Clone(),
	}
}

func (s *Solver) ScaleValues() map[string]float64 {
	out := make(map[string]float64, len(s.scaleValues))
	for k, v := range s.scaleValues {
		out[k] = v
	}
	return out
}

func (s *Solver) LocalRotations(frame model.Frame) ([]geom.Quat, error) {
	locals, _, err := s.deriveLocalRotations(frame)
	return locals, err
}

func (s *Solver) FrameRootScale(frame int) (float64, error) {
	if frame < 0 || frame >= len(s.clip.Frames) {
		return 0, model.ErrFrame(model.E_FRAME, "frame_scale", frame, "", "frame is out of range")
	}
	phase := s.clip.PhaseForFrame(frame)
	scale := s.bodyScale
	for _, sc := range s.mapping.ScaleDefaults {
		if phaseActive(sc.Phases, phase) {
			scale *= s.scaleValues[sc.Key]
		}
	}
	return scale, nil
}

func (s *Solver) SolveFrame(_ context.Context, frame int) (FrameResult, error) {
	if frame < 0 || frame >= len(s.clip.Frames) {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", "frame is out of range")
	}
	f := s.clip.Frames[frame]
	phase := s.clip.PhaseForFrame(frame)
	locals, angleStats, err := s.deriveLocalRotations(f)
	if err != nil {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
	}

	rootScale := s.bodyScale
	for _, sc := range s.mapping.ScaleDefaults {
		if phaseActive(sc.Phases, phase) {
			rootScale *= s.scaleValues[sc.Key]
		}
	}
	root := f.RootTranslation.Mul(rootScale)
	correctedRoot, rootShift, contacts, err := kinematics.ContactCorrectedRoot(s.target, root, locals, s.clip.Contacts, frame)
	if err != nil {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
	}
	root = correctedRoot

	sourceWP, err := kinematics.Forward(s.source, f.RootTranslation, f.LocalRotations)
	if err != nil {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
	}
	sourceRootIdx := -1
	for i, j := range s.source.Joints {
		if j.Parent < 0 {
			sourceRootIdx = i
			break
		}
	}
	if sourceRootIdx < 0 {
		return FrameResult{}, model.ErrFrame(model.E_SCHEMA, "solve", frame, "", "source skeleton has no root")
	}
	sourceRoot := sourceWP.Positions[sourceRootIdx]

	world, err := kinematics.Forward(s.target, root, locals)
	if err != nil {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
	}
	effectorStats := ConstraintStats{}
	type effectorGoal struct {
		target int
		goal   geom.Vec3
	}
	effectorGoals := make([]effectorGoal, 0, len(s.mapping.Effectors))
	for _, e := range s.mapping.Effectors {
		if !phaseActive(e.Phases, phase) {
			continue
		}
		ti := s.target.JointIndex(e.Target)
		si := s.source.JointIndex(e.Source)
		if ti < 0 || si < 0 {
			return FrameResult{}, model.ErrFrame(model.E_SCHEMA, "solve", frame, e.Target, "effector references an unknown joint")
		}
		goal := root.Add(sourceWP.Positions[si].Sub(sourceRoot).Mul(rootScale))
		effectorGoals = append(effectorGoals, effectorGoal{target: ti, goal: goal})
		err := world.Positions[ti].Sub(goal).Length()
		if err <= e.Tolerance {
			continue
		}
		if err := s.solveCCD(&locals, &world, root, frame, ti, goal, e, &effectorStats); err != nil {
			return FrameResult{}, err
		}
	}
	// Contact correction remains authoritative; if CCD moved a contacting foot,
	// re-ground the root and re-run FK.
	root, shift2, contactsAfter, err := kinematics.ContactCorrectedRoot(s.target, root, locals, s.clip.Contacts, frame)
	if err != nil {
		return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
	}
	if shift2.LengthSquared() > 0 {
		rootShift = rootShift.Add(shift2)
		for i := range effectorGoals {
			effectorGoals[i].goal = effectorGoals[i].goal.Add(shift2)
		}
		world, err = kinematics.Forward(s.target, root, locals)
		if err != nil {
			return FrameResult{}, model.ErrFrame(model.E_FRAME, "solve", frame, "", err.Error())
		}
	}
	if contactsAfter != nil {
		contacts = contactsAfter
	}
	sort.Strings(contacts)

	stats := angleStats
	stats.Iterations += effectorStats.Iterations
	for _, eg := range effectorGoals {
		ti, goal := eg.target, eg.goal
		d := world.Positions[ti].Sub(goal).Length()
		stats.EffectorConstraints++
		stats.EffectorViolationSum += d
		if d > stats.EffectorViolation {
			stats.EffectorViolation = d
		}
	}

	worldSummary := make([]JointWorld, len(s.target.Joints))
	for i, j := range s.target.Joints {
		worldSummary[i] = JointWorld{Joint: j.Name, Position: world.Positions[i], Rotation: world.Rotations[i]}
	}
	result := FrameResult{
		Frame:           frame,
		TargetRotations: append([]geom.Quat(nil), locals...),
		World:           worldSummary,
		Constraint:      stats,
		RootTrajectory:  root,
		RootShift:       rootShift,
		Contacts:        append([]string(nil), contacts...),
		Snapshot:        s.Snapshot(),
	}
	result.CacheKey = resultHash(result)
	return result, nil
}

func (s *Solver) deriveLocalRotations(frame model.Frame) ([]geom.Quat, ConstraintStats, error) {
	locals := make([]geom.Quat, len(s.target.Joints))
	for i := range locals {
		locals[i] = geom.IdentityQuat()
	}
	stats := ConstraintStats{}
	for _, ti := range s.order {
		tj := s.target.Joints[ti]
		var q geom.Quat
		if s.hasAssign[ti] {
			a := s.assignment[ti]
			q = frame.LocalRotations[a.sourceIdx].Normalize()
			if a.weight > 0 && a.weight < 1 {
				q = geom.Slerp(geom.IdentityQuat(), q, a.weight)
			}
		} else {
			mr, ok := s.missingBy[tj.Name]
			if !ok {
				return nil, stats, fmt.Errorf("target joint %q has no direct mapping or fallback", tj.Name)
			}
			switch mr.Policy {
			case model.MissingIdentity:
				q = geom.IdentityQuat()
			case model.MissingInheritParent:
				if tj.Parent < 0 {
					return nil, stats, fmt.Errorf("inherit_parent fallback for root joint %q", tj.Name)
				}
				q = locals[tj.Parent]
			case model.MissingNearestMappedAncestor:
				cur := tj.Parent
				found := false
				for cur >= 0 {
					if s.hasAssign[cur] {
						q = locals[cur]
						found = true
						break
					}
					cur = s.target.Joints[cur].Parent
				}
				if !found {
					return nil, stats, fmt.Errorf("no mapped ancestor for joint %q", tj.Name)
				}
			default:
				return nil, stats, fmt.Errorf("unknown fallback policy %q", mr.Policy)
			}
		}
		if tj.RotationLimitDeg > 0 {
			clamped, violation := geom.ClampAngle(q, tj.RotationLimitDeg*math.Pi/180)
			q = clamped
			if violation > 1e-12 {
				deg := violation * 180 / math.Pi
				stats.AngleViolations++
				stats.AngleViolationDeg += deg
				if deg > stats.MaxAngleViolationDeg {
					stats.MaxAngleViolationDeg = deg
				}
			}
		}
		locals[ti] = q.Normalize()
	}
	return locals, stats, nil
}

func (s *Solver) solveCCD(locals *[]geom.Quat, world *kinematics.WorldPose, root geom.Vec3, frame, targetIdx int, goal geom.Vec3, rule model.EffectorRule, stats *ConstraintStats) error {
	chain := make([]int, 0, len(s.target.Joints))
	cur := s.target.Joints[targetIdx].Parent
	for cur >= 0 {
		chain = append(chain, cur)
		cur = s.target.Joints[cur].Parent
	}
	if len(chain) == 0 {
		return nil
	}
	for iter := 0; iter < rule.MaxIterations; iter++ {
		if world.Positions[targetIdx].Sub(goal).Length() <= rule.Tolerance {
			break
		}
		for _, j := range chain {
			stats.Iterations++
			p := world.Positions[j]
			v1 := world.Positions[targetIdx].Sub(p)
			v2 := goal.Sub(p)
			if v1.LengthSquared() < 1e-16 || v2.LengthSquared() < 1e-16 {
				continue
			}
			delta := geom.QuatFromTo(v1, v2)
			delta, _ = geom.ClampAngle(delta, s.maxCCDStep)
			parentRot := geom.IdentityQuat()
			if s.target.Joints[j].Parent >= 0 {
				parentRot = world.Rotations[s.target.Joints[j].Parent]
			}
			newWorld := delta.Mul(world.Rotations[j]).Normalize()
			newLocal := parentRot.Inverse().Mul(newWorld).Normalize()
			if limit := s.target.Joints[j].RotationLimitDeg; limit > 0 {
				var violation float64
				newLocal, violation = geom.ClampAngle(newLocal, limit*math.Pi/180)
				if violation > 1e-12 {
					deg := violation * 180 / math.Pi
					stats.AngleViolations++
					stats.AngleViolationDeg += deg
					if deg > stats.MaxAngleViolationDeg {
						stats.MaxAngleViolationDeg = deg
					}
				}
			}
			(*locals)[j] = newLocal
			next, err := kinematics.Forward(s.target, root, *locals)
			if err != nil {
				return model.ErrFrame(model.E_FRAME, "solve_ccd", frame, s.target.Joints[j].Name, err.Error())
			}
			*world = next
			if world.Positions[targetIdx].Sub(goal).Length() <= rule.Tolerance {
				break
			}
		}
	}
	return nil
}

func resultHash(r FrameResult) string {
	clone := r
	clone.CacheKey = ""
	return model.StableHash(clone)
}

func phaseActive(phases []int, phase int) bool {
	if len(phases) == 0 {
		return true
	}
	for _, p := range phases {
		if p == phase {
			return true
		}
	}
	return false
}
