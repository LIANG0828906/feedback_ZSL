package service

import (
	"context"

	"dnslab/checkpoint"
	"dnslab/codes"
	"dnslab/engine"
)

// SaveCheckpoint persists the latest complete report and cache segments.
func (s *Service) SaveCheckpoint(ctx context.Context) (*checkpoint.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, codes.New(codes.ECheckpointNotConfigured, "checkpoint store is not configured")
	}
	cp := checkpoint.Checkpoint{
		ProjectVersion: s.project.Version,
		SchemaVersion:  s.schema,
		Segments:       s.engine.SegmentSnapshots(),
		Pending:        []checkpoint.PendingFetch{},
	}
	if s.report != nil {
		cp.CommittedSeq = len(s.report.Results)
		for _, r := range s.report.Results {
			cp.Queries = append(cp.Queries, checkpoint.CommittedQuery{
				Sequence: r.Sequence, QueryID: r.QueryID, Result: r,
			})
		}
	}
	if err := s.store.Save(ctx, cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// RestoreCheckpoint loads a version-matching checkpoint into the live engine.
func (s *Service) RestoreCheckpoint(ctx context.Context) (*checkpoint.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		return nil, codes.New(codes.ECheckpointNotConfigured, "checkpoint store is not configured")
	}
	cp, err := s.store.LoadRestored(ctx, s.project.Version)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return nil, codes.New(codes.ECheckpointOpen, "checkpoint has no commit point")
	}
	restored := engine.NewEngine(s.project)
	for _, seg := range cp.Segments {
		restored.RestoreSegment(seg)
	}
	s.engine = restored
	return cp, nil
}

// RecomputeFromStart is the deterministic recovery action after version mismatch.
func (s *Service) RecomputeFromStart(ctx context.Context, workers int) (*RunResponse, error) {
	return s.RunAll(ctx, workers)
}

// RecoveryResponse compares checkpoint recovery with deterministic recompute.
type RecoveryResponse struct {
	Restored   bool                   `json:"restored"`
	Checkpoint *checkpoint.Checkpoint `json:"checkpoint,omitempty"`
	Mismatch   *codes.Error           `json:"mismatch,omitempty"`
	Recompute  *RunResponse           `json:"recompute,omitempty"`
}

// Recover accepts only a matching checkpoint. A version mismatch triggers a
// full deterministic recompute from the beginning.
func (s *Service) Recover(ctx context.Context, workers int) (*RecoveryResponse, error) {
	cp, err := s.RestoreCheckpoint(ctx)
	if err == nil {
		return &RecoveryResponse{Restored: true, Checkpoint: cp}, nil
	}
	biz, ok := codes.As(err)
	if !ok || biz.Code != codes.ECheckpointVersion {
		return nil, err
	}
	run, runErr := s.RecomputeFromStart(ctx, workers)
	if runErr != nil {
		return nil, runErr
	}
	return &RecoveryResponse{Restored: false, Mismatch: biz, Recompute: run}, nil
}
