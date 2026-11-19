package worker

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"settlement/internal/api"
	"settlement/internal/bizcodes"
	"settlement/internal/config"
	"settlement/internal/storage"
	"settlement/internal/tasks"
)

type Worker struct {
	cfg     config.Worker
	logger  *slog.Logger
	client  api.SettlementLockClient
	conn    *grpc.ClientConn
	store   *storage.Store
	batches []tasks.Batch
	results map[string]tasks.Result
	order   []string
}

func New(ctx context.Context, cfg config.Worker, logger *slog.Logger) (*Worker, error) {
	if logger == nil {
		logger = slog.Default()
	}
	batches, err := loadBatches(cfg)
	if err != nil {
		return nil, err
	}
	if len(batches) != cfg.BatchCount {
		return nil, fmt.Errorf("loaded %d batches, expected %d", len(batches), cfg.BatchCount)
	}
	results := make(map[string]tasks.Result, len(batches))
	for _, batch := range batches {
		if len(batch.Tasks) != cfg.TasksPerBatch {
			return nil, fmt.Errorf("batch %s has %d tasks, expected %d", batch.ID, len(batch.Tasks), cfg.TasksPerBatch)
		}
		result, buildErr := tasks.BuildResult(batch)
		if buildErr != nil {
			return nil, buildErr
		}
		results[batch.ID] = result
	}
	conn, err := grpc.NewClient(cfg.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect coordinator: %w", err)
	}
	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	order := batchOrder(batches, cfg.WorkerID)
	return &Worker{
		cfg:     cfg,
		logger:  logger,
		client:  api.NewSettlementLockClient(conn),
		conn:    conn,
		store:   store,
		batches: batches,
		results: results,
		order:   order,
	}, nil
}

func (w *Worker) Close() error {
	var errs []error
	if w.store != nil {
		errs = append(errs, w.store.Close())
	}
	if w.conn != nil {
		errs = append(errs, w.conn.Close())
	}
	return errors.Join(errs...)
}

func loadBatches(cfg config.Worker) ([]tasks.Batch, error) {
	if cfg.TasksFile != "" {
		if info, err := os.Stat(cfg.TasksFile); err == nil && info.Size() > 0 {
			return tasks.ReadJSONL(cfg.TasksFile)
		}
	}
	return tasks.Generate(cfg.Seed, cfg.BatchCount, cfg.TasksPerBatch), nil
}

func batchOrder(batches []tasks.Batch, workerID string) []string {
	ids := make([]string, len(batches))
	for i, batch := range batches {
		ids[i] = batch.ID
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(workerID))
	rng := rand.New(rand.NewSource(int64(h.Sum64())))
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	return ids
}

func (w *Worker) Run(ctx context.Context) error {
	runCtx := ctx
	var cancel context.CancelFunc
	if w.cfg.MaxRuntimeMS > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(w.cfg.MaxRuntimeMS)*time.Millisecond)
		defer cancel()
	}
	w.logger.Info("worker started",
		"worker_id", w.cfg.WorkerID, "batches", len(w.order),
		"lease_ttl_ms", w.cfg.LeaseTTLMs, "database", w.cfg.DatabasePath)
	for {
		if err := runCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("worker runtime exceeded: %w", err)
		}
		status, err := w.listStatus(runCtx)
		if err != nil {
			w.logger.Warn("status query failed; retrying", "worker_id", w.cfg.WorkerID, "error", err)
			sleepContext(runCtx, w.retryDelay())
			continue
		}
		committed := make(map[string]bool)
		for _, item := range status.GetStatuses() {
			if item.GetCommitted() {
				committed[item.GetBatchId()] = true
			}
		}
		allCommitted := true
		for _, batchID := range w.order {
			if !committed[batchID] {
				allCommitted = false
				break
			}
		}
		allPersisted := w.syncCommittedResults(runCtx, status.GetStatuses())
		if allCommitted {
			if allPersisted {
				w.logger.Info("all batches committed and persisted", "worker_id", w.cfg.WorkerID)
				return nil
			}
			w.logger.Warn("all batches committed but SQLite repair is incomplete; retrying",
				"worker_id", w.cfg.WorkerID)
			if !sleepContext(runCtx, w.retryDelay()) {
				continue
			}
			continue
		}

		progressed := false
		for _, batchID := range w.order {
			if committed[batchID] {
				continue
			}
			if runCtx.Err() != nil {
				break
			}
			if w.batchAlreadyCommitted(runCtx, batchID) {
				committed[batchID] = true
				continue
			}
			outcome := w.processBatch(runCtx, batchID)
			if outcome == batchCommitted || outcome == batchAcquired {
				progressed = true
			}
		}
		if !progressed && !sleepContext(runCtx, w.retryDelay()) {
			continue
		}
	}
}

