package gait

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	CodeSchema   = "E_SCHEMA"
	CodeCycle    = "E_CYCLE"
	CodePhase    = "E_PHASE"
	CodeAlign    = "E_ALIGN"
	CodeHeight   = "E_HEIGHT"
	CodePara     = "E_PARA"
	CodeState    = "E_STATE"
	CodeJump     = "E_JUMP"
	CodeRatio    = "E_RATIO"
	CodeDuration = "E_DUR"
	CodeDiverge  = "E_DIVERGE"
	CodeBuffer   = "E_BUFFER"
)

// AppError is the machine-readable error returned by the retargeting pipeline.
// Frame and Cycle use zero-based indexes. A negative value means "not applicable".
type AppError struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Frame     int             `json:"frame"`
	Cycle     int             `json:"cycle"`
	Foot      string          `json:"foot,omitempty"`
	Field     string          `json:"field,omitempty"`
	Conflicts []AlignConflict `json:"conflicts,omitempty"`
	Cause     error           `json:"-"`
}

type AlignConflict struct {
	Cycle             int         `json:"cycle"`
	SourceProportions PhaseRatios `json:"source_proportions"`
	TargetProportions PhaseRatios `json:"target_proportions"`
	Reason            string      `json:"reason"`
}

func (e *AppError) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Cycle >= 0 {
		fmt.Fprintf(&b, " cycle=%d", e.Cycle)
	}
	if e.Frame >= 0 {
		fmt.Fprintf(&b, " frame=%d", e.Frame)
	}
	if e.Foot != "" {
		fmt.Fprintf(&b, " foot=%s", e.Foot)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if len(e.Conflicts) > 0 {
		conflicts := append([]AlignConflict(nil), e.Conflicts...)
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Cycle < conflicts[j].Cycle })
		for _, c := range conflicts {
			fmt.Fprintf(&b, "; cycle %d source={stance:%.4f swing:%.4f double:%.4f} target={stance:%.4f swing:%.4f double:%.4f}: %s",
				c.Cycle,
				c.SourceProportions.Stance, c.SourceProportions.Swing, c.SourceProportions.DoubleSupport,
				c.TargetProportions.Stance, c.TargetProportions.Swing, c.TargetProportions.DoubleSupport,
				c.Reason)
		}
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	return b.String()
}

func (e *AppError) Unwrap() error { return e.Cause }

func appErr(code, message string) *AppError {
	return &AppError{Code: code, Message: message, Frame: -1, Cycle: -1}
}

func errAt(code, message string, frame int) *AppError {
	e := appErr(code, message)
	e.Frame = frame
	return e
}

func errCycle(code, message string, cycle int) *AppError {
	e := appErr(code, message)
	e.Cycle = cycle
	return e
}

// CodeOf returns the AppError code, or an empty string for unrelated errors.
func CodeOf(err error) string {
	var app *AppError
	if errors.As(err, &app) {
		return app.Code
	}
	return ""
}
