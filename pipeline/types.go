package pipeline

import (
	"time"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/model"
	"iconflow/segment"
	"iconflow/state"
)

type Budget struct {
	Workers            int `json:"workers"`
	MaxPixelsPerThread int `json:"max_pixels_per_thread"`
	MaxQueuedBlocks    int `json:"max_queued_blocks"`
}

func DefaultBudget() Budget {
	return Budget{Workers: 2, MaxPixelsPerThread: 1024, MaxQueuedBlocks: 4}
}

type Options struct {
	Mode             string
	Budget           Budget
	Store            checkpoint.Store
	StopAfterBlocks  int
	FailFirstAttempt map[string]bool
}

type StageKeys struct {
	Gradient  string `json:"gradient"`
	Threshold string `json:"threshold"`
	Markers   string `json:"markers"`
	Stats     string `json:"stats"`
}

type Artifact struct {
	InputHash string                   `json:"input_hash"`
	Gradient  *segment.GradientResult  `json:"gradient,omitempty"`
	Threshold *segment.ThresholdResult `json:"threshold,omitempty"`
	Markers   []bool                   `json:"markers,omitempty"`
	Stats     *segment.BlockStats      `json:"stats,omitempty"`
}

type BlockResult struct {
	Coord           model.BlockCoord         `json:"coord"`
	InputHash       string                   `json:"input_hash"`
	Keys            StageKeys                `json:"keys"`
	Gradient        *segment.GradientResult  `json:"gradient,omitempty"`
	Threshold       *segment.ThresholdResult `json:"threshold,omitempty"`
	Markers         []bool                   `json:"markers,omitempty"`
	Stats           *segment.BlockStats      `json:"stats,omitempty"`
	Reused          bool                     `json:"reused"`
	ReusedStages    []string                 `json:"reused_stages,omitempty"`
	StageSeqs       map[string]int64         `json:"stage_seqs,omitempty"`
	ValidationError *errs.Error              `json:"validation_error,omitempty"`
}

type QueueEvent struct {
	BlockKey string    `json:"block"`
	Reason   string    `json:"reason"`
	WaitedMS int64     `json:"waited_ms"`
	WaitedNS int64     `json:"waited_ns"`
	At       time.Time `json:"at"`
}

type RunReport struct {
	ProjectID        string               `json:"project_id"`
	Mode             string               `json:"mode"`
	Results          []*BlockResult       `json:"results"`
	QueueEvents      []QueueEvent         `json:"queue_events,omitempty"`
	Progress         state.Progress       `json:"progress"`
	CommitSequence   []CommitSequenceItem `json:"commit_sequence"`
	ValidationFailed int                  `json:"validation_failed"`
	Interrupted      bool                 `json:"interrupted"`
}

type CommitSequenceItem struct {
	Seq         int64  `json:"seq"`
	BlockKey    string `json:"block"`
	WindowID    string `json:"window,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Status      string `json:"status"`
	CacheKey    string `json:"cache_key,omitempty"`
	Threshold   int    `json:"threshold,omitempty"`
	InputHash   string `json:"input_hash,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

type JobError struct {
	Coord model.BlockCoord
	Stage state.Stage
	Err   *errs.Error
}

func (e *JobError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Coord.Key() + " " + string(e.Stage) + ": " + e.Err.Error()
}

var ErrInterrupted = errs.New("E_INTERRUPTED", "$.runner", "deterministic interruption after committed blocks")
