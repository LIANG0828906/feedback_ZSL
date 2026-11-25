// Package anomaly provides deterministic injections for the local demo and
// tests. Every case works on a clone and returns a stable business error.
package anomaly

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"example.com/columnar/checkpoint"
	"example.com/columnar/domain"
	"example.com/columnar/engine"
	"example.com/columnar/generator"
	"example.com/columnar/loader"
)

type Case struct {
	Name        string
	Expected    domain.Code
	Description string
}

var Cases = []Case{
	{"duplicate_batch", domain.CodeBatchDuplicate, "declare the same batch id twice"},
	{"patch_range", domain.CodePatchRange, "patch a row interval outside the current batch"},
	{"unit_conflict", domain.CodeUnitConflict, "declare KiB fields under a byte unit"},
	{"dependency_cycle", domain.CodeDependencyCycle, "create a batch dependency cycle"},
	{"dependency_deleted", domain.CodeDependencyDeleted, "depend on a batch after it was deleted"},
	{"compact_drift", domain.CodeCompactDrift, "inject a compact checksum mismatch"},
	{"workers_budget", domain.CodeBudgetWorkers, "set zero worker budget"},
	{"buffer_budget", domain.CodeBudgetBuffer, "set per-batch buffer below required size"},
	{"checkpoint_truncated", domain.CodeCheckpointTruncated, "truncate a committed SQLite checkpoint"},
	{"checkpoint_version", domain.CodeCheckpointVersion, "restore with a mismatched schema version"},
}

func Find(name string) (Case, bool) {
	for _, c := range Cases {
		if c.Name == name {
			return c, true
		}
	}
	return Case{}, false
}

// Inject executes one named anomaly against a fresh loaded v2 project and
// returns its business error.
func Inject(name string, sample *generator.Sample) error {
	blob, ok := sample.VersionJSON["v2"]
	if !ok {
		return domain.NewError(domain.CodeSchemaMissing, "anomaly", "sample has no v2 document")
	}
	switch name {
	case "duplicate_batch":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		p.Batches = append(p.Batches, p.Batches[0].Clone())
		_, err = engine.Run(p, engine.Options{})
		return err
	case "patch_range":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		ed := engine.NewEditor(p)
		if err := ed.SetPatchRange(1, 500, 510); err != nil {
			return err
		}
		_, err = engine.Run(ed.Project(), engine.Options{})
		return err
	case "unit_conflict":
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(blob, &doc); err != nil {
			return err
		}
		doc["units"] = mustRaw(map[string]string{"time": "ms", "size": "byte"})
		bad, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		_, err = loader.Load(bad)
		return err
	case "dependency_cycle":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		for i := range p.Batches {
			if p.Batches[i].ID == 100 {
				p.Batches[i].DependsOn = []int{102}
			}
		}
		_, err = engine.Run(p, engine.Options{})
		return err
	case "dependency_deleted":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		ed := engine.NewEditor(p)
		if err := ed.SetOperationDependencies(8, []int{302}); err != nil {
			return err
		}
		_, err = engine.Run(ed.Project(), engine.Options{})
		return err
	case "compact_drift":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		for i := range p.Operations {
			if p.Operations[i].Index == 4 {
				p.Operations[i].Digest ^= 1
				break
			}
		}
		_, err = engine.Run(p, engine.Options{})
		return err
	case "workers_budget":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		p.Budget.Workers = 0
		_, err = engine.Run(p, engine.Options{})
		return err
	case "buffer_budget":
		p, err := loader.Load(blob)
		if err != nil {
			return err
		}
		p.Budget.PerBatchDecodeBufferBytes = 512
		_, err = engine.Run(p, engine.Options{})
		return err
	case "checkpoint_truncated":
		dir, err := os.MkdirTemp("", "columnar-anomaly-checkpoint-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "state.db")
		store, err := checkpoint.Open(path)
		if err != nil {
			return err
		}
		if err := store.Save(context.Background(), anomalyCheckpoint()); err != nil {
			_ = store.Close()
			return err
		}
		if err := store.Close(); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		truncated := filepath.Join(dir, "truncated.db")
		if err := os.WriteFile(truncated, raw[:len(raw)/2], 0o644); err != nil {
			return err
		}
		bad, err := checkpoint.Open(truncated)
		if bad != nil {
			_ = bad.Close()
		}
		return err
	case "checkpoint_version":
		dir, err := os.MkdirTemp("", "columnar-anomaly-version-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		store, err := checkpoint.Open(filepath.Join(dir, "state.db"))
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.Save(context.Background(), anomalyCheckpoint()); err != nil {
			return err
		}
		_, err = store.Load(context.Background(), checkpoint.Expectation{
			SchemaVersion: "v3", EngineVersion: engine.EngineVersion,
		})
		return err
	default:
		return domain.Errorf(domain.CodeSchemaConflict, "anomaly", "unknown anomaly %q", name)
	}
}

func anomalyCheckpoint() domain.CheckpointData {
	return domain.CheckpointData{
		SchemaVersion: "v2", EngineVersion: engine.EngineVersion, CommittedOp: 1,
		Batches: []domain.Batch{{
			ID: 1, Family: "core", Column: "id", RowStart: 0, RowEnd: 10, Rows: 10,
			Encoding: "rle", CompressedBytes: 64, Digest: 7, DecodeBufferBytes: 256,
			Status: domain.BatchActive,
		}},
	}
}

func Names() []string {
	out := make([]string, len(Cases))
	for i, c := range Cases {
		out[i] = c.Name
	}
	sort.Strings(out)
	return out
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
