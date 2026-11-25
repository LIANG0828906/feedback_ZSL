package model

import (
	"sort"
	"strconv"
	"strings"
)

type Labels map[string]string

func (l Labels) SortedKeys() []string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (l Labels) CanonicalKey() string {
	var b strings.Builder
	for _, k := range l.SortedKeys() {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
		b.WriteByte(';')
	}
	return b.String()
}

func (l Labels) Fingerprint() string {
	return "fp_" + strconv.FormatUint(hashLabels(l.CanonicalKey()), 16)
}

func (l Labels) Clone() Labels {
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (l Labels) GroupKey(keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, "|")
}

func hashLabels(canonical string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(canonical); i++ {
		h ^= uint64(canonical[i])
		h *= prime64
	}
	return h
}

type Clock struct {
	BaseEpochMS int64 `json:"base_epoch_ms"`
	StepMS      int64 `json:"step_ms"`
	Steps       int   `json:"steps"`
}

func (c Clock) TimeMS(t int) int64 {
	return c.BaseEpochMS + int64(t)*c.StepMS
}

func (c Clock) IndexAtOrBefore(epochMS int64) int {
	if epochMS < c.BaseEpochMS {
		return -1
	}
	return int((epochMS - c.BaseEpochMS) / c.StepMS)
}

type NumericConfig struct {
	Scale          int64  `json:"scale"`
	Bits           int    `json:"bits"`
	OverflowPolicy string `json:"overflow_policy"`
}

type Budget struct {
	Workers       int `json:"workers"`
	PerRuleBuffer int `json:"per_rule_buffer"`
}

type Sample struct {
	Index     int   `json:"index"`
	AtMS      int64 `json:"at_ms"`
	Raw       int64 `json:"raw"`
	Overwrote bool  `json:"overwrote,omitempty"`
}

type Series struct {
	ID      string   `json:"id"`
	Metric  string   `json:"metric"`
	Labels  Labels   `json:"labels"`
	Samples []Sample `json:"samples"`
}

type Matcher struct {
	Label string `json:"label"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

func (m Matcher) Matches(l Labels) bool {
	v, ok := l[m.Label]
	switch m.Op {
	case "=", "==", "eq":
		return ok && v == m.Value
	case "!=", "ne":
		return !ok || v != m.Value
	default:
		return false
	}
}

type Predicate struct {
	Metric      string    `json:"metric"`
	Matchers    []Matcher `json:"matchers,omitempty"`
	Op          string    `json:"op"`
	Threshold   int64     `json:"threshold_raw"`
	AddRaw      *int64    `json:"add_raw,omitempty"`
	FreshnessMS int64     `json:"freshness_ms"`
}

type Rule struct {
	ID           string            `json:"id"`
	Order        int               `json:"order"`
	Group        string            `json:"group,omitempty"`
	Priority     int               `json:"priority"`
	Predicate    Predicate         `json:"predicate"`
	ForMS        int64             `json:"for_ms"`
	KeepFiringMS int64             `json:"keep_firing_ms"`
	LabelRewrite map[string]string `json:"label_rewrite,omitempty"`
	GroupBy      []string          `json:"group_by,omitempty"`
	Receiver     string            `json:"receiver,omitempty"`
	NoDataAction string            `json:"no_data_action,omitempty"`
}

type Receiver struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type Route struct {
	ID         string    `json:"id"`
	Order      int       `json:"order"`
	Matchers   []Matcher `json:"matchers,omitempty"`
	Receiver   string    `json:"receiver"`
	Continue   bool      `json:"continue,omitempty"`
	BasisLabel string    `json:"basis_label,omitempty"`
}

type InhibitionRule struct {
	ID             string    `json:"id"`
	SourceMatchers []Matcher `json:"source_matchers"`
	TargetMatchers []Matcher `json:"target_matchers"`
	EqualLabels    []string  `json:"equal_labels,omitempty"`
	Priority       int       `json:"priority,omitempty"`
}

type SilenceWindow struct {
	ID       string    `json:"id"`
	Matchers []Matcher `json:"matchers"`
	StartT   int       `json:"start_t"`
	EndT     int       `json:"end_t"`
	Comment  string    `json:"comment,omitempty"`
}

type SuppressionWindow struct {
	ID           string `json:"id"`
	InhibitionID string `json:"inhibition_id"`
	StartT       int    `json:"start_t"`
	EndT         int    `json:"end_t"`
	SourceRuleID string `json:"source_rule_id,omitempty"`
	Comment      string `json:"comment,omitempty"`
}

type EventBundle struct {
	Silences           []SilenceWindow     `json:"silences,omitempty"`
	SuppressionWindows []SuppressionWindow `json:"suppression_windows,omitempty"`
}

type Policy struct {
	ReasonPriority []string `json:"reason_priority"`
	NoDataAction   string   `json:"no_data_action"`
}

type Project struct {
	ProjectID    string           `json:"project_id"`
	RuleVersion  string           `json:"rule_version"`
	SourceSchema string           `json:"source_schema,omitempty"`
	Clock        Clock            `json:"clock"`
	Numeric      NumericConfig    `json:"numeric"`
	Budget       Budget           `json:"budget"`
	Series       []*Series        `json:"series"`
	Rules        []*Rule          `json:"rules"`
	Receivers    []Receiver       `json:"receivers"`
	Routes       []Route          `json:"routes,omitempty"`
	Inhibitions  []InhibitionRule `json:"inhibitions,omitempty"`
	Silences     []SilenceWindow  `json:"silences,omitempty"`
	Policy       Policy           `json:"policy"`
}

func (p *Project) ReceiverNames() map[string]bool {
	out := make(map[string]bool, len(p.Receivers))
	for _, r := range p.Receivers {
		out[r.Name] = true
	}
	return out
}
