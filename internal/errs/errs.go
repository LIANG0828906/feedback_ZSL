package errs

import (
	"errors"
	"fmt"
)

type Code string

const (
	ESchema Code = "E_SCHEMA"
	EOVF    Code = "E_OVF"
	EOrder  Code = "E_ORDER"
	EState  Code = "E_STATE"
	EDecode Code = "E_DECODE"
	EFlag   Code = "E_FLAG"
	ECool   Code = "E_COOL"
	ECheck  Code = "E_CHECKPOINT"
)

type Error struct {
	Code    Code
	Field   string
	Message string
	Round   int64
	UnitID  string
	SkillID string
	Values  []string
	Cause   error
}

func (e *Error) Error() string {
	msg := string(e.Code)
	if e.Field != "" {
		msg += " field=" + e.Field
	}
	if e.Round != 0 {
		msg += fmt.Sprintf(" round=%d", e.Round)
	}
	if e.UnitID != "" {
		msg += " unit=" + e.UnitID
	}
	if e.SkillID != "" {
		msg += " skill=" + e.SkillID
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	for i, value := range e.Values {
		msg += fmt.Sprintf(" value[%d]=%s", i, value)
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) WithRound(round int64) *Error {
	e.Round = round
	return e
}

func (e *Error) WithUnit(unitID string) *Error {
	e.UnitID = unitID
	return e
}

func (e *Error) WithSkill(skillID string) *Error {
	e.SkillID = skillID
	return e
}

func Wrap(code Code, field string, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...), Cause: cause}
}

func WithLocation(err error, round int64, unitID, skillID string) error {
	var ke *Error
	if errors.As(err, &ke) {
		if ke.Round == 0 {
			ke.Round = round
		}
		if ke.UnitID == "" {
			ke.UnitID = unitID
		}
		if ke.SkillID == "" {
			ke.SkillID = skillID
		}
		return ke
	}
	return &Error{
		Code:    EState,
		Round:   round,
		UnitID:  unitID,
		SkillID: skillID,
		Message: err.Error(),
		Cause:   err,
	}
}

func CodeOf(err error) Code {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Code
	}
	return ""
}

func IsCode(err error, code Code) bool {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Code == code
	}
	return false
}
