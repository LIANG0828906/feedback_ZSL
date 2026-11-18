package model

import (
	"fmt"
	"net/http"
)

type Change struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Item struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type PublishRequest struct {
	Changes []Change `json:"changes"`
}

type PublishResponse struct {
	Namespace   string   `json:"namespace"`
	Version     int64    `json:"version"`
	ChangedKeys []string `json:"changed_keys"`
	Atomic      bool     `json:"atomic"`
}

type WatchResponse struct {
	Namespace   string `json:"namespace"`
	BaseVersion int64  `json:"base_version"`
	Version     int64  `json:"version"`
	Changed     bool   `json:"changed"`
	Changes     []Item `json:"changes"`
}

type CurrentResponse struct {
	Namespace string `json:"namespace"`
	Version   int64  `json:"version"`
}

type SnapshotResponse struct {
	Namespace    string            `json:"namespace"`
	Version      int64             `json:"version"`
	Config       map[string]string `json:"config"`
	ItemVersions map[string]int64  `json:"item_versions"`
}

type ClientReport struct {
	ClientID            string  `json:"client_id"`
	Namespace           string  `json:"namespace"`
	KnownVersion        int64   `json:"known_version"`
	AppliedVersion      int64   `json:"applied_version"`
	LastResponseVersion int64   `json:"last_response_version"`
	StaleResponses      uint64  `json:"stale_responses"`
	AppliedHistory      []int64 `json:"applied_history"`
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewAPIError(code, message string, status int) *APIError {
	return &APIError{Code: code, Message: message, HTTPStatus: status}
}

var (
	ErrVersionAhead = NewAPIError(
		"E_VERSION_AHEAD",
		"known version is greater than the current latest version",
		http.StatusConflict,
	)
	ErrNamespaceNotFound = NewAPIError(
		"E_NAMESPACE_NOT_FOUND",
		"namespace does not exist",
		http.StatusNotFound,
	)
	ErrVersionNotFound = NewAPIError(
		"E_VERSION_NOT_FOUND",
		"requested snapshot version does not exist",
		http.StatusNotFound,
	)
	ErrValidation = NewAPIError(
		"E_VALIDATION",
		"configuration validation failed",
		http.StatusBadRequest,
	)
	ErrBadRequest = NewAPIError(
		"E_BAD_REQUEST",
		"request is invalid",
		http.StatusBadRequest,
	)
	ErrMergePending = NewAPIError(
		"E_MERGE_PENDING",
		"a coalesced notification is still pending",
		http.StatusServiceUnavailable,
	)
)
