package engine

import "tlscheck/internal/model"

const (
	StatusQueued      = "QUEUED"
	StatusAdmitted    = "ADMITTED"
	StatusCompleted   = "COMPLETED"
	StatusRejected    = "REJECTED"
	StatusInterrupted = "INTERRUPTED"

	HandshakeSuccess          = "SUCCESS"
	HandshakeProtocolMismatch = "PROTOCOL_VERSION_MISMATCH"
	HandshakeCipherMismatch   = "CIPHER_SUITE_MISMATCH"
	HandshakeTimeout          = "TIMEOUT"
	HandshakeInterrupted      = "INTERRUPTED_BEFORE_COMMIT"

	GateOK                = "OK"
	GateBudgetFrozen      = "BUDGET_FROZEN"
	GateBudgetExhausted   = "BUDGET_EXHAUSTED"
	GateChainInvalid      = "CHAIN_INVALID"
	GateParallelExhausted = "PARALLEL_EXHAUSTED"
	GateBufferFull        = "BUFFER_FULL"

	ViolationBudget   = "C03_CHAIN_BUDGET"
	ViolationParallel = "C04_MAX_PARALLEL"
	ViolationFreeze   = "C05_BUDGET_FREEZE"
	ViolationBuffer   = "C06_PENDING_BUFFER"
	ViolationChain    = "C02_CERTIFICATE_CHAIN"
)

type Options struct {
	Workers        int
	EndpointOrder  []string
	Faults         FaultOptions
	InitialStates  map[string]*EndpointState
	InitialResults map[string][]*RequestResult
	Now            func() int64
	ObserveState   func(endpointID string, timeMs int64, state *EndpointState)
}

type FaultOptions struct {
	InterruptRequestIDs map[string]bool
	InterruptUpdateIDs  map[string]bool
	StepJournal         StepJournal
}

type StepJournal interface {
	LoadSteps(requestID string) ([]LevelResult, error)
	SaveStep(requestID, chainID string, level LevelResult) error
	ResetRequest(requestID string) error
}

type Report struct {
	ProjectID      string                    `json:"project_id"`
	SnapshotHash   string                    `json:"snapshot_hash"`
	Seed           int64                     `json:"seed"`
	Results        []*RequestResult          `json:"results"`
	QueueRecords   []*QueueRecord            `json:"queue_records"`
	Moments        []MomentReport            `json:"moments"`
	EndpointStates map[string]*EndpointState `json:"endpoint_states"`
	Diagnostics    Diagnostics               `json:"diagnostics"`
}

type Diagnostics struct {
	EndpointElapsedNS map[string]int64 `json:"endpoint_elapsed_ns,omitempty"`
	PeakWorkers       int              `json:"peak_workers,omitempty"`
	ThreadThrottles   int              `json:"thread_throttles,omitempty"`
	BackpressureCount int              `json:"backpressure_count,omitempty"`
}

type Provenance struct {
	State              string   `json:"state,omitempty"`
	Recomputed         bool     `json:"recomputed"`
	Reused             bool     `json:"reused"`
	Sources            []string `json:"sources,omitempty"`
	InvalidationSource []string `json:"invalidation_source,omitempty"`
	CacheKey           string   `json:"cache_key,omitempty"`
}

type RequestResult struct {
	RequestID           string         `json:"request_id"`
	EndpointID          string         `json:"endpoint_id"`
	EventSequence       int            `json:"event_sequence"`
	ArrivalAtMs         int64          `json:"arrival_at_ms"`
	Status              string         `json:"status"`
	GateCategory        string         `json:"gate_category"`
	HandshakeConclusion string         `json:"handshake_conclusion,omitempty"`
	Admitted            bool           `json:"admitted"`
	AdmissionAtMs       int64          `json:"admission_at_ms,omitempty"`
	StartedAtMs         int64          `json:"started_at_ms,omitempty"`
	CompletedAtMs       int64          `json:"completed_at_ms,omitempty"`
	FinalAdmissionAtMs  int64          `json:"final_admission_at_ms"`
	HasFinalAdmission   bool           `json:"has_final_admission"`
	QueueNo             int            `json:"queue_no,omitempty"`
	EndpointQueueSeq    int            `json:"endpoint_queue_seq,omitempty"`
	ViolatedConstraints []string       `json:"violated_constraints,omitempty"`
	Violations          []Violation    `json:"violations,omitempty"`
	CheckedChainIDs     []string       `json:"checked_chain_ids,omitempty"`
	SelectedChainID     string         `json:"selected_chain_id,omitempty"`
	ChainResults        []ChainResult  `json:"chain_results,omitempty"`
	Fallback            *FallbackBasis `json:"fallback,omitempty"`
	BudgetChargedBP     int64          `json:"budget_charged_bp"`
	BackpressureCode    string         `json:"backpressure_code,omitempty"`
	Provenance          Provenance     `json:"provenance"`
}

type Violation struct {
	ConstraintID string `json:"constraint_id"`
	Code         string `json:"code"`
	Message      string `json:"message"`
}

type ChainResult struct {
	ChainID       string        `json:"chain_id"`
	Valid         bool          `json:"valid"`
	TotalCostBP   int64         `json:"total_cost_bp"`
	ChargedCostBP int64         `json:"charged_cost_bp"`
	Levels        []LevelResult `json:"levels"`
}

