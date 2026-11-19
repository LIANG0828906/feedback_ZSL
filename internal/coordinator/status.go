package coordinator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type QuotaView struct {
	LogsPerSecond  int64  `json:"logs_per_second"`
	BytesPerSecond int64  `json:"bytes_per_second"`
	DropPolicy     string `json:"drop_policy"`
}

type UsageView struct {
	WindowSecond int64 `json:"window_second"`
	Records      int64 `json:"records"`
	Bytes        int64 `json:"bytes"`
}

type TenantStatus struct {
	TenantID                    string          `json:"tenant_id"`
	Quota                       QuotaView       `json:"quota"`
	Usage                       UsageView       `json:"usage"`
	PendingWriteBytes           int64           `json:"pending_write_bytes"`
	PendingBatches              int64           `json:"pending_batches"`
	BacklogBatches              int64           `json:"backlog_batches"`
	BacklogBytes                int64           `json:"backlog_bytes"`
	ConsumeRecordsPerSecond     float64         `json:"consume_records_per_second"`
	ConsumeBytesPerSecond       float64         `json:"consume_bytes_per_second"`
	Degraded                    bool            `json:"degraded"`
	LastDegrade                 *DegradeRecord  `json:"last_degrade"`
	DegradeHistory              []DegradeRecord `json:"degrade_history"`
	IngestedRecords             uint64          `json:"ingested_records"`
	IngestedBytes               uint64          `json:"ingested_bytes"`
	DroppedRecords              uint64          `json:"dropped_records"`
	BackpressureRejectedBatches uint64          `json:"backpressure_rejected_batches"`
	WriteTimeouts               uint64          `json:"write_timeouts"`
}

type BackpressureView struct {
	Active          bool    `json:"active"`
	Ratio           float64 `json:"ratio"`
	BacklogDepth    int64   `json:"backlog_depth"`
	EnterThreshold  int64   `json:"enter_threshold"`
	ExitThreshold   int64   `json:"exit_threshold"`
	EnteredAt       string  `json:"entered_at,omitempty"`
	ExitedAt        string  `json:"exited_at,omitempty"`
	TotalDurationMS int64   `json:"total_duration_ms"`
	EntryCount      uint64  `json:"entry_count"`
	MaxBacklogDepth int64   `json:"max_backlog_depth"`
}

type StatusResponse struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	StartedAt     string           `json:"started_at"`
	Backpressure  BackpressureView `json:"backpressure"`
	Tenants       []TenantStatus   `json:"tenants"`
}

