// Package scene contains the canonical in-memory scene model and the versioned
// scene exchange loader/exporter.
package scene

import (
	"math"
	"sort"
)

const (
	Version1       = "1.0"
	Version2       = "2.0"
	Version3       = "3.0"
	WorkingVersion = Version3

	DefaultShadowSoftness = 0.08
	DefaultToneMapping    = "linear"
	LuminousEfficacy      = 683.0 // lm/W, the fixed v3->canonical conversion.
)

// SupportedVersions is ordered from oldest to newest.
var SupportedVersions = []string{Version1, Version2, Version3}

// Vec3 is a canonical Cartesian vector.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3 { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3 { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 {
	return Vec3{v.X * s, v.Y * s, v.Z * s}
}
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) LengthSquared() float64 { return v.Dot(v) }
func (v Vec3) Length() float64        { return math.Sqrt(v.LengthSquared()) }
func (v Vec3) IsFinite() bool {
	return isFinite(v.X) && isFinite(v.Y) && isFinite(v.Z)
}
func (v Vec3) Normalized() (Vec3, bool) {
	l := v.Length()
	if !isFinite(l) || l <= 1e-15 {
		return Vec3{}, false
	}
	return v.Mul(1 / l), true
}

// Sphere is a canonical emissive-material sphere.
type Sphere struct {
	ID     string  `json:"id"`
	Center Vec3    `json:"center"`
	Radius float64 `json:"radius"`
	Albedo Vec3    `json:"albedo"`
	Coat   float64 `json:"coat"`
}

// Plane is an infinite plane: dot(normal, point) = offset.
type Plane struct {
	ID     string  `json:"id"`
	Normal Vec3    `json:"normal"`
	Offset float64 `json:"offset"`
	Albedo Vec3    `json:"albedo"`
	Coat   float64 `json:"coat"`
}

// AreaLight is a rectangular emitter.
type AreaLight struct {
	ID            string  `json:"id"`
	Center        Vec3    `json:"center"`
	Normal        Vec3    `json:"normal"`
	Up            Vec3    `json:"up"`
	HalfWidth     float64 `json:"halfWidth"`
	HalfHeight    float64 `json:"halfHeight"`
	RadiantPowerW float64 `json:"radiantPowerW"`
	Color         Vec3    `json:"color"`
}

// Camera is a pinhole camera. Only the LookAt direction is used for the pose;
// its magnitude is retained and exported where the target format can express it.
type Camera struct {
	ID      string  `json:"id"`
	Eye     Vec3    `json:"eye"`
	LookAt  Vec3    `json:"lookAt"`
	Up      Vec3    `json:"up"`
	FovYRad float64 `json:"fovYRad"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
}

// Settings contains renderer features that are added over schema generations.
type Settings struct {
	ShadowSoftness float64 `json:"shadowSoftness"`
	ToneMapping    string  `json:"toneMapping"`
}

// UpgradeTrace records every default that was supplied while upgrading a
// source scene. OldValue is "<missing>" when the source had no field.
type UpgradeTrace struct {
	Field         string `json:"field"`
	OldValue      string `json:"oldValue"`
	DefaultValue  any    `json:"defaultValue"`
	DefaultSource string `json:"defaultSource"`
	FromVersion   string `json:"fromVersion"`
	ToVersion     string `json:"toVersion"`
}

// Scene is the canonical internal model. SourceVersion is never rewritten by
// export; WorkingVersion is always the newest internal schema.
type Scene struct {
	ID             string         `json:"id"`
	SourceVersion  string         `json:"sourceVersion"`
	WorkingVersion string         `json:"workingVersion"`
	Spheres        []Sphere       `json:"spheres"`
	Planes         []Plane        `json:"planes"`
	Lights         []AreaLight    `json:"lights"`
	Cameras        []Camera       `json:"cameras"`
	Settings       Settings       `json:"settings"`
	Trace          []UpgradeTrace `json:"trace,omitempty"`
}

// Truth is the analytic task ground truth. It deliberately contains only the
// quantities asserted by the reconstruction task.
type Truth struct {
	Spheres []TruthSphere `json:"spheres"`
	Lights  []TruthLight  `json:"lights"`
}

type TruthSphere struct {
	ID     string  `json:"id"`
	Center Vec3    `json:"center"`
	Radius float64 `json:"radius"`
	Albedo Vec3    `json:"albedo"`
}

type TruthLight struct {
	ID            string  `json:"id"`
	RadiantPowerW float64 `json:"radiantPowerW"`
}

// Clone returns a deep copy that is safe to store in a committed checkpoint.
func (s *Scene) Clone() *Scene {
	if s == nil {
		return nil
	}
	out := *s
	out.Spheres = append([]Sphere(nil), s.Spheres...)
	out.Planes = append([]Plane(nil), s.Planes...)
	out.Lights = append([]AreaLight(nil), s.Lights...)
	out.Cameras = append([]Camera(nil), s.Cameras...)
	out.Trace = append([]UpgradeTrace(nil), s.Trace...)
	return &out
}

// SortedIDs exposes stable identifiers for diagnostics.
func (s *Scene) SortedIDs() []string {
	ids := make([]string, 0, len(s.Spheres)+len(s.Planes)+len(s.Lights)+len(s.Cameras))
	for _, x := range s.Spheres {
		ids = append(ids, x.ID)
	}
	for _, x := range s.Planes {
		ids = append(ids, x.ID)
	}
	for _, x := range s.Lights {
		ids = append(ids, x.ID)
	}
	for _, x := range s.Cameras {
		ids = append(ids, x.ID)
	}
	sort.Strings(ids)
	return ids
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
