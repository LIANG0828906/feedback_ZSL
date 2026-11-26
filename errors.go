package leasekit

import (
	"errors"
	"fmt"
)

const (
	CodeOK = "OK"

	CodeDuplicateHolder       = "E_DUPLICATE_HOLDER"
	CodeDuplicateResource     = "E_DUPLICATE_RESOURCE"
	CodeDuplicateOperation    = "E_DUPLICATE_OPERATION"
	CodeUnknownHolder         = "E_UNKNOWN_HOLDER"
	CodeUnknownResource       = "E_UNKNOWN_RESOURCE"
	CodeInvalidSchema         = "E_INVALID_SCHEMA"
	CodeSchemaVersionMismatch = "E_SCHEMA_VERSION_MISMATCH"
	CodeFieldConflict         = "E_FIELD_CONFLICT"
	CodeFieldOutOfRange       = "E_FIELD_OUT_OF_RANGE"
	CodeInvalidDuration       = "E_INVALID_DURATION"
	CodeTimeConversion        = "E_TIME_CONVERSION"
	CodeCountOverflow         = "E_COUNT_OVERFLOW"

	CodeRenewTooEarly       = "E_RENEW_TOO_EARLY"
	CodeRenewWindowClosed   = "E_RENEW_WINDOW_CLOSED"
	CodeRenewExpired        = "E_RENEW_EXPIRED"
	CodeRenewLimitReached   = "E_RENEW_LIMIT_REACHED"
	CodeCompetitionLost     = "E_COMPETITION_LOST"
	CodeResourceHeld        = "E_RESOURCE_HELD"
	CodeAttemptsExhausted   = "E_ATTEMPTS_EXHAUSTED"
	CodeWorkerBudgetOOR     = "E_WORKER_BUDGET_OOR"
	CodeAttemptBudgetOOR    = "E_ATTEMPT_BUDGET_OOR"
	CodeCheckpointTruncated = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointCorrupt   = "E_CHECKPOINT_CORRUPT"
	CodeNoActiveLease       = "E_NO_ACTIVE_LEASE"
)

type Error struct {
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Seq      int    `json:"seq,omitempty"`
	HolderID string `json:"holderId,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Err      error  `json:"-"`
}

func (e *Error) Error() string {
	where := e.Path
	if e.HolderID != "" {
		where = joinContext(where, "holder="+e.HolderID)
	}
	if e.Seq != 0 {
		where = joinContext(where, fmt.Sprintf("seq=%d", e.Seq))
	}
	if where != "" {
		where = " (" + where + ")"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s%s: %s", e.Code, where, e.Detail)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s%s: %v", e.Code, where, e.Err)
	}
	return e.Code + where
}

func (e *Error) Unwrap() error { return e.Err }

func newError(code, path string, err error, detail string) *Error {
	return &Error{Code: code, Path: path, Err: err, Detail: detail}
}

func validationError(code, path string, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Detail: fmt.Sprintf(format, args...)}
}

func withOp(err error, seq int, holderID string) error {
	var biz *Error
	if errors.As(err, &biz) {
		biz.Seq = seq
		biz.HolderID = holderID
		return biz
	}
	return &Error{Code: CodeInvalidSchema, Seq: seq, HolderID: holderID, Err: err}
}

func joinContext(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func AsError(err error) (*Error, bool) {
	var biz *Error
	if errors.As(err, &biz) {
		return biz, true
	}
	return nil, false
}
