package schedule

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"lip-pipeline-prototype/internal/domain"
)

type Candidate struct {
	ID         string
	DialogueID string
	ShardID    string
	ShardIndex int
	PackIndex  int
	ChannelID  string
	FrameCount int
	Work       func(ctx context.Context, emit func(domain.DispatchRecord)) error
}

type Outcome struct {
	CandidateID    string           `json:"candidate_id"`
	DialogueID     string           `json:"dialogue_id"`
	ShardID        string           `json:"shard_id"`
	ShardIndex     int              `json:"shard_index"`
	PackIndex      int              `json:"pack_index"`
	ChannelID      string           `json:"channel_id"`
	Err            error            `json:"error,omitempty"`
	ErrorCode      domain.ErrorCode `json:"error_code,omitempty"`
	DurationMicros int64            `json:"duration_micros"`
}

type WaitEvent struct {
	Kind         string `json:"kind"`
	CandidateID  string `json:"candidate_id,omitempty"`
	WorkerID     int    `json:"worker_id,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	Reason       string `json:"reason"`
	WaitedMicros int64  `json:"waited_micros"`
}

type BudgetCheck struct {
	CandidateID string `json:"candidate_id"`
	FrameCount  int    `json:"frame_count"`
	FrameBudget int    `json:"frame_budget"`
	OK          bool   `json:"ok"`
}

type Options struct {
	MaxParallelism           int            `json:"max_parallelism"`
	CandidateBufferPerWorker int            `json:"candidate_buffer_per_worker"`
	FrameQueueBudget         int            `json:"frame_queue_budget"`
	ChannelSlots             map[string]int `json:"channel_slots,omitempty"`
}

type Report struct {
	Options      Options                 `json:"options"`
	Outcomes     []Outcome               `json:"outcomes"`
	Records      []domain.DispatchRecord `json:"records"`
	Waits        []WaitEvent             `json:"waits"`
	BudgetChecks []BudgetCheck           `json:"budget_checks"`
}

func DefaultOptions() Options {
	return Options{MaxParallelism: 2, CandidateBufferPerWorker: 2, FrameQueueBudget: 64, ChannelSlots: map[string]int{"ch-a": 2, "ch-b": 1}}
}

type Recorder struct {
	mu     sync.Mutex
	waits  []WaitEvent
	checks []BudgetCheck
}

func (r *Recorder) Wait(e WaitEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, e)
}

func (r *Recorder) Check(c BudgetCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, c)
}

func (r *Recorder) Waits() []WaitEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WaitEvent(nil), r.waits...)
}

func (r *Recorder) Checks() []BudgetCheck {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BudgetCheck(nil), r.checks...)
}

type FrameQueue struct {
	ch     chan domain.DispatchRecord
	budget int
	rec    *Recorder
}

func NewFrameQueue(budget int, rec *Recorder) *FrameQueue {
	if budget <= 0 {
		budget = 64
	}
	return &FrameQueue{ch: make(chan domain.DispatchRecord, budget), budget: budget, rec: rec}
}

func (q *FrameQueue) Send(ctx context.Context, record domain.DispatchRecord, workerID int) error {
	select {
	case q.ch <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	start := time.Now()
	if q.rec != nil {
		q.rec.Wait(WaitEvent{Kind: "frame_queue", WorkerID: workerID, ChannelID: record.ChannelID, Reason: "pending_frame_queue_budget_exhausted"})
	}
	select {
	case q.ch <- record:
		if q.rec != nil {
			waited := time.Since(start).Microseconds()
			q.rec.mu.Lock()
			if n := len(q.rec.waits); n > 0 {
				q.rec.waits[n-1].WaitedMicros = waited
			}
			q.rec.mu.Unlock()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *FrameQueue) Next(ctx context.Context) (domain.DispatchRecord, bool, error) {
	select {
	case r, ok := <-q.ch:
		return r, ok, nil
	case <-ctx.Done():
		return domain.DispatchRecord{}, false, ctx.Err()
	}
}

func (q *FrameQueue) Close() { close(q.ch) }

func Run(ctx context.Context, candidates []Candidate, opts Options) (*Report, error) {
	if opts.MaxParallelism <= 0 {
		opts.MaxParallelism = 2
	}
	if opts.CandidateBufferPerWorker <= 0 {
		opts.CandidateBufferPerWorker = 1
	}
	if opts.FrameQueueBudget <= 0 {
		opts.FrameQueueBudget = 64
	}
	recorder := &Recorder{}
	frameQueue := NewFrameQueue(opts.FrameQueueBudget, recorder)
	report := &Report{Options: opts}

	workerChans := make([]chan Candidate, opts.MaxParallelism)
	var workerWG sync.WaitGroup
	var outcomeMu sync.Mutex
	outcomes := []Outcome{}

	channelSemaphores := map[string]chan struct{}{}
	for channelID, slots := range opts.ChannelSlots {
		if slots <= 0 {
			slots = 1
		}
		channelSemaphores[channelID] = make(chan struct{}, slots)
	}
	channelMu := sync.Mutex{}
	getSem := func(channelID string) chan struct{} {
		channelMu.Lock()
		defer channelMu.Unlock()
		if channelID == "" {
			return nil
		}
		sem, ok := channelSemaphores[channelID]
		if !ok {
			sem = make(chan struct{}, 1)
			channelSemaphores[channelID] = sem
		}
		return sem
	}

	for workerID := 0; workerID < opts.MaxParallelism; workerID++ {
		ch := make(chan Candidate, opts.CandidateBufferPerWorker)
		workerChans[workerID] = ch
		workerWG.Add(1)
		go func(workerID int, ch <-chan Candidate) {
			defer workerWG.Done()
			for candidate := range ch {
				outcome := executeCandidate(ctx, candidate, workerID, opts, recorder, frameQueue, getSem(candidate.ChannelID))
				outcomeMu.Lock()
				outcomes = append(outcomes, outcome)
				outcomeMu.Unlock()
			}
		}(workerID, ch)
	}

	var collectWG sync.WaitGroup
	var recordsMu sync.Mutex
	collectWG.Add(1)
	go func() {
		defer collectWG.Done()
		for record := range frameQueue.ch {
			recordsMu.Lock()
			report.Records = append(report.Records, record)
			recordsMu.Unlock()
		}
	}()

	rr := 0
	for _, candidate := range candidates {
		dispatched := false
		for attempt := 0; attempt < len(workerChans); attempt++ {
			idx := (rr + attempt) % len(workerChans)
			select {
			case workerChans[idx] <- candidate:
				rr = (idx + 1) % len(workerChans)
				dispatched = true
			default:
			}
			if dispatched {
				break
			}
		}
		if !dispatched {
			idx := rr % len(workerChans)
			start := time.Now()
			recorder.Wait(WaitEvent{Kind: "candidate_buffer", CandidateID: candidate.ID, WorkerID: idx, Reason: "per_worker_candidate_buffer_full"})
			select {
			case workerChans[idx] <- candidate:
				waited := time.Since(start).Microseconds()
				events := recorder.Waits()
				if len(events) > 0 {
					recorder.mu.Lock()
					recorder.waits[len(recorder.waits)-1].WaitedMicros = waited
					recorder.mu.Unlock()
				}
				rr = (idx + 1) % len(workerChans)
			case <-ctx.Done():
				return report, ctx.Err()
			}
		}
	}
	for _, ch := range workerChans {
		close(ch)
	}
	workerWG.Wait()
	frameQueue.Close()
	collectWG.Wait()

	report.Outcomes = outcomes
	report.Waits = recorder.Waits()
	report.BudgetChecks = recorder.Checks()
	return report, nil
}

func executeCandidate(ctx context.Context, candidate Candidate, workerID int, opts Options, rec *Recorder, q *FrameQueue, sem chan struct{}) Outcome {
	start := time.Now()
	out := Outcome{
		CandidateID: candidate.ID, DialogueID: candidate.DialogueID, ShardID: candidate.ShardID,
		ShardIndex: candidate.ShardIndex, PackIndex: candidate.PackIndex, ChannelID: candidate.ChannelID,
	}
	check := BudgetCheck{CandidateID: candidate.ID, FrameCount: candidate.FrameCount, FrameBudget: opts.FrameQueueBudget, OK: candidate.FrameCount <= opts.FrameQueueBudget}
	rec.Check(check)
	if !check.OK {
		err := &domain.Error{
			Code: domain.CodeVal, Message: "candidate frame count exceeds pending frame budget",
			DialogueID: candidate.DialogueID, ShardID: candidate.ShardID, ShardIndex: &candidate.ShardIndex,
			PackIndex: &candidate.PackIndex, Field: "frame_queue_budget",
			Expected: opts.FrameQueueBudget, Actual: candidate.FrameCount,
		}
		out.Err = err
		out.ErrorCode = domain.CodeVal
		out.DurationMicros = time.Since(start).Microseconds()
		return out
	}

	if sem != nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			out.Err = ctx.Err()
			out.DurationMicros = time.Since(start).Microseconds()
			return out
		}
		defer func() { <-sem }()
	}

	if candidate.Work == nil {
		err := &domain.Error{Code: domain.CodeStage, Message: "candidate has no work function"}
		out.Err = err
		out.ErrorCode = domain.CodeStage
		out.DurationMicros = time.Since(start).Microseconds()
		return out
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err := &domain.Error{
				Code: domain.CodeStage, Message: fmt.Sprint("recovered shard stage panic: ", recovered),
				DialogueID: candidate.DialogueID, ShardID: candidate.ShardID, ShardIndex: &candidate.ShardIndex,
				PackIndex: &candidate.PackIndex,
			}
			out.Err = err
			out.ErrorCode = domain.CodeStage
			out.DurationMicros = time.Since(start).Microseconds()
		}
	}()

	emit := func(record domain.DispatchRecord) {
		if record.DialogueID == "" {
			record.DialogueID = candidate.DialogueID
		}
		if record.ShardID == "" {
			record.ShardID = candidate.ShardID
		}
		if record.PackIndex == 0 && candidate.PackIndex != 0 {
			record.PackIndex = candidate.PackIndex
		}
		if record.ChannelID == "" {
			record.ChannelID = candidate.ChannelID
		}
		if err := q.Send(ctx, record, workerID); err != nil && out.Err == nil {
			out.Err = err
		}
	}
	if err := candidate.Work(ctx, emit); err != nil {
		out.Err = err
		if de, ok := domain.AsError(err); ok {
			out.ErrorCode = de.Code
		} else {
			out.ErrorCode = domain.CodeStage
		}
	}
	out.DurationMicros = time.Since(start).Microseconds()
	return out
}

func CanonicalRecords(records []domain.DispatchRecord) []domain.DispatchRecord {
	out := append([]domain.DispatchRecord(nil), records...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].DialogueID != out[j].DialogueID {
			return out[i].DialogueID < out[j].DialogueID
		}
		if out[i].ShardID != out[j].ShardID {
			return out[i].ShardID < out[j].ShardID
		}
		return out[i].FrameIndex < out[j].FrameIndex
	})
	return out
}

func FramesFromResult(result *domain.ShardResult) []domain.DispatchRecord {
	records := make([]domain.DispatchRecord, 0, len(result.Frames))
	for _, f := range result.Frames {
		records = append(records, domain.DispatchRecord{
			DialogueID: result.DialogueID, ShardID: result.ShardID, FrameIndex: f.Index,
			PackIndex: f.PackIndex, ChannelID: result.ChannelID, Mouth: f.Mouth, Kind: f.Kind,
		})
	}
	return records
}
