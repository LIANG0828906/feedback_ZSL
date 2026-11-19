package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quota-coordinator/internal/config"
	"quota-coordinator/internal/model"
	"quota-coordinator/internal/store"
)

var (
	ErrUnknownTenant = errors.New("unknown tenant")
	ErrInvalidQuota  = errors.New("invalid quota")
)

const (
	CodeAccepted           = "ACCEPTED"
	CodeDuplicate          = "DUPLICATE_ACCEPTED"
	CodePartialDrop        = "PARTIAL_QUOTA_DROP"
	CodeQuotaExceeded      = "TENANT_QUOTA_EXCEEDED"
	CodeGlobalBackpressure = "GLOBAL_BACKPRESSURE"
	CodeTenantDegraded     = "TENANT_DEGRADED"
	CodeUnknownTenant      = "UNKNOWN_TENANT"
	CodeInvalidBatch       = "INVALID_BATCH"
	CodeInvalidQuota       = "INVALID_QUOTA"
	CodeRedisUnavailable   = "REDIS_UNAVAILABLE"
	CodePartialAccept      = "PARTIAL_ACCEPT"
)

type counters struct {
	IngestedBatches             uint64
	IngestedRecords             uint64
	IngestedBytes               uint64
	DroppedBatches              uint64
	DroppedRecords              uint64
	BackpressureRejectedBatches uint64
	WriteTimeouts               uint64
}

type pendingBatch struct {
	BatchID      string
	Seq          int64
	Count        int64
	Bytes        int64
	EnqueuedAt   time.Time
	TimedOut     bool
	DegradeEpoch uint64
}

type consumeSample struct {
	At    time.Time
	Count int64
	Bytes int64
}

type DegradeRecord struct {
	TriggerMetric      string `json:"trigger_metric"`
	TriggerValue       int64  `json:"trigger_value"`
	Threshold          int64  `json:"threshold"`
	WindowSamples      int    `json:"window_samples"`
	StartedAt          string `json:"started_at"`
	RecoveredAt        string `json:"recovered_at,omitempty"`
	RecoveryAckSamples int    `json:"recovery_ack_samples"`
	RecoverySuccesses  int    `json:"recovery_successes"`
	RecoveryTimeouts   int    `json:"recovery_timeouts"`
}

type tenantState struct {
	cfg              config.TenantConfig
	windowSecond     int64
	windowCount      int64
	windowBytes      int64
	pending          map[string]*pendingBatch
	pendingBytes     int64
	timeoutEvents    []time.Time
	lastTimeout      time.Time
	degraded         bool
	degradeEpoch     uint64
	degrade          *DegradeRecord
	history          []DegradeRecord
	recoverySamples  int
	lastAck          time.Time
	counter          counters
	backlogBatches   int64
	backlogBytes     int64
	consumeSamples   []consumeSample
	consumeRecordsPS float64
	consumeBytesPS   float64
	lastError        string
}

type backpressureState struct {
	Active          bool
	EnteredAt       time.Time
	ExitedAt        time.Time
	ExitCandidateAt time.Time
	TotalDuration   time.Duration
	EntryCount      uint64
	MaxBacklog      int64
	LastBacklog     int64
}

type Service struct {
	cfg       config.Config
	store     *store.Store
	startedAt time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	mu           sync.Mutex
	tenants      map[string]*tenantState
	backpressure backpressureState

	redisDelayMS atomic.Int64
	lastRedisErr atomic.Value
}

type GroupResult struct {
	TenantID   string `json:"tenant_id"`
	BatchID    string `json:"batch_id"`
	Seq        int64  `json:"seq,omitempty"`
	Accepted   int64  `json:"accepted_records"`
	Dropped    int64  `json:"dropped_records"`
	Duplicate  bool   `json:"duplicate"`
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	RetryAfter int    `json:"retry_after_seconds,omitempty"`
}

type IngestResponse struct {
	RequestID    string        `json:"request_id"`
	OK           bool          `json:"ok"`
	Code         string        `json:"code"`
	Message      string        `json:"message,omitempty"`
	Accepted     int64         `json:"accepted_records"`
	Dropped      int64         `json:"dropped_records"`
	Backpressure bool          `json:"backpressure"`
	Results      []GroupResult `json:"results"`
}

