package gait

import (
	"fmt"
	"sort"
)

type solveContext struct {
	source  SourceLoop
	phases  PhaseTrack
	target  TargetSkeleton
	params  Params
	mapping Mapping
}

type legGeometry struct {
	leg      LegChain
	hip      Bone
	knee     Bone
	ankle    Bone
	ball     Bone
	toes     []Bone
	l1       float64
	l2       float64
	foot     FootTrack
	legIndex int
}

type footPositions struct {
	pos     []Vec3
	mapped  []string
	contact []bool
}

// SolveCycle computes one independent cycle. It never reads neighboring cycle
// outputs, which is what makes worker order and resume paths deterministic.
func solveCycle(ctx solveContext, cycle int) (*CycleOutput, error) {
	mc, err := ctx.mapping.QueryCycle(cycle)
	if err != nil {
		return nil, err
	}
	targetLen := mc.TargetEndFrame - mc.TargetStartFrame + 1
	if targetLen > ctx.params.CycleBufferLimit {
		e := errCycle(CodeBuffer, fmt.Sprintf("cycle %d needs %d frames, limit is %d", cycle, targetLen, ctx.params.CycleBufferLimit), cycle)
		e.Frame = mc.TargetStartFrame
		return nil, e
	}
	geoms, err := buildLegGeometry(ctx.source, ctx.target)
	if err != nil {
		return nil, err
	}
	out := &CycleOutput{
		Cycle:            cycle,
		SourceStartFrame: mc.SourceStartFrame,
		SourceEndFrame:   mc.SourceEndFrame,
		TargetStartFrame: mc.TargetStartFrame,
		TargetEndFrame:   mc.TargetEndFrame,
		Scale:            mc.Scale,
		Frames:           make([]FrameOutput, targetLen),
	}
	feet := make([]footPositions, len(geoms))
	for i := range feet {
		feet[i].pos = make([]Vec3, targetLen)
		feet[i].mapped = make([]string, targetLen)
		feet[i].contact = make([]bool, targetLen)
	}

	for local := 0; local < targetLen; local++ {
		sourceAbs := mc.SourceFrameMap[local]
		root := targetRoot(ctx.source, sourceAbs, ctx.target)

		for legIndex := range geoms {
			geom := &geoms[legIndex]
			mappedPhase := phaseForFoot(ctx.phases.Frames[sourceAbs], geom.foot.Foot, legIndex)
			feet[legIndex].mapped[local] = mappedPhase
			sourceSample := geom.foot.Frames[sourceAbs]
			desired := Vec3{
				X: sourceSample.Position.X * ctx.target.LegLengthRatio,
				Y: ctx.target.Skeleton.GroundPlane,
				Z: sourceSample.Position.Z * ctx.target.LegLengthRatio,
			}
			if mappedPhase == PhaseSwing {
				toeFactor := 1 + 0.02*float64(maxInt(0, len(geom.toes)-1))
				liftScale := ctx.params.SwingLiftScale
				if liftScale == 0 {
					liftScale = 1
				}
				lift := (sourceSample.Position.Y - ctx.source.Skeleton.GroundPlane) *
					ctx.target.LegLengthRatio * toeFactor * liftScale
				desired.Y = ctx.target.Skeleton.GroundPlane + lift
				if desired.Y < ctx.target.FootReachable.Min-1e-9 || desired.Y > ctx.target.FootReachable.Max+1e-9 {
					e := errAt(CodeHeight, fmt.Sprintf(
						"foot %s swing height %.6f is outside reachable range [%.6f, %.6f]",
						geom.foot.Foot, desired.Y, ctx.target.FootReachable.Min, ctx.target.FootReachable.Max),
						mc.TargetStartFrame+local)
					e.Cycle = cycle
					e.Foot = geom.foot.Foot
					e.Field = "foot_reachable_height"
					return nil, e
				}
			}
			feet[legIndex].pos[local] = desired.rounded()
		}

		root = lowerRootForReach(root, geoms, feet, local, ctx.target)
		out.Frames[local] = FrameOutput{
			Frame:       mc.TargetStartFrame + local,
			SourceFrame: sourceAbs,
			Root:        root.rounded(),
			Feet:        make([]SolvedFoot, 0, len(geoms)),
		}
		for legIndex := range geoms {
			geom := &geoms[legIndex]
			speed := footSpeed(feet[legIndex].pos, local, geom.foot, mc, ctx.source, ctx.target)
			detected := classifyFoot(FootFrame{Position: feet[legIndex].pos[local], Velocity: speed},
				ctx.target.Skeleton.GroundPlane, ctx.params)
			feet[legIndex].contact[local] = detected == PhaseStance
			if detected == PhaseStance {
				speed = Vec3{}
			}
			solved := solveLegPose(root, feet[legIndex].pos[local], *geom, detected, ctx.target)
			solved.Speed = speed.rounded()
			out.Frames[local].Feet = append(out.Frames[local].Feet, solved)
		}

		leftDetected, rightDetected := detectedFootPhases(out.Frames[local].Feet, geoms)
		detectedGlobal := aggregatePhase(leftDetected, rightDetected)
		mappedGlobal := ctx.phases.Frames[sourceAbs].Global
		if detectedGlobal == mappedGlobal {
			out.Frames[local].Phase = ctx.phases.Frames[sourceAbs]
		} else {
			out.Frames[local].Phase = FramePhase{Global: detectedGlobal, Left: leftDetected, Right: rightDetected}
		}
	}

	// Enforce the declared support point after classification. Each contiguous
	// stance segment is pinned to its first world position, making slide zero.
	for legIndex := range geoms {
		var anchor Vec3
		inStance := false
		for local := 0; local < targetLen; local++ {
			phase := phaseForFoot(out.Frames[local].Phase, geoms[legIndex].foot.Foot, legIndex)
			isStance := phase == PhaseStance
			if isStance {
				if !inStance {
					anchor = feet[legIndex].pos[local]
					inStance = true
				}
				feet[legIndex].pos[local].X = anchor.X
				feet[legIndex].pos[local].Z = anchor.Z
				feet[legIndex].pos[local].Y = ctx.target.Skeleton.GroundPlane
			} else {
				inStance = false
			}
		}
		for local := 0; local < targetLen; local++ {
			speed := footSpeed(feet[legIndex].pos, local, geoms[legIndex].foot, mc, ctx.source, ctx.target)
			detected := classifyFoot(FootFrame{Position: feet[legIndex].pos[local], Velocity: speed},
				ctx.target.Skeleton.GroundPlane, ctx.params)
			feet[legIndex].contact[local] = detected == PhaseStance
		}
	}

	// Rebuild poses and phase outcomes with the slide-suppressed positions.
	for local := 0; local < targetLen; local++ {
		sourceAbs := mc.SourceFrameMap[local]
		root := lowerRootForReach(out.Frames[local].Root, geoms, feet, local, ctx.target)
		out.Frames[local].Root = root.rounded()
		leftDetected, rightDetected := "", ""
		for legIndex := range geoms {
			geom := &geoms[legIndex]
			speed := footSpeed(feet[legIndex].pos, local, geom.foot, mc, ctx.source, ctx.target)
			detected := classifyFoot(FootFrame{Position: feet[legIndex].pos[local], Velocity: speed},
				ctx.target.Skeleton.GroundPlane, ctx.params)
			feet[legIndex].contact[local] = detected == PhaseStance
			if detected == PhaseStance {
				speed = Vec3{}
			}
			solved := solveLegPose(root, feet[legIndex].pos[local], *geom, detected, ctx.target)
			solved.Speed = speed.rounded()
			out.Frames[local].Feet[legIndex] = solved
			if geom.foot.Foot == "left" || (legIndex == 0 && geom.foot.Foot != "right") {
				leftDetected = detected
			} else {
				rightDetected = detected
			}
		}
		detectedGlobal := aggregatePhase(leftDetected, rightDetected)
		mappedGlobal := ctx.phases.Frames[sourceAbs].Global
		if detectedGlobal == mappedGlobal {
			out.Frames[local].Phase = ctx.phases.Frames[sourceAbs]
		} else {
			out.Frames[local].Phase = FramePhase{Global: detectedGlobal, Left: leftDetected, Right: rightDetected}
		}
	}

	out.Corrections = out.Corrections[:0]
	for local := 0; local < targetLen; local++ {
		sourceAbs := mc.SourceFrameMap[local]
		mapped := ctx.phases.Frames[sourceAbs]
		actual := out.Frames[local].Phase
		if mapped.Global == actual.Global && mapped.Left == actual.Left && mapped.Right == actual.Right {
			continue
		}
		foot := ""
		if mapped.Left != actual.Left {
			foot = geoms[0].foot.Foot
		} else if mapped.Right != actual.Right {
			foot = geoms[1].foot.Foot
		}
		out.Corrections = append(out.Corrections, Correction{
			Frame:          mc.TargetStartFrame + local,
			Cycle:          cycle,
			Foot:           foot,
			Reason:         "height/speed threshold classification differs from mapped phase; applied one local correction",
			MappedPhase:    mapped.Global,
			DetectedPhase:  actual.Global,
			CorrectedPhase: actual.Global,
		})
	}

	out.Events = buildEvents(ctx, mc, geoms, out.Frames, cycle)
	for i := range out.Frames {
		out.Frames[i].Root = out.Frames[i].Root.rounded()
		for j := range out.Frames[i].Feet {
			out.Frames[i].Feet[j].Position = out.Frames[i].Feet[j].Position.rounded()
			out.Frames[i].Feet[j].Speed = out.Frames[i].Feet[j].Speed.rounded()
		}
	}
	sortEvents(out.Events)
	sortCorrections(out.Corrections)
	return out, nil
}

