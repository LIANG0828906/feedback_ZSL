// Package app wires the reusable kernel packages into local CLI use cases.
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
	"multiplexgw/internal/sample"
	"multiplexgw/internal/schema"
)

const (
	DefaultDataDir     = "data"
	DefaultArtifactDir = "artifacts/demo"
)

type CrossVersionReport struct {
	Loaded []string `json:"loaded"`
	Equal  bool     `json:"equal"`
	Diffs  []string `json:"diffs,omitempty"`
}

type TruthReport struct {
	Match      bool     `json:"match"`
	TraceCount int      `json:"trace_count"`
	SendCount  int      `json:"send_count"`
	Mismatches []string `json:"mismatches,omitempty"`
}

type NumericalReport struct {
	Match    bool                   `json:"match"`
	Cases    []sample.NumericalCase `json:"cases"`
	Failures []string               `json:"failures,omitempty"`
}

type AllocationRow struct {
	StreamID                int          `json:"stream_id"`
	WeightPermyriad         int64        `json:"weight_permyriad"`
	WeightShareQ            fixedpoint.Q `json:"weight_share_q16"`
	AllocatedBytes          int64        `json:"allocated_bytes"`
	TruthAllocatedBytes     int64        `json:"truth_allocated_bytes"`
	AllocationShareQ        fixedpoint.Q `json:"allocation_share_q16"`
	TruthAllocationShareQ   fixedpoint.Q `json:"truth_allocation_share_q16"`
	ShareDiffQ              fixedpoint.Q `json:"share_diff_q16"`
	RemainingWindow         int64        `json:"remaining_window"`
	TruthRemainingWindow    int64        `json:"truth_remaining_window"`
	WindowExactMatch        bool         `json:"window_exact_match"`
	AllocationExactMatch    bool         `json:"allocation_exact_match"`
	WithinDeclaredTolerance bool         `json:"within_declared_tolerance"`
	ToleranceQ              fixedpoint.Q `json:"tolerance_q16"`
}

type ExceptionResult struct {
	Case       string         `json:"case"`
	Expected   string         `json:"expected_code"`
	ActualCode string         `json:"actual_code"`
	Match      bool           `json:"match"`
	Context    map[string]any `json:"context,omitempty"`
	Detail     string         `json:"detail,omitempty"`
}

type CheckpointReport struct {
	Match           bool     `json:"match"`
	DBPath          string   `json:"db_path"`
	CommittedBefore int      `json:"committed_before_crash_probe"`
	RecoveredFrom   int      `json:"recovered_from"`
	FinalCommitted  int      `json:"final_committed"`
	TraceCount      int      `json:"trace_count"`
	Mismatches      []string `json:"mismatches,omitempty"`
}

type ParallelSummary struct {
	Workers         int      `json:"workers"`
	BufferBudget    int      `json:"buffer_budget"`
	ConnectionIDs   []string `json:"connection_ids"`
	AllOK           bool     `json:"all_ok"`
	Codes           []string `json:"codes"`
	TraceCounts     []int    `json:"trace_counts"`
	ProbeCode       string   `json:"thread_probe_code"`
	BufferProbeCode string   `json:"buffer_probe_code,omitempty"`
	BufferProbeOK   bool     `json:"buffer_probe_ok"`
}

type VerificationReport struct {
	OK              bool                `json:"ok"`
	DataDir         string              `json:"data_dir"`
	ArtifactDir     string              `json:"artifact_dir"`
	CrossVersion    CrossVersionReport  `json:"cross_version"`
	Truth           TruthReport         `json:"truth"`
	Numerical       NumericalReport     `json:"numerical"`
	AllocationMatch bool                `json:"allocation_match"`
	Allocations     []AllocationRow     `json:"allocations,omitempty"`
	Exceptions      []ExceptionResult   `json:"exceptions"`
	Incremental     *IncrementalSummary `json:"incremental,omitempty"`
	Parallel        ParallelSummary     `json:"parallel"`
	Checkpoint      CheckpointReport    `json:"checkpoint"`
	OutputFiles     map[string]string   `json:"output_files"`
}

type IncrementalSummary struct {
	AllMatch bool                     `json:"all_match"`
	Cases    []IncrementalCaseSummary `json:"cases"`
}

type IncrementalCaseSummary struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	StartIndex       int    `json:"start_index"`
	ReusedEvents     int    `json:"reused_events"`
	RecomputedEvents int    `json:"recomputed_events"`
	SemanticEqual    bool   `json:"semantic_equal"`
	RootSource       string `json:"root_source"`
	Failure          string `json:"failure,omitempty"`
}

// Generate creates deterministic local sample files.
func Generate(dataDir string) (*sample.Bundle, error) {
	return GenerateSeed(sample.DefaultSeed, dataDir)
}

// GenerateSeed creates samples for an explicit deterministic seed.
func GenerateSeed(seed int64, dataDir string) (*sample.Bundle, error) {
	bundle, err := sample.Build(seed)
	if err != nil {
		return nil, err
	}
	if err := bundle.Write(dataDir); err != nil {
		return nil, err
	}
	return bundle, nil
}

func loadTruth(dataDir string) (*sample.Truth, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "truth.json"))
	if err != nil {
		return nil, bizerr.Wrap(bizerr.NotFound, filepath.Join(dataDir, "truth.json"), err, "read truth")
	}
	var t sample.Truth
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, bizerr.Wrap(bizerr.SchemaParse, "truth.json", err, "decode truth")
	}
	return &t, nil
}

func loadThree(dataDir string) ([]*model.Project, CrossVersionReport, error) {
	names := []string{"project-v1.json", "project-v2.json", "project-v3.json"}
	rep := CrossVersionReport{Loaded: names, Equal: true}
	var ps []*model.Project
	for _, name := range names {
		p, err := schema.LoadFile(filepath.Join(dataDir, name))
		if err != nil {
			return nil, rep, err
		}
		ps = append(ps, p)
	}
	for i := 1; i < len(ps); i++ {
		diffs := schema.CompareCanonical(ps[0], ps[i])
		if len(diffs) > 0 {
			rep.Equal = false
			rep.Diffs = append(rep.Diffs, fmt.Sprintf("v1-vs-v%d:", i+1))
			rep.Diffs = append(rep.Diffs, diffs...)
		}
	}
	return ps, rep, nil
}

func compareTraces(expected, actual []model.Trace) []string {
	if len(expected) != len(actual) {
		return []string{fmt.Sprintf("trace count truth=%d actual=%d", len(expected), len(actual))}
	}
	var mismatch []string
	for i := range expected {
		a := expected[i]
		b := actual[i]
		a.CacheStatus = ""
		a.Invalidation = nil
		b.CacheStatus = ""
		b.Invalidation = nil
		if !reflect.DeepEqual(a, b) {
			mismatch = append(mismatch, fmt.Sprintf("event %d (%s) differs", i, expected[i].EventType))
		}
	}
	return mismatch
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeJSONL(path string, sends []model.SendRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, s := range sends {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}

func engineOptions() engine.Options {
	return engine.Options{MaxPendingFrames: 8, RecordFaults: false}
}
