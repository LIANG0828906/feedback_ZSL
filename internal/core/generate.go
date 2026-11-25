package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type GeneratorOptions struct {
	OutDir string
	Seed   int64
}

type writtenAsset struct {
	decl AssetDecl
}

func GenerateSampleSet(opts GeneratorOptions) (*Manifest, error) {
	if opts.Seed == 0 {
		opts.Seed = DefaultSeed
	}
	if err := os.MkdirAll(filepath.Join(opts.OutDir, "clips"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.OutDir, "targets"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.OutDir, "truth"), 0o755); err != nil {
		return nil, err
	}

	source := sourceSkeleton()
	targets := []Skeleton{heroTallSkeleton(), compactSkeleton()}
	clipSpecs := []struct {
		id     string
		frames int
		period int
		speed  float64
		amp    float64
		phase  float64
	}{
		{id: "walk_cycle", frames: 48, period: 24, speed: 0.035, amp: 0.42, phase: 0},
		{id: "brisk_walk", frames: 60, period: 20, speed: 0.052, amp: 0.55, phase: 0.7},
		{id: "idle_shift", frames: 36, period: 36, speed: 0.006, amp: 0.20, phase: 1.4},
	}

	manifest := &Manifest{
		SchemaVersion: SchemaVersion,
		Seed:          opts.Seed,
		Generator:     GeneratorVersion,
	}

	for _, spec := range clipSpecs {
		clip := generateClip(source, spec.id, opts.Seed+int64(len(manifest.Clips)*97), spec.frames, spec.period, spec.speed, spec.amp, spec.phase)
		truth := BuildTruth(clip)
		clipPath := filepath.Join("clips", clip.ID+".json")
		truthPath := filepath.Join("truth", clip.ID+".truth.json")
		clipDecl, err := writeJSONAsset(filepath.Join(opts.OutDir, clipPath), clip.ID, clip)
		if err != nil {
			return nil, err
		}
		clipDecl.Frames = len(clip.Frames)
		clipDecl.Joints = len(clip.Skeleton.Joints)
		clipDecl.File = clipPath
		truthDecl, err := writeJSONAsset(filepath.Join(opts.OutDir, truthPath), truth.ClipID, truth)
		if err != nil {
			return nil, err
		}
		truthDecl.File = truthPath
		manifest.Clips = append(manifest.Clips, clipDecl)
		manifest.Truth = append(manifest.Truth, truthDecl)
	}

	for _, target := range targets {
		targetPath := filepath.Join("targets", target.ID+".json")
		decl, err := writeJSONAsset(filepath.Join(opts.OutDir, targetPath), target.ID, target)
		if err != nil {
			return nil, err
		}
		decl.File = targetPath
		decl.Joints = len(target.Joints)
		manifest.Targets = append(manifest.Targets, decl)
	}

	manifestPath := filepath.Join(opts.OutDir, "manifest.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func writeJSONAsset(path, id string, value any) (AssetDecl, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return AssetDecl{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return AssetDecl{}, err
	}
	sum := sha256.Sum256(data)
	return AssetDecl{ID: id, SHA256: hex.EncodeToString(sum[:])}, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func lim(min, max float64) *AngleLimit {
	return &AngleLimit{MinRadians: min, MaxRadians: max, Axis: Vec3{1, 0, 0}}
}

func sourceSkeleton() Skeleton {
	joints := []Joint{
		{Name: "hips", Parent: "", Offset: Vec3{0, 0.92, 0}, Limit: lim(-0.5, 0.5)},
		{Name: "spine", Parent: "hips", Offset: Vec3{0, 0.12, 0}, Limit: lim(-0.35, 0.45)},
		{Name: "chest", Parent: "spine", Offset: Vec3{0, 0.18, 0}, Limit: lim(-0.35, 0.4)},
		{Name: "neck", Parent: "chest", Offset: Vec3{0, 0.14, 0}, Limit: lim(-0.6, 0.6)},
		{Name: "head", Parent: "neck", Offset: Vec3{0, 0.1, 0}, EndEffector: true, Limit: lim(-0.7, 0.7)},

		{Name: "collar_l", Parent: "chest", Offset: Vec3{-0.04, 0.13, 0}, Limit: lim(-0.4, 0.4)},
		{Name: "upper_arm_l", Parent: "collar_l", Offset: Vec3{-0.16, -0.02, 0}, Limit: lim(-2.7, 2.7)},
		{Name: "lower_arm_l", Parent: "upper_arm_l", Offset: Vec3{-0.17, 0, 0}, Limit: lim(0.02, 1.55)},
		{Name: "hand_l", Parent: "lower_arm_l", Offset: Vec3{-0.09, 0, 0}, EndEffector: true, Limit: lim(-0.8, 0.8), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.56}},

		{Name: "collar_r", Parent: "chest", Offset: Vec3{0.04, 0.13, 0}, Limit: lim(-0.4, 0.4)},
		{Name: "upper_arm_r", Parent: "collar_r", Offset: Vec3{0.16, -0.02, 0}, Limit: lim(-2.7, 2.7)},
		{Name: "lower_arm_r", Parent: "upper_arm_r", Offset: Vec3{0.17, 0, 0}, Limit: lim(0.02, 1.55)},
		{Name: "hand_r", Parent: "lower_arm_r", Offset: Vec3{0.09, 0, 0}, EndEffector: true, Limit: lim(-0.8, 0.8), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.56}},

		{Name: "upper_leg_l", Parent: "hips", Offset: Vec3{-0.11, -0.02, 0}, Limit: lim(-1.45, 1.45)},
		{Name: "lower_leg_l", Parent: "upper_leg_l", Offset: Vec3{0, -0.45, 0}, Limit: lim(0.02, 1.65)},
		{Name: "foot_l", Parent: "lower_leg_l", Offset: Vec3{0, -0.45, 0}, Foot: true, Limit: lim(-0.85, 0.85)},
		{Name: "toe_l", Parent: "foot_l", Offset: Vec3{0.16, 0, 0}, Limit: lim(-0.7, 0.7)},

		{Name: "upper_leg_r", Parent: "hips", Offset: Vec3{0.11, -0.02, 0}, Limit: lim(-1.45, 1.45)},
		{Name: "lower_leg_r", Parent: "upper_leg_r", Offset: Vec3{0, -0.45, 0}, Limit: lim(0.02, 1.65)},
		{Name: "foot_r", Parent: "lower_leg_r", Offset: Vec3{0, -0.45, 0}, Foot: true, Limit: lim(-0.85, 0.85)},
		{Name: "toe_r", Parent: "foot_r", Offset: Vec3{0.16, 0, 0}, Limit: lim(-0.7, 0.7)},
	}
	return Skeleton{ID: "source_humanoid_v1", Name: "Source Humanoid 1.84m", GroundY: 0, Joints: joints}
}

