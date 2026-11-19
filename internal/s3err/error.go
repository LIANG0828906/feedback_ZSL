package s3err

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
)

type Error struct {
	Code     string
	Message  string
	Status   int
	Resource string
	Cause    error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func New(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

func Internal(err error) *Error {
	return &Error{Code: "InternalError", Message: "We encountered an internal error. Please try again.", Status: http.StatusInternalServerError, Cause: err}
}

func Wrap(code, message string, status int, err error) *Error {
	return &Error{Code: code, Message: message, Status: status, Cause: err}
}

var (
	AccessDenied = func() *Error {
		return New("AccessDenied", "Access Denied.", http.StatusForbidden)
	}
	SignatureDoesNotMatch = func() *Error {
		return New("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden)
	}
	InvalidAccessKeyID = func() *Error {
		return New("InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.", http.StatusForbidden)
	}
	NoSuchKey = func() *Error {
		return New("NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
	}
	NoSuchVersion = func() *Error {
		return New("NoSuchVersion", "The specified version does not exist.", http.StatusNotFound)
	}
	NoSuchUpload = func() *Error {
		return New("NoSuchUpload", "The specified multipart upload does not exist.", http.StatusNotFound)
	}
	NoSuchBucket = func() *Error {
		return New("NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
	}
	BucketAlreadyExists = func() *Error {
		return New("BucketAlreadyExists", "The requested bucket name is not available.", http.StatusConflict)
	}
	BucketNotEmpty = func() *Error {
		return New("BucketNotEmpty", "The bucket you tried to delete is not empty.", http.StatusConflict)
	}
	InvalidPart = func() *Error {
		return New("InvalidPart", "One or more of the specified parts could not be found or had an invalid length or ETag.", http.StatusBadRequest)
	}
	InvalidPartOrder = func() *Error {
		return New("InvalidPartOrder", "The list of parts was not in ascending order.", http.StatusBadRequest)
	}
	InvalidRequest = func() *Error {
		return New("InvalidRequest", "The request is invalid.", http.StatusBadRequest)
	}
	MalformedXML = func() *Error {
		return New("MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest)
	}
	EntityTooLarge = func() *Error {
		return New("EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", http.StatusRequestEntityTooLarge)
	}
	MethodNotAllowed = func() *Error {
		return New("MethodNotAllowed", "The specified method is not allowed against this resource.", http.StatusMethodNotAllowed)
	}
	NotImplemented = func() *Error {
		return New("NotImplemented", "A header or operation you provided implies functionality that is not implemented.", http.StatusNotImplemented)
	}
)

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

func RequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func Write(w http.ResponseWriter, requestID, hostID, resource string, e *Error) {
	if e == nil {
		e = Internal(nil)
	}
	if e.Status == 0 {
		e.Status = http.StatusInternalServerError
	}
	if requestID == "" {
		requestID = RequestID()
	}
	if hostID == "" {
		hostID = RequestID()
	}
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", hostID)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(e.Status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(errorBody{
		Code:      e.Code,
		Message:   e.Message,
		Resource:  resource,
		RequestID: requestID,
		HostID:    hostID,
	})
}