type batchOutcome int

const (
	batchSkipped batchOutcome = iota
	batchAcquired
	batchCommitted
	batchLost
)

func (w *Worker) processBatch(ctx context.Context, batchID string) batchOutcome {
	started := time.Now()
	acquireCtx, acquireCancel := context.WithTimeout(ctx, time.Duration(w.cfg.WaitTimeoutMs+3000)*time.Millisecond)
	defer acquireCancel()
	acquired, err := w.client.AcquireLock(acquireCtx, &api.AcquireLockRequest{
		BatchId:       batchID,
		HolderId:      w.cfg.WorkerID,
		LeaseTtlMs:    w.cfg.LeaseTTLMs,
		WaitTimeoutMs: w.cfg.WaitTimeoutMs,
		RequestId:     fmt.Sprintf("%s-%s-%d", w.cfg.WorkerID, batchID, started.UnixNano()),
	})
	if err != nil {
		w.logger.Warn("lock acquire RPC failed", "worker_id", w.cfg.WorkerID, "batch_id", batchID, "error", err)
		return batchSkipped
	}
	if !acquired.GetAcquired() {
		switch acquired.GetErrorCode() {
		case bizcodes.LockHeld, bizcodes.WaitTimeout:
			w.logger.Info("batch busy; moving on",
				"worker_id", w.cfg.WorkerID, "batch_id", batchID,
				"error_code", acquired.GetErrorCode(), "current_holder", acquired.GetCurrentHolderId(),
				"wait_ms", acquired.GetWaitMs())
			return batchSkipped
		default:
			w.logger.Warn("lock acquire rejected",
				"worker_id", w.cfg.WorkerID, "batch_id", batchID,
				"error_code", acquired.GetErrorCode(), "message", acquired.GetMessage())
			return batchSkipped
		}
	}
	w.logger.Info("batch lock acquired",
		"worker_id", w.cfg.WorkerID, "batch_id", batchID,
		"lease_id", acquired.GetLeaseId(), "expires_at_unix_ms", acquired.GetExpiresAtUnixMs(),
		"renewed", acquired.GetRenewed(), "wait_ms", acquired.GetWaitMs())

	processCtx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	var expiresAt atomic.Int64
	expiresAt.Store(acquired.GetExpiresAtUnixMs())
	lost := make(chan string, 1)
	renewDone := make(chan struct{})
	go w.renewLoop(processCtx, batchID, &expiresAt, lost, renewDone)
	defer func() {
		cancelProcess()
		<-renewDone
	}()

	release := func() {
		cancelProcess()
		<-renewDone
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer releaseCancel()
		resp, err := w.client.ReleaseLock(releaseCtx, &api.ReleaseLockRequest{
			BatchId: batchID, HolderId: w.cfg.WorkerID,
		})
		if err != nil {
			w.logger.Warn("lock release RPC failed; lease will expire",
				"worker_id", w.cfg.WorkerID, "batch_id", batchID, "error", err)
		} else if !resp.GetReleased() {
			w.logger.Warn("lock release rejected",
				"worker_id", w.cfg.WorkerID, "batch_id", batchID,
				"error_code", resp.GetErrorCode(), "message", resp.GetMessage())
		}
	}
	defer release()

	result := w.results[batchID]
	taskCount := len(w.batchesFor(batchID).Tasks)
	alreadyCommitted := w.batchAlreadyCommitted(processCtx, batchID)
	if alreadyCommitted {
		w.logger.Info("batch was committed while waiting; skipping deterministic replay work",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID)
	} else {
		perTask := time.Duration(0)
		if w.cfg.BatchWorkMS > 0 && taskCount > 0 {
			perTask = time.Duration(w.cfg.BatchWorkMS) * time.Millisecond / time.Duration(taskCount)
		}
		for index := 0; index < taskCount; index++ {
			if err := processCtx.Err(); err != nil {
				w.logger.Warn("processing abandoned before submission",
					"worker_id", w.cfg.WorkerID, "batch_id", batchID, "tasks_completed", index)
				return batchLost
			}
			select {
			case reason := <-lost:
				w.logger.Warn("renewal failed; abandoning unsubmitted batch",
					"worker_id", w.cfg.WorkerID, "batch_id", batchID,
					"tasks_completed", index, "reason", reason)
				cancelProcess()
				return batchLost
			default:
			}
			if perTask > 0 {
				if !sleepContext(processCtx, perTask) {
					return batchLost
				}
			}
		}
	}
	if time.Now().UnixMilli() >= expiresAt.Load() {
		w.logger.Warn("lease expired before result submission; result discarded",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID)
		return batchLost
	}
	select {
	case reason := <-lost:
		w.logger.Warn("lease lost before result submission; result discarded",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID, "reason", reason)
		return batchLost
	default:
	}

	submit, err := w.submitWithRetry(processCtx, &api.SubmitResultRequest{
		BatchId:      batchID,
		HolderId:     w.cfg.WorkerID,
		TaskCount:    int32(result.TaskCount),
		TotalAmount:  result.TotalAmount,
		ResultDigest: result.Digest,
		ResultJson:   result.PayloadJSON,
	}, &expiresAt)
	if err != nil {
		w.logger.Warn("result submission failed; no local rows written",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID, "error", err)
		return batchLost
	}
	if !submit.GetAccepted() {
		w.logger.Warn("result submission rejected; no local rows written",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID,
			"error_code", submit.GetErrorCode(), "message", submit.GetMessage())
		return batchLost
	}
	if submit.GetCommittedBy() != w.cfg.WorkerID {
		w.logger.Info("batch already committed; persisting canonical result",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID,
			"committed_by", submit.GetCommittedBy(), "idempotent", submit.GetIdempotent())
	}

	persistCtx, persistCancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = w.store.Apply(persistCtx, storage.CommittedResult{
		BatchID:         submit.GetBatchId(),
		HolderID:        submit.GetCommittedBy(),
		TaskCount:       submit.GetTaskCount(),
		TotalAmount:     submit.GetTotalAmount(),
		ResultDigest:    submit.GetResultDigest(),
		ResultJSON:      submit.GetResultJson(),
		CommittedAtUnix: submit.GetCommittedAtUnixMs(),
	})
	persistCancel()
	if err != nil {
		w.logger.Error("coordinator committed result but SQLite persist failed; another holder will repair",
			"worker_id", w.cfg.WorkerID, "batch_id", batchID, "error", err)
		return batchLost
	}
	w.logger.Info("batch settlement persisted",
		"worker_id", w.cfg.WorkerID, "batch_id", batchID,
		"committed_by", submit.GetCommittedBy(), "task_count", submit.GetTaskCount(),
		"total_amount", submit.GetTotalAmount(), "digest", submit.GetResultDigest(),
		"elapsed_ms", time.Since(started).Milliseconds())
	return batchCommitted
}

