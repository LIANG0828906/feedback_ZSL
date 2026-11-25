package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type SchemaName string

const (
	SchemaV1 SchemaName = "v1"
	SchemaV2 SchemaName = "v2"
	SchemaV3 SchemaName = "v3"
)

type WindowConfig struct {
	WidthMS int64 `json:"width_ms"`
	SlideMS int64 `json:"slide_ms"`
}

type ExportConfig struct {
	MaxWindowsPerBatch int            `json:"max_windows_per_batch"`
	MaxRecordsPerBatch int            `json:"max_records_per_batch"`
	PriorityRules      []PriorityRule `json:"priority_rules"`
}

type PriorityRule struct {
	Priority   int    `json:"priority"`
	Level      string `json:"level,omitempty"`
	ServiceID  string `json:"service_id,omitempty"`
	RouteID    string `json:"route_id,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

type Budget struct {
	Workers          int `json:"workers"`
	ServiceBuffer    int `json:"service_buffer"`
	MaxWorkers       int `json:"max_workers"`
	MaxServiceBuffer int `json:"max_service_buffer"`
}

type ErrorRule struct {
	Class       string `json:"class"`
	StatusMin   int    `json:"status_min,omitempty"`
	StatusMax   int    `json:"status_max,omitempty"`
	TimeoutOnly bool   `json:"timeout_only,omitempty"`
}

type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Route struct {
	ID         string      `json:"id"`
	ServiceID  string      `json:"service_id"`
	Path       string      `json:"path"`
	Prefix     string      `json:"prefix"`
	Order      int         `json:"order"`
	TimeoutMS  int64       `json:"timeout_ms"`
	ErrorRules []ErrorRule `json:"error_rules"`
}

type Record struct {
	ID             string        `json:"id"`
	ArrivalSeq     int           `json:"arrival_seq"`
	Path           string        `json:"path"`
	RouteID        string        `json:"route_id"`
	ServiceID      string        `json:"service_id"`
	EventTimeMS    int64         `json:"event_time_ms"`
	SourceEventSeq *int64        `json:"source_event_seq,omitempty"`
	DurationUS     int64         `json:"duration_us"`
	DurationFixed  FixedDuration `json:"duration_fixed_q16_16,omitempty"`
	DurationRaw    string        `json:"duration_raw,omitempty"`
	DurationUnit   string        `json:"duration_unit,omitempty"`
	StatusCode     int           `json:"status_code"`
	RequestBytes   int64         `json:"request_bytes"`
	ResponseBytes  int64         `json:"response_bytes"`
	TimedOut       bool          `json:"timed_out"`
	ErrorClass     string        `json:"error_class,omitempty"`
}

type WatermarkMark struct {
	BeforeArrivalSeq int   `json:"before_arrival_seq"`
	WatermarkMS      int64 `json:"watermark_ms"`
}

type Project struct {
	ProjectID             string            `json:"project_id"`
	ProjectVersion        string            `json:"project_version"`
	Schema                SchemaName        `json:"schema"`
	Seed                  int64             `json:"seed"`
	TimeOriginMS          int64             `json:"time_origin_ms"`
	SequenceStepMS        int64             `json:"sequence_step_ms"`
	Window                WindowConfig      `json:"window"`
	AllowedLatenessMS     int64             `json:"allowed_lateness_ms"`
	HistogramBoundariesUS []int64           `json:"histogram_boundaries_us"`
	DurationToleranceUS   int64             `json:"duration_tolerance_us"`
	CountTolerance        int64             `json:"count_tolerance"`
	Export                ExportConfig      `json:"export"`
	Budget                Budget            `json:"budget"`
	Services              []Service         `json:"services"`
	Routes                []Route           `json:"routes"`
	Records               []Record          `json:"records"`
	WatermarkMS           *int64            `json:"watermark_ms,omitempty"`
	WatermarkMarks        []WatermarkMark   `json:"watermark_marks,omitempty"`
	CommittedBoundaryMS   *int64            `json:"committed_boundary_ms,omitempty"`
	WatermarkSource       string            `json:"watermark_source,omitempty"`
	DeclaredFallbackRule  string            `json:"declared_fallback_rule,omitempty"`
	Metadata              map[string]string `json:"metadata,omitempty"`
}

type GroupKey struct {
	Level      string `json:"level"`
	ServiceID  string `json:"service_id"`
	RouteID    string `json:"route_id,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

type WindowKey struct {
	Index int64 `json:"index"`
	GroupKey
}

func (k WindowKey) String() string {
	return fmt.Sprintf("w%d/%s/%s/%s/%d", k.Index, k.Level, k.ServiceID, k.RouteID, k.StatusCode)
}

func ParseWindowKey(s string) (WindowKey, error) {
	var k WindowKey
	trimmed := strings.TrimPrefix(s, "w")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 5 {
		return k, NewError(CodeFieldType, "window_key", "cannot parse window key").WithContext("value", s)
	}
	index, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return k, NewError(CodeFieldType, "window_key", "invalid window index").WithContext("value", s)
	}
	status, err := strconv.Atoi(parts[4])
	if err != nil {
		return k, NewError(CodeFieldType, "window_key", "invalid status code").WithContext("value", s)
	}
	k.Index = index
	k.Level = parts[1]
	k.ServiceID = parts[2]
	k.RouteID = parts[3]
	k.StatusCode = status
	return k, nil
}

