package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
)

func LoadClip(path string) (model.Clip, error) {
	var clip model.Clip
	data, err := os.ReadFile(path)
	if err != nil {
		return clip, model.NewError(model.ErrSchema, filepath.Base(path), "file", err.Error())
	}
	if err := checkClipJSONFields(data, filepath.Base(path)); err != nil {
		return clip, err
	}
	if err := json.Unmarshal(data, &clip); err != nil {
		return clip, model.NewError(model.ErrSchema, filepath.Base(path), "json", err.Error())
	}
	if err := ValidateClip(clip); err != nil {
		return clip, err
	}
	return clip, nil
}

func LoadTarget(path string) (model.TargetSkeleton, error) {
	var target model.TargetSkeleton
	data, err := os.ReadFile(path)
	if err != nil {
		return target, model.NewError(model.ErrSchema, filepath.Base(path), "file", err.Error())
	}
	if err := checkTargetJSONFields(data, filepath.Base(path)); err != nil {
		return target, err
	}
	if err := json.Unmarshal(data, &target); err != nil {
		return target, model.NewError(model.ErrSchema, filepath.Base(path), "json", err.Error())
	}
	if target.ID == "" {
		return target, model.NewError(model.ErrSchema, filepath.Base(path), "id", "missing target id")
	}
	if len(target.Joints) == 0 {
		return target, model.NewError(model.ErrSchema, target.ID, "joints", "target has no joints")
	}
	if target.ContactJoints == nil {
		return target, model.NewError(model.ErrSchema, target.ID, "contact_joints", "missing contact joint map")
	}
	if err := ValidateSkeleton(target.Skeleton()); err != nil {
		return target, err
	}
	return target, nil
}

func LoadManifest(path string) (model.Manifest, error) {
	var manifest model.Manifest
	if err := readJSON(path, &manifest); err != nil {
		return manifest, err
	}
	if manifest.GeneratorVersion != model.GeneratorVersion {
		return manifest, model.NewError(model.ErrSchema, filepath.Base(path), "generator_version", "unsupported generator version")
	}
	return manifest, nil
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.NewError(model.ErrSchema, filepath.Base(path), "file", err.Error())
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return model.NewError(model.ErrSchema, filepath.Base(path), "json", err.Error())
	}
	return nil
}