func targetRoot(source SourceLoop, sourceFrame int, target TargetSkeleton) Vec3 {
	src := source.Root[sourceFrame]
	ratio := target.LegLengthRatio
	return Vec3{
		X: src.X * ratio,
		Y: target.Skeleton.GroundPlane + (src.Y-source.Skeleton.GroundPlane)*ratio,
		Z: src.Z * ratio,
	}
}

func lowerRootForReach(root Vec3, geoms []legGeometry, feet []footPositions, local int, target TargetSkeleton) Vec3 {
	adjusted := root
	for _, geom := range geoms {
		hipOffset := geom.hip.LocalOffset
		ankleTarget := feet[geom.legIndex].pos[local].Sub(geom.ball.LocalOffset)
		hip := adjusted.Add(hipOffset)
		distance := ankleTarget.Sub(hip).Length()
		maxReach := geom.l1 + geom.l2 - 0.002
		if distance > maxReach {
			adjusted.Y -= distance - maxReach
		}
	}
	minRootY := target.Skeleton.GroundPlane + 0.05
	if adjusted.Y < minRootY {
		adjusted.Y = minRootY
	}
	return adjusted
}

func buildLegGeometry(source SourceLoop, target TargetSkeleton) ([]legGeometry, error) {
	nodeMap := make(map[string]Bone, len(target.Skeleton.Nodes))
	for _, node := range target.Skeleton.Nodes {
		nodeMap[node.Name] = node
	}
	footMap := make(map[string]FootTrack, len(source.Feet))
	for _, foot := range source.Feet {
		footMap[foot.Foot] = foot
	}
	geoms := make([]legGeometry, 0, len(target.Legs))
	for legIndex, leg := range target.Legs {
		foot, ok := footMap[leg.Foot]
		if !ok {
			return nil, errAt(CodeSchema, fmt.Sprintf("source foot %q not found", leg.Foot), 0)
		}
		hip := nodeMap[leg.Hip]
		knee := nodeMap[leg.Knee]
		ankle := nodeMap[leg.Ankle]
		ballName := findBallNode(target.Skeleton.Nodes, leg)
		ball := nodeMap[ballName]
		toes := make([]Bone, 0, len(leg.Toes))
		for _, toe := range leg.Toes {
			toes = append(toes, nodeMap[toe])
		}
		geoms = append(geoms, legGeometry{
			leg:      leg,
			hip:      hip,
			knee:     knee,
			ankle:    ankle,
			ball:     ball,
			toes:     toes,
			l1:       knee.LocalOffset.Length(),
			l2:       ankle.LocalOffset.Length(),
			foot:     foot,
			legIndex: legIndex,
		})
	}
	return geoms, nil
}

