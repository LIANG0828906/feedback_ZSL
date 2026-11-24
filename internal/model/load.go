package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func LoadWorkspace(path string) (Workspace, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Workspace{}, &CodedError{Code: E_IO, Op: "load_workspace", Message: err.Error(), Cause: err}
	}
	return DecodeWorkspace(b)
}

func DecodeWorkspace(b []byte) (Workspace, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w Workspace
	if err := dec.Decode(&w); err != nil {
		return Workspace{}, &CodedError{
			Code: E_SCHEMA, Op: "decode_workspace",
			Message: fmt.Sprintf("invalid workspace JSON: %v", err), Cause: err,
		}
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Workspace{}, Err(E_SCHEMA, "decode_workspace", "workspace contains trailing JSON values")
		}
		return Workspace{}, &CodedError{
			Code: E_SCHEMA, Op: "decode_workspace",
			Message: fmt.Sprintf("invalid trailing workspace JSON: %v", err), Cause: err,
		}
	}
	if err := ValidateWorkspace(w); err != nil {
		return Workspace{}, err
	}
	return w, nil
}

func SaveWorkspace(path string, w Workspace) error {
	if err := ValidateWorkspace(w); err != nil {
		return err
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return &CodedError{Code: E_IO, Op: "save_workspace", Message: err.Error(), Cause: err}
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return &CodedError{Code: E_IO, Op: "save_workspace", Message: err.Error(), Cause: err}
	}
	return nil
}