func heroTallSkeleton() Skeleton {
	j := []Joint{
		{Name: "hips", Parent: "", Offset: Vec3{0, 1.08, 0}, Limit: lim(-0.45, 0.45)},
		{Name: "spine", Parent: "hips", Offset: Vec3{0, 0.14, 0}, Limit: lim(-0.3, 0.4)},
		{Name: "spine2", Parent: "spine", Offset: Vec3{0, 0.16, 0}, Limit: lim(-0.3, 0.35)},
		{Name: "chest", Parent: "spine2", Offset: Vec3{0, 0.2, 0}, Limit: lim(-0.3, 0.35)},
		{Name: "neck", Parent: "chest", Offset: Vec3{0, 0.17, 0}, Limit: lim(-0.55, 0.55)},
		{Name: "head", Parent: "neck", Offset: Vec3{0, 0.12, 0}, EndEffector: true, Limit: lim(-0.65, 0.65)},

		{Name: "upper_arm_l", Parent: "chest", Offset: Vec3{-0.22, 0.1, 0}, Limit: lim(-2.5, 2.5)},
		{Name: "lower_arm_l", Parent: "upper_arm_l", Offset: Vec3{-0.21, 0, 0}, Limit: lim(0.02, 1.45)},
		{Name: "hand_l", Parent: "lower_arm_l", Offset: Vec3{-0.11, 0, 0}, EndEffector: true, Limit: lim(-0.75, 0.75), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.5}},
		{Name: "upper_arm_r", Parent: "chest", Offset: Vec3{0.22, 0.1, 0}, Limit: lim(-2.5, 2.5)},
		{Name: "lower_arm_r", Parent: "upper_arm_r", Offset: Vec3{0.21, 0, 0}, Limit: lim(0.02, 1.45)},
		{Name: "hand_r", Parent: "lower_arm_r", Offset: Vec3{0.11, 0, 0}, EndEffector: true, Limit: lim(-0.75, 0.75), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.5}},

		{Name: "upper_leg_l", Parent: "hips", Offset: Vec3{-0.13, -0.02, 0}, Limit: lim(-1.35, 1.35)},
		{Name: "lower_leg_l", Parent: "upper_leg_l", Offset: Vec3{0, -0.53, 0}, Limit: lim(0.02, 1.55)},
		{Name: "foot_l", Parent: "lower_leg_l", Offset: Vec3{0, -0.53, 0}, Foot: true, Limit: lim(-0.8, 0.8)},
		{Name: "upper_leg_r", Parent: "hips", Offset: Vec3{0.13, -0.02, 0}, Limit: lim(-1.35, 1.35)},
		{Name: "lower_leg_r", Parent: "upper_leg_r", Offset: Vec3{0, -0.53, 0}, Limit: lim(0.02, 1.55)},
		{Name: "foot_r", Parent: "lower_leg_r", Offset: Vec3{0, -0.53, 0}, Foot: true, Limit: lim(-0.8, 0.8)},
	}
	return Skeleton{ID: "target_hero_tall", Name: "Hero Tall 2.10m / extra spine", GroundY: 0, Joints: j}
}

