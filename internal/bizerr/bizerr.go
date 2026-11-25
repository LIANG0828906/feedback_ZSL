package bizerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const (
	CodeSchemaUnsupported   = "E_SCHEMA_UNSUPPORTED"
	CodeSchemaAmbiguous     = "E_SCHEMA_AMBIGUOUS"
	CodeSchemaInvalid       = "E_SCHEMA_INVALID"
	CodeDuplicateSub        = "E_DUP_SUBSCRIPTION"
	CodeDuplicateCredential = "E_DUP_CREDENTIAL"
	CodeDuplicateRef        = "E_DUP_REF"
	CodeMissingCredential   = "E_MISSING_CREDENTIAL"
	CodeMissingSubscription = "E_MISSING_SUBSCRIPTION"
	CodeNegativeTTL         = "E_NEG_TTL"
	CodeTTLInvalid          = "E_TTL_INVALID"
	CodeSubCycle            = "E_SUB_CYCLE"
	CodeBatchLimit          = "E_BATCH_LIMIT"
	CodeBatchGap            = "E_BATCH_GAP"
	CodeVersionRegression   = "E_VERSION_REGRESSION"
	CodeWatermarkMissing    = "E_WATERMARK_MISSING"
	CodeGateRejected        = "E_GATE_REJECTED"
	CodeGateNoFallback      = "E_GATE_NO_FALLBACK"
	CodeWorkerBudget        = "E_WORKER_BUDGET"
	CodeBufferBudget        = "E_BUFFER_BUDGET"
	CodeCheckpointTruncated = "E_CHECKPOINT_TRUNCATED"
	CodeCheckpointCorrupt   = "E_CHECKPOINT_CORRUPT"
	CodeManifestMismatch    = "E_MANIFEST_MISMATCH"
	CodeNumericOverflow     = "E_NUMERIC_OVERFLOW"
	CodeInvalidRotation     = "E_INVALID_ROTATION"
	CodeInternal            = "E_INTERNAL"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
	Cause   error          `json:"-"`
}

func New(code, message string, context map[string]any) *Error {
	return &Error{Code: code, Message: message, Context: context}
}

func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code, message string, cause error, context map[string]any) *Error {
	return &Error{Code: code, Message: message, Context: context, Cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	ctx := ""
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
		ctx = " (" + join(parts, ", ") + ")"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s%s: %v", e.Code, e.Message, ctx, e.Cause)
	}
	return fmt.Sprintf("%s: %s%s", e.Code, e.Message, ctx)
}

func (e *Error) Unwrap() error {
	return e.Cause
}

func (e *Error) MarshalJSON() ([]byte, error) {
	type alias Error
	return json.Marshal((*alias)(e))
}

func Code(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func IsCode(err error, code string) bool {
	return Code(err) == code
}

func join(parts []string, sep string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += sep
		}
		out += part
	}
	return out
}
