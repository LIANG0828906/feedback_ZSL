package repair

import (
	"fmt"

	"meshforge/internal/errs"
	"meshforge/internal/mesh"
)

type StageName string

const (
	StageDiagnose StageName = "diagnose"
	StageWeld     StageName = "weld"
	StageFill     StageName = "fill"
	StageOrient   StageName = "orient"
	StageDenoise  StageName = "denoise"
	StageStats    StageName = "stats"
)

var StageOrder = []StageName{
	StageDiagnose, StageWeld, StageFill, StageOrient, StageDenoise, StageStats,
}

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCommitted  Status = "committed"
	StatusFailed     Status = "failed"
)

type StageRecord struct {
	Index       int              `json:"index"`
	Name        StageName        `json:"name"`
	Status      Status           `json:"status"`
	ParamsHash  string           `json:"params_version"`
	MeshHash    string           `json:"mesh_hash,omitempty"`
	ParentHash  string           `json:"parent_hash,omitempty"`
	Stats       *mesh.Stats      `json:"stats,omitempty"`
	Details     map[string]any   `json:"details,omitempty"`
	Error       *errs.CodedError `json:"error,omitempty"`
	StartedAt   string           `json:"started_at,omitempty"`
	CommittedAt string           `json:"committed_at,omitempty"`
	DurationMS  int64            `json:"duration_ms,omitempty"`
}

func legalTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusProcessing
	case StatusProcessing:
		return to == StatusCommitted || to == StatusFailed
	case StatusFailed:
		return to == StatusProcessing
	case StatusCommitted:
		return false
	default:
		return false
	}
}

func transitionError(from, to Status, stage StageName) *errs.CodedError {
	return errs.New(errs.CodeState,
		"illegal stage transition %s -> %s for %s; state preserved",
		from, to, stage)
}

func stageIndex(name StageName) int {
	for i, s := range StageOrder {
		if s == name {
			return i
		}
	}
	return -1
}

func stageAt(i int) (StageName, error) {
	if i < 0 || i >= len(StageOrder) {
		return "", fmt.Errorf("stage index %d outside [0,%d)", i, len(StageOrder))
	}
	return StageOrder[i], nil
}
