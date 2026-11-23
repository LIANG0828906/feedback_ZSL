package workbench

import "fmt"

const (
	CodeMeta     = "E_META"
	CodeBand     = "E_BAND"
	CodeOrder    = "E_ORDER"
	CodeReg      = "E_REG"
	CodePara     = "E_PARA"
	CodeGamut    = "E_GAMUT"
	CodeScale    = "E_SCALE"
	CodeState    = "E_STATE"
	CodeIO       = "E_IO"
	CodeBuffer   = "E_BUFFER"
	CodeInternal = "E_INTERNAL"
)

// WorkbenchError is the single error representation returned by the engine
// and HTTP API. The structured fields deliberately expose the offending field,
// measured value and decision context instead of collapsing failures into text.
type WorkbenchError struct {
	Code     string             `json:"code"`
	Message  string             `json:"message"`
	Field    string             `json:"field,omitempty"`
	Actual   any                `json:"actual,omitempty"`
	Expected any                `json:"expected,omitempty"`
	BandPair string             `json:"bandPair,omitempty"`
	Offset   *float64           `json:"offset,omitempty"`
	Pixel    *PixelCoord        `json:"pixel,omitempty"`
	Channel  int                `json:"channel,omitempty"`
	Segment  int                `json:"segment,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

func (e *WorkbenchError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s=%v)", e.Code, e.Message, e.Field, e.Actual)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func wbErr(code, format string, args ...any) *WorkbenchError {
	return &WorkbenchError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func fieldErr(code, field string, actual, expected any, format string, args ...any) *WorkbenchError {
	e := wbErr(code, format, args...)
	e.Field = field
	e.Actual = actual
	e.Expected = expected
	return e
}

// AsWorkbenchError returns a normalized error for callers that need to expose
// unexpected panics or wrapped failures without leaking internals as E_* codes.
func AsWorkbenchError(err error) *WorkbenchError {
	if err == nil {
		return nil
	}
	if e, ok := err.(*WorkbenchError); ok {
		return e
	}
	return wbErr(CodeInternal, "%v", err)
}
