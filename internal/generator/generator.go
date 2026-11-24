package generator

import (
	"fmt"
	"math"
	"sort"

	"retargetlab/internal/geom"
	"retargetlab/internal/kinematics"
	"retargetlab/internal/model"
)

type Options struct {
	Seed       int64
	FrameCount int
	PhaseSize  int
}

func DefaultOptions() Options {
	return Options{Seed: 42, FrameCount: 32, PhaseSize: 8}
}

type splitMix64 struct {
	state uint64
}

func newSplitMix64(seed int64) *splitMix64 {
	return &splitMix64{state: uint64(seed)}
}

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitMix64) float64() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func (r *splitMix64) between(lo, hi float64) float64 {
	return lo + (hi-lo)*r.float64()
}

func Generate(opt Options) (model.Workspace, error) {
	if opt.FrameCount <= 0 {
		return model.Workspace{}, model.Err(model.E_PARAM, "generate", "frame count must be positive")
	}
	if opt.PhaseSize <= 0 {
		return model.Workspace{}, model.Err(model.E_PARAM, "generate", "phase size must be positive")
	}
	rng := newSplitMix64(opt.Seed)
	source := sourceSkeleton()
	clip := sourceClip(source, opt, rng)

	targetSpecs := []struct {
		name  string
		scale float64
	}{
		{name: "small", scale: 0.82},
		{name: "reference", scale: 1.0},
		{name: "large", scale: 1.24},
	}
	targets := make([]model.TargetVariant, 0, len(targetSpecs))
	for _, spec := range targetSpecs {
		targets = append(targets, model.TargetVariant{
			Name:     spec.name,
			Scale:    spec.scale,
			Skeleton: scaledTarget(source, spec.name, spec.scale),
		})
	}
	mapping := retargetMapping(opt)

	workspace := model.Workspace{
		Seed:    opt.Seed,
		Source:  source,
		Clip:    clip,
		Targets: targets,
		Mapping: mapping,
	}
	for _, tv := range targets {
		truth, err := analyticTruth(workspace, tv)
		if err != nil {
			return model.Workspace{}, err
		}
		workspace.Truths = append(workspace.Truths, truth)
	}
	if err := model.ValidateWorkspace(workspace); err != nil {
		return model.Workspace{}, err
	}
	return workspace, nil
}

func sourceSkeleton() model.Skeleton {
	j := func(name string, parent int, x, y, z, limit float64) model.Joint {
		return model.Joint{
			Name: name, Parent: parent,
			LocalOffset:      geom.Vec3{X: x, Y: y, Z: z},
			RotationLimitDeg: limit,
		}
	}
	return model.Skeleton{
		Name: "source_humanoid",
		Joints: []model.Joint{
			j("pelvis", -1, 0, 0, 0, 35),
			j("spine", 0, 0, 0.18, 0, 35),
			j("chest", 1, 0, 0.24, 0, 35),
			j("neck", 2, 0, 0.20, 0, 50),
			j("head", 3, 0, 0.14, 0, 55),
			j("clavicle_l", 2, 0.11, 0.17, 0, 45),
			j("upperarm_l", 5, 0.18, 0, 0, 110),
			j("forearm_l", 6, 0.25, 0, 0, 130),
			j("hand_l", 7, 0.22, 0, 0, 70),
			j("clavicle_r", 2, -0.11, 0.17, 0, 45),
			j("upperarm_r", 9, -0.18, 0, 0, 110),
			j("forearm_r", 10, -0.25, 0, 0, 130),
			j("hand_r", 11, -0.22, 0, 0, 70),
			j("thigh_l", 0, 0.10, -0.08, 0, 115),
			j("shin_l", 13, 0, -0.42, 0, 125),
			j("foot_l", 14, 0, -0.40, 0.06, 55),
			j("toe_l", 15, 0, -0.03, 0.16, 35),
			j("thigh_r", 0, -0.10, -0.08, 0, 115),
			j("shin_r", 17, 0, -0.42, 0, 125),
			j("foot_r", 18, 0, -0.40, 0.06, 55),
			j("toe_r", 19, 0, -0.03, 0.16, 35),
		},
	}
}

