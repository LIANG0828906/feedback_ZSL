package gen

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"

	"warmcache/internal/domain"
)

const (
	DefaultSeed  int64 = 4242
	DefaultTicks int64 = 30
)

type VersionedSample struct {
	Version domain.SchemaVersion `json:"version"`
	Schema  string               `json:"schema"`
	Raw     json.RawMessage      `json:"raw"`
	Project domain.Project       `json:"project"`
}

func Project(seed int64) domain.Project {
	if seed == 0 {
		seed = DefaultSeed
	}
	nodes := []domain.Node{
		{ID: "edge-a", Group: "g1", JoinTick: 1, ClaimIntervalTicks: 2, LeaseTicks: 6, MaxConcurrent: 2, BufferLimit: 2},
		{ID: "edge-b", Group: "g1", JoinTick: 1, ClaimIntervalTicks: 3, LeaseTicks: 6, MaxConcurrent: 2, BufferLimit: 2},
		{ID: "edge-c", Group: "g2", JoinTick: 2, ClaimIntervalTicks: 2, LeaseTicks: 6, MaxConcurrent: 2, BufferLimit: 3},
	}
	costs := []int64{80, 80, 80, 70, 90, 60, 100, 70, 90}
	preheat := []int64{4, 5, 4, 3, 4, 5, 4, 3, 4}
	nodesForShard := []string{"edge-a", "edge-b", "edge-c", "edge-a", "edge-b", "edge-c", "edge-a", "edge-b", "edge-c"}
	groups := []string{"g1", "g1", "g2", "g1", "g1", "g2", "g1", "g1", "g2"}
	shards := make([]domain.Shard, 0, len(costs))
	for index := range costs {
		id := fmt.Sprintf("shard-%02d", index+1)
		shards = append(shards, domain.Shard{
			ID:              id,
			NodeID:          nodesForShard[index],
			Group:           groups[index],
			Digest:          Fingerprint(seed, id),
			SourceVersion:   4,
			PreviousVersion: 3,
			BandwidthMilli:  costs[index],
			PreheatTicks:    preheat[index],
		})
	}
	project := domain.Project{
		ID:          fmt.Sprintf("cache-warm-seed-%d", seed),
		Seed:        seed,
		TickMS:      250,
		Watermark:   1000,
		BudgetMilli: 200,
		BudgetBasis: 1000,
		Nodes:       nodes,
		Shards:      shards,
	}
	project.Normalize()
	return project
}

func Fingerprint(seed int64, shardID string) uint64 {
	hasher := fnv.New64a()
	_, _ = fmt.Fprintf(hasher, "%d:%s", seed, shardID)
	return hasher.Sum64() & 0x0000FFFFFFFFFFFF
}

func Requests(seed int64) []domain.Request {
	if seed == 0 {
		seed = DefaultSeed
	}
	return []domain.Request{
		{ID: "r-01", Tick: 2, Kind: domain.RequestRead, ShardID: "shard-01"},
		{ID: "w-01", Tick: 2, Kind: domain.RequestWrite, ShardID: "shard-01", Version: 4, Payload: "replay-a"},
		{ID: "r-02", Tick: 3, Kind: domain.RequestRead, ShardID: "shard-02", Fresh: true},
		{ID: "w-02", Tick: 3, Kind: domain.RequestWrite, ShardID: "shard-02", Version: 2, Payload: "stale-b"},
		{ID: "w-03", Tick: 3, Kind: domain.RequestWrite, ShardID: "shard-03", Version: 4, Payload: "replay-c"},
		{ID: "r-03", Tick: 5, Kind: domain.RequestRead, ShardID: "shard-01"},
		{ID: "w-04", Tick: 5, Kind: domain.RequestWrite, ShardID: "shard-01", Version: 4, Payload: "edge-visible"},
		{ID: "r-04", Tick: 6, Kind: domain.RequestRead, ShardID: "shard-03"},
		{ID: "r-05", Tick: 7, Kind: domain.RequestRead, ShardID: "missing-shard"},
		{ID: "r-06", Tick: 18, Kind: domain.RequestRead, ShardID: "shard-03"},
		{ID: "w-05", Tick: 18, Kind: domain.RequestWrite, ShardID: "shard-03", Version: 4, Payload: "late-c"},
	}
}

func Raw(project domain.Project, version domain.SchemaVersion) ([]byte, error) {
	var value any
	switch version {
	case domain.SchemaV1:
		value = rawV1(project)
	case domain.SchemaV2:
		value = rawV2(project)
	case domain.SchemaV3:
		value = rawV3(project)
	default:
		return nil, fmt.Errorf("unsupported version %q", version)
	}
	return json.MarshalIndent(value, "", "  ")
}

func Samples(seed int64) ([]VersionedSample, error) {
	base := Project(seed)
	versions := []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3}
	samples := make([]VersionedSample, 0, len(versions))
	for _, version := range versions {
		project := base.Clone()
		project.Schema = version
		raw, err := Raw(project, version)
		if err != nil {
			return nil, err
		}
		var schema struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, err
		}
		samples = append(samples, VersionedSample{
			Version: version,
			Schema:  schema.Schema,
			Raw:     raw,
			Project: project,
		})
	}
	return samples, nil
}

