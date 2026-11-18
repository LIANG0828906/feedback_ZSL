package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"archive/internal/codec"
	"archive/internal/model"
)

type RepairQueueEntry struct {
	Rank            int    `json:"rank"`
	StripeID        string `json:"stripe_id"`
	ObjectID        string `json:"object_id"`
	StripeIndex     int    `json:"stripe_index"`
	ObjectSize      int64  `json:"object_size"`
	AvailableBlocks int    `json:"available_blocks"`
	MissingBlocks   []int  `json:"missing_blocks"`
	Risk            string `json:"risk"`
	EstimatedBytes  int64  `json:"estimated_bytes"`
	LastReadAt      string `json:"last_read_at"`
	PriorityReason  string `json:"priority_reason"`
	State           string `json:"state"`
	SkipReason      string `json:"skip_reason,omitempty"`
	RepairState     string `json:"repair_state,omitempty"`
	RepairJobID     string `json:"repair_job_id,omitempty"`
	CooldownUntil   string `json:"cooldown_until,omitempty"`
}

type RepairQueue struct {
	GeneratedAt       string             `json:"generated_at"`
	Day               string             `json:"day"`
	Quota             model.QuotaUsage   `json:"quota"`
	RepairConcurrency int                `json:"repair_concurrency"`
	ActiveRepairs     int64              `json:"active_repairs"`
	TotalCandidates   int                `json:"total_candidates"`
	Queue             []RepairQueueEntry `json:"queue"`
	Skipped           []RepairQueueEntry `json:"skipped"`
}

type ScheduleSummary struct {
	Started           int      `json:"started"`
	StartedStripes    []string `json:"started_stripes"`
	ActiveRepairs     int      `json:"active_repairs"`
	RunnableRemaining int      `json:"runnable_remaining"`
	QuotaRemaining    int64    `json:"quota_remaining_bytes"`
	PausedReason      string   `json:"paused_reason,omitempty"`
}

type repairResult struct {
	Verification   string          `json:"verification"`
	DataSHA256     string          `json:"data_sha256"`
	ParityVerified bool            `json:"parity_verified"`
	LogicalBytes   int64           `json:"logical_bytes"`
	ShardLength    int64           `json:"shard_length"`
	MissingBlocks  []int           `json:"missing_blocks"`
	WritebackBytes int64           `json:"writeback_bytes"`
	Blocks         []repairedBlock `json:"blocks"`
}

type repairedBlock struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

func (s *Service) repairLoop() {
	defer s.wg.Done()
	interval := time.Duration(s.cfg.RepairPollMS) * time.Millisecond
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
		if _, err := s.scheduleOnce(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Printf("repair scheduler: %v", err)
		}
	}
}

func (s *Service) resumeLoop() {
	defer s.wg.Done()
	jobs, err := s.db.ActiveRepairJobs(s.ctx)
	if err != nil {
		s.log.Printf("load active repair jobs: %v", err)
		return
	}
	for i := range jobs {
		job := jobs[i]
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			select {
			case s.repairSem <- struct{}{}:
			case <-s.ctx.Done():
				return
			}
			defer func() { <-s.repairSem }()
			s.active.Add(1)
			defer s.active.Add(-1)
			s.log.Printf("resuming repair job=%s stripe=%s missing=%v reserved=%d", job.JobID, job.StripeID, job.MissingBlocks, job.ReservedBytes)
			if err := s.executeRepair(s.ctx, job.StripeID, &job); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Printf("resume repair job=%s: %v", job.JobID, err)
			}
		}()
	}
	s.WakeRepair()
	<-s.ctx.Done()
}

