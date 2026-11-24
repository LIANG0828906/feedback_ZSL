package engine

import (
	"math"
	"sort"

	"retargetlab/internal/fsm"
	"retargetlab/internal/geom"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
)

func buildDependencyGraph(target model.Skeleton, clip model.Clip, mapping model.Mapping) DependencyGraph {
	g := DependencyGraph{}
	phaseFrames := make(map[int][]int)
	for _, p := range clip.Phases {
		for f := p.StartFrame; f <= p.EndFrame; f++ {
			phaseFrames[p.Index] = append(phaseFrames[p.Index], f)
		}
	}
	if len(phaseFrames) == 0 {
		for f := range clip.Frames {
			phaseFrames[0] = append(phaseFrames[0], f)
		}
	}
	for ri, rule := range mapping.Rules {
		joints := make([]string, 0, len(rule.Targets))
		for _, targetName := range rule.Targets {
			joints = append(joints, descendants(target, targetName)...)
		}
		joints = uniqueSorted(joints)
		frames := framesForPhases(clip, rule.Phases)
		g.Rules = append(g.Rules, RuleDependency{
			RuleIndex: ri, Source: rule.Source, Targets: append([]string(nil), rule.Targets...),
			Joints: joints, Frames: frames,
		})
	}
	for _, mr := range mapping.Missing {
		g.Fallbacks = append(g.Fallbacks, FallbackDependency{
			Joint: mr.Joint, Policy: mr.Policy, SourceAncestor: mr.SourceAncestor,
			Joints: descendants(target, mr.Joint), Frames: framesForPhases(clip, mr.Phases),
		})
	}
	for _, sc := range mapping.ScaleDefaults {
		g.Scales = append(g.Scales, ScaleDependency{
			Key: sc.Key, Joint: sc.Joint, Frames: framesForPhases(clip, sc.Phases),
		})
	}
	for f := range clip.Frames {
		fd := FrameDependency{Frame: f}
		phase := clip.PhaseForFrame(f)
		for ri, rule := range mapping.Rules {
			if phaseActiveForDependency(rule.Phases, phase) {
				fd.MappingRules = append(fd.MappingRules, ri)
				for _, targetName := range rule.Targets {
					fd.Joints = append(fd.Joints, descendants(target, targetName)...)
				}
			}
		}
		for _, mr := range mapping.Missing {
			if phaseActiveForDependency(mr.Phases, phase) {
				fd.Joints = append(fd.Joints, descendants(target, mr.Joint)...)
			}
		}
		for _, sc := range mapping.ScaleDefaults {
			if phaseActiveForDependency(sc.Phases, phase) {
				fd.ScaleKeys = append(fd.ScaleKeys, sc.Key)
				if sc.Joint != "" {
					fd.Joints = append(fd.Joints, sc.Joint)
				}
			}
		}
		fd.MappingRules = uniqueInts(fd.MappingRules)
		fd.ScaleKeys = uniqueStrings(fd.ScaleKeys)
		fd.Joints = uniqueSorted(fd.Joints)
		g.Frames = append(g.Frames, fd)
	}
	return g
}

func (e *Engine) mappingEditInvalidations(oldSolver, newSolver *solver.Solver, oldMapping, newMapping model.Mapping, ruleIndex int) []Invalidation {
	oldRule := oldMapping.Rules[ruleIndex]
	newRule := newMapping.Rules[ruleIndex]
	declaredJoints := make([]string, 0, len(newRule.Targets)+len(oldRule.Targets))
	for _, name := range append(append([]string(nil), oldRule.Targets...), newRule.Targets...) {
		declaredJoints = append(declaredJoints, descendants(e.target.Skeleton, name)...)
	}
	declaredJoints = uniqueSorted(declaredJoints)

	perFrame := make(map[int]*Invalidation)
	merge := func(frame int, joints, reasons []string) {
		if frame < 0 || frame >= len(e.clip.Frames) {
			return
		}
		e.mu.RLock()
		state := e.states[frame].State()
		e.mu.RUnlock()
		if state == fsm.Committed {
			return
		}
		inv := perFrame[frame]
		if inv == nil {
			inv = &Invalidation{Frame: frame, Phase: e.clip.PhaseForFrame(frame)}
			perFrame[frame] = inv
		}
		inv.Joints = uniqueSorted(append(inv.Joints, joints...))
		inv.Reasons = uniqueStrings(append(inv.Reasons, reasons...))
	}

	for frame := range e.clip.Frames {
		phase := e.clip.PhaseForFrame(frame)
		if phaseActiveForDependency(oldRule.Phases, phase) || phaseActiveForDependency(newRule.Phases, phase) {
			merge(frame, declaredJoints, []string{"mapping rule is declared for the downstream action phase"})
		}
		oldLocals, errOld := oldSolver.LocalRotations(e.clip.Frames[frame])
		newLocals, errNew := newSolver.LocalRotations(e.clip.Frames[frame])
		if errOld != nil || errNew != nil {
			merge(frame, declaredJoints, []string{"mapping evaluation failed and requires recomputation"})
			continue
		}
		var changed []string
		for i := range oldLocals {
			if quatDiff(oldLocals[i], newLocals[i]) > 0 {
				changed = append(changed, e.target.Skeleton.Joints[i].Name)
				changed = append(changed, descendants(e.target.Skeleton, e.target.Skeleton.Joints[i].Name)...)
			}
		}
		if len(changed) > 0 {
			merge(frame, uniqueSorted(changed), []string{"edited mapping changes the solved local rotation"})
		}
	}
	out := make([]Invalidation, 0, len(perFrame))
	for _, inv := range perFrame {
		out = append(out, *inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Frame < out[j].Frame })
	return out
}