func findBallNode(nodes []Bone, leg LegChain) string {
	for _, node := range nodes {
		if node.Parent == leg.Ankle && node.Role == "foot" {
			return node.Name
		}
	}
	return leg.Ankle
}

func solveLegPose(root, footTarget Vec3, geom legGeometry, phase string, target TargetSkeleton) SolvedFoot {
	hip := root.Add(geom.hip.LocalOffset)
	ankleTarget := footTarget.Sub(geom.ball.LocalOffset)
	vec := ankleTarget.Sub(hip)
	distance := vec.Length()
	maxReach := geom.l1 + geom.l2 - 1e-6
	minReach := abs(geom.l1-geom.l2) + 1e-6
	if distance > maxReach {
		distance = maxReach
	}
	if distance < minReach {
		distance = minReach
	}
	direction := Vec3{}
	if vec.Length() > 1e-9 {
		direction = vec.Scale(1 / vec.Length())
	} else {
		direction = Vec3{Y: -1}
	}
	a := (geom.l1*geom.l1 - geom.l2*geom.l2 + distance*distance) / (2 * distance)
	hSq := geom.l1*geom.l1 - a*a
	if hSq < 0 {
		hSq = 0
	}
	h := sqrt(hSq)
	// Bend forward (+X) relative to the hip-ankle line.
	pole := Vec3{X: 1}
	proj := direction.Scale(direction.X)
	pole = pole.Sub(proj)
	if pole.Length() < 1e-6 {
		pole = Vec3{Z: 1}
		proj = direction.Scale(direction.Z)
		pole = pole.Sub(proj)
	}
	pole = pole.Scale(1 / pole.Length())
	kneePos := hip.Add(direction.Scale(a)).Add(pole.Scale(h))
	kneeFlex := pi() - acos(clamp(
		(geom.l1*geom.l1+geom.l2*geom.l2-distance*distance)/(2*geom.l1*geom.l2), -1, 1))
	hipFlex := atan2(direction.X, -direction.Y)
	ankleFlex := 0.0
	if phase == PhaseSwing {
		ankleFlex = clamp(-0.40*hipFlex+0.22*kneeFlex, target.AnkleROM.Min, target.AnkleROM.Max)
	} else {
		ankleFlex = clamp(0.05*hipFlex, target.AnkleROM.Min, target.AnkleROM.Max)
	}
	joints := []JointState{
		{Bone: geom.hip.Name, Position: hip.rounded(), Angle: round6(hipFlex)},
		{Bone: geom.knee.Name, Position: kneePos.rounded(), Angle: round6(kneeFlex)},
		{Bone: geom.ankle.Name, Position: ankleTarget.rounded(), Angle: round6(ankleFlex)},
	}
	toes := make([]ToeState, 0, len(geom.toes))
	prev := footTarget
	for i, toe := range geom.toes {
		angle := 0.0
		if phase == PhaseSwing {
			angle = clamp(0.10+0.08*float64(i)+0.12*kneeFlex, target.AnkleROM.Min, target.AnkleROM.Max)
		}
		offset := toe.LocalOffset
		pos := Vec3{
			X: prev.X + offset.X*cos(angle),
			Y: prev.Y + offset.Y + offset.X*sin(angle),
			Z: prev.Z + offset.Z,
		}
		toes = append(toes, ToeState{Bone: toe.Name, Position: pos.rounded(), Angle: round6(angle)})
		prev = pos
	}
	return SolvedFoot{
		Foot:     geom.foot.Foot,
		Position: footTarget.rounded(),
		Contact:  phase == PhaseStance,
		Joints:   joints,
		Toes:     toes,
	}
}

