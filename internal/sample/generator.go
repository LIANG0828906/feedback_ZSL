package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mocapworkbench/internal/model"
)

const generatorSeed int64 = 4208691

type clipSpec struct {
	ID     string
	Frames int
	Seed   int64
}

var clipSpecs = []clipSpec{
	{ID: "clip_short", Frames: 72, Seed: generatorSeed + 11},
	{ID: "clip_medium", Frames: 240, Seed: generatorSeed + 29},
	{ID: "clip_long", Frames: 600, Seed: generatorSeed + 47},
}

// Generate writes the complete deterministic sample corpus. The manifest never
// includes timestamps and every JSON object uses encoding/json's stable key
// ordering, so repeated invocations produce byte-identical files.
func Generate(outDir string, seed int64) (model.Manifest, error) {
	if seed == 0 {
		seed = generatorSeed
	}
	dirs := []string{
		filepath.Join(outDir, "clips"),
		filepath.Join(outDir, "targets"),
		filepath.Join(outDir, "truth"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return model.Manifest{}, err
		}
	}

	source := sourceSkeleton()
	targets := []model.TargetSkeleton{compactTarget(source), tallTarget(source), wideTarget(source)}
	manifest := model.Manifest{
		GeneratorVersion: model.GeneratorVersion,
		SchemaVersion:    model.SchemaVersion,
		Seed:             seed,
	}

	for i, spec := range clipSpecs {
		clip := generateClip(spec, source, seed+int64(i)*101)
		clipPath := filepath.Join("clips", spec.ID+".json")
		if err := writeJSON(filepath.Join(outDir, clipPath), clip); err != nil {
			return model.Manifest{}, err
		}
		truthPath := filepath.Join("truth", spec.ID+".truth.json")
		if err := writeJSON(filepath.Join(outDir, truthPath), clip.Truth); err != nil {
			return model.Manifest{}, err
		}
		clipEntry, err := manifestFile(outDir, clipPath, "clip", map[string]string{
			"clip_id": spec.ID,
			"frames":  fmt.Sprintf("%d", len(clip.Frames)),
			"joints":  fmt.Sprintf("%d", len(source.Joints)),
		})
		if err != nil {
			return model.Manifest{}, err
		}
		truthEntry, err := manifestFile(outDir, truthPath, "truth", map[string]string{"clip_id": spec.ID})
		if err != nil {
			return model.Manifest{}, err
		}
		manifest.Files = append(manifest.Files, clipEntry, truthEntry)
	}

	for _, target := range targets {
		targetPath := filepath.Join("targets", target.ID+".json")
		if err := writeJSON(filepath.Join(outDir, targetPath), target); err != nil {
			return model.Manifest{}, err
		}
		entry, err := manifestFile(outDir, targetPath, "target", map[string]string{
			"target_id": target.ID,
			"joints":    fmt.Sprintf("%d", len(target.Joints)),
		})
		if err != nil {
			return model.Manifest{}, err
		}
		manifest.Files = append(manifest.Files, entry)
	}

	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if err := writeJSON(filepath.Join(outDir, "manifest.json"), manifest); err != nil {
		return model.Manifest{}, err
	}
	return manifest, nil
}

func manifestFile(root, rel, kind string, metadata map[string]string) (model.ManifestEntry, error) {
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return model.ManifestEntry{}, err
	}
	sum := sha256.Sum256(data)
	return model.ManifestEntry{
		ID:       strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)),
		Path:     filepath.ToSlash(rel),
		Kind:     kind,
		SHA256:   hex.EncodeToString(sum[:]),
		Metadata: metadata,
	}, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	// Windows cannot rename over an existing destination.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, path)
}

