package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
)

const (
	InternalQuatScale = int64(1_000_000_000)
	InternalPosScale  = int64(1_000_000_000)
	AngleScale        = int64(1_000_000)
)

// RoundMode is explicit so every integer division can record how its
// remainder was resolved.
type RoundMode string

const (
	RoundHalfUp RoundMode = "half_up"
	RoundFloor  RoundMode = "floor"
	RoundCeil   RoundMode = "ceil"
	RoundToward RoundMode = "toward_zero"
)

type Quat struct {
	W, X, Y, Z int64
	Scale      int64
}

func (q Quat) Components() [4]int64 { return [4]int64{q.W, q.X, q.Y, q.Z} }

type Vec3 struct {
	X, Y, Z int64
}

type FixedVec3 struct {
	X, Y, Z int64
	Scale   int64
}

func (v FixedVec3) Components() [3]int64 { return [3]int64{v.X, v.Y, v.Z} }

type Constraint struct {
	MinRad int64
	MaxRad int64
	Scale  int64
}

type Joint struct {
	ID         string
	Parent     string
	Rest       FixedVec3
	Constraint Constraint
}

type Skeleton struct {
	Joints []Joint
}

func (s Skeleton) Index() map[string]int {
	m := make(map[string]int, len(s.Joints))
	for i, joint := range s.Joints {
		m[joint.ID] = i
	}
	return m
}

type Frame struct {
	Index     int
	TimeMS    int64
	Root      FixedVec3
	Rotations []Quat // same order as Skeleton.Joints
}

type Clip struct {
	FPS    int64
	Frames []Frame
}

type Ratio struct {
	Num int64
	Den int64
}

func (r Ratio) Valid() bool { return r.Den > 0 }

func (r Ratio) String() string {
	if !r.Valid() {
		return "invalid"
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

type TimeBase struct {
	Kind  string
	FPS   int64
	Round RoundMode
}

type Mapping struct {
	ID               string
	Source           string
	Target           string
	RotationScale    Ratio
	TranslationScale Ratio
}

type RetargetSpec struct {
	Mappings []Mapping
}

type TargetJoint struct {
	ID         string
	Parent     string
	Rest       FixedVec3
	Proportion Ratio
}

type TargetSkeleton struct {
	Joints []TargetJoint
}

func (s TargetSkeleton) Index() map[string]int {
	m := make(map[string]int, len(s.Joints))
	for i, joint := range s.Joints {
		m[joint.ID] = i
	}
	return m
}

type TruthSample struct {
	Frame    int
	Joint    string
	Rotation Quat
	Position FixedVec3
}

type Tolerance struct {
	NormPPM      int64
	RoundTripPPM int64
	RotationPPM  int64
}

type ConversionRecord struct {
	Path      string
	Kind      string
	From      int64
	To        int64
	Remainder int64
	Divisor   int64
	Round     RoundMode
}

type Document struct {
	Format        string
	Version       string
	SourceVersion string
	TimeBase      TimeBase
	Skeleton      Skeleton
	Clip          Clip
	Retarget      RetargetSpec
	Target        TargetSkeleton
	Truth         []TruthSample
	Tolerance     Tolerance
	Conversions   []ConversionRecord
}

func (d *Document) FrameIndexOf(frame int) int {
	for i := range d.Clip.Frames {
		if d.Clip.Frames[i].Index == frame {
			return i
		}
	}
	return -1
}

func (d *Document) TruthFor(frame int, joint string) (TruthSample, bool) {
	for _, sample := range d.Truth {
		if sample.Frame == frame && sample.Joint == joint {
			return sample, true
		}
	}
	return TruthSample{}, false
}

// ContentHash is the action-content identity. Topology, mapping, tolerance and
// source-format spelling are deliberately excluded so those edits can be
// propagated without pretending the clip itself changed.
func (d *Document) ContentHash() string {
	const hashScale = int64(1_000_000)
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	write("action", strconv.FormatInt(d.Clip.FPS, 10))
	for _, joint := range d.Skeleton.Joints {
		write("joint", joint.ID)
	}
	for _, frame := range d.Clip.Frames {
		write("frame", strconv.Itoa(frame.Index), strconv.FormatInt(frame.TimeMS, 10),
			strconv.FormatInt(rescaleRound(frame.Root.X, frame.Root.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(frame.Root.Y, frame.Root.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(frame.Root.Z, frame.Root.Scale, hashScale), 10))
		for _, q := range frame.Rotations {
			write("q",
				strconv.FormatInt(rescaleRound(q.W, q.Scale, hashScale), 10),
				strconv.FormatInt(rescaleRound(q.X, q.Scale, hashScale), 10),
				strconv.FormatInt(rescaleRound(q.Y, q.Scale, hashScale), 10),
				strconv.FormatInt(rescaleRound(q.Z, q.Scale, hashScale), 10))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (d *Document) TopologyHash() string {
	const hashScale = int64(1_000_000)
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	for _, joint := range d.Skeleton.Joints {
		write("source", joint.ID, joint.Parent,
			strconv.FormatInt(rescaleRound(joint.Rest.X, joint.Rest.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(joint.Rest.Y, joint.Rest.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(joint.Rest.Z, joint.Rest.Scale, hashScale), 10))
	}
	for _, joint := range d.Target.Joints {
		write("target", joint.ID, joint.Parent,
			strconv.FormatInt(rescaleRound(joint.Rest.X, joint.Rest.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(joint.Rest.Y, joint.Rest.Scale, hashScale), 10),
			strconv.FormatInt(rescaleRound(joint.Rest.Z, joint.Rest.Scale, hashScale), 10),
			strconv.FormatInt(joint.Proportion.Num, 10),
			strconv.FormatInt(joint.Proportion.Den, 10))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (d *Document) MappingHash() string {
	h := sha256.New()
	for _, mapping := range d.Retarget.Mappings {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d/%d\x00%d/%d\x00",
			mapping.ID, mapping.Source, mapping.Target,
			mapping.RotationScale.Num, mapping.RotationScale.Den,
			mapping.TranslationScale.Num, mapping.TranslationScale.Den)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rescaleRound(value, from, to int64) int64 {
	if from <= 0 || from == to {
		return value
	}
	if value >= 0 {
		return (value*to + from/2) / from
	}
	return -((-value*to + from/2) / from)
}

// SortedIntervals is useful for deterministic diagnostics and tests.
func SortedIntervals(intervals []int) []int {
	out := append([]int(nil), intervals...)
	sort.Ints(out)
	return out
}