func (s *Service) scheduleOnce(ctx context.Context) (ScheduleSummary, error) {
	var summary ScheduleSummary
	if ctx.Err() != nil {
		return summary, ctx.Err()
	}
	if _, err := s.applyCompletedRepairs(ctx); err != nil {
		s.log.Printf("apply completed repairs: %v", err)
	}
	queue, err := s.RepairQueue(ctx, 1<<30)
	if err != nil {
		return summary, err
	}
	summary.ActiveRepairs = int(queue.ActiveRepairs)
	summary.QuotaRemaining = queue.Quota.RemainingBytes
	remaining := queue.Quota.RemainingBytes
	startedThisRound := make(map[string]bool)
	for pos := 0; pos < len(queue.Queue); {
		groupAvailable := queue.Queue[pos].AvailableBlocks
		pending := 0
		startedInGroup := 0
		quotaSkippedInGroup := false
		for pos < len(queue.Queue) && queue.Queue[pos].AvailableBlocks == groupAvailable {
			entry := queue.Queue[pos]
			pos++
			if entry.RepairState == "running" || startedThisRound[entry.StripeID] {
				continue
			}
			if entry.State == "cooldown" {
				pending++
				continue
			}
			if entry.State == "waiting_quota" {
				pending++
				quotaSkippedInGroup = true
				continue
			}
			pending++
			if entry.EstimatedBytes > remaining {
				quotaSkippedInGroup = true
				s.recordSkip(ctx, entry, "quota_insufficient")
				continue
			}
			select {
			case s.repairSem <- struct{}{}:
			case <-ctx.Done():
				return summary, ctx.Err()
			default:
				summary.PausedReason = "concurrency_limit"
				return summary, nil
			}
			leaseToken := randomID()
			locked, err := s.redis.AcquireLock(ctx, "repair:"+entry.StripeID, leaseToken, 30*time.Minute)
			if err != nil {
				<-s.repairSem
				return summary, err
			}
			if !locked {
				<-s.repairSem
				s.recordSkip(ctx, entry, "stripe_lease_busy")
				continue
			}
			startedThisRound[entry.StripeID] = true
			remaining -= entry.EstimatedBytes
			startedInGroup++
			summary.Started++
			summary.StartedStripes = append(summary.StartedStripes, entry.StripeID)
			s.active.Add(1)
			s.wg.Add(1)
			go func(stripeID, token string) {
				defer s.wg.Done()
				defer s.active.Add(-1)
				defer func() { <-s.repairSem }()
				defer func() { _ = s.redis.ReleaseLock(context.Background(), "repair:"+stripeID, token) }()
				if err := s.executeRepair(ctx, stripeID, nil); err != nil && !errors.Is(err, context.Canceled) {
					s.log.Printf("repair stripe %s: %v", stripeID, err)
				}
			}(entry.StripeID, leaseToken)
		}
		if pending > startedInGroup {
			// A higher-risk stripe remains pending. Never let a lower-risk
			// stripe consume quota ahead of it.
			if quotaSkippedInGroup {
				summary.PausedReason = "quota_insufficient"
			} else {
				summary.PausedReason = "higher_risk_pending"
			}
			break
		}
	}
	summary.RunnableRemaining = len(queue.Queue)
	summary.ActiveRepairs += summary.Started
	if summary.RunnableRemaining == 0 && summary.ActiveRepairs == 0 {
		summary.PausedReason = ""
	}
	return summary, nil
}

