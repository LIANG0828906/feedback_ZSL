package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
)

const (
	SourceScale = int64(100_000)
	TruthScale  = int64(1_000_000_000)
	FrameCount  = 24
	FPS         = int64(24)
)

type SampleSet struct {
	Studio []byte
	Engine []byte
	Legacy []byte
}

type baseJoint struct {
	ID             string
	Parent         string
	Rest           model.FixedVec3
	MinDegreeMicro int64
	MaxDegreeMicro int64
}

type baseTarget struct {
	ID         string
	Parent     string
	Rest       model.FixedVec3
	Proportion model.Ratio
}

type baseFrame struct {
	Index  int
	Root   model.FixedVec3
	Locals []model.Quat
}

type sampleBase struct {
	joints  []baseJoint
	frames  []baseFrame
	target  []baseTarget
	mapping []model.Mapping
	truth   []model.TruthSample
}

func Generate(seed int64) (*SampleSet, error) {
	base, err := buildBase(seed)
	if err != nil {
		return nil, err
	}
	studio, err := encodeStudio(base)
	if err != nil {
		return nil, err
	}
	engine, err := encodeEngine(base)
	if err != nil {
		return nil, err
	}
	legacy, err := encodeLegacy(base)
	if err != nil {
		return nil, err
	}
	return &SampleSet{Studio: studio, Engine: engine, Legacy: legacy}, nil
}

func buildBase(seed int64) (*sampleBase, error) {
	rng := rand.New(rand.NewSource(seed))
	joints := []baseJoint{
		{ID: "root", Parent: "", Rest: vec(0, 0, 0, SourceScale), MinDegreeMicro: -180_000_000, MaxDegreeMicro: 180_000_000},
		{ID: "spine", Parent: "root", Rest: vec(0, 100000, 0, SourceScale), MinDegreeMicro: -35_000_000, MaxDegreeMicro: 35_000_000},
		{ID: "chest", Parent: "spine", Rest: vec(0, 120000, 0, SourceScale), MinDegreeMicro: -30_000_000, MaxDegreeMicro: 30_000_000},
		{ID: "neck", Parent: "chest", Rest: vec(0, 80000, 0, SourceScale), MinDegreeMicro: -45_000_000, MaxDegreeMicro: 45_000_000},
		{ID: "arm_l", Parent: "chest", Rest: vec(90000, 50000, 0, SourceScale), MinDegreeMicro: -80_000_000, MaxDegreeMicro: 80_000_000},
		{ID: "arm_r", Parent: "chest", Rest: vec(-90000, 50000, 0, SourceScale), MinDegreeMicro: -80_000_000, MaxDegreeMicro: 80_000_000},
	}
	target := []baseTarget{
		{ID: "root_t", Parent: "", Rest: vec(0, 0, 0, SourceScale), Proportion: model.Ratio{Num: 3, Den: 2}},
		{ID: "spine_t", Parent: "root_t", Rest: vec(0, 133333, 0, SourceScale), Proportion: model.Ratio{Num: 4, Den: 3}},
		{ID: "chest_t", Parent: "spine_t", Rest: vec(0, 90000, 0, SourceScale), Proportion: model.Ratio{Num: 3, Den: 4}},
		{ID: "neck_t", Parent: "chest_t", Rest: vec(0, 80000, 0, SourceScale), Proportion: model.Ratio{Num: 1, Den: 1}},
		{ID: "arm_l_t", Parent: "chest_t", Rest: vec(112500, 62500, 0, SourceScale), Proportion: model.Ratio{Num: 5, Den: 4}},
		{ID: "arm_r_t", Parent: "chest_t", Rest: vec(-72000, 40000, 0, SourceScale), Proportion: model.Ratio{Num: 4, Den: 5}},
	}
	mapping := []model.Mapping{
		{ID: "m_root", Source: "root", Target: "root_t", RotationScale: model.Ratio{Num: 1, Den: 1}, TranslationScale: model.Ratio{Num: 3, Den: 2}},
		{ID: "m_spine", Source: "spine", Target: "spine_t", RotationScale: model.Ratio{Num: 1, Den: 1}, TranslationScale: model.Ratio{Num: 4, Den: 3}},
		{ID: "m_chest", Source: "chest", Target: "chest_t", RotationScale: model.Ratio{Num: 2, Den: 3}, TranslationScale: model.Ratio{Num: 3, Den: 4}},
		{ID: "m_neck", Source: "neck", Target: "neck_t", RotationScale: model.Ratio{Num: 5, Den: 4}, TranslationScale: model.Ratio{Num: 1, Den: 1}},
		{ID: "m_arm_l", Source: "arm_l", Target: "arm_l_t", RotationScale: model.Ratio{Num: 3, Den: 4}, TranslationScale: model.Ratio{Num: 5, Den: 4}},
		{ID: "m_arm_r", Source: "arm_r", Target: "arm_r_t", RotationScale: model.Ratio{Num: 4, Den: 3}, TranslationScale: model.Ratio{Num: 4, Den: 5}},
	}

	frames := make([]baseFrame, 0, FrameCount)
	for frame := 0; frame < FrameCount; frame++ {
		phase := float64(frame) / float64(FrameCount)
		angles := []float64{
			9*math.Sin(2*math.Pi*phase) + 2,
			7*math.Sin(2*math.Pi*phase+0.3) + rngJitter(rng),
			10*math.Sin(2*math.Pi*phase+0.8) + rngJitter(rng),
			13*math.Sin(2*math.Pi*phase+1.2) + rngJitter(rng),
			18*math.Sin(2*math.Pi*phase+0.4) + rngJitter(rng),
			16*math.Sin(2*math.Pi*phase-0.4) + rngJitter(rng),
		}
		axes := [][3]float64{
			{0, 1, 0},
			{1, 0, 0},
			{0, 0, 1},
			{0, 1, 0},
			{1, 0.2, 0.3},
			{-0.2, 1, 0.1},
		}
		locals := make([]model.Quat, 0, len(joints))
		for i := range joints {
			q, err := axisAngleQuat(axes[i], angles[i], SourceScale)
			if err != nil {
				return nil, err
			}
			locals = append(locals, q)
		}
		frames = append(frames, baseFrame{
			Index: frame,
			Root: vec(
				int64(math.Round(120000*math.Sin(2*math.Pi*phase))),
				int64(math.Round(18000*math.Sin(4*math.Pi*phase))),
				int64(math.Round(50000*math.Cos(2*math.Pi*phase))),
				SourceScale,
			),
			Locals: locals,
		})
	}

	base := &sampleBase{joints: joints, frames: frames, target: target, mapping: mapping}
	truth, err := referenceTruth(base)
	if err != nil {
		return nil, err
	}
	base.truth = truth
	return base, nil
}