func checkClipJSONFields(data []byte, object string) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return model.NewError(model.ErrSchema, object, "json", err.Error())
	}
	if err := requireTop(root, object, "schema_version", "id", "fps", "skeleton", "frames", "truth"); err != nil {
		return err
	}
	if err := checkSkeletonJSONFields(root["skeleton"], object, "skeleton"); err != nil {
		return err
	}
	var frames []json.RawMessage
	if err := json.Unmarshal(root["frames"], &frames); err != nil {
		return model.NewError(model.ErrSchema, object, "frames", "frames must be an array")
	}
	for i, raw := range frames {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(raw, &frame); err != nil {
			return model.FrameError(model.ErrSchema, object, i, "frames", "frame must be an object")
		}
		if err := requireFrame(frame, object, i, "index", "root_translation", "rotations"); err != nil {
			return err
		}
		if !isVec3Object(frame["root_translation"]) {
			return model.FrameError(model.ErrSchema, object, i, "root_translation", "root_translation requires finite x/y/z fields")
		}
		var rotations []json.RawMessage
		if err := json.Unmarshal(frame["rotations"], &rotations); err != nil {
			return model.FrameError(model.ErrSchema, object, i, "rotations", "rotations must be an array")
		}
		for j, rawRot := range rotations {
			if !isQuatObject(rawRot) {
				return model.FrameError(model.ErrSchema, object, i, fmt.Sprintf("rotations[%d]", j), "quaternion requires x/y/z/w fields")
			}
		}
	}
	var truth map[string]json.RawMessage
	if err := json.Unmarshal(root["truth"], &truth); err != nil {
		return model.NewError(model.ErrSchema, object, "truth", "truth must be an object")
	}
	if err := requireTop(truth, object, "frame_count", "joint_count", "keyframes", "bone_lengths", "contact_intervals", "root_start", "root_end", "tolerances"); err != nil {
		return &model.AppError{Code: model.ErrSchema, Object: object, Frame: -1, Field: "truth." + fieldFromError(err), Message: err.Error()}
	}
	if !isVec3Object(truth["root_start"]) {
		return model.NewError(model.ErrSchema, object, "truth.root_start", "root_start requires finite x/y/z fields")
	}
	if !isVec3Object(truth["root_end"]) {
		return model.NewError(model.ErrSchema, object, "truth.root_end", "root_end requires finite x/y/z fields")
	}
	var keyframes []json.RawMessage
	if err := json.Unmarshal(truth["keyframes"], &keyframes); err != nil {
		return model.NewError(model.ErrSchema, object, "truth.keyframes", "keyframes must be an array")
	}
	for i, rawKeyframe := range keyframes {
		var keyframe map[string]json.RawMessage
		if err := json.Unmarshal(rawKeyframe, &keyframe); err != nil {
			return model.NewError(model.ErrSchema, object, fmt.Sprintf("truth.keyframes[%d]", i), "keyframe must be an object")
		}
		if err := requireTop(keyframe, object, "frame", "world"); err != nil {
			return &model.AppError{
				Code: model.ErrSchema, Object: object, Frame: -1,
				Field: fmt.Sprintf("truth.keyframes[%d].%s", i, fieldFromError(err)), Message: err.Error(),
			}
		}
		var world map[string]json.RawMessage
		if err := json.Unmarshal(keyframe["world"], &world); err != nil {
			return model.NewError(model.ErrSchema, object, fmt.Sprintf("truth.keyframes[%d].world", i), "world must be an object")
		}
		for _, joint := range sortedKeys(world) {
			position := world[joint]
			if !isVec3Object(position) {
				return model.NewError(model.ErrSchema, object, fmt.Sprintf("truth.keyframes[%d].world.%s", i, joint), "world position requires finite x/y/z fields")
			}
		}
	}
	return nil
}

func checkTargetJSONFields(data []byte, object string) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return model.NewError(model.ErrSchema, object, "json", err.Error())
	}
	if err := requireTop(root, object, "id", "joints", "mapping", "contact_joints"); err != nil {
		return err
	}
	rootRaw, err := json.Marshal(root)
	if err != nil {
		return err
	}
	return checkSkeletonJSONFields(rootRaw, object, "target")
}

func checkSkeletonJSONFields(raw json.RawMessage, object, prefix string) error {
	var skeleton map[string]json.RawMessage
	if err := json.Unmarshal(raw, &skeleton); err != nil {
		return model.NewError(model.ErrSchema, object, prefix, "skeleton must be an object")
	}
	fieldPrefix := ""
	if prefix == "skeleton" {
		if err := requireTop(skeleton, object, "id", "joints"); err != nil {
			return err
		}
		fieldPrefix = "skeleton."
	} else if err := requireTop(skeleton, object, "joints"); err != nil {
		return err
	}
	var joints []json.RawMessage
	if err := json.Unmarshal(skeleton["joints"], &joints); err != nil {
		return model.NewError(model.ErrSchema, object, fieldPrefix+"joints", "joints must be an array")
	}
	for i, rawJoint := range joints {
		var joint map[string]json.RawMessage
		if err := json.Unmarshal(rawJoint, &joint); err != nil {
			return model.NewError(model.ErrSchema, object, fmt.Sprintf("%sjoints[%d]", fieldPrefix, i), "joint must be an object")
		}
		if err := requireTop(joint, object, "name", "offset", "max_angle_deg"); err != nil {
			return &model.AppError{
				Code: model.ErrSchema, Object: object, Frame: -1,
				Field: fmt.Sprintf("%sjoints[%d].%s", fieldPrefix, i, fieldFromError(err)), Message: err.Error(),
			}
		}
		if !isVec3Object(joint["offset"]) {
			return model.NewError(model.ErrSchema, object, fmt.Sprintf("%sjoints[%d].offset", fieldPrefix, i), "offset requires finite x/y/z fields")
		}
	}
	return nil
}

