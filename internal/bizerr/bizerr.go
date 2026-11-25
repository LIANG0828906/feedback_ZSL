package bizerr

import (
	"errors"
	"fmt"
)

const (
	ESchemaUnsupported      = "E_SCHEMA_UNSUPPORTED"
	ESchemaFieldConflict    = "E_FIELD_CONFLICT"
	ESchemaFieldInvalid     = "E_FIELD_INVALID"
	EBudgetScaleUnsupported = "E_BUDGET_SCALE_UNSUPPORTED"
	EChainCycle             = "E_CHAIN_CYCLE"
	ECheckpointTruncated    = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersion      = "E_CHECKPOINT_VERSION"
	ESnapshotMismatch       = "E_SNAPSHOT_MISMATCH"
	EBufferFull             = "E_BUFFER_FULL"
	EThreadBudget           = "E_THREAD_BUDGET"
	ENotFound               = "E_NOT_FOUND"
	EInterrupt              = "E_INTERRUPTED_BEFORE_COMMIT"
	EInvalidArgument        = "E_INVALID_ARGUMENT"
	ESQLite                 = "E_SQLITE"
)

type Error struct {
	Code    string         `json:"code"`
	Path    string         `json:"path,omitempty"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func At(err error, path string) error {
	be := As(err)
	if be == nil {
		return err
	}
	return be.At(path)
}

func (e *Error) At(path string) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Path = path
	return &cp
}

func WithContext(err error, key string, value any) error {
	be := As(err)
	if be == nil {
		return err
	}
	return be.WithContext(key, value)
}

func (e *Error) WithContext(key string, value any) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	if cp.Context == nil {
		cp.Context = map[string]any{}
	} else {
		next := make(map[string]any, len(cp.Context)+1)
		for k, v := range cp.Context {
			next[k] = v
		}
		cp.Context = next
	}
	cp.Context[key] = value
	return &cp
}

func AtError(err error, path string) error {
	if be := As(err); be != nil {
		return be.At(path)
	}
	return err
}

func ContextError(err error, key string, value any) error {
	if be := As(err); be != nil {
		return be.WithContext(key, value)
	}
	return err
}

func (e *Error) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

func CodeOf(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func As(err error) *Error {
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	return nil
}
