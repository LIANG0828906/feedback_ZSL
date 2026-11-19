package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"settlement/internal/api"
	"settlement/internal/bizcodes"
)

func (s *Service) AcquireLock(ctx context.Context, req *api.AcquireLockRequest) (*api.AcquireLockResponse, error) {
	started := time.Now()
	if req == nil {
		return acquireError("", "", "", bizcodes.InvalidArgument, "request is required"), nil
	}
	batchID := strings.TrimSpace(req.BatchId)
	holderID := strings.TrimSpace(req.HolderId)
	requestID := strings.TrimSpace(req.RequestId)
	if requestID == "" {
		requestID = randomID("req")
	}
	if !validBatchID(batchID) {
		return acquireError(batchID, holderID, requestID, bizcodes.InvalidArgument, "invalid batch_id"), nil
	}
	if holderID == "" || len(holderID) > 128 {
		return acquireError(batchID, holderID, requestID, bizcodes.InvalidArgument, "holder_id is required and must be <=128 bytes"), nil
	}
	ttlMs, code, message := s.resolveAcquireTTL(req.LeaseTtlMs)
	if code != "" {
		s.metrics.ObserveAcquire(batchID)
		s.logger.Info("lock acquire rejected",
			"request_id", requestID, "batch_id", batchID, "holder_id", holderID,
			"error_code", code, "wait_ms", time.Since(started).Milliseconds())
		return acquireError(batchID, holderID, requestID, code, message), nil
	}
	waitMs := req.WaitTimeoutMs
	if waitMs < 0 {
		waitMs = s.cfg.DefaultWaitTimeoutMs
	}
	s.metrics.ObserveAcquire(batchID)

	// Re-applying from the current holder is a renewal, not a queue entry.
	// Handle it before FIFO registration so a live owner can still refresh
	// while other processes are waiting.
	if lock, revision, lockErr := s.getLock(ctx, batchID); lockErr != nil {
		return nil, status.Errorf(codes.Unavailable, "read existing lock: %v", lockErr)
	} else if lock != nil && lock.HolderID == holderID && lock.ExpiresAtUnixMs > time.Now().UnixMilli() {
		resp, renewErr := s.renewExisting(ctx, *lock, revision, ttlMs)
		if renewErr != nil {
			return nil, status.Errorf(codes.Unavailable, "renew existing lock: %v", renewErr)
		}
		if resp.GetAcquired() && resp.GetRenewed() {
			resp.RequestId = requestID
			resp.WaitMs = time.Since(started).Milliseconds()
			s.logAcquireOutcome(resp)
			return resp, nil
		}
	}

	q := s.queues.get(batchID)
	ticket := q.enqueue(holderID)
	defer q.remove(ticket)
	deadline := started.Add(time.Duration(waitMs) * time.Millisecond)

	var lastHeld *api.AcquireLockResponse
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if q.isHead(ticket) {
			resp, err := s.tryAcquire(ctx, batchID, holderID, ttlMs)
			if err != nil {
				return nil, status.Errorf(codes.Unavailable, "acquire lock: %v", err)
			}
			resp.RequestId = requestID
			resp.WaitMs = time.Since(started).Milliseconds()
			if resp.ErrorCode != bizcodes.LockHeld {
				s.logAcquireOutcome(resp)
				return resp, nil
			}
			lastHeld = resp
			if waitMs == 0 {
				s.logAcquireOutcome(resp)
				return resp, nil
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return s.waitTimeoutResponse(ctx, batchID, holderID, requestID, started, lastHeld), nil
			}
			sleepFor := time.Duration(s.cfg.QueuePollIntervalMs) * time.Millisecond
			if remaining < sleepFor {
				sleepFor = remaining
			}
			s.waitOnQueue(ctx, ticket, sleepFor)
			continue
		}

		if waitMs == 0 {
			resp := s.heldResponseFromState(ctx, batchID)
			resp.HolderId = holderID
			resp.RequestId = requestID
			resp.WaitMs = time.Since(started).Milliseconds()
			s.logAcquireOutcome(resp)
			return resp, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return s.waitTimeoutResponse(ctx, batchID, holderID, requestID, started, lastHeld), nil
		}
		sleepFor := time.Duration(s.cfg.QueuePollIntervalMs) * time.Millisecond
		if remaining < sleepFor {
			sleepFor = remaining
		}
		s.waitOnQueue(ctx, ticket, sleepFor)
	}
}

