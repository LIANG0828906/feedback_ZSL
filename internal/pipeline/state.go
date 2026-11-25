package pipeline

import (
	"time"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
	"meshdoctor/internal/sample"
)

type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateCommitted State = "committed"
	StateFailed    State = "failed"
)

const (
	StageDiagnostic = "diagnostic"
	StageWeld       = "weld"
	StageFill       = "fill"
	StageOrient     = "orient"
	StageSmooth     = "smooth"
	StageStats      = "stats"
)

var Stages = []string{StageDiagnostic, StageWeld, StageFill, StageOrient, StageSmooth, StageStats}

func StageIndex(stage string) int {
	for i, s := range Stages {
		if s == stage {
			return i
		}
	}
	return -1
}

var legalTransitions = map[State]map[State]bool{
	StatePending:   {StateRunning: true},
	StateRunning:   {StateCommitted: true, StateFailed: true, StatePending: true},
	StateFailed:    {StateRunning: true},
	StateCommitted: {},
}

func Transition(current, next State) error {
	if current == next {
		return nil
	}
	if legalTransitions[current] != nil && legalTransitions[current][next] {
		return nil
	}
	return errs.New(errs.ESTATE, "illegal stage transition %s -> %s", current, next)
}

type DiagnosticReport struct {
	ValidationOK bool               `json:"validation_ok"`
	Stats        mesh.TopologyStats `json:"stats"`
	Defects      []sample.Defect    `json:"defects"`
}

type PointCheck struct {
	PointID   string    `json:"point_id"`
	Distance  float64   `json:"distance"`
	Tolerance float64   `json:"tolerance"`
	FaceIndex int       `json:"face_index"`
	Position  mesh.Vec3 `json:"position"`
	Passed    bool      `json:"passed"`
}

type LoopCheck struct {
	RingID        string      `json:"ring_id"`
	ClosureError  float64     `json:"closure_error"`
	Tolerance     float64     `json:"tolerance"`
	WorstSegment  int         `json:"worst_segment"`
	WorstDistance float64     `json:"worst_distance"`
	Coordinates   []mesh.Vec3 `json:"coordinates"`
	Passed        bool        `json:"passed"`
}

type TruthReport struct {
	Points []PointCheck `json:"points"`
	Loops  []LoopCheck  `json:"loops"`
	OK     bool         `json:"ok"`
}

type StageSnapshot struct {
	SampleID     string             `json:"sample_id"`
	Stage        string             `json:"stage"`
	StageIndex   int                `json:"stage_index"`
	State        State              `json:"state"`
	ParamVersion string             `json:"param_version"`
	AlgoVersion  string             `json:"algo_version"`
	Params       Params             `json:"params"`
	Mesh         *mesh.Mesh         `json:"mesh,omitempty"`
	MeshHash     string             `json:"mesh_hash"`
	Stats        mesh.TopologyStats `json:"stats"`
	Diagnostic   *DiagnosticReport  `json:"diagnostic,omitempty"`
	Weld         *WeldReport        `json:"weld,omitempty"`
	Fill         *FillReport        `json:"fill,omitempty"`
	Orient       *OrientReport      `json:"orient,omitempty"`
	Smooth       *SmoothReport      `json:"smooth,omitempty"`
	Truth        *TruthReport       `json:"truth,omitempty"`
	ErrorCode    string             `json:"error_code,omitempty"`
	ErrorMessage string             `json:"error_message,omitempty"`
	CommittedAt  time.Time          `json:"committed_at,omitempty"`
}

func (s StageSnapshot) CanonicalMeshBytes() []byte {
	if s.Mesh == nil {
		return nil
	}
	return s.Mesh.CanonicalBytes()
}

type PathResult struct {
	Path              string             `json:"path"`
	SampleID          string             `json:"sample_id"`
	ParamVersion      string             `json:"param_version"`
	Stages            []StageSnapshot    `json:"stages"`
	CommittedSequence []string           `json:"committed_sequence"`
	FinalHash         string             `json:"final_hash"`
	FinalStats        mesh.TopologyStats `json:"final_stats"`
	CacheHits         int                `json:"cache_hits,omitempty"`
	CacheMisses       int                `json:"cache_misses,omitempty"`
	StopReason        string             `json:"stop_reason,omitempty"`
}

func (r *PathResult) FinalMesh() *mesh.Mesh {
	if r == nil || len(r.Stages) == 0 {
		return nil
	}
	return r.Stages[len(r.Stages)-1].Mesh
}
