package bonerig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	FormatV1 = "rig-flat-v1"
	FormatV2 = "rig-flat-v2"
	FormatV3 = "rig-tree-v3"

	AlgorithmVersion = "retarget-int-v1"
	RuleVersion      = "schema-rules-v1"
	BoundRuleVersion = "ub-int-v1"
)

// Quat is a fixed-point quaternion. Value = [W,X,Y,Z] / Scale.
type Quat struct {
	W     int64 `json:"w"`
	X     int64 `json:"x"`
	Y     int64 `json:"y"`
	Z     int64 `json:"z"`
	Scale int64 `json:"scale"`
}

// Vec3 is a fixed-point displacement. Value = [X,Y,Z] / Scale.
type Vec3 struct {
	X     int64 `json:"x"`
	Y     int64 `json:"y"`
	Z     int64 `json:"z"`
	Scale int64 `json:"scale"`
}

// Constraint uses signed angle in micro-radians (1e-6 rad).
type Constraint struct {
	Enabled          bool   `json:"enabled"`
	AxisX            int64  `json:"axis_x"`
	AxisY            int64  `json:"axis_y"`
	AxisZ            int64  `json:"axis_z"`
	MinAngleMicroRad int64  `json:"min_angle_micro_rad"`
	MaxAngleMicroRad int64  `json:"max_angle_micro_rad"`
	RuleVersion      string `json:"rule_version,omitempty"`
	DefaultFilled    bool   `json:"default_filled,omitempty"`
}

// Joint is the unified flat joint record. ParentID empty denotes a root.
type Joint struct {
	ID              string      `json:"id"`
	ParentID        string      `json:"parent_id,omitempty"`
	RestTranslation Vec3        `json:"rest_translation"`
	BindRotation    Quat        `json:"bind_rotation"`
	Constraint      *Constraint `json:"constraint,omitempty"`
}

// Skeleton is a flat, parent-referenced unified skeleton.
type Skeleton struct {
	ID      string  `json:"id"`
	Version string  `json:"version"`
	Joints  []Joint `json:"joints"`
}

func (s Skeleton) JointIDs() []string {
	out := make([]string, 0, len(s.Joints))
	for _, j := range s.Joints {
		out = append(out, j.ID)
	}
	return out
}

func (s Skeleton) Index() map[string]int {
	out := make(map[string]int, len(s.Joints))
	for i, j := range s.Joints {
		out[j.ID] = i
	}
	return out
}

// JointPose contains the local animated transform for one joint/frame.
type JointPose struct {
	Rotation    Quat  `json:"rotation"`
	Translation *Vec3 `json:"translation,omitempty"`
}

// Frame is time-normalized but retains source tick/millisecond evidence.
type Frame struct {
	Index           int                  `json:"index"`
	Tick            int64                `json:"tick"`
	TimeMS          int64                `json:"time_ms"`
	RootTranslation Vec3                 `json:"root_translation"`
	Poses           map[string]JointPose `json:"poses"`
}

// Clip is the unified action clip.
type Clip struct {
	ID       string  `json:"id"`
	Frames   []Frame `json:"frames"`
	TickRate int64   `json:"tick_rate"`
}

// WeightedJoint is one source influence in target-world redistribution.
type WeightedJoint struct {
	SourceJoint string `json:"source_joint"`
	Weight      int64  `json:"weight"`
}

// MapItem maps one target joint to one or more source influences.
type MapItem struct {
	TargetJoint        string          `json:"target_joint"`
	SourceJoint        string          `json:"source_joint"`
	ExtraSources       []WeightedJoint `json:"extra_sources,omitempty"`
	WeightScale        int64           `json:"weight_scale"`
	RotScaleNum        int64           `json:"rot_scale_num"`
	RotScaleDen        int64           `json:"rot_scale_den"`
	TransScaleNum      int64           `json:"trans_scale_num"`
	TransScaleDen      int64           `json:"trans_scale_den"`
	ScaleDefaultFilled bool            `json:"scale_default_filled,omitempty"`
}

