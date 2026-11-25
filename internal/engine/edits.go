package engine

import (
	"encoding/json"
	"os"

	"uvenhance/internal/apperr"
	"uvenhance/internal/yuv"
)

type EditStep struct {
	Name   string     `json:"name"`
	Params yuv.Params `json:"params"`
}

type EditSequence struct {
	Name             string     `json:"name"`
	ParamVersion     int        `json:"parameter_version"`
	AlignmentVersion int        `json:"alignment_version"`
	Steps            []EditStep `json:"steps"`
}

func LoadEditSequence(path string) (*EditSequence, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.IO("read edit sequence", err)
	}
	var seq EditSequence
	if err := json.Unmarshal(raw, &seq); err != nil {
		return nil, apperr.Meta("edits", "invalid JSON: "+err.Error())
	}
	if seq.ParamVersion <= 0 || seq.AlignmentVersion <= 0 || len(seq.Steps) == 0 {
		return nil, apperr.Meta("edits", "parameter_version, alignment_version and steps are required")
	}
	for i, step := range seq.Steps {
		if step.Name == "" {
			return nil, apperr.Meta("edits.steps.name", "step name is required")
		}
		if err := step.Params.Validate(); err != nil {
			return nil, err
		}
		_ = i
	}
	return &seq, nil
}
