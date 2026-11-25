package schema

import (
	"fmt"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
)

const (
	PermilleDenominator  int64 = 1000
	PermyriadDenominator int64 = 10000
)

func ScalePermille(base, value int64) (int64, error) {
	if value < 0 {
		return 0, bpderr.New(bpderr.ESchemaInvalid, "scale", "negative permille scale")
	}
	numerator := base * value
	if numerator%PermilleDenominator != 0 {
		return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, "scale", "permille value %d does not normalize exactly from base %d", value, base)
	}
	return numerator / PermilleDenominator, nil
}

func ScalePermyriad(base, value int64) (int64, error) {
	if value < 0 {
		return 0, bpderr.New(bpderr.ESchemaInvalid, "scale", "negative permyriad scale")
	}
	numerator := base * value
	if numerator%PermyriadDenominator != 0 {
		return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, "scale", "permyriad value %d does not normalize exactly from base %d", value, base)
	}
	return numerator / PermyriadDenominator, nil
}

func NormalizeBuffer(explicitBytes int64, hasBytes bool, equivMessages int, hasEquiv bool, equivBytes int64) (int64, error) {
	value, err := NormalizeByteOrEquiv(explicitBytes, hasBytes, equivMessages, hasEquiv, equivBytes, "buffer")
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, bpderr.New(bpderr.EBufferLimitZero, "send_buffer", "agent send buffer limit is zero")
	}
	return value, nil
}

func NormalizeByteOrEquiv(explicitBytes int64, hasBytes bool, equivMessages int, hasEquiv bool, equivBytes int64, field string) (int64, error) {
	switch {
	case hasBytes && hasEquiv:
		if equivBytes <= 0 {
			return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field+".equiv_bytes_per_message", "equivalent-message %s requires a positive byte equivalence", field)
		}
		fromEquiv := int64(equivMessages) * equivBytes
		if explicitBytes != fromEquiv {
			return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field, "byte quantity %d disagrees with %d equivalent messages * %d bytes = %d", explicitBytes, equivMessages, equivBytes, fromEquiv)
		}
		return explicitBytes, nil
	case hasBytes:
		if explicitBytes < 0 {
			return 0, bpderr.Newf(bpderr.ESchemaInvalid, field, "%s byte quantity is negative", field)
		}
		return explicitBytes, nil
	case hasEquiv:
		if equivBytes <= 0 {
			return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field+".equiv_bytes_per_message", "equivalent-message %s requires a positive byte equivalence", field)
		}
		if equivMessages <= 0 {
			return 0, bpderr.Newf(bpderr.ESchemaInvalid, field, "equivalent-message %s quantity must be positive", field)
		}
		return int64(equivMessages) * equivBytes, nil
	default:
		return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field, "neither byte nor equivalent-message expression is present for %s", field)
	}
}

func TickFromMs(ms, tickMillis int64, field string) (int, error) {
	if tickMillis <= 0 {
		return 0, bpderr.New(bpderr.ESchemaInvalid, "tick_duration_ms", "tick duration must be positive")
	}
	if ms < 0 {
		return 0, bpderr.New(bpderr.ESchemaInvalid, field, "millisecond time must not be negative")
	}
	if ms%tickMillis != 0 {
		return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field, "time %dms is not an exact multiple of tick duration %dms", ms, tickMillis)
	}
	return int(ms / tickMillis), nil
}

func NormalizeTickTime(explicitTick int, hasTick bool, ms int64, hasMS bool, tickMillis int64, field string) (int, error) {
	switch {
	case hasTick && hasMS:
		tick, err := TickFromMs(ms, tickMillis, field)
		if err != nil {
			return 0, err
		}
		if tick != explicitTick {
			return 0, bpderr.Newf(bpderr.ESchemaAmbiguous, field, "tick %d disagrees with %dms (tick=%d)", explicitTick, ms, tick)
		}
		return explicitTick, nil
	case hasTick:
		if explicitTick < 0 {
			return 0, bpderr.New(bpderr.ESchemaInvalid, field, "tick must not be negative")
		}
		return explicitTick, nil
	case hasMS:
		return TickFromMs(ms, tickMillis, field)
	default:
		return 0, bpderr.New(bpderr.ESchemaAmbiguous, field, "neither tick nor millisecond time is present")
	}
}

