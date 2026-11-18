package model

import (
	"errors"
	"net/http"
)

const (
	CodeOK                = "OK"
	CodeInvalidArgument   = "E_INVALID_ARGUMENT"
	CodeNotFound          = "E_NOT_FOUND"
	CodeConflict          = "E_CONFLICT"
	CodeInvalidTransition = "E_INVALID_TRANSITION"
	CodeObservationActive = "E_OBSERVATION_ACTIVE"
	CodeStaleBase         = "E_STALE_BASE"
	CodeReleaseActive     = "E_RELEASE_ACTIVE"
	CodeNotCanary         = "E_NOT_CANARY"
	CodeInternal          = "E_INTERNAL"
)

type BusinessError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
}

func (e *BusinessError) Error() string {
	return e.Code + ": " + e.Message
}

func Biz(code, message string) *BusinessError {
	status := http.StatusBadRequest
	switch code {
	case CodeNotFound:
		status = http.StatusNotFound
	case CodeConflict, CodeInvalidTransition, CodeObservationActive, CodeStaleBase, CodeReleaseActive, CodeNotCanary:
		status = http.StatusConflict
	case CodeInternal:
		status = http.StatusInternalServerError
	}
	return &BusinessError{Code: code, Message: message, HTTPStatus: status}
}

func AsBusiness(err error) *BusinessError {
	var biz *BusinessError
	if errors.As(err, &biz) {
		return biz
	}
	return Biz(CodeInternal, err.Error())
}
