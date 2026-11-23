package gait

import (
	"fmt"
)

const (
	sourceStride = 1.2
)

type GeneratorConfig struct {
	Seed         uint64
	SourceCount  int
	TargetCount  int
	SourceCycles []int
}

func DefaultGeneratorConfig(seed uint64) GeneratorConfig {
	return GeneratorConfig{
		Seed:         seed,
		SourceCount:  2,
		TargetCount:  3,
		SourceCycles: []int{54, 58, 56, 60, 62, 58},
	}
}

type deterministicRand struct {
	state uint64
}

func newDeterministicRand(seed uint64) *deterministicRand {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &deterministicRand{state: seed}
}

func (r *deterministicRand) next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

func (r *deterministicRand) unit() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func (r *deterministicRand) signed(scale float64) float64 {
	return (r.unit()*2 - 1) * scale
}

// GenerateDataset creates all inputs and analytic truth from a fixed seed.
// It deliberately uses only integer/fraction deterministic operations around
// the seed-dependent root bob, so repeated runs produce byte-identical JSON.
func GenerateDataset(cfg GeneratorConfig) (Dataset, error) {
	if cfg.SourceCount <= 0 {
		cfg.SourceCount = 1
	}
	if cfg.TargetCount <= 0 {
		cfg.TargetCount = 1
	}
	if len(cfg.SourceCycles) < cfg.SourceCount*3 {
		perSource := []int{54, 58, 56}
		cfg.SourceCycles = append([]int(nil), perSource...)
		for i := 3; i < cfg.SourceCount*3; i++ {
			cfg.SourceCycles = append(cfg.SourceCycles, 54+2*(i%4))
		}
	}

	ds := Dataset{SchemaVersion: DatasetSchemaVersion}
	ds.Params = Params{
		ParameterVersion:       fmt.Sprintf("params-seed-%d-v1", cfg.Seed),
		AlgorithmVersion:       "phase-locked-retarget-v1",
		ContactHeight:          0.035,
		SwingHeightMin:         0.07,
		SwingSpeedMin:          0.015,
		SwingLiftScale:         1,
		StanceSlideMax:         0.002,
		FootJumpMax:            0.45,
		LandingToleranceFrames: 2,
		PhaseRatioTolerance:    0.06,
		MaxParallelism:         2,
		CycleBufferLimit:       512,
	}

	rng := newDeterministicRand(cfg.Seed)
	cycleOffset := 0
	for s := 0; s < cfg.SourceCount; s++ {
		cycleLens := make([]int, 3)
		copy(cycleLens, cfg.SourceCycles[cycleOffset:cycleOffset+3])
		cycleOffset += 3
		source, phases, truth, err := generateSource(cfg.Seed, s, cycleLens, rng)
		if err != nil {
			return Dataset{}, err
		}
		ds.Sources = append(ds.Sources, source)
		ds.Phases = append(ds.Phases, phases)
		ds.Truths = append(ds.Truths, truth)
		for t := 0; t < cfg.TargetCount; t++ {
			id := fmt.Sprintf("target-%s-%02d", []string{"compact", "human", "stylized"}[t%3], t+1)
			if s > 0 {
				id = fmt.Sprintf("%s-s%d", id, s+1)
			}
			target := generateTarget(id, t, source, rng)
			ds.Targets = append(ds.Targets, target)
		}
	}
	if err := ValidateDataset(ds); err != nil {
		return Dataset{}, err
	}
	return ds, nil
}

