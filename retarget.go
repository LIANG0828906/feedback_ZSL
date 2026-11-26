package bonerig

import (
	"fmt"
)

// PrecisionRecord is the per-frame/per-joint numerical propagation report.
type PrecisionRecord struct {
	IntervalID  string `json:"interval_id"`
	FrameIndex  int    `json:"frame_index"`
	Joint       string `json:"joint"`
	SourceJoint string `json:"source_joint"`
	ChainLength int64  `json:"chain_length"`

	NormResidual int64 `json:"norm_residual"`
	NormUpper    int64 `json:"norm_upper"`

	ChainResidual int64 `json:"chain_residual"`
	ChainUpper    int64 `json:"chain_upper"`

	ScaleResidual            int64 `json:"scale_residual"`
	ScaleUpper               int64 `json:"scale_upper"`
	TranslationScaleResidual int64 `json:"translation_scale_residual"`

	MapResidual int64 `json:"map_residual"`
	MapUpper    int64 `json:"map_upper"`

	TotalUpper        int64 `json:"total_upper"`
	MeasuredDeviation int64 `json:"measured_deviation"`
	WithinBound       bool  `json:"within_bound"`
}

// JointOutput is one target joint's fixed-point pose.
type JointOutput struct {
	Joint       string `json:"joint"`
	Rotation    Quat   `json:"rotation"`
	Translation Vec3   `json:"translation"`
}

// FrameResult contains deterministic target poses and precision records.
type FrameResult struct {
	Index      int                    `json:"index"`
	IntervalID string                 `json:"interval_id"`
	Outputs    map[string]JointOutput `json:"outputs"`
	Precision  []PrecisionRecord      `json:"precision"`
}

// IntervalID returns the stable half-open interval identifier for a frame.
func IntervalID(frame, size int) string {
	if size <= 0 {
		size = 4
	}
	start := (frame / size) * size
	return fmt.Sprintf("frames-%04d-%04d", start, start+size)
}