func WriteSamples(directory string, seed int64) ([]string, error) {
	samples, err := Samples(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(samples))
	for _, sample := range samples {
		path := filepath.Join(directory, string(sample.Version)+".json")
		if err := os.WriteFile(path, append(sample.Raw, '\n'), 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func rawNodes(project domain.Project, camel bool, durationMS bool) []any {
	result := make([]any, 0, len(project.Nodes))
	for _, node := range project.Nodes {
		item := map[string]any{}
		if camel {
			item["id"] = node.ID
			item["group"] = node.Group
			item["joinTick"] = node.JoinTick
			item["claimIntervalMs"] = node.ClaimIntervalTicks * project.TickMS
			item["leaseMs"] = node.LeaseTicks * project.TickMS
			item["maxConcurrent"] = node.MaxConcurrent
			item["bufferLimit"] = node.BufferLimit
		} else {
			item["node_id"] = node.ID
			item["replica_group"] = node.Group
			item["join_tick"] = node.JoinTick
			item["claim_interval_ticks"] = node.ClaimIntervalTicks
			item["lease_ticks"] = node.LeaseTicks
			item["max_concurrent"] = node.MaxConcurrent
			item["buffer_limit"] = node.BufferLimit
		}
		result = append(result, item)
	}
	return result
}

func rawV1(project domain.Project) map[string]any {
	shards := make([]any, 0, len(project.Shards))
	for _, shard := range project.Shards {
		shards = append(shards, map[string]any{
			"shard_id":           shard.ID,
			"node_id":            shard.NodeID,
			"replica_group":      shard.Group,
			"digest":             fmt.Sprintf("0x%016x", shard.Digest),
			"source_version":     shard.SourceVersion,
			"previous_version":   shard.PreviousVersion,
			"bandwidth_permille": shard.BandwidthMilli,
			"preheat_ticks":      shard.PreheatTicks,
		})
	}
	return map[string]any{
		"schema":         "cache-warm.v1",
		"project_id":     project.ID,
		"seed":           project.Seed,
		"source_version": 4,
		"clock": map[string]any{
			"tick_ms":       project.TickMS,
			"lease_ticks":   6,
			"preheat_ticks": 4,
		},
		"bandwidth": map[string]any{
			"unit":  "permille",
			"total": project.BudgetMilli,
		},
		"nodes":  rawNodes(project, false, false),
		"shards": shards,
	}
}

func rawV2(project domain.Project) map[string]any {
	shards := make([]any, 0, len(project.Shards))
	for _, shard := range project.Shards {
		shards = append(shards, map[string]any{
			"shard":           shard.ID,
			"node":            shard.NodeID,
			"group":           shard.Group,
			"digest":          fmt.Sprintf("0x%016x", shard.Digest),
			"sourceVersion":   shard.SourceVersion,
			"previousVersion": shard.PreviousVersion,
			"bandwidthBps":    shard.BandwidthMilli * 10,
			"preheatMs":       shard.PreheatTicks * project.TickMS,
		})
	}
	return map[string]any{
		"schema":        "cache-warm.v2",
		"projectId":     project.ID,
		"seed":          project.Seed,
		"sourceVersion": 4,
		"clock": map[string]any{
			"tickMs":    project.TickMS,
			"leaseMs":   1500,
			"preheatMs": 1000,
		},
		"bandwidth": map[string]any{
			"scale":  "basis_points",
			"budget": project.BudgetMilli * 10,
		},
		"nodes":        rawNodes(project, true, true),
		"replicaTable": shards,
	}
}

func rawV3(project domain.Project) map[string]any {
	groupOrder := []string{"g1", "g2"}
	groups := make([]any, 0, len(groupOrder))
	for _, group := range groupOrder {
		nodes := make([]any, 0)
		for _, node := range project.Nodes {
			if node.Group != group {
				continue
			}
			nodes = append(nodes, map[string]any{
				"node_id":           node.ID,
				"join_tick":         node.JoinTick,
				"claim_interval_ms": node.ClaimIntervalTicks * project.TickMS,
				"lease_ms":          node.LeaseTicks * project.TickMS,
				"max_concurrent":    node.MaxConcurrent,
				"buffer_limit":      node.BufferLimit,
			})
		}
		replicas := make([]any, 0)
		for _, shard := range project.Shards {
			if shard.Group != group {
				continue
			}
			replicas = append(replicas, map[string]any{
				"shard_id":         shard.ID,
				"node_id":          shard.NodeID,
				"digest":           shard.Digest,
				"source_version":   shard.SourceVersion,
				"previous_version": shard.PreviousVersion,
				"bandwidth_bps":    shard.BandwidthMilli * 10,
				"preheat_ms":       shard.PreheatTicks * project.TickMS,
			})
		}
		groups = append(groups, map[string]any{
			"group":    group,
			"members":  nodes,
			"replicas": replicas,
		})
	}
	return map[string]any{
		"schema":         "cache-warm.v3",
		"project":        project.ID,
		"seed":           project.Seed,
		"source_version": 4,
		"clock": map[string]any{
			"tick_millis":     project.TickMS,
			"lease_seconds":   1.5,
			"preheat_seconds": 1,
		},
		"bandwidth": map[string]any{
			"unit":  "basis_points",
			"limit": project.BudgetMilli * 10,
		},
		"preheat_watermark": 1000,
		"replica_groups":    groups,
	}
}
