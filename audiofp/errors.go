package audiofp

import (
	"errors"
	"fmt"
)

const (
	CodeSchema    = "E_SCHEMA"
	CodeParameter = "E_PARA"
	CodeDecode    = "E_DECODE"
	CodeOrder     = "E_ORDER"
	CodeStage     = "E_STAGE"
	CodeDiff      = "E_DIFF"
)

// Error is the stable, machine-readable error envelope used by the domain
// component. The CLI serializes it directly in demo reports.
type Error struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	AssetID string `json:"asset_id,omitempty"`
	Shard   int    `json:"shard,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

func (e *Error) Error() string {
	msg := e.Code
	if e.AssetID != "" {
		msg += fmt.Sprintf(" asset=%s", e.AssetID)
	}
	if e.Shard > 0 {
		msg += fmt.Sprintf(" shard=%d", e.Shard)
	}
	if e.Frame >= 0 && (e.Frame > 0 || e.Field == "frame_order") {
		msg += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Field != "" {
		msg += fmt.Sprintf(" field=%s", e.Field)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func schemaError(field, message string) error {
	return &Error{Code: CodeSchema, Field: field, Message: message, Frame: -1}
}

func parameterError(field, message string) error {
	return &Error{Code: CodeParameter, Field: field, Message: message, Frame: -1}
}

func decodeError(assetID, field, message string) error {
	return &Error{Code: CodeDecode, AssetID: assetID, Field: field, Message: message, Frame: -1}
}

func orderError(assetID string, shard, frame int, message string) error {
	return &Error{Code: CodeOrder, AssetID: assetID, Shard: shard, Frame: frame, Message: message}
}

func stageError(assetID string, shard, frame int, err error) error {
	return &Error{Code: CodeStage, AssetID: assetID, Shard: shard, Frame: frame, Message: err.Error(), Err: err}
}

// DiffError creates the E_DIFF envelope used by consistency comparison.
func DiffError(assetID string, frame int, field string, message string) error {
	return &Error{Code: CodeDiff, AssetID: assetID, Frame: frame, Field: field, Message: message}
}

// CodeOf returns the stable error code, or an empty string.
func CodeOf(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// AsError exposes the typed error when present.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
