package main

import (
	"fmt"
	"os"
	"path/filepath"

	"warmcache/internal/domain"
	"warmcache/internal/engine"
	"warmcache/internal/gen"
	"warmcache/internal/loader"
	"warmcache/internal/store"
)

func verifyInstallation(seed int64) error {
	samples, err := gen.Samples(seed)
	if err != nil {
		return err
	}
	projects := map[domain.SchemaVersion]domain.Project{}
	for _, sample := range samples {
		project, err := loader.Load(sample.Raw, sample.Version)
		if err != nil {
			return err
		}
		projects[sample.Version] = project
	}
	if err := domain.MustEqualProjects(projects[domain.SchemaV1], projects[domain.SchemaV2]); err != nil {
		return err
	}
	if err := domain.MustEqualProjects(projects[domain.SchemaV1], projects[domain.SchemaV3]); err != nil {
		return err
	}
	fmt.Printf("PASS schema normalization: v1=v2=v3 %s\n", projects[domain.SchemaV1].SemanticHash()[:16])

	var baseline domain.RunResult
	for _, version := range []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		result, err := engine.Run(projects[version], gen.Requests(seed), domain.Options{Ticks: 30, Workers: 3})
		if err != nil {
			return err
		}
		if baseline.RunID == "" {
			baseline = result
		} else if err := engine.CompareResults(baseline, result); err != nil {
			return err
		}
	}
	fmt.Printf("PASS lease/stagger timeline: %d ticks, %d leases, %d commits\n",
		len(baseline.Timeline), len(baseline.Leases), availableCount(baseline.Shards))

	gateCodes := map[string]bool{}
	for _, request := range baseline.Requests {
		if request.Code != "" {
			gateCodes[request.Code] = true
		}
	}
	if !gateCodes[domain.CodeReadFreshUnavailable] ||
		!gateCodes[domain.CodeWriteVersionConflict] ||
		!gateCodes[domain.CodeNotFound] {
		return fmt.Errorf("gate verification did not exercise rejection causes: %v", gateCodes)
	}
	fmt.Printf("PASS write gate: fallback, defer, reject causes=%v\n", gateCodes)

	interrupted, err := engine.Run(projects[domain.SchemaV2], gen.Requests(seed), domain.Options{
		Ticks: 30, Workers: 3,
		Interrupts: []domain.Interrupt{{ID: "verify-int", Tick: 6, ShardID: "shard-03", Reason: "verify"}},
	})
	if err != nil {
		return err
	}
	if !shardAvailable(interrupted.Shards, "shard-03") {
		return fmt.Errorf("interrupted shard did not resume")
	}
	fmt.Println("PASS interruption resume: shard-03 committed after re-lease")

	service := engine.NewService(projects[domain.SchemaV3], gen.Requests(seed))
	_, _, err = service.CompareFull(domain.Options{
		Ticks: 30, Workers: 3,
		Edits: []domain.Edit{
			{ID: "verify-bump", Kind: domain.EditBumpShard, AtTick: 7, ShardID: "shard-03"},
			{ID: "verify-retire", Kind: domain.EditRetireNode, AtTick: 11, NodeID: "edge-b"},
			{ID: "verify-budget", Kind: domain.EditSetBudget, AtTick: 14, BudgetMilli: 150},
		},
	})
	if err != nil {
		return err
	}
	fmt.Println("PASS incremental invalidation: incremental equals full recompute")

	directory, err := os.MkdirTemp("", "warmcache-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "checkpoint.sqlite")
	checkpoint, err := store.Open(path)
	if err != nil {
		return err
	}
	if err := checkpoint.SaveResult(baseline); err != nil {
		_ = checkpoint.Close()
		return err
	}
	if err := checkpoint.Close(); err != nil {
		return err
	}
	reopened, err := store.Open(path)
	if err != nil {
		return err
	}
	latest, err := reopened.Latest()
	if err != nil {
		_ = reopened.Close()
		return err
	}
	if err := reopened.Close(); err != nil {
		return err
	}
	if latest.Tick != 30 || latest.StateHash != store.TickHash(baseline.Timeline[29]) {
		return fmt.Errorf("checkpoint recovery mismatch")
	}
	fmt.Printf("PASS SQLite recovery: tick=%d snapshot=%s\n", latest.Tick, latest.SnapshotVersion)
	return nil
}

func availableCount(shards []domain.ShardState) int {
	count := 0
	for _, shard := range shards {
		if shard.Available {
			count++
		}
	}
	return count
}

func shardAvailable(shards []domain.ShardState, shardID string) bool {
	for _, shard := range shards {
		if shard.ShardID == shardID {
			return shard.Available
		}
	}
	return false
}
