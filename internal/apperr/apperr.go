package apperr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Code is a stable business error code. Callers may compare it without
// depending on human-readable error text.
type Code string

const (
	SchemaVersion       Code = "E_SCHEMA_VERSION"
	SchemaField         Code = "E_SCHEMA_FIELD"
	SchemaRequired      Code = "E_SCHEMA_REQUIRED"
	SchemaType          Code = "E_SCHEMA_TYPE"
	SchemaUnit          Code = "E_SCHEMA_UNIT"
	SchemaCacheKey      Code = "E_SCHEMA_CACHE_KEY"
	LayerMetadata       Code = "E_LAYER_METADATA"
	InputConflict       Code = "E_INPUT_CONFLICT"
	GraphMissingRef     Code = "E_GRAPH_MISSING_REF"
	GraphCycle          Code = "E_GRAPH_CYCLE"
	GraphShareConflict  Code = "E_GRAPH_SHARE_CONFLICT"
	GraphShareMissing   Code = "E_GRAPH_SHARE_MISSING"
	GraphDuplicate      Code = "E_GRAPH_DUPLICATE"
	BudgetThreads       Code = "E_BUDGET_THREADS"
	BudgetMemory        Code = "E_BUDGET_MEMORY"
	BudgetLatency       Code = "E_BUDGET_LATENCY"
	BackpressureQueue   Code = "E_BACKPRESSURE_QUEUE"
	CleanupConflict     Code = "E_CLEANUP_CONFLICT"
	CheckpointVersion   Code = "E_CHECKPOINT_VERSION"
	CheckpointTruncated Code = "E_CHECKPOINT_TRUNCATED"
	CheckpointIntegrity Code = "E_CHECKPOINT_INTEGRITY"
	CheckpointIO        Code = "E_CHECKPOINT_IO"
	RecoveryMismatch    Code = "E_RECOVERY_MISMATCH"
	Interrupted         Code = "E_INTERRUPTED"
	Internal            Code = "E_INTERNAL"
)

// Error keeps enough machine-readable context for a caller to identify the
// affected manifest location, steps and layers.
type Error struct {
	Code    Code
	Op      string
	Path    string
	Steps   []int
	Layers  []string
	Detail  string
	Wrapped error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Op != "" {
		b.WriteString(" op=")
		b.WriteString(e.Op)
	}
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if len(e.Steps) > 0 {
		steps := append([]int(nil), e.Steps...)
		sort.Ints(steps)
		b.WriteString(" steps=")
		for i, step := range steps {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", step)
		}
	}
	if len(e.Layers) > 0 {
		layers := append([]string(nil), e.Layers...)
		sort.Strings(layers)
		b.WriteString(" layers=")
		b.WriteString(strings.Join(layers, ","))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Wrapped != nil {
		b.WriteString(": ")
		b.WriteString(e.Wrapped.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Wrapped }

func New(code Code, op, path, detail string) *Error {
	return &Error{Code: code, Op: op, Path: path, Detail: detail}
}

func WithSteps(err *Error, steps ...int) *Error {
	if err == nil {
		return nil
	}
	err.Steps = append(err.Steps, steps...)
	return err
}

func WithLayers(err *Error, layers ...string) *Error {
	if err == nil {
		return nil
	}
	err.Layers = append(err.Layers, layers...)
	return err
}

func Wrap(code Code, op, path string, err error) *Error {
	return &Error{Code: code, Op: op, Path: path, Wrapped: err}
}

func IsCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func CodeOf(err error) Code {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}