func footSpeed(positions []Vec3, local int, foot FootTrack, mc MappedCycle, source SourceLoop, target TargetSkeleton) Vec3 {
	if local > 0 {
		return positions[local].Sub(positions[local-1])
	}
	sourceFrame := mc.SourceFrameMap[0]
	return foot.Frames[sourceFrame].Velocity.Scale(target.LegLengthRatio)
}

func phaseForFoot(frame FramePhase, foot string, legIndex int) string {
	if foot == "left" {
		return frame.Left
	}
	if foot == "right" {
		return frame.Right
	}
	if legIndex == 0 {
		return frame.Left
	}
	return frame.Right
}

func detectedFootPhases(feet []SolvedFoot, geoms []legGeometry) (string, string) {
	left, right := PhaseStance, PhaseStance
	for i, foot := range feet {
		phase := PhaseSwing
		if foot.Contact {
			phase = PhaseStance
		}
		if geoms[i].foot.Foot == "left" || (i == 0 && geoms[i].foot.Foot != "right") {
			left = phase
		} else {
			right = phase
		}
	}
	return left, right
}

func buildEvents(ctx solveContext, mc MappedCycle, geoms []legGeometry, frames []FrameOutput, cycle int) []FootEvent {
	events := make([]FootEvent, 0)
	for legIndex := range geoms {
		prevContact := false
		havePrev := mc.SourceStartFrame > 0
		if havePrev {
			prevPhase := phaseForFoot(ctx.phases.Frames[mc.SourceStartFrame-1], geoms[legIndex].foot.Foot, legIndex)
			prevContact = prevPhase == PhaseStance
		}
		for local := range frames {
			contact := false
			for _, foot := range frames[local].Feet {
				if foot.Foot == geoms[legIndex].foot.Foot {
					contact = foot.Contact
					break
				}
			}
			if havePrev && contact != prevContact {
				eventType := EventLiftoff
				if contact {
					eventType = EventLand
				}
				events = append(events, FootEvent{
					Frame: frames[local].Frame,
					Cycle: cycle,
					Foot:  geoms[legIndex].foot.Foot,
					Type:  eventType,
				})
			}
			prevContact = contact
			havePrev = true
		}
	}
	return events
}

