package model

import (
	"encoding/json"
	"fmt"
)

// BizError is the stable, machine-readable error contract used by the loader,
// engine, demo, and fault-injection path.
type BizError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
	Err     error          `json:"-"`
}

func (e *BizError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *BizError) Unwrap() error { return e.Err }

func (e *BizError) JSON() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func Err(code, message string, context map[string]any) *BizError {
	return &BizError{Code: code, Message: message, Context: context}
}

func Wrap(code, message string, cause error, context map[string]any) *BizError {
	return &BizError{Code: code, Message: message, Context: context, Err: cause}
}

func IsCode(err error, code string) bool {
	for err != nil {
		if be, ok := err.(*BizError); ok {
			if be.Code == code {
				return true
			}
			err = be.Err
			continue
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}

const (
	EInvalidJSON               = "E_INVALID_JSON"
	EInvalidSchema             = "E_INVALID_SCHEMA"
	ESchemaVersion             = "E_SCHEMA_VERSION"
	EAmbiguousField            = "E_AMBIGUOUS_FIELD"
	EUnsupportedAttributeKind  = "E_UNSUPPORTED_ATTRIBUTE_KIND"
	EBadAttributeScale         = "E_BAD_ATTRIBUTE_SCALE"
	EUnsupportedUnit           = "E_UNSUPPORTED_UNIT"
	EDuplicateID               = "E_DUPLICATE_ID"
	EMissingVertex             = "E_MISSING_VERTEX"
	EInvalidEdge               = "E_INVALID_EDGE"
	EOverlapShard              = "E_OVERLAP_SHARD"
	EEmptyShardKeyspace        = "E_EMPTY_SHARD_KEYSPACE"
	EKeyOutsideShard           = "E_KEY_OUTSIDE_SHARD"
	EShardNotFound             = "E_SHARD_NOT_FOUND"
	EInvalidOperation          = "E_INVALID_OPERATION"
	EInvalidQuery              = "E_INVALID_QUERY"
	EThreadBudget              = "E_THREAD_BUDGET"
	EBufferBudget              = "E_BUFFER_BUDGET"
	ECheckpointTruncated       = "E_CHECKPOINT_TRUNCATED"
	ECheckpointVersionMismatch = "E_CHECKPOINT_VERSION_MISMATCH"
	ECheckpointCorrupt         = "E_CHECKPOINT_CORRUPT"
	ECheckpointIO              = "E_CHECKPOINT_IO"
	ENoCommit                  = "E_NO_COMMIT"
	EValidationFailed          = "E_VALIDATION_FAILED"
)