func isVec3Object(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	for _, field := range []string{"x", "y", "z"} {
		if _, ok := value[field]; !ok {
			return false
		}
	}
	return true
}

func isQuatObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	for _, field := range []string{"x", "y", "z", "w"} {
		if _, ok := value[field]; !ok {
			return false
		}
	}
	return true
}

func requireFrame(raw map[string]json.RawMessage, object string, frame int, fields ...string) error {
	for _, field := range fields {
		if _, ok := raw[field]; !ok {
			return model.FrameError(model.ErrSchema, object, frame, field, "required field is missing")
		}
	}
	return nil
}

func requireTop(raw map[string]json.RawMessage, object string, fields ...string) error {
	for _, field := range fields {
		if _, ok := raw[field]; !ok {
			return model.NewError(model.ErrSchema, object, field, "required field is missing")
		}
	}
	return nil
}

func fieldFromError(err error) string {
	var appErr *model.AppError
	if errors.As(err, &appErr) {
		return appErr.Field
	}
	return "field"
}

// ValidateClip checks structure, cycles, frame alignment and the declared
// analytic truth. Contact marker/target binding is validated in the retarget
// stage because only the target declares the required contact semantics.
func ValidateClip(clip model.Clip) error {
	object := clip.ID
	if object == "" {
		object = "clip"
	}
	if clip.SchemaVersion == "" {
		return model.NewError(model.ErrSchema, object, "schema_version", "missing clip schema version")
	}
	if clip.SchemaVersion != model.SchemaVersion {
		return model.NewError(model.ErrSchema, object, "schema_version", "unsupported clip schema version")
	}
	if clip.ID == "" {
		return model.NewError(model.ErrSchema, object, "id", "missing clip id")
	}
	if clip.FPS <= 0 || !finite(clip.FPS) {
		return model.NewError(model.ErrSchema, object, "fps", "fps must be finite and positive")
	}
	if len(clip.Frames) == 0 {
		return model.NewError(model.ErrSchema, object, "frames", "clip has no frames")
	}
	if err := ValidateSkeleton(clip.Skeleton); err != nil {
		return err
	}
	for frameNo, frame := range clip.Frames {
		if frame.Index != frameNo {
			return model.FrameError(model.ErrFrame, object, frameNo, "index", fmt.Sprintf("frame index is %d", frame.Index))
		}
		if !frame.RootTranslation.IsFinite() {
			return model.FrameError(model.ErrSchema, object, frameNo, "root_translation", "root translation is not finite")
		}
		if len(frame.Rotations) != len(clip.Skeleton.Joints) {
			return model.FrameError(model.ErrFrame, object, frameNo, "rotations", fmt.Sprintf("rotation count %d != joint count %d", len(frame.Rotations), len(clip.Skeleton.Joints)))
		}
		for jointNo, q := range frame.Rotations {
			if !q.IsFinite() {
				return model.FrameError(model.ErrSchema, object, frameNo, fmt.Sprintf("rotations[%d]", jointNo), "quaternion is not finite")
			}
			if n := q.Norm(); math.Abs(n-1) > 1e-4 {
				return model.FrameError(model.ErrSchema, object, frameNo, fmt.Sprintf("rotations[%d]", jointNo), "quaternion is not unit length")
			}
		}
	}
	if clip.Truth.FrameCount != len(clip.Frames) {
		return model.FrameError(model.ErrFrame, object, min(clip.Truth.FrameCount, len(clip.Frames)-1), "truth.frame_count", fmt.Sprintf("declared %d, actual %d", clip.Truth.FrameCount, len(clip.Frames)))
	}
	if clip.Truth.JointCount != len(clip.Skeleton.Joints) {
		return model.NewError(model.ErrSchema, object, "truth.joint_count", fmt.Sprintf("declared %d, actual %d", clip.Truth.JointCount, len(clip.Skeleton.Joints)))
	}
	if err := validateTruth(clip); err != nil {
		return err
	}
	return nil
}

