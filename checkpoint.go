package acoustic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type RoomSnapshot struct {
	RoomID             int                `json:"roomId"`
	CommitSequence     int                `json:"commitSequence"`
	ParamsVersion      int64              `json:"paramsVersion"`
	AlgorithmVersion   string             `json:"algorithmVersion"`
	ProjectFingerprint string             `json:"projectFingerprint"`
	ContentHash        string             `json:"contentHash"`
	Reflectors         []Reflector        `json:"reflectors"`
	Openings           []Opening          `json:"openings"`
	ChainSummaries     []ChainSummary     `json:"chainSummaries"`
	ListenerSolutions  []ListenerSolution `json:"listenerSolutions"`
}

type CheckpointLoadResult struct {
	Snapshots     []RoomSnapshot `json:"snapshots"`
	Invalidated   bool           `json:"invalidated"`
	InvalidReason string         `json:"invalidReason,omitempty"`
}

type CheckpointStore interface {
	Load(ctx context.Context, projectFingerprint string, paramsVersion int64, algorithmVersion string) (CheckpointLoadResult, error)
	Commit(ctx context.Context, snapshot RoomSnapshot) error
	Discard(ctx context.Context) error
}

func SnapshotDigest(snapshot RoomSnapshot) (string, error) {
	clone := snapshot
	clone.ContentHash = ""
	data, err := json.Marshal(clone)
	if err != nil {
		return "", &Error{Code: CodeCheckpoint, Op: "snapshot_hash", RoomIDs: []int{snapshot.RoomID}, Message: "cannot encode room snapshot", Cause: err.Error()}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ProjectFingerprint(project *Project) (string, error) {
	if project == nil {
		return "", &Error{Code: CodeCheckpoint, Op: "project_fingerprint", Message: "project is nil"}
	}
	clone := *project
	clone.Truth = nil
	clone.Rooms = append([]Room(nil), project.Rooms...)
	clone.Walls = append([]Wall(nil), project.Walls...)
	clone.Openings = append([]Opening(nil), project.Openings...)
	clone.Sources = append([]Source(nil), project.Sources...)
	clone.Reflectors = append([]Reflector(nil), project.Reflectors...)
	clone.Listeners = append([]Listener(nil), project.Listeners...)
	sortRooms(clone.Rooms)
	sortWalls(clone.Walls)
	sortOpenings(clone.Openings)
	sortSourcesByID(clone.Sources)
	sortReflectorsByID(clone.Reflectors)
	sortListeners(clone.Listeners)
	data, err := json.Marshal(clone)
	if err != nil {
		return "", &Error{Code: CodeCheckpoint, Op: "project_fingerprint", Message: "cannot encode project", Cause: err.Error()}
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func sortRooms(values []Room) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].ID < values[j-1].ID; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortWalls(values []Wall) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].ID < values[j-1].ID; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortOpenings(values []Opening) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].ID < values[j-1].ID; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortListeners(values []Listener) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].ID < values[j-1].ID; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