type ingestPlan struct {
	batch      model.Batch
	accepted   model.Batch
	duplicate  bool
	code       string
	message    string
	seq        int64
	refundable bool
}

func New(cfg config.Config, st *store.Store) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg:       cfg,
		store:     st,
		startedAt: time.Now().UTC(),
		ctx:       ctx,
		cancel:    cancel,
		tenants:   make(map[string]*tenantState, len(cfg.Tenants)),
	}
	for id, tenantCfg := range cfg.Tenants {
		config.NormalizeTenant(&tenantCfg)
		s.tenants[id] = &tenantState{
			cfg:     tenantCfg,
			pending: make(map[string]*pendingBatch),
		}
	}
	if err := s.loadPending(ctx); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func (s *Service) Start() {
	s.wg.Add(2)
	go s.backpressureLoop()
	go s.timeoutLoop()
}

func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) Store() *store.Store {
	return s.store
}

func (s *Service) Config() config.Config {
	return s.cfg
}

func (s *Service) SetRedisDelay(delay time.Duration) {
	s.redisDelayMS.Store(delay.Milliseconds())
}

func (s *Service) RedisDelay() time.Duration {
	return time.Duration(s.redisDelayMS.Load()) * time.Millisecond
}

func (s *Service) applyRedisDelay(ctx context.Context) error {
	delay := s.RedisDelay()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) loadPending(ctx context.Context) error {
	for tenant := range s.tenants {
		jobs, err := s.store.LoadPending(ctx, tenant)
		if err != nil {
			return fmt.Errorf("load pending %s: %w", tenant, err)
		}
		state := s.tenants[tenant]
		for _, job := range jobs {
			receipt, err := s.store.Receipt(ctx, tenant, job.BatchID)
			if err == nil && receipt.Acknowledged {
				continue
			}
			enqueued, err := time.Parse(time.RFC3339Nano, job.Enqueued)
			if err != nil {
				enqueued = time.Now().UTC()
			}
			state.pending[job.BatchID] = &pendingBatch{
				BatchID:    job.BatchID,
				Seq:        job.Seq,
				Count:      job.Count,
				Bytes:      job.Bytes,
				EnqueuedAt: enqueued,
			}
			state.pendingBytes += job.Bytes
		}
		state.backlogBatches = int64(len(state.pending))
	}
	return nil
}

