package forward

import (
	"sync"
)

type Budget struct {
	Workers         int `json:"workers"`
	PerTunnelBuffer int `json:"per_tunnel_buffer"`
}

func NormalizeBudget(b Budget) (Budget, error) {
	if b.Workers == 0 {
		b.Workers = 4
	}
	if b.PerTunnelBuffer == 0 {
		b.PerTunnelBuffer = 32
	}
	if biz := ValidateBudget(b); biz != nil {
		return b, biz
	}
	return b, nil
}

func ValidateBudget(b Budget) *BizError {
	if b.Workers < 1 || b.Workers > 64 {
		return NewBizError(CodeBudgetWorkers, "budget.workers", "workers must be 1..64").
			WithContext("actual", b.Workers)
	}
	if b.PerTunnelBuffer < 1 || b.PerTunnelBuffer > 4096 {
		return NewBizError(CodeBudgetBuffer, "budget.per_tunnel_buffer", "per-tunnel buffer must be 1..4096").
			WithContext("actual", b.PerTunnelBuffer)
	}
	return nil
}

type SubmitAck struct {
	Accepted     bool   `json:"accepted"`
	PartitionKey string `json:"partition_key"`
	Pending      int    `json:"pending"`
	BufferLimit  int    `json:"buffer_limit"`
	Code         string `json:"code,omitempty"`
	Message      string `json:"message,omitempty"`
}

type scheduledJob struct {
	fn func()
}

type Scheduler struct {
	budget Budget
	slots  chan struct{}

	mu     sync.Mutex
	queues map[string]chan scheduledJob
	wg     sync.WaitGroup
	closed bool
}

func NewScheduler(b Budget) (*Scheduler, *BizError) {
	if biz := ValidateBudget(b); biz != nil {
		return nil, biz
	}
	return &Scheduler{
		budget: b,
		slots:  make(chan struct{}, b.Workers),
		queues: map[string]chan scheduledJob{},
	}, nil
}

func (s *Scheduler) Budget() Budget { return s.budget }

func (s *Scheduler) queueFor(key string) chan scheduledJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.queues[key]; ok {
		return ch
	}
	ch := make(chan scheduledJob, s.budget.PerTunnelBuffer)
	s.queues[key] = ch
	go s.runQueue(ch)
	return ch
}

func (s *Scheduler) runQueue(ch chan scheduledJob) {
	for job := range ch {
		s.slots <- struct{}{}
		job.fn()
		<-s.slots
	}
}

// TrySubmit provides an observable, deterministic backpressure result. It never grows the buffer.
func (s *Scheduler) TrySubmit(key string, fn func()) (SubmitAck, *BizError) {
	if key == "" {
		key = "global"
	}
	ch := s.queueFor(key)
	ack := SubmitAck{PartitionKey: key, BufferLimit: s.budget.PerTunnelBuffer, Pending: len(ch) + 1}
	s.wg.Add(1)
	job := scheduledJob{fn: func() {
		defer s.wg.Done()
		fn()
	}}
	select {
	case ch <- job:
		ack.Accepted = true
		ack.Pending = len(ch)
		return ack, nil
	default:
		// Cancel the WaitGroup add because the job was not queued.
		s.wg.Done()
		biz := NewBizError(CodeBackpressureBufferFull, "budget.per_tunnel_buffer",
			"partition %q buffer is full at limit %d", key, s.budget.PerTunnelBuffer).
			WithContext("partition_key", key).
			WithContext("buffer_limit", s.budget.PerTunnelBuffer).
			WithContext("pending", len(ch))
		ack.Accepted = false
		ack.Code = biz.Code
		ack.Message = biz.Message
		ack.Pending = len(ch)
		return ack, biz
	}
}