func scaledTarget(source model.Skeleton, name string, scale float64) model.Skeleton {
	out := source
	out.Name = "target_" + name
	out.Joints = append([]model.Joint(nil), source.Joints...)
	for i := range out.Joints {
		out.Joints[i].LocalOffset = out.Joints[i].LocalOffset.Mul(scale)
	}
	chest := out.JointIndex("chest")
	handR := out.JointIndex("hand_r")
	out.Joints = append(out.Joints,
		model.Joint{
			Name: "cape_01", Parent: chest,
			LocalOffset:      geom.Vec3{Y: -0.10 * scale, Z: -0.12 * scale},
			RotationLimitDeg: 75,
		},
		model.Joint{
			Name: "weapon_socket", Parent: handR,
			LocalOffset:      geom.Vec3{X: -0.04 * scale, Z: 0.03 * scale},
			RotationLimitDeg: 45,
		},
	)
	return out
}

func sourceClip(source model.Skeleton, opt Options, rng *splitMix64) model.Clip {
	phaseCount := (opt.FrameCount + opt.PhaseSize - 1) / opt.PhaseSize
	names := []string{"idle", "stride_left", "stride_right", "gesture"}
	phases := make([]model.Phase, 0, phaseCount)
	for i := 0; i < phaseCount; i++ {
		start := i * opt.PhaseSize
		end := start + opt.PhaseSize - 1
		if end >= opt.FrameCount {
			end = opt.FrameCount - 1
		}
		name := fmt.Sprintf("phase_%d", i)
		if i < len(names) {
			name = names[i]
		}
		phases = append(phases, model.Phase{Index: i, StartFrame: start, EndFrame: end, Name: name})
	}

	amplitudes := make([]float64, len(source.Joints))
	offsets := make([]float64, len(source.Joints))
	for i := range source.Joints {
		amplitudes[i] = rng.between(0.10, 0.35)
		offsets[i] = rng.between(-math.Pi, math.Pi)
	}
	frames := make([]model.Frame, 0, opt.FrameCount)
	for f := 0; f < opt.FrameCount; f++ {
		phase := float64(f) / float64(opt.PhaseSize)
		rots := make([]geom.Quat, len(source.Joints))
		for i := range source.Joints {
			name := source.Joints[i].Name
			var axis geom.Vec3
			var amp float64
			switch {
			case hasPrefix(name, "thigh"), hasPrefix(name, "shin"):
				axis = geom.Vec3{X: 1}
				amp = 0.28
			case hasPrefix(name, "upperarm"), hasPrefix(name, "forearm"):
				axis = geom.Vec3{X: 0.7, Z: 0.7}
				amp = 0.22
			case name == "pelvis":
				axis = geom.Vec3{Y: 1}
				amp = 0.10
			case name == "spine", name == "chest":
				axis = geom.Vec3{Z: 1}
				amp = 0.10
			case name == "head", name == "neck":
				axis = geom.Vec3{Y: 1}
				amp = 0.08
			default:
				axis = geom.Vec3{Z: 1}
				amp = amplitudes[i] * 0.12
			}
			wave := math.Sin(2*math.Pi*phase + offsets[i])
			if i >= 13 {
				wave = math.Sin(2*math.Pi*phase + offsets[i] + math.Pi)
			}
			rots[i] = geom.QuatFromAxisAngle(axis, amp*wave)
		}
		root := geom.Vec3{
			X: 0.025 * math.Sin(2*math.Pi*phase),
			Y: 0.012 * math.Sin(4*math.Pi*phase),
			Z: 0.16 * float64(f) / float64(max(1, opt.FrameCount-1)),
		}
		frames = append(frames, model.Frame{Index: f, RootTranslation: root, LocalRotations: rots})
	}

	contactSpan := max(4, opt.FrameCount/4)
	left1 := min(2, opt.FrameCount-1)
	left1End := min(left1+contactSpan-1, opt.FrameCount-1)
	right1 := min(left1End+1, opt.FrameCount-1)
	right1End := min(right1+contactSpan-1, opt.FrameCount-1)
	left2 := min(right1End+1, opt.FrameCount-1)
	left2End := min(left2+contactSpan-1, opt.FrameCount-1)
	right2 := min(left2End+1, opt.FrameCount-1)
	right2End := opt.FrameCount - 1
	contacts := []model.ContactInterval{
		{Foot: "foot_l", Start: left1, End: left1End},
		{Foot: "foot_r", Start: right1, End: right1End},
	}
	if left2 <= left2End {
		contacts = append(contacts, model.ContactInterval{Foot: "foot_l", Start: left2, End: left2End})
	}
	if right2 <= right2End {
		contacts = append(contacts, model.ContactInterval{Foot: "foot_r", Start: right2, End: right2End})
	}
	sort.Slice(contacts, func(i, j int) bool {
		if contacts[i].Start == contacts[j].Start {
			return contacts[i].Foot < contacts[j].Foot
		}
		return contacts[i].Start < contacts[j].Start
	})
	return model.Clip{Name: "deterministic_locomotion", Frames: frames, Contacts: contacts, Phases: phases}
}