func ValidateSkeleton(skeleton model.Skeleton) error {
	object := skeleton.ID
	if object == "" {
		return model.NewError(model.ErrSchema, "skeleton", "id", "missing skeleton id")
	}
	if len(skeleton.Joints) == 0 {
		return model.NewError(model.ErrSchema, object, "joints", "skeleton has no joints")
	}
	byName := make(map[string]model.Joint, len(skeleton.Joints))
	roots := 0
	for i, joint := range skeleton.Joints {
		if joint.Name == "" {
			return model.NewError(model.ErrSchema, object, fmt.Sprintf("joints[%d].name", i), "missing joint name")
		}
		if _, exists := byName[joint.Name]; exists {
			return model.NewError(model.ErrSchema, object, "joints", "duplicate joint "+joint.Name)
		}
		if !joint.Offset.IsFinite() {
			return model.NewError(model.ErrSchema, object, "joints."+joint.Name+".offset", "offset is not finite")
		}
		if joint.MaxAngleDeg <= 0 || joint.MaxAngleDeg > 180 || !finite(joint.MaxAngleDeg) {
			return model.NewError(model.ErrSchema, object, "joints."+joint.Name+".max_angle_deg", "must be finite in (0,180]")
		}
		if joint.Parent == "" {
			roots++
		} else if joint.Parent == joint.Name {
			return model.NewError(model.ErrCycle, object, "joints."+joint.Name+".parent", "joint is its own parent")
		}
		byName[joint.Name] = joint
	}
	if roots != 1 {
		return model.NewError(model.ErrSchema, object, "joints", fmt.Sprintf("expected exactly one root, found %d", roots))
	}
	state := make(map[string]int, len(byName))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return model.NewError(model.ErrCycle, object, "joints."+name+".parent", "parent chain contains a cycle")
		case 2:
			return nil
		}
		state[name] = 1
		joint := byName[name]
		if joint.Parent != "" {
			if _, ok := byName[joint.Parent]; !ok {
				return model.NewError(model.ErrSchema, object, "joints."+name+".parent", "unknown parent "+joint.Parent)
			}
			if err := visit(joint.Parent); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for name := range byName {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

func ValidateTarget(target model.TargetSkeleton, source model.Skeleton) error {
	if err := ValidateSkeleton(target.Skeleton()); err != nil {
		return err
	}
	if target.ID == "" {
		return model.NewError(model.ErrSchema, "target", "id", "missing target id")
	}
	if len(target.Mapping) == 0 {
		return model.NewError(model.ErrMap, target.ID, "mapping", "target has no mapping rules")
	}
	sourceJoints, err := jointIndex(source)
	if err != nil {
		return err
	}
	targetJoints, err := jointIndex(model.Skeleton{ID: target.ID, Joints: target.Joints})
	if err != nil {
		return err
	}
	covered := make(map[string]bool, len(target.Joints))
	for _, rule := range target.Mapping {
		if _, ok := sourceJoints[rule.Source]; !ok {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping.source", Message: "unknown source joint", Details: []string{rule.Source}}
		}
		if len(rule.Targets) == 0 {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping.targets", Message: "mapping target list is empty", Details: []string{rule.Source}}
		}
		for _, name := range rule.Targets {
			if _, ok := targetJoints[name]; !ok {
				return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping.targets", Message: "unknown target joint", Details: []string{name}}
			}
			if covered[name] {
				return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping.targets", Message: "target joint mapped more than once", Details: []string{name}}
			}
			covered[name] = true
		}
	}
	for _, fallback := range target.Fallbacks {
		if _, ok := targetJoints[fallback.Target]; !ok {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "fallbacks.target", Message: "unknown fallback target", Details: []string{fallback.Target}}
		}
		if _, ok := sourceJoints[fallback.Source]; !ok {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "fallbacks.source", Message: "unknown fallback source", Details: []string{fallback.Source}}
		}
		if covered[fallback.Target] {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "fallbacks.target", Message: "fallback target already mapped", Details: []string{fallback.Target}}
		}
		covered[fallback.Target] = true
	}
	var missing []string
	for _, joint := range target.Joints {
		if !covered[joint.Name] {
			missing = append(missing, joint.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "mapping", Message: "no feasible mapping for target joints", Details: missing}
	}
	if len(target.ContactJoints) == 0 {
		return model.NewError(model.ErrSchema, target.ID, "contact_joints", "missing contact joint declaration")
	}
	for sourceFoot, targetFoot := range target.ContactJoints {
		if sourceFoot == "" {
			return model.NewError(model.ErrSchema, target.ID, "contact_joints", "empty source contact joint")
		}
		if _, ok := targetJoints[targetFoot]; !ok {
			return &model.AppError{Code: model.ErrMap, Object: target.ID, Frame: -1, Field: "contact_joints." + sourceFoot, Message: "contact target joint does not exist", Details: []string{targetFoot}}
		}
	}
	return nil
}

