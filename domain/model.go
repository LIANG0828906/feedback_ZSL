// Package domain contains the storage-neutral model shared by the loader,
// update engine, scan planner and checkpoint store.
package domain

import (
	"fmt"
	"sort"
	"strings"
)

// BatchStatus is the lifecycle state of a column batch.
type BatchStatus string

const (
	BatchActive     BatchStatus = "active"
	BatchDeleted    BatchStatus = "deleted"
	BatchSuperseded BatchStatus = "superseded"
)

// OperationKind is the canonical update kind.
type OperationKind string

const (
	OpReplace OperationKind = "replace"
	OpPatch   OperationKind = "patch"
	OpDelete  OperationKind = "delete"
	OpCompact OperationKind = "compact"
)

// Batch is one immutable-shaped column batch. Updates advance its generation
// while preserving the identity, which mirrors a logical batch being rewritten.
type Batch struct {
	ID                int         `json:"id"`
	Family            string      `json:"family"`
	Column            string      `json:"column"`
	RowStart          int64       `json:"row_start"`
	RowEnd            int64       `json:"row_end"`
	Rows              int64       `json:"rows"`
	Encoding          string      `json:"encoding"`
	CompressedBytes   int64       `json:"compressed_bytes"`
	Digest            uint64      `json:"digest"`
	DecodeBufferBytes int64       `json:"decode_buffer_bytes"`
	Status            BatchStatus `json:"status"`
	Invalidated       bool        `json:"invalidated,omitempty"`
	DependsOn         []int       `json:"depends_on,omitempty"`
	SourceID          int         `json:"source_id,omitempty"`
}

// Clone returns a deep copy. Domain paths never expose mutable aliases to a
// caller-owned project.
func (b Batch) Clone() Batch {
	b.DependsOn = append([]int(nil), b.DependsOn...)
	return b
}

func (b Batch) IsReadable() bool {
	return b.Status == BatchActive && b.Rows > 0
}

// Family declares a column family and its deterministic batch order.
type Family struct {
	ID       string `json:"id"`
	Order    int    `json:"order"`
	BatchIDs []int  `json:"batch_ids"`
}

// RowRange is a half-open row range [From, To).
type RowRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Operation is the canonical update representation. A replace/patch operation
// can carry replacement dependencies; when DependsOn is nil the current
// dependencies are preserved.
type Operation struct {
	Index           int           `json:"index"`
	Kind            OperationKind `json:"kind"`
	Target          int           `json:"target"`
	NewBatchID      int           `json:"new_batch_id,omitempty"`
	Family          string        `json:"family,omitempty"`
	Range           *RowRange     `json:"range,omitempty"`
	Rows            int64         `json:"rows,omitempty"`
	RowDelta        int64         `json:"row_delta,omitempty"`
	Digest          uint64        `json:"digest"`
	CompressedBytes int64         `json:"compressed_bytes,omitempty"`
	BytesDelta      int64         `json:"bytes_delta,omitempty"`
	Encoding        string        `json:"encoding,omitempty"`
	DependsOn       []int         `json:"depends_on,omitempty"`
	SourceID        int           `json:"source_id,omitempty"`
	AtMillis        int64         `json:"at_millis"`
}

func (o Operation) Clone() Operation {
	if o.Range != nil {
		r := *o.Range
		o.Range = &r
	}
	o.DependsOn = append([]int(nil), o.DependsOn...)
	return o
}

// Scan is one projection/predicate read scheduled after an operation index.
type Scan struct {
	ID       string   `json:"id"`
	Order    int      `json:"order"`
	AfterOp  int      `json:"after_op"`
	Family   string   `json:"family,omitempty"`
	Columns  []string `json:"columns"`
	FromRow  int64    `json:"from_row"`
	ToRow    int64    `json:"to_row"`
	AtMillis int64    `json:"at_millis"`
}

func (s Scan) Clone() Scan {
	s.Columns = append([]string(nil), s.Columns...)
	return s
}