func retargetMapping(opt Options) model.Mapping {
	core := []string{
		"pelvis", "spine", "chest", "neck", "clavicle_l", "upperarm_l", "forearm_l", "hand_l",
		"clavicle_r", "upperarm_r", "forearm_r", "hand_r", "thigh_l", "shin_l", "foot_l", "toe_l",
		"thigh_r", "shin_r", "foot_r", "toe_r",
	}
	rules := make([]model.JointRule, 0, len(core)+1)
	for _, name := range core {
		phases := []int(nil)
		switch {
		case hasPrefix(name, "thigh"), hasPrefix(name, "shin"), hasPrefix(name, "foot"), hasPrefix(name, "toe"):
			phases = []int{0, 1, 2, 3}
		case hasPrefix(name, "upperarm"), hasPrefix(name, "forearm"), hasPrefix(name, "hand"):
			phases = []int{3}
		}
		rules = append(rules, model.JointRule{Source: name, Targets: []string{name}, Weight: 1, Phases: phases})
	}
	// One source drives two targets: the normal head and the cape attachment.
	rules = append(rules, model.JointRule{
		Source:  "head",
		Targets: []string{"head", "cape_01"},
		Weight:  1,
		Phases:  []int{0, 1, 2, 3},
	})

	return model.Mapping{
		Name:  "humanoid_to_humanoid",
		Rules: rules,
		Missing: []model.MissingRule{
			{Joint: "weapon_socket", Policy: model.MissingInheritParent, SourceAncestor: "hand_r"},
		},
		Effectors: []model.EffectorRule{
			{Target: "hand_l", Source: "hand_l", Tolerance: 1e-6, MaxIterations: 24, Phases: []int{0, 1, 2, 3}},
			{Target: "hand_r", Source: "hand_r", Tolerance: 1e-6, MaxIterations: 24, Phases: []int{0, 1, 2, 3}},
			{Target: "foot_l", Source: "foot_l", Tolerance: 1e-6, MaxIterations: 24, Phases: []int{0, 1, 2, 3}},
			{Target: "foot_r", Source: "foot_r", Tolerance: 1e-6, MaxIterations: 24, Phases: []int{0, 1, 2, 3}},
		},
		ScaleDefaults: []model.ScaleCoefficient{
			{Key: "body", Value: 1, Joint: "pelvis"},
			{Key: "stride", Value: 1, Joint: "pelvis", Phases: []int{1, 2}},
			{Key: "gesture", Value: 1, Joint: "pelvis", Phases: []int{3}},
		},
		PhaseSize: opt.PhaseSize,
	}
}

