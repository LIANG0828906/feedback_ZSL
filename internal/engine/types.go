package engine

import "meshpolicy/internal/model"

type Basis struct {
	PolicyID string       `json:"policy_id"`
	Priority int          `json:"priority"`
	Order    int          `json:"order"`
	Action   model.Action `json:"action"`
	State    string       `json:"state"`
	Port     string       `json:"port"`
	Reason   string       `json:"reason"`
}

type GateInfo struct {
	State             string `json:"state"`
	Code              string `json:"code,omitempty"`
	MarkerID          string `json:"marker_id,omitempty"`
	Generation        int    `json:"generation,omitempty"`
	ArrivalGeneration int    `json:"arrival_generation,omitempty"`
	Arrived           bool   `json:"arrived"`
	Acknowledged      bool   `json:"acknowledged"`
	AppliedPolicyID   string `json:"applied_policy_id,omitempty"`
	AppliedAllow      *bool  `json:"applied_allow,omitempty"`
	AppliedRevision   int    `json:"applied_revision,omitempty"`
	NewPolicyApplied  bool   `json:"new_policy_applied"`
}

type Decision struct {
	AttemptID        string   `json:"attempt_id"`
	Seq              int64    `json:"seq"`
	Order            int      `json:"order"`
	Namespace        string   `json:"namespace"`
	FlowKey          string   `json:"flow_key"`
	SourceWorkloadID string   `json:"source_workload_id"`
	TargetWorkloadID string   `json:"target_workload_id"`
	Allow            bool     `json:"allow"`
	ReasonCode       string   `json:"reason_code"`
	HitPolicyID      string   `json:"hit_policy_id,omitempty"`
	CandidateAllow   bool     `json:"candidate_allow"`
	CandidateHitID   string   `json:"candidate_hit_id,omitempty"`
	Basis            []Basis  `json:"basis"`
	AppliedPolicyID  string   `json:"applied_policy_id,omitempty"`
	AppliedRevision  int      `json:"applied_revision,omitempty"`
	Stale            bool     `json:"stale"`
	Gate             GateInfo `json:"gate"`
}

type MarkerRef struct {
	MarkerID       string `json:"marker_id"`
	ChangeID       string `json:"change_id"`
	SourcePolicyID string `json:"source_policy_id"`
	Generation     int    `json:"generation"`
	BornSeq        int64  `json:"born_seq"`
	ArrivalSeq     int64  `json:"arrival_seq"`
}

type MarkerLayer struct {
	MarkerID         string   `json:"marker_id"`
	ChangeID         string   `json:"change_id"`
	SourcePolicyID   string   `json:"source_policy_id"`
	Kind             string   `json:"kind"`
	Generation       int      `json:"generation"`
	BornSeq          int64    `json:"born_seq"`
	ArrivalSeq       int64    `json:"arrival_seq"`
	ReachedWorkloads []string `json:"reached_workloads"`
	NewArrivals      []string `json:"new_arrivals"`
	ArrivalSet       []string `json:"arrival_set"`
}

type PropagationReport struct {
	Layers    []MarkerLayer          `json:"layers"`
	ByAttempt map[string][]MarkerRef `json:"by_attempt"`
}

type MomentReport struct {
	Seq       int64         `json:"seq"`
	Decisions []Decision    `json:"decisions"`
	Arrivals  []MarkerLayer `json:"arrivals"`
}

type RunReport struct {
	ProjectID   string             `json:"project_id"`
	Schema      string             `json:"schema"`
	Decisions   []Decision         `json:"decisions"`
	Moments     []MomentReport     `json:"moments"`
	Propagation *PropagationReport `json:"propagation"`
	WorkerCount int                `json:"worker_count"`
	BufferPerNS int                `json:"buffer_per_ns"`
}