func rngJitter(rng *rand.Rand) float64 {
	return (rng.Float64() - 0.5) * 3
}

func vec(x, y, z, scale int64) model.FixedVec3 {
	return model.FixedVec3{X: x, Y: y, Z: z, Scale: scale}
}

func axisAngleQuat(axis [3]float64, degrees float64, scale int64) (model.Quat, error) {
	length := math.Sqrt(axis[0]*axis[0] + axis[1]*axis[1] + axis[2]*axis[2])
	if length == 0 {
		return fixed.Identity(scale), nil
	}
	half := degrees * math.Pi / 360
	s, c := math.Sincos(half)
	raw := model.Quat{
		W:     int64(math.Round(c * float64(scale))),
		X:     int64(math.Round(s * float64(scale) * axis[0] / length)),
		Y:     int64(math.Round(s * float64(scale) * axis[1] / length)),
		Z:     int64(math.Round(s * float64(scale) * axis[2] / length)),
		Scale: scale,
	}
	normalized, err := fixed.Normalize(raw, model.RoundHalfUp)
	if err != nil {
		return model.Quat{}, err
	}
	return normalized.Q, nil
}

func degreeToMicroRadians(microDegrees int64) int64 {
	return int64(math.Round(float64(microDegrees) * math.Pi / 180))
}