func compactSkeleton() Skeleton {
	j := []Joint{
		{Name: "hips", Parent: "", Offset: Vec3{0, 0.68, 0}, Limit: lim(-0.55, 0.55)},
		{Name: "spine", Parent: "hips", Offset: Vec3{0, 0.11, 0}, Limit: lim(-0.4, 0.45)},
		{Name: "chest", Parent: "spine", Offset: Vec3{0, 0.15, 0}, Limit: lim(-0.4, 0.4)},
		{Name: "head", Parent: "chest", Offset: Vec3{0, 0.16, 0}, EndEffector: true, Limit: lim(-0.8, 0.8)},

		{Name: "upper_arm_l", Parent: "chest", Offset: Vec3{-0.13, 0.08, 0}, Limit: lim(-2.8, 2.8)},
		{Name: "lower_arm_l", Parent: "upper_arm_l", Offset: Vec3{-0.13, 0, 0}, Limit: lim(0.02, 1.5)},
		{Name: "hand_l", Parent: "lower_arm_l", Offset: Vec3{-0.07, 0, 0}, EndEffector: true, Limit: lim(-0.8, 0.8), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.3}},
		{Name: "upper_arm_r", Parent: "chest", Offset: Vec3{0.13, 0.08, 0}, Limit: lim(-2.8, 2.8)},
		{Name: "lower_arm_r", Parent: "upper_arm_r", Offset: Vec3{0.13, 0, 0}, Limit: lim(0.02, 1.5)},
		{Name: "hand_r", Parent: "lower_arm_r", Offset: Vec3{0.07, 0, 0}, EndEffector: true, Limit: lim(-0.8, 0.8), Reach: &ReachLimit{Anchor: "chest", MaxDistance: 0.3}},

		{Name: "upper_leg_l", Parent: "hips", Offset: Vec3{-0.08, -0.01, 0}, Limit: lim(-1.5, 1.5)},
		{Name: "lower_leg_l", Parent: "upper_leg_l", Offset: Vec3{0, -0.34, 0}, Limit: lim(0.02, 1.7)},
		{Name: "foot_l", Parent: "lower_leg_l", Offset: Vec3{0, -0.33, 0}, Foot: true, Limit: lim(-0.9, 0.9)},
		{Name: "upper_leg_r", Parent: "hips", Offset: Vec3{0.08, -0.01, 0}, Limit: lim(-1.5, 1.5)},
		{Name: "lower_leg_r", Parent: "upper_leg_r", Offset: Vec3{0, -0.34, 0}, Limit: lim(0.02, 1.7)},
		{Name: "foot_r", Parent: "lower_leg_r", Offset: Vec3{0, -0.33, 0}, Foot: true, Limit: lim(-0.9, 0.9)},
	}
	return Skeleton{ID: "target_compact", Name: "Compact 1.35m / no neck or toes", GroundY: 0, Joints: j}
}

