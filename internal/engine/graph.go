package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"combatjudge/internal/geom"
	"combatjudge/internal/model"
)

type EditSpec struct {
	Kind       string `json:"kind"`
	ColliderNo int    `json:"collider_no,omitempty"`
	SweepID    string `json:"sweep_id,omitempty"`
	Field      string `json:"field,omitempty"`
}

func TimebaseVersion(project *model.Project) string {
	return fmt.Sprintf("timebase-int-v1:frame1=0ms:step_ms=%d", project.StepMS)
}

func ParamsVersion(project *model.Project) string {
	h := sha256.New()
	for _, s := range project.Sweeps {
		h.Write([]byte(s.ContentHash()))
	}
	return "params-v1:" + hex.EncodeToString(h.Sum(nil))[:16]
}

func ProjectIdentity(project *model.Project) string {
	h := sha256.New()
	fmt.Fprintf(h, "battle=%s schema=%s step=%d\n", project.BattleID, project.SchemaVersion, project.StepMS)
	for _, c := range project.Colliders {
		fmt.Fprintf(h, "collider=%d:%s\n", c.Index, c.ContentHash())
	}
	for _, s := range project.Sweeps {
		fmt.Fprintf(h, "sweep=%s:%s\n", s.ID, s.ContentHash())
	}
	return "project-v1:" + hex.EncodeToString(h.Sum(nil))[:24]
}

func intervalCandidateColliders(project *model.Project, spec IntervalSpec) map[int]model.Collider {
	out := map[int]model.Collider{}
	for _, req := range requestsInInterval(project, spec) {
		sweep := project.SweepByID(req.SweepID)
		if sweep == nil {
			continue
		}
		cur, _, err := positionAt(*sweep, req.Frame, project.SchemaVersion)
		if err != nil {
			continue
		}
		prevFrame := max(sweep.StartFrame, req.Frame-1)
		prev, _, err := positionAt(*sweep, prevFrame, project.SchemaVersion)
		if err != nil {
			continue
		}
		seg := geom.SegmentFromModel(prev, cur)
		for _, c := range project.Colliders {
			if broadphaseCandidate(seg, sweep.Radius, c) {
				out[c.Index] = c
			}
		}
	}
	return out
}

func BuildDependencies(project *model.Project, specs []IntervalSpec) map[int][]Dependency {
	out := map[int][]Dependency{}
	for _, spec := range specs {
		deps := []Dependency{{IntervalNo: spec.No, Kind: "timebase", Ref: "frame_ms", Hash: TimebaseVersion(project), Reason: "frame_to_ms integer time base affects at_ms and checkpoint version"}}
		sweepSeen := map[string]bool{}
		for _, req := range requestsInInterval(project, spec) {
			sweep := project.SweepByID(req.SweepID)
			if sweep == nil || sweepSeen[sweep.ID] {
				continue
			}
			sweepSeen[sweep.ID] = true
			deps = append(deps, Dependency{IntervalNo: spec.No, Kind: "sweep", Ref: sweep.ID, Hash: sweep.ContentHash(), Reason: "sweep endpoints/radius/frame range/damage/penetration feed intersection and settlement"})
		}
		candidates := intervalCandidateColliders(project, spec)
		for no := range candidates {
			c := candidates[no]
			deps = append(deps, Dependency{IntervalNo: spec.No, Kind: "collider", Ref: fmt.Sprintf("collider:%d", c.Index), Hash: c.ContentHash(), Reason: "collider dimensions/pose/radius feed swept intersection and hit part"})
		}
		deps = append(deps, Dependency{IntervalNo: spec.No, Kind: "settlement", Ref: "penetration_chain", Reason: "upstream remaining damage, segment count and hit-collider set feed settlement"})
		sort.Slice(deps, func(i, j int) bool {
			if deps[i].Kind != deps[j].Kind {
				return deps[i].Kind < deps[j].Kind
			}
			return deps[i].Ref < deps[j].Ref
		})
		out[spec.No] = deps
	}
	return out
}