func referenceTruth(base *sampleBase) ([]model.TruthSample, error) {
	jointIndex := make(map[string]int, len(base.joints))
	for i, joint := range base.joints {
		jointIndex[joint.ID] = i
	}
	targetIndex := make(map[string]int, len(base.target))
	for i, joint := range base.target {
		targetIndex[joint.ID] = i
	}
	mapByTarget := make(map[string]model.Mapping, len(base.mapping))
	for _, mapping := range base.mapping {
		mapByTarget[mapping.Target] = mapping
	}
	samples := map[int]bool{0: true, 5: true, 11: true, 17: true, 23: true}
	var truth []model.TruthSample
	for _, frame := range base.frames {
		if !samples[frame.Index] {
			continue
		}
		sourceWorld := make([]model.Quat, len(base.joints))
		for i, joint := range base.joints {
			local, _ := fixed.RescaleQuat(frame.Locals[i], TruthScale, model.RoundHalfUp)
			normalized, err := fixed.Normalize(local, model.RoundHalfUp)
			if err != nil {
				return nil, err
			}
			local = normalized.Q
			if joint.Parent == "" {
				sourceWorld[i] = local
				continue
			}
			parent := jointIndex[joint.Parent]
			world, _, err := fixed.Mul(sourceWorld[parent], local, model.RoundHalfUp)
			if err != nil {
				return nil, err
			}
			sourceWorld[i] = world
		}
		targetWorld := make([]model.Quat, len(base.target))
		targetPos := make([]model.FixedVec3, len(base.target))
		for i, joint := range base.target {
			mapping, ok := mapByTarget[joint.ID]
			if !ok {
				return nil, fmt.Errorf("target %s has no mapping", joint.ID)
			}
			sourceIdx, ok := jointIndex[mapping.Source]
			if !ok {
				return nil, fmt.Errorf("mapping source %s missing", mapping.Source)
			}
			world, _, err := fixed.AdjustRotation(sourceWorld[sourceIdx], mapping.RotationScale, model.RoundHalfUp)
			if err != nil {
				return nil, err
			}
			targetWorld[i] = world
			rest, _ := fixed.RescaleVec(joint.Rest, TruthScale, model.RoundHalfUp)
			if joint.Parent == "" {
				root, _ := fixed.RescaleVec(frame.Root, TruthScale, model.RoundHalfUp)
				position, _, err := fixed.ScaleVec(root, mapping.TranslationScale, joint.Proportion)
				if err != nil {
					return nil, err
				}
				targetPos[i] = position
			} else {
				parent := targetIndex[joint.Parent]
				offset, err := fixed.RotateVec(targetWorld[parent], rest)
				if err != nil {
					return nil, err
				}
				targetPos[i] = addVec(targetPos[parent], offset)
			}
		}
		for i, joint := range base.target {
			truth = append(truth, model.TruthSample{
				Frame: frame.Index, Joint: joint.ID,
				Rotation: targetWorld[i], Position: targetPos[i],
			})
		}
	}
	return truth, nil
}

func addVec(a, b model.FixedVec3) model.FixedVec3 {
	return model.FixedVec3{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z, Scale: a.Scale}
}

// --- wire contracts -------------------------------------------------------

type wireTruth struct {
	Frame    int      `json:"frame"`
	Joint    string   `json:"joint"`
	Rotation wireQuat `json:"rotation"`
	Position wireVec3 `json:"position"`
}

type wireQuat struct {
	Fixed int64   `json:"fixed"`
	Order string  `json:"order"`
	V     []int64 `json:"v"`
}

type wireVec3 struct {
	Fixed int64   `json:"fixed"`
	V     []int64 `json:"v"`
}

type wireTolerance struct {
	NormPPM      int64 `json:"normPpm"`
	RoundTripPPM int64 `json:"roundTripPpm"`
	RotationPPM  int64 `json:"rotationPpm"`
}

type studioDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      studioTimeline `json:"timeline"`
	Skeleton      studioSkeleton `json:"skeleton"`
	Clip          studioClip     `json:"clip"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        studioTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type studioTimeline struct {
	Unit  string `json:"unit"`
	FPS   int64  `json:"fps"`
	Round string `json:"round"`
}

type studioSkeleton struct {
	Scale int64         `json:"scale"`
	Tree  []studioJoint `json:"tree"`
}

type studioJoint struct {
	Name       string        `json:"name"`
	Parent     *string       `json:"parent"`
	Rest       []int64       `json:"rest"`
	Constraint studioLimit   `json:"constraint"`
	Children   []studioJoint `json:"children,omitempty"`
}

type studioLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type studioClip struct {
	RotationScale    int64         `json:"rotationScale"`
	TranslationScale int64         `json:"translationScale"`
	Frames           []studioFrame `json:"frames"`
}

type studioFrame struct {
	Frame int          `json:"frame"`
	Root  []int64      `json:"root"`
	Poses []studioPose `json:"poses"`
}

type studioPose struct {
	Joint string  `json:"joint"`
	Q     []int64 `json:"q"`
}

type studioTarget struct {
	Scale int64               `json:"scale"`
	Tree  []studioTargetJoint `json:"tree"`
}

type studioTargetJoint struct {
	Name       string              `json:"name"`
	Parent     *string             `json:"parent"`
	Rest       []int64             `json:"rest"`
	Proportion []int64             `json:"proportion"`
	Children   []studioTargetJoint `json:"children,omitempty"`
}

type engineDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      engineTimeline `json:"timeline"`
	Skeleton      engineSkeleton `json:"skeleton"`
	Clip          engineClip     `json:"clip"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        engineTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type engineTimeline struct {
	Unit     string `json:"unit"`
	FPS      int64  `json:"fps"`
	Rounding string `json:"rounding"`
}

