package app

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"graphlab/internal/engine"
	"graphlab/internal/generator"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

type Options struct {
	SampleDir       string
	CheckpointPath  string
	OutputDir       string
	Workers         int
	BufferPerWorker int
	Seed            int64
	Logf            func(string, ...any)
}

func DefaultOptions() Options {
	return Options{
		SampleDir:      "samples/seed42",
		CheckpointPath: "artifacts/demo/checkpoint.db",
		OutputDir:      "artifacts/demo",
		Workers:        4, BufferPerWorker: 4096, Seed: generator.DefaultSeed,
	}
}

func (o Options) BuildOptions() engine.BuildOptions {
	return engine.BuildOptions{Workers: o.Workers, BufferPerWorker: o.BufferPerWorker}
}

func LoadOrGenerate(opts Options) (map[string][]byte, *model.Truth, bool, error) {
	docs, truth, err := generator.LoadSample(opts.SampleDir)
	if err == nil {
		return docs, truth, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, false, err
	}
	sample, genErr := generator.Generate(opts.Seed)
	if genErr != nil {
		return nil, nil, false, genErr
	}
	if writeErr := sample.Write(opts.SampleDir); writeErr != nil {
		return nil, nil, false, writeErr
	}
	return sample.Documents, sample.Truth, true, nil
}

type SchemaSummary struct {
	Version            string   `json:"version"`
	SchemaVersion      string   `json:"schema_version"`
	HierarchyEnabled   bool     `json:"hierarchy_enabled"`
	HierarchyDegraded  bool     `json:"hierarchy_degraded,omitempty"`
	DegradationNotes   []string `json:"degradation_notes,omitempty"`
	CoreDigest         string   `json:"core_digest"`
	CoreDifferences    []string `json:"core_differences,omitempty"`
	ObservedAtMillis   int64    `json:"observed_at_ms"`
	CacheCapacityBytes int64    `json:"cache_capacity_bytes"`
}

type QueryDisplay struct {
	QueryID         string   `json:"query_id"`
	CacheID         string   `json:"cache_id"`
	Status          string   `json:"status"`
	VisitedVertices []string `json:"visited_vertices"`
	VisitedEdges    []string `json:"visited_edges"`
	ResultVertices  []string `json:"result_vertices"`
	PathVertices    []string `json:"path_vertices,omitempty"`
	PathEdges       []string `json:"path_edges,omitempty"`
	IndexKeys       []string `json:"index_keys"`
	Dependencies    []string `json:"dependencies"`
}

func ToQueryDisplay(results []model.QueryResult) []QueryDisplay {
	out := make([]QueryDisplay, 0, len(results))
	for _, result := range results {
		out = append(out, QueryDisplay{
			QueryID: result.QueryID, CacheID: result.CacheID,
			Status: result.Status, VisitedVertices: result.VisitedVertices,
			VisitedEdges: result.VisitedEdges, ResultVertices: result.ResultVertices,
			PathVertices: result.PathVertices, PathEdges: result.PathEdges,
			IndexKeys: result.IndexKeys, Dependencies: result.Dependencies,
		})
	}
	return out
}

func CompareShardTruth(rt *engine.Runtime, truth *model.Truth) []string {
	var diffs []string
	for shardID, want := range truth.Shards {
		si := rt.Index.Shards[shardID]
		if si == nil {
			diffs = append(diffs, "missing shard index "+shardID)
			continue
		}
		if !equalSlices(si.VertexOrder, want.VertexOrder) {
			diffs = append(diffs, "vertex order differs in "+shardID)
		}
		if !equalSlices(si.EdgeOrder, want.EdgeOrder) {
			diffs = append(diffs, "edge order differs in "+shardID)
		}
		if !equalSlices(rt.Index.ShardKeys(shardID), want.IndexKeys) {
			diffs = append(diffs, "index key set differs in "+shardID)
		}
	}
	return diffs
}

func CompareTruthQueries(results []model.QueryResult, truth map[string]model.QueryTruth) []string {
	var diffs []string
	if len(results) != len(truth) {
		diffs = append(diffs, fmt.Sprintf("query count got=%d want=%d", len(results), len(truth)))
	}
	for _, result := range results {
		want, ok := truth[result.QueryID]
		if !ok {
			diffs = append(diffs, "unexpected query "+result.QueryID)
			continue
		}
		if result.Status != want.Status ||
			!equalSlices(result.VisitedVertices, want.VisitedVertices) ||
			!equalSlices(result.VisitedEdges, want.VisitedEdges) ||
			!equalSlices(result.ResultVertices, want.ResultVertices) ||
			!equalSlices(result.PathVertices, want.PathVertices) ||
			!equalSlices(result.PathEdges, want.PathEdges) {
			diffs = append(diffs, "query result differs: "+result.QueryID)
		}
	}
	return diffs
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringSets(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return equalSlices(aa, bb)
}

func WriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func DocHashes(docs map[string][]byte) map[string]string {
	out := make(map[string]string, len(docs))
	for key, data := range docs {
		sum := sha256.Sum256(data)
		out[key] = hex.EncodeToString(sum[:])
	}
	return out
}

func loadBundle(docs map[string][]byte) (*loader.Bundle, error) {
	return loader.LoadBundle(docs)
}

func digestBytes(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])[:12]
}