func validateTruth(clip model.Clip) error {
	truth := clip.Truth
	object := clip.ID
	if len(truth.Keyframes) == 0 {
		return model.NewError(model.ErrSchema, object, "truth.keyframes", "truth has no keyframes")
	}
	tol := truth.Tolerances
	if tol.WorldPosition <= 0 || tol.BoneLength <= 0 || tol.AngleDeg <= 0 || tol.Reach <= 0 ||
		tol.ContactDrift <= 0 || tol.Root <= 0 || tol.ScaleRoot <= 0 {
		return model.NewError(model.ErrSchema, object, "truth.tolerances", "all tolerances must be positive")
	}
	if tol.ContactFrameSlack < 0 || tol.ScaleContactSlack < 0 {
		return model.NewError(model.ErrSchema, object, "truth.tolerances", "contact slack must be non-negative")
	}
	for _, joint := range clip.Skeleton.Joints {
		length, ok := truth.BoneLengths[joint.Name]
		if !ok {
			return model.NewError(model.ErrSchema, object, "truth.bone_lengths."+joint.Name, "missing declared bone length")
		}
		if math.Abs(length-joint.Offset.Length()) > tol.BoneLength {
			return model.NewError(model.ErrSchema, object, "truth.bone_lengths."+joint.Name, "declared bone length does not match offset")
		}
	}
	for frameNo, kf := range truth.Keyframes {
		if kf.Frame < 0 || kf.Frame >= len(clip.Frames) {
			return model.FrameError(model.ErrFrame, object, frameNo, "truth.keyframes.frame", "keyframe is outside clip")
		}
		if len(kf.World) != len(clip.Skeleton.Joints) {
			return model.FrameError(model.ErrFrame, object, kf.Frame, "truth.keyframes.world", "keyframe joint count does not match skeleton")
		}
		positions, _ := sample.ForwardKinematics(clip.Skeleton, clip.Frames[kf.Frame])
		for i, joint := range clip.Skeleton.Joints {
			world, ok := kf.World[joint.Name]
			if !ok {
				return model.FrameError(model.ErrSchema, object, kf.Frame, "truth.keyframes.world."+joint.Name, "missing keyframe joint")
			}
			if !world.IsFinite() {
				return model.FrameError(model.ErrSchema, object, kf.Frame, "truth.keyframes.world."+joint.Name, "world position is not finite")
			}
			if world.Sub(positions[i]).Length() > tol.WorldPosition {
				return model.FrameError(model.ErrFrame, object, kf.Frame, "truth.keyframes.world."+joint.Name, "declared world position does not match FK")
			}
		}
	}
	if clip.Frames[0].RootTranslation.Sub(truth.RootStart).Length() > tol.Root {
		return model.FrameError(model.ErrFrame, object, 0, "truth.root_start", "declared root start does not match frame")
	}
	last := len(clip.Frames) - 1
	if clip.Frames[last].RootTranslation.Sub(truth.RootEnd).Length() > tol.Root {
		return model.FrameError(model.ErrFrame, object, last, "truth.root_end", "declared root end does not match frame")
	}
	derived := contactIntervals(clip.Frames)
	for _, foot := range sortedKeys(derived) {
		declared := truth.ContactIntervals[foot]
		if err := validateIntervals(object, foot, declared, len(clip.Frames)); err != nil {
			return err
		}
		if !intervalsEqual(derived[foot], declared) {
			frame := firstIntervalDifference(derived[foot], declared)
			return model.FrameError(model.ErrMark, object, frame, "truth.contact_intervals."+foot, "marker transitions contradict declared contact intervals")
		}
	}
	for foot := range truth.ContactIntervals {
		if _, ok := derived[foot]; !ok {
			return model.FrameError(model.ErrMark, object, 0, "truth.contact_intervals."+foot, "declared foot has no contact markers")
		}
	}
	return nil
}

