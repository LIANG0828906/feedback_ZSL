package errs

import (
	"encoding/json"
	"fmt"
)

type Code string

const (
	CodeGeom   Code = "E_GEOM"
	CodeTolo   Code = "E_TOLO"
	CodePara   Code = "E_PARA"
	CodeOrient Code = "E_ORIENT"
	CodeScale  Code = "E_SCALE"
	CodeState  Code = "E_STATE"
	CodeIO     Code = "E_IO"
)

// CodedError is deliberately data-rich so CLI, HTTP and test callers can make
// the same decision without parsing an error string.
type CodedError struct {
	Code               Code    `json:"code"`
	Message            string  `json:"message"`
	Stage              string  `json:"stage,omitempty"`
	FaceIndex          *int    `json:"face_index,omitempty"`
	SuggestedTolerance float64 `json:"suggested_tolerance,omitempty"`
	ConflictComponents []int   `json:"conflict_components,omitempty"`
	Item               string  `json:"item,omitempty"`
	Left               any     `json:"left,omitempty"`
	Right              any     `json:"right,omitempty"`
	Tolerance          any     `json:"tolerance,omitempty"`
	Details            any     `json:"details,omitempty"`
}

func (e *CodedError) Error() string {
	if e == nil {
		return ""
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func New(code Code, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func WithFace(err *CodedError, face int) *CodedError {
	err.FaceIndex = &face
	return err
}

func As(err error) (*CodedError, bool) {
	if err == nil {
		return nil, false
	}
	ce, ok := err.(*CodedError)
	return ce, ok
}
