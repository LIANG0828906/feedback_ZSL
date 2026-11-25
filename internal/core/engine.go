package core

import (
	"math"
)

type Engine struct {
	Clip          Clip
	Target        Skeleton
	Mapping       *Mapping
	Params        Parameters
	ParamsVersion string
	TargetIndex   *SkeletonIndex
}

func NormalizeParameters(p Parameters) Parameters {
	d := DefaultParameters()
	if p.QuantStep <= 0 {
		p.QuantStep = d.QuantStep
	}
	if p.GlobalScaleMultiplier <= 0 {
		p.GlobalScaleMultiplier = d.GlobalScaleMultiplier
	}
	if p.AngleTolerance <= 0 {
		p.AngleTolerance = d.AngleTolerance
	}
	if p.ReachTolerance <= 0 {
		p.ReachTolerance = d.ReachTolerance
	}
	if p.MaxIterations <= 0 {
		p.MaxIterations = d.MaxIterations
	}
	if p.ContactDriftLimit <= 0 {
		p.ContactDriftLimit = d.ContactDriftLimit
	}
	if p.ContactFrameTolerance < 0 {
		p.ContactFrameTolerance = d.ContactFrameTolerance
	}
	if p.SmoothingAlpha <= 0 || p.SmoothingAlpha > 1 {
		p.SmoothingAlpha = d.SmoothingAlpha
	}
	return p
}

func NewEngine(clip Clip, target Skeleton, params Parameters) (*Engine, *Error) {
	params = NormalizeParameters(params)
	mapping, err := BuildMapping(clip.Skeleton, target, params)
	if err != nil {
		return nil, err
	}
	idx, err := NewSkeletonIndex(target)
	if err != nil {
		return nil, err
	}
	params.MappingID = mapping.ID
	versionInput := struct {
		Parameters  Parameters `json:"parameters"`
		MappingHash string     `json:"mapping_hash"`
	}{params, mapping.HashValue}
	paramsVersion := "pv-" + CanonicalHash(versionInput)[:16]
	return &Engine{Clip: clip, Target: target, Mapping: mapping, Params: params, ParamsVersion: paramsVersion, TargetIndex: idx}, nil
}

type solvedFrame struct {
	result        FrameResult
	rawMapped     []Quat
	quantMapped   []Quat
	rawRoot       Vec3
	solve         *SolveOutput
	rawPose       *Pose
	quantPose     *Pose
	quantResidual map[string]float64
}

func (e *Engine) solveFrameData(frameIndex int) (*solvedFrame, *Error) {
	if frameIndex < 0 || frameIndex >= len(e.Clip.Frames) {
		return nil, &Error{Code: ECodeSchema, Frame: frameIndex, Message: "frame index outside clip"}
	}
	sourceFrame := e.Clip.Frames[frameIndex]
	targetCount := len(e.Target.Joints)
	rawMapped := make([]Quat, targetCount)
	quantMapped := make([]Quat, targetCount)
	quantResidual := map[string]float64{}

	for ti, tj := range e.Target.Joints {
		sourceName := e.Mapping.TargetSource[tj.Name]
		if sourceName == "" {
			rawMapped[ti] = Quat{1, 0, 0, 0}
			quantMapped[ti] = Quat{1, 0, 0, 0}
			quantResidual[tj.Name] = 0
			continue
		}
		si, ok := e.Clip.Skeleton.IndexByName(sourceName)
		if !ok {
			return nil, &Error{Code: ECodeMap, Frame: frameIndex, Joint: sourceName, Message: "mapped source joint disappeared"}
		}
		raw := sourceFrame.Rotations[si].Normalized()
		quant, residual := QuantizeQuat(raw, e.Params.QuantStep)
		rawMapped[ti] = raw
		quantMapped[ti] = quant
		quantResidual[tj.Name] = residual
	}

	rawRoot := sourceFrame.Root.Scale(e.Mapping.GlobalScale)
	rawPose, ferr := ForwardKinematics(e.Target, rawRoot, rawMapped)
	if ferr != nil {
		return nil, ferr
	}
	quantPose, ferr := ForwardKinematics(e.Target, rawRoot, quantMapped)
	if ferr != nil {
		return nil, ferr
	}
	solved, serr := SolveFrame(frameIndex, e.Target, quantMapped, e.Params)
	if serr != nil {
		return nil, serr
	}
	solvedRootPose, ferr := ForwardKinematics(e.Target, rawRoot, solved.Local)
	if ferr != nil {
		return nil, ferr
	}

	result := FrameResult{
		ClipID: e.Clip.ID, TargetID: e.Target.ID, Frame: frameIndex,
		State: string(StateSolving), Rotations: append([]Quat{}, solved.Local...),
		Root: rawRoot, WorldPositions: []Vec3{}, ParamsVersion: e.ParamsVersion, AlgoVersion: AlgorithmVersion,
	}
	result.Stats = FrameStats{
		Iterations:        solved.Iterations,
		MaxAngleViolation: solved.MaxAngle,
		MaxReachViolation: solved.MaxReach,
		MaxViolation:      math.Max(solved.MaxAngle, solved.MaxReach),
		Converged:         solved.Converged,
		JointCount:        targetCount,
		Depth:             e.maxDepth(),
	}
	sf := &solvedFrame{
		result: result, rawMapped: rawMapped, quantMapped: quantMapped, rawRoot: rawRoot,
		solve: solved, rawPose: rawPose, quantPose: quantPose, quantResidual: quantResidual,
	}
	sf.result.Diagnostics = e.buildDiagnostics(sf)
	sf.result.WorldPositions = solvedRootPose.WorldPos
	return sf, nil
}