func generateSource(seed uint64, index int, cycleLens []int, rng *deterministicRand) (SourceLoop, PhaseTrack, Truth, error) {
	id := fmt.Sprintf("src-walk-%03d", index+1)
	total := 0
	for _, n := range cycleLens {
		total += n
	}
	legs, nodes := sourceSkeleton(id)
	skeleton := Skeleton{ID: id + "-rig", GroundPlane: 0, Nodes: nodes}
	source := SourceLoop{
		ID:         id,
		FrameCount: total,
		Skeleton:   skeleton,
		Legs:       legs,
		Root:       make([]Vec3, total),
		Feet: []FootTrack{
			{Foot: "left", Frames: make([]FootFrame, total)},
			{Foot: "right", Frames: make([]FootFrame, total)},
		},
	}
	position := make([][2]Vec3, total) // [0]=left, [1]=right
	cycleStart := 0
	for c, length := range cycleLens {
		end := cycleStart + length - 1
		source.Cycles = append(source.Cycles, CycleSpan{Index: c, StartFrame: cycleStart, EndFrame: end})
		// Keep support anchors continuous across cycle boundaries. Only the
		// vertical root bob varies with the seed.
		base := float64(c) * sourceStride
		stride := sourceStride
		for local := 0; local < length; local++ {
			frame := cycleStart + local
			u := float64(local) / float64(length)
			rootBob := 0.028 * sin(2*pi()*u+float64(seed%17)*0.01+rng.signed(0.02))
			source.Root[frame] = Vec3{
				X: round6(base + stride*u),
				Y: round6(0.98 + rootBob),
				Z: 0,
			}
			position[frame][0] = sourceFootPosition(base, stride, u, true)
			position[frame][1] = sourceFootPosition(base, stride, u, false)
		}
		cycleStart += length
	}
	for frame := 0; frame < total; frame++ {
		next := frame + 1
		if next >= total {
			next = frame
		}
		for side := 0; side < 2; side++ {
			p := position[frame][side]
			delta := position[next][side].Sub(p)
			if frame == total-1 {
				delta = p.Sub(position[frame-1][side])
			}
			velocity := delta
			source.Feet[side].Frames[frame] = FootFrame{
				Position: p.rounded(),
				Velocity: velocity.rounded(),
			}
		}
	}

	phases := PhaseTrack{SourceID: id, Frames: make([]FramePhase, total)}
	for frame := 0; frame < total; frame++ {
		left := classifyFoot(source.Feet[0].Frames[frame], 0, Dataset{}.Params)
		right := classifyFoot(source.Feet[1].Frames[frame], 0, Dataset{}.Params)
		global := aggregatePhase(left, right)
		phases.Frames[frame] = FramePhase{Global: global, Left: left, Right: right}
	}
	truth := buildTruth(source, phases, cycleLens)
	return source, phases, truth, nil
}

func sourceFootPosition(base, stride, u float64, left bool) Vec3 {
	var x, y float64
	if left {
		switch {
		case u < 0.68:
			x = base + 0.40*stride
			y = 0
		default:
			t := (u - 0.68) / 0.32
			x = base + (0.40+t)*stride
			y = swingHeight(t)
		}
	} else {
		switch {
		case u < 0.18:
			x = base - 0.10*stride
			y = 0
		case u < 0.52:
			t := (u - 0.18) / 0.34
			x = base + (-0.10+t)*stride
			y = swingHeight(t)
		default:
			x = base + 0.90*stride
			y = 0
		}
	}
	z := -0.16
	if left {
		z = 0.16
	}
	return Vec3{X: round6(x), Y: round6(y), Z: z}
}

func swingHeight(t float64) float64 {
	// Non-zero at both ends keeps the threshold-derived phase unambiguous.
	return 0.31 * (0.34 + 0.66*sin(pi()*(t*0.94+0.03)))
}

func classifyFoot(frame FootFrame, ground float64, params Params) string {
	contactHeight := params.ContactHeight
	swingSpeed := params.SwingSpeedMin
	if contactHeight == 0 {
		contactHeight = 0.035
	}
	if swingSpeed == 0 {
		swingSpeed = 0.015
	}
	swingHeight := params.SwingHeightMin
	if swingHeight <= contactHeight {
		swingHeight = 0.07
	}
	h := frame.Position.Y - ground
	// Height is the primary switch. Horizontal speed only promotes a foot that
	// is already just above the contact band; this avoids labelling a landing
	// frame as swing solely because its approach velocity is high.
	if h > swingHeight || (h > contactHeight && frame.Velocity.HorizontalLength() > swingSpeed) {
		return PhaseSwing
	}
	return PhaseStance
}

func aggregatePhase(left, right string) string {
	if left == PhaseStance && right == PhaseStance {
		return PhaseDoubleSupport
	}
	if left == PhaseSwing {
		return PhaseSwing
	}
	return PhaseStance
}

