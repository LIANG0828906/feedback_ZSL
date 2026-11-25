// Package model contains the schema-independent collector domain model.
package model

import "time"

type OverflowPolicy string

const (
	OverflowDropNewest OverflowPolicy = "drop_newest"
	OverflowError      OverflowPolicy = "error"
	OverflowSuspend    OverflowPolicy = "suspend"
)

type Action string

const (
	ActionSend    Action = "send"
	ActionPending Action = "pending"
	ActionDrop    Action = "drop"
	ActionSlow    Action = "slow"
)

type WatermarkSource string

const (
	WatermarkDeclared WatermarkSource = "declared"
	WatermarkDerived  WatermarkSource = "derived_rate"
	WatermarkUpdated  WatermarkSource = "declared_update"
)

type Project struct {
	ID               string
	Version          string
	SchemaVersion    string
	Seed             int64
	TickMillis       int64
	RateBaseBytes    int64
	BackoffBaseTick  int
	Ticks            int
	Agents           []Agent
	Partitions       []Partition
	Rules            []KeyRule
	Batches          []Batch
	WatermarkUpdates []WatermarkUpdate
}

type Agent struct {
	ID                        string
	Host                      string
	BufferMaxBytes            int64
	MaxBatchBytes             int64
	MaxBatchMessages          int
	BackoffTicks              int
	PendingQueueLimit         int
	OverflowPolicy            OverflowPolicy
	BufferExhaustedLimitTicks int
}

type Partition struct {
	ID                   string
	Index                int
	RateBytesPerTick     int64
	AckDelayTicks        int
	BacklogLimitBytes    int64
	ConsumerWatermark    *int64
	StrictStaleWatermark bool
}

type KeyRule struct {
	ID          string
	Tenant      string
	Key         string
	PartitionID string
	ParentID    string
	Priority    int
}

type Message struct {
	ID    string
	Bytes int64
}

type Batch struct {
	ID          string
	AgentID     string
	Tenant      string
	Key         string
	ArrivalTick int
	Messages    []Message
}

type WatermarkUpdate struct {
	PartitionID string
	Tick        int
	Watermark   int64
}

type Chunk struct {
	ChunkID           string
	BatchID           string
	AgentID           string
	Tenant            string
	Key               string
	PartitionID       string
	ArrivalTick       int
	SplitIndex        int
	SplitCount        int
	FirstMessageIndex int
	LastMessageIndex  int
	Messages          []Message
	Count             int
	Bytes             int64
}

type PendingItem struct {
	ChunkID      string `json:"chunk_id"`
	BatchID      string `json:"batch_id"`
	PartitionID  string `json:"partition_id"`
	Bytes        int64  `json:"bytes"`
	Position     int    `json:"position"`
	ViolatedRule string `json:"violated_rule,omitempty"`
	SinceTick    int    `json:"since_tick"`
}

type AgentReport struct {
	AgentID            string        `json:"agent_id"`
	InflightBytesStart int64         `json:"inflight_bytes_start"`
	InflightBytesEnd   int64         `json:"inflight_bytes_end"`
	BufferMaxBytes     int64         `json:"buffer_max_bytes"`
	AcceptedChunks     []string      `json:"accepted_chunks"`
	SentChunks         []string      `json:"sent_chunks"`
	Pending            []PendingItem `json:"pending"`
	DroppedChunks      []string      `json:"dropped_chunks,omitempty"`
	BackoffUntilTick   int           `json:"backoff_until_tick"`
	ExhaustedSinceTick *int          `json:"exhausted_since_tick,omitempty"`
	Elapsed            time.Duration `json:"-"`
	ElapsedNanoseconds int64         `json:"elapsed_ns"`
	WorkerIndex        int           `json:"worker_index"`
	Codes              []string      `json:"codes,omitempty"`
}

type PartitionReport struct {
	PartitionID           string          `json:"partition_id"`
	WrittenBytes          int64           `json:"written_bytes_cumulative"`
	AckedBytes            int64           `json:"acked_bytes"`
	BacklogBytes          int64           `json:"backlog_bytes"`
	BacklogLimitBytes     int64           `json:"backlog_limit_bytes"`
	EffectiveWatermark    int64           `json:"effective_watermark_bytes"`
	WatermarkSource       WatermarkSource `json:"watermark_source"`
	WatermarkLagBytes     int64           `json:"watermark_lag_bytes"`
	WatermarkBacklogBytes int64           `json:"watermark_backlog_bytes"`
	Pressure              bool            `json:"pressure"`
	TriggeredRule         string          `json:"triggered_rule,omitempty"`
	AckedChunks           []string        `json:"acked_chunks,omitempty"`
	ElapsedNanoseconds    int64           `json:"elapsed_ns"`
	WorkerIndex           int             `json:"worker_index"`
}

type BackpressureSignal struct {
	Tick              int      `json:"tick"`
	SourceType        string   `json:"source_type"`
	SourceID          string   `json:"source_id"`
	RuleID            string   `json:"rule_id"`
	Code              string   `json:"code"`
	Action            Action   `json:"action"`
	TargetAgentIDs    []string `json:"target_agent_ids"`
	TargetPartitionID string   `json:"target_partition_id,omitempty"`
	UntilTick         int      `json:"until_tick,omitempty"`
	SlowTicks         int      `json:"slow_ticks,omitempty"`
}

type DropRecord struct {
	Tick             int      `json:"tick"`
	BatchID          string   `json:"batch_id"`
	ChunkIDs         []string `json:"chunk_ids"`
	AgentID          string   `json:"agent_id"`
	PartitionID      string   `json:"partition_id"`
	TriggerSourceID  string   `json:"trigger_source_id"`
	TriggerPartition string   `json:"trigger_partition_id"`
	RuleID           string   `json:"rule_id"`
	Code             string   `json:"code"`
	Reason           string   `json:"reason"`
}

type TickReport struct {
	Tick          int                  `json:"tick"`
	Agents        []AgentReport        `json:"agents"`
	Partitions    []PartitionReport    `json:"partitions"`
	Signals       []BackpressureSignal `json:"signals,omitempty"`
	Drops         []DropRecord         `json:"drops,omitempty"`
	ObservedCodes []string             `json:"observed_codes,omitempty"`
	Invalidation  *TickInvalidation    `json:"invalidation,omitempty"`
}

type TickInvalidation struct {
	Recomputed         bool     `json:"recomputed"`
	Sources            []string `json:"sources,omitempty"`
	PropagationClosure []string `json:"propagation_closure,omitempty"`
}

type Result struct {
	ProjectID      string           `json:"project_id"`
	ProjectVersion string           `json:"project_version"`
	SchemaVersion  string           `json:"schema_version"`
	Ticks          []TickReport     `json:"ticks"`
	Chunks         []Chunk          `json:"chunks"`
	ChunkByID      map[string]Chunk `json:"-"`
}

func (p Project) Agent(id string) (Agent, bool) {
	for _, agent := range p.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return Agent{}, false
}

func (p Project) Partition(id string) (Partition, bool) {
	for _, partition := range p.Partitions {
		if partition.ID == id {
			return partition, true
		}
	}
	return Partition{}, false
}
