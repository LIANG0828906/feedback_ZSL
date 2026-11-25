package core

import (
	"fmt"
	"math"
)

const (
	SchemaVersion    = "mocap-sample-v1"
	GeneratorVersion = "generator-1.0.0"
	AlgorithmVersion = "algo-1.0.0"
	DefaultSeed      = int64(0x4D4F434150) // "MOCAP" style fixed seed
)

const (
	ECodeSchema  = "E_SCHEMA"
	ECodeCycle   = "E_CYCLE"
	ECodeQuat    = "E_QUAT"
	ECodeMap     = "E_MAP"
	ECodeSolve   = "E_SOLVE"
	ECodeContact = "E_CONTACT"
	ECodeState   = "E_STATE"
	ECodeDiff    = "E_DIFF"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Frame   int    `json:"frame,omitempty"`
	Joint   string `json:"joint,omitempty"`
	Field   string `json:"field,omitempty"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Frame >= 0 && e.Joint != "" {
		return fmt.Sprintf("%s frame=%d joint=%s field=%s: %s", e.Code, e.Frame, e.Joint, e.Field, e.Message)
	}
	if e.Frame >= 0 {
		return fmt.Sprintf("%s frame=%d: %s", e.Code, e.Frame, e.Message)
	}
	if e.Joint != "" {
		return fmt.Sprintf("%s joint=%s: %s", e.Code, e.Joint, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message, Frame: -1}
}

func IsCode(err error, code string) bool {
	var me *Error
	if AsError(err, &me) {
		return me.Code == code
	}
	return false
}

func AsError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

type Vec3 [3]float64
type Quat [4]float64 // W, X, Y, Z

func FiniteFloat(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type AngleLimit struct {
	MinRadians float64 `json:"min_radians"`
	MaxRadians float64 `json:"max_radians"`
	Axis       Vec3    `json:"axis"`
}

type ReachLimit struct {
	Anchor      string  `json:"anchor"`
	MaxDistance float64 `json:"max_distance"`
}

type Joint struct {
	Name        string      `json:"name"`
	Parent      string      `json:"parent"`
	Offset      Vec3        `json:"offset"`
	Foot        bool        `json:"foot,omitempty"`
	EndEffector bool        `json:"end_effector,omitempty"`
	Limit       *AngleLimit `json:"limit,omitempty"`
	Reach       *ReachLimit `json:"reach,omitempty"`
}

type Skeleton struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	GroundY float64 `json:"ground_y"`
	Joints  []Joint `json:"joints"`
}

type Frame struct {
	Index     int    `json:"index"`
	Root      Vec3   `json:"root"`
	Rotations []Quat `json:"rotations"`
	Contacts  []bool `json:"contacts,omitempty"`
}

type ContactTrack struct {
	Foot   string `json:"foot"`
	Joint  string `json:"joint"`
	Values []bool `json:"values"`
}

type Clip struct {
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	Seed          int64          `json:"seed"`
	FramesCount   int            `json:"frames_count"`
	Feet          []string       `json:"feet"`
	Skeleton      Skeleton       `json:"skeleton"`
	Frames        []Frame        `json:"frames"`
	ContactTracks []ContactTrack `json:"contact_tracks"`
}

type TargetSet struct {
	SchemaVersion string     `json:"schema_version"`
	Targets       []Skeleton `json:"targets"`
}

type Interval [2]int

type Truth struct {
	SchemaVersion    string                     `json:"schema_version"`
	ClipID           string                     `json:"clip_id"`
	Seed             int64                      `json:"seed"`
	KeyFrames        []int                      `json:"key_frames"`
	WorldPositions   map[string]map[string]Vec3 `json:"world_positions"`
	BoneLengths      map[string]float64         `json:"bone_lengths"`
	ContactIntervals map[string][]Interval      `json:"contact_intervals"`
	RootStart        Vec3                       `json:"root_start"`
	RootEnd          Vec3                       `json:"root_end"`
	RootTrajectory   []Vec3                     `json:"root_trajectory"`
}

type AssetDecl struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Frames int    `json:"frames,omitempty"`
	Joints int    `json:"joints,omitempty"`
}

type Manifest struct {
	SchemaVersion string      `json:"schema_version"`
	Seed          int64       `json:"seed"`
	Generator     string      `json:"generator_version"`
	Clips         []AssetDecl `json:"clips"`
	Targets       []AssetDecl `json:"targets"`
	Truth         []AssetDecl `json:"truth"`
}

type Parameters struct {
	MappingID             string             `json:"mapping_id"`
	QuantStep             float64            `json:"quant_step"`
	GlobalScaleMultiplier float64            `json:"global_scale_multiplier"`
	AngleTolerance        float64            `json:"angle_tolerance"`
	ReachTolerance        float64            `json:"reach_tolerance"`
	MaxIterations         int                `json:"max_iterations"`
	ContactDriftLimit     float64            `json:"contact_drift_limit"`
	ContactFrameTolerance int                `json:"contact_frame_tolerance"`
	SmoothingAlpha        float64            `json:"smoothing_alpha"`
	MappingOverrides      map[string]string  `json:"mapping_overrides,omitempty"`
	ScaleOverrides        map[string]float64 `json:"scale_overrides,omitempty"`
}

func DefaultParameters() Parameters {
	return Parameters{
		QuantStep:             1.0 / 1024.0,
		GlobalScaleMultiplier: 1.0,
		AngleTolerance:        1e-4,
		ReachTolerance:        1e-4,
		MaxIterations:         24,
		ContactDriftLimit:     0.025,
		ContactFrameTolerance: 1,
		SmoothingAlpha:        0.8,
	}
}

type ResourceConfig struct {
	Workers         int `json:"workers"`
	BufferPerWorker int `json:"buffer_per_worker"`
}

func DefaultResources() ResourceConfig {
	return ResourceConfig{Workers: 3, BufferPerWorker: 4}
}