func sourceSkeleton() model.Skeleton {
	return model.Skeleton{
		ID: "source_biped_21",
		Joints: []model.Joint{
			joint("pelvis", "", 0, 0, 0, 180, false),
			joint("spine", "pelvis", 0, 0.18, 0, 22, false),
			joint("chest", "spine", 0, 0.28, 0, 22, false),
			joint("neck", "chest", 0, 0.22, 0, 28, false),
			joint("head", "neck", 0, 0.16, 0, 32, true),
			joint("clavicle_l", "chest", 0.055, 0.16, 0, 18, false),
			joint("upper_arm_l", "clavicle_l", 0.16, 0, 0, 105, false),
			joint("forearm_l", "upper_arm_l", 0.31, 0, 0, 82, false),
			joint("hand_l", "forearm_l", 0.26, 0, 0, 48, true),
			joint("clavicle_r", "chest", -0.055, 0.16, 0, 18, false),
			joint("upper_arm_r", "clavicle_r", -0.16, 0, 0, 105, false),
			joint("forearm_r", "upper_arm_r", -0.31, 0, 0, 82, false),
			joint("hand_r", "forearm_r", -0.26, 0, 0, 48, true),
			joint("thigh_l", "pelvis", 0.105, -0.055, 0, 88, false),
			joint("shin_l", "thigh_l", 0, -0.45, 0, 78, false),
			joint("foot_l", "shin_l", 0, -0.42, 0.025, 42, true),
			joint("toe_l", "foot_l", 0, -0.07, 0.17, 35, true),
			joint("thigh_r", "pelvis", -0.105, -0.055, 0, 88, false),
			joint("shin_r", "thigh_r", 0, -0.45, 0, 78, false),
			joint("foot_r", "shin_r", 0, -0.42, 0.025, 42, true),
			joint("toe_r", "foot_r", 0, -0.07, 0.17, 35, true),
		},
	}
}

func joint(name, parent string, x, y, z, limit float64, end bool) model.Joint {
	return model.Joint{
		Name: name, Parent: parent,
		Offset:      model.Vec3{X: x, Y: y, Z: z},
		MaxAngleDeg: limit, EndEffector: end,
	}
}

func compactTarget(source model.Skeleton) model.TargetSkeleton {
	joints := []model.Joint{
		joint("pelvis", "", 0, 0, 0, 180, false),
		joint("spine", "pelvis", 0, 0.22, 0, 22, false),
		joint("chest", "spine", 0, 0.34, 0, 22, false),
		joint("neck", "chest", 0, 0.27, 0, 28, false),
		joint("head", "neck", 0, 0.20, 0, 32, true),
		joint("upper_arm_l", "chest", 0.20, 0.18, 0, 105, false),
		joint("forearm_l", "upper_arm_l", 0.38, 0, 0, 82, false),
		joint("hand_l", "forearm_l", 0.32, 0, 0, 48, true),
		joint("upper_arm_r", "chest", -0.20, 0.18, 0, 105, false),
		joint("forearm_r", "upper_arm_r", -0.38, 0, 0, 82, false),
		joint("hand_r", "forearm_r", -0.32, 0, 0, 48, true),
		joint("thigh_l", "pelvis", 0.13, -0.07, 0, 88, false),
		joint("shin_l", "thigh_l", 0, -0.54, 0, 78, false),
		joint("foot_l", "shin_l", 0, -0.50, 0.03, 42, true),
		joint("thigh_r", "pelvis", -0.13, -0.07, 0, 88, false),
		joint("shin_r", "thigh_r", 0, -0.54, 0, 78, false),
		joint("foot_r", "shin_r", 0, -0.50, 0.03, 42, true),
	}
	return model.TargetSkeleton{
		ID:     "target_compact_17",
		Joints: joints,
		Mapping: []model.MapRule{
			{Source: "pelvis", Targets: []string{"pelvis"}, Strategy: "identity"},
			{Source: "spine", Targets: []string{"spine"}, Strategy: "identity"},
			{Source: "chest", Targets: []string{"chest"}, Strategy: "identity"},
			{Source: "neck", Targets: []string{"neck"}, Strategy: "identity"},
			{Source: "head", Targets: []string{"head"}, Strategy: "identity"},
			{Source: "upper_arm_l", Targets: []string{"upper_arm_l"}, Strategy: "proportional"},
			{Source: "forearm_l", Targets: []string{"forearm_l"}, Strategy: "proportional"},
			{Source: "hand_l", Targets: []string{"hand_l"}, Strategy: "proportional"},
			{Source: "upper_arm_r", Targets: []string{"upper_arm_r"}, Strategy: "proportional"},
			{Source: "forearm_r", Targets: []string{"forearm_r"}, Strategy: "proportional"},
			{Source: "hand_r", Targets: []string{"hand_r"}, Strategy: "proportional"},
			{Source: "thigh_l", Targets: []string{"thigh_l"}, Strategy: "proportional"},
			{Source: "shin_l", Targets: []string{"shin_l"}, Strategy: "proportional"},
			{Source: "foot_l", Targets: []string{"foot_l"}, Strategy: "proportional"},
			{Source: "thigh_r", Targets: []string{"thigh_r"}, Strategy: "proportional"},
			{Source: "shin_r", Targets: []string{"shin_r"}, Strategy: "proportional"},
			{Source: "foot_r", Targets: []string{"foot_r"}, Strategy: "proportional"},
		},
		ContactJoints: map[string]string{"foot_l": "foot_l", "foot_r": "foot_r"},
	}
}

