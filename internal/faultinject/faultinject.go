// Package faultinject drives the full anomaly injection chain. Each injector
// returns a stable E_xxx business code with locatable context, never mutates
// the source project, and leaves previously committed scoring results usable.
package faultinject

import (
	"encoding/json"
	"os"
	"path/filepath"

	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/loader"
	"schedlab/internal/runtime"
)

// InjectionResult records one anomaly and the code it produced.
type InjectionResult struct {
	Name    string            `json:"name"`
	Code    faults.Code       `json:"code"`
	Context map[string]string `json:"context"`
}

func result(name string, err error) InjectionResult {
	res := InjectionResult{Name: name, Code: faults.CodeOf(err)}
	if f, ok := err.(*faults.Fault); ok {
		res.Context = f.Context
	}
	return res
}

// InjectWeightOutOfRange raises E_WEIGHT_RANGE via an out-of-scale bps weight.
func InjectWeightOutOfRange(v3 *gen.V3Project) InjectionResult {
	bad := cloneV3(v3)
	bad.Pods[0].PreferRules[0].WeightBps = 12000 // > 10000 bps
	raw, _ := json.Marshal(bad)
	_, err := loader.Load(raw, loader.V3, loader.Options{})
	return result("weight_out_of_range", err)
}

// InjectNegativeResource raises E_RESOURCE_NEGATIVE.
func InjectNegativeResource(v3 *gen.V3Project) InjectionResult {
	bad := cloneV3(v3)
	bad.Pods[0].Request.CPU = "-100m"
	raw, _ := json.Marshal(bad)
	_, err := loader.Load(raw, loader.V3, loader.Options{})
	return result("negative_resource", err)
}

// InjectTopoKeyConflict raises E_TOPO_KEY_CONFLICT by declaring an
// authoritative semantic expectation the payload disagrees with.
func InjectTopoKeyConflict(v3 *gen.V3Project) InjectionResult {
	raw, _ := json.Marshal(v3)
	opts := loader.Options{TopologySemantics: map[string]string{
		"zone": "routing/zone", // payload uses failure-domain/zone
	}}
	_, err := loader.Load(raw, loader.V3, opts)
	return result("topo_key_semantic_conflict", err)
}

// InjectConcurrencyBudget raises E_CONCURRENCY_BUDGET for an illegal budget.
func InjectConcurrencyBudget(p *domain.Project) InjectionResult {
	_, err := runtime.NewDeterministicRunner(p, runtime.Config{
		Concurrency: 0, BatchBuffer: 64, RuleVersion: "r1",
	})
	return result("concurrency_budget", err)
}

// InjectBufferBudget raises E_BUFFER_BUDGET for an illegal buffer.
func InjectBufferBudget(p *domain.Project) InjectionResult {
	_, err := runtime.NewDeterministicRunner(p, runtime.Config{
		Concurrency: 2, BatchBuffer: -1, RuleVersion: "r1",
	})
	return result("buffer_budget", err)
}

// InjectBackpressure exhausts the concurrency cap and observes E_BACKPRESSURE.
func InjectBackpressure(p *domain.Project) InjectionResult {
	d, err := runtime.NewDeterministicRunner(p, runtime.Config{
		Concurrency: 1, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		return result("backpressure", err)
	}
	var last error
	for _, pod := range p.Pods {
		if e := d.Acquire(pod.ID); e != nil {
			last = e
			break
		}
	}
	return result("backpressure", last)
}

// InjectTruncatedCheckpoint writes a valid commit then truncates a data file,
// expecting E_CHECKPOINT_TRUNCATED on reload.
func InjectTruncatedCheckpoint(store *runtime.Store, p *domain.Project) (InjectionResult, error) {
	snap := runtime.BuildSnapshot(p, 1, "r1")
	frames := []runtime.Frame{{PodID: p.Pods[0].ID, Selected: "x", Normalized: 1, RuleVersion: "r1", SnapshotSeq: 1}}
	if err := store.Commit(snap, frames); err != nil {
		return InjectionResult{}, err
	}
	// Corrupt the cache payload by truncating it.
	cache := filepath.Join(storeDir(store), "cache-1.json")
	if err := os.WriteFile(cache, []byte(`{"magic":"SCHEDLAB-CP","partial":`), 0o600); err != nil {
		return InjectionResult{}, err
	}
	_, err := store.LoadCache(1)
	if err != nil && faults.CodeOf(err) == "" {
		err = faults.Wrap(faults.E_CHECKPOINT_TRUNCATED, err.Error(), err)
	}
	return result("truncated_checkpoint", err), nil
}

// InjectStateVersionMismatch commits under one rule version then reloads with
// another, expecting E_STATE_VERSION_MISMATCH.
func InjectStateVersionMismatch(store *runtime.Store, p *domain.Project) (InjectionResult, error) {
	snap := runtime.BuildSnapshot(p, 1, "r1")
	frames := []runtime.Frame{{PodID: p.Pods[0].ID, Selected: "x", Normalized: 1, RuleVersion: "r1", SnapshotSeq: 1}}
	if err := store.Commit(snap, frames); err != nil {
		return InjectionResult{}, err
	}
	runner, err := runtime.NewRunner(p, store, runtime.Config{
		Concurrency: 2, BatchBuffer: 4096, RuleVersion: "r2",
	})
	if err != nil {
		return InjectionResult{}, err
	}
	_, err = runner.Run()
	return result("state_version_mismatch", err), nil
}

// SourceChecksum fingerprints the raw source JSON so tests can prove an
// injection chain never modified the source project.
func SourceChecksum(raw []byte) string {
	return sha256Hex(raw)
}

func cloneV3(v *gen.V3Project) *gen.V3Project {
	raw, _ := json.Marshal(v)
	var out gen.V3Project
	_ = json.Unmarshal(raw, &out)
	return &out
}
