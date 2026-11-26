package model

import (
	"fmt"
	"sort"
	"strings"
)

type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

type PolicyState string

const (
	StateActive   PolicyState = "active"
	StateRevoked  PolicyState = "revoked"
	StateExpired  PolicyState = "expired"
	StateReplaced PolicyState = "replaced"
	StateDeleted  PolicyState = "deleted"
)

type ChangeKind string

const (
	ChangeAdd              ChangeKind = "policy_add"
	ChangeDelete           ChangeKind = "policy_delete"
	ChangePriority         ChangeKind = "priority_adjust"
	ChangeSelector         ChangeKind = "selector_rewrite"
	ChangeAction           ChangeKind = "action_edit"
	ChangePort             ChangeKind = "port_edit"
	ChangeWindow           ChangeKind = "window_edit"
	ChangeReplace          ChangeKind = "policy_replace"
	ChangeWorkload         ChangeKind = "workload_edit"
	ChangeAdmissionReplace ChangeKind = "admission_replace"
)

type Workload struct {
	ID             string            `json:"id"`
	Namespace      string            `json:"namespace"`
	ServiceAccount string            `json:"service_account"`
	Labels         map[string]string `json:"labels"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Exited         bool              `json:"exited,omitempty"`
}

type Selector struct {
	Namespace       string            `json:"namespace,omitempty"`
	MatchLabels     map[string]string `json:"match_labels,omitempty"`
	ServiceAccounts []string          `json:"service_accounts,omitempty"`
	WorkloadRefs    []string          `json:"workload_refs,omitempty"`
}

type Policy struct {
	ID         string      `json:"id"`
	Namespace  string      `json:"namespace"`
	Source     Selector    `json:"source"`
	PortFrom   int         `json:"port_from"`
	PortTo     int         `json:"port_to"`
	Action     Action      `json:"action"`
	Priority   int         `json:"priority"`
	Order      int         `json:"order"`
	StartSeq   int64       `json:"start_seq"`
	EndSeq     int64       `json:"end_seq"`
	State      PolicyState `json:"state"`
	Revision   int         `json:"revision"`
	Replaces   string      `json:"replaces,omitempty"`
	ReplacedBy string      `json:"replaced_by,omitempty"`
}

type ConnectionAttempt struct {
	ID               string `json:"id"`
	Seq              int64  `json:"seq"`
	Order            int    `json:"order"`
	SourceWorkloadID string `json:"source_workload_id"`
	TargetWorkloadID string `json:"target_workload_id"`
	Port             int    `json:"port"`
	Protocol         string `json:"protocol"`
}

func (a ConnectionAttempt) FlowKey() string {
	return strings.Join([]string{a.SourceWorkloadID, a.TargetWorkloadID, fmt.Sprint(a.Port), a.Protocol}, "|")
}

type PolicyChange struct {
	ID                string     `json:"id"`
	Seq               int64      `json:"seq"`
	Order             int        `json:"order"`
	Kind              ChangeKind `json:"kind"`
	Namespace         string     `json:"namespace"`
	PolicyID          string     `json:"policy_id"`
	EditedFields      []string   `json:"edited_fields,omitempty"`
	Before            *Policy    `json:"before,omitempty"`
	After             *Policy    `json:"after,omitempty"`
	WorkloadBefore    *Workload  `json:"workload_before,omitempty"`
	WorkloadAfter     *Workload  `json:"workload_after,omitempty"`
	WorkloadID        string     `json:"workload_id,omitempty"`
	ReplacementRuleID string     `json:"replacement_rule_id,omitempty"`
}

type Event struct {
	Seq     int64              `json:"seq"`
	Order   int                `json:"order"`
	Attempt *ConnectionAttempt `json:"attempt,omitempty"`
	Change  *PolicyChange      `json:"change,omitempty"`
}

func (e Event) SortKey() (int64, int) {
	if e.Attempt != nil {
		return e.Attempt.Seq, e.Attempt.Order
	}
	if e.Change != nil {
		return e.Change.Seq, e.Change.Order
	}
	return e.Seq, e.Order
}

type Project struct {
	ID           string         `json:"id"`
	Seed         int64          `json:"seed"`
	SchemaSource string         `json:"schema_source,omitempty"`
	TickMS       int64          `json:"tick_ms"`
	DefaultDeny  bool           `json:"default_deny"`
	PolicyLimits map[string]int `json:"policy_limits,omitempty"`
	Workloads    []Workload     `json:"workloads"`
	Policies     []Policy       `json:"policies"`
	Timeline     []Event        `json:"timeline"`
}

func (p *Project) WorkloadByID(id string) (Workload, bool) {
	for _, w := range p.Workloads {
		if w.ID == id {
			return w, true
		}
	}
	return Workload{}, false
}

func (p *Project) PolicyByID(id string) (Policy, bool) {
	for _, pol := range p.Policies {
		if pol.ID == id {
			return pol, true
		}
	}
	return Policy{}, false
}

func (p *Project) SortedEvents() []Event {
	out := append([]Event(nil), p.Timeline...)
	sort.SliceStable(out, func(i, j int) bool {
		si, oi := out[i].SortKey()
		sj, oj := out[j].SortKey()
		if si != sj {
			return si < sj
		}
		return oi < oj
	})
	return out
}

func (p *Project) Attempts() []ConnectionAttempt {
	var out []ConnectionAttempt
	for _, ev := range p.SortedEvents() {
		if ev.Attempt != nil {
			out = append(out, *ev.Attempt)
		}
	}
	return out
}

func CloneProject(in *Project) *Project {
	if in == nil {
		return nil
	}
	b, _ := JSONMarshal(in)
	var out Project
	_ = JSONUnmarshal(b, &out)
	return &out
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func SortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
