package model

import (
	"fmt"
	"hash/fnv"
)

const (
	SchemaV1 = "gray/v1"
	SchemaV2 = "gray/v2"
	SchemaV3 = "gray/v3"

	CanonicalBasis = 10000

	SideBaseline = "baseline"
	SideCanary   = "canary"
)

type BizError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

func (e *BizError) Error() string {
	if len(e.Context) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Context)
}

func NewError(code, message string) *BizError {
	return &BizError{Code: code, Message: message, Context: map[string]any{}}
}

func (e *BizError) With(key string, value any) *BizError {
	if e.Context == nil {
		e.Context = map[string]any{}
	}
	e.Context[key] = value
	return e
}

func AsBizError(err error) (*BizError, bool) {
	if err == nil {
		return nil, false
	}
	if be, ok := err.(*BizError); ok {
		return be, true
	}
	return nil, false
}

type Service struct {
	Name         string   `json:"name"`
	Baseline     string   `json:"baseline"`
	Canary       string   `json:"canary"`
	Dependencies []string `json:"dependencies,omitempty"`
}

type Match struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type Rule struct {
	ID         string `json:"id"`
	RuleNo     int    `json:"rule_no"`
	Service    string `json:"service"`
	Priority   int    `json:"priority"`
	Order      int    `json:"order"`
	Match      Match  `json:"match"`
	WeightBPS  int    `json:"weight_bps"`
	DeclaredIn string `json:"declared_in,omitempty"`
}

type SamplePolicy struct {
	Present   bool   `json:"present"`
	Modulus   uint64 `json:"modulus"`
	Remainder uint64 `json:"remainder"`
}

type MirrorPolicy struct {
	Service  string        `json:"service"`
	RuleID   string        `json:"rule_id,omitempty"`
	RatioBPS int           `json:"ratio_bps"`
	Sample   *SamplePolicy `json:"sample,omitempty"`
}

type Project struct {
	SchemaVersion      string         `json:"schema_version"`
	EngineeringVersion string         `json:"engineering_version"`
	Name               string         `json:"name"`
	TickIntervalMS     int64          `json:"tick_interval_ms"`
	Services           []Service      `json:"services"`
	Rules              []Rule         `json:"rules"`
	Mirrors            []MirrorPolicy `json:"mirrors,omitempty"`
	SourceFormat       string         `json:"source_format"`
}

type Request struct {
	Sequence    int               `json:"-"`
	Tick        int               `json:"tick"`
	RequestID   string            `json:"request_id"`
	Service     string            `json:"service"`
	Key         string            `json:"key"`
	Labels      map[string]string `json:"labels,omitempty"`
	ShadowFault string            `json:"shadow_fault,omitempty"`
}

func HashUint32(seed, key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}

func StableBucket(seed, key string) int {
	return int(HashUint32(seed, key) % uint32(CanonicalBasis))
}

func StableSample(seed, key string, modulus, remainder uint64) bool {
	if modulus <= 1 {
		return true
	}
	return uint64(HashUint32(seed, key))%modulus == remainder%modulus
}