func (s *Service) Ingest(ctx context.Context, batches []model.Batch) IngestResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	response := IngestResponse{OK: true}
	if len(batches) == 0 {
		response.OK = false
		response.Code = CodeInvalidBatch
		return response
	}
	plans := make([]ingestPlan, 0, len(batches))
	type windowSnapshot struct {
		sec   int64
		count int64
		bytes int64
	}
	snapshots := make(map[string]windowSnapshot, len(s.tenants))
	for id, state := range s.tenants {
		snapshots[id] = windowSnapshot{sec: state.windowSecond, count: state.windowCount, bytes: state.windowBytes}
	}
	globalBPFailure := false
	for _, batch := range batches {
		plan := s.planBatchLocked(ctx, batch)
		if plan.code == CodeGlobalBackpressure {
			globalBPFailure = true
		}
		plans = append(plans, plan)
	}
	if globalBPFailure {
		for id, snapshot := range snapshots {
			state := s.tenants[id]
			state.windowSecond = snapshot.sec
			state.windowCount = snapshot.count
			state.windowBytes = snapshot.bytes
		}
		for _, plan := range plans {
			if plan.duplicate {
				response.Results = append(response.Results, GroupResult{
					TenantID:  plan.batch.TenantID,
					BatchID:   plan.batch.BatchID,
					Seq:       plan.seq,
					Duplicate: true,
					Code:      CodeDuplicate,
				})
				continue
			}
			state := s.tenants[plan.batch.TenantID]
			if state != nil {
				state.droppedLocked(plan.batch.Count)
				state.counter.BackpressureRejectedBatches++
			}
			response.Results = append(response.Results, GroupResult{
				TenantID:   plan.batch.TenantID,
				BatchID:    plan.batch.BatchID,
				Dropped:    plan.batch.Count,
				Code:       CodeGlobalBackpressure,
				Message:    "global backlog threshold exceeded; retry after the advertised delay",
				RetryAfter: s.retryAfterSeconds(),
			})
			response.Dropped += plan.batch.Count
		}
		response.OK = false
		response.Code = CodeGlobalBackpressure
		response.Backpressure = true
		return response
	}

	for i := range plans {
		plan := &plans[i]
		if plan.duplicate {
			response.Results = append(response.Results, GroupResult{
				TenantID:  plan.batch.TenantID,
				BatchID:   plan.batch.BatchID,
				Seq:       plan.seq,
				Duplicate: true,
				Code:      CodeDuplicate,
			})
			continue
		}
		if plan.code != CodeAccepted && plan.code != CodePartialDrop {
			response.Results = append(response.Results, GroupResult{
				TenantID:   plan.batch.TenantID,
				BatchID:    plan.batch.BatchID,
				Dropped:    plan.batch.Count,
				Code:       plan.code,
				Message:    plan.message,
				RetryAfter: s.retryAfterSeconds(),
			})
			response.OK = false
			response.Dropped += plan.batch.Count
			if state := s.tenants[plan.batch.TenantID]; state != nil {
				state.droppedLocked(plan.batch.Count)
				if plan.code == CodeTenantDegraded {
					state.counter.BackpressureRejectedBatches++
				}
			}
			continue
		}
		state := s.tenants[plan.batch.TenantID]
		seq, duplicate, err := s.enqueueLocked(ctx, state, plan.accepted)
		if err != nil {
			s.lastRedisErr.Store(err.Error())
			state.refundLocked(plan.accepted.Count, plan.accepted.Bytes)
			state.lastError = err.Error()
			response.Results = append(response.Results, GroupResult{
				TenantID:   plan.batch.TenantID,
				BatchID:    plan.batch.BatchID,
				Dropped:    plan.batch.Count,
				Code:       CodeRedisUnavailable,
				Message:    "redis enqueue failed",
				RetryAfter: s.retryAfterSeconds(),
			})
			response.OK = false
			response.Dropped += plan.batch.Count
			state.droppedLocked(plan.batch.Count)
			continue
		}
		if duplicate {
			response.Results = append(response.Results, GroupResult{
				TenantID:  plan.batch.TenantID,
				BatchID:   plan.batch.BatchID,
				Seq:       seq,
				Duplicate: true,
				Code:      CodeDuplicate,
			})
			continue
		}
		plan.seq = seq
		state.ingestedLocked(plan.accepted)
		dropped := plan.batch.Count - plan.accepted.Count
		if dropped > 0 {
			state.droppedLocked(dropped)
			response.Dropped += dropped
			response.OK = false
		}
		response.Accepted += plan.accepted.Count
		response.Results = append(response.Results, GroupResult{
			TenantID: plan.batch.TenantID,
			BatchID:  plan.batch.BatchID,
			Seq:      seq,
			Accepted: plan.accepted.Count,
			Dropped:  dropped,
			Code:     plan.code,
		})
	}
	if response.OK {
		response.Code = CodeAccepted
	} else {
		response.Code = CodePartialAccept
		if response.Accepted == 0 {
			for _, item := range response.Results {
				if item.Code != CodeDuplicate && item.Code != CodeAccepted {
					response.Code = item.Code
					break
				}
			}
		}
	}
	return response
}

