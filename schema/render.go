package schema

import (
	"encoding/json"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
)

func inverseScale(value, base, denominator int64) (int64, error) {
	if base == 0 || value*denominator%base != 0 {
		return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, "scale", "canonical value %d is not representable on scale base %d", value, base)
	}
	return value * denominator / base, nil
}

func ToV1(p *model.Project) (*V1Project, error) {
	out := &V1Project{
		Schema: V1, ProjectID: p.ID, ProjectVersion: p.Version, Seed: p.Seed,
		TickDurationMs: p.TickMillis, RateBaseBytes: p.RateBaseBytes,
		BackoffBaseTicks: p.BackoffBaseTick, TickCount: p.Ticks,
	}
	for _, a := range p.Agents {
		buf := a.BufferMaxBytes
		backoff, err := inverseScale(int64(a.BackoffTicks), int64(p.BackoffBaseTick), PermilleDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.ID)
		}
		out.Agents = append(out.Agents, V1Agent{
			AgentID: a.ID, Host: a.Host, SendBufferBytes: &buf,
			MaxBatchBytes: a.MaxBatchBytes, MaxBatchMessages: a.MaxBatchMessages,
			BackoffPermille: backoff, PendingQueueLimit: a.PendingQueueLimit,
			OverflowPolicy: a.OverflowPolicy, BufferExhaustedLimitTicks: a.BufferExhaustedLimitTicks,
		})
	}
	for _, part := range p.Partitions {
		rate, err := inverseScale(part.RateBytesPerTick, p.RateBaseBytes, PermilleDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "partition_id", part.ID)
		}
		out.Partitions = append(out.Partitions, V1Partition{
			PartitionID: part.ID, Index: part.Index, ConsumeRatePermille: rate,
			AckDelayTicks: part.AckDelayTicks, BacklogLimitBytes: part.BacklogLimitBytes,
			StrictStaleWatermark: part.StrictStaleWatermark,
		})
	}
	for _, r := range p.Rules {
		out.KeyRules = append(out.KeyRules, V1Rule{RuleID: r.ID, Tenant: r.Tenant, MatchKey: r.Key, PartitionID: r.PartitionID, ParentRuleID: r.ParentID, Priority: r.Priority})
	}
	for _, b := range p.Batches {
		v1b := V1Batch{BatchID: b.ID, AgentID: b.AgentID, Tenant: b.Tenant, PartitionKey: b.Key, ArrivalTick: b.ArrivalTick}
		for _, m := range b.Messages {
			v1b.Messages = append(v1b.Messages, V1Message{MessageID: m.ID, Bytes: m.Bytes})
		}
		out.Batches = append(out.Batches, v1b)
	}
	for _, u := range p.WatermarkUpdates {
		out.WatermarkUpdates = append(out.WatermarkUpdates, V1WatermarkUpdate{PartitionID: u.PartitionID, Tick: u.Tick, Watermark: u.Watermark})
	}
	return out, nil
}

func ToV2(p *model.Project, equivalentBytes int64) (*V2Project, error) {
	if equivalentBytes <= 0 {
		equivalentBytes = 8
	}
	out := &V2Project{
		Schema: V2, ProjectID: p.ID, ProjectVersion: p.Version, Seed: p.Seed,
		TickDurationMs: p.TickMillis, RateBaseBytes: p.RateBaseBytes,
		BackoffBaseTicks: p.BackoffBaseTick, EquivalentBytesPerMessage: equivalentBytes,
		TickCount: p.Ticks,
	}
	for _, a := range p.Agents {
		if a.BufferMaxBytes%equivalentBytes != 0 {
			return nil, bpderr.New(bpderr.ESchemaAmbiguous, "buffer", "buffer is not an integral equivalent-message count").WithContext("agent_id", a.ID)
		}
		equiv := int(a.BufferMaxBytes / equivalentBytes)
		backoff, err := inverseScale(int64(a.BackoffTicks), int64(p.BackoffBaseTick), PermyriadDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "agent_id", a.ID)
		}
		out.Agents = append(out.Agents, V2Agent{
			AgentID: a.ID, HostName: a.Host, BufferEquivMessages: &equiv,
			EquivalentBytesPerMessage: equivalentBytes, MaxBatchBytes: a.MaxBatchBytes,
			MaxBatchMessages: a.MaxBatchMessages, BackoffPermyriad: backoff,
			PendingQueueLimit: a.PendingQueueLimit, OverflowPolicy: a.OverflowPolicy,
			BufferExhaustedDurationMs: int64(a.BufferExhaustedLimitTicks) * p.TickMillis,
		})
	}
	for _, part := range p.Partitions {
		if part.BacklogLimitBytes%equivalentBytes != 0 {
			return nil, bpderr.New(bpderr.ESchemaAmbiguous, "backlogLimit", "backlog limit is not an integral equivalent-message count").WithContext("partition_id", part.ID)
		}
		backlogEquiv := int(part.BacklogLimitBytes / equivalentBytes)
		rate, err := inverseScale(part.RateBytesPerTick, p.RateBaseBytes, PermyriadDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "partition_id", part.ID)
		}
		out.Partitions = append(out.Partitions, V2Partition{
			PartitionID: part.ID, Index: part.Index, ConsumeRatePermyriad: rate,
			AckDelayMs:                int64(part.AckDelayTicks) * p.TickMillis,
			BacklogLimitEquivMessages: &backlogEquiv, StrictStaleWatermark: part.StrictStaleWatermark,
		})
	}
	for _, r := range p.Rules {
		out.KeyRules = append(out.KeyRules, V2Rule{RuleID: r.ID, TenantID: r.Tenant, MatchKey: r.Key, PartitionID: r.PartitionID, ParentRuleID: r.ParentID, Priority: r.Priority})
	}
	for _, b := range p.Batches {
		v2b := V2Batch{BatchID: b.ID, AgentID: b.AgentID, TenantID: b.Tenant, PartitionKey: b.Key, ArrivalMs: int64(b.ArrivalTick) * p.TickMillis}
		for _, m := range b.Messages {
			v2b.Messages = append(v2b.Messages, V2Message{ID: m.ID, Bytes: m.Bytes})
		}
		out.Batches = append(out.Batches, v2b)
	}
	for _, u := range p.WatermarkUpdates {
		out.WatermarkUpdates = append(out.WatermarkUpdates, V2WatermarkUpdate{PartitionID: u.PartitionID, AtMs: int64(u.Tick) * p.TickMillis, Watermark: u.Watermark})
	}
	return out, nil
}