func (s *Service) waitOnQueue(ctx context.Context, ticket *queueTicket, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-ticket.wake:
	case <-timer.C:
	}
}

func (s *Service) waitTimeoutResponse(
	ctx context.Context,
	batchID, holderID, requestID string,
	started time.Time,
	held *api.AcquireLockResponse,
) *api.AcquireLockResponse {
	s.metrics.ObserveWaitTimeout(batchID)
	if held == nil {
		held = s.heldResponseFromState(ctx, batchID)
	}
	held.Acquired = false
	held.HolderId = holderID
	held.RequestId = requestID
	held.WaitMs = time.Since(started).Milliseconds()
	held.ErrorCode = bizcodes.WaitTimeout
	held.Message = "wait timeout before lock could be acquired"
	s.logAcquireOutcome(held)
	return held
}

func (s *Service) logAcquireOutcome(resp *api.AcquireLockResponse) {
	s.logger.Info("lock acquire result",
		"request_id", resp.GetRequestId(),
		"batch_id", resp.GetBatchId(),
		"holder_id", resp.GetHolderId(),
		"acquired", resp.GetAcquired(),
		"renewed", resp.GetRenewed(),
		"error_code", resp.GetErrorCode(),
		"current_holder_id", resp.GetCurrentHolderId(),
		"wait_ms", resp.GetWaitMs(),
	)
}

func (s *Service) resolveAcquireTTL(requestedMs int64) (int64, string, string) {
	ttlMs := requestedMs
	if ttlMs == 0 {
		ttlMs = s.cfg.DefaultLeaseTTLMs
	}
	if ttlMs < s.cfg.MinLeaseTTLMs || ttlMs < 3000 {
		return 0, bizcodes.LeaseTooShort,
			fmt.Sprintf("lease_ttl_ms must be >= %d", maxInt64(s.cfg.MinLeaseTTLMs, 3000))
	}
	return ttlMs, "", ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Service) tryAcquire(ctx context.Context, batchID, holderID string, ttlMs int64) (*api.AcquireLockResponse, error) {
	for attempt := 0; attempt < 2; attempt++ {
		lock, revision, err := s.getLock(ctx, batchID)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if lock != nil && lock.ExpiresAtUnixMs > now.UnixMilli() {
			if lock.HolderID == holderID {
				return s.renewExisting(ctx, *lock, revision, ttlMs)
			}
			return heldResponse(*lock, holderID), nil
		}
		if lock != nil {
			removed, err := s.expireLock(ctx, *lock, revision, releaseReasonLeaseExpired)
			if err != nil {
				return nil, err
			}
			if !removed {
				continue
			}
		}
		return s.acquireNew(ctx, batchID, holderID, ttlMs)
	}
	return s.heldResponseFromState(ctx, batchID), nil
}

func (s *Service) acquireNew(ctx context.Context, batchID, holderID string, ttlMs int64) (*api.AcquireLockResponse, error) {
	lease, err := s.grantLease(ctx, ttlMs)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	lock := lockRecord{
		BatchID:          batchID,
		HolderID:         holderID,
		Generation:       randomID("gen"),
		LeaseID:          strconv.FormatInt(int64(lease.ID), 10),
		LeaseTTLMs:       ttlMs,
		ExpiresAtUnixMs:  now.Add(time.Duration(ttlMs) * time.Millisecond).UnixMilli(),
		AcquiredAtUnixMs: now.UnixMilli(),
	}
	value := marshalLock(lock)
	lockKey := s.lockKey(batchID)
	txn, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(lockKey), "=", 0)).
		Then(
			clientv3.OpPut(lockKey, value, clientv3.WithLease(lease.ID)),
			clientv3.OpPut(s.currentKey(batchID), value),
		).
		Else(clientv3.OpGet(lockKey)).
		Commit()
	if err != nil {
		s.revokeLease(lease.ID)
		return nil, err
	}
	if !txn.Succeeded {
		s.revokeLease(lease.ID)
		if len(txn.Responses) > 0 {
			if get := txn.Responses[0].GetResponseRange(); get != nil && len(get.Kvs) > 0 {
				if existing, decodeErr := decodeLock(get.Kvs[0].Value); decodeErr == nil {
					if existing.HolderID == holderID && existing.ExpiresAtUnixMs > time.Now().UnixMilli() {
						return s.renewExisting(ctx, existing, get.Kvs[0].ModRevision, ttlMs)
					}
					return heldResponse(existing, holderID), nil
				}
			}
		}
		return s.heldResponseFromState(ctx, batchID), nil
	}
	return &api.AcquireLockResponse{
		Acquired:               true,
		BatchId:                batchID,
		HolderId:               holderID,
		LeaseId:                lock.LeaseID,
		ExpiresAtUnixMs:        lock.ExpiresAtUnixMs,
		LeaseTtlMs:             ttlMs,
		CurrentHolderId:        holderID,
		CurrentExpiresAtUnixMs: lock.ExpiresAtUnixMs,
	}, nil
}

