package proxy

import (
	"encoding/json"
	"net/http"
)

const (
	CodeNoRoute                 = "NO_ROUTE"
	CodeProtocolHeaderTooLarge  = "PROTOCOL_HEADER_TOO_LARGE"
	CodeProtocolInvalidHeader   = "PROTOCOL_INVALID_HEADER"
	CodeProtocolMissingHeader   = "PROTOCOL_MISSING_REQUIRED_HEADER"
	CodeProtocolInvalidTrailer  = "PROTOCOL_INVALID_TRAILER"
	CodeProtocolInvalidRequest  = "PROTOCOL_INVALID_REQUEST"
	CodeRequestBodyTooLarge     = "REQUEST_BODY_TOO_LARGE"
	CodeDownstreamCanceled      = "DOWNSTREAM_CANCELED"
	CodeQueueTimeout            = "PRIORITY_QUEUE_TIMEOUT"
	CodeUpstreamUnavailable     = "UPSTREAM_UNAVAILABLE"
	CodeUpstreamReadFailed      = "UPSTREAM_READ_FAILED"
	CodeUpstreamWriteAmbiguous  = "UPSTREAM_WRITE_AMBIGUOUS"
	CodeUpstreamResponseAborted = "UPSTREAM_RESPONSE_ABORTED"
	CodeGatewayInternal         = "GATEWAY_INTERNAL_ERROR"
)

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeError(w http.ResponseWriter, status int, body errorBody) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Error-Code", body.Code)
	if body.Phase != "" {
		w.Header().Set("X-Gateway-Disconnect-Phase", body.Phase)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
