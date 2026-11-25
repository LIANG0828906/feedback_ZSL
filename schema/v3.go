package schema

import (
	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
)

type V3Metadata struct {
	ProjectID string `json:"projectId"`
	Revision  string `json:"revision"`
	Seed      int64  `json:"seed"`
}

type V3Time struct {
	TickIntervalMs int64 `json:"tickIntervalMs"`
}

type V3Scales struct {
	RateBaseBytesPerTick int64 `json:"rateBaseBytesPerTick"`
	BackoffBaseTicks     int   `json:"backoffBaseTicks"`
}

type V3Collector struct {
	Name                      string               `json:"name"`
	HostEndpoint              string               `json:"hostEndpoint"`
	BufferLimitBytes          int64                `json:"bufferLimitBytes"`
	BatchByteCeiling          int64                `json:"batchByteCeiling"`
	BatchRecordCeiling        int                  `json:"batchRecordCeiling"`
	BackoffPermyriad          int64                `json:"backoffPermyriad"`
	PendingQueueCeiling       int                  `json:"pendingQueueCeiling"`
	OverflowDisposition       model.OverflowPolicy `json:"overflowDisposition"`
	BufferSaturatedDurationMs int64                `json:"bufferSaturatedDurationMs"`
}

type V3Partition struct {
	Name                   string `json:"name"`
	Index                  int    `json:"index"`
	ConsumptionPermyriad   int64  `json:"consumptionPermyriad"`
	AcknowledgementDelayMs int64  `json:"acknowledgementDelayMs"`
	BacklogCeilingBytes    int64  `json:"backlogCeilingBytes"`
	ConsumerWatermarkBytes *int64 `json:"consumerWatermarkBytes,omitempty"`
	StrictStaleWatermark   bool   `json:"strictStaleWatermark"`
}

type V3TreeRule struct {
	RuleName      string       `json:"ruleName"`
	MatchKey      string       `json:"matchKey"`
	PartitionName string       `json:"partitionName"`
	Priority      int          `json:"priority"`
	Children      []V3TreeRule `json:"children,omitempty"`
}

type V3Tenant struct {
	TenantID string       `json:"tenantId"`
	Rules    []V3TreeRule `json:"rules"`
}

type V3Entry struct {
	EntryUID     string `json:"entryUid"`
	PayloadBytes int64  `json:"payloadBytes"`
}

type V3Batch struct {
	BatchUID     string    `json:"batchUid"`
	Collector    string    `json:"collector"`
	TenantID     string    `json:"tenantId"`
	PartitionKey string    `json:"partitionKey"`
	ArrivalMs    int64     `json:"arrivalAtMs"`
	Entries      []V3Entry `json:"entries"`
}

type V3WatermarkUpdate struct {
	PartitionName string `json:"partitionName"`
	AtMs          int64  `json:"atMs"`
	Watermark     int64  `json:"watermarkBytes"`
}

type V3Project struct {
	APIVersion       string              `json:"apiVersion"`
	Metadata         V3Metadata          `json:"metadata"`
	Time             V3Time              `json:"time"`
	Scales           V3Scales            `json:"scales"`
	HorizonTicks     int                 `json:"horizonTicks"`
	Collectors       []V3Collector       `json:"collectors"`
	Partitions       []V3Partition       `json:"partitions"`
	TenantKeyTree    []V3Tenant          `json:"tenantKeyTree"`
	LogBatches       []V3Batch           `json:"logBatches"`
	WatermarkUpdates []V3WatermarkUpdate `json:"watermarkUpdates,omitempty"`
}

