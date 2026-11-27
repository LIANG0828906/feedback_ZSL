package stage

import (
	"fmt"
	"sort"
	"strings"
)

type DependencyNode struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	FixtureID    string   `json:"fixture_id,omitempty"`
	CueID        string   `json:"cue_id,omitempty"`
	WindowNumber int      `json:"window_number,omitempty"`
	TimeMS       int64    `json:"time_ms,omitempty"`
	Upstream     []string `json:"upstream,omitempty"`
	ContentHash  string   `json:"content_hash,omitempty"`
	CacheKey     string   `json:"cache_key,omitempty"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
}

// BuildDependencyGraph materialises the fixture components, cues, per-time
// poses/samples and final window summaries used by the invalidation engine.
func BuildDependencyGraph(p *Project, cfg SamplingConfig) (*DependencyGraph, error) {
	if p == nil {
		return nil, domainError(CodeValue, "nil project", "project")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if len(cfg.DistancesMM) == 0 && cfg.Density == 0 {
		cfg = ExactSampling()
	}
	nodes := make(map[string]DependencyNode)
	add := func(n DependencyNode) {
		n.Upstream = sortedUnique(n.Upstream)
		nodes[n.ID] = n
	}
	for _, f := range p.Fixtures {
		components := []struct {
			kind  string
			value any
		}{
			{"position", f.PositionUM},
			{"cone", [2]int64{f.InnerHalfMicroDeg, f.OuterHalfMicroDeg}},
			{"orientation", []any{f.PanMicroDeg, f.TiltMicroDeg, f.BaseAim}},
			{"color", f.RGBPermyriad},
			{"level", f.LevelPermyriad},
		}
		upstream := make([]string, 0, len(components))
		for _, comp := range components {
			id := fmt.Sprintf("fixture:%s:%s", f.ID, comp.kind)
			hash, err := HashJSON(comp.value)
			if err != nil {
				return nil, err
			}
			add(DependencyNode{ID: id, Kind: comp.kind, FixtureID: f.ID, ContentHash: hash})
			upstream = append(upstream, id)
		}
		add(DependencyNode{
			ID: fmt.Sprintf("fixture:%s:content", f.ID), Kind: "fixture_content", FixtureID: f.ID,
			Upstream: upstream, ContentHash: FixtureContentHash(f),
		})
	}
	for _, c := range p.Cues {
		add(DependencyNode{
			ID: fmt.Sprintf("cue:%s", c.ID), Kind: "cue", FixtureID: c.FixtureID, CueID: c.ID,
			ContentHash: CueContentHash(c),
		})
	}
	windows := append([]SampleWindow(nil), p.Timeline.Windows...)
	sort.Slice(windows, func(i, j int) bool { return windows[i].Number < windows[j].Number })
	for _, w := range windows {
		samples := append([]int64(nil), w.Samples...)
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		windowUpstream := make([]string, 0)
		for _, t := range samples {
			for _, f := range sortedFixtures(p) {
				pose, err := EvaluateFixture(p, f, t)
				if err != nil {
					return nil, err
				}
				poseID := dependencyNodeID(f.ID, w.Number, t) + ":pose"
				upstream := []string{fmt.Sprintf("fixture:%s:content", f.ID)}
				for _, kind := range []CueKind{CueFade, CueSweep, CueColor} {
					if c := pickCue(p.Cues, f.ID, kind, t); c != nil {
						upstream = append(upstream, fmt.Sprintf("cue:%s", c.ID))
					}
				}
				poseHash, err := HashJSON(pose)
				if err != nil {
					return nil, err
				}
				add(DependencyNode{
					ID: poseID, Kind: "pose", FixtureID: f.ID, WindowNumber: w.Number, TimeMS: t,
					Upstream: upstream, ContentHash: poseHash,
				})
				cacheKey := FixtureCacheKey(pose.FixtureHash, t, p.ParameterVersion, pose.UpstreamKeys...)
				sampleID := dependencyNodeID(f.ID, w.Number, t) + ":sample"
				add(DependencyNode{
					ID: sampleID, Kind: "sample", FixtureID: f.ID, WindowNumber: w.Number, TimeMS: t,
					Upstream: []string{poseID}, ContentHash: cacheKey, CacheKey: cacheKey,
				})
				windowUpstream = append(windowUpstream, sampleID)
			}
		}
		summaryID := fmt.Sprintf("window:%d:summary", w.Number)
		aggregateID := fmt.Sprintf("window:%d:aggregates", w.Number)
		add(DependencyNode{ID: summaryID, Kind: "window_summary", WindowNumber: w.Number, Upstream: windowUpstream})
		add(DependencyNode{ID: aggregateID, Kind: "window_aggregates", WindowNumber: w.Number, Upstream: windowUpstream})
	}
	out := make([]DependencyNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &DependencyGraph{Nodes: out}, nil
}

func (g *DependencyGraph) Node(id string) (DependencyNode, bool) {
	if g == nil {
		return DependencyNode{}, false
	}
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return DependencyNode{}, false
}

// Downstream returns all transitive dependants in deterministic order.
func (g *DependencyGraph) Downstream(ids ...string) []string {
	if g == nil {
		return nil
	}
	children := make(map[string][]string)
	for _, n := range g.Nodes {
		for _, up := range n.Upstream {
			children[up] = append(children[up], n.ID)
		}
	}
	seen := make(map[string]struct{})
	queue := append([]string(nil), ids...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range children[id] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type InvalidationReport struct {
	Invalidated []Invalidation `json:"invalidated"`
	Reused      []int          `json:"reused"`
}

// CompareProjects computes a precise edit delta. A static direction or cone
// edit invalidates every window using that fixture; a cue edit invalidates only
// windows whose sample times intersect the old or new cue interval.
func CompareProjects(oldProject, newProject *Project) (InvalidationReport, error) {
	if newProject == nil {
		return InvalidationReport{}, domainError(CodeValue, "nil new project", "project")
	}
	if err := ValidateProject(newProject); err != nil {
		return InvalidationReport{}, err
	}
	if oldProject == nil {
		oldProject = &Project{}
	}
	windows := append([]SampleWindow(nil), newProject.Timeline.Windows...)
	sort.Slice(windows, func(i, j int) bool { return windows[i].Number < windows[j].Number })
	byWindow := make(map[int]*Invalidation)
	ensure := func(number int) *Invalidation {
		inv := byWindow[number]
		if inv == nil {
			inv = &Invalidation{
				WindowNumber: number,
				Downstream: []string{
					fmt.Sprintf("window:%d:summary", number),
					fmt.Sprintf("window:%d:aggregates", number),
				},
			}
			byWindow[number] = inv
		}
		return inv
	}
	allWindows := func(fixtureID, reason string) {
		for _, w := range windows {
			inv := ensure(w.Number)
			inv.FixtureIDs = append(inv.FixtureIDs, fixtureID)
			inv.Reasons = append(inv.Reasons, reason)
		}
	}

	oldFixtures := make(map[string]Fixture)
	for _, f := range oldProject.Fixtures {
		oldFixtures[f.ID] = f
	}
	newFixtures := make(map[string]Fixture)
	for _, f := range newProject.Fixtures {
		newFixtures[f.ID] = f
	}
	for id := range newFixtures {
		if _, ok := oldFixtures[id]; !ok {
			allWindows(id, fmt.Sprintf("fixture %s was added", id))
		}
	}
	for id := range oldFixtures {
		if _, ok := newFixtures[id]; !ok {
			allWindows(id, fmt.Sprintf("fixture %s was removed", id))
		}
	}
	for id, oldF := range oldFixtures {
		newF, ok := newFixtures[id]
		if !ok {
			continue
		}
		if oldF.PositionUM != newF.PositionUM {
			allWindows(id, fmt.Sprintf("fixture %s position changed", id))
		}
		if oldF.PanMicroDeg != newF.PanMicroDeg || oldF.TiltMicroDeg != newF.TiltMicroDeg || !sameAim(oldF.BaseAim, newF.BaseAim) {
			allWindows(id, fmt.Sprintf("fixture %s orientation changed", id))
		}
		if oldF.InnerHalfMicroDeg != newF.InnerHalfMicroDeg || oldF.OuterHalfMicroDeg != newF.OuterHalfMicroDeg {
			allWindows(id, fmt.Sprintf("fixture %s cone changed", id))
		}
		if oldF.LevelPermyriad != newF.LevelPermyriad || oldF.RGBPermyriad != newF.RGBPermyriad || oldF.CCTKelvin != newF.CCTKelvin {
			allWindows(id, fmt.Sprintf("fixture %s color/level changed", id))
		}
	}

	oldCues := make(map[string]Cue)
	for _, c := range oldProject.Cues {
		oldCues[c.ID] = c
	}
	newCues := make(map[string]Cue)
	for _, c := range newProject.Cues {
		newCues[c.ID] = c
	}
	affectedByCue := func(oldCue, newCue *Cue) {
		fixtures := map[string]struct{}{}
		var start, end int64
		first := true
		if oldCue != nil {
			fixtures[oldCue.FixtureID] = struct{}{}
			start, end = oldCue.StartMS, oldCue.EndMS
			first = false
		}
		if newCue != nil {
			fixtures[newCue.FixtureID] = struct{}{}
			if first || newCue.StartMS < start {
				start = newCue.StartMS
			}
			if first || newCue.EndMS > end {
				end = newCue.EndMS
			}
		}
		for _, w := range windows {
			hit := false
			for _, t := range w.Samples {
				if t >= start && t <= end {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			inv := ensure(w.Number)
			for f := range fixtures {
				inv.FixtureIDs = append(inv.FixtureIDs, f)
			}
			if newCue != nil {
				inv.CueIDs = append(inv.CueIDs, newCue.ID)
				inv.Reasons = append(inv.Reasons, fmt.Sprintf("cue %s (%s) changed", newCue.ID, newCue.Kind))
			} else if oldCue != nil {
				inv.CueIDs = append(inv.CueIDs, oldCue.ID)
				inv.Reasons = append(inv.Reasons, fmt.Sprintf("cue %s (%s) removed", oldCue.ID, oldCue.Kind))
			}
		}
	}
	for id, oldCue := range oldCues {
		newCue, ok := newCues[id]
		if !ok {
			c := oldCue
			affectedByCue(&c, nil)
			continue
		}
		if CueContentHash(oldCue) != CueContentHash(newCue) {
			a, b := oldCue, newCue
			affectedByCue(&a, &b)
		}
	}
	for id, newCue := range newCues {
		if _, ok := oldCues[id]; !ok {
			c := newCue
			affectedByCue(nil, &c)
		}
	}

	oldWindows := make(map[int]SampleWindow)
	for _, w := range oldProject.Timeline.Windows {
		oldWindows[w.Number] = w
	}
	newWindows := make(map[int]SampleWindow)
	for _, w := range windows {
		newWindows[w.Number] = w
	}
	for number, oldW := range oldWindows {
		if _, ok := newWindows[number]; !ok {
			ensure(number).Reasons = append(ensure(number).Reasons, "sample window removed")
		} else if !sameInt64s(oldW.Samples, newWindows[number].Samples) ||
			oldW.StartMS != newWindows[number].StartMS || oldW.EndMS != newWindows[number].EndMS {
			ensure(number).Reasons = append(ensure(number).Reasons, "sample window time range changed")
		}
	}
	for number := range newWindows {
		if _, ok := oldWindows[number]; !ok {
			ensure(number).Reasons = append(ensure(number).Reasons, "sample window added")
		}
	}
	if oldProject.ParameterVersion != "" && oldProject.ParameterVersion != newProject.ParameterVersion {
		for _, w := range windows {
			ensure(w.Number).Reasons = append(ensure(w.Number).Reasons, "parameter version changed")
		}
	}
	if oldProject.Timeline.DurationMS != newProject.Timeline.DurationMS {
		for _, w := range windows {
			ensure(w.Number).Reasons = append(ensure(w.Number).Reasons, "timeline duration changed")
		}
	}

	report := InvalidationReport{}
	for _, w := range windows {
		if inv := byWindow[w.Number]; inv != nil && len(inv.Reasons) > 0 {
			inv.FixtureIDs = sortedUnique(inv.FixtureIDs)
			inv.CueIDs = sortedUnique(inv.CueIDs)
			inv.Reasons = sortedUnique(inv.Reasons)
			inv.Downstream = sortedUnique(inv.Downstream)
			report.Invalidated = append(report.Invalidated, *inv)
		} else {
			report.Reused = append(report.Reused, w.Number)
		}
	}
	for number, inv := range byWindow {
		if _, ok := newWindows[number]; !ok && len(inv.Reasons) > 0 {
			inv.Reasons = sortedUnique(inv.Reasons)
			inv.Downstream = sortedUnique(inv.Downstream)
			report.Invalidated = append(report.Invalidated, *inv)
		}
	}
	sort.Slice(report.Invalidated, func(i, j int) bool {
		return report.Invalidated[i].WindowNumber < report.Invalidated[j].WindowNumber
	})
	sort.Ints(report.Reused)
	return report, nil
}

func sameAim(a, b *Vec3) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]int64(nil), a...)
	bb := append([]int64(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i] < aa[j] })
	sort.Slice(bb, func(i, j int) bool { return bb[i] < bb[j] })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// InvalidationReasons returns the window-level reasons as a compact list for
// HTTP and CLI diagnostics.
func InvalidationReasons(report InvalidationReport) map[int]string {
	out := make(map[int]string, len(report.Invalidated))
	for _, inv := range report.Invalidated {
		out[inv.WindowNumber] = strings.Join(inv.Reasons, "; ")
	}
	return out
}
