package gateway

import (
	"context"
	"time"

	"s3gw/internal/store"
)

type ReapResult struct {
	StartedAt         string `json:"started_at"`
	FinishedAt        string `json:"finished_at"`
	ExaminedSessions  int    `json:"examined_sessions"`
	ReclaimedSessions int    `json:"reclaimed_sessions"`
	FailedSessions    int    `json:"failed_sessions"`
	ReclaimedBytes    int64  `json:"reclaimed_bytes"`
}

func (s *Server) ReapOnce(ctx context.Context) ReapResult {
	started := time.Now().UTC()
	result := ReapResult{StartedAt: started.Format(time.RFC3339Nano)}
	buckets, err := s.store.ListBuckets(ctx)
	if err != nil {
		s.logger.Printf("reaper: list buckets: %v", err)
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return result
	}
	for _, bucket := range buckets {
		s.metrics.SetLastReapBytes(bucket, 0)
	}
	cutoff := time.Now().Add(-s.cfg.PartRetention).UnixNano()
	sessions, err := s.store.ExpiredSessions(ctx, cutoff)
	if err != nil {
		s.logger.Printf("reaper: list expired sessions: %v", err)
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return result
	}
	result.ExaminedSessions = len(sessions)
	for _, candidate := range sessions {
		unlock := s.locks.Lock(candidate.UploadID)
		session, getErr := s.store.GetSession(ctx, candidate.UploadID)
		if getErr != nil {
			s.metrics.AddReapFailures(candidate.Bucket, 1)
			result.FailedSessions++
			unlock()
			continue
		}
		if session == nil || session.Status != store.SessionActive || session.CreatedAt >= cutoff {
			unlock()
			continue
		}
		claimed, claimErr := s.store.ClaimSession(ctx, session.UploadID, store.SessionActive, store.SessionReaping)
		if claimErr != nil || !claimed {
			if claimErr != nil {
				s.metrics.AddReapFailures(session.Bucket, 1)
				result.FailedSessions++
			}
			unlock()
			continue
		}
		parts, partErr := s.store.ListParts(ctx, session.ID)
		if partErr != nil {
			_ = s.store.SetSessionStatus(ctx, session.UploadID, store.SessionReaping, store.SessionActive)
			s.metrics.AddReapFailures(session.Bucket, 1)
			result.FailedSessions++
			unlock()
			continue
		}
		failed := false
		for _, part := range parts {
			if err := s.backend.DeleteObject(ctx, session.Bucket, part.BackendKey); err != nil {
				s.logger.Printf("reaper: delete upload=%s part=%d: %v", session.UploadID, part.PartNumber, err)
				failed = true
			}
		}
		if failed {
			_ = s.store.SetSessionStatus(ctx, session.UploadID, store.SessionReaping, store.SessionActive)
			s.metrics.AddReapFailures(session.Bucket, 1)
			result.FailedSessions++
			unlock()
			continue
		}
		bytes, finishErr := s.store.FinishReap(ctx, session.ID, session.UploadID)
		if finishErr != nil {
			_ = s.store.SetSessionStatus(ctx, session.UploadID, store.SessionReaping, store.SessionActive)
			s.metrics.AddReapFailures(session.Bucket, 1)
			result.FailedSessions++
			unlock()
			continue
		}
		result.ReclaimedSessions++
		result.ReclaimedBytes += bytes
		s.metrics.AddReapBytes(session.Bucket, bytes)
		unlock()
	}
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return result
}
