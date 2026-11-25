package schema

import (
	"encoding/json"
	"strings"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
)

const (
	V1 = "collector.v1"
	V2 = "collector.v2"
	V3 = "collector.v3"
)

type V1Message struct {
	MessageID string `json:"message_id"`
	Bytes     int64  `json:"payload_bytes"`
}

type V1Batch struct {
	BatchID      string      `json:"batch_id"`
	AgentID      string      `json:"agent_id"`
	Tenant       string      `json:"tenant"`
	PartitionKey string      `json:"partition_key"`
	ArrivalTick  int         `json:"arrival_tick"`
	Messages     []V1Message `json:"messages"`
}

type V1Agent struct {
	AgentID                   string               `json:"agent_id"`
	Host                      string               `json:"host"`
	SendBufferBytes           *int64               `json:"send_buffer_bytes,omitempty"`
	MaxBatchBytes             int64                `json:"max_batch_bytes"`
	MaxBatchMessages          int                  `json:"max_batch_messages"`
	BackoffPermille           int64                `json:"backoff_permille"`
	PendingQueueLimit         int                  `json:"pending_queue_limit"`
	OverflowPolicy            model.OverflowPolicy `json:"overflow_policy"`
	BufferExhaustedLimitTicks int                  `json:"buffer_exhausted_limit_ticks"`
}

type V1Partition struct {
	PartitionID          string `json:"partition_id"`
	Index                int    `json:"index"`
	ConsumeRatePermille  int64  `json:"consume_rate_permille"`
	AckDelayTicks        int    `json:"ack_delay_ticks"`
	BacklogLimitBytes    int64  `json:"backlog_limit_bytes"`
	StrictStaleWatermark bool   `json:"strict_stale_watermark"`
}

type V1Rule struct {
	RuleID       string `json:"rule_id"`
	Tenant       string `json:"tenant"`
	MatchKey     string `json:"match_key"`
	PartitionID  string `json:"partition_id"`
	ParentRuleID string `json:"parent_rule_id,omitempty"`
	Priority     int    `json:"priority"`
}

type V1WatermarkUpdate struct {
	PartitionID string `json:"partition_id"`
	Tick        int    `json:"tick"`
	Watermark   int64  `json:"watermark_bytes"`
}

type V1Project struct {
	Schema           string              `json:"schema"`
	ProjectID        string              `json:"project_id"`
	ProjectVersion   string              `json:"project_version"`
	Seed             int64               `json:"seed"`
	TickDurationMs   int64               `json:"tick_duration_ms"`
	RateBaseBytes    int64               `json:"rate_base_bytes_per_tick"`
	BackoffBaseTicks int                 `json:"backoff_base_ticks"`
	TickCount        int                 `json:"tick_count"`
	Agents           []V1Agent           `json:"agents"`
	Partitions       []V1Partition       `json:"partitions"`
	KeyRules         []V1Rule            `json:"key_rules"`
	Batches          []V1Batch           `json:"batches"`
	WatermarkUpdates []V1WatermarkUpdate `json:"watermark_updates,omitempty"`
}

type V2Message struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
}

type V2Batch struct {
	BatchID      string      `json:"batchId"`
	AgentID      string      `json:"agentId"`
	TenantID     string      `json:"tenantId"`
	PartitionKey string      `json:"partitionKey"`
	ArrivalMs    int64       `json:"arrivalMs"`
	Messages     []V2Message `json:"messages"`
}

type V2Agent struct {
	AgentID                   string               `json:"agentId"`
	HostName                  string               `json:"hostName"`
	BufferBytes               *int64               `json:"bufferBytes,omitempty"`
	BufferEquivMessages       *int                 `json:"bufferEquivMessages,omitempty"`
	EquivalentBytesPerMessage int64                `json:"equivalentBytesPerMessage"`
	MaxBatchBytes             int64                `json:"maxBatchBytes"`
	MaxBatchMessages          int                  `json:"maxBatchMessages"`
	BackoffPermyriad          int64                `json:"backoffPermyriad"`
	PendingQueueLimit         int                  `json:"pendingQueueLimit"`
	OverflowPolicy            model.OverflowPolicy `json:"overflowPolicy"`
	BufferExhaustedDurationMs int64                `json:"bufferExhaustedDurationMs"`
}

type V2Partition struct {
	PartitionID               string `json:"partitionId"`
	Index                     int    `json:"index"`
	ConsumeRatePermyriad      int64  `json:"consumeRatePermyriad"`
	AckDelayMs                int64  `json:"ackDelayMs"`
	BacklogLimitBytes         *int64 `json:"backlogLimitBytes,omitempty"`
	BacklogLimitEquivMessages *int   `json:"backlogLimitEquivMessages,omitempty"`
	EquivalentBytesPerMessage int64  `json:"equivalentBytesPerMessage,omitempty"`
	StrictStaleWatermark      bool   `json:"strictStaleWatermark"`
}