type Budget struct {
	Workers                   int   `json:"workers"`
	TotalDecodeBufferBytes    int64 `json:"total_decode_buffer_bytes"`
	PerBatchDecodeBufferBytes int64 `json:"per_batch_decode_buffer_bytes"`
}

// Project is the normalized, canonical representation used by all components.
type Project struct {
	SchemaVersion       string      `json:"schema_version"`
	Table               string      `json:"table"`
	TimeUnit            string      `json:"time_unit"`
	SizeUnit            string      `json:"size_unit"`
	TickMillis          int64       `json:"tick_millis"`
	DigestBits          uint        `json:"digest_bits"`
	DigestToleranceBits uint        `json:"digest_tolerance_bits"`
	RowTolerance        int64       `json:"row_tolerance"`
	Budget              Budget      `json:"budget"`
	Families            []Family    `json:"families"`
	Batches             []Batch     `json:"batches"`
	Operations          []Operation `json:"operations"`
	Scans               []Scan      `json:"scans"`
}

// Clone copies the complete project graph.
func (p Project) Clone() Project {
	out := p
	out.Families = make([]Family, len(p.Families))
	for i, f := range p.Families {
		out.Families[i] = f
		out.Families[i].BatchIDs = append([]int(nil), f.BatchIDs...)
	}
	out.Batches = make([]Batch, len(p.Batches))
	for i, b := range p.Batches {
		out.Batches[i] = b.Clone()
	}
	out.Operations = make([]Operation, len(p.Operations))
	for i, op := range p.Operations {
		out.Operations[i] = op.Clone()
	}
	out.Scans = make([]Scan, len(p.Scans))
	for i, s := range p.Scans {
		out.Scans[i] = s.Clone()
	}
	return out
}

// FindBatch returns a clone of a batch and whether it exists.
func (p Project) FindBatch(id int) (Batch, bool) {
	for _, b := range p.Batches {
		if b.ID == id {
			return b.Clone(), true
		}
	}
	return Batch{}, false
}