func (s *Service) RenewLock(ctx context.Context, req *api.RenewLockRequest) (*api.RenewLockResponse, error) {
	if req == nil {
		return renewError("", "", bizcodes.InvalidArgument, "request is required"), nil
	}
	batchID := strings.TrimSpace(req.BatchId)
	holderID := strings.TrimSpace(req.HolderId)
	if !validBatchID(batchID) {
		return renewError(batchID, holderID, bizcodes.InvalidArgument, "invalid batch_id"), nil
	}
	if holderID == "" {
		return renewError(batchID, holderID, bizcodes.InvalidArgument, "holder_id is required"), nil
	}
	s.metrics.ObserveRenew(batchID)
	lock, revision, err := s.getLock(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read lock: %v", err)
	}
	if lock == nil {
		return renewError(batchID, holderID, bizcodes.LockLost, "lock is not held"), nil
	}
	ttlMs := req.LeaseTtlMs
	if ttlMs == 0 {
		ttlMs = lock.LeaseTTLMs
	}
	if ttlMs < s.cfg.MinLeaseTTLMs || ttlMs < 3000 {
		return renewError(batchID, holderID, bizcodes.LeaseTooShort,
			fmt.Sprintf("lease_ttl_ms must be >= %d", maxInt64(s.cfg.MinLeaseTTLMs, 3000))), nil
	}
	resp, err := s.renewExisting(ctx, *lock, revision, ttlMs)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "renew lock: %v", err)
	}
	if resp.ErrorCode != "" {
		return renewError(batchID, holderID, resp.ErrorCode, resp.Message), nil
	}
	return &api.RenewLockResponse{
		Renewed:         true,
		BatchId:         batchID,
		HolderId:        holderID,
		LeaseId:         resp.LeaseId,
		ExpiresAtUnixMs: resp.ExpiresAtUnixMs,
		LeaseTtlMs:      ttlMs,
	}, nil
}

func (s *Service) renewExisting(ctx context.Context, lock lockRecord, revision int64, ttlMs int64) (*api.AcquireLockResponse, error) {
	now := time.Now()
	if lock.ExpiresAtUnixMs <= now.UnixMilli() {
		_, _ = s.expireLock(ctx, lock, revision, releaseReasonLeaseExpired)
		return acquireError(lock.BatchID, lock.HolderID, "", bizcodes.LockLost, "lease already expired"), nil
	}
	lockKey := s.lockKey(lock.BatchID)
	if ttlMs == lock.LeaseTTLMs {
		leaseID, err := strconv.ParseInt(lock.LeaseID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse lease id: %w", err)
		}
		if _, err := s.client.KeepAliveOnce(ctx, clientv3.LeaseID(leaseID)); err != nil {
			return acquireError(lock.BatchID, lock.HolderID, "", bizcodes.LockLost, "lease is no longer valid"), nil
		}
		lock.ExpiresAtUnixMs = now.Add(time.Duration(ttlMs) * time.Millisecond).UnixMilli()
		value := marshalLock(lock)
		txn, err := s.client.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(lockKey), "=", revision)).
			Then(
				clientv3.OpPut(lockKey, value, clientv3.WithLease(clientv3.LeaseID(leaseID))),
				clientv3.OpPut(s.currentKey(lock.BatchID), value),
			).
			Commit()
		if err != nil {
			return nil, err
		}
		if !txn.Succeeded {
			return s.latestRenewalOrLost(ctx, lock.BatchID, lock.HolderID)
		}
		return renewedResponse(lock, ttlMs), nil
	}

	newLease, err := s.grantLease(ctx, ttlMs)
	if err != nil {
		return nil, err
	}
	oldLeaseID, _ := strconv.ParseInt(lock.LeaseID, 10, 64)
	lock.LeaseID = strconv.FormatInt(int64(newLease.ID), 10)
	lock.LeaseTTLMs = ttlMs
	lock.ExpiresAtUnixMs = now.Add(time.Duration(ttlMs) * time.Millisecond).UnixMilli()
	value := marshalLock(lock)
	txn, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(lockKey), "=", revision)).
		Then(
			clientv3.OpPut(lockKey, value, clientv3.WithLease(newLease.ID)),
			clientv3.OpPut(s.currentKey(lock.BatchID), value),
		).
		Commit()
	if err != nil {
		s.revokeLease(newLease.ID)
		return nil, err
	}
	if !txn.Succeeded {
		s.revokeLease(newLease.ID)
		return s.latestRenewalOrLost(ctx, lock.BatchID, lock.HolderID)
	}
	if oldLeaseID != 0 {
		s.revokeLease(clientv3.LeaseID(oldLeaseID))
	}
	return renewedResponse(lock, ttlMs), nil
}