func tallTarget(source model.Skeleton) model.TargetSkeleton {
	joints := []model.Joint{
		joint("pelvis", "", 0, 0, 0, 180, false),
		joint("spine", "pelvis", 0, 0.21, 0, 22, false),
		joint("chest", "spine", 0, 0.32, 0, 22, false),
		joint("neck", "chest", 0, 0.26, 0, 28, false),
		joint("head", "neck", 0, 0.19, 0, 32, true),
		joint("chin", "head", 0, 0.02, 0.10, 16, true),
		joint("clavicle_l", "chest", 0.065, 0.18, 0, 18, false),
		joint("upper_arm_l", "clavicle_l", 0.19, 0, 0, 105, false),
		joint("upper_arm_twist_l", "upper_arm_l", 0.02, -0.01, 0, 60, false),
		joint("forearm_l", "upper_arm_l", 0.36, 0, 0, 82, false),
		joint("hand_l", "forearm_l", 0.30, 0, 0, 48, true),
		joint("thumb_l", "hand_l", 0.05, -0.03, 0.04, 30, true),
		joint("index_l", "hand_l", 0.11, 0.015, 0.01, 24, true),
		joint("clavicle_r", "chest", -0.065, 0.18, 0, 18, false),
		joint("upper_arm_r", "clavicle_r", -0.19, 0, 0, 105, false),
		joint("upper_arm_twist_r", "upper_arm_r", -0.02, -0.01, 0, 60, false),
		joint("forearm_r", "upper_arm_r", -0.36, 0, 0, 82, false),
		joint("hand_r", "forearm_r", -0.30, 0, 0, 48, true),
		joint("thumb_r", "hand_r", -0.05, -0.03, 0.04, 30, true),
		joint("index_r", "hand_r", -0.11, 0.015, 0.01, 24, true),
		joint("thigh_l", "pelvis", 0.11, -0.07, 0, 88, false),
		joint("shin_l", "thigh_l", 0, -0.50, 0, 78, false),
		joint("foot_l", "shin_l", 0, -0.45, 0.03, 42, true),
		joint("toe_l", "foot_l", 0, -0.07, 0.18, 35, true),
		joint("thigh_r", "pelvis", -0.11, -0.07, 0, 88, false),
		joint("shin_r", "thigh_r", 0, -0.50, 0, 78, false),
		joint("foot_r", "shin_r", 0, -0.45, 0.03, 42, true),
		joint("toe_r", "foot_r", 0, -0.07, 0.18, 35, true),
		joint("tail", "pelvis", 0, -0.03, -0.14, 35, true),
	}
	return model.TargetSkeleton{
		ID:     "target_tall_29",
		Joints: joints,
		Mapping: []model.MapRule{
			{Source: "pelvis", Targets: []string{"pelvis", "tail"}, Strategy: "one_to_many"},
			{Source: "spine", Targets: []string{"spine"}, Strategy: "identity"},
			{Source: "chest", Targets: []string{"chest"}, Strategy: "identity"},
			{Source: "neck", Targets: []string{"neck"}, Strategy: "identity"},
			{Source: "head", Targets: []string{"head"}, Strategy: "identity"},
			{Source: "head", Targets: []string{"chin"}, Strategy: "one_to_many"},
			{Source: "clavicle_l", Targets: []string{"clavicle_l"}, Strategy: "proportional"},
			{Source: "upper_arm_l", Targets: []string{"upper_arm_l", "upper_arm_twist_l"}, Strategy: "one_to_many"},
			{Source: "forearm_l", Targets: []string{"forearm_l"}, Strategy: "proportional"},
			{Source: "hand_l", Targets: []string{"hand_l", "thumb_l", "index_l"}, Strategy: "one_to_many"},
			{Source: "clavicle_r", Targets: []string{"clavicle_r"}, Strategy: "proportional"},
			{Source: "upper_arm_r", Targets: []string{"upper_arm_r", "upper_arm_twist_r"}, Strategy: "one_to_many"},
			{Source: "forearm_r", Targets: []string{"forearm_r"}, Strategy: "proportional"},
			{Source: "hand_r", Targets: []string{"hand_r", "thumb_r", "index_r"}, Strategy: "one_to_many"},
			{Source: "thigh_l", Targets: []string{"thigh_l"}, Strategy: "proportional"},
			{Source: "shin_l", Targets: []string{"shin_l"}, Strategy: "proportional"},
			{Source: "foot_l", Targets: []string{"foot_l"}, Strategy: "proportional"},
			{Source: "toe_l", Targets: []string{"toe_l"}, Strategy: "proportional"},
			{Source: "thigh_r", Targets: []string{"thigh_r"}, Strategy: "proportional"},
			{Source: "shin_r", Targets: []string{"shin_r"}, Strategy: "proportional"},
			{Source: "foot_r", Targets: []string{"foot_r"}, Strategy: "proportional"},
			{Source: "toe_r", Targets: []string{"toe_r"}, Strategy: "proportional"},
		},
		ContactJoints: map[string]string{"foot_l": "foot_l", "foot_r": "foot_r"},
	}
}

