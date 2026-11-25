package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	SchemaV1 = "battleformat-v1"
	SchemaV2 = "battleformat-v2"
	SchemaV3 = "battleformat-v3"

	TransformRuleVersion = "transform-rules-2026-11-26"
	AlgoVersion          = "sweep-geom-v1.0.0"
	DefaultPolicyVersion = "default-policy-v1"

	InternalScale int64 = 1_000_000

	CodeSchema = "E_SCHEMA"
	CodeOVF    = "E_OVF"
	CodeLossy  = "E_LOSSY"
	CodeDecode = "E_DECODE"
	CodeStep   = "E_STEP"
	CodeUnit   = "E_UNIT"
	CodeBudget = "E_BUDGET"
)

type Error struct {
	Code          string `json:"code"`
	FieldPath     string `json:"field_path"`
	SourceVersion string `json:"source_version,omitempty"`
	Message       string `json:"message"`
	Op1           string `json:"op1,omitempty"`
	Op2           string `json:"op2,omitempty"`
	IntervalIndex int    `json:"interval_index,omitempty"`
}

func (e *Error) Error() string {
	if e.FieldPath == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s (%s): %s", e.Code, e.FieldPath, e.SourceVersion, e.Message)
}

func NewError(code, path, version, format string, args ...any) *Error {
	return &Error{Code: code, FieldPath: path, SourceVersion: version, Message: fmt.Sprintf(format, args...)}
}

type Vec3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

func (v Vec3) Array() [3]int64 { return [3]int64{v.X, v.Y, v.Z} }

type Trace struct {
	SourceVersion string `json:"source_version"`
	FieldPath     string `json:"field_path"`
	Op            string `json:"op"`
	OldValue      string `json:"old_value,omitempty"`
	NewValue      string `json:"new_value,omitempty"`
	Source        string `json:"source,omitempty"`
	RuleVersion   string `json:"rule_version"`
	Detail        string `json:"detail,omitempty"`
}

type LossNotice struct {
	Code          string `json:"code"`
	FieldPath     string `json:"field_path"`
	SourceVersion string `json:"source_version,omitempty"`
	Value         string `json:"value,omitempty"`
	Disposition   string `json:"disposition"`
	RuleVersion   string `json:"rule_version"`
}

type ColliderKind string

const (
	KindAABB    ColliderKind = "aabb"
	KindCapsule ColliderKind = "oriented_capsule"
)

type Collider struct {
	Index int          `json:"index"`
	ID    string       `json:"id"`
	Kind  ColliderKind `json:"kind"`
	Part  string       `json:"part"`

	Min Vec3 `json:"min,omitempty"`
	Max Vec3 `json:"max,omitempty"`

	P0     Vec3  `json:"p0,omitempty"`
	P1     Vec3  `json:"p1,omitempty"`
	Radius int64 `json:"radius_q,omitempty"`

	DeclaredScale int64 `json:"declared_scale,omitempty"`
}

func (c Collider) ContentHash() string {
	b, _ := json.Marshal(struct {
		Kind   ColliderKind
		Part   string
		Min    Vec3
		Max    Vec3
		P0     Vec3
		P1     Vec3
		Radius int64
	}{c.Kind, c.Part, c.Min, c.Max, c.P0, c.P1, c.Radius})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

type RepeatPolicy string

const (
	RepeatOncePerSweep RepeatPolicy = "once_per_collider"
	RepeatAllowed      RepeatPolicy = "allowed"
)

type Sweep struct {
	Index int    `json:"index"`
	ID    string `json:"id"`

	From   Vec3  `json:"from_q"`
	To     Vec3  `json:"to_q"`
	Radius int64 `json:"radius_q"`

	StartFrame int   `json:"start_frame"`
	EndFrame   int   `json:"end_frame"`
	StepMS     int64 `json:"step_ms"`

	DamageQ int64 `json:"damage_q"`

	Penetration  bool         `json:"penetration"`
	DecayQ       int64        `json:"decay_q"`
	MultiHit     bool         `json:"multi_hit"`
	MaxSegments  int          `json:"max_segments"`
	RepeatPolicy RepeatPolicy `json:"repeat_policy"`

	EndpointMode string `json:"endpoint_mode"`
}

func (s Sweep) ContentHash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

type FrameRequest struct {
	Index   int    `json:"index"`
	Frame   int    `json:"frame"`
	SweepID string `json:"sweep_id"`
	AtMS    int64  `json:"at_ms"`
}

type Project struct {
	SchemaVersion string         `json:"schema_version"`
	BattleID      string         `json:"battle_id"`
	StepMS        int64          `json:"step_ms"`
	Colliders     []Collider     `json:"colliders"`
	Sweeps        []Sweep        `json:"sweeps"`
	Requests      []FrameRequest `json:"requests"`
	Traces        []Trace        `json:"traces,omitempty"`
}

func (p *Project) SweepByID(id string) *Sweep {
	for i := range p.Sweeps {
		if p.Sweeps[i].ID == id {
			return &p.Sweeps[i]
		}
	}
	return nil
}

func (p *Project) ColliderByIndex(index int) *Collider {
	for i := range p.Colliders {
		if p.Colliders[i].Index == index {
			return &p.Colliders[i]
		}
	}
	return nil
}

type TruthFile struct {
	Schema        string          `json:"schema"`
	BattleID      string          `json:"battle_id"`
	ToleranceQ    int64           `json:"tolerance_q"`
	GeneratorSeed int64           `json:"generator_seed"`
	Samples       []TruthSample   `json:"samples"`
	Sequences     []TruthSequence `json:"sequences"`
}

type TruthHit struct {
	Segment          int    `json:"segment"`
	Status           string `json:"status"`
	ColliderNo       int    `json:"collider_no"`
	ColliderID       string `json:"collider_id"`
	Part             string `json:"part"`
	Surface          string `json:"surface"`
	RemainingDamageQ int64  `json:"remaining_damage_q"`
	NearestDistanceQ int64  `json:"nearest_distance_q"`
}

type TruthSequence struct {
	SweepID  string     `json:"sweep_id"`
	Segments []TruthHit `json:"segments"`
}

type TruthSample struct {
	Frame            int        `json:"frame"`
	SweepID          string     `json:"sweep_id"`
	Hit              bool       `json:"hit"`
	FirstColliderNo  int        `json:"first_collider_no"`
	FirstPart        string     `json:"first_part"`
	FirstSurface     string     `json:"first_surface"`
	RemainingDamageQ int64      `json:"remaining_damage_q"`
	NearestDistanceQ int64      `json:"nearest_distance_q"`
	ExpectedHits     []TruthHit `json:"expected_hits"`
}