func ToV3(p *model.Project) (*V3Project, error) {
	out := &V3Project{
		APIVersion:   "collector.pipeline/v3",
		Metadata:     V3Metadata{ProjectID: p.ID, Revision: p.Version, Seed: p.Seed},
		Time:         V3Time{TickIntervalMs: p.TickMillis},
		Scales:       V3Scales{RateBaseBytesPerTick: p.RateBaseBytes, BackoffBaseTicks: p.BackoffBaseTick},
		HorizonTicks: p.Ticks,
	}
	for _, a := range p.Agents {
		backoff, err := inverseScale(int64(a.BackoffTicks), int64(p.BackoffBaseTick), PermyriadDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "collector", a.ID)
		}
		out.Collectors = append(out.Collectors, V3Collector{
			Name: a.ID, HostEndpoint: a.Host, BufferLimitBytes: a.BufferMaxBytes,
			BatchByteCeiling: a.MaxBatchBytes, BatchRecordCeiling: a.MaxBatchMessages,
			BackoffPermyriad: backoff, PendingQueueCeiling: a.PendingQueueLimit,
			OverflowDisposition:       a.OverflowPolicy,
			BufferSaturatedDurationMs: int64(a.BufferExhaustedLimitTicks) * p.TickMillis,
		})
	}
	for _, part := range p.Partitions {
		rate, err := inverseScale(part.RateBytesPerTick, p.RateBaseBytes, PermyriadDenominator)
		if err != nil {
			return nil, bpderr.Attach(err, "partition", part.ID)
		}
		out.Partitions = append(out.Partitions, V3Partition{
			Name: part.ID, Index: part.Index, ConsumptionPermyriad: rate,
			AcknowledgementDelayMs: int64(part.AckDelayTicks) * p.TickMillis,
			BacklogCeilingBytes:    part.BacklogLimitBytes,
			ConsumerWatermarkBytes: part.ConsumerWatermark,
			StrictStaleWatermark:   part.StrictStaleWatermark,
		})
	}

	children := map[string][]model.KeyRule{}
	var roots []model.KeyRule
	for _, r := range p.Rules {
		if r.ParentID == "" {
			roots = append(roots, r)
		} else {
			children[r.ParentID] = append(children[r.ParentID], r)
		}
	}
	tenantOrder := []string{}
	byTenant := map[string][]V3TreeRule{}
	var makeTree func(r model.KeyRule) V3TreeRule
	makeTree = func(r model.KeyRule) V3TreeRule {
		node := V3TreeRule{RuleName: r.ID, MatchKey: r.Key, PartitionName: r.PartitionID, Priority: r.Priority}
		for _, child := range children[r.ID] {
			node.Children = append(node.Children, makeTree(child))
		}
		return node
	}
	for _, r := range roots {
		if _, ok := byTenant[r.Tenant]; !ok {
			tenantOrder = append(tenantOrder, r.Tenant)
		}
		byTenant[r.Tenant] = append(byTenant[r.Tenant], makeTree(r))
	}
	for _, tenant := range tenantOrder {
		out.TenantKeyTree = append(out.TenantKeyTree, V3Tenant{TenantID: tenant, Rules: byTenant[tenant]})
	}

	for _, b := range p.Batches {
		v3b := V3Batch{BatchUID: b.ID, Collector: b.AgentID, TenantID: b.Tenant, PartitionKey: b.Key, ArrivalMs: int64(b.ArrivalTick) * p.TickMillis}
		for _, m := range b.Messages {
			v3b.Entries = append(v3b.Entries, V3Entry{EntryUID: m.ID, PayloadBytes: m.Bytes})
		}
		out.LogBatches = append(out.LogBatches, v3b)
	}
	for _, u := range p.WatermarkUpdates {
		out.WatermarkUpdates = append(out.WatermarkUpdates, V3WatermarkUpdate{PartitionName: u.PartitionID, AtMs: int64(u.Tick) * p.TickMillis, Watermark: u.Watermark})
	}
	return out, nil
}

func RenderJSON(version string, p *model.Project) ([]byte, error) {
	var doc any
	var err error
	switch version {
	case V1:
		doc, err = ToV1(p)
	case V2:
		doc, err = ToV2(p, 4)
	case V3:
		doc, err = ToV3(p)
	default:
		return nil, bpderr.New(bpderr.ESchemaUnsupported, "version", "unknown render schema version")
	}
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, bpderr.Wrap(bpderr.ESchemaInvalid, "json", err, "failed to render schema document")
	}
	return data, nil
}