type V2Rule struct {
	RuleID       string `json:"ruleId"`
	TenantID     string `json:"tenantId"`
	MatchKey     string `json:"matchKey"`
	PartitionID  string `json:"partitionId"`
	ParentRuleID string `json:"parentRuleId,omitempty"`
	Priority     int    `json:"priority"`
}

type V2WatermarkUpdate struct {
	PartitionID string `json:"partitionId"`
	AtMs        int64  `json:"atMs"`
	Watermark   int64  `json:"watermarkBytes"`
}

type V2Project struct {
	Schema                    string              `json:"schema"`
	ProjectID                 string              `json:"projectId"`
	ProjectVersion            string              `json:"projectVersion"`
	Seed                      int64               `json:"seed"`
	TickDurationMs            int64               `json:"tickDurationMs"`
	RateBaseBytes             int64               `json:"rateBaseBytesPerTick"`
	BackoffBaseTicks          int                 `json:"backoffBaseTicks"`
	EquivalentBytesPerMessage int64               `json:"equivalentBytesPerMessage,omitempty"`
	TickCount                 int                 `json:"tickCount"`
	Agents                    []V2Agent           `json:"agents"`
	Partitions                []V2Partition       `json:"partitions"`
	KeyRules                  []V2Rule            `json:"keyRules"`
	Batches                   []V2Batch           `json:"batches"`
	WatermarkUpdates          []V2WatermarkUpdate `json:"watermarkUpdates,omitempty"`
}

