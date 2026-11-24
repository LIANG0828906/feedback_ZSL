package acceptance

import (
	"time"

	"cadpipe/internal/cad"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail any    `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Report struct {
	OK          bool      `json:"ok"`
	Seed        int64     `json:"seed"`
	GeneratedAt time.Time `json:"generated_at"`
	Checks      []Check   `json:"checks"`
}

func (r *Report) add(name string, fn func() (any, error)) {
	detail, err := fn()
	check := Check{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		check.Error = err.Error()
		if ce, ok := cad.AsError(err); ok {
			check.Code = ce.Code
		}
		r.OK = false
	}
	r.Checks = append(r.Checks, check)
}