func (s *Service) latestRenewalOrLost(ctx context.Context, batchID, holderID string) (*api.AcquireLockResponse, error) {
	lock, _, err := s.getLock(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if lock == nil || lock.HolderID != holderID || lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		return acquireError(batchID, holderID, "", bizcodes.LockLost, "lock ownership changed"), nil
	}
	return renewedResponse(*lock, lock.LeaseTTLMs), nil
}

func renewedResponse(lock lockRecord, ttlMs int64) *api.AcquireLockResponse {
	return &api.AcquireLockResponse{
		Acquired:               true,
		Renewed:                true,
		BatchId:                lock.BatchID,
		HolderId:               lock.HolderID,
		LeaseId:                lock.LeaseID,
		ExpiresAtUnixMs:        lock.ExpiresAtUnixMs,
		LeaseTtlMs:             ttlMs,
		CurrentHolderId:        lock.HolderID,
		CurrentExpiresAtUnixMs: lock.ExpiresAtUnixMs,
	}
}

func (s *Service) ReleaseLock(ctx context.Context, req *api.ReleaseLockRequest) (*api.ReleaseLockResponse, error) {
	if req == nil {
		return releaseError("", "", bizcodes.InvalidArgument, "request is required"), nil
	}
	batchID := strings.TrimSpace(req.BatchId)
	holderID := strings.TrimSpace(req.HolderId)
	if !validBatchID(batchID) {
		return releaseError(batchID, holderID, bizcodes.InvalidArgument, "invalid batch_id"), nil
	}
	if holderID == "" {
		return releaseError(batchID, holderID, bizcodes.InvalidArgument, "holder_id is required"), nil
	}
	s.metrics.ObserveReleaseRequest(batchID)
	lock, revision, err := s.getLock(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read lock: %v", err)
	}
	if lock == nil {
		if current, _, currentErr := s.getCurrent(ctx, batchID); currentErr == nil && current != nil && current.HolderID == holderID {
			if exists, _ := s.releaseEventExists(ctx, batchID, current.Generation); exists {
				return &api.ReleaseLockResponse{Released: true, BatchId: batchID, HolderId: holderID}, nil
			}
		}
		return releaseError(batchID, holderID, bizcodes.LockLost, "lock is not held"), nil
	}
	if lock.HolderID != holderID {
		return releaseError(batchID, holderID, bizcodes.LockLost,
			fmt.Sprintf("lock is held by %s", lock.HolderID)), nil
	}
	if lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		_, _ = s.expireLock(ctx, *lock, revision, releaseReasonLeaseExpired)
		return releaseError(batchID, holderID, bizcodes.LockLost, "lease already expired"), nil
	}
	event := releaseEvent{
		BatchID:              lock.BatchID,
		HolderID:             lock.HolderID,
		Generation:           lock.Generation,
		LeaseID:              lock.LeaseID,
		LeaseExpiresAtUnixMs: lock.ExpiresAtUnixMs,
		ReleasedAtUnixMs:     time.Now().UnixMilli(),
		Reason:               releaseReasonReleased,
	}
	txn, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(s.lockKey(batchID)), "=", revision)).
		Then(
			clientv3.OpDelete(s.lockKey(batchID)),
			clientv3.OpPut(s.eventKey(batchID, lock.Generation), marshalRelease(event)),
		).
		Commit()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "release lock: %v", err)
	}
	if !txn.Succeeded {
		return releaseError(batchID, holderID, bizcodes.LockLost, "lock ownership changed"), nil
	}
	if leaseID, err := strconv.ParseInt(lock.LeaseID, 10, 64); err == nil {
		s.revokeLease(clientv3.LeaseID(leaseID))
	}
	s.queues.get(batchID).signalAll()
	s.logger.Info("lock released",
		"batch_id", batchID, "holder_id", holderID, "generation", lock.Generation,
		"lease_id", lock.LeaseID, "reason", releaseReasonReleased)
	return &api.ReleaseLockResponse{Released: true, BatchId: batchID, HolderId: holderID}, nil
}