func (e *Engine) maxDepth() int {
	max := 0
	for _, d := range e.TargetIndex.Depth {
		if d > max {
			max = d
		}
	}
	return max
}

func (e *Engine) buildDiagnostics(sf *solvedFrame) []JointDiagnostic {
	n := len(e.Target.Joints)
	out := make([]JointDiagnostic, n)
	quantOri := make([]float64, n)
	quantPosBound := make([]float64, n)
	scaleCumBound := make([]float64, n)
	projOri := make([]float64, n)
	projCumBound := make([]float64, n)
	eps := 1e-14

	for _, i := range e.TargetIndex.Order {
		j := e.Target.Joints[i]
		parent := -1
		if j.Parent != "" {
			parent = e.TargetIndex.ByName[j.Parent]
		}
		depth := e.TargetIndex.Depth[j.Name]
		sourceName := e.Mapping.TargetSource[j.Name]
		entry, hasEntry := e.Mapping.QueryTarget(j.Name)
		strategy := ""
		if hasEntry {
			strategy = entry.Strategy
		} else if sourceName == "" {
			strategy = "identity_extra_segment"
		}
		ratio := e.Mapping.GlobalScale
		if r, ok := e.Mapping.ScaleRatios[sourceName+"->"+j.Name]; ok {
			ratio = r
		}
		sourceLen := 0.0
		if sourceName != "" {
			if si, ok := e.Clip.Skeleton.IndexByName(sourceName); ok {
				sourceLen = e.Clip.Skeleton.Joints[si].Offset.Norm()
			}
		}
		scaleResidual := sourceLen * math.Abs(ratio-e.Mapping.GlobalScale)
		scaleLocalBound := scaleResidual + eps*math.Max(1, j.Offset.Norm())
		if parent >= 0 {
			scaleCumBound[i] = scaleCumBound[parent] + scaleLocalBound
		} else {
			scaleCumBound[i] = scaleLocalBound
		}

		qResidual := sf.quantResidual[j.Name]
		qAngularBound := 8*e.Params.QuantStep + eps
		if parent >= 0 {
			quantOri[i] = quantOri[parent] + qAngularBound
			quantPosBound[i] = quantPosBound[parent] + j.Offset.Norm()*quantOri[parent]
		} else {
			quantOri[i] = qAngularBound
			quantPosBound[i] = 0
		}

		angleResidual := sf.solve.AngleResidual[j.Name]
		reachResidual := sf.solve.ReachResidual[j.Name]
		// Deterministic diagnostic envelope: quantization granularity is amplified
		// by every projection iteration. It intentionally does not read truth.
		ratioAmplification := 1.0 + math.Abs(ratio-e.Mapping.GlobalScale)
		projAngularBound := 16*e.Params.QuantStep*float64(sf.solve.Iterations+1)*math.Pow(ratioAmplification, float64(depth+1)) +
			eps*float64(sf.solve.Iterations+1)
		if parent >= 0 {
			projOri[i] = projOri[parent] + projAngularBound
			projCumBound[i] = projCumBound[parent] + j.Offset.Norm()*projOri[parent]
		} else {
			projOri[i] = projAngularBound
			projCumBound[i] = 0
		}

		fkObserved := 0.0
		if sf.rawPose != nil && sf.quantPose != nil {
			fkObserved = sf.rawPose.WorldPos[i].Sub(sf.quantPose.WorldPos[i]).Norm()
		}
		arithmeticBound := eps * float64(depth+2) * (j.Offset.Norm() + 1) * math.Pow(ratioAmplification, float64(depth+1))
		fkBound := quantPosBound[i] + scaleCumBound[i] + arithmeticBound
		projectionBound := projCumBound[i] + arithmeticBound*float64(sf.solve.Iterations+1)

		out[i] = JointDiagnostic{
			Joint: j.Name, Parent: j.Parent, Depth: depth, SourceJoint: sourceName, MappingStrategy: strategy,
			Iterations: sf.solve.Iterations, QuantStep: e.Params.QuantStep, QuantResidual: qResidual,
			QuantBound: quantPosBound[i], ScaleRatio: ratio, ScaleResidual: scaleResidual, ScaleBound: scaleCumBound[i],
			FKObservedError: fkObserved, FKBound: fkBound, AngleResidual: angleResidual, ReachResidual: reachResidual,
			ProjectionResidual: math.Max(angleResidual, reachResidual), ProjectionBound: projectionBound,
			TotalBound: fkBound + projectionBound,
		}
	}
	return out
}