func ceilDivPositive(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func powL1Gain(gain, depth int64) int64 {
	out := int64(1)
	for i := int64(0); i < depth; i++ {
		out *= gain
	}
	return out
}

// ValidateRetargetInput checks mapping presence and both parent chains.
func ValidateRetargetInput(a Asset) *Error {
	srcIdx := a.Skeleton.Index()
	if len(srcIdx) != len(a.Skeleton.Joints) {
		e := errorf(CodeMap, "duplicate source joint id")
		e.Breakpoint = "skeleton.joints"
		return e
	}
	tgtIdx := a.TargetSkeleton.Index()
	if len(tgtIdx) != len(a.TargetSkeleton.Joints) {
		e := errorf(CodeMap, "duplicate target joint id")
		e.Breakpoint = "target_skeleton.joints"
		return e
	}
	for _, j := range a.Skeleton.Joints {
		seen := map[string]bool{j.ID: true}
		parent := j.ParentID
		for parent != "" {
			pj, ok := srcIdx[parent]
			if !ok {
				e := errorf(CodeMap, "source parent chain breakpoint")
				e.Joint = j.ID
				e.Breakpoint = "missing_parent:" + parent
				e.OpA = j.ID
				e.OpB = parent
				return e
			}
			if seen[parent] {
				e := errorf(CodeMap, "parent chain cycle")
				e.Joint = parent
				e.Breakpoint = "cycle_at:" + parent
				return e
			}
			seen[parent] = true
			parent = a.Skeleton.Joints[pj].ParentID
		}
	}
	for _, j := range a.TargetSkeleton.Joints {
		seen := map[string]bool{j.ID: true}
		parent := j.ParentID
		for parent != "" {
			pj, ok := tgtIdx[parent]
			if !ok {
				e := errorf(CodeMap, "target parent chain breakpoint")
				e.Joint = j.ID
				e.Breakpoint = "missing_target_parent:" + parent
				return e
			}
			if seen[parent] {
				e := errorf(CodeMap, "target parent chain cycle")
				e.Joint = parent
				e.Breakpoint = "cycle_at:" + parent
				return e
			}
			seen[parent] = true
			parent = a.TargetSkeleton.Joints[pj].ParentID
		}
	}

	mapByTarget := map[string]MapItem{}
	for _, m := range a.Mapping.Items {
		if m.RotScaleDen <= 0 || m.TransScaleDen <= 0 || m.WeightScale <= 0 {
			e := errorf(CodeMap, "mapping coefficient denominator must be positive")
			e.Joint = m.TargetJoint
			e.OpA = m.RotScaleDen
			e.OpB = m.TransScaleDen
			return e
		}
		if _, ok := srcIdx[m.SourceJoint]; !ok {
			e := errorf(CodeMap, "mapping source joint missing")
			e.Joint = m.TargetJoint
			e.Breakpoint = "source_joint:" + m.SourceJoint
			e.OpA = m.SourceJoint
			return e
		}
		for _, inf := range m.ExtraSources {
			if _, ok := srcIdx[inf.SourceJoint]; !ok {
				e := errorf(CodeMap, "mapping influence source joint missing")
				e.Joint = m.TargetJoint
				e.Breakpoint = "influence:" + inf.SourceJoint
				return e
			}
		}
		mapByTarget[m.TargetJoint] = m
	}
	for _, tj := range a.TargetSkeleton.Joints {
		if _, ok := mapByTarget[tj.ID]; !ok {
			e := errorf(CodeMap, "target joint has no retarget mapping")
			e.Joint = tj.ID
			e.Breakpoint = "mapping:" + tj.ID
			return e
		}
	}
	return nil
}

type normTraceKey struct {
	frame int
	joint string
}

func buildNormTraceIndex(a Asset) map[normTraceKey]QuatNormTrace {
	out := map[normTraceKey]QuatNormTrace{}
	for _, tr := range a.NormTraces {
		out[normTraceKey{tr.Frame, tr.Joint}] = tr
	}
	return out
}

// RetargetFrame computes one deterministic target pose and precision report.
func RetargetFrame(a Asset, frame Frame, intervalID string) (FrameResult, *Error) {
	if intervalID == "" {
		intervalID = IntervalID(frame.Index, a.Precision.IntervalFrames)
	}
	if verr := ValidateRetargetInput(a); verr != nil {
		return FrameResult{}, verr
	}
	p := a.Precision
	s := p.WorkScale
	srcIdx := a.Skeleton.Index()
	traceIdx := buildNormTraceIndex(a)

	localQ := map[string]FQ{}
	localBound := map[string]int64{}
	worldQ := map[string]FQ{}
	worldBound := map[string]int64{}
	chainResidual := map[string]int64{}
	chainUpper := map[string]int64{}
	normResidual := map[string]int64{}

	for _, j := range a.Skeleton.Joints {
		pose, ok := frame.Poses[j.ID]
		if !ok {
			e := errorf(CodeSchema, "frame is missing joint pose")
			e.Joint = j.ID
			e.Frame = &frame.Index
			e.Field = "frames[" + fmt.Sprint(frame.Index) + "].poses." + j.ID
			return FrameResult{}, e
		}
		nq, fq, currentTrace, qerr := NormalizeQuat(pose.Rotation, p, j.ID, frame.Index)
		if qerr != nil {
			return FrameResult{}, qerr
		}
		localQ[j.ID] = fq
		ratio := s / nq.Scale
		quantBound := p.Bound.Components * ((ratio + 1) / 2)
		residual := currentTrace.AdjustmentL1 * ratio
		if loaded, ok := traceIdx[normTraceKey{frame.Index, j.ID}]; ok {
			r := s / loaded.SourceScale
			residual = loaded.AdjustmentL1 * r
			if loaded.NormErrorUnits*r > residual {
				residual = loaded.NormErrorUnits * r
			}
		}
		normResidual[j.ID] = residual
		localBound[j.ID] = quantBound + residual + p.Bound.Components*p.Bound.RoundingUnitCeil

		if j.ParentID == "" {
			worldQ[j.ID] = fq
			worldBound[j.ID] = localBound[j.ID]
			chainResidual[j.ID] = 0
			chainUpper[j.ID] = 0
			continue
		}
		parentQ := worldQ[j.ParentID]
		mul, rem, merr := QMul(parentQ, fq, s)
		if merr != nil {
			return FrameResult{}, withFrame(merr, j.ID, frame.Index)
		}
		var remSum int64
		for _, rv := range rem {
			remSum += abs64(rv)
		}
		roundBound := ceilDivPositive(remSum, s) + p.Bound.Components*p.Bound.RoundingUnitCeil
		pb := worldBound[j.ParentID]
		lb := localBound[j.ID]
		cross := ceilDivPositive(pb*lb, s)
		bound := p.Bound.L1Gain*pb + p.Bound.L1Gain*lb + cross + roundBound
		worldQ[j.ID] = mul
		worldBound[j.ID] = bound
		chainResidual[j.ID] = chainResidual[j.ParentID] + roundBound
		depth := chainDepth(a.Skeleton, j.ID)
		chainUpper[j.ID] = powL1Gain(p.Bound.L1Gain, depth+2) * (chainResidual[j.ID] + p.Bound.Components*p.Bound.RoundingUnitCeil)
	}

	mapByTarget := map[string]MapItem{}
	for _, m := range a.Mapping.Items {
		mapByTarget[m.TargetJoint] = m
	}
	targetWorld := map[string]FQ{}
	targetWorldBound := map[string]int64{}
	targetMapResidual := map[string]int64{}
	targetMapUpper := map[string]int64{}
	outputs := map[string]JointOutput{}
	records := []PrecisionRecord{}

	for _, tj := range a.TargetSkeleton.Joints {
		m := mapByTarget[tj.ID]
		sourceID := m.SourceJoint
		if _, ok := srcIdx[sourceID]; !ok {
			verr := ValidateRetargetInput(a)
			return FrameResult{}, verr
		}
		weightScale := m.WeightScale
		extraWeight := int64(0)
		for _, inf := range m.ExtraSources {
			extraWeight += inf.Weight
		}
		primaryWeight := weightScale - extraWeight
		qs := []FQ{worldQ[sourceID]}
		ws := []int64{primaryWeight}
		weightedBoundNum := primaryWeight * worldBound[sourceID]
		for _, inf := range m.ExtraSources {
			qs = append(qs, worldQ[inf.SourceJoint])
			ws = append(ws, inf.Weight)
			weightedBoundNum += inf.Weight * worldBound[inf.SourceJoint]
		}
		blend, mapRound, merr := WeightedQ(qs, ws, weightScale)
		if merr != nil {
			return FrameResult{}, withFrame(merr, tj.ID, frame.Index)
		}
		normBlend, normRound, nerr := QNormalize(blend, s, tj.ID, frame.Index)
		if nerr != nil {
			return FrameResult{}, nerr
		}
		mapInputBound := ceilDivPositive(weightedBoundNum, weightScale)
		mapProcessBound := p.Bound.L1Gain*mapInputBound + mapRound + normRound + p.Bound.Components*p.Bound.MapRoundingUnitCeil
		targetWorld[tj.ID] = normBlend
		targetWorldBound[tj.ID] = mapProcessBound
		targetMapResidual[tj.ID] = mapRound + normRound
		targetMapUpper[tj.ID] = mapProcessBound

		localTarget := normBlend
		localBound := mapProcessBound
		deriveResidual := int64(0)
		if tj.ParentID != "" {
			parentWorld, ok := targetWorld[tj.ParentID]
			if !ok {
				e := errorf(CodeMap, "target parent world pose unavailable")
				e.Joint = tj.ID
				e.Breakpoint = "processing_order:" + tj.ParentID
				return FrameResult{}, withFrame(e, tj.ID, frame.Index)
			}
			invParent := QConj(parentWorld)
			derived, rem, derr := QMul(invParent, normBlend, s)
			if derr != nil {
				return FrameResult{}, withFrame(derr, tj.ID, frame.Index)
			}
			var remSum int64
			for _, rv := range rem {
				remSum += abs64(rv)
			}
			deriveResidual = ceilDivPositive(remSum, s) + p.Bound.Components*p.Bound.RoundingUnitCeil
			pb := targetWorldBound[tj.ParentID]
			mb := mapProcessBound
			localTarget = derived
			localBound = p.Bound.L1Gain*pb + p.Bound.L1Gain*mb + ceilDivPositive(pb*mb, s) + deriveResidual
		}

		scaled, scaleRem, serr := QScaleVectorPart(localTarget, m.RotScaleNum, m.RotScaleDen, s, tj.ID, frame.Index)
		if serr != nil {
			return FrameResult{}, serr
		}
		var scaleRemSum int64
		for _, rv := range scaleRem {
			scaleRemSum += abs64(rv)
		}
		scaleDivBound := ceilDivPositive(scaleRemSum, m.RotScaleDen) + p.Bound.Components*p.Bound.ScaleRoundingUnitCeil
		finalQ, scaleNormRound, nerr := QNormalize(scaled, s, tj.ID, frame.Index)
		if nerr != nil {
			return FrameResult{}, nerr
		}
		scaleNumerical := scaleDivBound + scaleNormRound
		scaleGain := m.RotScaleNum / m.RotScaleDen
		if m.RotScaleNum%m.RotScaleDen != 0 {
			scaleGain++
		}
		if scaleGain < 1 {
			scaleGain = 1
		}
		totalUpper := p.Bound.L1Gain*scaleGain*localBound + p.Bound.L1Gain*scaleDivBound + scaleNormRound
		scaleUpper := p.Bound.L1Gain*scaleNumerical + p.Bound.Components*p.Bound.ScaleRoundingUnitCeil
		mapUpper := targetMapUpper[tj.ID] + localBound
		cumulativeUpper := worldBound[sourceID] + chainUpper[sourceID] + scaleUpper + mapUpper
		if cumulativeUpper > totalUpper {
			totalUpper = cumulativeUpper
		}

		trans, transRound := retargetTranslation(a, m, frame, sourceID, tj.ID)
		outRot := Quat{W: finalQ.W, X: finalQ.X, Y: finalQ.Y, Z: finalQ.Z, Scale: s}
		outputs[tj.ID] = JointOutput{Joint: tj.ID, Rotation: outRot, Translation: trans}

		truthQ, hasTruth := truthRotation(a, frame.Index, tj.ID)
		measured := int64(0)
		if hasTruth {
			tfq := FQ{W: truthQ.W, X: truthQ.X, Y: truthQ.Y, Z: truthQ.Z}
			if truthQ.Scale != s && truthQ.Scale > 0 {
				r := s / truthQ.Scale
				tfq.W *= r
				tfq.X *= r
				tfq.Y *= r
				tfq.Z *= r
			}
			measured = quatL1(finalQ, tfq)
		}
		rec := PrecisionRecord{
			IntervalID: intervalID, FrameIndex: frame.Index, Joint: tj.ID, SourceJoint: sourceID,
			ChainLength:  chainDepth(a.Skeleton, sourceID),
			NormResidual: normResidual[sourceID], NormUpper: worldBound[sourceID],
			ChainResidual: chainResidual[sourceID], ChainUpper: chainUpper[sourceID],
			ScaleResidual: scaleNumerical, ScaleUpper: scaleUpper,
			TranslationScaleResidual: transRound,
			MapResidual:              targetMapResidual[tj.ID] + deriveResidual, MapUpper: mapUpper,
			TotalUpper: totalUpper, MeasuredDeviation: measured, WithinBound: measured <= totalUpper,
		}
		records = append(records, rec)
		if measured > totalUpper {
			e := errorf(CodeUB, "target rotation deviation exceeds derived upper bound")
			e.Joint = tj.ID
			f := frame.Index
			e.Frame = &f
			e.IntervalID = intervalID
			e.OpA = measured
			e.OpB = totalUpper
			e.Expected = map[string]int64{
				"norm_residual": rec.NormResidual, "norm_upper": rec.NormUpper,
				"chain_residual": rec.ChainResidual, "chain_upper": rec.ChainUpper,
				"scale_residual": rec.ScaleResidual, "scale_upper": rec.ScaleUpper,
				"map_residual": rec.MapResidual, "map_upper": rec.MapUpper,
			}
			return FrameResult{}, e
		}
	}

	return FrameResult{Index: frame.Index, IntervalID: intervalID, Outputs: outputs, Precision: records}, nil
}

func chainDepth(s Skeleton, joint string) int64 {
	idx := s.Index()
	depth := int64(0)
	id := joint
	for {
		i, ok := idx[id]
		if !ok || s.Joints[i].ParentID == "" {
			return depth
		}
		depth++
		id = s.Joints[i].ParentID
	}
}

func retargetTranslation(a Asset, m MapItem, frame Frame, sourceID, targetID string) (Vec3, int64) {
	ds := a.Precision.DistanceScale
	sj := a.Skeleton.Joints[a.Skeleton.Index()[sourceID]]
	base := sj.RestTranslation
	if sourceID == "root" {
		base = frame.RootTranslation
	}
	srcLen := int64(1)
	tgtLen := int64(1)
	if bp, ok := a.Proportions.Bones[targetID]; ok && bp.SourceLength > 0 && bp.TargetLength > 0 {
		srcLen = bp.SourceLength
		tgtLen = bp.TargetLength
	} else if a.Proportions.GlobalDen > 0 {
		srcLen = a.Proportions.GlobalDen
		tgtLen = a.Proportions.GlobalNum
	}
	rounded := FVec{}
	orig := [3]int64{base.X, base.Y, base.Z}
	out := [3]int64{}
	var roundBias int64
	for i, v := range orig {
		scaled, rem1 := roundDiv(v*tgtLen, srcLen)
		out[i], _ = roundDiv(scaled*m.TransScaleNum, m.TransScaleDen)
		roundBias += abs64(rem1 / srcLen)
	}
	rounded = FVec{X: out[0], Y: out[1], Z: out[2]}
	_ = rounded
	return Vec3{X: out[0], Y: out[1], Z: out[2], Scale: ds}, roundBias
}

func truthRotation(a Asset, frame int, target string) (Quat, bool) {
	if frame < 0 || frame >= len(a.Truth.Frames) {
		return Quat{}, false
	}
	q, ok := a.Truth.Frames[frame].TargetRotations[target]
	return q, ok
}

// RetargetAll computes frames in index order without using the scheduler.
func RetargetAll(a Asset) ([]FrameResult, *Error) {
	out := make([]FrameResult, 0, len(a.Clip.Frames))
	for _, f := range a.Clip.Frames {
		fr, err := RetargetFrame(a, f, IntervalID(f.Index, a.Precision.IntervalFrames))
		if err != nil {
			return nil, err
		}
		out = append(out, fr)
	}
	return out, nil
}
