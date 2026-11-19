package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"settlement/internal/api"
	"settlement/internal/bizcodes"
)

func (s *Service) SubmitResult(ctx context.Context, req *api.SubmitResultRequest) (*api.SubmitResultResponse, error) {
	if req == nil {
		return submitError("", "", bizcodes.InvalidArgument, "request is required"), nil
	}
	batchID := strings.TrimSpace(req.BatchId)
	holderID := strings.TrimSpace(req.HolderId)
	if !validBatchID(batchID) {
		return submitError(batchID, holderID, bizcodes.InvalidArgument, "invalid batch_id"), nil
	}
	if holderID == "" {
		return submitError(batchID, holderID, bizcodes.InvalidArgument, "holder_id is required"), nil
	}
	if req.TaskCount <= 0 {
		return submitError(batchID, holderID, bizcodes.InvalidArgument, "task_count must be positive"), nil
	}
	if strings.TrimSpace(req.ResultDigest) == "" {
		return submitError(batchID, holderID, bizcodes.InvalidArgument, "result_digest is required"), nil
	}
	if strings.TrimSpace(req.ResultJson) == "" {
		return submitError(batchID, holderID, bizcodes.InvalidArgument, "result_json is required"), nil
	}
	s.metrics.ObserveSubmit(batchID)

	lock, revision, err := s.getLock(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read lock: %v", err)
	}
	if lock == nil {
		return submitError(batchID, holderID, bizcodes.LockLost, "lock is not held"), nil
	}
	if lock.HolderID != holderID {
		return submitError(batchID, holderID, bizcodes.LockLost,
			fmt.Sprintf("lock is held by %s", lock.HolderID)), nil
	}
	if lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		_, _ = s.expireLock(ctx, *lock, revision, releaseReasonLeaseExpired)
		return submitError(batchID, holderID, bizcodes.LockLost, "lease already expired"), nil
	}
	existing, err := s.getCommitted(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read committed result: %v", err)
	}
	if existing != nil {
		return submitFromRecord(*existing, true), nil
	}

	record := committedRecord{
		BatchID:         batchID,
		HolderID:        holderID,
		TaskCount:       req.TaskCount,
		TotalAmount:     req.TotalAmount,
		ResultDigest:    req.ResultDigest,
		ResultJSON:      req.ResultJson,
		CommittedAtUnix: time.Now().UnixMilli(),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode result: %v", err)
	}
	resultKey := s.resultKey(batchID)
	aliasKey := s.key("idempotency", batchID, holderID)
	txn, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(s.lockKey(batchID)), "=", revision),
			clientv3.Compare(clientv3.CreateRevision(resultKey), "=", 0),
		).
		Then(
			clientv3.OpPut(resultKey, string(encoded)),
			clientv3.OpPut(aliasKey, string(encoded)),
		).
		Commit()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "commit result: %v", err)
	}
	if txn.Succeeded {
		s.logger.Info("settlement result committed",
			"batch_id", batchID, "holder_id", holderID, "task_count", req.TaskCount,
			"total_amount", req.TotalAmount, "result_digest", req.ResultDigest)
		return submitFromRecord(record, false), nil
	}

	currentLock, _, lockErr := s.getLock(ctx, batchID)
	if lockErr != nil {
		return nil, status.Errorf(codes.Unavailable, "recheck lock: %v", lockErr)
	}
	if currentLock == nil || currentLock.HolderID != holderID || currentLock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
		return submitError(batchID, holderID, bizcodes.LockLost, "lock ownership changed before commit"), nil
	}
	existing, err = s.getCommitted(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "recheck result: %v", err)
	}
	if existing != nil {
		return submitFromRecord(*existing, true), nil
	}
	return submitError(batchID, holderID, bizcodes.Internal, "commit transaction did not succeed"), nil
}

func (s *Service) getCommitted(ctx context.Context, batchID string) (*committedRecord, error) {
	resp, err := s.client.Get(ctx, s.resultKey(batchID))
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, nil
	}
	var record committedRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func submitFromRecord(record committedRecord, idempotent bool) *api.SubmitResultResponse {
	return &api.SubmitResultResponse{
		Accepted:          true,
		Idempotent:        idempotent,
		BatchId:           record.BatchID,
		CommittedBy:       record.HolderID,
		TaskCount:         record.TaskCount,
		TotalAmount:       record.TotalAmount,
		ResultDigest:      record.ResultDigest,
		ResultJson:        record.ResultJSON,
		CommittedAtUnixMs: record.CommittedAtUnix,
	}
}