func (s *Service) planBatchLocked(ctx context.Context, batch model.Batch) ingestPlan {
	plan := ingestPlan{batch: batch, accepted: batch, code: CodeAccepted}
	if batch.BatchID == "" || batch.Count <= 0 || batch.Bytes <= 0 {
		plan.code = CodeInvalidBatch
		plan.message = "batch must have an id and at least one log record"
		return plan
	}
	state := s.tenants[batch.TenantID]
	if state == nil {
		plan.code = CodeUnknownTenant
		plan.message = "tenant is not configured"
		return plan
	}
	seq, exists, err := s.store.Seen(ctx, batch.TenantID, batch.BatchID)
	if err != nil {
		s.lastRedisErr.Store(err.Error())
		plan.code = CodeRedisUnavailable
		plan.message = "redis idempotency lookup failed"
		return plan
	}
	if exists {
		plan.duplicate = true
		plan.seq = seq
		plan.code = CodeDuplicate
		return plan
	}
	now := time.Now().UTC()
	state.rollWindowLocked(now)
	if state.degraded {
		wouldExceed := len(state.pending)+1 > int(state.cfg.MaxPendingBatches)
		if wouldExceed || state.pendingBytes+batch.Bytes > state.cfg.MaxPendingBytes {
			plan.code = CodeTenantDegraded
			plan.message = "tenant is in a limited queue because of repeated write timeouts"
			return plan
		}
	}
	limitCount, limitBytes := s.effectiveLimitsLocked(state)
	remainingCount := limitCount - state.windowCount
	remainingBytes := limitBytes - state.windowBytes
	if remainingCount < 0 {
		remainingCount = 0
	}
	if remainingBytes < 0 {
		remainingBytes = 0
	}
	if batch.Count <= remainingCount && batch.Bytes <= remainingBytes {
		state.windowCount += batch.Count
		state.windowBytes += batch.Bytes
		plan.refundable = true
		return plan
	}
	policy := strings.ToLower(state.cfg.DropPolicy)
	if policy == config.DropPolicyDrop {
		accepted := prefixThatFits(batch, remainingCount, remainingBytes)
		if accepted.Count > 0 {
			plan.accepted = accepted
			plan.code = CodePartialDrop
			plan.message = "records beyond the tenant quota were dropped by policy"
			state.windowCount += accepted.Count
			state.windowBytes += accepted.Bytes
			plan.refundable = true
			return plan
		}
	}
	if s.backpressure.Active {
		plan.code = CodeGlobalBackpressure
		plan.message = "global backpressure is applying a reduced admission ratio"
	} else {
		plan.code = CodeQuotaExceeded
		plan.message = "tenant per-second count or byte quota would be exceeded"
	}
	return plan
}

func prefixThatFits(batch model.Batch, maxCount, maxBytes int64) model.Batch {
	out := model.Batch{TenantID: batch.TenantID, BatchID: batch.BatchID}
	if maxCount <= 0 || maxBytes <= 0 {
		return out
	}
	for _, record := range batch.Records {
		if out.Count+1 > maxCount || out.Bytes+record.Bytes > maxBytes {
			break
		}
		out.Records = append(out.Records, record)
		out.Count++
		out.Bytes += record.Bytes
	}
	return out
}

func (s *Service) enqueueLocked(ctx context.Context, state *tenantState, batch model.Batch) (int64, bool, error) {
	if err := s.applyRedisDelay(ctx); err != nil {
		return 0, false, err
	}
	seq, err := s.store.NextSeq(ctx, batch.TenantID)
	if err != nil {
		return 0, false, err
	}
	now := time.Now().UTC()
	job := model.QueueJob{
		TenantID: batch.TenantID,
		BatchID:  batch.BatchID,
		Seq:      seq,
		Records:  batch.Records,
		Count:    batch.Count,
		Bytes:    batch.Bytes,
		Enqueued: now.Format(time.RFC3339Nano),
	}
	actualSeq, duplicate, err := s.store.Enqueue(ctx, batch.TenantID, batch.BatchID, seq, job)
	if err != nil {
		return 0, false, err
	}
	if duplicate {
		return actualSeq, true, nil
	}
	state.pending[batch.BatchID] = &pendingBatch{
		BatchID:      batch.BatchID,
		Seq:          actualSeq,
		Count:        batch.Count,
		Bytes:        batch.Bytes,
		EnqueuedAt:   now,
		DegradeEpoch: state.degradeEpoch,
	}
	state.pendingBytes += batch.Bytes
	state.backlogBatches = int64(len(state.pending))
	state.backlogBytes = state.pendingBytes
	return actualSeq, false, nil
}

func (s *Service) effectiveLimitsLocked(state *tenantState) (int64, int64) {
	countLimit := state.cfg.LogsPerSecond
	byteLimit := state.cfg.BytesPerSecond
	if s.backpressure.Active {
		countLimit = int64(math.Floor(float64(countLimit) * s.cfg.Backpressure.Ratio))
		byteLimit = int64(math.Floor(float64(byteLimit) * s.cfg.Backpressure.Ratio))
		if countLimit < 1 {
			countLimit = 1
		}
		if byteLimit < 1 {
			byteLimit = 1
		}
	}
	return countLimit, byteLimit
}