func analyticTruth(w model.Workspace, tv model.TargetVariant) (model.TargetTruth, error) {
	target := tv.Skeleton
	bodyScale := target.Height() / w.Source.Height()
	keyFrames := []int{0, len(w.Clip.Frames) / 4, len(w.Clip.Frames) / 2, (3 * len(w.Clip.Frames)) / 4, len(w.Clip.Frames) - 1}
	keyFrames = uniqueInts(keyFrames)
	truth := model.TargetTruth{
		Target:            tv.Name,
		Tolerance:         1e-6,
		SourceBoneLengths: kinematics.BoneLengths(w.Source),
		BoneLengths:       kinematics.BoneLengths(target),
		Contacts:          append([]model.ContactInterval(nil), w.Clip.Contacts...),
	}
	for _, f := range keyFrames {
		locals, err := deriveAnalyticRotations(w.Source, target, w.Mapping, w.Clip.Frames[f])
		if err != nil {
			return model.TargetTruth{}, err
		}
		root := w.Clip.Frames[f].RootTranslation.Mul(frameScaleProduct(w.Mapping, w.Clip, f, bodyScale, nil))
		root, _, feet, err := kinematics.ContactCorrectedRoot(target, root, locals, w.Clip.Contacts, f)
		if err != nil {
			return model.TargetTruth{}, err
		}
		wp, err := kinematics.Forward(target, root, locals)
		if err != nil {
			return model.TargetTruth{}, err
		}
		tf := model.TruthFrame{Frame: f, Root: root, Contacts: feet}
		for i, j := range target.Joints {
			tf.World = append(tf.World, model.TruthJoint{Joint: j.Name, Position: wp.Positions[i]})
		}
		truth.KeyFrames = append(truth.KeyFrames, tf)
	}
	return truth, nil
}

func deriveAnalyticRotations(source, target model.Skeleton, m model.Mapping, frame model.Frame) ([]geom.Quat, error) {
	locals := make([]geom.Quat, len(target.Joints))
	for i := range locals {
		locals[i] = geom.IdentityQuat()
	}
	targetRule := make(map[string]int, len(m.Rules))
	for ri, r := range m.Rules {
		for _, tn := range r.Targets {
			targetRule[tn] = ri
		}
	}
	order, err := kinematics.TopologicalOrder(target)
	if err != nil {
		return nil, err
	}
	missing := make(map[string]model.MissingRule, len(m.Missing))
	for _, mr := range m.Missing {
		missing[mr.Joint] = mr
	}
	for _, ti := range order {
		tj := target.Joints[ti]
		if ri, ok := targetRule[tj.Name]; ok {
			ri := m.Rules[ri]
			src := source.JointIndex(ri.Source)
			if src < 0 {
				return nil, fmt.Errorf("unknown source joint %q", ri.Source)
			}
			q := frame.LocalRotations[src].Normalize()
			if ri.Weight > 0 && ri.Weight < 1 {
				q = geom.Slerp(geom.IdentityQuat(), q, ri.Weight)
			}
			locals[ti] = q
			continue
		}
		mr, ok := missing[tj.Name]
		if !ok {
			return nil, fmt.Errorf("target joint %q has no mapping or fallback", tj.Name)
		}
		switch mr.Policy {
		case model.MissingIdentity:
			locals[ti] = geom.IdentityQuat()
		case model.MissingInheritParent:
			locals[ti] = locals[tj.Parent]
		case model.MissingNearestMappedAncestor:
			cur := tj.Parent
			for cur >= 0 {
				if _, mapped := targetRule[target.Joints[cur].Name]; mapped {
					locals[ti] = locals[cur]
					break
				}
				cur = target.Joints[cur].Parent
			}
		default:
			return nil, fmt.Errorf("unknown missing policy %q", mr.Policy)
		}
	}
	return locals, nil
}

func frameScaleProduct(m model.Mapping, clip model.Clip, frame int, bodyScale float64, overrides map[string]float64) float64 {
	phase := clip.PhaseForFrame(frame)
	out := bodyScale
	for _, s := range m.ScaleDefaults {
		if !phaseActive(s.Phases, phase) {
			continue
		}
		value := s.Value
		if overrides != nil {
			if v, ok := overrides[s.Key]; ok {
				value = v
			}
		}
		out *= value
	}
	return out
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

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func uniqueInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}
