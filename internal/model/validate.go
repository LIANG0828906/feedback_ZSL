package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

func ValidateSkeleton(s Skeleton) error {
	if strings.TrimSpace(s.Name) == "" {
		return Err(E_SCHEMA, "validate_skeleton", "skeleton.name is required")
	}
	if len(s.Joints) == 0 {
		return Err(E_SCHEMA, "validate_skeleton", "skeleton.joints must not be empty")
	}

	names := make(map[string]int, len(s.Joints))
	roots := 0
	for i, j := range s.Joints {
		if strings.TrimSpace(j.Name) == "" {
			return ErrJoint(E_SCHEMA, "validate_skeleton", "", fmt.Sprintf("joints[%d].name is required", i))
		}
		if _, exists := names[j.Name]; exists {
			return ErrJoint(E_SCHEMA, "validate_skeleton", j.Name, "duplicate joint name")
		}
		names[j.Name] = i
		if !j.LocalOffset.IsFinite() {
			return ErrJoint(E_SCHEMA, "validate_skeleton", j.Name, "T-pose localOffset must be finite")
		}
		if math.IsNaN(j.RotationLimitDeg) || math.IsInf(j.RotationLimitDeg, 0) || j.RotationLimitDeg < 0 {
			return ErrJoint(E_SCHEMA, "validate_skeleton", j.Name, "rotationLimitDeg must be a finite non-negative number")
		}
		if j.Parent < -1 || j.Parent >= len(s.Joints) {
			return ErrJoint(E_SCHEMA, "validate_skeleton", j.Name, "parent reference is out of range")
		}
		if j.Parent == -1 {
			roots++
		}
	}
	for i := range s.Joints {
		seen := make(map[int]bool, len(s.Joints))
		cur := i
		for cur >= 0 {
			if seen[cur] {
				return ErrJoint(E_CYCLE, "validate_skeleton", s.Joints[cur].Name, "parent chain contains a cycle")
			}
			seen[cur] = true
			cur = s.Joints[cur].Parent
		}
	}
	if roots != 1 {
		return Err(E_SCHEMA, "validate_skeleton", fmt.Sprintf("skeleton must contain exactly one root joint, found %d", roots))
	}
	return nil
}

func ValidateClip(s Skeleton, c Clip) error {
	if strings.TrimSpace(c.Name) == "" {
		return Err(E_SCHEMA, "validate_clip", "clip.name is required")
	}
	if len(c.Frames) == 0 {
		return ErrFrame(E_FRAME, "validate_clip", 0, "", "clip must contain at least one frame")
	}

	for i, f := range c.Frames {
		if f.Index != i {
			return ErrFrame(E_FRAME, "validate_clip", i, "", fmt.Sprintf("frame index %d does not match sequence position %d", f.Index, i))
		}
		if len(f.LocalRotations) != len(s.Joints) {
			return ErrFrame(E_FRAME, "validate_clip", i, "", fmt.Sprintf("local rotation count %d does not match joint count %d", len(f.LocalRotations), len(s.Joints)))
		}
		if !f.RootTranslation.IsFinite() {
			return ErrFrame(E_FRAME, "validate_clip", i, "", "root translation is not finite")
		}
		for j, q := range f.LocalRotations {
			if !q.IsFinite() {
				return ErrFrame(E_FRAME, "validate_clip", i, s.Joints[j].Name, "quaternion contains NaN or infinity")
			}
			if q.W*q.W+q.X*q.X+q.Y*q.Y+q.Z*q.Z < 1e-12 {
				return ErrFrame(E_FRAME, "validate_clip", i, s.Joints[j].Name, "quaternion has zero length")
			}
		}
	}

	for i, ci := range c.Contacts {
		if strings.TrimSpace(ci.Foot) == "" {
			return Err(E_SCHEMA, "validate_clip", fmt.Sprintf("contacts[%d].foot is required", i))
		}
		if s.JointIndex(ci.Foot) < 0 {
			return ErrJoint(E_SCHEMA, "validate_clip", ci.Foot, "contact foot does not exist in source skeleton")
		}
		if ci.Start < 0 || ci.End < ci.Start || ci.End >= len(c.Frames) {
			return ErrFrame(E_FRAME, "validate_clip", ci.Start, ci.Foot, "contact interval is invalid")
		}
	}

	seenPhases := make(map[int]bool, len(c.Phases))
	for i, p := range c.Phases {
		if seenPhases[p.Index] {
			return ErrFrame(E_FRAME, "validate_clip", p.StartFrame, "", "duplicate phase index")
		}
		seenPhases[p.Index] = true
		if p.StartFrame < 0 || p.EndFrame < p.StartFrame || p.EndFrame >= len(c.Frames) {
			return ErrFrame(E_FRAME, "validate_clip", p.StartFrame, "", fmt.Sprintf("phase %d range is invalid", p.Index))
		}
		if i > 0 && p.StartFrame <= c.Phases[i-1].EndFrame {
			return ErrFrame(E_FRAME, "validate_clip", p.StartFrame, "", "phases must be ordered and non-overlapping")
		}
	}
	return nil
}