func (w *Worker) renewLoop(
	ctx context.Context,
	batchID string,
	expiresAt *atomic.Int64,
	lost chan<- string,
	done chan<- struct{},
) {
	defer close(done)
	interval := time.Duration(w.cfg.RenewIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, minDuration(2*time.Second, time.Duration(w.cfg.LeaseTTLMs/2)*time.Millisecond))
			resp, err := w.client.RenewLock(callCtx, &api.RenewLockRequest{
				BatchId: batchID, HolderId: w.cfg.WorkerID, LeaseTtlMs: w.cfg.LeaseTTLMs,
			})
			cancel()
			if err != nil {
				select {
				case lost <- "renew RPC error: " + err.Error():
				default:
				}
				return
			}
			if !resp.GetRenewed() {
				select {
				case lost <- resp.GetErrorCode() + ": " + resp.GetMessage():
				default:
				}
				return
			}
			expiresAt.Store(resp.GetExpiresAtUnixMs())
			w.logger.Debug("batch lock renewed",
				"worker_id", w.cfg.WorkerID, "batch_id", batchID,
				"expires_at_unix_ms", resp.GetExpiresAtUnixMs())
		}
	}
}

func (w *Worker) submitWithRetry(
	ctx context.Context,
	req *api.SubmitResultRequest,
	expiresAt *atomic.Int64,
) (*api.SubmitResultResponse, error) {
	deadline := time.Now().Add(minDuration(5*time.Second, time.Duration(w.cfg.LeaseTTLMs)*time.Millisecond/2))
	var lastErr error
	for {
		if time.Now().UnixMilli() >= expiresAt.Load() {
			return nil, errors.New("lease expired while submitting result")
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := w.client.SubmitResult(callCtx, req)
		cancel()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("submit result: %w", lastErr)
		}
		if !sleepContext(ctx, 150*time.Millisecond) {
			return nil, fmt.Errorf("submit result: %w", lastErr)
		}
	}
}

