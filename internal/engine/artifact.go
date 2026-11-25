package engine

import (
	"encoding/json"
	"fmt"

	"maskpreview/internal/core"
	"maskpreview/internal/segment"
)

type Artifact struct {
	Stage   Stage                `json:"stage"`
	Block   Block                `json:"block"`
	Mask    []byte               `json:"mask,omitempty"`
	Regions []segment.RegionStat `json:"regions,omitempty"`
}

func EncodeArtifact(a Artifact) ([]byte, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("encode stage artifact: %w", err)
	}
	return raw, nil
}

func DecodeArtifact(raw []byte) (Artifact, error) {
	var a Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return Artifact{}, fmt.Errorf("decode stage artifact: %w", err)
	}
	return a, nil
}

func ArtifactHash(raw []byte) string {
	return core.HashBytes(raw)
}