func FromV3(in *V3Project) (*model.Project, error) {
	if in.APIVersion == "" {
		in.APIVersion = "collector.pipeline/v3"
	}
	out := &model.Project{
		ID: in.Metadata.ProjectID, Version: in.Metadata.Revision, SchemaVersion: V3,
		Seed: in.Metadata.Seed, TickMillis: in.Time.TickIntervalMs,
		RateBaseBytes: in.Scales.RateBaseBytesPerTick, BackoffBaseTick: in.Scales.BackoffBaseTicks,
		Ticks: in.HorizonTicks,
	}
	if out.RateBaseBytes == 0 {
		out.RateBaseBytes = 10000
	}
	if out.BackoffBaseTick == 0 {
		out.BackoffBaseTick = 1000
	}
	for _, a := range in.Collectors {
		buf, err := NormalizeBuffer(a.BufferLimitBytes, true, 0, false, 0)
		if err != nil {
			return nil, bpderr.Attach(err, "collector", a.Name)
		}
		backoff, err := ScalePermyriad(int64(out.BackoffBaseTick), a.BackoffPermyriad)
		if err != nil {
			return nil, bpderr.Attach(err, "collector", a.Name)
		}
		exhausted, err := TickFromMs(a.BufferSaturatedDurationMs, out.TickMillis, "bufferSaturatedDurationMs")
		if err != nil {
			return nil, bpderr.Attach(err, "collector", a.Name)
		}
		policy := a.OverflowDisposition
		if policy == "" {
			policy = model.OverflowDropNewest
		}
		out.Agents = append(out.Agents, model.Agent{
			ID: a.Name, Host: a.HostEndpoint, BufferMaxBytes: buf,
			MaxBatchBytes: a.BatchByteCeiling, MaxBatchMessages: a.BatchRecordCeiling,
			BackoffTicks: int(backoff), PendingQueueLimit: a.PendingQueueCeiling,
			OverflowPolicy: policy, BufferExhaustedLimitTicks: exhausted,
		})
	}
	for _, p := range in.Partitions {
		rate, err := ScalePermyriad(out.RateBaseBytes, p.ConsumptionPermyriad)
		if err != nil {
			return nil, bpderr.Attach(err, "partition", p.Name)
		}
		delay, err := TickFromMs(p.AcknowledgementDelayMs, out.TickMillis, "acknowledgementDelayMs")
		if err != nil {
			return nil, bpderr.Attach(err, "partition", p.Name)
		}
		var watermark *int64
		if p.ConsumerWatermarkBytes != nil {
			v := *p.ConsumerWatermarkBytes
			watermark = &v
		}
		out.Partitions = append(out.Partitions, model.Partition{
			ID: p.Name, Index: p.Index, RateBytesPerTick: rate, AckDelayTicks: delay,
			BacklogLimitBytes: p.BacklogCeilingBytes, ConsumerWatermark: watermark,
			StrictStaleWatermark: p.StrictStaleWatermark,
		})
	}
	var walk func(rules []V3TreeRule, tenant, parent string) error
	walk = func(rules []V3TreeRule, tenant, parent string) error {
		for _, r := range rules {
			if r.RuleName == "" {
				return bpderr.New(bpderr.ESchemaInvalid, "tenantKeyTree.ruleName", "tree rule name is required")
			}
			out.Rules = append(out.Rules, model.KeyRule{
				ID: r.RuleName, Tenant: tenant, Key: r.MatchKey,
				PartitionID: r.PartitionName, ParentID: parent, Priority: r.Priority,
			})
			if err := walk(r.Children, tenant, r.RuleName); err != nil {
				return err
			}
		}
		return nil
	}
	for _, t := range in.TenantKeyTree {
		if err := walk(t.Rules, t.TenantID, ""); err != nil {
			return nil, err
		}
	}
	for _, b := range in.LogBatches {
		tick, err := TickFromMs(b.ArrivalMs, out.TickMillis, "arrivalAtMs")
		if err != nil {
			return nil, bpderr.Attach(err, "batchUid", b.BatchUID)
		}
		batch := model.Batch{ID: b.BatchUID, AgentID: b.Collector, Tenant: b.TenantID, Key: b.PartitionKey, ArrivalTick: tick}
		for _, e := range b.Entries {
			batch.Messages = append(batch.Messages, model.Message{ID: e.EntryUID, Bytes: e.PayloadBytes})
		}
		out.Batches = append(out.Batches, batch)
	}
	for _, u := range in.WatermarkUpdates {
		tick, err := TickFromMs(u.AtMs, out.TickMillis, "atMs")
		if err != nil {
			return nil, bpderr.Attach(err, "partitionName", u.PartitionName)
		}
		out.WatermarkUpdates = append(out.WatermarkUpdates, model.WatermarkUpdate{PartitionID: u.PartitionName, Tick: tick, Watermark: u.Watermark})
	}
	if err := ValidateProject(out); err != nil {
		return nil, err
	}
	return out, nil
}