// Mapping is the complete source-to-target redistribution table.
type Mapping struct {
	Version string    `json:"version"`
	Items   []MapItem `json:"items"`
}

// BoneProportion gives declared rest lengths in DistanceScale units.
type BoneProportion struct {
	SourceLength int64 `json:"source_length"`
	TargetLength int64 `json:"target_length"`
}

// ProportionSet contains target skeleton proportion declarations.
type ProportionSet struct {
	Version       string                    `json:"version"`
	DistanceScale int64                     `json:"distance_scale"`
	GlobalNum     int64                     `json:"global_num"`
	GlobalDen     int64                     `json:"global_den"`
	Bones         map[string]BoneProportion `json:"bones"`
}

// BoundParams declares every structural coefficient used by UB formulas.
type BoundParams struct {
	RuleVersion           string `json:"rule_version"`
	Components            int64  `json:"components"`
	L1Gain                int64  `json:"l1_gain"`
	RoundingUnitCeil      int64  `json:"rounding_unit_ceil"`
	MapRoundingUnitCeil   int64  `json:"map_rounding_unit_ceil"`
	ScaleRoundingUnitCeil int64  `json:"scale_rounding_unit_ceil"`
}

// PrecisionParams contains declared scales, tolerances and bound constants.
type PrecisionParams struct {
	Version            string      `json:"version"`
	WorkScale          int64       `json:"work_scale"`
	DistanceScale      int64       `json:"distance_scale"`
	NormToleranceUnits int64       `json:"norm_tolerance_units"`
	MaxComponent       int64       `json:"max_component"`
	IntervalFrames     int         `json:"interval_frames"`
	Bound              BoundParams `json:"bound"`
}

// DefaultPrecision returns the deterministic declared precision profile.
func DefaultPrecision() PrecisionParams {
	const workScale int64 = 655_360_000 // LCM(10000, 32768, 1048576)
	return PrecisionParams{
		Version:            "prec-seed42-v1",
		WorkScale:          workScale,
		DistanceScale:      1_000_000,
		NormToleranceUnits: 4_096,
		MaxComponent:       1 << 40,
		IntervalFrames:     4,
		Bound: BoundParams{
			RuleVersion:           BoundRuleVersion,
			Components:            4,
			L1Gain:                2,
			RoundingUnitCeil:      1,
			MapRoundingUnitCeil:   1,
			ScaleRoundingUnitCeil: 1,
		},
	}
}

// FrameTruth is generator-produced acceptance truth for one frame.
type FrameTruth struct {
	Index           int             `json:"index"`
	TargetRotations map[string]Quat `json:"target_rotations"`
	TargetTrans     map[string]Vec3 `json:"target_translations"`
}

// TruthManifest is the deterministic truth checklist.
type TruthManifest struct {
	Seed      int64        `json:"seed"`
	Frames    []FrameTruth `json:"frames"`
	Algorithm string       `json:"algorithm"`
	Tolerance int64        `json:"tolerance_l1_units"`
}

// Asset is the unified internal model plus provenance.
type Asset struct {
	Seed            int64            `json:"seed"`
	SourceFormat    string           `json:"source_format"`
	SourceVersion   string           `json:"source_version"`
	SchemaVersion   string           `json:"schema_version"`
	Skeleton        Skeleton         `json:"skeleton"`
	TargetSkeleton  Skeleton         `json:"target_skeleton"`
	Clip            Clip             `json:"clip"`
	Mapping         Mapping          `json:"mapping"`
	Proportions     ProportionSet    `json:"proportions"`
	Precision       PrecisionParams  `json:"precision"`
	Truth           TruthManifest    `json:"truth"`
	Traces          []TransformTrace `json:"traces,omitempty"`
	NormTraces      []QuatNormTrace  `json:"-"`
	ContentHash     string           `json:"content_hash"`
	TopologyVersion string           `json:"topology_version"`
}

// CanonicalHash returns a stable SHA-256 hash of JSON-ish content.
func CanonicalHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SortedKeys is a small helper for deterministic reports.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