func sourceSkeleton(id string) ([]LegChain, []Bone) {
	nodes := []Bone{
		{Name: "root", Role: "root", LocalOffset: Vec3{}},
		{Name: "hip_L", Parent: "root", Role: "hip", LocalOffset: Vec3{Y: 0, Z: 0.16}},
		{Name: "knee_L", Parent: "hip_L", Role: "knee", LocalOffset: Vec3{Y: -0.43}},
		{Name: "ankle_L", Parent: "knee_L", Role: "ankle", LocalOffset: Vec3{Y: -0.42}},
		{Name: "ball_L", Parent: "ankle_L", Role: "foot", LocalOffset: Vec3{Y: -0.05, X: 0.08}},
		{Name: "toe_L_1", Parent: "ball_L", Role: "toe", LocalOffset: Vec3{X: 0.08, Y: -0.02}},
		{Name: "toe_L_2", Parent: "toe_L_1", Role: "toe", LocalOffset: Vec3{X: 0.06, Y: -0.01}},
		{Name: "hip_R", Parent: "root", Role: "hip", LocalOffset: Vec3{Z: -0.16}},
		{Name: "knee_R", Parent: "hip_R", Role: "knee", LocalOffset: Vec3{Y: -0.43}},
		{Name: "ankle_R", Parent: "knee_R", Role: "ankle", LocalOffset: Vec3{Y: -0.42}},
		{Name: "ball_R", Parent: "ankle_R", Role: "foot", LocalOffset: Vec3{Y: -0.05, X: 0.08}},
		{Name: "toe_R_1", Parent: "ball_R", Role: "toe", LocalOffset: Vec3{X: 0.08, Y: -0.02}},
		{Name: "toe_R_2", Parent: "toe_R_1", Role: "toe", LocalOffset: Vec3{X: 0.06, Y: -0.01}},
	}
	legs := []LegChain{
		{Foot: "left", Hip: "hip_L", Knee: "knee_L", Ankle: "ankle_L", Toes: []string{"toe_L_1", "toe_L_2"}},
		{Foot: "right", Hip: "hip_R", Knee: "knee_R", Ankle: "ankle_R", Toes: []string{"toe_R_1", "toe_R_2"}},
	}
	_ = id
	return legs, nodes
}

func generateTarget(id string, variant int, source SourceLoop, rng *deterministicRand) TargetSkeleton {
	ratios := []float64{0.82, 1.0, 1.18}
	ratio := ratios[variant%len(ratios)]
	toeCounts := []int{1, 2, 3}
	toeCount := toeCounts[variant%len(toeCounts)]
	rom := ROM{Min: -0.55 + 0.04*float64(variant), Max: 0.45 + 0.05*float64(variant)}
	reachMax := round6(0.58 * ratio)
	nodes := scaledSkeletonNodes(ratio, toeCount)
	legs := scaledLegs(toeCount)
	cycleFrames := make([]int, len(source.Cycles))
	for i, c := range source.Cycles {
		n := int(round(float64(c.EndFrame-c.StartFrame+1) * ratio))
		if n < 12 {
			n = 12
		}
		cycleFrames[i] = n
	}
	_ = rng
	return TargetSkeleton{
		ID:             id,
		Skeleton:       Skeleton{ID: id + "-rig", GroundPlane: 0, Nodes: nodes},
		LegLengthRatio: ratio,
		AnkleROM:       rom,
		FootReachable:  HeightRange{Min: 0, Max: reachMax},
		CycleFrames:    cycleFrames,
		Legs:           legs,
	}
}

func scaledSkeletonNodes(ratio float64, toeCount int) []Bone {
	nodes := []Bone{
		{Name: "root", Role: "root", LocalOffset: Vec3{}},
	}
	for _, side := range []struct {
		name string
		z    float64
	}{{"L", 0.16}, {"R", -0.16}} {
		prefix := side.name
		hip := "hip_" + prefix
		knee := "knee_" + prefix
		ankle := "ankle_" + prefix
		ball := "ball_" + prefix
		nodes = append(nodes,
			Bone{Name: hip, Parent: "root", Role: "hip", LocalOffset: Vec3{Z: side.z * ratio}},
			Bone{Name: knee, Parent: hip, Role: "knee", LocalOffset: Vec3{Y: -0.43 * ratio}},
			Bone{Name: ankle, Parent: knee, Role: "ankle", LocalOffset: Vec3{Y: -0.42 * ratio}},
			Bone{Name: ball, Parent: ankle, Role: "foot", LocalOffset: Vec3{Y: -0.05 * ratio, X: 0.08 * ratio}},
		)
		parent := ball
		for i := 0; i < toeCount; i++ {
			toe := fmt.Sprintf("toe_%s_%d", prefix, i+1)
			nodes = append(nodes, Bone{
				Name:   toe,
				Parent: parent,
				Role:   "toe",
				LocalOffset: Vec3{
					X: (0.08 - 0.014*float64(i)) * ratio,
					Y: -0.018 * ratio,
				},
			})
			parent = toe
		}
	}
	return nodes
}