func wideTarget(source model.Skeleton) model.TargetSkeleton {
	joints := []model.Joint{
		joint("pelvis", "", 0, 0, 0, 180, false),
		joint("spine", "pelvis", 0, 0.16, 0, 22, false),
		joint("chest", "spine", 0, 0.26, 0, 22, false),
		joint("neck", "chest", 0, 0.20, 0, 28, false),
		joint("neck_upper", "neck", 0, 0.10, 0, 24, false),
		joint("head", "neck_upper", 0, 0.12, 0, 32, true),
		joint("clavicle_l", "chest", 0.09, 0.14, 0, 18, false),
		joint("upper_arm_l", "clavicle_l", 0.24, 0, 0, 105, false),
		joint("forearm_l", "upper_arm_l", 0.42, 0, 0, 82, false),
		joint("hand_l", "forearm_l", 0.34, 0, 0, 48, true),
		joint("clavicle_r", "chest", -0.09, 0.14, 0, 18, false),
		joint("upper_arm_r", "clavicle_r", -0.24, 0, 0, 105, false),
		joint("forearm_r", "upper_arm_r", -0.42, 0, 0, 82, false),
		joint("hand_r", "forearm_r", -0.34, 0, 0, 48, true),
		joint("thigh_l", "pelvis", 0.15, -0.05, 0, 88, false),
		joint("shin_l", "thigh_l", 0, -0.40, 0, 78, false),
		joint("foot_l", "shin_l", 0, -0.38, 0.02, 42, true),
		joint("toe_l", "foot_l", 0, -0.08, 0.20, 35, true),
		joint("thigh_r", "pelvis", -0.15, -0.05, 0, 88, false),
		joint("shin_r", "thigh_r", 0, -0.40, 0, 78, false),
		joint("foot_r", "shin_r", 0, -0.38, 0.02, 42, true),
		joint("toe_r", "foot_r", 0, -0.08, 0.20, 35, true),
	}
	return model.TargetSkeleton{
		ID:     "target_wide_21",
		Joints: joints,
		Mapping: []model.MapRule{
			{Source: "pelvis", Targets: []string{"pelvis"}, Strategy: "identity"},
			{Source: "spine", Targets: []string{"spine"}, Strategy: "identity"},
			{Source: "chest", Targets: []string{"chest"}, Strategy: "identity"},
			{Source: "neck", Targets: []string{"neck"}, Strategy: "identity"},
			{Source: "head", Targets: []string{"head"}, Strategy: "identity"},
			{Source: "clavicle_l", Targets: []string{"clavicle_l"}, Strategy: "proportional"},
			{Source: "upper_arm_l", Targets: []string{"upper_arm_l"}, Strategy: "proportional"},
			{Source: "forearm_l", Targets: []string{"forearm_l"}, Strategy: "proportional"},
			{Source: "hand_l", Targets: []string{"hand_l"}, Strategy: "proportional"},
			{Source: "clavicle_r", Targets: []string{"clavicle_r"}, Strategy: "proportional"},
			{Source: "upper_arm_r", Targets: []string{"upper_arm_r"}, Strategy: "proportional"},
			{Source: "forearm_r", Targets: []string{"forearm_r"}, Strategy: "proportional"},
			{Source: "hand_r", Targets: []string{"hand_r"}, Strategy: "proportional"},
			{Source: "thigh_l", Targets: []string{"thigh_l"}, Strategy: "proportional"},
			{Source: "shin_l", Targets: []string{"shin_l"}, Strategy: "proportional"},
			{Source: "foot_l", Targets: []string{"foot_l"}, Strategy: "proportional"},
			{Source: "toe_l", Targets: []string{"toe_l"}, Strategy: "proportional"},
			{Source: "thigh_r", Targets: []string{"thigh_r"}, Strategy: "proportional"},
			{Source: "shin_r", Targets: []string{"shin_r"}, Strategy: "proportional"},
			{Source: "foot_r", Targets: []string{"foot_r"}, Strategy: "proportional"},
			{Source: "toe_r", Targets: []string{"toe_r"}, Strategy: "proportional"},
		},
		Fallbacks: []model.FallbackRule{
			{Target: "neck_upper", Source: "neck", Strategy: "fallback_inherit", Reason: "target has no source neck_upper joint"},
		},
		ContactJoints: map[string]string{"foot_l": "foot_l", "foot_r": "foot_r"},
	}
}

