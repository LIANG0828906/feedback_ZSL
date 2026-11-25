package fog

import (
	"errors"
	"fmt"
)

// Code is the stable machine-readable failure category used by the kernel.
type Code string

const (
	ESchema  Code = "E_SCHEMA"
	ETrans   Code = "E_TRANS"
	EDiverge Code = "E_DIVERGE"
	EDecode  Code = "E_DECODE"
	EOrder   Code = "E_ORDER"
	EDens    Code = "E_DENS"
	EBlock   Code = "E_BLOCK"
	EBudget  Code = "E_BUDGET"
	EStore   Code = "E_STORE"
)

// Sentinel errors allow errors.Is to recognize both specialized and schema classes.
var (
	ErrSchema  = errors.New(string(ESchema))
	ErrTrans   = errors.New(string(ETrans))
	ErrDiverge = errors.New(string(EDiverge))
	ErrDecode  = errors.New(string(EDecode))
	ErrOrder   = errors.New(string(EOrder))
	ErrDens    = errors.New(string(EDens))
	ErrBlock   = errors.New(string(EBlock))
	ErrBudget  = errors.New(string(EBudget))
	ErrStore   = errors.New(string(EStore))
)

// Error carries all localization information required by diagnostics and tests.
type Error struct {
	Code      Code    `json:"code"`
	MediaID   string  `json:"media_id,omitempty"`
	BlockID   *int    `json:"block_id,omitempty"`
	Field     string  `json:"field,omitempty"`
	RayID     string  `json:"ray_id,omitempty"`
	Segment   string  `json:"segment,omitempty"`
	X         int     `json:"x,omitempty"`
	Y         int     `json:"y,omitempty"`
	Actual    float64 `json:"actual,omitempty"`
	Expected  float64 `json:"expected,omitempty"`
	Tolerance float64 `json:"tolerance,omitempty"`
	Message   string  `json:"message"`
	Detail    any     `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	msg := string(e.Code)
	switch e.Code {
	case EOrder, EDens, EDecode:
		msg += "(also E_SCHEMA)"
	}
	if e.MediaID != "" {
		msg += " media=" + e.MediaID
	}
	if e.BlockID != nil {
		msg += fmt.Sprintf(" block=%d", *e.BlockID)
	}
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.RayID != "" {
		msg += " ray=" + e.RayID
	}
	if e.Segment != "" {
		msg += " segment=" + e.Segment
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Unwrap exposes broad classes. Order and density failures are also schema
// failures, while retaining their precise injection-chain code.
func (e *Error) Unwrap() []error {
	switch e.Code {
	case EOrder:
		return []error{ErrOrder, ErrSchema}
	case EDens:
		return []error{ErrDens, ErrSchema}
	case EDecode:
		return []error{ErrDecode, ErrSchema}
	case ETrans:
		return []error{ErrTrans}
	case EDiverge:
		return []error{ErrDiverge}
	case EBlock:
		return []error{ErrBlock}
	case EBudget:
		return []error{ErrBudget}
	case EStore:
		return []error{ErrStore}
	default:
		return []error{ErrSchema}
	}
}

func blockIDPtr(id int) *int { return &id }
