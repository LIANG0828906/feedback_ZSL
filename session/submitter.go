package session

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"performanceexpressions/workspace/domain"
)

type BatchResult struct {
	Ordinal     uint64               `json:"ordinal"`
	BatchID     string               `json:"batch_id"`
	SegmentIDs  []string             `json:"segment_ids"`
	Receipt     domain.CommitReceipt `json:"receipt,omitempty"`
	Error       *domain.AppError     `json:"error,omitempty"`
	SubmittedAt time.Time            `json:"submitted_at"`
	FinishedAt  time.Time            `json:"finished_at"`
}

type BatchSubmitter struct {
	session *Session
	pool    *Pool
	ordinal atomic.Uint64
	mu      sync.Mutex
	results []BatchResult
	closed  bool
}

// NewBatchSubmitter binds bounded parallel submission to a session. Worker
// queue exhaustion blocks Submit; session state itself remains serialized and
// atomic. Results can be read later in submission order.
func NewBatchSubmitter(ctx context.Context, sess *Session, maxWorkers, perWorkerBuffer int) *BatchSubmitter {
	return &BatchSubmitter{
		session: sess,
		pool:    NewPool(ctx, maxWorkers, perWorkerBuffer),
	}
}

func (s *BatchSubmitter) Submit(ctx context.Context, batchID string, segmentIDs []string) (uint64, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, context.Canceled
	}
	ordinal := s.ordinal.Add(1)
	submittedAt := time.Now().UTC()
	s.mu.Unlock()
	err := s.pool.Submit(ctx, func(jobCtx context.Context) error {
		receipt, commitErr := s.session.CommitBatch(jobCtx, batchID, segmentIDs)
		result := BatchResult{
			Ordinal:     ordinal,
			BatchID:     batchID,
			SegmentIDs:  append([]string(nil), segmentIDs...),
			Receipt:     receipt,
			SubmittedAt: submittedAt,
			FinishedAt:  time.Now().UTC(),
		}
		if commitErr != nil {
			if appErr, ok := domain.AsAppError(commitErr); ok {
				result.Error = appErr
			} else {
				result.Error = domain.NewError(domain.CodeCommit, "%v", commitErr)
			}
		}
		s.mu.Lock()
		s.results = append(s.results, result)
		s.mu.Unlock()
		return commitErr
	})
	return ordinal, err
}

func (s *BatchSubmitter) Results() []BatchResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]BatchResult(nil), s.results...)
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

func (s *BatchSubmitter) CloseAndWait() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.pool.CloseAndWait()
}