func (s *Service) Status(ctx context.Context) StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	response := StatusResponse{
		SchemaVersion: 1,
		GeneratedAt:   now.Format(time.RFC3339Nano),
		StartedAt:     s.startedAt.Format(time.RFC3339Nano),
	}
	bp := s.backpressure
	total := bp.TotalDuration
	if bp.Active && !bp.EnteredAt.IsZero() {
		total += now.Sub(bp.EnteredAt)
	}
	response.Backpressure = BackpressureView{
		Active:          bp.Active,
		Ratio:           s.cfg.Backpressure.Ratio,
		BacklogDepth:    bp.LastBacklog,
		EnterThreshold:  s.cfg.Backpressure.EnterBacklogBatches,
		ExitThreshold:   s.cfg.Backpressure.ExitBacklogBatches,
		TotalDurationMS: total.Milliseconds(),
		EntryCount:      bp.EntryCount,
		MaxBacklogDepth: bp.MaxBacklog,
	}
	if !bp.EnteredAt.IsZero() {
		response.Backpressure.EnteredAt = bp.EnteredAt.Format(time.RFC3339Nano)
	}
	if !bp.ExitedAt.IsZero() {
		response.Backpressure.ExitedAt = bp.ExitedAt.Format(time.RFC3339Nano)
	}
	ids := s.sortedTenantIDs()
	response.Tenants = make([]TenantStatus, 0, len(ids))
	for _, id := range ids {
		state := s.tenants[id]
		state.rollWindowLocked(now)
		s.updateConsumeRatesLocked(state, now)
		history := append([]DegradeRecord{}, state.history...)
		if history == nil {
			history = []DegradeRecord{}
		}
		var last *DegradeRecord
		if state.degrade != nil {
			copyRecord := *state.degrade
			last = &copyRecord
		}
		response.Tenants = append(response.Tenants, TenantStatus{
			TenantID: id,
			Quota: QuotaView{
				LogsPerSecond:  state.cfg.LogsPerSecond,
				BytesPerSecond: state.cfg.BytesPerSecond,
				DropPolicy:     state.cfg.DropPolicy,
			},
			Usage: UsageView{
				WindowSecond: state.windowSecond,
				Records:      state.windowCount,
				Bytes:        state.windowBytes,
			},
			PendingWriteBytes:           state.pendingBytes,
			PendingBatches:              int64(len(state.pending)),
			BacklogBatches:              state.backlogBatches,
			BacklogBytes:                state.pendingBytes,
			ConsumeRecordsPerSecond:     round3(state.consumeRecordsPS),
			ConsumeBytesPerSecond:       round3(state.consumeBytesPS),
			Degraded:                    state.degraded,
			LastDegrade:                 last,
			DegradeHistory:              history,
			IngestedRecords:             state.counter.IngestedRecords,
			IngestedBytes:               state.counter.IngestedBytes,
			DroppedRecords:              state.counter.DroppedRecords,
			BackpressureRejectedBatches: state.counter.BackpressureRejectedBatches,
			WriteTimeouts:               state.counter.WriteTimeouts,
		})
	}
	return response
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func (s *Service) PrometheusMetrics(ctx context.Context) string {
	s.mu.Lock()
	now := time.Now().UTC()
	ids := s.sortedTenantIDs()
	rows := make([]TenantStatus, 0, len(ids))
	var totalPending, totalBacklog int64
	var totalIngested, totalDropped, totalBPRejected, totalTimeouts uint64
	var totalConsumeRecords, totalConsumeBytes float64
	for _, id := range ids {
		state := s.tenants[id]
		s.updateConsumeRatesLocked(state, now)
		rows = append(rows, TenantStatus{
			TenantID: id,
			Quota: QuotaView{
				LogsPerSecond:  state.cfg.LogsPerSecond,
				BytesPerSecond: state.cfg.BytesPerSecond,
			},
			Usage: UsageView{
				WindowSecond: state.windowSecond,
				Records:      state.windowCount,
				Bytes:        state.windowBytes,
			},
			PendingWriteBytes:           state.pendingBytes,
			PendingBatches:              int64(len(state.pending)),
			BacklogBatches:              state.backlogBatches,
			BacklogBytes:                state.pendingBytes,
			ConsumeRecordsPerSecond:     state.consumeRecordsPS,
			ConsumeBytesPerSecond:       state.consumeBytesPS,
			Degraded:                    state.degraded,
			IngestedRecords:             state.counter.IngestedRecords,
			IngestedBytes:               state.counter.IngestedBytes,
			DroppedRecords:              state.counter.DroppedRecords,
			BackpressureRejectedBatches: state.counter.BackpressureRejectedBatches,
			WriteTimeouts:               state.counter.WriteTimeouts,
		})
		totalPending += state.pendingBytes
		totalBacklog += state.backlogBatches
		totalIngested += state.counter.IngestedRecords
		totalDropped += state.counter.DroppedRecords
		totalBPRejected += state.counter.BackpressureRejectedBatches
		totalTimeouts += state.counter.WriteTimeouts
		totalConsumeRecords += state.consumeRecordsPS
		totalConsumeBytes += state.consumeBytesPS
	}
	active := 0
	if s.backpressure.Active {
		active = 1
	}
	s.mu.Unlock()

	var b strings.Builder
	writeMetricHeader(&b, "log_quota_ingested_records_total", "counter", "Log records accepted into the reliable Redis queue.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_ingested_records_total{tenant=%q} %d\n", row.TenantID, row.IngestedRecords)
	}
	writeMetricHeader(&b, "log_quota_ingested_bytes_total", "counter", "Normalized log bytes accepted into the reliable Redis queue.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_ingested_bytes_total{tenant=%q} %d\n", row.TenantID, row.IngestedBytes)
	}
	writeMetricHeader(&b, "log_quota_dropped_records_total", "counter", "Log records rejected or deliberately dropped before enqueue.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_dropped_records_total{tenant=%q} %d\n", row.TenantID, row.DroppedRecords)
	}
	writeMetricHeader(&b, "log_quota_backpressure_rejected_batches_total", "counter", "Batches rejected because of global or tenant backpressure.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_backpressure_rejected_batches_total{tenant=%q} %d\n", row.TenantID, row.BackpressureRejectedBatches)
	}
	writeMetricHeader(&b, "log_quota_write_timeouts_total", "counter", "Batches that remained unacknowledged past the write timeout.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_write_timeouts_total{tenant=%q} %d\n", row.TenantID, row.WriteTimeouts)
	}
	writeMetricHeader(&b, "log_quota_pending_write_bytes", "gauge", "Bytes enqueued but not yet acknowledged as written.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_pending_write_bytes{tenant=%q} %d\n", row.TenantID, row.PendingWriteBytes)
	}
	writeMetricHeader(&b, "log_quota_backlog_depth", "gauge", "Batches currently in the tenant Redis queue and processing list.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_backlog_depth{tenant=%q} %d\n", row.TenantID, row.BacklogBatches)
	}
	writeMetricHeader(&b, "log_quota_consume_records_per_second", "gauge", "Acknowledged records per second over a rolling five second window.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_consume_records_per_second{tenant=%q} %.3f\n", row.TenantID, row.ConsumeRecordsPerSecond)
	}
	writeMetricHeader(&b, "log_quota_consume_bytes_per_second", "gauge", "Acknowledged bytes per second over a rolling five second window.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_consume_bytes_per_second{tenant=%q} %.3f\n", row.TenantID, row.ConsumeBytesPerSecond)
	}
	writeMetricHeader(&b, "log_quota_degraded", "gauge", "Whether the tenant is limited after repeated write timeouts.")
	for _, row := range rows {
		value := 0
		if row.Degraded {
			value = 1
		}
		fmt.Fprintf(&b, "log_quota_degraded{tenant=%q} %d\n", row.TenantID, value)
	}
	writeMetricHeader(&b, "log_quota_quota_records_per_second", "gauge", "Configured per-tenant record quota.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_quota_records_per_second{tenant=%q} %d\n", row.TenantID, row.Quota.LogsPerSecond)
	}
	writeMetricHeader(&b, "log_quota_quota_bytes_per_second", "gauge", "Configured per-tenant byte quota.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_quota_bytes_per_second{tenant=%q} %d\n", row.TenantID, row.Quota.BytesPerSecond)
	}
	writeMetricHeader(&b, "log_quota_window_records", "gauge", "Records admitted in the current one-second quota window.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_window_records{tenant=%q} %d\n", row.TenantID, row.Usage.Records)
	}
	writeMetricHeader(&b, "log_quota_window_bytes", "gauge", "Bytes admitted in the current one-second quota window.")
	for _, row := range rows {
		fmt.Fprintf(&b, "log_quota_window_bytes{tenant=%q} %d\n", row.TenantID, row.Usage.Bytes)
	}
	writeMetricHeader(&b, "log_quota_backpressure_active", "gauge", "Whether global backpressure is active.")
	fmt.Fprintf(&b, "log_quota_backpressure_active %d\n", active)
	writeMetricHeader(&b, "log_quota_backpressure_ratio", "gauge", "Admission ratio applied while global backpressure is active.")
	fmt.Fprintf(&b, "log_quota_backpressure_ratio %.3f\n", s.cfg.Backpressure.Ratio)
	writeMetricHeader(&b, "log_quota_redis_backlog_depth", "gauge", "Total Redis queue and processing depth across all tenants.")
	fmt.Fprintf(&b, "log_quota_redis_backlog_depth %d\n", totalBacklog)
	writeMetricHeader(&b, "log_quota_pending_write_bytes_total", "gauge", "Total unacknowledged bytes across all tenants.")
	fmt.Fprintf(&b, "log_quota_pending_write_bytes_total %d\n", totalPending)
	writeMetricHeader(&b, "log_quota_ingested_records_total_all", "counter", "Accepted records across all tenants.")
	fmt.Fprintf(&b, "log_quota_ingested_records_total_all %d\n", totalIngested)
	writeMetricHeader(&b, "log_quota_dropped_records_total_all", "counter", "Dropped records across all tenants.")
	fmt.Fprintf(&b, "log_quota_dropped_records_total_all %d\n", totalDropped)
	writeMetricHeader(&b, "log_quota_backpressure_rejected_batches_total_all", "counter", "Backpressure-rejected batches across all tenants.")
	fmt.Fprintf(&b, "log_quota_backpressure_rejected_batches_total_all %d\n", totalBPRejected)
	writeMetricHeader(&b, "log_quota_write_timeouts_total_all", "counter", "Write timeouts across all tenants.")
	fmt.Fprintf(&b, "log_quota_write_timeouts_total_all %d\n", totalTimeouts)
	writeMetricHeader(&b, "log_quota_consume_records_per_second_all", "gauge", "Aggregate acknowledged records per second.")
	fmt.Fprintf(&b, "log_quota_consume_records_per_second_all %.3f\n", totalConsumeRecords)
	writeMetricHeader(&b, "log_quota_consume_bytes_per_second_all", "gauge", "Aggregate acknowledged bytes per second.")
	fmt.Fprintf(&b, "log_quota_consume_bytes_per_second_all %.3f\n", totalConsumeBytes)
	return b.String()
}

func writeMetricHeader(b *strings.Builder, name, metricType, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, metricType)
}