type ContactProcessor struct {
	engine      *Engine
	anchors     map[string]Vec3
	smooth      Vec3
	intervalAt  [][]int
	targetJoint []string
	labels      []string
}

func NewContactProcessor(e *Engine, restored *FrameResult) *ContactProcessor {
	cp := &ContactProcessor{engine: e, anchors: map[string]Vec3{}, intervalAt: make([][]int, len(e.Clip.ContactTracks))}
	for ti, track := range e.Clip.ContactTracks {
		intervals := ExtractIntervals(track.Values)
		cp.intervalAt[ti] = make([]int, len(track.Values))
		for i := range cp.intervalAt[ti] {
			cp.intervalAt[ti][i] = -1
		}
		for ii, in := range intervals {
			for f := in[0]; f <= in[1]; f++ {
				cp.intervalAt[ti][f] = ii
			}
		}
		target := ""
		if entry, ok := e.Mapping.QuerySource(track.Joint); ok && len(entry.Targets) > 0 {
			target = entry.Targets[0]
		}
		cp.targetJoint = append(cp.targetJoint, target)
		cp.labels = append(cp.labels, track.Foot)
	}
	if restored != nil {
		cp.smooth = restored.Contacts.SmoothedCorrection
		for label, anchor := range restored.Contacts.Anchors {
			cp.anchors[label] = anchor
		}
	}
	return cp
}

