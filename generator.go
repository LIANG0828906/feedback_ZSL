package bonerig

import (
	"fmt"
)

const DefaultSeed int64 = 42

// SampleSet is the deterministic on-disk/in-memory acceptance bundle.
type SampleSet struct {
	Seed  int64             `json:"seed"`
	Asset Asset             `json:"asset"`
	Docs  map[string][]byte `json:"docs"`
	Truth TruthManifest     `json:"truth"`
}

type deterministicRNG struct{ state uint64 }

func newRNG(seed int64) deterministicRNG {
	return deterministicRNG{state: uint64(seed)*2862933555777941757 + 3037000493}
}

func (r *deterministicRNG) next() int64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int64(r.state >> 33)
}

func distVec(scale int64, x, y, z int64) Vec3 {
	return Vec3{X: x * scale, Y: y * scale, Z: z * scale, Scale: scale}
}

func genQuat(p PrecisionParams, seed int64, xAmp, yAmp, zAmp int64) Quat {
	r := newRNG(seed)
	x := (r.next() % (2*xAmp + 1)) - xAmp
	y := (r.next() % (2*yAmp + 1)) - yAmp
	z := (r.next() % (2*zAmp + 1)) - zAmp
	q, _, err := QNormalize(FQ{W: p.WorkScale, X: x, Y: y, Z: z}, p.WorkScale, "generator", int(seed%1_000_000))
	if err != nil {
		panic(err.Error())
	}
	return Quat{W: q.W, X: q.X, Y: q.Y, Z: q.Z, Scale: p.WorkScale}
}

func triangle(frame, period int64) int64 {
	x := frame % period
	if x < 0 {
		x += period
	}
	half := period / 2
	if x <= half {
		return 2*x - half
	}
	return 3*half - 2*x
}