func submitError(batchID, holderID, code, message string) *api.SubmitResultResponse {
	return &api.SubmitResultResponse{
		BatchId:     batchID,
		CommittedBy: holderID,
		ErrorCode:   code,
		Message:     message,
	}
}

func (s *Service) GetStatus(ctx context.Context, req *api.GetStatusRequest) (*api.GetStatusResponse, error) {
	if req == nil || !validBatchID(strings.TrimSpace(req.BatchId)) {
		return &api.GetStatusResponse{
			ErrorCode: bizcodes.InvalidArgument,
			Message:   "invalid batch_id",
		}, nil
	}
	batchID := strings.TrimSpace(req.BatchId)
	batchStatus, err := s.buildBatchStatus(ctx, batchID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "build status: %v", err)
	}
	return &api.GetStatusResponse{Status: batchStatus}, nil
}

func (s *Service) ListStatus(ctx context.Context, _ *api.ListStatusRequest) (*api.ListStatusResponse, error) {
	statuses, err := s.listStatuses(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list status: %v", err)
	}
	return &api.ListStatusResponse{Statuses: statuses}, nil
}

func (s *Service) listStatuses(ctx context.Context) ([]*api.BatchStatus, error) {
	discovered, err := s.discoverBatchIDs(ctx)
	if err != nil {
		return nil, err
	}
	batches := sortedUnion(s.expectedBatches(), discovered)
	statuses := make([]*api.BatchStatus, 0, len(batches))
	for _, batchID := range batches {
		item, buildErr := s.buildBatchStatus(ctx, batchID)
		if buildErr != nil {
			return nil, buildErr
		}
		statuses = append(statuses, item)
	}
	return statuses, nil
}

func (s *Service) buildBatchStatus(ctx context.Context, batchID string) (*api.BatchStatus, error) {
	lock, revision, err := s.getLock(ctx, batchID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if lock != nil && lock.ExpiresAtUnixMs <= now.UnixMilli() {
		_, _ = s.expireLock(ctx, *lock, revision, releaseReasonLeaseExpired)
		lock = nil
	}
	result := &api.BatchStatus{BatchId: batchID}
	if lock != nil {
		result.Held = true
		result.HolderId = lock.HolderID
		result.LeaseId = lock.LeaseID
		result.ExpiresAtUnixMs = lock.ExpiresAtUnixMs
		result.LeaseRemainingSeconds = remainingSeconds(lock.ExpiresAtUnixMs, now)
	}
	committed, err := s.getCommitted(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if committed != nil {
		result.Committed = true
		result.CommittedBy = committed.HolderID
		result.TaskCount = committed.TaskCount
		result.TotalAmount = committed.TotalAmount
		result.ResultDigest = committed.ResultDigest
		result.CommittedAtUnixMs = committed.CommittedAtUnix
		result.ResultJson = committed.ResultJSON
	}
	events, err := s.listReleaseEvents(ctx, batchID)
	if err != nil {
		return nil, err
	}
	result.ReleaseEvents = make([]*api.ReleaseEvent, 0, len(events))
	for _, event := range events {
		result.ReleaseEvents = append(result.ReleaseEvents, protoReleaseEvent(event))
	}
	return result, nil
}

func (s *Service) discoverBatchIDs(ctx context.Context) ([]string, error) {
	var ids []string
	for _, prefix := range []string{"locks", "results", "current", "release-events"} {
		resp, err := s.client.Get(ctx, s.key(prefix)+"/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
		if err != nil {
			return nil, err
		}
		for _, kv := range resp.Kvs {
			remainder := strings.TrimPrefix(string(kv.Key), s.key(prefix)+"/")
			if index := strings.IndexByte(remainder, '/'); index >= 0 {
				remainder = remainder[:index]
			}
			if validBatchID(remainder) {
				ids = append(ids, remainder)
			}
		}
	}
	return ids, nil
}