// Submit blocks when the bounded queue is full. The caller therefore applies backpressure instead
// of dropping the event or allocating an unbounded queue.
func (s *Scheduler) Submit(key string, fn func()) {
	if key == "" {
		key = "global"
	}
	ch := s.queueFor(key)
	s.wg.Add(1)
	ch <- scheduledJob{fn: func() {
		defer s.wg.Done()
		fn()
	}}
}

type WorkerLease struct {
	s *Scheduler
}

func (l WorkerLease) Release() {
	if l.s != nil {
		<-l.s.slots
	}
}

func (s *Scheduler) TryAcquireWorker() (WorkerLease, *BizError) {
	select {
	case s.slots <- struct{}{}:
		return WorkerLease{s: s}, nil
	default:
		return WorkerLease{}, NewBizError(CodeBackpressureWorkers, "budget.workers",
			"all %d worker slots are busy", s.budget.Workers).
			WithContext("workers", s.budget.Workers)
	}
}

func (s *Scheduler) Wait() {
	s.wg.Wait()
}

func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	queues := make([]chan scheduledJob, 0, len(s.queues))
	for _, ch := range s.queues {
		queues = append(queues, ch)
	}
	s.mu.Unlock()
	for _, ch := range queues {
		close(ch)
	}
	// Queue goroutines exit after channels drain and close. Wait ensures job callbacks are complete.
	s.wg.Wait()
}

func RunParallelReport(p *Project, events []Event, budget Budget) (*Report, error) {
	budget, err := NormalizeBudget(budget)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	scheduler, bizErr := NewScheduler(budget)
	if bizErr != nil {
		return nil, bizErr
	}
	results := make([]EventReport, len(events))

	for i, ev := range events {
		idx := i
		event := ev
		key := "global"
		var purePacket *PacketEvaluation
		if event.Kind == "packet" && event.Packet != nil {
			pe := evaluatePacketPure(p, event.Seq, event.ID, *event.Packet)
			purePacket = &pe
			if pe.Plan.TunnelID != "" {
				key = pe.Plan.TunnelID
			} else if pe.Verdict.Action.Type != "" {
				key = "action:" + pe.Verdict.Action.Type
			}
		} else if event.TunnelID != "" {
			key = event.TunnelID
		}

		scheduler.Submit(key, func() {
			er := EventReport{
				Seq: event.Seq, EventID: event.ID, Kind: event.Kind, AtMs: event.AtMs,
				NormalizedTimeMs: event.AtMs, TunnelID: event.TunnelID,
				ConfigRevision: event.ConfigRevision,
				Transitions:    []Transition{}, Packets: []PacketEvaluation{},
			}
			switch event.Kind {
			case "connect", "handshake_complete", "disconnect", "keepalive_timeout":
				tr := engine.transition(event)
				er.Transitions = append(er.Transitions, tr)
				if tr.Error != nil {
					er.Error = tr.Error
				}
			case "packet":
				if purePacket != nil {
					pe := *purePacket
					if pe.Plan.TunnelID != "" {
						if st := engine.states[pe.Plan.TunnelID]; st != nil {
							pe.TunnelState = st.State
							pe.Forwardable = st.State == StateEstablished
						}
					}
					er.Packets = append(er.Packets, pe)
				}
			case "config_apply":
				// Revision carried in event.
			default:
				er.Error = NewBizError(CodeEventUnknown, "events.kind", "unknown event kind %q", event.Kind)
			}
			results[idx] = er
		})
	}
	scheduler.Wait()
	scheduler.Close()

	report := &Report{
		ProjectID: p.ID, SchemaVersion: p.SourceSchema, ContentHash: p.ContentHash,
		EventCount: len(results), Events: results, FinalStates: engine.SnapshotStates(),
		Budget: &budget,
	}
	FillEventStateSnapshots(report, p)
	if p.WatermarkDegraded {
		report.WatermarkNote = "schema v1 has no keepalive watermark field; declared degradation disables watermark processing"
	}
	report.RejectSet = collectRejects(results)
	return report, nil
}
