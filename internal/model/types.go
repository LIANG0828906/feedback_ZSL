package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"retargetlab/internal/geom"
)

type Joint struct {
	Name             string    `json:"name"`
	Parent           int       `json:"parent"`
	LocalOffset      geom.Vec3 `json:"localOffset"`
	RotationLimitDeg float64   `json:"rotationLimitDeg,omitempty"`
}

type Skeleton struct {
	Name   string  `json:"name"`
	Joints []Joint `json:"joints"`
}

func (s Skeleton) JointIndex(name string) int {
	for i := range s.Joints {
		if s.Joints[i].Name == name {
			return i
		}
	}
	return -1
}

func (s Skeleton) JointNames() []string {
	out := make([]string, len(s.Joints))
	for i := range s.Joints {
		out[i] = s.Joints[i].Name
	}
	return out
}

func (s Skeleton) Height() float64 {
	maxY := 0.0
	minY := 0.0
	for _, j := range s.Joints {
		if j.LocalOffset.Y > maxY {
			maxY = j.LocalOffset.Y
		}
		if j.LocalOffset.Y < minY {
			minY = j.LocalOffset.Y
		}
	}
	// Offsets form chains; accumulate root-to-every-joint to obtain a stable height.
	for i := range s.Joints {
		y := 0.0
		for cur, guard := i, 0; cur >= 0 && guard < len(s.Joints)+1; guard++ {
			y += s.Joints[cur].LocalOffset.Y
			cur = s.Joints[cur].Parent
		}
		if y > maxY {
			maxY = y
		}
		if y < minY {
			minY = y
		}
	}
	return maxY - minY
}

func (s Skeleton) CanonicalJSON() ([]byte, error) {
	return json.Marshal(s)
}