func ValidateMapping(source, target Skeleton, m Mapping) error {
	if strings.TrimSpace(m.Name) == "" {
		return Err(E_SCHEMA, "validate_mapping", "mapping.name is required")
	}
	if len(m.Rules) == 0 {
		return Err(E_SCHEMA, "validate_mapping", "mapping.rules must not be empty")
	}
	if m.PhaseSize < 0 {
		return Err(E_PARAM, "validate_mapping", "phaseSize must not be negative")
	}

	targetMapped := make(map[string]int, len(m.Rules))
	for i := range m.Rules {
		r := m.Rules[i]
		if source.JointIndex(r.Source) < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Source, fmt.Sprintf("rules[%d] references an unknown source joint", i))
		}
		if len(r.Targets) == 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Source, fmt.Sprintf("rules[%d].targets must not be empty", i))
		}
		seen := make(map[string]bool, len(r.Targets))
		for _, name := range r.Targets {
			if target.JointIndex(name) < 0 {
				return ErrJoint(E_SCHEMA, "validate_mapping", name, fmt.Sprintf("rules[%d] references an unknown target joint", i))
			}
			if seen[name] {
				return ErrJoint(E_SCHEMA, "validate_mapping", name, fmt.Sprintf("rules[%d] contains a duplicate target", i))
			}
			seen[name] = true
			if prev, exists := targetMapped[name]; exists {
				return ErrJoint(E_SCHEMA, "validate_mapping", name, fmt.Sprintf("target is mapped by both rules[%d] and rules[%d]", prev, i))
			}
			targetMapped[name] = i
		}
		if r.Weight < 0 || math.IsNaN(r.Weight) || math.IsInf(r.Weight, 0) {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Source, "weight must be finite and non-negative")
		}
		if err := validatePhaseList("mapping", r.Phases); err != nil {
			return err
		}
	}

	missingSeen := make(map[string]bool, len(m.Missing))
	for i, r := range m.Missing {
		idx := target.JointIndex(r.Joint)
		if idx < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, fmt.Sprintf("missing[%d] references an unknown target joint", i))
		}
		if missingSeen[r.Joint] {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, "duplicate missing-joint fallback")
		}
		missingSeen[r.Joint] = true
		if _, direct := targetMapped[r.Joint]; direct {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, "directly mapped target cannot also have a missing fallback")
		}
		switch r.Policy {
		case MissingIdentity:
		case MissingInheritParent:
			if target.Joints[idx].Parent < 0 {
				return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, "inherit_parent requires a parent joint")
			}
		case MissingNearestMappedAncestor:
			if nearestMappedAncestor(target, targetMapped, idx) < 0 {
				return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, "nearest_mapped_ancestor requires a mapped ancestor")
			}
		default:
			return ErrJoint(E_SCHEMA, "validate_mapping", r.Joint, fmt.Sprintf("unknown missing-joint policy %q", r.Policy))
		}
		if r.SourceAncestor != "" && source.JointIndex(r.SourceAncestor) < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", r.SourceAncestor, "fallback sourceAncestor does not exist")
		}
		if err := validatePhaseList("mapping", r.Phases); err != nil {
			return err
		}
	}
	for i, j := range target.Joints {
		if _, ok := targetMapped[j.Name]; ok {
			continue
		}
		if !missingSeen[j.Name] {
			return ErrJoint(E_SCHEMA, "validate_mapping", j.Name, fmt.Sprintf("target joints[%d] has neither a direct mapping nor a missing-joint fallback", i))
		}
	}

	effectorSeen := make(map[string]bool, len(m.Effectors))
	for i, e := range m.Effectors {
		if target.JointIndex(e.Target) < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", e.Target, fmt.Sprintf("effectors[%d] references an unknown target joint", i))
		}
		if source.JointIndex(e.Source) < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", e.Source, fmt.Sprintf("effectors[%d] references an unknown source joint", i))
		}
		if effectorSeen[e.Target] {
			return ErrJoint(E_SCHEMA, "validate_mapping", e.Target, "duplicate effector target")
		}
		effectorSeen[e.Target] = true
		if math.IsNaN(e.Tolerance) || math.IsInf(e.Tolerance, 0) || e.Tolerance <= 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", e.Target, "effector tolerance must be finite and positive")
		}
		if e.MaxIterations <= 0 || e.MaxIterations > 256 {
			return ErrJoint(E_SCHEMA, "validate_mapping", e.Target, "effector maxIterations must be between 1 and 256")
		}
		if err := validatePhaseList("mapping", e.Phases); err != nil {
			return err
		}
	}

	keys := make(map[string]bool, len(m.ScaleDefaults))
	for i, s := range m.ScaleDefaults {
		if strings.TrimSpace(s.Key) == "" {
			return Err(E_SCHEMA, "validate_mapping", fmt.Sprintf("scaleDefaults[%d].key is required", i))
		}
		if keys[s.Key] {
			return Err(E_SCHEMA, "validate_mapping", fmt.Sprintf("duplicate scale key %q", s.Key))
		}
		keys[s.Key] = true
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value <= 0 {
			return Err(E_SCHEMA, "validate_mapping", fmt.Sprintf("scale key %q must have a finite positive value", s.Key))
		}
		if s.Joint != "" && target.JointIndex(s.Joint) < 0 {
			return ErrJoint(E_SCHEMA, "validate_mapping", s.Joint, "scale coefficient references an unknown target joint")
		}
		if err := validatePhaseList("mapping", s.Phases); err != nil {
			return err
		}
	}
	return nil
}

