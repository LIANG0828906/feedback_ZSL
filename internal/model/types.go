package model

import "sort"

const (
	SchemaV1 = "wal.v1.flat-lsn-tick-bytes"
	SchemaV2 = "wal.v2.flat-offset-ms-kib"
	SchemaV3 = "wal.v3.tree-offset-tick-kib"

	CheckpointSchemaVersion = "recovery-checkpoint-v1"

	IntegrityComplete  = "complete"
	IntegrityTruncated = "truncated"
	IntegrityDegraded  = "degraded"

	CacheRecomputed = "recomputed"
	CacheReused     = "reused"

	StateReplayed = "replayed"
	StateSkipped  = "skipped_checkpoint"
	StateDegraded = "degraded"
	StateRejected = "rejected"
)

type OpKind string

const (
	OpBegin      OpKind = "begin"
	OpPut        OpKind = "put"
	OpDelete     OpKind = "delete"
	OpCommit     OpKind = "commit"
	OpAbort      OpKind = "abort"
	OpCheckpoint OpKind = "checkpoint"
)

type SchemaVersion string

type Diagnostic struct {
	Code     ErrorCode      `json:"code"`
	Severity string         `json:"severity"`
	Locator  string         `json:"locator"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

type Record struct {
	ID            string `json:"id"`
	SegmentID     string `json:"segment_id"`
	Index         int    `json:"index"`
	LSN           uint64 `json:"lsn"`
	Offset        uint64 `json:"offset"`
	TransactionID string `json:"transaction_id,omitempty"`
	Op            OpKind `json:"op"`
	Key           string `json:"key,omitempty"`
	Value         string `json:"value,omitempty"`
	Version       int64  `json:"version,omitempty"`
	TimeMS        int64  `json:"time_ms"`
	Tick          int64  `json:"tick"`
	Complete      bool   `json:"complete"`
}

type Segment struct {
	ID              string       `json:"id"`
	Index           int          `json:"index"`
	BaseLSN         uint64       `json:"base_lsn"`
	FirstLSN        uint64       `json:"first_lsn"`
	LastLSN         uint64       `json:"last_lsn"`
	SizeBytes       int64        `json:"size_bytes"`
	CreatedMS       int64        `json:"created_ms"`
	CreatedTick     int64        `json:"created_tick"`
	PredecessorIDs  []string     `json:"predecessor_ids,omitempty"`
	Records         []*Record    `json:"records"`
	Integrity       string       `json:"integrity"`
	CompleteRecords int          `json:"complete_records"`
	RawRecordCount  int          `json:"raw_record_count"`
	Diagnostics     []Diagnostic `json:"diagnostics,omitempty"`
}

type Checkpoint struct {
	ID        string `json:"id"`
	LSN       uint64 `json:"lsn"`
	SegmentID string `json:"segment_id"`
	RecordID  string `json:"record_id,omitempty"`
	AtMS      int64  `json:"at_ms"`
	Sequence  int    `json:"sequence"`
}

type Project struct {
	ID               string        `json:"id"`
	Seed             int64         `json:"seed"`
	SchemaVersion    SchemaVersion `json:"schema_version"`
	TickIntervalMS   int64         `json:"tick_interval_ms"`
	Segments         []*Segment    `json:"segments"`
	Checkpoints      []*Checkpoint `json:"checkpoints,omitempty"`
	LatestCheckpoint *Checkpoint   `json:"latest_checkpoint,omitempty"`
	Diagnostics      []Diagnostic  `json:"diagnostics,omitempty"`
}

func (p *Project) Segment(id string) *Segment {
	for _, seg := range p.Segments {
		if seg.ID == id {
			return seg
		}
	}
	return nil
}

func (p *Project) SortedSegments() []*Segment {
	out := append([]*Segment(nil), p.Segments...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (p *Project) SortedCheckpoints() []*Checkpoint {
	out := append([]*Checkpoint(nil), p.Checkpoints...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sequence != out[j].Sequence {
			return out[i].Sequence < out[j].Sequence
		}
		return out[i].LSN < out[j].LSN
	})
	return out
}

type DataItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   int64  `json:"version"`
	LSN       uint64 `json:"lsn"`
	TxID      string `json:"tx_id"`
	SegmentID string `json:"segment_id"`
	Deleted   bool   `json:"deleted,omitempty"`
}

type Write struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Version   int64  `json:"version"`
	LSN       uint64 `json:"lsn"`
	CommitLSN uint64 `json:"commit_lsn"`
	TxID      string `json:"tx_id"`
	SegmentID string `json:"segment_id"`
	Deleted   bool   `json:"deleted,omitempty"`
}

type Point struct {
	SegmentID string `json:"segment_id"`
	RecordID  string `json:"record_id,omitempty"`
	LSN       uint64 `json:"lsn"`
	Offset    uint64 `json:"offset"`
	Index     int    `json:"index"`
	TimeMS    int64  `json:"time_ms"`
	Tick      int64  `json:"tick"`
}

type ReplayRange struct {
	SegmentID   string `json:"segment_id"`
	Replay      bool   `json:"replay"`
	Reason      string `json:"reason"`
	Start       Point  `json:"start"`
	End         Point  `json:"end"`
	StartLSN    uint64 `json:"start_lsn"`
	EndLSN      uint64 `json:"end_lsn"`
	StartIndex  int    `json:"start_index"`
	EndIndex    int    `json:"end_index"`
	RecordCount int    `json:"record_count"`
}

type InvalidationSource struct {
	Code      ErrorCode `json:"code"`
	EditID    string    `json:"edit_id,omitempty"`
	Kind      string    `json:"kind"`
	SegmentID string    `json:"segment_id"`
	Path      []string  `json:"path,omitempty"`
	Message   string    `json:"message"`
}

type SegmentReport struct {
	SegmentID             string               `json:"segment_id"`
	State                 string               `json:"state"`
	Integrity             string               `json:"integrity"`
	Range                 ReplayRange          `json:"range"`
	AppliedRecords        int                  `json:"applied_records"`
	VisibleRecords        []string             `json:"visible_records,omitempty"`
	CommittedTransactions []string             `json:"committed_transactions,omitempty"`
	AbortedTransactions   []string             `json:"aborted_transactions,omitempty"`
	Writes                []Write              `json:"writes,omitempty"`
	LastRecovered         Point                `json:"last_recovered"`
	EstimatedMemoryBytes  int64                `json:"estimated_memory_bytes"`
	ElapsedNanoseconds    int64                `json:"elapsed_nanoseconds"`
	CacheStatus           string               `json:"cache_status,omitempty"`
	CacheKey              string               `json:"cache_key,omitempty"`
	InvalidationSources   []InvalidationSource `json:"invalidation_sources,omitempty"`
	Diagnostics           []Diagnostic         `json:"diagnostics,omitempty"`
}

type Budget struct {
	Workers               int   `json:"workers"`
	QueueDepth            int   `json:"queue_depth"`
	MaxSegmentMemoryBytes int64 `json:"max_segment_memory_bytes"`
}

type RecoveryResult struct {
	ProjectID       string                    `json:"project_id"`
	CheckpointID    string                    `json:"checkpoint_id,omitempty"`
	Items           []DataItem                `json:"items"`
	ItemMap         map[string]DataItem       `json:"item_map,omitempty"`
	Segments        []*SegmentReport          `json:"segments"`
	SegmentMap      map[string]*SegmentReport `json:"segment_map,omitempty"`
	Ranges          map[string]ReplayRange    `json:"ranges"`
	CommittedTxIDs  []string                  `json:"committed_tx_ids,omitempty"`
	AbortedTxIDs    []string                  `json:"aborted_tx_ids,omitempty"`
	Diagnostics     []Diagnostic              `json:"diagnostics,omitempty"`
	Budget          Budget                    `json:"budget"`
	ScheduleOrder   []string                  `json:"schedule_order,omitempty"`
	CommitBatchSize int                       `json:"commit_batch_size,omitempty"`
	Backpressure    []Diagnostic              `json:"backpressure,omitempty"`
	CacheRecomputed []string                  `json:"cache_recomputed,omitempty"`
	CacheReused     []string                  `json:"cache_reused,omitempty"`
}