type engineSkeleton struct {
	Fixed int64        `json:"fixed"`
	Bones []engineBone `json:"bones"`
}

type engineBone struct {
	ID     string      `json:"id"`
	Parent string      `json:"parent"`
	Rest   []int64     `json:"rest"`
	Limit  engineLimit `json:"limit"`
}

type engineLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type engineClip struct {
	Fixed  int64         `json:"fixed"`
	Frames []engineFrame `json:"frames"`
}

type engineFrame struct {
	AtMS  int64        `json:"atMs"`
	Root  []int64      `json:"root"`
	Poses []enginePose `json:"poses"`
}

type enginePose struct {
	ID string  `json:"id"`
	Q  []int64 `json:"q"`
}

type engineTarget struct {
	Fixed int64              `json:"fixed"`
	Bones []engineTargetBone `json:"bones"`
}

type engineTargetBone struct {
	ID         string  `json:"id"`
	Parent     string  `json:"parent"`
	Rest       []int64 `json:"rest"`
	Proportion []int64 `json:"proportion"`
}

type legacyDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      legacyTimeline `json:"timeline"`
	Skeleton      legacySkeleton `json:"skeleton"`
	Motion        legacyMotion   `json:"motion"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        legacyTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type legacyTimeline struct {
	Unit       string `json:"unit"`
	FrameRate  int64  `json:"frameRate"`
	Truncation string `json:"truncation"`
}

type legacySkeleton struct {
	Fixed int64         `json:"fixed"`
	Flat  []legacyJoint `json:"flat"`
}

type legacyJoint struct {
	Joint      string          `json:"joint"`
	Parent     string          `json:"parent"`
	Rest       legacyFixedVec3 `json:"rest"`
	Constraint legacyLimit     `json:"constraint"`
}

type legacyFixedVec3 struct {
	Fixed int64   `json:"fixed"`
	V     []int64 `json:"v"`
}

type legacyLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type legacyMotion struct {
	Fixed  int64         `json:"fixed"`
	Frames []legacyFrame `json:"frames"`
}

type legacyFrame struct {
	Index int            `json:"index"`
	Root  []int64        `json:"root"`
	Q     []legacyQValue `json:"rotations"`
}

type legacyQValue struct {
	Joint string   `json:"joint"`
	Value wireQuat `json:"value"`
}

type legacyTarget struct {
	Fixed int64               `json:"fixed"`
	Flat  []legacyTargetJoint `json:"flat"`
}

type legacyTargetJoint struct {
	Joint      string  `json:"joint"`
	Parent     string  `json:"parent"`
	Rest       []int64 `json:"rest"`
	Proportion []int64 `json:"proportion"`
}

type wireRetarget struct {
	Mappings []wireMapping `json:"mappings"`
}

type wireMapping struct {
	ID               string  `json:"id"`
	Source           string  `json:"source"`
	Target           string  `json:"target"`
	RotationScale    []int64 `json:"rotationScale"`
	TranslationScale []int64 `json:"translationScale"`
}

// --- encoders -------------------------------------------------------------

func encodeStudio(base *sampleBase) ([]byte, error) {
	tree, err := nestedJoints(base)
	if err != nil {
		return nil, err
	}
	targetTree, err := nestedTargets(base)
	if err != nil {
		return nil, err
	}
	doc := studioDoc{
		Format: "studio", Version: "3", SourceVersion: "studio-3",
		Timeline:  studioTimeline{Unit: "frame", FPS: FPS, Round: "half_up"},
		Skeleton:  studioSkeleton{Scale: SourceScale, Tree: tree},
		Retarget:  wireRetarget{Mappings: wireMappings(base.mapping)},
		Target:    studioTarget{Scale: SourceScale, Tree: targetTree},
		Truth:     wireTruthList(base.truth, "xyzw"),
		Tolerance: wireTolerance{NormPPM: 5000, RoundTripPPM: 20, RotationPPM: 1500},
	}
	for _, frame := range base.frames {
		poses := make([]studioPose, 0, len(base.joints))
		for i, joint := range base.joints {
			q := resizeQuat(frame.Locals[i], SourceScale)
			poses = append(poses, studioPose{Joint: joint.ID, Q: []int64{q.X, q.Y, q.Z, q.W}})
		}
		doc.Clip.Frames = append(doc.Clip.Frames, studioFrame{
			Frame: frame.Index, Root: []int64{frame.Root.X, frame.Root.Y, frame.Root.Z}, Poses: poses,
		})
	}
	doc.Clip.RotationScale = SourceScale
	doc.Clip.TranslationScale = SourceScale
	return json.MarshalIndent(doc, "", "  ")
}

func encodeEngine(base *sampleBase) ([]byte, error) {
	const scale = int64(1_000_000)
	bones := make([]engineBone, 0, len(base.joints))
	for _, joint := range base.joints {
		bones = append(bones, engineBone{
			ID: joint.ID, Parent: joint.Parent,
			Rest:  scaleVec(joint.Rest, scale),
			Limit: engineLimit{Unit: "rad", Min: degreeToMicroRadians(joint.MinDegreeMicro), Max: degreeToMicroRadians(joint.MaxDegreeMicro)},
		})
	}
	target := make([]engineTargetBone, 0, len(base.target))
	for _, joint := range base.target {
		target = append(target, engineTargetBone{
			ID: joint.ID, Parent: joint.Parent, Rest: scaleVec(joint.Rest, scale),
			Proportion: []int64{joint.Proportion.Num, joint.Proportion.Den},
		})
	}
	doc := engineDoc{
		Format: "engine", Version: "2", SourceVersion: "engine-2",
		Timeline:  engineTimeline{Unit: "ms", FPS: FPS, Rounding: "half_up"},
		Skeleton:  engineSkeleton{Fixed: scale, Bones: bones},
		Retarget:  wireRetarget{Mappings: wireMappings(base.mapping)},
		Target:    engineTarget{Fixed: scale, Bones: target},
		Truth:     wireTruthList(base.truth, "wxyz"),
		Tolerance: wireTolerance{NormPPM: 5000, RoundTripPPM: 20, RotationPPM: 1500},
	}
	for _, frame := range base.frames {
		ms, _ := fixed.FrameToMS(int64(frame.Index), FPS, model.RoundHalfUp)
		poses := make([]enginePose, 0, len(base.joints))
		for i, joint := range base.joints {
			q := resizeQuat(frame.Locals[i], scale)
			poses = append(poses, enginePose{ID: joint.ID, Q: []int64{q.W, q.X, q.Y, q.Z}})
		}
		doc.Clip.Frames = append(doc.Clip.Frames, engineFrame{
			AtMS: ms, Root: scaleVec(frame.Root, scale), Poses: poses,
		})
	}
	doc.Clip.Fixed = scale
	return json.MarshalIndent(doc, "", "  ")
}

func encodeLegacy(base *sampleBase) ([]byte, error) {
	const scale = int64(10_000_000)
	flat := make([]legacyJoint, 0, len(base.joints))
	for _, joint := range base.joints {
		flat = append(flat, legacyJoint{
			Joint: joint.ID, Parent: joint.Parent,
			Rest:       legacyFixedVec3{Fixed: scale, V: scaleVec(joint.Rest, scale)},
			Constraint: legacyLimit{Unit: "rad", Min: degreeToMicroRadians(joint.MinDegreeMicro), Max: degreeToMicroRadians(joint.MaxDegreeMicro)},
		})
	}
	target := make([]legacyTargetJoint, 0, len(base.target))
	for _, joint := range base.target {
		target = append(target, legacyTargetJoint{
			Joint: joint.ID, Parent: joint.Parent, Rest: scaleVec(joint.Rest, scale),
			Proportion: []int64{joint.Proportion.Num, joint.Proportion.Den},
		})
	}
	doc := legacyDoc{
		Format: "legacy", Version: "1", SourceVersion: "legacy-1",
		Timeline:  legacyTimeline{Unit: "frame", FrameRate: FPS, Truncation: "half_up"},
		Skeleton:  legacySkeleton{Fixed: scale, Flat: flat},
		Retarget:  wireRetarget{Mappings: wireMappings(base.mapping)},
		Target:    legacyTarget{Fixed: scale, Flat: target},
		Truth:     wireTruthList(base.truth, "xyzw"),
		Tolerance: wireTolerance{NormPPM: 5000, RoundTripPPM: 20, RotationPPM: 1500},
	}
	for _, frame := range base.frames {
		values := make([]legacyQValue, 0, len(base.joints))
		for i, joint := range base.joints {
			q := resizeQuat(frame.Locals[i], scale)
			values = append(values, legacyQValue{
				Joint: joint.ID,
				Value: wireQuat{Fixed: scale, Order: "xyzw", V: []int64{q.X, q.Y, q.Z, q.W}},
			})
		}
		doc.Motion.Frames = append(doc.Motion.Frames, legacyFrame{
			Index: frame.Index, Root: scaleVec(frame.Root, scale), Q: values,
		})
	}
	doc.Motion.Fixed = scale
	return json.MarshalIndent(doc, "", "  ")
}

func nestedJoints(base *sampleBase) ([]studioJoint, error) {
	children := make(map[string][]baseJoint)
	var roots []baseJoint
	for _, joint := range base.joints {
		if joint.Parent == "" {
			roots = append(roots, joint)
		} else {
			children[joint.Parent] = append(children[joint.Parent], joint)
		}
	}
	var out []studioJoint
	var walk func(joint baseJoint) (studioJoint, error)
	walk = func(joint baseJoint) (studioJoint, error) {
		var parent *string
		if joint.Parent != "" {
			p := joint.Parent
			parent = &p
		}
		node := studioJoint{
			Name: joint.ID, Parent: parent, Rest: []int64{joint.Rest.X, joint.Rest.Y, joint.Rest.Z},
			Constraint: studioLimit{Unit: "deg", Min: joint.MinDegreeMicro, Max: joint.MaxDegreeMicro},
		}
		for _, child := range children[joint.ID] {
			childNode, err := walk(child)
			if err != nil {
				return studioJoint{}, err
			}
			node.Children = append(node.Children, childNode)
		}
		return node, nil
	}
	for _, root := range roots {
		node, err := walk(root)
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

func nestedTargets(base *sampleBase) ([]studioTargetJoint, error) {
	children := make(map[string][]baseTarget)
	var roots []baseTarget
	for _, joint := range base.target {
		if joint.Parent == "" {
			roots = append(roots, joint)
		} else {
			children[joint.Parent] = append(children[joint.Parent], joint)
		}
	}
	var out []studioTargetJoint
	var walk func(joint baseTarget) studioTargetJoint
	walk = func(joint baseTarget) studioTargetJoint {
		var parent *string
		if joint.Parent != "" {
			p := joint.Parent
			parent = &p
		}
		node := studioTargetJoint{
			Name: joint.ID, Parent: parent, Rest: []int64{joint.Rest.X, joint.Rest.Y, joint.Rest.Z},
			Proportion: []int64{joint.Proportion.Num, joint.Proportion.Den},
		}
		for _, child := range children[joint.ID] {
			node.Children = append(node.Children, walk(child))
		}
		return node
	}
	for _, root := range roots {
		out = append(out, walk(root))
	}
	return out, nil
}

func wireMappings(mappings []model.Mapping) []wireMapping {
	out := make([]wireMapping, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, wireMapping{
			ID: mapping.ID, Source: mapping.Source, Target: mapping.Target,
			RotationScale:    []int64{mapping.RotationScale.Num, mapping.RotationScale.Den},
			TranslationScale: []int64{mapping.TranslationScale.Num, mapping.TranslationScale.Den},
		})
	}
	return out
}

func wireTruthList(truth []model.TruthSample, order string) []wireTruth {
	out := make([]wireTruth, 0, len(truth))
	for _, sample := range truth {
		q := sample.Rotation
		components := []int64{q.W, q.X, q.Y, q.Z}
		switch order {
		case "xyzw":
			components = []int64{q.X, q.Y, q.Z, q.W}
		}
		out = append(out, wireTruth{
			Frame: sample.Frame, Joint: sample.Joint,
			Rotation: wireQuat{Fixed: q.Scale, Order: order, V: components},
			Position: wireVec3{Fixed: sample.Position.Scale, V: []int64{sample.Position.X, sample.Position.Y, sample.Position.Z}},
		})
	}
	return out
}

func resizeQuat(q model.Quat, scale int64) model.Quat {
	resized, _ := fixed.RescaleQuat(q, scale, model.RoundHalfUp)
	return resized
}

func scaleVec(v model.FixedVec3, scale int64) []int64 {
	resized, _ := fixed.RescaleVec(v, scale, model.RoundHalfUp)
	return []int64{resized.X, resized.Y, resized.Z}
}
