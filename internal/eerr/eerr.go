package eerr

import (
	"errors"
	"fmt"
	"strings"
)

type Code string

const (
	ESchema     Code = "E_SCHEMA"
	EOvf        Code = "E_OVF"
	EConflict   Code = "E_CONFLICT"
	EDiff       Code = "E_DIFF"
	ECheckpoint Code = "E_CHECKPOINT"
	ECanceled   Code = "E_CANCELED"
)

type Error struct {
	Code      Code
	Field     string
	Message   string
	Operands  []any
	Set       []string
	Round     int
	Expected  any
	Actual    any
	FirstDiff string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", e.Code)
	if e.Field != "" {
		fmt.Fprintf(&b, " field=%s", e.Field)
	}
	if e.Round > 0 {
		fmt.Fprintf(&b, " round=%d", e.Round)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if len(e.Operands) > 0 {
		parts := make([]string, len(e.Operands))
		for i, op := range e.Operands {
			parts[i] = fmt.Sprint(op)
		}
		fmt.Fprintf(&b, " operands=[%s]", strings.Join(parts, ", "))
	}
	if len(e.Set) > 0 {
		fmt.Fprintf(&b, " set={%s}", strings.Join(e.Set, ","))
	}
	if e.FirstDiff != "" {
		fmt.Fprintf(&b, " first_diff=%s", e.FirstDiff)
	}
	return b.String()
}

func New(code Code, field, format string, args ...any) *Error {
	return &Error{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func Schema(field, format string, args ...any) *Error {
	return New(ESchema, field, format, args...)
}

func Ovf(field string, a, b any) *Error {
	return &Error{Code: EOvf, Field: field, Message: "integer conversion or accumulation overflow", Operands: []any{a, b}}
}

func Conflict(field string, set []string, format string, args ...any) *Error {
	return &Error{Code: EConflict, Field: field, Set: append([]string(nil), set...), Message: fmt.Sprintf(format, args...)}
}

func Diff(round int, field string, expected, actual any) *Error {
	return &Error{
		Code: EDiff, Round: round, Field: field, FirstDiff: field,
		Message:  fmt.Sprintf("snapshot mismatch at round %d field %s", round, field),
		Expected: expected, Actual: actual,
	}
}

func IsCode(err error, code Code) bool {
	var target *Error
	if errors.As(err, &target) {
		return target.Code == code
	}
	return false
}

func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