func ValidateProject(p *model.Project) error {
	if p.ID == "" {
		return bpderr.New(bpderr.ESchemaInvalid, "project_id", "project id is required")
	}
	if p.Version == "" {
		return bpderr.New(bpderr.ESchemaInvalid, "project_version", "project version is required for checkpoint matching")
	}
	if p.TickMillis <= 0 {
		return bpderr.New(bpderr.ESchemaInvalid, "tick_duration_ms", "tick duration must be positive")
	}
	if p.RateBaseBytes <= 0 {
		return bpderr.New(bpderr.ESchemaInvalid, "rate_base_bytes_per_tick", "rate base must be positive")
	}
	if len(p.Agents) == 0 || len(p.Partitions) == 0 || len(p.Batches) == 0 {
		return bpderr.New(bpderr.ESchemaInvalid, "project", "agents, partitions and batches must not be empty")
	}

	agentIDs := map[string]struct{}{}
	for i := range p.Agents {
		a := &p.Agents[i]
		if a.ID == "" {
			return bpderr.New(bpderr.ESchemaInvalid, fmt.Sprintf("agents[%d].id", i), "agent id is required")
		}
		if _, ok := agentIDs[a.ID]; ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "agents.id", "duplicate agent id %q", a.ID)
		}
		agentIDs[a.ID] = struct{}{}
		if a.BufferMaxBytes == 0 {
			return bpderr.New(bpderr.EBufferLimitZero, "agents.send_buffer", "agent %s buffer limit is zero").WithContext("agent_id", a.ID)
		}
		if a.BufferMaxBytes < 0 {
			return bpderr.New(bpderr.ESchemaInvalid, "agents.send_buffer", "agent %s buffer limit is negative").WithContext("agent_id", a.ID)
		}
		if a.MaxBatchBytes <= 0 || a.MaxBatchMessages <= 0 {
			return bpderr.New(bpderr.ESchemaInvalid, "agents.max_batch", "agent %s batch limits must be positive").WithContext("agent_id", a.ID)
		}
		if a.BackoffTicks < 0 {
			return bpderr.New(bpderr.ESchemaInvalid, "agents.backoff", "agent %s backoff must not be negative").WithContext("agent_id", a.ID)
		}
		if a.PendingQueueLimit < 0 {
			return bpderr.New(bpderr.EBudgetOutOfRange, "agents.pending_queue_limit", "pending queue budget must be positive").
				WithContext("agent_id", a.ID).WithContext("value", a.PendingQueueLimit)
		}
		if a.BufferExhaustedLimitTicks < 0 {
			return bpderr.New(bpderr.ESchemaInvalid, "agents.buffer_exhausted_ticks", "buffer exhausted duration must not be negative")
		}
	}

	partitionIDs := map[string]struct{}{}
	for i := range p.Partitions {
		pd := &p.Partitions[i]
		if pd.ID == "" {
			return bpderr.New(bpderr.ESchemaInvalid, fmt.Sprintf("partitions[%d].id", i), "partition id is required")
		}
		if _, ok := partitionIDs[pd.ID]; ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "partitions.id", "duplicate partition id %q", pd.ID)
		}
		partitionIDs[pd.ID] = struct{}{}
		if pd.RateBytesPerTick < 0 || pd.AckDelayTicks < 0 || pd.BacklogLimitBytes <= 0 {
			return bpderr.New(bpderr.ESchemaInvalid, "partitions", "partition %s has invalid rate/delay/backlog limit").WithContext("partition_id", pd.ID)
		}
	}

	ruleIDs := map[string]struct{}{}
	for _, r := range p.Rules {
		if r.ID == "" {
			return bpderr.New(bpderr.ESchemaInvalid, "key_rules.id", "rule id is required")
		}
		if _, ok := ruleIDs[r.ID]; ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "key_rules.id", "duplicate rule id %q", r.ID)
		}
		ruleIDs[r.ID] = struct{}{}
		if _, ok := partitionIDs[r.PartitionID]; !ok {
			return bpderr.New(bpderr.ESchemaInvalid, "key_rules.partition_id", "rule %s references unknown partition %s").
				WithContext("rule_id", r.ID).WithContext("partition_id", r.PartitionID)
		}
	}
	if err := ValidateRuleGraph(p.Rules); err != nil {
		return err
	}

	batchIDs := map[string]struct{}{}
	for i := range p.Batches {
		b := &p.Batches[i]
		if b.ID == "" {
			return bpderr.New(bpderr.ESchemaInvalid, fmt.Sprintf("batches[%d].id", i), "batch id is required")
		}
		if _, ok := batchIDs[b.ID]; ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "batches.id", "duplicate batch id %q", b.ID)
		}
		batchIDs[b.ID] = struct{}{}
		if _, ok := agentIDs[b.AgentID]; !ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "batches.agent_id", "batch %s references unknown agent %s", b.ID, b.AgentID).
				WithContext("batch_id", b.ID).WithContext("agent_id", b.AgentID)
		}
		if b.ArrivalTick < 0 || b.ArrivalTick >= p.Ticks {
			return bpderr.Newf(bpderr.ESchemaInvalid, "batches.arrival_tick", "batch %s arrival tick %d outside [0,%d)", b.ID, b.ArrivalTick, p.Ticks).
				WithContext("batch_id", b.ID)
		}
		if len(b.Messages) == 0 {
			return bpderr.Newf(bpderr.ESchemaInvalid, "batches.messages", "batch %s has no messages", b.ID)
		}
	}
	for _, u := range p.WatermarkUpdates {
		if _, ok := partitionIDs[u.PartitionID]; !ok {
			return bpderr.Newf(bpderr.ESchemaInvalid, "watermark_updates.partition_id", "unknown partition %s", u.PartitionID)
		}
		if u.Tick < 0 || u.Tick >= p.Ticks {
			return bpderr.New(bpderr.ESchemaInvalid, "watermark_updates.tick", "update tick outside simulation range")
		}
	}
	return nil
}

func ValidateRuleGraph(rules []model.KeyRule) error {
	byID := map[string]model.KeyRule{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		st := state[id]
		if st == 1 {
			cycle = append(cycle, id)
			return true
		}
		if st == 2 {
			return false
		}
		r, ok := byID[id]
		if !ok || r.ParentID == "" {
			state[id] = 2
			return false
		}
		state[id] = 1
		if visit(r.ParentID) {
			cycle = append(cycle, id)
			return true
		}
		state[id] = 2
		return false
	}
	for _, r := range rules {
		if visit(r.ID) {
			return bpderr.New(bpderr.EKeyRuleCycle, "key_rules.parent_id", "partition key rule parent graph contains a cycle").
				WithContext("cycle", cycle)
		}
	}
	for _, r := range rules {
		if r.ParentID != "" {
			if _, ok := byID[r.ParentID]; !ok {
				return bpderr.Newf(bpderr.ESchemaInvalid, "key_rules.parent_id", "rule %s references missing parent %s", r.ID, r.ParentID)
			}
		}
	}
	return nil
}