func (s *Service) retryAfterSeconds() int {
	if s.cfg.Backpressure.MaxRetryAfterSeconds > 0 {
		return s.cfg.Backpressure.MaxRetryAfterSeconds
	}
	return 1
}

func (s *Service) Acknowledge(ctx context.Context, tenant, batchID string, seq, storageOffset int64) (model.Cursor, error) {
	if _, ok := s.cfg.Tenants[tenant]; !ok {
		return model.Cursor{}, ErrUnknownTenant
	}
	receipt, err := s.store.Receipt(ctx, tenant, batchID)
	if err != nil {
		return model.Cursor{}, err
	}
	if seq <= 0 {
		seq = receipt.Seq
	}
	now := time.Now().UTC()
	cursor, err := s.store.Ack(ctx, store.AckInput{
		TenantID:      tenant,
		BatchID:       batchID,
		Seq:           seq,
		AckedAt:       now,
		StorageOffset: storageOffset,
	})
	if err != nil {
		return model.Cursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.tenants[tenant]
	if pending := state.pending[batchID]; pending != nil {
		state.pendingBytes -= pending.Bytes
		if state.pendingBytes < 0 {
			state.pendingBytes = 0
		}
		delete(state.pending, batchID)
		state.backlogBatches = int64(len(state.pending))
		state.backlogBytes = state.pendingBytes
	}
	if !receipt.Acknowledged {
		state.consumeSamples = append(state.consumeSamples, consumeSample{At: now, Count: receipt.RecordCount, Bytes: receipt.Bytes})
		state.lastAck = now
		if state.degraded {
			state.recoverySamples++
			state.degrade.RecoverySuccesses++
			state.degrade.RecoveryAckSamples = state.recoverySamples
			state.tryRecoverLocked(now, s.cfg.Degrade.RecoveryQuietMS, s.cfg.Degrade.RecoverySuccessSamples)
		}
	}
	return cursor, nil
}

func (s *Service) Quota(ctx context.Context, tenant string) (config.TenantConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.tenants[tenant]
	if state == nil {
		return config.TenantConfig{}, ErrUnknownTenant
	}
	return state.cfg, nil
}

type QuotaPatch struct {
	LogsPerSecond     *int64  `json:"logs_per_second"`
	BytesPerSecond    *int64  `json:"bytes_per_second"`
	DropPolicy        *string `json:"drop_policy"`
	MaxPendingBatches *int64  `json:"max_degraded_pending_batches"`
	MaxPendingBytes   *int64  `json:"max_degraded_pending_bytes"`
}

func (s *Service) UpdateQuota(tenant string, patch QuotaPatch) (config.TenantConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.tenants[tenant]
	if state == nil {
		return config.TenantConfig{}, ErrUnknownTenant
	}
	next := state.cfg
	changed := false
	if patch.LogsPerSecond != nil {
		if *patch.LogsPerSecond <= 0 {
			return state.cfg, fmt.Errorf("%w: logs_per_second must be positive", ErrInvalidQuota)
		}
		next.LogsPerSecond = *patch.LogsPerSecond
		changed = true
	}
	if patch.BytesPerSecond != nil {
		if *patch.BytesPerSecond <= 0 {
			return state.cfg, fmt.Errorf("%w: bytes_per_second must be positive", ErrInvalidQuota)
		}
		next.BytesPerSecond = *patch.BytesPerSecond
		changed = true
	}
	if patch.DropPolicy != nil {
		policy := strings.ToLower(*patch.DropPolicy)
		if policy != config.DropPolicyReject && policy != config.DropPolicyDrop {
			return state.cfg, fmt.Errorf("%w: unsupported drop_policy", ErrInvalidQuota)
		}
		next.DropPolicy = policy
		changed = true
	}
	if patch.MaxPendingBatches != nil {
		if *patch.MaxPendingBatches <= 0 {
			return state.cfg, fmt.Errorf("%w: max_degraded_pending_batches must be positive", ErrInvalidQuota)
		}
		next.MaxPendingBatches = *patch.MaxPendingBatches
		changed = true
	}
	if patch.MaxPendingBytes != nil {
		if *patch.MaxPendingBytes <= 0 {
			return state.cfg, fmt.Errorf("%w: max_degraded_pending_bytes must be positive", ErrInvalidQuota)
		}
		next.MaxPendingBytes = *patch.MaxPendingBytes
		changed = true
	}
	if !changed {
		return state.cfg, fmt.Errorf("%w: no quota fields supplied", ErrInvalidQuota)
	}
	state.cfg = next
	return next, nil
}

func (s *Service) Receipt(ctx context.Context, tenant, batchID string) (model.Receipt, error) {
	if _, ok := s.cfg.Tenants[tenant]; !ok {
		return model.Receipt{}, ErrUnknownTenant
	}
	return s.store.Receipt(ctx, tenant, batchID)
}

func (s *Service) Cursors(ctx context.Context) ([]model.Cursor, error) {
	return s.store.Cursors(ctx, s.cfg.TenantIDs())
}

func (s *Service) Cursor(ctx context.Context, tenant string) (model.Cursor, error) {
	if _, ok := s.cfg.Tenants[tenant]; !ok {
		return model.Cursor{}, ErrUnknownTenant
	}
	return s.store.Cursor(ctx, tenant)
}

func (s *Service) ingestBatchesLocked(batch model.Batch) {}

func (t *tenantState) rollWindowLocked(now time.Time) {
	second := now.Unix()
	if t.windowSecond != second {
		t.windowSecond = second
		t.windowCount = 0
		t.windowBytes = 0
	}
}

func (t *tenantState) refundLocked(count, bytes int64) {
	t.windowCount -= count
	t.windowBytes -= bytes
	if t.windowCount < 0 {
		t.windowCount = 0
	}
	if t.windowBytes < 0 {
		t.windowBytes = 0
	}
}

func (t *tenantState) ingestedLocked(batch model.Batch) {
	t.counter.IngestedBatches++
	t.counter.IngestedRecords += uint64(batch.Count)
	t.counter.IngestedBytes += uint64(batch.Bytes)
}

func (t *tenantState) droppedLocked(count int64) {
	t.counter.DroppedBatches++
	t.counter.DroppedRecords += uint64(count)
}

func (s *Service) backpressureLoop() {
	defer s.wg.Done()
	interval := s.cfg.BackpressurePollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.sampleBackpressure(now.UTC())
		}
	}
}

