package leasekit

import (
	"encoding/json"
	"sort"
)

const (
	CanonicalSchemaVersion = "lease-project/canonical-v1"
	CheckpointVersion      = "lease-checkpoint/v1"

	BoundaryLowerInclusiveUpperExclusive BoundaryPolicy = "lower_inclusive_upper_exclusive"
	FailureRetainDeadline                FailurePolicy  = "retain_original_deadline"

	ModeExclusive LockMode = "exclusive"
	ModeShared    LockMode = "shared"

	OpAcquire OpKind = "acquire"
	OpRenew   OpKind = "renew"
	OpRelease OpKind = "release"
	OpAbandon OpKind = "abandon"
)

type Milli int64

type BoundaryPolicy string
type FailurePolicy string
type LockMode string
type OpKind string

type Project struct {
	SchemaVersion string         `json:"schemaVersion"`
	ProjectID     string         `json:"projectId"`
	Seed          int64          `json:"seed"`
	Boundary      BoundaryPolicy `json:"boundaryPolicy"`
	FailurePolicy FailurePolicy  `json:"failurePolicy"`
	Resources     []Resource     `json:"resources"`
	Holders       []Holder       `json:"holders"`
	Operations    []Operation    `json:"operations"`
}

type Resource struct {
	ID                 string   `json:"id"`
	Mode               LockMode `json:"mode"`
	LeaseTTLMS         Milli    `json:"leaseTtlMs"`
	RenewWindowMS      Milli    `json:"renewWindowMs"`
	MaxRenewals        int      `json:"maxRenewals"`
	MaxRenewDurationMS Milli    `json:"maxRenewDurationMs"`
	RenewAttemptBudget int      `json:"renewAttemptBudget,omitempty"`
}

type Holder struct {
	ID         string `json:"id"`
	ResourceID string `json:"resourceId"`
	Priority   int    `json:"priority"`
}

type Operation struct {
	Seq            int    `json:"seq"`
	HolderID       string `json:"holderId"`
	Kind           OpKind `json:"kind"`
	AtMS           Milli  `json:"atMs"`
	RenewForMS     Milli  `json:"renewForMs,omitempty"`
	Cohort         string `json:"cohort,omitempty"`
	ExpectedReason string `json:"expectedReason,omitempty"`
}

type SourceRef struct {
	HolderID string `json:"holderId,omitempty"`
	Seq      int    `json:"seq,omitempty"`
	Event    string `json:"event,omitempty"`
	AtMS     Milli  `json:"atMs,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Arbitration struct {
	Cohort          string `json:"cohort"`
	Candidates      []int  `json:"candidateSeqs"`
	WinnerSeq       int    `json:"winnerSeq"`
	Winner          string `json:"winnerHolderId"`
	Rule            string `json:"rule"`
	TemporalOverlap bool   `json:"temporalOverlap"`
	Basis           string `json:"basis"`
}

type OpResult struct {
	Seq           int          `json:"seq"`
	HolderID      string       `json:"holderId"`
	ResourceID    string       `json:"resourceId"`
	Kind          OpKind       `json:"kind"`
	AtMS          Milli        `json:"atMs"`
	Code          string       `json:"code"`
	Granted       bool         `json:"granted"`
	WindowOpenMS  Milli        `json:"windowOpenMs,omitempty"`
	WindowCloseMS Milli        `json:"windowCloseMs,omitempty"`
	ExpiresAtMS   Milli        `json:"expiresAtMs,omitempty"`
	RemainingMS   Milli        `json:"remainingMs,omitempty"`
	RenewCount    int          `json:"renewCount,omitempty"`
	Arbitration   *Arbitration `json:"arbitration,omitempty"`
	Source        SourceRef    `json:"source"`
	Recomputed    bool         `json:"recomputed,omitempty"`
	CacheHit      bool         `json:"cacheHit,omitempty"`
	Diagnostic    string       `json:"diagnostic,omitempty"`
}

type TimedEvent struct {
	Order      int       `json:"order"`
	AtMS       Milli     `json:"atMs"`
	Type       string    `json:"type"`
	HolderID   string    `json:"holderId"`
	ResourceID string    `json:"resourceId"`
	Reason     string    `json:"reason"`
	Source     SourceRef `json:"source"`
}

type HolderState struct {
	HolderID    string `json:"holderId"`
	ResourceID  string `json:"resourceId"`
	Active      bool   `json:"active"`
	RenewCount  int    `json:"renewCount"`
	ExpiresAtMS Milli  `json:"expiresAtMs"`
	LastSeq     int    `json:"lastSeq"`
}

type Report struct {
	SchemaVersion string        `json:"schemaVersion"`
	ProjectID     string        `json:"projectId"`
	Seed          int64         `json:"seed"`
	ToleranceMS   Milli         `json:"toleranceMs"`
	Results       []OpResult    `json:"results"`
	Events        []TimedEvent  `json:"events"`
	HolderStates  []HolderState `json:"holderStates"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(b, &out)
	return &out
}

func (p *Project) SortedOperations() []Operation {
	out := append([]Operation(nil), p.Operations...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (r *Report) ResultBySeq(seq int) *OpResult {
	for i := range r.Results {
		if r.Results[i].Seq == seq {
			return &r.Results[i]
		}
	}
	return nil
}

func (r *Report) CanonicalJSON() []byte {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Results = append([]OpResult(nil), r.Results...)
	for i := range cp.Results {
		cp.Results[i].Recomputed = false
		cp.Results[i].CacheHit = false
	}
	b, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}