func (s *Service) executeRepair(ctx context.Context, stripeID string, existing *model.RepairJob) error {
	s.mu.RLock()
	dirs, err := s.dirs(ctx)
	if err != nil {
		s.mu.RUnlock()
		return err
	}
	st, err := s.redis.GetStripe(ctx, stripeID)
	if err != nil {
		s.mu.RUnlock()
		return err
	}
	obj, err := s.redis.GetObject(ctx, st.ObjectID)
	if err != nil {
		s.mu.RUnlock()
		return err
	}
	st.Refresh(dirs)
	if len(st.MissingBlocks) == 0 {
		s.mu.RUnlock()
		if existing != nil {
			_ = s.failRepair(ctx, existing, st.ID, st.ObjectID, "superseded_no_missing_blocks", "stripe no longer needs repair")
		}
		return nil
	}
	if st.AvailableBlocks < model.DataBlocks {
		details, _ := json.Marshal(map[string]any{"available_blocks": st.AvailableBlocks, "missing_blocks": st.MissingBlocks})
		_ = s.db.RecordDirEvent(ctx, "repair", -1, "unrecoverable", []model.DegradationRecord{{
			StripeID: st.ID, ObjectID: st.ObjectID, DirIndex: -1, Action: "repair_unrecoverable",
			Reason: "available_blocks_below_4", AvailableBlocks: st.AvailableBlocks, Status: st.Status, Details: string(details),
		}}, string(details))
		s.mu.RUnlock()
		if existing != nil {
			_ = s.failRepair(ctx, existing, st.ID, st.ObjectID, "stripe_unrecoverable", "fewer than four blocks are available")
		}
		return &AppError{Code: "STRIPE_UNRECOVERABLE", Message: "stripe cannot be repaired", StripeID: st.ID,
			ObjectID: st.ObjectID, MissingBlocks: st.MissingBlocks, AvailableBlocks: st.AvailableBlocks}
	}
	plannedMissing := append([]int(nil), st.MissingBlocks...)
	estimated := int64(len(plannedMissing)) * st.ShardLength
	job := existing
	if job == nil {
		job = &model.RepairJob{
			JobID:         stripeID + ":r" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":" + joinInts(plannedMissing),
			StripeID:      stripeID,
			ObjectID:      st.ObjectID,
			MissingBlocks: plannedMissing,
			State:         "running",
			ReservedBytes: estimated,
			Day:           s.Today(time.Now()),
			StartedAt:     time.Now().UTC(),
		}
		details, _ := json.Marshal(map[string]any{
			"available_blocks": st.AvailableBlocks, "missing_blocks": plannedMissing,
			"estimated_bytes": estimated, "object_size": obj.Size,
		})
		ok, usage, err := s.db.ReserveRepair(ctx, *job, s.cfg.WritebackQuotaBytes, string(details))
		if err != nil {
			s.mu.RUnlock()
			return err
		}
		if !ok {
			s.mu.RUnlock()
			s.recordSkip(ctx, RepairQueueEntry{StripeID: stripeID, ObjectID: st.ObjectID, EstimatedBytes: estimated,
				AvailableBlocks: st.AvailableBlocks, MissingBlocks: plannedMissing}, "quota_insufficient")
			_ = s.redis.SetRepairState(ctx, stripeID, "", "", "quota_insufficient")
			_ = usage
			return nil
		}
	}
	if err := s.redis.SetRepairState(ctx, stripeID, "running", job.JobID, ""); err != nil {
		s.mu.RUnlock()
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "set_repair_state_failed", err.Error())
		return err
	}
	shards, missing, err := s.reconstructRepair(ctx, &st, dirs)
	s.mu.RUnlock()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "reconstruct_or_verify_failed", err.Error())
		return err
	}
	if len(missing) == 0 {
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "superseded_no_missing_blocks", "stripe no longer needs repair")
		return nil
	}
	actualBytes := int64(len(missing)) * st.ShardLength
	if actualBytes != job.ReservedBytes {
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "repair_size_changed",
			fmt.Sprintf("planned=%d actual=%d bytes", job.ReservedBytes, actualBytes))
		return fmt.Errorf("repair size changed: planned=%d actual=%d", job.ReservedBytes, actualBytes)
	}
	if s.cfg.RepairDelayMS > 0 {
		select {
		case <-time.After(time.Duration(s.cfg.RepairDelayMS) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	blocks := make([]repairedBlock, 0, len(missing))
	for _, idx := range missing {
		if idx < 0 || idx >= len(shards) || shards[idx] == nil {
			_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "reconstructed_shard_missing", fmt.Sprintf("block %d", idx))
			return fmt.Errorf("reconstructed shard %d missing", idx)
		}
		rel := standbyRel(idx, stripeID)
		if err := s.writeAtomic(rel, shards[idx]); err != nil {
			_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "standby_write_failed", err.Error())
			return err
		}
		blocks = append(blocks, repairedBlock{Index: idx, Path: rel, Length: int64(len(shards[idx])), SHA256: codec.SHA256(shards[idx])})
	}
	result := repairResult{
		Verification:   "parity_ok+logical_sha256_ok",
		DataSHA256:     st.DataSHA256,
		ParityVerified: true,
		LogicalBytes:   st.Length,
		ShardLength:    st.ShardLength,
		MissingBlocks:  missing,
		WritebackBytes: estimated,
		Blocks:         blocks,
	}
	resultJSON, _ := json.Marshal(result)

	s.mu.Lock()
	latest, err := s.redis.GetStripe(ctx, stripeID)
	if err != nil {
		s.mu.Unlock()
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "reload_stripe_failed", err.Error())
		return err
	}
	latestDir, err := s.dirs(ctx)
	if err != nil {
		s.mu.Unlock()
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "reload_dirs_failed", err.Error())
		return err
	}
	for _, b := range blocks {
		bm := latest.Block(b.Index)
		if bm == nil {
			continue
		}
		if bm.BackupState == "committed" && bm.BackupSHA256 == b.SHA256 {
			continue
		}
		bm.BackupPath = b.Path
		bm.BackupLength = b.Length
		bm.BackupSHA256 = b.SHA256
		bm.BackupState = "committed"
		bm.BackupWritten = time.Now().UTC().Format(time.RFC3339Nano)
		bm.BackupJobID = job.JobID
	}
	latest.RepairEpoch++
	latest.RepairState = ""
	latest.RepairJobID = ""
	latest.LastError = ""
	latest.Refresh(latestDir)
	details, _ := json.Marshal(map[string]any{
		"job_id": job.JobID, "missing_blocks": missing, "writeback_bytes": estimated,
		"verification": result.Verification, "data_sha256": result.DataSHA256,
		"available_blocks": latest.AvailableBlocks, "status": latest.Status,
	})
	if err := s.db.CompleteRepair(ctx, job.JobID, stripeID, latest.ObjectID, string(resultJSON), string(details)); err != nil {
		s.mu.Unlock()
		_ = s.failRepair(ctx, job, st.ID, st.ObjectID, "sqlite_complete_failed", err.Error())
		return err
	}
	if err := s.redis.CommitRepair(ctx, &latest); err != nil {
		s.mu.Unlock()
		s.log.Printf("repair %s committed in sqlite but redis metadata update failed: %v", job.JobID, err)
		return err
	}
	if err := s.db.MarkMetadataApplied(ctx, job.JobID); err != nil {
		s.log.Printf("mark metadata applied job=%s: %v", job.JobID, err)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) reconstructRepair(ctx context.Context, st *model.StripeMeta, dirs model.DirSet) ([][]byte, []int, error) {
	shards := make([][]byte, model.BlockCount)
	present := make([]bool, model.BlockCount)
	missing := make([]int, 0, model.BlockCount)
	for i := 0; i < model.BlockCount; i++ {
		srcs := model.SourcesForBlock(st, dirs, i)
		found := false
		for _, src := range srcs {
			data, err := s.readRel(src.Path)
			if err != nil {
				continue
			}
			if int64(len(data)) != src.Length || codec.SHA256(data) != src.SHA256 {
				continue
			}
			shards[i] = data
			present[i] = true
			found = true
			break
		}
		if !found {
			missing = append(missing, i)
		}
	}
	available := model.BlockCount - len(missing)
	if available < model.DataBlocks {
		return nil, missing, &AppError{Code: "STRIPE_UNRECOVERABLE", Message: "fewer than four verified blocks are available",
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: missing, AvailableBlocks: available}
	}
	if err := s.codec.Reconstruct(shards, present); err != nil {
		return nil, missing, &AppError{Code: "STRIPE_RECONSTRUCT_FAILED", Message: err.Error(),
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: missing, AvailableBlocks: available}
	}
	ok, err := s.codec.Verify(shards)
	if err != nil || !ok {
		msg := "reconstructed stripe failed parity verification"
		if err != nil {
			msg = err.Error()
		}
		return nil, missing, &AppError{Code: "STRIPE_INTEGRITY_FAILED", Message: msg,
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: missing, AvailableBlocks: available}
	}
	logical, err := s.codec.LogicalData(shards, st.Length)
	if err != nil {
		return nil, missing, err
	}
	if codec.SHA256(logical) != st.DataSHA256 {
		return nil, missing, &AppError{Code: "STRIPE_INTEGRITY_FAILED", Message: "logical stripe digest mismatch after reconstruction",
			StripeID: st.ID, ObjectID: st.ObjectID, MissingBlocks: missing, AvailableBlocks: available}
	}
	return shards, missing, nil
}