type Aggregate struct {
	Key              WindowKey        `json:"key"`
	StartMS          int64            `json:"start_ms"`
	EndMS            int64            `json:"end_ms"`
	RecordCount      int64            `json:"record_count"`
	BucketCounts     []int64          `json:"bucket_counts"`
	RequestBytes     int64            `json:"request_bytes"`
	ResponseBytes    int64            `json:"response_bytes"`
	ErrorClassCounts map[string]int64 `json:"error_class_counts"`
	RecordIDs        []string         `json:"record_ids"`
	Committed        bool             `json:"committed"`
	CommitSeq        int64            `json:"commit_seq,omitempty"`
	Priority         int              `json:"priority"`
}

type RecomputeEvent struct {
	WindowKey  WindowKey `json:"window_key"`
	Source     string    `json:"source"`
	SourceID   string    `json:"source_id,omitempty"`
	Reason     string    `json:"reason"`
	LatenessMS int64     `json:"lateness_ms,omitempty"`
	TriggerSeq int       `json:"trigger_seq"`
}

type DroppedRecord struct {
	RecordID    string    `json:"record_id"`
	WindowKey   WindowKey `json:"window_key"`
	WindowIndex int64     `json:"window_index"`
	LatenessMS  int64     `json:"lateness_ms"`
	Reason      string    `json:"reason"`
}

type WindowResult struct {
	Aggregate       *Aggregate       `json:"aggregate"`
	RecomputeEvents []RecomputeEvent `json:"recompute_events,omitempty"`
	Dropped         []DroppedRecord  `json:"dropped,omitempty"`
}

type RecordSummary struct {
	RecordID      string `json:"record_id"`
	ArrivalSeq    int    `json:"arrival_seq"`
	ServiceID     string `json:"service_id"`
	RouteID       string `json:"route_id"`
	StatusCode    int    `json:"status_code"`
	ErrorClass    string `json:"error_class"`
	EventTimeMS   int64  `json:"event_time_ms"`
	DurationUS    int64  `json:"duration_us"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
}

type ExportBatch struct {
	BatchNo         int             `json:"batch_no"`
	BatchID         string          `json:"batch_id"`
	PriorityCode    int             `json:"priority_code"`
	WindowKeys      []string        `json:"window_keys"`
	WindowCount     int             `json:"window_count"`
	RecordCount     int64           `json:"record_count"`
	PriorityCodes   map[string]int  `json:"priority_codes"`
	SnapshotDigest  string          `json:"snapshot_digest"`
	SnapshotSummary string          `json:"snapshot_summary"`
	Snapshots       []*Aggregate    `json:"snapshots,omitempty"`
	Records         []RecordSummary `json:"records,omitempty"`
}

type RunReport struct {
	ProjectVersion  string               `json:"project_version"`
	WatermarkMS     *int64               `json:"watermark_ms,omitempty"`
	WatermarkSource string               `json:"watermark_source"`
	Windows         []*Aggregate         `json:"windows"`
	RecomputeEvents []RecomputeEvent     `json:"recompute_events"`
	RecomputedKeys  []string             `json:"recomputed_keys"`
	Dropped         []DroppedRecord      `json:"dropped"`
	Batches         []*ExportBatch       `json:"batches"`
	DurationTraces  []DurationTrace      `json:"duration_traces,omitempty"`
	Diagnostics     *ParallelDiagnostics `json:"diagnostics,omitempty"`
}

func (p *Project) Clone() *Project {
	data, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(data, &out)
	return &out
}

func (p *Project) Fingerprint() string {
	cp := p.Clone()
	cp.Schema = ""
	cp.WatermarkSource = ""
	cp.Metadata = nil
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func SortedWindowKeys(m map[string]*Aggregate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func CanonicalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"canonical_error":%q}`, err.Error())
	}
	return string(data)
}