type rng struct{ state uint64 }

func newRNG(seed int64) *rng { return &rng{state: uint64(seed) | 1} }
func (r *rng) next() float64 {
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return float64(r.state%100000) / 100000.0
}

func generateClip(sk Skeleton, id string, seed int64, frames, period int, speed, amp, basePhase float64) Clip {
	r := newRNG(seed)
	rotations := make([][]Quat, frames)
	roots := make([]Vec3, frames)
	contactL := make([]bool, frames)
	contactR := make([]bool, frames)
	feet := []string{"foot_l", "foot_r"}

	jointIndex := map[string]int{}
	for i, j := range sk.Joints {
		jointIndex[j.Name] = i
	}

	for f := 0; f < frames; f++ {
		t := float64(f)
		phase := 2 * math.Pi * t / float64(period)
		jitter := (r.next() - 0.5) * 0.025
		q := make([]Quat, len(sk.Joints))
		for i := range q {
			q[i] = Quat{1, 0, 0, 0}
		}
		set := func(name string, quat Quat) { q[jointIndex[name]] = quat }

		legSwing := amp * math.Sin(phase+basePhase)
		set("upper_leg_l", AxisAngle(Vec3{1, 0, 0}, legSwing+jitter))
		set("upper_leg_r", AxisAngle(Vec3{1, 0, 0}, -legSwing-jitter))
		kneeL := 0.12 + amp*0.9*math.Max(0, -math.Sin(phase+basePhase+0.5))
		kneeR := 0.12 + amp*0.9*math.Max(0, math.Sin(phase+basePhase+0.5))
		set("lower_leg_l", AxisAngle(Vec3{1, 0, 0}, kneeL))
		set("lower_leg_r", AxisAngle(Vec3{1, 0, 0}, kneeR))
		set("foot_l", AxisAngle(Vec3{1, 0, 0}, -0.55*legSwing-0.35*kneeL))
		set("foot_r", AxisAngle(Vec3{1, 0, 0}, 0.55*legSwing-0.35*kneeR))
		if i, ok := jointIndex["toe_l"]; ok {
			q[i] = AxisAngle(Vec3{1, 0, 0}, 0.25*kneeL)
			q[jointIndex["toe_r"]] = AxisAngle(Vec3{1, 0, 0}, 0.25*kneeR)
		}

		armSwing := 0.72 * legSwing
		set("upper_arm_l", AxisAngle(Vec3{1, 0, 0}, -armSwing+0.08*math.Sin(phase+basePhase)))
		set("upper_arm_r", AxisAngle(Vec3{1, 0, 0}, armSwing-0.08*math.Sin(phase+basePhase)))
		set("lower_arm_l", AxisAngle(Vec3{1, 0, 0}, 0.25+0.35*math.Max(0, math.Sin(phase+basePhase))))
		set("lower_arm_r", AxisAngle(Vec3{1, 0, 0}, 0.25+0.35*math.Max(0, -math.Sin(phase+basePhase))))

		set("spine", Quat{1, 0, 0, 0}.Mul(AxisAngle(Vec3{0, 1, 0}, 0.08*math.Sin(phase))).Mul(AxisAngle(Vec3{1, 0, 0}, 0.05*math.Sin(2*phase))))
		set("chest", AxisAngle(Vec3{0, 1, 0}, -0.06*math.Sin(phase)))
		set("neck", AxisAngle(Vec3{1, 0, 0}, -0.04*math.Sin(phase)))
		set("head", AxisAngle(Vec3{1, 0, 0}, 0.03*math.Sin(phase)))

		bob := 0.018 * math.Abs(math.Sin(phase))
		roots[f] = Vec3{0, bob, speed * t}
		rotations[f] = q

		stance := int(math.Round(float64(period) * 0.42))
		contactL[f] = ((f + period/4) % period) < stance
		contactR[f] = ((f + 3*period/4) % period) < stance
	}

	framesData := make([]Frame, frames)
	for f := 0; f < frames; f++ {
		framesData[f] = Frame{Index: f, Root: roots[f], Rotations: rotations[f], Contacts: []bool{contactL[f], contactR[f]}}
	}
	return Clip{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Seed:          seed,
		FramesCount:   frames,
		Feet:          feet,
		Skeleton:      sk,
		Frames:        framesData,
		ContactTracks: []ContactTrack{
			{Foot: "left", Joint: "foot_l", Values: contactL},
			{Foot: "right", Joint: "foot_r", Values: contactR},
		},
	}
}