func (cp *ContactProcessor) Apply(frameIndex int, sf *solvedFrame) *Error {
	e := cp.engine
	rawPose, err := ForwardKinematics(e.Target, sf.rawRoot, sf.result.Rotations)
	if err != nil {
		return err
	}
	activeLabels := []string{}
	activeAnchors := map[string]Vec3{}
	var deltaSum Vec3
	deltaCount := 0

	for ti, track := range e.Clip.ContactTracks {
		if frameIndex >= len(track.Values) || !track.Values[frameIndex] || cp.targetJoint[ti] == "" {
			if !track.Values[frameIndex] {
				delete(cp.anchors, track.Foot)
			}
			continue
		}
		label := track.Foot
		targetName := cp.targetJoint[ti]
		ji, ok := e.TargetIndex.ByName[targetName]
		if !ok {
			return &Error{Code: ECodeMap, Frame: frameIndex, Joint: targetName, Message: "contact target joint missing"}
		}
		anchor, ok := cp.anchors[label]
		if !ok {
			anchor = rawPose.WorldPos[ji]
			anchor[1] = e.Target.GroundY
			cp.anchors[label] = anchor
		}
		current := rawPose.WorldPos[ji]
		delta := anchor.Sub(current)
		deltaSum = deltaSum.Add(delta)
		deltaCount++
		activeLabels = append(activeLabels, label)
		activeAnchors[label] = anchor
	}

	targetCorrection := Vec3{}
	if deltaCount > 0 {
		targetCorrection = deltaSum.Scale(1 / float64(deltaCount))
	} else {
		cp.anchors = map[string]Vec3{}
	}
	alpha := e.Params.SmoothingAlpha
	if deltaCount > 0 {
		cp.smooth = cp.smooth.Scale(1 - alpha).Add(targetCorrection.Scale(alpha))
	} else {
		cp.smooth = cp.smooth.Scale(1 - alpha)
	}

	root := sf.rawRoot.Add(cp.smooth)
	smoothPose, ferr := ForwardKinematics(e.Target, root, sf.result.Rotations)
	if ferr != nil {
		return ferr
	}
	residualSum := Vec3{}
	residualCount := 0
	for ti, track := range e.Clip.ContactTracks {
		if !track.Values[frameIndex] || cp.targetJoint[ti] == "" {
			continue
		}
		label := track.Foot
		ji := e.TargetIndex.ByName[cp.targetJoint[ti]]
		residualSum = residualSum.Add(cp.anchors[label].Sub(smoothPose.WorldPos[ji]))
		residualCount++
	}
	projection := Vec3{}
	if residualCount > 0 {
		projection = residualSum.Scale(1 / float64(residualCount))
		root = root.Add(projection)
	}

	finalPose, ferr := ForwardKinematics(e.Target, root, sf.result.Rotations)
	if ferr != nil {
		return ferr
	}
	contact := FrameContact{Active: activeLabels, Anchors: activeAnchors, Correction: root.Sub(sf.rawRoot), SmoothedCorrection: cp.smooth}
	maxDrift := 0.0
	for ti, track := range e.Clip.ContactTracks {
		if !track.Values[frameIndex] || cp.targetJoint[ti] == "" {
			continue
		}
		label := track.Foot
		ji := e.TargetIndex.ByName[cp.targetJoint[ti]]
		pos := finalPose.WorldPos[ji]
		drift := pos.Sub(cp.anchors[label]).Norm()
		contact.Drifts = append(contact.Drifts, ContactDrift{Frame: frameIndex, Foot: label, Joint: cp.targetJoint[ti], Anchor: cp.anchors[label], Position: pos, Drift: drift})
		if drift > maxDrift {
			maxDrift = drift
		}
	}
	contact.MaxDrift = maxDrift
	sf.result.Root = root
	sf.result.Contacts = contact
	sf.result.WorldPositions = finalPose.WorldPos
	sf.result.Stats.MaxContactDrift = maxDrift
	sf.result.Stats.ContactViolations = len(contact.Drifts)
	if maxDrift > e.Params.ContactDriftLimit {
		sf.result.Stats.ContactViolations = 1
		for _, d := range contact.Drifts {
			if d.Drift <= e.Params.ContactDriftLimit {
				sf.result.Stats.ContactViolations--
			}
		}
	}
	sf.result.ContentHash = sf.result.ComputeHash()
	return nil
}

func (e *Engine) FinalizeContactCheck(frames []FrameResult, truth *Truth) ContactCheck {
	check := ContactCheck{
		OK: true, DriftLimit: e.Params.ContactDriftLimit, FrameTolerance: e.Params.ContactFrameTolerance,
		ExpectedIntervals: map[string][]Interval{}, ObservedIntervals: map[string][]Interval{}, IntervalFrameDiffs: map[string]int{},
	}
	observed := map[string][]bool{}
	for _, track := range e.Clip.ContactTracks {
		observed[track.Foot] = make([]bool, len(frames))
		if truth != nil {
			check.ExpectedIntervals[track.Foot] = truth.ContactIntervals[track.Foot]
		}
	}
	for _, f := range frames {
		for _, label := range f.Contacts.Active {
			if row, ok := observed[label]; ok {
				row[f.Frame] = true
			}
		}
		for _, d := range f.Contacts.Drifts {
			if d.Drift > e.Params.ContactDriftLimit {
				check.OK = false
				check.DriftViolations = append(check.DriftViolations, d)
			}
			if d.Drift > check.MaxDrift {
				check.MaxDrift = d.Drift
			}
		}
	}
	for label, values := range observed {
		intervals := ExtractIntervals(values)
		check.ObservedIntervals[label] = intervals
		expected := check.ExpectedIntervals[label]
		diff := maxIntervalEndpointDiff(expected, intervals)
		check.IntervalFrameDiffs[label] = diff
		if diff > e.Params.ContactFrameTolerance {
			check.OK = false
		}
	}
	return check
}

func maxIntervalEndpointDiff(expected, observed []Interval) int {
	if len(expected) != len(observed) {
		return 1 << 20
	}
	maxDiff := 0
	for i := range expected {
		for k := 0; k < 2; k++ {
			d := expected[i][k] - observed[i][k]
			if d < 0 {
				d = -d
			}
			if d > maxDiff {
				maxDiff = d
			}
		}
	}
	return maxDiff
}

func (sk Skeleton) IndexByName(name string) (int, bool) {
	for i, j := range sk.Joints {
		if j.Name == name {
			return i, true
		}
	}
	return -1, false
}