func (s *Service) failRepair(ctx context.Context, job *model.RepairJob, stripeID, objectID, reason, message string) error {
	if job == nil {
		return nil
	}
	message = s.sanitizeMessage(message)
	result := map[string]any{"verified": false, "reason": reason, "message": message}
	resultJSON, _ := json.Marshal(result)
	details, _ := json.Marshal(map[string]any{"job_id": job.JobID, "reason": reason, "message": message})
	_ = s.redis.SetRepairFailure(context.Background(), stripeID, reason, time.Now().Add(30*time.Second))
	return s.db.FailRepair(ctx, job.JobID, stripeID, objectID, reason, string(resultJSON), string(details), true)
}

func (s *Service) applyCompletedRepairs(ctx context.Context) (int, error) {
	jobs, err := s.db.CompletedUnappliedJobs(ctx)
	if err != nil {
		return 0, err
	}
	applied := 0
	for i := range jobs {
		job := jobs[i]
		var result repairResult
		if err := json.Unmarshal([]byte(job.ResultJSON), &result); err != nil {
			continue
		}
		s.mu.Lock()
		st, err := s.redis.GetStripe(ctx, job.StripeID)
		if err != nil {
			s.mu.Unlock()
			continue
		}
		dirs, err := s.dirs(ctx)
		if err != nil {
			s.mu.Unlock()
			return applied, err
		}
		changed := false
		for _, b := range result.Blocks {
			bm := st.Block(b.Index)
			if bm == nil {
				continue
			}
			if bm.BackupState == "committed" && bm.BackupSHA256 == b.SHA256 {
				continue
			}
			bm.BackupPath = b.Path
			bm.BackupLength = b.Length
			bm.BackupSHA256 = b.SHA256
			bm.BackupState = "committed"
			bm.BackupWritten = time.Now().UTC().Format(time.RFC3339Nano)
			bm.BackupJobID = job.JobID
			changed = true
		}
		if changed {
			st.RepairEpoch++
		}
		st.RepairState = ""
		st.RepairJobID = ""
		st.Refresh(dirs)
		if err := s.redis.CommitRepair(ctx, &st); err != nil {
			s.mu.Unlock()
			return applied, err
		}
		s.mu.Unlock()
		if err := s.db.MarkMetadataApplied(ctx, job.JobID); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

func (s *Service) RepairQueue(ctx context.Context, limit int) (RepairQueue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dirs, err := s.dirs(ctx)
	if err != nil {
		return RepairQueue{}, err
	}
	stripes, err := s.redis.ListAllStripes(ctx)
	if err != nil {
		return RepairQueue{}, err
	}
	objects, err := s.redis.ListObjects(ctx)
	if err != nil {
		return RepairQueue{}, err
	}
	objSize := make(map[string]int64, len(objects))
	for _, o := range objects {
		objSize[o.ID] = o.Size
	}
	quota, err := s.db.QuotaUsage(ctx, s.Today(time.Now()), s.cfg.WritebackQuotaBytes)
	if err != nil {
		return RepairQueue{}, err
	}
	inProgress, _, _, err := s.db.RepairCounts(ctx)
	if err != nil {
		return RepairQueue{}, err
	}
	queue := RepairQueue{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		Day:               quota.Day,
		Quota:             quota,
		RepairConcurrency: s.cfg.RepairConcurrency,
		ActiveRepairs:     inProgress,
		Queue:             []RepairQueueEntry{},
		Skipped:           []RepairQueueEntry{},
	}
	for i := range stripes {
		st := &stripes[i]
		st.Refresh(dirs)
		if len(st.MissingBlocks) == 0 || st.AvailableBlocks < model.DataBlocks {
			continue
		}
		entry := RepairQueueEntry{
			StripeID:        st.ID,
			ObjectID:        st.ObjectID,
			StripeIndex:     st.Index,
			ObjectSize:      objSize[st.ObjectID],
			AvailableBlocks: st.AvailableBlocks,
			MissingBlocks:   append([]int(nil), st.MissingBlocks...),
			Risk:            riskLabel(st.AvailableBlocks),
			EstimatedBytes:  int64(len(st.MissingBlocks)) * st.ShardLength,
			LastReadAt:      formatOptionalTime(st.LastReadAt),
			PriorityReason:  priorityReason(st, objSize[st.ObjectID]),
			RepairState:     st.RepairState,
			RepairJobID:     st.RepairJobID,
			CooldownUntil:   formatOptionalTime(st.CooldownUntil),
			State:           "runnable",
		}
		queue.Queue = append(queue.Queue, entry)
	}
	sort.Slice(queue.Queue, func(i, j int) bool {
		a, b := queue.Queue[i], queue.Queue[j]
		if a.AvailableBlocks != b.AvailableBlocks {
			return a.AvailableBlocks < b.AvailableBlocks
		}
		at, bt := a.LastReadAt, b.LastReadAt
		if at != bt {
			if at == "" {
				return true
			}
			if bt == "" {
				return false
			}
			return at < bt
		}
		if a.ObjectSize != b.ObjectSize {
			return a.ObjectSize < b.ObjectSize
		}
		return a.StripeID < b.StripeID
	})
	queue.TotalCandidates = len(queue.Queue)
	highestAvailable := 0
	for _, e := range queue.Queue {
		if e.RepairState != "running" {
			highestAvailable = e.AvailableBlocks
			break
		}
	}
	for i := range queue.Queue {
		e := &queue.Queue[i]
		e.Rank = i + 1
		switch {
		case e.RepairState == "running":
			e.State = "in_progress"
			e.SkipReason = "repair_in_progress"
		case e.CooldownUntil != "" && timeAfter(e.CooldownUntil, time.Now()):
			e.State = "cooldown"
			e.SkipReason = "repair_cooldown"
		case e.EstimatedBytes > quota.RemainingBytes:
			e.State = "waiting_quota"
			e.SkipReason = "quota_insufficient"
		case highestAvailable > 0 && e.AvailableBlocks > highestAvailable:
			e.State = "waiting_higher_risk"
			e.SkipReason = "higher_risk_pending"
		}
		if e.State != "runnable" {
			queue.Skipped = append(queue.Skipped, *e)
		}
	}
	if limit > 0 && len(queue.Queue) > limit {
		queue.Queue = queue.Queue[:limit]
	}
	if limit > 0 && len(queue.Skipped) > limit {
		queue.Skipped = queue.Skipped[:limit]
	}
	return queue, nil
}

func riskLabel(available int) string {
	switch {
	case available <= 4:
		return "critical"
	case available == 5:
		return "high"
	default:
		return "normal"
	}
}

func priorityReason(st *model.StripeMeta, objectSize int64) string {
	tie := "last_read_at=never"
	if !st.LastReadAt.IsZero() {
		tie = "last_read_at=" + st.LastReadAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("available_blocks=%d;%s;object_size=%d;stripe_id=%s", st.AvailableBlocks, tie, objectSize, st.ID)
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeAfter(raw string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && t.After(now)
}

func (s *Service) recordSkip(ctx context.Context, entry RepairQueueEntry, reason string) {
	key := entry.StripeID + "|" + reason
	now := time.Now()
	s.skipMu.Lock()
	last := s.skipRecord[key]
	if now.Sub(last) < 30*time.Second {
		s.skipMu.Unlock()
		return
	}
	s.skipRecord[key] = now
	s.skipMu.Unlock()
	details, _ := json.Marshal(map[string]any{
		"available_blocks": entry.AvailableBlocks, "missing_blocks": entry.MissingBlocks,
		"estimated_bytes": entry.EstimatedBytes, "reason": reason,
	})
	_ = s.redis.SetSkip(ctx, entry.StripeID, reason, now)
	_ = s.db.RecordRepairSkip(ctx, entry.StripeID, entry.ObjectID, reason, string(details))
}

func (s *Service) DrainRepairs(ctx context.Context, timeout time.Duration) (ScheduleSummary, error) {
	deadline := time.Now().Add(timeout)
	var summary ScheduleSummary
	var err error
	totalStarted := 0
	startedStripes := make([]string, 0)
	lastPaused := ""
	finalize := func() ScheduleSummary {
		summary.Started = totalStarted
		summary.StartedStripes = startedStripes
		summary.ActiveRepairs = int(s.active.Load())
		if lastPaused != "" && !(summary.RunnableRemaining == 0 && summary.ActiveRepairs == 0) {
			summary.PausedReason = lastPaused
		}
		if summary.RunnableRemaining == 0 && summary.ActiveRepairs == 0 {
			summary.PausedReason = ""
		}
		return summary
	}
	for {
		if time.Now().After(deadline) {
			summary.PausedReason = "timeout"
			return finalize(), nil
		}
		summary, err = s.scheduleOnce(ctx)
		if err != nil {
			return finalize(), err
		}
		totalStarted += summary.Started
		startedStripes = append(startedStripes, summary.StartedStripes...)
		if summary.PausedReason != "" {
			lastPaused = summary.PausedReason
		}
		inProgress, _, _, err := s.db.RepairCounts(ctx)
		if err != nil {
			return finalize(), err
		}
		queue, err := s.RepairQueue(ctx, 50000)
		if err != nil {
			return finalize(), err
		}
		runnable := 0
		for _, e := range queue.Queue {
			if e.State == "runnable" || e.State == "in_progress" {
				runnable++
			}
		}
		active := s.active.Load()
		if active == 0 && inProgress == 0 && (runnable == 0 || summary.PausedReason == "quota_insufficient") {
			return finalize(), nil
		}
		select {
		case <-ctx.Done():
			return finalize(), ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func (s *Service) sanitizeMessage(msg string) string {
	msg = strings.ReplaceAll(msg, s.dataRoot, "<data_root>")
	msg = strings.ReplaceAll(msg, s.dbPath, "<sqlite_path>")
	if len(msg) > 512 {
		return msg[:512]
	}
	return msg
}
