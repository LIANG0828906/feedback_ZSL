package coordinator

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *Service) recordReleaseEvent(lock lockRecord, reason string) {
	if lock.BatchID == "" || lock.Generation == "" {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	event := releaseEvent{
		BatchID:              lock.BatchID,
		HolderID:             lock.HolderID,
		Generation:           lock.Generation,
		LeaseID:              lock.LeaseID,
		LeaseExpiresAtUnixMs: lock.ExpiresAtUnixMs,
		ReleasedAtUnixMs:     time.Now().UnixMilli(),
		Reason:               reason,
	}
	eventKey := s.eventKey(lock.BatchID, lock.Generation)
	txn, err := s.client.Txn(recordCtx).
		If(clientv3.Compare(clientv3.CreateRevision(eventKey), "=", 0)).
		Then(clientv3.OpPut(eventKey, marshalRelease(event))).
		Commit()
	if err != nil {
		s.logger.Error("failed to record release event",
			"batch_id", lock.BatchID, "holder_id", lock.HolderID,
			"generation", lock.Generation, "reason", reason, "error", err)
		return
	}
	if txn.Succeeded {
		s.logger.Info("release event recorded",
			"batch_id", event.BatchID, "holder_id", event.HolderID,
			"generation", event.Generation, "lease_id", event.LeaseID,
			"lease_expires_at_unix_ms", event.LeaseExpiresAtUnixMs,
			"released_at_unix_ms", event.ReleasedAtUnixMs, "reason", event.Reason)
	}
}

func (s *Service) releaseEventExists(ctx context.Context, batchID, generation string) (bool, error) {
	resp, err := s.client.Get(ctx, s.eventKey(batchID, generation), clientv3.WithCountOnly())
	if err != nil {
		return false, err
	}
	return resp.Count > 0, nil
}

func (s *Service) listReleaseEvents(ctx context.Context, batchID string) ([]releaseEvent, error) {
	prefix := s.key("release-events", batchID) + "/"
	resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	events := make([]releaseEvent, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var event releaseEvent
		if err := json.Unmarshal(kv.Value, &event); err != nil {
			s.logger.Warn("ignoring malformed release event", "key", string(kv.Key), "error", err)
			continue
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].ReleasedAtUnixMs == events[j].ReleasedAtUnixMs {
			return events[i].Generation < events[j].Generation
		}
		return events[i].ReleasedAtUnixMs < events[j].ReleasedAtUnixMs
	})
	limit := s.cfg.ReleaseEventListLimit
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func (s *Service) listAllReleaseEvents(ctx context.Context) ([]releaseEvent, error) {
	prefix := s.key("release-events") + "/"
	resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	events := make([]releaseEvent, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var event releaseEvent
		if err := json.Unmarshal(kv.Value, &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func (s *Service) watchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		watchCtx, cancel := context.WithCancel(ctx)
		watch := s.client.Watch(watchCtx, s.key("locks")+"/",
			clientv3.WithPrefix(), clientv3.WithPrevKV())
		reconnect := false
		for response := range watch {
			if response.Err() != nil {
				s.logger.Warn("etcd lock watch interrupted", "error", response.Err())
				reconnect = true
				break
			}
			for _, event := range response.Events {
				if event.Type != clientv3.EventTypeDelete {
					continue
				}
				batchID := batchFromKey(string(event.Kv.Key))
				if batchID != "" {
					s.queues.get(batchID).signalAll()
				}
				if len(event.PrevKv.Value) == 0 {
					continue
				}
				lock, err := decodeLock(event.PrevKv.Value)
				if err != nil {
					s.logger.Warn("cannot decode deleted lock", "key", string(event.Kv.Key), "error", err)
					continue
				}
				if exists, err := s.releaseEventExists(ctx, lock.BatchID, lock.Generation); err == nil && exists {
					continue
				}
				reason := releaseReasonLockMissing
				if lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
					reason = releaseReasonLeaseExpired
				}
				s.recordReleaseEvent(lock, reason)
			}
		}
		cancel()
		if !reconnect {
			return
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) sweepLoop(ctx context.Context) {
	interval := time.Duration(s.cfg.SweepIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 150 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context) {
	now := time.Now().UnixMilli()
	resp, err := s.client.Get(ctx, s.key("locks")+"/", clientv3.WithPrefix())
	if err != nil {
		return
	}
	for _, kv := range resp.Kvs {
		lock, decodeErr := decodeLock(kv.Value)
		if decodeErr != nil {
			continue
		}
		if lock.ExpiresAtUnixMs <= now {
			_, _ = s.expireLock(ctx, lock, kv.ModRevision, releaseReasonLeaseExpired)
		}
	}

	currentResp, err := s.client.Get(ctx, s.key("current")+"/", clientv3.WithPrefix())
	if err != nil {
		return
	}
	for _, kv := range currentResp.Kvs {
		lock, decodeErr := decodeLock(kv.Value)
		if decodeErr != nil {
			continue
		}
		currentLock, _, getErr := s.getLock(ctx, lock.BatchID)
		if getErr != nil || currentLock != nil {
			continue
		}
		if exists, existsErr := s.releaseEventExists(ctx, lock.BatchID, lock.Generation); existsErr == nil && exists {
			continue
		}
		reason := releaseReasonLockMissing
		if lock.ExpiresAtUnixMs <= time.Now().UnixMilli() {
			reason = releaseReasonLeaseExpired
		}
		s.recordReleaseEvent(lock, reason)
	}
}

func batchFromKey(key string) string {
	index := strings.LastIndexByte(key, '/')
	if index < 0 || index+1 >= len(key) {
		return ""
	}
	return key[index+1:]
}