func LoadJSON(data []byte) (*model.Project, error) {
	var head struct {
		Schema     string `json:"schema"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "json", err, "cannot parse schema envelope")
	}
	kind := strings.ToLower(head.Schema + " " + head.APIVersion)
	switch {
	case strings.Contains(kind, "v1"):
		var in V1Project
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "json", err, "cannot parse v1 project")
		}
		return FromV1(&in)
	case strings.Contains(kind, "v2"):
		var in V2Project
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "json", err, "cannot parse v2 project")
		}
		return FromV2(&in)
	case strings.Contains(kind, "v3"):
		var in V3Project
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "json", err, "cannot parse v3 project")
		}
		return FromV3(&in)
	default:
		return nil, bpderr.Newf(bpderr.ESchemaUnsupported, "schema", "unsupported schema envelope %q", strings.TrimSpace(kind))
	}
}

func FromV1(in *V1Project) (*model.Project, error) {
	if in.Schema == "" {
		in.Schema = V1
	}
	out := &model.Project{
		ID: in.ProjectID, Version: in.ProjectVersion, SchemaVersion: V1, Seed: in.Seed,
		TickMillis: in.TickDurationMs, RateBaseBytes: in.RateBaseBytes,
		BackoffBaseTick: in.BackoffBaseTicks, Ticks: in.TickCount,
	}
	if out.RateBaseBytes == 0 {
		out.RateBaseBytes = 10000
	}
	if out.BackoffBaseTick == 0 {
		out.BackoffBaseTick = 1000
	}
	for _, a := range in.Agents {
		buf, err := NormalizeBuffer(derefInt64(a.SendBufferBytes), a.SendBufferBytes != nil, 0, false, 0)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.AgentID)
		}
		backoff, err := ScalePermille(int64(out.BackoffBaseTick), a.BackoffPermille)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.AgentID)
		}
		out.Agents = append(out.Agents, model.Agent{
			ID: a.AgentID, Host: a.Host, BufferMaxBytes: buf,
			MaxBatchBytes: a.MaxBatchBytes, MaxBatchMessages: a.MaxBatchMessages,
			BackoffTicks: int(backoff), PendingQueueLimit: a.PendingQueueLimit,
			OverflowPolicy: a.OverflowPolicy, BufferExhaustedLimitTicks: a.BufferExhaustedLimitTicks,
		})
	}
	for _, p := range in.Partitions {
		rate, err := ScalePermille(out.RateBaseBytes, p.ConsumeRatePermille)
		if err != nil {
			return nil, bpderr.Attach(err, "partition_id", p.PartitionID)
		}
		out.Partitions = append(out.Partitions, model.Partition{
			ID: p.PartitionID, Index: p.Index, RateBytesPerTick: rate,
			AckDelayTicks: p.AckDelayTicks, BacklogLimitBytes: p.BacklogLimitBytes,
			StrictStaleWatermark: p.StrictStaleWatermark,
		})
	}
	for _, r := range in.KeyRules {
		out.Rules = append(out.Rules, model.KeyRule{ID: r.RuleID, Tenant: r.Tenant, Key: r.MatchKey, PartitionID: r.PartitionID, ParentID: r.ParentRuleID, Priority: r.Priority})
	}
	for _, b := range in.Batches {
		batch := model.Batch{ID: b.BatchID, AgentID: b.AgentID, Tenant: b.Tenant, Key: b.PartitionKey, ArrivalTick: b.ArrivalTick}
		for _, m := range b.Messages {
			batch.Messages = append(batch.Messages, model.Message{ID: m.MessageID, Bytes: m.Bytes})
		}
		out.Batches = append(out.Batches, batch)
	}
	for _, u := range in.WatermarkUpdates {
		out.WatermarkUpdates = append(out.WatermarkUpdates, model.WatermarkUpdate{PartitionID: u.PartitionID, Tick: u.Tick, Watermark: u.Watermark})
	}
	if err := ValidateProject(out); err != nil {
		return nil, err
	}
	return out, nil
}

func FromV2(in *V2Project) (*model.Project, error) {
	if in.Schema == "" {
		in.Schema = V2
	}
	out := &model.Project{
		ID: in.ProjectID, Version: in.ProjectVersion, SchemaVersion: V2, Seed: in.Seed,
		TickMillis: in.TickDurationMs, RateBaseBytes: in.RateBaseBytes,
		BackoffBaseTick: in.BackoffBaseTicks, Ticks: in.TickCount,
	}
	if out.RateBaseBytes == 0 {
		out.RateBaseBytes = 10000
	}
	if out.BackoffBaseTick == 0 {
		out.BackoffBaseTick = 1000
	}
	for _, a := range in.Agents {
		buf, err := NormalizeBuffer(derefInt64(a.BufferBytes), a.BufferBytes != nil, derefInt(a.BufferEquivMessages), a.BufferEquivMessages != nil, a.EquivalentBytesPerMessage)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.AgentID)
		}
		backoff, err := ScalePermyriad(int64(out.BackoffBaseTick), a.BackoffPermyriad)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.AgentID)
		}
		exhaustedTicks, err := TickFromMs(a.BufferExhaustedDurationMs, out.TickMillis, "bufferExhaustedDurationMs")
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.AgentID)
		}
		out.Agents = append(out.Agents, model.Agent{
			ID: a.AgentID, Host: a.HostName, BufferMaxBytes: buf,
			MaxBatchBytes: a.MaxBatchBytes, MaxBatchMessages: a.MaxBatchMessages,
			BackoffTicks: int(backoff), PendingQueueLimit: a.PendingQueueLimit,
			OverflowPolicy: a.OverflowPolicy, BufferExhaustedLimitTicks: exhaustedTicks,
		})
	}
	for _, p := range in.Partitions {
		rate, err := ScalePermyriad(out.RateBaseBytes, p.ConsumeRatePermyriad)
		if err != nil {
			return nil, bpderr.Attach(err, "partitionId", p.PartitionID)
		}
		delay, err := TickFromMs(p.AckDelayMs, out.TickMillis, "ackDelayMs")
		if err != nil {
			return nil, bpderr.Attach(err, "partitionId", p.PartitionID)
		}
		equivBytes := p.EquivalentBytesPerMessage
		if equivBytes == 0 {
			equivBytes = in.EquivalentBytesPerMessage
		}
		backlogLimit, err := NormalizeByteOrEquiv(
			derefInt64(p.BacklogLimitBytes), p.BacklogLimitBytes != nil,
			derefInt(p.BacklogLimitEquivMessages), p.BacklogLimitEquivMessages != nil,
			equivBytes, "backlogLimit",
		)
		if err != nil {
			return nil, bpderr.Attach(err, "partitionId", p.PartitionID)
		}
		out.Partitions = append(out.Partitions, model.Partition{
			ID: p.PartitionID, Index: p.Index, RateBytesPerTick: rate,
			AckDelayTicks: delay, BacklogLimitBytes: backlogLimit,
			StrictStaleWatermark: p.StrictStaleWatermark,
		})
	}
	for _, r := range in.KeyRules {
		out.Rules = append(out.Rules, model.KeyRule{ID: r.RuleID, Tenant: r.TenantID, Key: r.MatchKey, PartitionID: r.PartitionID, ParentID: r.ParentRuleID, Priority: r.Priority})
	}
	for _, b := range in.Batches {
		tick, err := TickFromMs(b.ArrivalMs, out.TickMillis, "arrivalMs")
		if err != nil {
			return nil, bpderr.Attach(err, "batchId", b.BatchID)
		}
		batch := model.Batch{ID: b.BatchID, AgentID: b.AgentID, Tenant: b.TenantID, Key: b.PartitionKey, ArrivalTick: tick}
		for _, m := range b.Messages {
			batch.Messages = append(batch.Messages, model.Message{ID: m.ID, Bytes: m.Bytes})
		}
		out.Batches = append(out.Batches, batch)
	}
	for _, u := range in.WatermarkUpdates {
		tick, err := TickFromMs(u.AtMs, out.TickMillis, "atMs")
		if err != nil {
			return nil, bpderr.Attach(err, "partitionId", u.PartitionID)
		}
		out.WatermarkUpdates = append(out.WatermarkUpdates, model.WatermarkUpdate{PartitionID: u.PartitionID, Tick: tick, Watermark: u.Watermark})
	}
	if err := ValidateProject(out); err != nil {
		return nil, err
	}
	return out, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
