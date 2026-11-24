package editorpick

import "fmt"

const (
	ESchema    = "E_SCHEMA"
	ECycle     = "E_CYCLE"
	ERef       = "E_REF"
	EAmbiguous = "E_AMBIG"
	ETxn       = "E_TXN"
	EHash      = "E_HASH"
	EVersion   = "E_VERSION"
)

// DomainError carries a stable product error code and the affected node/object.
type DomainError struct {
	Code     string `json:"code"`
	NodeID   string `json:"node_id,omitempty"`
	ObjectID string `json:"object_id,omitempty"`
	QueryID  string `json:"query_id,omitempty"`
	Message  string `json:"message"`
}

func (e *DomainError) Error() string {
	id := e.NodeID
	if id == "" {
		id = e.ObjectID
	}
	if id != "" {
		return fmt.Sprintf("%s[%s]: %s", e.Code, id, e.Message)
	}
	if e.QueryID != "" {
		return fmt.Sprintf("%s[query=%s]: %s", e.Code, e.QueryID, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newSchema(nodeID, format string, args ...any) *DomainError {
	return &DomainError{Code: ESchema, NodeID: nodeID, Message: fmt.Sprintf(format, args...)}
}

func newRef(nodeID, format string, args ...any) *DomainError {
	return &DomainError{Code: ERef, NodeID: nodeID, Message: fmt.Sprintf(format, args...)}
}

func newCycle(nodeID string) *DomainError {
	return &DomainError{Code: ECycle, NodeID: nodeID, Message: "parent reference contains a cycle"}
}

func newTxn(format string, args ...any) *DomainError {
	return &DomainError{Code: ETxn, Message: fmt.Sprintf(format, args...)}
}
