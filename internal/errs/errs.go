package errs

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	EIDX   = "E_IDX"
	EMETA  = "E_META"
	EMERGE = "E_MERGE"
	EHOLE  = "E_HOLE"
	EPARA  = "E_PARA"
	ETOPO  = "E_TOPO"
	ESTATE = "E_STATE"
	EVER   = "E_VER"
	EDIFF  = "E_DIFF"
	EPOINT = "E_POINT"
	ELOOP  = "E_LOOP"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Stage   string         `json:"stage,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Stage != "" {
		return fmt.Sprintf("%s [%s] %s", e.Code, e.Stage, e.Message)
	}
	return fmt.Sprintf("%s %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return nil }

func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func InStage(err error, stage string) error {
	var ce *Error
	if errors.As(err, &ce) {
		out := *ce
		out.Stage = stage
		return &out
	}
	return err
}

func WithDetails(err error, details map[string]any) error {
	var ce *Error
	if !errors.As(err, &ce) {
		return err
	}
	out := *ce
	if out.Details == nil {
		out.Details = map[string]any{}
	}
	for k, v := range details {
		out.Details[k] = v
	}
	return &out
}

func WithDetail(err error, key string, value any) error {
	return WithDetails(err, map[string]any{key: value})
}

func Code(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func Details(err error) map[string]any {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Details
	}
	return nil
}

func JSON(err error) []byte {
	payload := map[string]any{"ok": false}
	var ce *Error
	if errors.As(err, &ce) {
		payload["error"] = ce
	} else if err != nil {
		payload["error"] = Error{Code: "E_INTERNAL", Message: err.Error()}
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return b
}