func (s Skeleton) Hash() string {
	b, err := s.CanonicalJSON()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func CombinedSkeletonHash(source, target Skeleton) string {
	return StableHash(struct {
		Source string `json:"source"`
		Target string `json:"target"`
	}{
		Source: source.Hash(),
		Target: target.Hash(),
	})
}

type Frame struct {
	Index           int         `json:"index"`
	RootTranslation geom.Vec3   `json:"rootTranslation"`
	LocalRotations  []geom.Quat `json:"localRotations"`
}

type ContactInterval struct {
	Foot  string `json:"foot"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

func (c ContactInterval) Contains(frame int) bool {
	return frame >= c.Start && frame <= c.End
}

type Phase struct {
	Index      int    `json:"index"`
	StartFrame int    `json:"startFrame"`
	EndFrame   int    `json:"endFrame"`
	Name       string `json:"name"`
}

type Clip struct {
	Name     string            `json:"name"`
	Frames   []Frame           `json:"frames"`
	Contacts []ContactInterval `json:"contacts"`
	Phases   []Phase           `json:"phases"`
}

func (c Clip) PhaseForFrame(frame int) int {
	for _, p := range c.Phases {
		if frame >= p.StartFrame && frame <= p.EndFrame {
			return p.Index
		}
	}
	if len(c.Phases) > 0 {
		return c.Phases[len(c.Phases)-1].Index
	}
	return 0
}

func (c Clip) ContactFeet(frame int) []string {
	out := make([]string, 0, 2)
	for _, ci := range c.Contacts {
		if ci.Contains(frame) {
			out = append(out, ci.Foot)
		}
	}
	sort.Strings(out)
	return out
}

type JointRule struct {
	Source string `json:"source"`
	// Targets supports both one-to-one (one target) and one-to-many (multiple targets).
	Targets []string `json:"targets"`
	Weight  float64  `json:"weight,omitempty"`
	Phases  []int    `json:"phases,omitempty"`
}

type MissingPolicy string

const (
	MissingIdentity              MissingPolicy = "identity"
	MissingInheritParent         MissingPolicy = "inherit_parent"
	MissingNearestMappedAncestor MissingPolicy = "nearest_mapped_ancestor"
)

type MissingRule struct {
	Joint          string        `json:"joint"`
	Policy         MissingPolicy `json:"policy"`
	SourceAncestor string        `json:"sourceAncestor,omitempty"`
	Phases         []int         `json:"phases,omitempty"`
}

type EffectorRule struct {
	Target        string  `json:"target"`
	Source        string  `json:"source"`
	Tolerance     float64 `json:"tolerance"`
	MaxIterations int     `json:"maxIterations"`
	Phases        []int   `json:"phases,omitempty"`
}

type ScaleCoefficient struct {
	Key    string  `json:"key"`
	Value  float64 `json:"value"`
	Joint  string  `json:"joint,omitempty"`
	Phases []int   `json:"phases,omitempty"`
}

type Mapping struct {
	Name          string             `json:"name"`
	Rules         []JointRule        `json:"rules"`
	Missing       []MissingRule      `json:"missing"`
	Effectors     []EffectorRule     `json:"effectors"`
	ScaleDefaults []ScaleCoefficient `json:"scaleDefaults"`
	PhaseSize     int                `json:"phaseSize"`
}

func (m Mapping) CanonicalJSON() ([]byte, error) {
	return json.Marshal(m)
}

func (m Mapping) Hash() string {
	b, err := m.CanonicalJSON()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m Mapping) Clone() Mapping {
	out := m
	out.Rules = make([]JointRule, len(m.Rules))
	for i := range m.Rules {
		out.Rules[i] = m.Rules[i]
		out.Rules[i].Targets = append([]string(nil), m.Rules[i].Targets...)
		out.Rules[i].Phases = append([]int(nil), m.Rules[i].Phases...)
	}
	out.Missing = append([]MissingRule(nil), m.Missing...)
	for i := range out.Missing {
		out.Missing[i].Phases = append([]int(nil), m.Missing[i].Phases...)
	}
	out.Effectors = append([]EffectorRule(nil), m.Effectors...)
	for i := range out.Effectors {
		out.Effectors[i].Phases = append([]int(nil), m.Effectors[i].Phases...)
	}
	out.ScaleDefaults = append([]ScaleCoefficient(nil), m.ScaleDefaults...)
	for i := range out.ScaleDefaults {
		out.ScaleDefaults[i].Phases = append([]int(nil), out.ScaleDefaults[i].Phases...)
	}
	return out
}

type TargetVariant struct {
	Name     string   `json:"name"`
	Skeleton Skeleton `json:"skeleton"`
	Scale    float64  `json:"scale"`
}

type TruthFrame struct {
	Frame    int          `json:"frame"`
	World    []TruthJoint `json:"world"`
	Root     geom.Vec3    `json:"root"`
	Contacts []string     `json:"contacts"`
}

type TruthJoint struct {
	Joint    string    `json:"joint"`
	Position geom.Vec3 `json:"position"`
}

type TargetTruth struct {
	Target            string             `json:"target"`
	Tolerance         float64            `json:"tolerance"`
	KeyFrames         []TruthFrame       `json:"keyFrames"`
	SourceBoneLengths map[string]float64 `json:"sourceBoneLengths"`
	BoneLengths       map[string]float64 `json:"boneLengths"`
	Contacts          []ContactInterval  `json:"contacts"`
}

type Workspace struct {
	Seed    int64           `json:"seed"`
	Source  Skeleton        `json:"source"`
	Clip    Clip            `json:"clip"`
	Targets []TargetVariant `json:"targets"`
	Mapping Mapping         `json:"mapping"`
	Truths  []TargetTruth   `json:"truths"`
}

func (w Workspace) TargetByName(name string) (TargetVariant, bool) {
	for _, t := range w.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return TargetVariant{}, false
}

func (w Workspace) TruthByTarget(name string) (TargetTruth, bool) {
	for _, t := range w.Truths {
		if t.Target == name {
			return t, true
		}
	}
	return TargetTruth{}, false
}

func StableHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
