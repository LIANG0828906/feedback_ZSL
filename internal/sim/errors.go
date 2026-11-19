package sim

import "fmt"

const (
	CodeConfigDuplicateID      = "DNS_CFG_DUPLICATE_ID"
	CodeConfigMissingRef       = "DNS_CFG_MISSING_REF"
	CodeConfigForwardCycle     = "DNS_CFG_FORWARD_CYCLE"
	CodeConfigDelegationCycle  = "DNS_CFG_DELEGATION_CYCLE"
	CodeConfigCNAMECycle       = "DNS_CFG_CNAME_CYCLE"
	CodeConfigInvalidRole      = "DNS_CFG_INVALID_ROLE"
	CodeConfigInvalidAddress   = "DNS_CFG_INVALID_ADDRESS"
	CodeConfigInvalidName      = "DNS_CFG_INVALID_NAME"
	CodeConfigInvalidType      = "DNS_CFG_INVALID_TYPE"
	CodeConfigInvalidValue     = "DNS_CFG_INVALID_VALUE"
	CodeConfigInvalidLimit     = "DNS_CFG_INVALID_LIMIT"
	CodeConfigInvalidFault     = "DNS_CFG_INVALID_FAULT"
	CodeConfigInvalidTopology  = "DNS_CFG_INVALID_TOPOLOGY"
	CodeBindFailed             = "DNS_BIND_FAILED"
	CodeInvalidRequest         = "DNS_INVALID_REQUEST"
	CodeUnknownSite            = "DNS_UNKNOWN_SITE"
	CodeUnknownDomain          = "DNS_UNKNOWN_DOMAIN"
	CodeInvalidType            = "DNS_INVALID_TYPE"
	CodeBusy                   = "DNS_BUSY"
	CodeQueueTimeout           = "DNS_QUEUE_TIMEOUT"
	CodeShuttingDown           = "DNS_SHUTTING_DOWN"
	CodeInternal               = "DNS_INTERNAL"
	CodeReferenceUnavailable   = "DNS_REFERENCE_UNAVAILABLE"
	CodeCacheEntryNotFound     = "DNS_CACHE_ENTRY_NOT_FOUND"
	CodeStaticMappingNotFound  = "DNS_STATIC_MAPPING_NOT_FOUND"
	CodeUnsupportedContentType = "DNS_UNSUPPORTED_CONTENT_TYPE"
	CodeMethodNotAllowed       = "DNS_METHOD_NOT_ALLOWED"
	CodeRequestBodyTooLarge    = "DNS_REQUEST_BODY_TOO_LARGE"
	CodeMalformedJSON          = "DNS_MALFORMED_JSON"
)

// BusinessError is returned for deterministic, user-facing failures. The code
// is the stable part of the contract; Message is intended for operators.
type BusinessError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *BusinessError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func bizError(code, field, format string, args ...any) *BusinessError {
	return &BusinessError{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

func asBusinessError(err error) *BusinessError {
	if err == nil {
		return nil
	}
	if be, ok := err.(*BusinessError); ok {
		return be
	}
	return &BusinessError{Code: CodeInternal, Message: err.Error()}
}