type cacheKeyInput struct {
	AlgorithmVersion string
	TimebaseVersion  string
	ParamsVersion    string
	Interval         IntervalSpec
	Dependencies     []Dependency
	UpstreamKey      string
}

func MakeCacheKey(project *model.Project, spec IntervalSpec, deps []Dependency, upstreamKey string) (cacheKey, contentHash string) {
	canonicalDeps := append([]Dependency{}, deps...)
	for i := range canonicalDeps {
		canonicalDeps[i].IntervalNo = 0
	}
	b, _ := json.Marshal(cacheKeyInput{
		AlgorithmVersion: model.AlgoVersion,
		TimebaseVersion:  TimebaseVersion(project),
		ParamsVersion:    "cache-params-v1",
		Interval:         spec,
		Dependencies:     canonicalDeps,
		UpstreamKey:      upstreamKey,
	})
	sum := sha256.Sum256(b)
	cacheKey = "ik-" + hex.EncodeToString(sum[:16])

	h := sha256.New()
	for _, d := range deps {
		h.Write([]byte(d.Kind + ":" + d.Ref + ":" + d.Hash + "\n"))
	}
	contentHash = "ch-" + hex.EncodeToString(h.Sum(nil))[:16]
	return cacheKey, contentHash
}

func BuildKeys(project *model.Project, specs []IntervalSpec, graph map[int][]Dependency) map[int]string {
	keys := map[int]string{}
	upstream := GenesisKey
	for _, spec := range specs {
		key, _ := MakeCacheKey(project, spec, graph[spec.No], upstream)
		keys[spec.No] = key
		upstream = key
	}
	return keys
}

const GenesisKey = "genesis:" + model.AlgoVersion

func PlanEdit(oldProject, newProject *model.Project, intervalSize int, edit EditSpec) []Invalidation {
	oldSpecs := BuildIntervals(oldProject, intervalSize)
	newSpecs := BuildIntervals(newProject, intervalSize)
	oldGraph := BuildDependencies(oldProject, oldSpecs)
	newGraph := BuildDependencies(newProject, newSpecs)
	affectedGeometry := map[int]string{}

	hasDep := func(graph map[int][]Dependency, no int, kind, ref string) bool {
		for _, d := range graph[no] {
			if d.Kind == kind && d.Ref == ref {
				return true
			}
		}
		return false
	}

	maxNo := max(len(oldSpecs), len(newSpecs))
	for no := 1; no <= maxNo; no++ {
		reason := ""
		switch edit.Kind {
		case "collider_size":
			ref := fmt.Sprintf("collider:%d", edit.ColliderNo)
			if hasDep(oldGraph, no, "collider", ref) || hasDep(newGraph, no, "collider", ref) {
				reason = fmt.Sprintf("collider_size_changed collider_no=%d field=%s", edit.ColliderNo, edit.Field)
			}
		case "sweep_endpoint", "sweep_duration":
			if hasDep(oldGraph, no, "sweep", edit.SweepID) || hasDep(newGraph, no, "sweep", edit.SweepID) {
				reason = fmt.Sprintf("%s sweep_id=%s field=%s", edit.Kind, edit.SweepID, edit.Field)
			}
		case "time_step":
			reason = "time_step_changed_and_timebase_version_changed"
		}
		if reason != "" {
			affectedGeometry[no] = reason
		}
	}

	out := []Invalidation{}
	earliest := maxNo + 1
	for no, reason := range affectedGeometry {
		out = append(out, Invalidation{IntervalNo: no, Kind: "intersection", Reason: reason})
		earliest = min(earliest, no)
	}
	if edit.Kind != "time_step" {
		for no := earliest + 1; no <= maxNo; no++ {
			if _, direct := affectedGeometry[no]; direct {
				continue
			}
			out = append(out, Invalidation{IntervalNo: no, Kind: "downstream_settlement", Reason: fmt.Sprintf("upstream_cache_key_changed_after_interval_%d", earliest)})
		}
	}
	sortInvalidations(out)
	return out
}

func sortInvalidations(in []Invalidation) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].IntervalNo > in[j].IntervalNo; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}