func validateIntervals(object, foot string, intervals []model.ContactInterval, frameCount int) error {
	lastEnd := -2
	for _, span := range intervals {
		if span.Start < 0 || span.End < span.Start || span.End >= frameCount {
			frame := span.Start
			if frame < 0 {
				frame = 0
			}
			return model.FrameError(model.ErrMark, object, frame, "truth.contact_intervals."+foot, "contact interval is outside frame range")
		}
		if span.Start <= lastEnd {
			return model.FrameError(model.ErrMark, object, span.Start, "truth.contact_intervals."+foot, "contact intervals overlap or are unsorted")
		}
		lastEnd = span.End
	}
	return nil
}

func contactIntervals(frames []model.Frame) map[string][]model.ContactInterval {
	feet := make(map[string]bool)
	for _, frame := range frames {
		for foot := range frame.Contacts {
			feet[foot] = true
		}
	}
	out := make(map[string][]model.ContactInterval, len(feet))
	for foot := range feet {
		start := -1
		for i, frame := range frames {
			on := frame.Contacts[foot]
			if on && start < 0 {
				start = i
			}
			if !on && start >= 0 {
				out[foot] = append(out[foot], model.ContactInterval{Start: start, End: i - 1})
				start = -1
			}
		}
		if start >= 0 {
			out[foot] = append(out[foot], model.ContactInterval{Start: start, End: len(frames) - 1})
		}
	}
	return out
}

func intervalsEqual(a, b []model.ContactInterval) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstIntervalDifference(a, b []model.ContactInterval) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i].Start < b[i].Start {
				return a[i].Start
			}
			return b[i].Start
		}
	}
	if len(a) > n {
		return a[n].Start
	}
	if len(b) > n {
		return b[n].Start
	}
	return 0
}

func jointIndex(skeleton model.Skeleton) (map[string]int, error) {
	index := make(map[string]int, len(skeleton.Joints))
	for i, joint := range skeleton.Joints {
		index[joint.Name] = i
	}
	if len(index) != len(skeleton.Joints) {
		return nil, model.NewError(model.ErrSchema, skeleton.ID, "joints", "duplicate joint names")
	}
	return index, nil
}

func VerifyManifest(manifestPath string) error {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(manifestPath)
	for _, entry := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return model.NewError(model.ErrSchema, entry.Path, "file", err.Error())
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
			return model.NewError(model.ErrSchema, entry.Path, "sha256", fmt.Sprintf("manifest hash mismatch: got %s want %s", got, entry.SHA256))
		}
	}
	return nil
}

func SortedFiles(manifest model.Manifest) []model.ManifestEntry {
	out := append([]model.ManifestEntry(nil), manifest.Files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func FormatObject(object string) string {
	return strings.TrimSpace(object)
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