func (e *Engine) scaleEditInvalidations(oldSolver, newSolver *solver.Solver, coeff model.ScaleCoefficient) []Invalidation {
	joint := coeff.Joint
	if joint == "" {
		joint = rootJointName(e.target.Skeleton)
	}
	var out []Invalidation
	for frame := range e.clip.Frames {
		e.mu.RLock()
		state := e.states[frame].State()
		e.mu.RUnlock()
		if state == fsm.Committed {
			continue
		}
		oldScale, err1 := oldSolver.FrameRootScale(frame)
		newScale, err2 := newSolver.FrameRootScale(frame)
		reason := ""
		if err1 != nil || err2 != nil {
			reason = "scale evaluation failed and requires recomputation"
		} else if oldScale != newScale {
			reason = "edited scale coefficient changes the root trajectory"
		} else if phaseActiveForDependency(coeff.Phases, e.clip.PhaseForFrame(frame)) {
			reason = "scale coefficient is declared for the downstream action phase"
		}
		if reason != "" {
			out = append(out, Invalidation{
				Frame: frame, Joints: []string{joint}, Reasons: []string{reason},
				Phase: e.clip.PhaseForFrame(frame),
			})
		}
	}
	return out
}

func (e *Engine) mergeInvalidations(entries []Invalidation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, inv := range entries {
		cur := e.invalid[inv.Frame]
		if cur == nil {
			copyInv := inv
			cur = &copyInv
			e.invalid[inv.Frame] = cur
		} else {
			cur.Joints = uniqueSorted(append(cur.Joints, inv.Joints...))
			cur.Reasons = uniqueStrings(append(cur.Reasons, inv.Reasons...))
		}
	}
}

func (e *Engine) invalidListLocked() []Invalidation {
	out := make([]Invalidation, 0, len(e.invalid))
	for _, inv := range e.invalid {
		out = append(out, *inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Frame < out[j].Frame })
	return out
}

func framesForPhases(clip model.Clip, phases []int) []int {
	var out []int
	for f := range clip.Frames {
		if phaseActiveForDependency(phases, clip.PhaseForFrame(f)) {
			out = append(out, f)
		}
	}
	return out
}

func phaseActiveForDependency(phases []int, phase int) bool {
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

func descendants(s model.Skeleton, name string) []string {
	start := s.JointIndex(name)
	if start < 0 {
		return nil
	}
	out := []string{name}
	for i := range s.Joints {
		cur := s.Joints[i].Parent
		for cur >= 0 {
			if cur == start {
				out = append(out, s.Joints[i].Name)
				break
			}
			cur = s.Joints[cur].Parent
		}
	}
	return uniqueSorted(out)
}

func rootJointName(s model.Skeleton) string {
	for _, j := range s.Joints {
		if j.Parent < 0 {
			return j.Name
		}
	}
	return ""
}

func quatDiff(a, b geom.Quat) float64 {
	d1 := math.Abs(a.W-b.W) + math.Abs(a.X-b.X) + math.Abs(a.Y-b.Y) + math.Abs(a.Z-b.Z)
	d2 := math.Abs(a.W+b.W) + math.Abs(a.X+b.X) + math.Abs(a.Y+b.Y) + math.Abs(a.Z+b.Z)
	if d2 < d1 {
		return d2
	}
	return d1
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

func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func uniqueSorted(in []string) []string {
	return uniqueStrings(in)
}
