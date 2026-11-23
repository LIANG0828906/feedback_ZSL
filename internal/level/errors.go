package level

import "fmt"

type ErrCode string

const (
	ESchema  ErrCode = "E_SCHEMA"
	EPara    ErrCode = "E_PARA"
	EHist    ErrCode = "E_HIST"
	EBudget  ErrCode = "E_BUDGET"
	EConn    ErrCode = "E_CONN"
	EDiff    ErrCode = "E_DIFF"
	EDiverge ErrCode = "E_DIVERGE"
)

// Error carries a stable machine-readable code plus locating context
// (field name, partition id, op sequence, diff item) for callers.
type Error struct {
	Code        ErrCode        `json:"code"`
	Message     string         `json:"message"`
	Field       string         `json:"field,omitempty"`
	Partition   int            `json:"partition"`
	OpSeq       int            `json:"op_seq"`
	Item        string         `json:"item,omitempty"`
	Difficulty  int64          `json:"difficulty,omitempty"`
	BudgetMin   int64          `json:"budget_min,omitempty"`
	BudgetMax   int64          `json:"budget_max,omitempty"`
	Unreachable []string       `json:"unreachable_targets,omitempty"`
	Path        []int          `json:"path_partitions,omitempty"`
	Report      *DivergeReport `json:"report,omitempty"`
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Code, e.Message)
	if e.Field != "" {
		s += fmt.Sprintf(" (field=%s)", e.Field)
	}
	if e.Partition >= 0 {
		s += fmt.Sprintf(" (partition=%d)", e.Partition)
	}
	if e.OpSeq >= 0 {
		s += fmt.Sprintf(" (op_seq=%d)", e.OpSeq)
	}
	if e.Item != "" {
		s += fmt.Sprintf(" (item=%s)", e.Item)
	}
	return s
}

func CodeOf(err error) ErrCode {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func schemaErr(field, msg string) *Error {
	return &Error{Code: ESchema, Message: msg, Field: field, Partition: -1, OpSeq: -1}
}

func paraErr(field string, partition int, msg string) *Error {
	return &Error{Code: EPara, Message: msg, Field: field, Partition: partition, OpSeq: -1}
}

func histErr(opSeq int, msg string) *Error {
	return &Error{Code: EHist, Message: msg, Partition: -1, OpSeq: opSeq}
}

func budgetErr(partition int, difficulty, min, max int64) *Error {
	return &Error{
		Code: EBudget, Partition: partition, OpSeq: -1,
		Message:    "difficulty budget cannot be satisfied within iteration limit",
		Difficulty: difficulty, BudgetMin: min, BudgetMax: max,
	}
}

func connErr(targets []string, path []int) *Error {
	return &Error{
		Code: EConn, Partition: -1, OpSeq: -1,
		Message:     "spawn cannot reach targets after boundary repair",
		Unreachable: targets, Path: path,
	}
}

func diffErr(opSeq, partition int, item string) *Error {
	return &Error{
		Code: EDiff, Partition: partition, OpSeq: opSeq,
		Message: "paths diverge: first mismatch located", Item: item,
	}
}

func divergeErr(rep *DivergeReport) *Error {
	return &Error{
		Code: EDiverge, Partition: rep.FirstPartition, OpSeq: -1,
		Message: "preview and precise results diverge beyond tolerance",
		Item:    rep.Item, Report: rep,
	}
}
