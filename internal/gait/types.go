package gait

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	PhaseStance        = "stance"
	PhaseSwing         = "swing"
	PhaseDoubleSupport = "double_support"

	EventLand    = "land"
	EventLiftoff = "liftoff"

	CyclePending   CycleState = "pending"
	CycleAligning  CycleState = "aligning"
	CycleSolving   CycleState = "solving"
	CycleCommitted CycleState = "committed"
	CycleFailed    CycleState = "failed"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) Add(o Vec3) Vec3           { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3           { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Scale(s float64) Vec3      { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Length() float64           { return sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z) }
func (v Vec3) HorizontalLength() float64 { return sqrt(v.X*v.X + v.Z*v.Z) }

func round6(v float64) float64 {
	return round(v*1_000_000) / 1_000_000
}

func (v Vec3) rounded() Vec3 {
	return Vec3{X: round6(v.X), Y: round6(v.Y), Z: round6(v.Z)}
}

type CycleState string

type Bone struct {
	Name        string `json:"name"`
	Parent      string `json:"parent,omitempty"`
	Role        string `json:"role,omitempty"`
	LocalOffset Vec3   `json:"local_offset"`
}

type Skeleton struct {
	ID          string  `json:"id"`
	GroundPlane float64 `json:"ground_plane"`
	Nodes       []Bone  `json:"nodes"`
}

type LegChain struct {
	Foot  string   `json:"foot"`
	Hip   string   `json:"hip"`
	Knee  string   `json:"knee"`
	Ankle string   `json:"ankle"`
	Toes  []string `json:"toes"`
}

type ROM struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

type HeightRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

type TargetSkeleton struct {
	ID             string      `json:"id"`
	Skeleton       Skeleton    `json:"skeleton"`
	LegLengthRatio float64     `json:"leg_length_ratio"`
	AnkleROM       ROM         `json:"ankle_rom"`
	FootReachable  HeightRange `json:"foot_reachable_height"`
	CycleFrames    []int       `json:"cycle_frames"`
	Legs           []LegChain  `json:"legs"`
}

type FootFrame struct {
	Position Vec3 `json:"position"`
	Velocity Vec3 `json:"velocity"`
}

type FootTrack struct {
	Foot   string      `json:"foot"`
	Frames []FootFrame `json:"frames"`
}

type CycleSpan struct {
	Index      int `json:"index"`
	StartFrame int `json:"start_frame"`
	EndFrame   int `json:"end_frame"` // inclusive
}

type SourceLoop struct {
	ID         string      `json:"id"`
	FrameCount int         `json:"frame_count"`
	Skeleton   Skeleton    `json:"skeleton"`
	Legs       []LegChain  `json:"legs"`
	Cycles     []CycleSpan `json:"cycles"`
	Root       []Vec3      `json:"root"`
	Feet       []FootTrack `json:"feet"`
}

type FramePhase struct {
	Global string `json:"global"`
	Left   string `json:"left"`
	Right  string `json:"right"`
}

type PhaseTrack struct {
	SourceID string       `json:"source_id"`
	Frames   []FramePhase `json:"frames"`
}

type PhaseTransition struct {
	Frame int    `json:"frame"`
	From  string `json:"from"`
	To    string `json:"to"`
}

type StrideLength struct {
	Cycle int     `json:"cycle"`
	Value float64 `json:"value"`
}

type StancePosition struct {
	Cycle    int    `json:"cycle"`
	Foot     string `json:"foot"`
	Position Vec3   `json:"position"`
}

type RootKeyframe struct {
	Frame    int  `json:"frame"`
	Position Vec3 `json:"position"`
}

type FootEvent struct {
	Frame int    `json:"frame"`
	Cycle int    `json:"cycle"`
	Foot  string `json:"foot"`
	Type  string `json:"type"`
}

type Truth struct {
	SourceID            string            `json:"source_id"`
	FrameCount          int               `json:"frame_count"`
	CycleCount          int               `json:"cycle_count"`
	PhaseTransitions    []PhaseTransition `json:"phase_transitions"`
	StrideLengths       []StrideLength    `json:"stride_lengths"`
	StanceFootPositions []StancePosition  `json:"stance_foot_positions"`
	RootKeyframes       []RootKeyframe    `json:"root_keyframes"`
	FootEvents          []FootEvent       `json:"foot_events"`
	GroundPlane         float64           `json:"ground_plane"`
}

type Params struct {
	ParameterVersion       string  `json:"parameter_version"`
	AlgorithmVersion       string  `json:"algorithm_version"`
	ContactHeight          float64 `json:"contact_height"`
	SwingHeightMin         float64 `json:"swing_height_min"`
	SwingSpeedMin          float64 `json:"swing_speed_min"`
	SwingLiftScale         float64 `json:"swing_lift_scale"`
	StanceSlideMax         float64 `json:"stance_slide_max"`
	FootJumpMax            float64 `json:"foot_jump_max"`
	LandingToleranceFrames int     `json:"landing_tolerance_frames"`
	PhaseRatioTolerance    float64 `json:"phase_ratio_tolerance"`
	MaxParallelism         int     `json:"max_parallelism"`
	CycleBufferLimit       int     `json:"cycle_buffer_limit"`
}

type PhaseRatios struct {
	Stance        float64 `json:"stance"`
	Swing         float64 `json:"swing"`
	DoubleSupport float64 `json:"double_support"`
}

type PhaseSegment struct {
	StartFrame int    `json:"start_frame"`
	EndFrame   int    `json:"end_frame"`
	Phase      string `json:"phase"`
}

type MappedCycle struct {
	Cycle             int            `json:"cycle"`
	SourceStartFrame  int            `json:"source_start_frame"`
	SourceEndFrame    int            `json:"source_end_frame"`
	TargetStartFrame  int            `json:"target_start_frame"`
	TargetEndFrame    int            `json:"target_end_frame"`
	Scale             float64        `json:"scale"`
	SourceProportions PhaseRatios    `json:"source_proportions"`
	TargetProportions PhaseRatios    `json:"target_proportions"`
	SourceSegments    []PhaseSegment `json:"source_segments"`
	TargetSegments    []PhaseSegment `json:"target_segments"`
	SourceFrameMap    []int          `json:"source_frame_map"`
}

type Mapping struct {
	SourceID string        `json:"source_id"`
	TargetID string        `json:"target_id"`
	Cycles   []MappedCycle `json:"cycles"`
}

type Correction struct {
	Frame          int    `json:"frame"`
	Cycle          int    `json:"cycle"`
	Foot           string `json:"foot,omitempty"`
	Reason         string `json:"reason"`
	MappedPhase    string `json:"mapped_phase"`
	DetectedPhase  string `json:"detected_phase"`
	CorrectedPhase string `json:"corrected_phase"`
}

type JointState struct {
	Bone     string  `json:"bone"`
	Position Vec3    `json:"position"`
	Angle    float64 `json:"angle"`
}

type ToeState struct {
	Bone     string  `json:"bone"`
	Position Vec3    `json:"position"`
	Angle    float64 `json:"angle"`
}

type SolvedFoot struct {
	Foot     string       `json:"foot"`
	Position Vec3         `json:"position"`
	Contact  bool         `json:"contact"`
	Speed    Vec3         `json:"speed"`
	Joints   []JointState `json:"joints"`
	Toes     []ToeState   `json:"toes"`
}

type FrameOutput struct {
	Frame       int          `json:"frame"`
	SourceFrame int          `json:"source_frame"`
	Phase       FramePhase   `json:"phase"`
	Root        Vec3         `json:"root"`
	Feet        []SolvedFoot `json:"feet"`
}

type CycleOutput struct {
	Cycle            int           `json:"cycle"`
	SourceStartFrame int           `json:"source_start_frame"`
	SourceEndFrame   int           `json:"source_end_frame"`
	TargetStartFrame int           `json:"target_start_frame"`
	TargetEndFrame   int           `json:"target_end_frame"`
	Scale            float64       `json:"scale"`
	Frames           []FrameOutput `json:"frames"`
	Events           []FootEvent   `json:"events"`
	Corrections      []Correction  `json:"corrections"`
}

type Result struct {
	SourceID    string        `json:"source_id"`
	TargetID    string        `json:"target_id"`
	FrameCount  int           `json:"frame_count"`
	Frames      []FrameOutput `json:"frames"`
	Events      []FootEvent   `json:"events"`
	Corrections []Correction  `json:"corrections"`
	Cycles      []CycleOutput `json:"cycles"`
}

type Status struct {
	Committed int          `json:"committed"`
	Total     int          `json:"total"`
	States    []CycleState `json:"states"`
	Paused    bool         `json:"paused"`
}

func canonicalHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func inputHash(source SourceLoop, phases PhaseTrack, target TargetSkeleton, params Params) (string, error) {
	return canonicalHash(struct {
		Source SourceLoop
		Phases PhaseTrack
		Target TargetSkeleton
		Params Params
	}{source, phases, target, params})
}

func cloneCycleOutput(in *CycleOutput) *CycleOutput {
	if in == nil {
		return nil
	}
	out := *in
	out.Frames = make([]FrameOutput, len(in.Frames))
	for i, frame := range in.Frames {
		out.Frames[i] = frame
		out.Frames[i].Feet = make([]SolvedFoot, len(frame.Feet))
		for j, foot := range frame.Feet {
			out.Frames[i].Feet[j] = foot
			out.Frames[i].Feet[j].Joints = append([]JointState(nil), foot.Joints...)
			out.Frames[i].Feet[j].Toes = append([]ToeState(nil), foot.Toes...)
		}
	}
	out.Events = append([]FootEvent(nil), in.Events...)
	out.Corrections = append([]Correction(nil), in.Corrections...)
	return &out
}
