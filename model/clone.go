package model

import "encoding/json"

func CloneProject(in *Project) (*Project, error) {
	if in == nil {
		return nil, NewError(CodeInternal, "project", "cannot clone nil project")
	}
	buf, err := json.Marshal(in)
	if err != nil {
		return nil, NewError(CodeInternal, "project", "clone encode failed: %v", err)
	}
	var out Project
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, NewError(CodeInternal, "project", "clone decode failed: %v", err)
	}
	return &out, nil
}
