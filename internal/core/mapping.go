package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

type MappingEntry struct {
	Source            string             `json:"source"`
	Targets           []string           `json:"targets"`
	Strategy          string             `json:"strategy"`
	Reason            string             `json:"reason"`
	Required          bool               `json:"required"`
	SourceBoneLength  float64            `json:"source_bone_length"`
	TargetBoneLengths map[string]float64 `json:"target_bone_lengths,omitempty"`
	Ratio             float64            `json:"ratio"`
}

type Mapping struct {
	ID             string                  `json:"id"`
	SourceID       string                  `json:"source_id"`
	TargetID       string                  `json:"target_id"`
	Entries        []MappingEntry          `json:"entries"`
	BySource       map[string]MappingEntry `json:"by_source"`
	TargetSource   map[string]string       `json:"target_source"`
	UnmappedSource []string                `json:"unmapped_source"`
	DegradedSource []string                `json:"degraded_source"`
	UnmappedTarget []string                `json:"unmapped_target"`
	GlobalScale    float64                 `json:"global_scale"`
	HeightSource   float64                 `json:"height_source"`
	HeightTarget   float64                 `json:"height_target"`
	ScaleRatios    map[string]float64      `json:"scale_ratios"`
	HashValue      string                  `json:"hash"`
}

func CanonicalHash(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func SkeletonHeight(sk Skeleton) float64 {
	pose, err := TPose(sk)
	if err != nil {
		return 0
	}
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range pose.WorldPos {
		minY = math.Min(minY, p[1])
		maxY = math.Max(maxY, p[1])
	}
	return maxY - minY
}

func semanticName(name string) string {
	n := strings.ToLower(name)
	n = strings.ReplaceAll(n, "shoulder", "upper_arm")
	n = strings.ReplaceAll(n, "clavicle", "collar")
	n = strings.ReplaceAll(n, "pelvis", "hips")
	n = strings.ReplaceAll(n, "foot_end", "toe")
	return n
}

func BuildMapping(source, target Skeleton, params Parameters) (*Mapping, *Error) {
	si, e := NewSkeletonIndex(source)
	if e != nil {
		return nil, e
	}
	ti, e := NewSkeletonIndex(target)
	if e != nil {
		return nil, e
	}
	m := &Mapping{
		SourceID: source.ID, TargetID: target.ID,
		BySource: map[string]MappingEntry{}, TargetSource: map[string]string{},
		ScaleRatios: map[string]float64{},
	}
	m.HeightSource = SkeletonHeight(source)
	m.HeightTarget = SkeletonHeight(target)
	m.GlobalScale = m.HeightTarget / m.HeightSource * params.GlobalScaleMultiplier
	heightValid := m.HeightSource > 0 && m.HeightTarget > 0

	targetBySemantic := map[string]string{}
	for _, j := range target.Joints {
		targetBySemantic[semanticName(j.Name)] = j.Name
	}

	required := map[string]bool{"hips": true, "foot_l": true, "foot_r": true}
	for _, sj := range source.Joints {
		entry := MappingEntry{
			Source:            sj.Name,
			Required:          required[sj.Name],
			SourceBoneLength:  sj.Offset.Norm(),
			TargetBoneLengths: map[string]float64{},
		}
		if override, ok := params.MappingOverrides[sj.Name]; ok {
			if strings.TrimSpace(override) == "" || override == "@degrade" {
				entry.Strategy = "degrade_identity"
				entry.Reason = "explicit override: missing target, identity fallback"
			} else {
				names := strings.Split(override, ",")
				for _, name := range names {
					name = strings.TrimSpace(name)
					if _, ok := ti.ByName[name]; !ok {
						return nil, &Error{Code: ECodeMap, Joint: sj.Name, Field: "mapping_overrides", Message: "override target does not exist: " + name}
					}
					entry.Targets = append(entry.Targets, name)
				}
				entry.Strategy = "override"
				entry.Reason = "user adjusted mapping"
			}
		} else if sj.Name == "spine" {
			entry.Targets = []string{"spine"}
			if _, ok := ti.ByName["spine2"]; ok {
				entry.Targets = append(entry.Targets, "spine2")
				entry.Strategy = "fanout"
				entry.Reason = "one source spine drives primary and extra target spine segment"
			} else {
				entry.Strategy = "exact"
				entry.Reason = "exact semantic name"
			}
		} else if tn, ok := targetBySemantic[semanticName(sj.Name)]; ok {
			entry.Targets = []string{tn}
			entry.Strategy = "exact"
			entry.Reason = "exact semantic name"
		} else {
			entry.Strategy = "degrade_identity"
			entry.Reason = "target has no corresponding joint; explicit identity degradation"
		}

		for _, tn := range entry.Targets {
			tj := target.Joints[ti.ByName[tn]]
			l := tj.Offset.Norm()
			entry.TargetBoneLengths[tn] = l
			if entry.SourceBoneLength > 1e-12 {
				m.ScaleRatios[sj.Name+"->"+tn] = l / entry.SourceBoneLength
			}
			if entry.Ratio == 0 {
				entry.Ratio = l / entry.SourceBoneLength
			}
			if old, exists := m.TargetSource[tn]; exists && old != sj.Name {
				return nil, &Error{Code: ECodeMap, Joint: sj.Name, Message: fmt.Sprintf("target %s mapped from multiple sources %s and %s", tn, old, sj.Name)}
			}
			m.TargetSource[tn] = sj.Name
		}
		if len(entry.Targets) == 0 {
			m.UnmappedSource = append(m.UnmappedSource, sj.Name)
			m.DegradedSource = append(m.DegradedSource, sj.Name)
		}
		m.Entries = append(m.Entries, entry)
		m.BySource[sj.Name] = entry
	}

	mappedTarget := map[string]bool{}
	for _, e := range m.Entries {
		for _, t := range e.Targets {
			mappedTarget[t] = true
		}
	}
	for _, tj := range target.Joints {
		if !mappedTarget[tj.Name] {
			m.UnmappedTarget = append(m.UnmappedTarget, tj.Name)
			// Extra target segments inherit parent/source motion during retarget.
			m.TargetSource[tj.Name] = ""
		}
	}

	var impossible []string
	for _, name := range []string{"hips", "foot_l", "foot_r"} {
		if _, ok := si.ByName[name]; ok {
			if entry, ok := m.BySource[name]; !ok || len(entry.Targets) == 0 {
				impossible = append(impossible, name)
			}
		}
	}
	if len(impossible) > 0 {
		return nil, &Error{Code: ECodeMap, Message: "no feasible skeleton mapping", Details: impossible}
	}
	if !heightValid {
		return nil, NewError(ECodeSchema, "skeleton T-pose height is not positive")
	}

	sort.Strings(m.UnmappedSource)
	sort.Strings(m.DegradedSource)
	sort.Strings(m.UnmappedTarget)
	m.ID = m.SourceID + "__to__" + m.TargetID
	m.HashValue = CanonicalHash(struct {
		Entries     []MappingEntry    `json:"entries"`
		GlobalScale float64           `json:"global_scale"`
		Overrides   map[string]string `json:"overrides"`
	}{m.Entries, m.GlobalScale, params.MappingOverrides})
	return m, nil
}

func (m *Mapping) QuerySource(sourceJoint string) (MappingEntry, bool) {
	e, ok := m.BySource[sourceJoint]
	return e, ok
}

func (m *Mapping) QueryTarget(targetJoint string) (MappingEntry, bool) {
	src, ok := m.TargetSource[targetJoint]
	if !ok || src == "" {
		return MappingEntry{}, false
	}
	e, ok := m.BySource[src]
	return e, ok
}

func (m *Mapping) EffectiveScale(params Parameters, sourceJoint string) float64 {
	s := m.GlobalScale
	if factor, ok := params.ScaleOverrides[sourceJoint]; ok && FiniteFloat(factor) && factor > 0 {
		s *= factor
	}
	return s
}