func BuildTruth(clip Clip) Truth {
	keySet := map[int]bool{0: true, len(clip.Frames) / 4: true, len(clip.Frames) / 2: true, 3 * len(clip.Frames) / 4: true, len(clip.Frames) - 1: true}
	keys := make([]int, 0, len(keySet))
	for f := range keySet {
		keys = append(keys, f)
	}
	sortInts(keys)
	positions := map[string]map[string]Vec3{}
	trajectory := make([]Vec3, len(clip.Frames))
	for f, frame := range clip.Frames {
		pose, err := ForwardKinematics(clip.Skeleton, frame.Root, frame.Rotations)
		if err != nil {
			panic(err.Error())
		}
		trajectory[f] = frame.Root
		if keySet[f] {
			row := map[string]Vec3{}
			for i, j := range clip.Skeleton.Joints {
				row[j.Name] = pose.WorldPos[i]
			}
			positions[fmt.Sprintf("%d", f)] = row
		}
	}
	bones := map[string]float64{}
	for _, j := range clip.Skeleton.Joints {
		if j.Parent != "" {
			bones[j.Name] = j.Offset.Norm()
		}
	}
	intervals := map[string][]Interval{}
	for _, track := range clip.ContactTracks {
		intervals[track.Foot] = ExtractIntervals(track.Values)
	}
	return Truth{
		SchemaVersion:    SchemaVersion,
		ClipID:           clip.ID,
		Seed:             clip.Seed,
		KeyFrames:        keys,
		WorldPositions:   positions,
		BoneLengths:      bones,
		ContactIntervals: intervals,
		RootStart:        clip.Frames[0].Root,
		RootEnd:          clip.Frames[len(clip.Frames)-1].Root,
		RootTrajectory:   trajectory,
	}
}

func ExtractIntervals(values []bool) []Interval {
	var out []Interval
	start := -1
	for i, v := range values {
		if v && start < 0 {
			start = i
		}
		if (!v || i == len(values)-1) && start >= 0 {
			end := i - 1
			if v {
				end = i
			}
			out = append(out, Interval{start, end})
			start = -1
		}
	}
	return out
}

func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