func generateClip(spec clipSpec, skeleton model.Skeleton, seed int64) model.Clip {
	rng := newLCG(seed)
	phase0 := rng.Float64() * math.Pi * 2
	bobPhase := rng.Float64() * math.Pi * 2
	frames := make([]model.Frame, spec.Frames)
	for f := 0; f < spec.Frames; f++ {
		frames[f] = generateFrame(f, spec.Frames, skeleton, phase0, bobPhase)
	}
	truth := buildTruth(skeleton, frames)
	return model.Clip{
		SchemaVersion: model.SchemaVersion,
		ID:            spec.ID,
		Seed:          seed,
		FPS:           30,
		Skeleton:      skeleton,
		Frames:        frames,
		Truth:         truth,
	}
}

func generateFrame(index, total int, skeleton model.Skeleton, phase0, bobPhase float64) model.Frame {
	phase := phase0 + float64(index)*2*math.Pi/48
	x := math.Sin(phase)
	y := math.Cos(phase)
	rot := make([]model.Quat, len(skeleton.Joints))
	for i := range rot {
		rot[i] = model.QuatIdentity
	}
	rot[0] = model.QuatFromAxisAngle(model.Vec3{Y: 1}, 0.035*x)
	rot[1] = model.QuatFromEulerXYZ(0.018*y, 0.012*x, 0.012*x)
	rot[2] = model.QuatFromEulerXYZ(0.012*y, 0, -0.018*x)
	rot[3] = model.QuatFromEulerXYZ(-0.02*x, 0, 0.012*x)
	rot[4] = model.QuatFromEulerXYZ(0.018*x, 0, 0)

	legL := 0.44 * x
	legR := -0.44 * x
	kneeL := 0.20 + 0.26*math.Max(0, -x)
	kneeR := 0.20 + 0.26*math.Max(0, x)
	rot[13] = model.QuatFromAxisAngle(model.Vec3{X: 1}, legL)
	rot[14] = model.QuatFromAxisAngle(model.Vec3{X: 1}, kneeL)
	rot[15] = model.QuatFromAxisAngle(model.Vec3{X: 1}, -0.16*x)
	rot[16] = model.QuatFromAxisAngle(model.Vec3{X: 1}, 0.10*math.Max(0, x))
	rot[17] = model.QuatFromAxisAngle(model.Vec3{X: 1}, legR)
	rot[18] = model.QuatFromAxisAngle(model.Vec3{X: 1}, kneeR)
	rot[19] = model.QuatFromAxisAngle(model.Vec3{X: 1}, 0.16*x)
	rot[20] = model.QuatFromAxisAngle(model.Vec3{X: 1}, 0.10*math.Max(0, -x))

	armSwing := 0.28 * x
	rot[5] = model.QuatFromEulerXYZ(0.02*x, 0.02*y, 0)
	rot[6] = model.QuatFromEulerXYZ(armSwing, 0.04*x, 1.08)
	rot[7] = model.QuatFromEulerXYZ(0.24+0.08*y, 0, 0.04)
	rot[8] = model.QuatFromEulerXYZ(0, 0.06*x, 0.02*x)
	rot[9] = model.QuatFromEulerXYZ(-0.02*x, -0.02*y, 0)
	rot[10] = model.QuatFromEulerXYZ(-armSwing, -0.04*x, -1.08)
	rot[11] = model.QuatFromEulerXYZ(0.24+0.08*y, 0, -0.04)
	rot[12] = model.QuatFromEulerXYZ(0, -0.06*x, -0.02*x)

	cyc := float64(index) * 2 * math.Pi / 48
	leftContact := math.Sin(cyc) >= -0.05
	rightContact := math.Sin(cyc) <= 0.05
	if int(math.Floor(float64(index)/24))%2 == 1 {
		leftContact, rightContact = rightContact, leftContact
	}
	// The overlap is deliberate double support and is represented explicitly.
	if math.Abs(math.Sin(cyc)) < 0.10 {
		leftContact, rightContact = true, true
	}
	root := model.Vec3{
		X: 0.012 * math.Sin(phase0*0.7) * math.Sin(phase*0.5),
		Y: 1.0 + 0.028*math.Sin(phase0+bobPhase+float64(index)*2*math.Pi/24),
		Z: -0.95 + 0.008*float64(index),
	}
	return model.Frame{
		Index:           index,
		RootTranslation: root,
		Rotations:       rot,
		Contacts: map[string]bool{
			"foot_l": leftContact,
			"foot_r": rightContact,
		},
	}
}