func (w *Worker) listStatus(ctx context.Context) (*api.ListStatusResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return w.client.ListStatus(callCtx, &api.ListStatusRequest{})
}

func (w *Worker) batchAlreadyCommitted(ctx context.Context, batchID string) bool {
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := w.client.GetStatus(callCtx, &api.GetStatusRequest{BatchId: batchID})
	return err == nil && resp.GetStatus().GetCommitted()
}

func (w *Worker) syncCommittedResults(ctx context.Context, statuses []*api.BatchStatus) bool {
	allPersisted := true
	for _, item := range statuses {
		if !item.GetCommitted() {
			continue
		}
		hasBatch, err := w.store.HasBatch(ctx, item.GetBatchId())
		if err != nil {
			w.logger.Warn("SQLite completeness check failed",
				"worker_id", w.cfg.WorkerID, "batch_id", item.GetBatchId(), "error", err)
			allPersisted = false
			continue
		}
		if hasBatch {
			continue
		}
		if item.GetResultJson() == "" {
			w.logger.Warn("committed batch has no canonical result payload for SQLite repair",
				"worker_id", w.cfg.WorkerID, "batch_id", item.GetBatchId())
			allPersisted = false
			continue
		}
		err = w.store.Apply(ctx, storage.CommittedResult{
			BatchID:         item.GetBatchId(),
			HolderID:        item.GetCommittedBy(),
			TaskCount:       item.GetTaskCount(),
			TotalAmount:     item.GetTotalAmount(),
			ResultDigest:    item.GetResultDigest(),
			ResultJSON:      item.GetResultJson(),
			CommittedAtUnix: item.GetCommittedAtUnixMs(),
		})
		if err != nil {
			w.logger.Warn("SQLite repair failed",
				"worker_id", w.cfg.WorkerID, "batch_id", item.GetBatchId(), "error", err)
			allPersisted = false
			continue
		}
		w.logger.Info("repaired missing SQLite settlement from canonical result",
			"worker_id", w.cfg.WorkerID, "batch_id", item.GetBatchId(),
			"committed_by", item.GetCommittedBy())
	}
	return allPersisted
}

func (w *Worker) batchesFor(batchID string) tasks.Batch {
	for _, batch := range w.batches {
		if batch.ID == batchID {
			return batch
		}
	}
	return tasks.Batch{ID: batchID}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *Worker) retryDelay() time.Duration {
	if w.cfg.RetryDelayMS <= 0 {
		return 250 * time.Millisecond
	}
	return time.Duration(w.cfg.RetryDelayMS) * time.Millisecond
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func WorkerIDFromConfig(path string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(path, ".json"), ".JSON")
	index := strings.LastIndexAny(base, `/\`)
	if index >= 0 {
		base = base[index+1:]
	}
	return base
}