func sortEvents(events []FootEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Frame != events[j].Frame {
			return events[i].Frame < events[j].Frame
		}
		if events[i].Foot != events[j].Foot {
			return events[i].Foot < events[j].Foot
		}
		return events[i].Type < events[j].Type
	})
}

func sortCorrections(corrections []Correction) {
	sort.Slice(corrections, func(i, j int) bool {
		if corrections[i].Frame != corrections[j].Frame {
			return corrections[i].Frame < corrections[j].Frame
		}
		return corrections[i].FieldlessKey() < corrections[j].FieldlessKey()
	})
}

func (c Correction) FieldlessKey() string {
	return c.Foot + "|" + c.Reason + "|" + c.MappedPhase + "|" + c.CorrectedPhase
}

func assembleResult(source SourceLoop, target TargetSkeleton, cycles []*CycleOutput) Result {
	result := Result{SourceID: source.ID, TargetID: target.ID}
	for _, cycle := range cycles {
		if cycle == nil {
			continue
		}
		result.Cycles = append(result.Cycles, *cycle)
		result.Frames = append(result.Frames, cycle.Frames...)
		result.Events = append(result.Events, cycle.Events...)
		result.Corrections = append(result.Corrections, cycle.Corrections...)
	}
	sortEvents(result.Events)
	sortCorrections(result.Corrections)
	result.FrameCount = len(result.Frames)
	return result
}

// FullRetarget is the no-checkpoint path used as the recomputation reference.
func FullRetarget(source SourceLoop, phases PhaseTrack, truth Truth, target TargetSkeleton, params Params) (Result, Mapping, error) {
	if err := ValidateForRetarget(source, phases, truth, target, params); err != nil {
		return Result{}, Mapping{}, err
	}
	mapping, err := BuildMapping(source, phases, target, params)
	if err != nil {
		return Result{}, Mapping{}, err
	}
	ctx := solveContext{source: source, phases: phases, target: target, params: params, mapping: mapping}
	cycles := make([]*CycleOutput, len(source.Cycles))
	for i := range source.Cycles {
		out, err := solveCycle(ctx, i)
		if err != nil {
			return Result{}, mapping, err
		}
		cycles[i] = out
	}
	result := assembleResult(source, target, cycles)
	if err := ValidateEventTimeline(result, truth, mapping, params); err != nil {
		return Result{}, mapping, err
	}
	return result, mapping, nil
}

// ValidateEventTimeline checks analytic truth landing/liftoff events after
// mapping their source frames into target time, allowing the declared tolerance.
func ValidateEventTimeline(result Result, truth Truth, mapping Mapping, params Params) error {
	type mappedTruthEvent struct {
		event FootEvent
		frame int
	}
	mappedTruth := make([]mappedTruthEvent, 0, len(truth.FootEvents))
	for _, expected := range truth.FootEvents {
		cycle := expected.Cycle
		targetFrame, err := mapping.QuerySourceFrame(cycle, expected.Frame)
		if err != nil {
			return err
		}
		mappedTruth = append(mappedTruth, mappedTruthEvent{event: expected, frame: targetFrame})
		found := false
		for _, event := range result.Events {
			if event.Foot != expected.Foot || event.Type != expected.Type {
				continue
			}
			delta := event.Frame - targetFrame
			if delta < 0 {
				delta = -delta
			}
			if delta <= params.LandingToleranceFrames {
				found = true
				break
			}
		}
		if !found {
			e := errAt(CodeDiverge, fmt.Sprintf("mapped %s event for foot %s expected near frame %d but was not found within %d frames",
				expected.Type, expected.Foot, targetFrame, params.LandingToleranceFrames), targetFrame)
			e.Cycle = cycle
			e.Foot = expected.Foot
			e.Field = "event." + expected.Type
			return e
		}
	}
	for _, actual := range result.Events {
		found := false
		for _, expected := range mappedTruth {
			if actual.Foot != expected.event.Foot || actual.Type != expected.event.Type {
				continue
			}
			delta := actual.Frame - expected.frame
			if delta < 0 {
				delta = -delta
			}
			if delta <= params.LandingToleranceFrames {
				found = true
				break
			}
		}
		if !found {
			e := errAt(CodeDiverge, fmt.Sprintf("unexpected %s event for foot %s at frame %d",
				actual.Type, actual.Foot, actual.Frame), actual.Frame)
			e.Cycle = actual.Cycle
			e.Foot = actual.Foot
			e.Field = "event." + actual.Type
			return e
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