type lcg struct {
	state uint64
}

func newLCG(seed int64) *lcg {
	return &lcg{state: uint64(seed) ^ 0x9e3779b97f4a7c15}
}

func (l *lcg) Uint64() uint64 {
	l.state ^= l.state >> 12
	l.state ^= l.state << 25
	l.state ^= l.state >> 27
	return l.state * 2685821657736338717
}

func (l *lcg) Float64() float64 {
	return float64(l.Uint64()>>11) / float64(1<<53)
}

func buildTruth(skeleton model.Skeleton, frames []model.Frame) model.Truth {
	keySet := map[int]bool{0: true, len(frames) / 4: true, len(frames) / 2: true, 3 * len(frames) / 4: true, len(frames) - 1: true}
	for f := 30; f < len(frames); f += 120 {
		keySet[f] = true
	}
	keyFrames := make([]int, 0, len(keySet))
	for f := range keySet {
		keyFrames = append(keyFrames, f)
	}
	sort.Ints(keyFrames)
	keyframes := make([]model.KeyframeTruth, 0, len(keyFrames))
	for _, f := range keyFrames {
		positions, _ := ForwardKinematics(skeleton, frames[f])
		world := make(map[string]model.Vec3, len(positions))
		for i, joint := range skeleton.Joints {
			world[joint.Name] = positions[i]
		}
		keyframes = append(keyframes, model.KeyframeTruth{Frame: f, World: world})
	}
	boneLengths := make(map[string]float64, len(skeleton.Joints))
	for _, joint := range skeleton.Joints {
		boneLengths[joint.Name] = joint.Offset.Length()
	}
	contacts := ContactIntervals(frames, []string{"foot_l", "foot_r"})
	return model.Truth{
		FrameCount:       len(frames),
		JointCount:       len(skeleton.Joints),
		Keyframes:        keyframes,
		BoneLengths:      boneLengths,
		ContactIntervals: contacts,
		RootStart:        frames[0].RootTranslation,
		RootEnd:          frames[len(frames)-1].RootTranslation,
		Tolerances: model.Tolerances{
			WorldPosition:      0.002,
			BoneLength:         0.00001,
			AngleDeg:           0.25,
			Reach:              0.005,
			ContactDrift:       0.001,
			ContactFrameSlack:  1,
			Root:               0.002,
			ScaleContactSlack:  1,
			ScaleRoot:          0.005,
			ResidualNormalized: 1,
		},
	}
}