func ValidateWorkspace(w Workspace) error {
	if err := ValidateSkeleton(w.Source); err != nil {
		return err
	}
	if err := ValidateClip(w.Source, w.Clip); err != nil {
		return err
	}
	if len(w.Targets) == 0 {
		return Err(E_SCHEMA, "validate_workspace", "at least one target skeleton is required")
	}
	targetNames := make(map[string]bool, len(w.Targets))
	for _, t := range w.Targets {
		if targetNames[t.Name] {
			return Err(E_SCHEMA, "validate_workspace", fmt.Sprintf("duplicate target name %q", t.Name))
		}
		targetNames[t.Name] = true
		if math.IsNaN(t.Scale) || math.IsInf(t.Scale, 0) || t.Scale <= 0 {
			return Err(E_PARAM, "validate_workspace", fmt.Sprintf("target %q scale must be finite and positive", t.Name))
		}
		if err := ValidateSkeleton(t.Skeleton); err != nil {
			return err
		}
	}
	for _, target := range w.Targets {
		if err := ValidateMapping(w.Source, target.Skeleton, w.Mapping); err != nil {
			return err
		}
		for _, contact := range w.Clip.Contacts {
			if target.Skeleton.JointIndex(contact.Foot) < 0 {
				return ErrJoint(E_SCHEMA, "validate_workspace", contact.Foot,
					fmt.Sprintf("contact foot is missing from target %q", target.Name))
			}
		}
	}

	for _, truth := range w.Truths {
		tt, ok := w.TargetByName(truth.Target)
		if !ok {
			return Err(E_SCHEMA, "validate_workspace", fmt.Sprintf("truth references unknown target %q", truth.Target))
		}
		if truth.Tolerance <= 0 || math.IsNaN(truth.Tolerance) {
			return Err(E_PARAM, "validate_workspace", "truth tolerance must be positive")
		}
		for _, kf := range truth.KeyFrames {
			if kf.Frame < 0 || kf.Frame >= len(w.Clip.Frames) {
				return ErrFrame(E_FRAME, "validate_workspace", kf.Frame, "", "truth key frame is out of range")
			}
			for _, wj := range kf.World {
				if tt.Skeleton.JointIndex(wj.Joint) < 0 {
					return ErrJoint(E_SCHEMA, "validate_workspace", wj.Joint, "truth references an unknown target joint")
				}
				if !wj.Position.IsFinite() {
					return ErrJoint(E_SCHEMA, "validate_workspace", wj.Joint, "truth position is not finite")
				}
			}
		}
		for joint, length := range truth.SourceBoneLengths {
			if w.Source.JointIndex(joint) < 0 {
				return ErrJoint(E_SCHEMA, "validate_workspace", joint, "truth source bone length references an unknown joint")
			}
			if math.IsNaN(length) || math.IsInf(length, 0) || length < 0 {
				return ErrJoint(E_SCHEMA, "validate_workspace", joint, "truth source bone length is invalid")
			}
		}
		for joint, length := range truth.BoneLengths {
			if tt.Skeleton.JointIndex(joint) < 0 {
				return ErrJoint(E_SCHEMA, "validate_workspace", joint, "truth target bone length references an unknown joint")
			}
			if math.IsNaN(length) || math.IsInf(length, 0) || length < 0 {
				return ErrJoint(E_SCHEMA, "validate_workspace", joint, "truth target bone length is invalid")
			}
		}
	}
	return nil
}

func validatePhaseList(op string, phases []int) error {
	seen := make(map[int]bool, len(phases))
	for _, p := range phases {
		if p < 0 {
			return Err(E_PARAM, op, "phase indexes must be non-negative")
		}
		if seen[p] {
			return Err(E_PARAM, op, fmt.Sprintf("duplicate phase dependency %d", p))
		}
		seen[p] = true
	}
	return nil
}

func nearestMappedAncestor(target Skeleton, mapped map[string]int, idx int) int {
	cur := target.Joints[idx].Parent
	for cur >= 0 {
		if _, ok := mapped[target.Joints[cur].Name]; ok {
			return cur
		}
		cur = target.Joints[cur].Parent
	}
	return -1
}

func SortedPhases(phases []int) []int {
	out := append([]int(nil), phases...)
	sort.Ints(out)
	return out
}