func scaledLegs(toeCount int) []LegChain {
	legs := make([]LegChain, 0, 2)
	for _, side := range []string{"L", "R"} {
		toes := make([]string, toeCount)
		for i := 0; i < toeCount; i++ {
			toes[i] = fmt.Sprintf("toe_%s_%d", side, i+1)
		}
		foot := "left"
		if side == "R" {
			foot = "right"
		}
		legs = append(legs, LegChain{
			Foot:  foot,
			Hip:   "hip_" + side,
			Knee:  "knee_" + side,
			Ankle: "ankle_" + side,
			Toes:  toes,
		})
	}
	return legs
}

func buildTruth(source SourceLoop, phases PhaseTrack, cycleLens []int) Truth {
	truth := Truth{
		SourceID:    source.ID,
		FrameCount:  source.FrameCount,
		CycleCount:  len(source.Cycles),
		GroundPlane: source.Skeleton.GroundPlane,
	}
	for frame := 1; frame < len(phases.Frames); frame++ {
		from := phases.Frames[frame-1].Global
		to := phases.Frames[frame].Global
		if from != to {
			truth.PhaseTransitions = append(truth.PhaseTransitions, PhaseTransition{Frame: frame, From: from, To: to})
		}
	}
	for _, c := range source.Cycles {
		length := c.EndFrame - c.StartFrame + 1
		truth.StrideLengths = append(truth.StrideLengths, StrideLength{Cycle: c.Index, Value: sourceStride})
		truth.RootKeyframes = append(truth.RootKeyframes,
			RootKeyframe{Frame: c.StartFrame, Position: source.Root[c.StartFrame]},
			RootKeyframe{Frame: c.StartFrame + length/2, Position: source.Root[c.StartFrame+length/2]},
			RootKeyframe{Frame: c.EndFrame, Position: source.Root[c.EndFrame]},
		)
		for _, foot := range []string{"left", "right"} {
			stance := -1
			for frame := c.StartFrame; frame <= c.EndFrame; frame++ {
				state := phases.Frames[frame].Left
				if foot == "right" {
					state = phases.Frames[frame].Right
				}
				if state == PhaseStance {
					stance = frame
					break
				}
			}
			if stance >= 0 {
				pos := source.Feet[0].Frames[stance].Position
				if foot == "right" {
					pos = source.Feet[1].Frames[stance].Position
				}
				truth.StanceFootPositions = append(truth.StanceFootPositions, StancePosition{Cycle: c.Index, Foot: foot, Position: pos})
			}
		}
	}
	for frame := 1; frame < len(phases.Frames); frame++ {
		prev := phases.Frames[frame-1]
		cur := phases.Frames[frame]
		for _, side := range []struct {
			name string
			prev string
			cur  string
		}{{"left", prev.Left, cur.Left}, {"right", prev.Right, cur.Right}} {
			if side.prev == side.cur {
				continue
			}
			cycle := cycleForFrame(source.Cycles, frame)
			event := EventLiftoff
			if side.cur == PhaseStance {
				event = EventLand
			}
			truth.FootEvents = append(truth.FootEvents, FootEvent{Frame: frame, Cycle: cycle, Foot: side.name, Type: event})
		}
	}
	_ = cycleLens
	return truth
}

func cycleForFrame(cycles []CycleSpan, frame int) int {
	for _, c := range cycles {
		if frame >= c.StartFrame && frame <= c.EndFrame {
			return c.Index
		}
	}
	return -1
}

func validateFiniteVec(v Vec3, field string, frame int) error {
	if !isFinite(v.X) || !isFinite(v.Y) || !isFinite(v.Z) {
		e := errAt(CodeSchema, fmt.Sprintf("%s contains a non-finite value", field), frame)
		e.Field = field
		return e
	}
	return nil
}