func (s *Service) sampleBackpressure(now time.Time) {
	depths := make(map[string]store.QueueDepth, len(s.tenants))
	var total int64
	for tenant := range s.tenants {
		depth, err := s.store.Depth(s.ctx, tenant)
		if err != nil {
			s.lastRedisErr.Store(err.Error())
			continue
		}
		depths[tenant] = depth
		total += depth.TotalBatches
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tenant, depth := range depths {
		state := s.tenants[tenant]
		state.backlogBatches = depth.TotalBatches
	}
	s.backpressure.LastBacklog = total
	if total > s.backpressure.MaxBacklog {
		s.backpressure.MaxBacklog = total
	}
	if !s.cfg.Backpressure.Enabled {
		return
	}
	if !s.backpressure.Active && total >= s.cfg.Backpressure.EnterBacklogBatches {
		s.backpressure.Active = true
		s.backpressure.EnteredAt = now
		s.backpressure.EntryCount++
		s.backpressure.ExitCandidateAt = time.Time{}
		for _, state := range s.tenants {
			state.rollWindowLocked(now)
		}
		return
	}
	if !s.backpressure.Active {
		return
	}
	if total > s.cfg.Backpressure.ExitBacklogBatches {
		s.backpressure.ExitCandidateAt = time.Time{}
		return
	}
	if s.backpressure.ExitCandidateAt.IsZero() {
		s.backpressure.ExitCandidateAt = now
		return
	}
	hold := time.Duration(s.cfg.Backpressure.ExitHoldMS) * time.Millisecond
	if now.Sub(s.backpressure.ExitCandidateAt) < hold {
		return
	}
	s.backpressure.Active = false
	s.backpressure.ExitedAt = now
	if !s.backpressure.EnteredAt.IsZero() {
		s.backpressure.TotalDuration += now.Sub(s.backpressure.EnteredAt)
	}
	s.backpressure.ExitCandidateAt = time.Time{}
	second := now.Unix()
	for _, state := range s.tenants {
		state.windowSecond = second
		state.windowCount = state.cfg.LogsPerSecond
		state.windowBytes = state.cfg.BytesPerSecond
	}
}

func (s *Service) timeoutLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.DegradeScanInterval())
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.scanTimeouts(now.UTC())
		}
	}
}

