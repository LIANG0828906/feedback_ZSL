package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"archive/internal/model"
)

type DirStatus struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Online    bool   `json:"online"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type RepairStatus struct {
	InProgress    int                `json:"in_progress"`
	QueueDepth    int                `json:"queue_depth"`
	Degraded      int                `json:"degraded_stripes"`
	Unrecoverable int                `json:"unrecoverable_stripes"`
	Queue         []RepairQueueEntry `json:"queue"`
}

type ReconcileStatus struct {
	ID              string          `json:"reconcile_id"`
	State           string          `json:"state"`
	FinishedAt      string          `json:"finished_at,omitempty"`
	Counts          ReconcileCounts `json:"counts"`
	ExcludedDirs    []string        `json:"excluded_offline_dirs"`
	StripesCompared int             `json:"stripes_compared"`
	BlocksCompared  int             `json:"blocks_compared"`
}

type StatusSummary struct {
	GeneratedAt        string           `json:"generated_at"`
	ServiceStartedAt   string           `json:"service_started_at"`
	RedisVersion       string           `json:"redis_version"`
	ObjectCount        int              `json:"object_count"`
	StripeCount        int              `json:"stripe_count"`
	BlockDirs          []DirStatus      `json:"block_dirs"`
	Quota              model.QuotaUsage `json:"quota"`
	Repairs            RepairStatus     `json:"repairs"`
	LastReconciliation *ReconcileStatus `json:"last_reconciliation"`
}

func (s *Service) Status(ctx context.Context, queueLimit int) (StatusSummary, error) {
	s.mu.RLock()
	dirs, err := s.dirs(ctx)
	if err != nil {
		s.mu.RUnlock()
		return StatusSummary{}, err
	}
	stripes, err := s.redis.ListAllStripes(ctx)
	if err != nil {
		s.mu.RUnlock()
		return StatusSummary{}, err
	}
	objects, err := s.redis.ListObjects(ctx)
	if err != nil {
		s.mu.RUnlock()
		return StatusSummary{}, err
	}
	s.mu.RUnlock()

	degraded, unrecoverable := 0, 0
	for i := range stripes {
		stripes[i].Refresh(dirs)
		switch stripes[i].Status {
		case model.StatusDegraded:
			degraded++
		case model.StatusUnrecoverable:
			unrecoverable++
		}
	}
	queue, err := s.RepairQueue(ctx, queueLimit)
	if err != nil {
		return StatusSummary{}, err
	}
	st := StatusSummary{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		ServiceStartedAt: s.startedAt.Format(time.RFC3339Nano),
		RedisVersion:     s.redis.Version(),
		ObjectCount:      len(objects),
		StripeCount:      len(stripes),
		BlockDirs:        make([]DirStatus, 0, model.BlockCount),
		Quota:            queue.Quota,
		Repairs: RepairStatus{
			InProgress:    int(queue.ActiveRepairs),
			QueueDepth:    queue.TotalCandidates,
			Degraded:      degraded,
			Unrecoverable: unrecoverable,
			Queue:         queue.Queue,
		},
	}
	for i := 0; i < model.BlockCount; i++ {
		st.BlockDirs = append(st.BlockDirs, DirStatus{
			Index:     i,
			Name:      fmt.Sprintf("block-%d", i),
			Online:    dirs.IsOnline(i),
			Reason:    dirs.Dirs[i].Reason,
			UpdatedAt: formatOptionalTime(dirs.Dirs[i].UpdatedAt),
		})
	}
	if rec, err := s.LastReconciliation(ctx); err == nil {
		excluded := make([]string, len(rec.ExcludedOfflineDirs))
		copy(excluded, rec.ExcludedOfflineDirs)
		st.LastReconciliation = &ReconcileStatus{
			ID:              rec.ID,
			State:           rec.State,
			FinishedAt:      rec.FinishedAt,
			Counts:          rec.Counts,
			ExcludedDirs:    excluded,
			StripesCompared: rec.StripesCompared,
			BlocksCompared:  rec.BlocksCompared,
		}
	}
	return st, nil
}

func (s *Service) Metrics(ctx context.Context) (string, error) {
	status, err := s.Status(ctx, 10)
	if err != nil {
		return "", err
	}
	inProgress, success, failure, err := s.db.RepairCounts(ctx)
	if err != nil {
		return "", err
	}
	last, lastErr := s.LastReconciliation(ctx)
	if lastErr != nil {
		last = ReconcileResult{}
	}
	var b strings.Builder
	b.WriteString("# HELP archive_block_dir_online Whether a block directory is online (1) or offline (0).\n")
	b.WriteString("# TYPE archive_block_dir_online gauge\n")
	for _, d := range status.BlockDirs {
		v := 0
		if d.Online {
			v = 1
		}
		fmt.Fprintf(&b, "archive_block_dir_online{dir=%q} %d\n", d.Name, v)
	}
	b.WriteString("# HELP archive_degraded_stripes Number of stripes currently degraded but readable.\n")
	b.WriteString("# TYPE archive_degraded_stripes gauge\n")
	fmt.Fprintf(&b, "archive_degraded_stripes %d\n", status.Repairs.Degraded)
	b.WriteString("# HELP archive_unrecoverable_stripes Number of stripes with fewer than four available blocks.\n")
	b.WriteString("# TYPE archive_unrecoverable_stripes gauge\n")
	fmt.Fprintf(&b, "archive_unrecoverable_stripes %d\n", status.Repairs.Unrecoverable)
	b.WriteString("# HELP archive_writeback_bytes_today Standby write-back bytes charged to the local-day quota.\n")
	b.WriteString("# TYPE archive_writeback_bytes_today gauge\n")
	fmt.Fprintf(&b, "archive_writeback_bytes_today %d\n", status.Quota.UsedBytes)
	b.WriteString("# HELP archive_quota_remaining_bytes_today Remaining standby write-back quota for the local day.\n")
	b.WriteString("# TYPE archive_quota_remaining_bytes_today gauge\n")
	fmt.Fprintf(&b, "archive_quota_remaining_bytes_today %d\n", status.Quota.RemainingBytes)
	b.WriteString("# HELP archive_repairs_in_progress Number of stripe repairs currently running.\n")
	b.WriteString("# TYPE archive_repairs_in_progress gauge\n")
	fmt.Fprintf(&b, "archive_repairs_in_progress %d\n", inProgress)
	b.WriteString("# HELP archive_repair_success_total Total successfully verified repair jobs.\n")
	b.WriteString("# TYPE archive_repair_success_total counter\n")
	fmt.Fprintf(&b, "archive_repair_success_total %d\n", success)
	b.WriteString("# HELP archive_repair_failure_total Total failed repair jobs.\n")
	b.WriteString("# TYPE archive_repair_failure_total counter\n")
	fmt.Fprintf(&b, "archive_repair_failure_total %d\n", failure)
	b.WriteString("# HELP archive_repair_queue_depth Number of repairable stripes waiting in the risk-priority queue.\n")
	b.WriteString("# TYPE archive_repair_queue_depth gauge\n")
	fmt.Fprintf(&b, "archive_repair_queue_depth %d\n", status.Repairs.QueueDepth)
	b.WriteString("# HELP archive_reconcile_differences Last reconciliation differences by category.\n")
	b.WriteString("# TYPE archive_reconcile_differences gauge\n")
	fmt.Fprintf(&b, "archive_reconcile_differences{type=\"missing\"} %d\n", last.Counts.Missing)
	fmt.Fprintf(&b, "archive_reconcile_differences{type=\"length\"} %d\n", last.Counts.LengthMismatch)
	fmt.Fprintf(&b, "archive_reconcile_differences{type=\"digest\"} %d\n", last.Counts.DigestMismatch)
	b.WriteString("# HELP archive_objects_total Number of objects registered in Redis.\n")
	b.WriteString("# TYPE archive_objects_total gauge\n")
	fmt.Fprintf(&b, "archive_objects_total %d\n", status.ObjectCount)
	b.WriteString("# HELP archive_stripes_total Number of stripes registered in Redis.\n")
	b.WriteString("# TYPE archive_stripes_total gauge\n")
	fmt.Fprintf(&b, "archive_stripes_total %d\n", status.StripeCount)
	return b.String(), nil
}

func (s *Service) StatusJSON(ctx context.Context, queueLimit int) ([]byte, error) {
	st, err := s.Status(ctx, queueLimit)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(st, "", "  ")
}

func SortStringsStable(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func promQuote(s string) string {
	return strconv.Quote(s)
}
