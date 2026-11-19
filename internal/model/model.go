package model

import (
	"encoding/json"
	"time"
)

type Record struct {
	RecordID  string          `json:"record_id"`
	TenantID  string          `json:"tenant_id"`
	BatchID   string          `json:"batch_id,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	Bytes     int64           `json:"bytes"`
}

type Batch struct {
	TenantID string   `json:"tenant_id"`
	BatchID  string   `json:"batch_id"`
	Records  []Record `json:"records"`
	Count    int64    `json:"count"`
	Bytes    int64    `json:"bytes"`
}

type QueueJob struct {
	TenantID string   `json:"tenant_id"`
	BatchID  string   `json:"batch_id"`
	Seq      int64    `json:"seq"`
	Records  []Record `json:"records"`
	Count    int64    `json:"count"`
	Bytes    int64    `json:"bytes"`
	Enqueued string   `json:"enqueued_at"`
}

type Receipt struct {
	TenantID       string `json:"tenant_id"`
	BatchID        string `json:"batch_id"`
	Seq            int64  `json:"seq"`
	RecordCount    int64  `json:"record_count"`
	Bytes          int64  `json:"bytes"`
	EnqueuedAt     string `json:"enqueued_at"`
	Acknowledged   bool   `json:"acknowledged"`
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
	StorageOffset  int64  `json:"storage_offset"`
}

type Cursor struct {
	TenantID       string `json:"tenant_id"`
	LastSeq        int64  `json:"last_seq"`
	LastBatchID    string `json:"last_batch_id"`
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
	RecordsTotal   int64  `json:"records_total"`
	BytesTotal     int64  `json:"bytes_total"`
}