// NewDeterministicSample builds the canonical asset and all three wire docs.
func NewDeterministicSample(seed int64) (*SampleSet, *Error) {
	p := DefaultPrecision()
	ds := p.DistanceScale
	sourceJoints := []Joint{
		{ID: "root", ParentID: "", RestTranslation: distVec(ds, 0, 0, 0), BindRotation: identityQuat(p.WorkScale)},
		{ID: "spine", ParentID: "root", RestTranslation: distVec(ds, 0, 220, 0), BindRotation: identityQuat(p.WorkScale)},
		{ID: "arm_l", ParentID: "spine", RestTranslation: distVec(ds, 180, 140, 0), BindRotation: identityQuat(p.WorkScale)},
		{ID: "hand_l", ParentID: "arm_l", RestTranslation: distVec(ds, 260, 20, 0), BindRotation: identityQuat(p.WorkScale), Constraint: &Constraint{
			Enabled: true, AxisZ: 1, MinAngleMicroRad: -3_141_593, MaxAngleMicroRad: 3_141_593,
			RuleVersion: RuleVersion, DefaultFilled: true,
		}},
		{ID: "leg_l", ParentID: "root", RestTranslation: distVec(ds, -90, -120, 0), BindRotation: identityQuat(p.WorkScale)},
		{ID: "foot_l", ParentID: "leg_l", RestTranslation: distVec(ds, 0, -420, 80), BindRotation: identityQuat(p.WorkScale)},
	}
	for i := range sourceJoints {
		if sourceJoints[i].Constraint == nil {
			c := defaultConstraint()
			sourceJoints[i].Constraint = &c
		}
	}

	targetIDs := []string{"Root", "Spine", "ArmL", "HandL", "LegL", "FootL"}
	targetJoints := make([]Joint, 0, len(sourceJoints))
	bones := map[string]BoneProportion{}
	lengths := []struct{ src, tgt int64 }{{100, 115}, {220, 242}, {228, 251}, {261, 287}, {150, 165}, {428, 471}}
	for i, sj := range sourceJoints {
		rest := sj.RestTranslation
		targetRest := Vec3{
			X:     roundQ(rest.X*lengths[i].tgt, lengths[i].src),
			Y:     roundQ(rest.Y*lengths[i].tgt, lengths[i].src),
			Z:     roundQ(rest.Z*lengths[i].tgt, lengths[i].src),
			Scale: ds,
		}
		targetJoints = append(targetJoints, Joint{ID: targetIDs[i], ParentID: mapTargetParent(sj.ParentID), RestTranslation: targetRest, BindRotation: identityQuat(p.WorkScale)})
		bones[targetIDs[i]] = BoneProportion{SourceLength: lengths[i].src * ds, TargetLength: lengths[i].tgt * ds}
	}

	frameCount := 12
	frames := make([]Frame, 0, frameCount)
	for f := 0; f < frameCount; f++ {
		poses := map[string]JointPose{}
		amp := p.WorkScale / 12
		values := map[string]FQ{
			"root":   {W: p.WorkScale, Y: triangle(int64(f), 8) * (p.WorkScale / 200)},
			"spine":  {W: p.WorkScale, Z: triangle(int64(f)+1, 8) * amp / 3},
			"arm_l":  {W: p.WorkScale, Z: triangle(int64(f)+2, 8) * amp},
			"hand_l": {W: p.WorkScale},
			"leg_l":  {W: p.WorkScale, X: triangle(int64(f)+4, 8) * amp},
			"foot_l": {W: p.WorkScale, X: -triangle(int64(f)+5, 8) * amp / 2},
		}
		if f >= 8 {
			values["hand_l"] = FQ{W: p.WorkScale, Y: int64(f-7) * p.WorkScale / 40}
		}
		for _, j := range sourceJoints {
			base := values[j.ID]
			q, _, qerr := QNormalize(base, p.WorkScale, j.ID, f)
			if qerr != nil {
				return nil, qerr
			}
			poses[j.ID] = JointPose{Rotation: Quat{W: q.W, X: q.X, Y: q.Y, Z: q.Z, Scale: p.WorkScale}}
		}
		rootX := int64(f) * 12_500 * ds / 1_000_000 // 0.0125 units/frame in fixed distance
		rootY := (triangle(int64(f), 4) + 2) * 5_000 * ds / 1_000_000
		frames = append(frames, Frame{
			Index: f, Tick: int64(f), TimeMS: int64(f) * 10,
			RootTranslation: Vec3{X: rootX, Y: rootY, Z: 0, Scale: ds},
			Poses:           poses,
		})
	}

	mapping := Mapping{Version: "mapping-seed42-v1", Items: []MapItem{
		{SourceJoint: "root", TargetJoint: "Root", WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
		{SourceJoint: "spine", TargetJoint: "Spine", WeightScale: 1000, ExtraSources: []WeightedJoint{{SourceJoint: "root", Weight: 100}}, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
		{SourceJoint: "arm_l", TargetJoint: "ArmL", WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
		{SourceJoint: "hand_l", TargetJoint: "HandL", WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
		{SourceJoint: "leg_l", TargetJoint: "LegL", WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
		{SourceJoint: "foot_l", TargetJoint: "FootL", WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1},
	}}
	props := ProportionSet{Version: "prop-seed42-v1", DistanceScale: ds, GlobalNum: 23, GlobalDen: 20, Bones: bones}
	a := Asset{
		Seed: seed, SourceFormat: "canonical", SourceVersion: "canonical", SchemaVersion: "canonical-v1",
		Skeleton:       Skeleton{ID: "source_rig_seed42", Version: "topo-seed42-v1", Joints: sourceJoints},
		TargetSkeleton: Skeleton{ID: "target_rig_seed42", Version: "target-seed42-v1", Joints: targetJoints},
		Clip:           Clip{ID: "walk_reach_seed42", Frames: frames, TickRate: 100},
		Mapping:        mapping, Proportions: props, Precision: p,
	}
	a.ContentHash = CanonicalHash(a.Clip)
	a.TopologyVersion = TopologyHash(a.Skeleton)
	truth := BuildReferenceTruth(a)
	a.Truth = truth

	docs := map[string][]byte{}
	for _, schema := range []string{V1Schema, V2Schema, V3Schema} {
		b, _, eerr := ExportFormat(a, schema)
		if eerr != nil {
			return nil, eerr
		}
		docs[schema] = b
	}
	return &SampleSet{Seed: seed, Asset: a, Docs: docs, Truth: truth}, nil
}

func identityQuat(scale int64) Quat {
	return Quat{W: scale, Scale: scale}
}

func mapTargetParent(parent string) string {
	switch parent {
	case "":
		return ""
	case "root":
		return "Root"
	case "spine":
		return "Spine"
	case "arm_l":
		return "ArmL"
	case "hand_l":
		return "HandL"
	case "leg_l":
		return "LegL"
	case "foot_l":
		return "FootL"
	default:
		return capitalize(parent)
	}
}

func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%c%s", s[0]-32, s[1:])
}
