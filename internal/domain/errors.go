package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Stable business codes. They are part of the diagnostic API and must not be
// renamed without a schema-version migration.
const (
	CodeOK = "OK"

	ESchemaUnknown          = "E_SCHEMA_UNKNOWN"
	ESchemaBadCombination   = "E_SCHEMA_BAD_COMBINATION"
	EInputDuplicateItem     = "E_INPUT_DUPLICATE_ITEM"
	EInputDuplicateTxn      = "E_INPUT_DUPLICATE_TXN"
	EInputDuplicateVersion  = "E_INPUT_DUPLICATE_VERSION"
	EInputBadDeleteRef      = "E_INPUT_BAD_DELETE_REF"
	EInputBadCreateRef      = "E_INPUT_BAD_CREATE_REF"
	EInputTimeOutOfRange    = "E_INPUT_TIME_OUT_OF_RANGE"
	EInputInvalidOperation  = "E_INPUT_INVALID_OPERATION"
	EInputInvalidIsolation  = "E_INPUT_INVALID_ISOLATION"
	EInputInvalidRef        = "E_INPUT_INVALID_REFERENCE"
	EInputInjectionRejected = "E_INPUT_INJECTION_REJECTED"

	EBackpressureWorkers  = "E_BACKPRESSURE_WORKER_BUDGET"
	EBackpressureItemBuf  = "E_BACKPRESSURE_ITEM_BUFFER"
	EBackpressureChainCap = "E_BACKPRESSURE_CHAIN_CAP"

	ECkptOpen            = "E_CHECKPOINT_OPEN"
	ECkptWrite           = "E_CHECKPOINT_WRITE"
	ECkptRead            = "E_CHECKPOINT_READ"
	ECkptCorrupt         = "E_CHECKPOINT_CORRUPT"
	ECkptTruncated       = "E_CHECKPOINT_TRUNCATED"
	ECkptVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	ESnapshotVersionBad  = "E_SNAPSHOT_VERSION_MISMATCH"
	EInterrupted         = "E_RUN_INTERRUPTED"
)

// Error is the only error type expected across package boundaries for
// predictable diagnostics. Context values are rendered in stable key order.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	if e.Path != "" {
		fmt.Fprintf(&b, " at %s", e.Path)
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	if len(e.Context) > 0 {
		keys := make([]string, 0, len(e.Context))
		for k := range e.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, e.Context[k]))
		}
		b.WriteString(" [")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString("]")
	}
	return b.String()
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message, Context: map[string]any{}}
}

func Errorf(code, format string, args ...any) *Error {
	return NewError(code, fmt.Sprintf(format, args...))
}

func (e *Error) At(path string) *Error {
	e.Path = path
	return e
}

func (e *Error) With(key string, value any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func (e *Error) WithContext(ctx map[string]any) *Error {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	for k, v := range ctx {
		e.Context[k] = v
	}
	return e
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}

func CodeOf(err error) string {
	if err == nil {
		return CodeOK
	}
	if de, ok := AsError(err); ok {
		return de.Code
	}
	return "E_INTERNAL"
}