func (s *Service) scanTimeouts(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	timeout := time.Duration(s.cfg.Degrade.WriteTimeoutMS) * time.Millisecond
	window := time.Duration(s.cfg.Degrade.WindowMS) * time.Millisecond
	for _, state := range s.tenants {
		for _, pending := range state.pending {
			if pending.TimedOut || now.Sub(pending.EnqueuedAt) < timeout {
				continue
			}
			pending.TimedOut = true
			state.counter.WriteTimeouts++
			state.timeoutEvents = append(state.timeoutEvents, now)
			state.lastTimeout = now
			if !state.degraded && len(state.timeoutEvents) >= s.cfg.Degrade.TimeoutBatchesThreshold {
				threshold := s.cfg.Degrade.TimeoutBatchesThreshold
				state.startDegradeLocked(now, threshold, threshold)
			} else if state.degraded && state.degrade != nil {
				state.degrade.RecoveryTimeouts++
			}
		}
		cutoff := now.Add(-window)
		trimmed := state.timeoutEvents[:0]
		for _, eventAt := range state.timeoutEvents {
			if eventAt.After(cutoff) {
				trimmed = append(trimmed, eventAt)
			}
		}
		state.timeoutEvents = trimmed
		if state.degraded {
			state.tryRecoverLocked(now, s.cfg.Degrade.RecoveryQuietMS, s.cfg.Degrade.RecoverySuccessSamples)
		}
		s.updateConsumeRatesLocked(state, now)
	}
}

func (s *Service) updateConsumeRatesLocked(state *tenantState, now time.Time) {
	cutoff := now.Add(-5 * time.Second)
	trimmed := state.consumeSamples[:0]
	var records, bytesValue int64
	for _, sample := range state.consumeSamples {
		if sample.At.After(cutoff) {
			trimmed = append(trimmed, sample)
			records += sample.Count
			bytesValue += sample.Bytes
		}
	}
	state.consumeSamples = trimmed
	windowSeconds := 5.0
	state.consumeRecordsPS = float64(records) / windowSeconds
	state.consumeBytesPS = float64(bytesValue) / windowSeconds
}

func (t *tenantState) startDegradeLocked(now time.Time, samples, threshold int) {
	t.degraded = true
	t.degradeEpoch++
	t.recoverySamples = 0
	record := DegradeRecord{
		TriggerMetric: "log_quota_write_timeouts_total",
		TriggerValue:  int64(samples),
		Threshold:     int64(threshold),
		WindowSamples: samples,
		StartedAt:     now.Format(time.RFC3339Nano),
	}
	t.degrade = &record
}

func (t *tenantState) tryRecoverLocked(now time.Time, quietMS, requiredSamples int) {
	if !t.degraded || t.degrade == nil {
		return
	}
	quiet := time.Duration(quietMS) * time.Millisecond
	if t.recoverySamples < requiredSamples {
		return
	}
	if !t.lastTimeout.IsZero() && now.Sub(t.lastTimeout) < quiet {
		return
	}
	if int64(len(t.pending)) > t.cfg.MaxPendingBatches {
		return
	}
	t.degrade.RecoveredAt = now.Format(time.RFC3339Nano)
	t.degrade.RecoveryAckSamples = t.recoverySamples
	t.history = append(t.history, *t.degrade)
	if len(t.history) > 10 {
		t.history = t.history[len(t.history)-10:]
	}
	t.degraded = false
	t.degradeEpoch++
	t.timeoutEvents = nil
}

func (s *Service) SetDegradeRecoveryConfigForTest(quiet time.Duration, samples int) {
	s.cfg.Degrade.RecoveryQuietMS = int(quiet.Milliseconds())
	s.cfg.Degrade.RecoverySuccessSamples = samples
	s.mu.Lock()
	defer s.mu.Unlock()
}

func (s *Service) sortedTenantIDs() []string {
	ids := make([]string, 0, len(s.tenants))
	for id := range s.tenants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
