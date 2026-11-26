package vpf

import "fmt"

type LossItem struct {
	Code          ErrorCode `json:"code"`
	Kind          string    `json:"kind"`
	FieldPath     string    `json:"field_path"`
	SourceID      string    `json:"source_id,omitempty"`
	Disposition   string    `json:"disposition"`
	PrecisionLoss bool      `json:"precision_loss"`
	Detail        string    `json:"detail,omitempty"`
}

type LossReport struct {
	Code  ErrorCode  `json:"code"`
	Items []LossItem `json:"items"`
}

func (r *LossReport) Error() string {
	if r == nil {
		return ""
	}
	precisionLosses := 0
	for _, item := range r.Items {
		if item.PrecisionLoss {
			precisionLosses++
		}
	}
	return fmt.Sprintf("%s: %d unexpressible-feature disposition(s), %d precision loss(es)", CodeLossy, len(r.Items), precisionLosses)
}

func (r *LossReport) asError() *Error {
	if r == nil || len(r.Items) == 0 {
		return nil
	}
	return &Error{Code: CodeLossy, Message: r.Error(), Details: r.Items}
}