type LevelResult struct {
	Index   int    `json:"index"`
	Digest  string `json:"digest"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Passed  bool   `json:"passed"`
	Skipped bool   `json:"skipped,omitempty"`
	Reused  bool   `json:"reused,omitempty"`
	CostBP  int64  `json:"cost_bp"`
}

type FallbackBasis struct {
	AttemptedChainID      string `json:"attempted_chain_id"`
	FallbackChainID       string `json:"fallback_chain_id"`
	Reason                string `json:"reason"`
	LastCommittedUpdateID string `json:"last_committed_update_id,omitempty"`
	CommittedAtMs         int64  `json:"committed_at_ms"`
	RevalidatedAtMs       int64  `json:"revalidated_at_ms"`
	RevalidationPassed    bool   `json:"revalidation_passed"`
}

type QueueRecord struct {
	QueueNo             int      `json:"queue_no"`
	EndpointQueueSeq    int      `json:"endpoint_queue_seq"`
	RequestID           string   `json:"request_id"`
	EndpointID          string   `json:"endpoint_id"`
	EventSequence       int      `json:"event_sequence"`
	EnqueuedAtMs        int64    `json:"enqueued_at_ms"`
	FinalAdmissionAtMs  int64    `json:"final_admission_at_ms"`
	HasFinalAdmission   bool     `json:"has_final_admission"`
	Status              string   `json:"status"`
	PrimaryGate         string   `json:"primary_gate"`
	ViolatedConstraints []string `json:"violated_constraints"`
}

type MomentReport struct {
	TimeMs    int64                     `json:"time_ms"`
	Sequence  int                       `json:"logical_sequence"`
	Endpoints map[string]MomentEndpoint `json:"endpoints"`
}

type MomentEndpoint struct {
	UsedBudgetBP     int64    `json:"used_budget_bp"`
	ReservedBudgetBP int64    `json:"reserved_budget_bp"`
	Frozen           bool     `json:"frozen"`
	ActiveCount      int      `json:"active_count"`
	QueueIDs         []string `json:"queue_ids"`
	CurrentChainID   string   `json:"current_chain_id"`
	LastValidChainID string   `json:"last_valid_chain_id"`
}

type EndpointState struct {
	EndpointID            string                        `json:"endpoint_id"`
	PeriodStartMs         int64                         `json:"period_start_ms"`
	UsedBudgetBP          int64                         `json:"used_budget_bp"`
	ReservedBudgetBP      int64                         `json:"reserved_budget_bp"`
	ActiveCount           int                           `json:"active_count"`
	Frozen                bool                          `json:"frozen"`
	FrozenReason          string                        `json:"frozen_reason,omitempty"`
	CurrentChainID        string                        `json:"current_chain_id"`
	LastValidChainID      string                        `json:"last_valid_chain_id"`
	LastCommittedUpdateID string                        `json:"last_committed_update_id,omitempty"`
	LastCommitAtMs        int64                         `json:"last_commit_at_ms"`
	NextQueueSequence     int                           `json:"next_queue_sequence"`
	Queue                 []*QueueItem                  `json:"queue"`
	History               []*QueueItem                  `json:"history,omitempty"`
	Active                map[string]*ActiveHandshake   `json:"active,omitempty"`
	Charges               []BudgetCharge                `json:"charges,omitempty"`
	CommittedTimeMs       int64                         `json:"committed_time_ms"`
	ProcessedEvents       map[int]bool                  `json:"processed_events,omitempty"`
	PendingUpdates        map[string]*model.ChainUpdate `json:"pending_updates,omitempty"`
}

type QueueItem struct {
	Status              string      `json:"status"`
	EndpointID          string      `json:"endpoint_id"`
	EndpointQueueSeq    int         `json:"endpoint_queue_seq"`
	RequestID           string      `json:"request_id"`
	EventSequence       int         `json:"event_sequence"`
	EnqueuedAtMs        int64       `json:"enqueued_at_ms"`
	LastAttemptAtMs     int64       `json:"last_attempt_at_ms"`
	AdmittedAtMs        int64       `json:"admitted_at_ms,omitempty"`
	HasAdmission        bool        `json:"has_admission"`
	PrimaryGate         string      `json:"primary_gate"`
	ViolatedConstraints []string    `json:"violated_constraints"`
	Violations          []Violation `json:"violations,omitempty"`
}

type ActiveHandshake struct {
	RequestID          string         `json:"request_id"`
	StartedAtMs        int64          `json:"started_at_ms"`
	FinishAtMs         int64          `json:"finish_at_ms"`
	TimeoutAtMs        int64          `json:"timeout_at_ms"`
	TimedOut           bool           `json:"timed_out"`
	ChainID            string         `json:"chain_id"`
	ReservedBP         int64          `json:"reserved_bp"`
	ChargedBP          int64          `json:"charged_bp"`
	NextLevel          int            `json:"next_level"`
	ValidationComplete bool           `json:"validation_complete"`
	Levels             []LevelResult  `json:"levels"`
	ChainResults       []ChainResult  `json:"chain_results,omitempty"`
	SelectedChainID    string         `json:"selected_chain_id,omitempty"`
	GateCategory       string         `json:"gate_category,omitempty"`
	Fallback           *FallbackBasis `json:"fallback,omitempty"`
	CheckedChainIDs    []string       `json:"checked_chain_ids,omitempty"`
	Conclusion         string         `json:"conclusion,omitempty"`
	Request            *model.Request `json:"request,omitempty"`
	EventSequence      int            `json:"event_sequence"`
	ArrivalAtMs        int64          `json:"arrival_at_ms"`
}

type BudgetCharge struct {
	TimeMs    int64  `json:"time_ms"`
	RequestID string `json:"request_id"`
	ChainID   string `json:"chain_id"`
	Level     int    `json:"level"`
	AmountBP  int64  `json:"amount_bp"`
}
