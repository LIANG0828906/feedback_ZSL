package wb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	ParamVersion     = 1
	AlgorithmVersion = 1
)

type PathSummary struct {
	SourceID       string   `json:"sourceId"`
	ListenerID     string   `json:"listenerId"`
	SourceRoom     string   `json:"sourceRoom"`
	ListenerRoom   string   `json:"listenerRoom"`
	RoomChain      []string `json:"roomChain"`
	DoorChain      []string `json:"doorChain"`
	Length         int      `json:"length"`
	OcclusionCount int      `json:"occlusionCount"`
	OcclusionDB    Fixed    `json:"occlusionDb"`
}

func SummarizePath(p PathResult) PathSummary {
	return PathSummary{
		SourceID:       p.SourceID,
		ListenerID:     p.ListenerID,
		SourceRoom:     p.SourceRoom,
		ListenerRoom:   p.ListenerRoom,
		RoomChain:      append([]string(nil), p.RoomChain...),
		DoorChain:      append([]string(nil), p.DoorChain...),
		Length:         p.Length,
		OcclusionCount: p.OcclusionCount,
		OcclusionDB:    p.OcclusionDB,
	}
}

type CheckpointData struct {
	RoomID          string           `json:"roomId"`
	Rect            Rect             `json:"rect"`
	Params          RoomParams       `json:"params"`
	Openings        []Opening        `json:"openings"`
	PathSummaries   []PathSummary    `json:"pathSummaries"`
	ListenerResults []ListenerResult `json:"listenerResults"`
}

type StoredCheckpoint struct {
	RoomID           string
	CommitOrder      int
	InputHash        string
	ContentHash      string
	ParamVersion     int
	AlgorithmVersion int
	ParamsJSON       []byte
	OpeningsJSON     []byte
	PathsJSON        []byte
	ListenersJSON    []byte
}

type CheckpointBundle struct {
	InputHash  string
	Revision   int
	SampleJSON []byte
	Items      []StoredCheckpoint
}

type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, item StoredCheckpoint, sampleJSON []byte, revision int) error
	LoadCheckpoints(ctx context.Context) (CheckpointBundle, error)
	ResetCheckpoints(ctx context.Context) error
}

func HashCheckpointParts(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BuildStoredCheckpoint(inputHash string, order int, data CheckpointData) (StoredCheckpoint, *APIError) {
	paramsJSON, err := json.Marshal(data.Params)
	if err != nil {
		return StoredCheckpoint{}, NewAPIError(CodeInternal, data.RoomID, "", "marshal params: %v", err)
	}
	openingsJSON, err := json.Marshal(data.Openings)
	if err != nil {
		return StoredCheckpoint{}, NewAPIError(CodeInternal, data.RoomID, "", "marshal openings: %v", err)
	}
	pathsJSON, err := json.Marshal(data.PathSummaries)
	if err != nil {
		return StoredCheckpoint{}, NewAPIError(CodeInternal, data.RoomID, "", "marshal paths: %v", err)
	}
	listenersJSON, err := json.Marshal(data.ListenerResults)
	if err != nil {
		return StoredCheckpoint{}, NewAPIError(CodeInternal, data.RoomID, "", "marshal listeners: %v", err)
	}
	hash := HashCheckpointParts(paramsJSON, openingsJSON, pathsJSON, listenersJSON)
	return StoredCheckpoint{
		RoomID:           data.RoomID,
		CommitOrder:      order,
		InputHash:        inputHash,
		ContentHash:      hash,
		ParamVersion:     ParamVersion,
		AlgorithmVersion: AlgorithmVersion,
		ParamsJSON:       paramsJSON,
		OpeningsJSON:     openingsJSON,
		PathsJSON:        pathsJSON,
		ListenersJSON:    listenersJSON,
	}, nil
}

func VerifyAndDecodeStoredCheckpoint(item StoredCheckpoint, expectedInputHash string) (*CheckpointData, *APIError) {
	objectID := "checkpoint:" + item.RoomID
	if item.ParamVersion != ParamVersion || item.AlgorithmVersion != AlgorithmVersion {
		return nil, NewAPIError(CodeDiff, objectID, "version",
			"version mismatch: got p%d/a%d want p%d/a%d",
			item.ParamVersion, item.AlgorithmVersion, ParamVersion, AlgorithmVersion)
	}
	if item.InputHash != expectedInputHash {
		return nil, NewAPIError(CodeDiff, objectID, "inputHash",
			"checkpoint input generation mismatch")
	}
	actualHash := HashCheckpointParts(item.ParamsJSON, item.OpeningsJSON, item.PathsJSON, item.ListenersJSON)
	if actualHash != item.ContentHash {
		return nil, NewAPIError(CodeDiff, objectID, "contentHash",
			"content hash mismatch: stored %s computed %s", item.ContentHash, actualHash)
	}

	var params RoomParams
	if err := json.Unmarshal(item.ParamsJSON, &params); err != nil {
		return nil, NewAPIError(CodeDiff, objectID, "paramsJson", "params snapshot is corrupt: %v", err)
	}
	var openings []Opening
	if err := json.Unmarshal(item.OpeningsJSON, &openings); err != nil {
		return nil, NewAPIError(CodeDiff, objectID, "openingsJson", "opening snapshot is corrupt: %v", err)
	}
	var summaries []PathSummary
	if err := json.Unmarshal(item.PathsJSON, &summaries); err != nil {
		return nil, NewAPIError(CodeDiff, objectID, "pathsJson", "path summary is corrupt: %v", err)
	}
	var listeners []ListenerResult
	if err := json.Unmarshal(item.ListenersJSON, &listeners); err != nil {
		return nil, NewAPIError(CodeDiff, objectID, "listenersJson", "listener result is corrupt: %v", err)
	}
	return &CheckpointData{
		RoomID:          item.RoomID,
		Params:          params,
		Openings:        openings,
		PathSummaries:   summaries,
		ListenerResults: listeners,
	}, nil
}

type RecoveryReport struct {
	Recovered   int    `json:"recovered"`
	Discarded   bool   `json:"discarded"`
	Reason      string `json:"reason,omitempty"`
	SampleFound bool   `json:"sampleFound"`
}