// ForwardKinematics returns world positions aligned with skeleton.Joints.
func ForwardKinematics(skeleton model.Skeleton, frame model.Frame) ([]model.Vec3, []model.Quat) {
	index := make(map[string]int, len(skeleton.Joints))
	for i, joint := range skeleton.Joints {
		index[joint.Name] = i
	}
	pos := make([]model.Vec3, len(skeleton.Joints))
	rot := make([]model.Quat, len(skeleton.Joints))
	done := make([]bool, len(skeleton.Joints))
	var solve func(int)
	solve = func(i int) {
		if done[i] {
			return
		}
		j := skeleton.Joints[i]
		localRot := model.QuatIdentity
		if i < len(frame.Rotations) {
			localRot = frame.Rotations[i].Normalized()
		}
		if j.Parent == "" {
			pos[i] = frame.RootTranslation.Add(j.Offset)
			rot[i] = localRot
		} else {
			p := index[j.Parent]
			solve(p)
			pos[i] = pos[p].Add(rot[p].Rotate(j.Offset))
			rot[i] = rot[p].Mul(localRot).Normalized()
		}
		done[i] = true
	}
	for i := range skeleton.Joints {
		solve(i)
	}
	return pos, rot
}

func ContactIntervals(frames []model.Frame, feet []string) map[string][]model.ContactInterval {
	out := make(map[string][]model.ContactInterval, len(feet))
	for _, foot := range feet {
		var intervals []model.ContactInterval
		start := -1
		for i := range frames {
			on := frames[i].Contacts[foot]
			if on && start < 0 {
				start = i
			}
			if !on && start >= 0 {
				intervals = append(intervals, model.ContactInterval{Start: start, End: i - 1})
				start = -1
			}
		}
		if start >= 0 {
			intervals = append(intervals, model.ContactInterval{Start: start, End: len(frames) - 1})
		}
		out[foot] = intervals
	}
	return out
}
