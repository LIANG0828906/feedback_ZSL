// Package planner assigns keys and splits batches into sendable chunks.
package planner

import (
	"sort"
	"strconv"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/schema"
)

func AssignPartition(p *model.Project, tenant, key string) (string, error) {
	type candidate struct {
		rule  model.KeyRule
		score int
	}
	var candidates []candidate
	for _, rule := range p.Rules {
		tenantMatch := rule.Tenant == tenant || rule.Tenant == "*"
		keyMatch := rule.Key == key || rule.Key == "*"
		if !tenantMatch || !keyMatch {
			continue
		}
		score := rule.Priority
		if rule.Tenant == tenant {
			score += 2
		}
		if rule.Key == key {
			score++
		}
		candidates = append(candidates, candidate{rule: rule, score: score})
	}
	if len(candidates) == 0 {
		return "", bpderr.New(bpderr.EKeyRuleAmbiguous, "key_rules", "no partition key rule matches").
			WithContext("tenant", tenant).WithContext("key", key)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].rule.ID < candidates[j].rule.ID
	})
	bestScore := candidates[0].score
	chosen := candidates[0].rule.PartitionID
	for _, c := range candidates {
		if c.score < bestScore {
			break
		}
		if c.rule.PartitionID != chosen {
			return "", bpderr.New(bpderr.EKeyRuleAmbiguous, "key_rules", "rules with equal specificity map to different partitions").
				WithContext("tenant", tenant).WithContext("key", key).
				WithContext("rule_ids", []string{candidates[0].rule.ID, c.rule.ID})
		}
	}
	return chosen, nil
}

func BuildChunks(p *model.Project) ([]model.Chunk, error) {
	if err := schema.ValidateProject(p); err != nil {
		return nil, err
	}
	agentByID := map[string]model.Agent{}
	for _, a := range p.Agents {
		agentByID[a.ID] = a
	}
	var chunks []model.Chunk
	for _, batch := range p.Batches {
		agent := agentByID[batch.AgentID]
		partitionID, err := AssignPartition(p, batch.Tenant, batch.Key)
		if err != nil {
			return nil, bpderr.Attach(err, "batch_id", batch.ID)
		}
		var groups [][]model.Message
		var groupRanges [][2]int
		var current []model.Message
		var currentBytes int64
		start := 0
		flush := func() {
			if len(current) > 0 {
				groups = append(groups, current)
				groupRanges = append(groupRanges, [2]int{start, start + len(current) - 1})
			}
			current = nil
			currentBytes = 0
			start = 0
		}
		for messageIndex, message := range batch.Messages {
			if message.Bytes > agent.MaxBatchBytes {
				return nil, bpderr.New(bpderr.EBatchTooLarge, "messages.bytes", "message exceeds agent max batch bytes").
					WithContext("batch_id", batch.ID).WithContext("message_id", message.ID).
					WithContext("message_bytes", message.Bytes).WithContext("max_batch_bytes", agent.MaxBatchBytes)
			}
			if len(current) >= agent.MaxBatchMessages || (len(current) > 0 && currentBytes+message.Bytes > agent.MaxBatchBytes) {
				flush()
			}
			if len(current) == 0 {
				start = messageIndex
			}
			current = append(current, message)
			currentBytes += message.Bytes
			if currentBytes > agent.MaxBatchBytes {
				flush()
			}
		}
		flush()
		for i, group := range groups {
			var total int64
			for _, m := range group {
				total += m.Bytes
			}
			chunks = append(chunks, model.Chunk{
				ChunkID: batch.ID + "#" + strconv.Itoa(i+1),
				BatchID: batch.ID, AgentID: batch.AgentID, Tenant: batch.Tenant, Key: batch.Key,
				PartitionID: partitionID, ArrivalTick: batch.ArrivalTick,
				SplitIndex: i + 1, SplitCount: len(groups),
				FirstMessageIndex: groupRanges[i][0], LastMessageIndex: groupRanges[i][1],
				Messages: group, Count: len(group), Bytes: total,
			})
		}
	}
	return chunks, nil
}