// SortedBatches returns a stable ID-ordered batch list.
func SortedBatches(in map[int]Batch) []Batch {
	out := make([]Batch, 0, len(in))
	for _, b := range in {
		out = append(out, b.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func SortedInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

func CanonicalIntSet(in []int) []int {
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func CanonicalColumns(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// DigestCheck records fixed-width verification for one committed operation.
type DigestCheck struct {
	Before          uint64   `json:"before"`
	Expected        uint64   `json:"expected"`
	Actual          uint64   `json:"actual"`
	Bits            uint     `json:"bits"`
	Match           bool     `json:"match"`
	HammingDistance uint     `json:"hamming_distance"`
	ExpectedRows    int64    `json:"expected_rows"`
	ActualRows      int64    `json:"actual_rows"`
	RowDeviation    int64    `json:"row_deviation"`
	WithinTolerance bool     `json:"within_tolerance"`
	SaturatedFields []string `json:"saturated_fields,omitempty"`
	OverflowFields  []string `json:"overflow_fields,omitempty"`
	Attribution     []string `json:"attribution,omitempty"`
}

// DependencyNode is the auditable per-batch dependency view.
type DependencyNode struct {
	BatchID      int         `json:"batch_id"`
	Family       string      `json:"family"`
	Status       BatchStatus `json:"status"`
	Dependencies []int       `json:"dependencies"`
	SourceID     int         `json:"source_id,omitempty"`
	Dependents   []int       `json:"dependents,omitempty"`
}

type BudgetObservation struct {
	Workers              int   `json:"workers"`
	Waves                int   `json:"waves"`
	MaxInFlight          int   `json:"max_in_flight"`
	BackpressureEvents   int   `json:"backpressure_events"`
	BackpressuredBatches []int `json:"backpressured_batches,omitempty"`
}

// OperationResult is an immutable checkpoint-style snapshot after one op.
type OperationResult struct {
	Index      int               `json:"index"`
	Kind       OperationKind     `json:"kind"`
	Changed    []int             `json:"changed"`
	Affected   []int             `json:"affected"`
	Batches    []Batch           `json:"batches"`
	Graph      []DependencyNode  `json:"graph"`
	Check      DigestCheck       `json:"check"`
	Budget     BudgetObservation `json:"budget"`
	Recomputed bool              `json:"recomputed"`
}

// ScanResult describes the selected batches and exactly which cache slices
// were rebuilt for a scan.
type ScanResult struct {
	ScanID         string            `json:"scan_id"`
	AfterOp        int               `json:"after_op"`
	ReadBatches    []int             `json:"read_batches"`
	DecodedRows    int64             `json:"decoded_rows"`
	RebuiltBatches []int             `json:"rebuilt_batches"`
	ReusedBatches  []int             `json:"reused_batches"`
	RebuiltRows    int64             `json:"rebuilt_rows"`
	ReusedRows     int64             `json:"reused_rows"`
	InvalidSources []int             `json:"invalid_sources"`
	ColdCache      bool              `json:"cold_cache"`
	CacheHit       bool              `json:"cache_hit"`
	Budget         BudgetObservation `json:"budget"`
	Recomputed     bool              `json:"recomputed"`
}

type ScanCacheEntry struct {
	ScanID         string `json:"scan_id"`
	AfterOp        int    `json:"after_op"`
	ReadBatches    []int  `json:"read_batches"`
	DecodedRows    int64  `json:"decoded_rows"`
	InvalidSources []int  `json:"invalid_sources"`
}

// RunResult contains all state needed for comparison, incremental replay and
// checkpoint persistence.
type RunResult struct {
	Version        string                    `json:"version"`
	Table          string                    `json:"table"`
	Project        Project                   `json:"project"`
	InitialBatches []Batch                   `json:"initial_batches"`
	Operations     []OperationResult         `json:"operations"`
	Scans          []ScanResult              `json:"scans"`
	FinalBatches   []Batch                   `json:"final_batches"`
	Graph          []DependencyNode          `json:"graph"`
	ScanCache      map[string]ScanCacheEntry `json:"scan_cache"`
}

type IncrementalStats struct {
	ClosureBatchIDs      []int    `json:"closure_batch_ids"`
	RecomputedOps        []int    `json:"recomputed_ops"`
	ReusedOps            []int    `json:"reused_ops"`
	RecomputedScans      []string `json:"recomputed_scans"`
	ReusedScans          []string `json:"reused_scans"`
	AddedScans           []string `json:"added_scans"`
	RemovedScans         []string `json:"removed_scans"`
	ChangedFields        []string `json:"changed_fields"`
	ReusedOperationCache bool     `json:"reused_operation_cache"`
}

// CheckpointData is the unit persisted by the SQLite store.
type CheckpointData struct {
	SchemaVersion string           `json:"schema_version"`
	EngineVersion string           `json:"engine_version"`
	CommittedOp   int              `json:"committed_op"`
	Batches       []Batch          `json:"batches"`
	ScanCache     []ScanCacheEntry `json:"scan_cache"`
	ContentDigest uint64           `json:"content_digest"`
}

func (c CheckpointData) Validate() error {
	if strings.TrimSpace(c.SchemaVersion) == "" {
		return &Error{Code: CodeCheckpointVersion, Context: "checkpoint.schema_version", Message: "checkpoint schema version is empty"}
	}
	if strings.TrimSpace(c.EngineVersion) == "" {
		return &Error{Code: CodeCheckpointVersion, Context: "checkpoint.engine_version", Message: "checkpoint engine version is empty"}
	}
	if c.CommittedOp < 0 {
		return &Error{Code: CodeCheckpointCorrupt, Context: "checkpoint.committed_op", Message: fmt.Sprintf("invalid committed op %d", c.CommittedOp)}
	}
	return nil
}
