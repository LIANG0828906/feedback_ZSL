package retarget

import (
	"sort"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

type Mapping struct {
	ByTarget       map[string]model.MappedJoint
	SourceIndex    map[string]int
	TargetIndex    map[string]int
	FootJoints     map[string]string
	Trace          model.MappingTrace
	RootScale      float64
	TargetSkeleton model.TargetSkeleton
	SourceSkeleton model.Skeleton
}

func BuildMapping(source model.Skeleton, target model.TargetSkeleton) (*Mapping, error) {
	if err := loader.ValidateSkeleton(source); err != nil {
		return nil, err
	}
	if err := loader.ValidateTarget(target, source); err != nil {
		return nil, err
	}
	sourceIndex := make(map[string]int, len(source.Joints))
	targetIndex := make(map[string]int, len(target.Joints))
	for i, joint := range source.Joints {
		sourceIndex[joint.Name] = i
	}
	for i, joint := range target.Joints {
		targetIndex[joint.Name] = i
	}
	sourceRest := identityFrame(source)
	targetRest := identityFrame(target.Skeleton())
	sourcePos, _ := sample.ForwardKinematics(source, sourceRest)
	targetPos, _ := sample.ForwardKinematics(target.Skeleton(), targetRest)
	sourceHeight := tPoseHeight(sourcePos)
	targetHeight := tPoseHeight(targetPos)
	rootScale := 1.0
	if sourceHeight > 1e-9 {
		rootScale = targetHeight / sourceHeight
	}

	byTarget := make(map[string]model.MappedJoint, len(target.Joints))
	referenced := make(map[string]bool, len(source.Joints))
	for _, rule := range target.Mapping {
		referenced[rule.Source] = true
		sourceJoint := source.Joints[sourceIndex[rule.Source]]
		for _, targetName := range rule.Targets {
			targetJoint := target.Joints[targetIndex[targetName]]
			scale := boneScale(sourceJoint, targetJoint)
			if scale <= 0 {
				scale = rootScale
			}
			align := restAlignment(rule.Source, targetName, source, target, sourceIndex, targetIndex)
			byTarget[targetName] = model.MappedJoint{
				Target: targetName, Source: rule.Source, Strategy: rule.Strategy,
				Scale: scale, Align: align,
			}
		}
	}
	for _, fallback := range target.Fallbacks {
		referenced[fallback.Source] = true
		targetJoint := target.Joints[targetIndex[fallback.Target]]
		sourceJoint := source.Joints[sourceIndex[fallback.Source]]
		scale := boneScale(sourceJoint, targetJoint)
		if scale <= 0 {
			scale = rootScale
		}
		align := restAlignment(fallback.Source, fallback.Target, source, target, sourceIndex, targetIndex)
		byTarget[fallback.Target] = model.MappedJoint{
			Target: fallback.Target, Source: fallback.Source, Strategy: fallback.Strategy,
			Scale: scale, Align: align, Reason: fallback.Reason,
		}
	}
	var missing []string
	for _, joint := range target.Joints {
		if _, ok := byTarget[joint.Name]; !ok {
			missing = append(missing, joint.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &model.AppError{
			Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping",
			Message: "no feasible mapping for target joints", Details: missing,
		}
	}
	var unmapped []model.UnmappedSource
	for _, joint := range source.Joints {
		if !referenced[joint.Name] {
			unmapped = append(unmapped, model.UnmappedSource{
				Joint: joint.Name, Reason: "source joint has no target mapping; motion channel deliberately ignored",
			})
		}
	}
	sort.Slice(unmapped, func(i, j int) bool { return unmapped[i].Joint < unmapped[j].Joint })

	trace := model.MappingTrace{
		UnmappedSources: unmapped,
		RootScale:       rootScale,
		SourceHeight:    sourceHeight,
		TargetHeight:    targetHeight,
	}
	targetOrder := make([]string, 0, len(target.Joints))
	for _, joint := range target.Joints {
		mapped := byTarget[joint.Name]
		trace.Rules = append(trace.Rules, mapped)
		targetOrder = append(targetOrder, joint.Name)
	}
	return &Mapping{
		ByTarget: byTarget, SourceIndex: sourceIndex, TargetIndex: targetIndex,
		FootJoints: target.ContactJoints, Trace: trace, RootScale: rootScale,
		TargetSkeleton: target, SourceSkeleton: source,
	}, nil
}

func (m *Mapping) FrameRotations(sourceFrame model.Frame) []model.Quat {
	rotations := make([]model.Quat, len(m.TargetSkeleton.Joints))
	for i, joint := range m.TargetSkeleton.Joints {
		mapped := m.ByTarget[joint.Name]
		source := m.SourceIndex[mapped.Source]
		q := model.QuatIdentity
		if source < len(sourceFrame.Rotations) {
			q = sourceFrame.Rotations[source].Normalized()
		}
		align := mapped.Align.Normalized()
		q = align.Mul(q).Mul(align.Conjugate()).Normalized()
		rotations[i] = q
	}
	return rotations
}

func (m *Mapping) RootTranslation(sourceRoot model.Vec3) model.Vec3 {
	return sourceRoot.Scale(m.RootScale)
}

func identityFrame(skeleton model.Skeleton) model.Frame {
	rotations := make([]model.Quat, len(skeleton.Joints))
	for i := range rotations {
		rotations[i] = model.QuatIdentity
	}
	return model.Frame{RootTranslation: model.Vec3{}, Rotations: rotations}
}

func tPoseHeight(positions []model.Vec3) float64 {
	if len(positions) == 0 {
		return 0
	}
	lo, hi := positions[0].Y, positions[0].Y
	for _, position := range positions[1:] {
		if position.Y < lo {
			lo = position.Y
		}
		if position.Y > hi {
			hi = position.Y
		}
	}
	return hi - lo
}

func boneScale(sourceJoint, targetJoint model.Joint) float64 {
	sourceLen := sourceJoint.Offset.Length()
	targetLen := targetJoint.Offset.Length()
	if sourceLen < 1e-9 || targetLen < 1e-9 {
		return 0
	}
	return targetLen / sourceLen
}

func restAlignment(sourceName, targetName string, source model.Skeleton, target model.TargetSkeleton, sourceIndex, targetIndex map[string]int) model.Quat {
	sourceJoint := source.Joints[sourceIndex[sourceName]]
	targetJoint := target.Joints[targetIndex[targetName]]
	if sourceJoint.Parent == "" || targetJoint.Parent == "" {
		return model.QuatIdentity
	}
	sourceDir := sourceJoint.Offset.Normalize()
	targetDir := targetJoint.Offset.Normalize()
	if sourceDir.Length() < 0.5 || targetDir.Length() < 0.5 {
		return model.QuatIdentity
	}
	return model.QuatFromTo(sourceDir, targetDir)
}