func (s *Service) getLock(ctx context.Context, batchID string) (*lockRecord, int64, error) {
	resp, err := s.client.Get(ctx, s.lockKey(batchID))
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, nil
	}
	lock, err := decodeLock(resp.Kvs[0].Value)
	if err != nil {
		return nil, 0, err
	}
	return &lock, resp.Kvs[0].ModRevision, nil
}

func (s *Service) getCurrent(ctx context.Context, batchID string) (*lockRecord, int64, error) {
	resp, err := s.client.Get(ctx, s.currentKey(batchID))
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, nil
	}
	lock, err := decodeLock(resp.Kvs[0].Value)
	if err != nil {
		return nil, 0, err
	}
	return &lock, resp.Kvs[0].ModRevision, nil
}

func (s *Service) expireLock(ctx context.Context, lock lockRecord, revision int64, reason string) (bool, error) {
	txn, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(s.lockKey(lock.BatchID)), "=", revision)).
		Then(clientv3.OpDelete(s.lockKey(lock.BatchID))).
		Commit()
	if err != nil {
		return false, err
	}
	if !txn.Succeeded {
		return false, nil
	}
	if leaseID, parseErr := strconv.ParseInt(lock.LeaseID, 10, 64); parseErr == nil {
		s.revokeLease(clientv3.LeaseID(leaseID))
	}
	s.recordReleaseEvent(lock, reason)
	s.queues.get(lock.BatchID).signalAll()
	s.logger.Warn("lock expired",
		"batch_id", lock.BatchID, "holder_id", lock.HolderID, "generation", lock.Generation,
		"lease_id", lock.LeaseID, "lease_expires_at_unix_ms", lock.ExpiresAtUnixMs,
		"reason", reason)
	return true, nil
}

func (s *Service) grantLease(ctx context.Context, ttlMs int64) (*clientv3.LeaseGrantResponse, error) {
	seconds := (ttlMs + 999) / 1000
	if seconds < 3 {
		seconds = 3
	}
	return s.client.Grant(ctx, seconds)
}

func (s *Service) revokeLease(leaseID clientv3.LeaseID) {
	if leaseID == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.client.Revoke(ctx, leaseID)
	}()
}

func (s *Service) heldResponseFromState(ctx context.Context, batchID string) *api.AcquireLockResponse {
	lock, _, err := s.getLock(ctx, batchID)
	if err != nil || lock == nil || lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		return &api.AcquireLockResponse{
			ErrorCode: bizcodes.LockHeld,
			Message:   "batch lock is not available",
			BatchId:   batchID,
		}
	}
	return heldResponse(*lock, "")
}

func heldResponse(lock lockRecord, requestedBy string) *api.AcquireLockResponse {
	return &api.AcquireLockResponse{
		Acquired:               false,
		ErrorCode:              bizcodes.LockHeld,
		Message:                "batch lock is held",
		BatchId:                lock.BatchID,
		HolderId:               requestedBy,
		LeaseId:                "",
		CurrentHolderId:        lock.HolderID,
		CurrentExpiresAtUnixMs: lock.ExpiresAtUnixMs,
	}
}

func acquireError(batchID, holderID, requestID, code, message string) *api.AcquireLockResponse {
	return &api.AcquireLockResponse{
		BatchId:   batchID,
		HolderId:  holderID,
		RequestId: requestID,
		ErrorCode: code,
		Message:   message,
	}
}

func renewError(batchID, holderID, code, message string) *api.RenewLockResponse {
	return &api.RenewLockResponse{
		BatchId:   batchID,
		HolderId:  holderID,
		ErrorCode: code,
		Message:   message,
	}
}

func releaseError(batchID, holderID, code, message string) *api.ReleaseLockResponse {
	return &api.ReleaseLockResponse{
		BatchId:   batchID,
		HolderId:  holderID,
		ErrorCode: code,
		Message:   message,
	}
}

func randomID(prefix string) string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(data[:])
}
