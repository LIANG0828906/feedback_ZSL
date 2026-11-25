package lipsync

import (
	"context"
	"sort"
	"sync"
	"time"
)

type PoolOptions struct {
	MaxParallelism  int `json:"maxParallelism"`
	PerWorkerBuffer int `json:"perWorkerBuffer"`
	QueueBudget     int `json:"queueBudget"`
}

func DefaultPoolOptions() PoolOptions {
	return PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 64, QueueBudget: 8}
}

type StateBuffer struct {
	WorkerID int
	Capacity int
	states   []string
}

func (b *StateBuffer) Push(state string) error {
	if b == nil {
		return nil
	}
	if len(b.states) >= b.Capacity {
		return NewError(ECStage, "", 0, false, "worker.stateBuffer",
			"worker %d state buffer capacity %d exhausted", b.WorkerID, b.Capacity)
	}
	b.states = append(b.states, state)
	return nil
}

func (b *StateBuffer) States() []string {
	return append([]string(nil), b.states...)
}

func (b *StateBuffer) Len() int { return len(b.states) }

type ShardWork struct {
	ShardIndex int
	DialogueID string
	Task       func(ctx context.Context, buffer *StateBuffer) (any, error)
}

type ShardOutcome struct {
	ShardIndex int           `json:"shardIndex"`
	DialogueID string        `json:"dialogueId"`
	WorkerID   int           `json:"workerId"`
	Value      any           `json:"-"`
	Err        error         `json:"-"`
	Waited     time.Duration `json:"waited"`
}

type PoolRun struct {
	Outcomes []ShardOutcome `json:"outcomes"`
	Events   []QueueEvent   `json:"events"`
}

func RunShardPool(ctx context.Context, opts PoolOptions, works []ShardWork) (*PoolRun, error) {
	if opts.MaxParallelism <= 0 {
		opts.MaxParallelism = 1
	}
	if opts.PerWorkerBuffer <= 0 {
		opts.PerWorkerBuffer = 16
	}
	if opts.QueueBudget <= 0 {
		opts.QueueBudget = opts.MaxParallelism
	}

	jobs := make(chan ShardWork, opts.QueueBudget)
	outcomes := make(chan ShardOutcome, len(works))
	var wg sync.WaitGroup
	var eventsMu sync.Mutex
	events := []QueueEvent{}

	addEvent := func(event QueueEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	}

	for workerID := 0; workerID < opts.MaxParallelism; workerID++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			buffer := &StateBuffer{WorkerID: workerID, Capacity: opts.PerWorkerBuffer}
			for work := range jobs {
				outcome := ShardOutcome{
					ShardIndex: work.ShardIndex,
					DialogueID: work.DialogueID,
					WorkerID:   workerID,
				}
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							outcome.Err = NewError(ECStage, work.DialogueID, work.ShardIndex, true,
								"shard.task", "internal shard failure: %v", recovered)
						}
					}()
					if work.Task == nil {
						outcome.Err = NewError(ECStage, work.DialogueID, work.ShardIndex, true,
							"shard.task", "nil task")
						return
					}
					value, err := work.Task(ctx, buffer)
					outcome.Value = value
					outcome.Err = err
				}()
				if outcome.Err != nil {
					if domainErr, ok := AsError(outcome.Err); ok && !domainErr.HasShard {
						domainErr.ShardIndex = work.ShardIndex
						domainErr.HasShard = true
						if domainErr.DialogueID == "" {
							domainErr.DialogueID = work.DialogueID
						}
					}
				}
				outcomes <- outcome
			}
		}(workerID)
	}

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for _, work := range works {
			wasFull := len(jobs) == cap(jobs)
			started := time.Now()
			select {
			case jobs <- work:
				waited := time.Since(started)
				if wasFull {
					addEvent(QueueEvent{
						Time:         started,
						ShardIndex:   work.ShardIndex,
						DialogueID:   work.DialogueID,
						Reason:       "queue_budget_exhausted",
						Waited:       waited,
						WaitedMillis: waited.Milliseconds(),
						Parallelism:  opts.MaxParallelism,
						BufferUsed:   len(jobs),
						QueueBudget:  opts.QueueBudget,
					})
				}
			case <-ctx.Done():
				return
			}
		}
		close(jobs)
	}()

	go func() {
		<-producerDone
		wg.Wait()
		close(outcomes)
	}()

	collected := []ShardOutcome{}
	for outcome := range outcomes {
		collected = append(collected, outcome)
	}
	<-producerDone
	if ctx.Err() != nil {
		return &PoolRun{Outcomes: sortedOutcomes(collected), Events: events}, ctx.Err()
	}
	sort.SliceStable(collected, func(i, j int) bool {
		return collected[i].ShardIndex < collected[j].ShardIndex
	})
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ShardIndex != events[j].ShardIndex {
			return events[i].ShardIndex < events[j].ShardIndex
		}
		return events[i].Time.Before(events[j].Time)
	})
	return &PoolRun{Outcomes: collected, Events: events}, nil
}

func sortedOutcomes(items []ShardOutcome) []ShardOutcome {
	sort.SliceStable(items, func(i, j int) bool { return items[i].ShardIndex < items[j].ShardIndex })
	return items
}

func FirstOutcomeError(outcomes []ShardOutcome) error {
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			return outcome.Err
		}
	}
	return nil
}

func ProcessShard(project *Project, dialogue *Dialogue) (*DialogueResult, error) {
	if dialogue == nil {
		return nil, NewError(ECStage, "", 0, false, "dialogue", "nil dialogue shard")
	}
	quantized, truncations, err := QuantizeDialogue(dialogue, project.Recipe)
	if err != nil {
		return nil, err
	}
	packs, err := PackDialogue(dialogue, quantized, project.Recipe)
	if err != nil {
		return nil, err
	}
	return &DialogueResult{
		Dialogue:    dialogue,
		Quantized:   quantized,
		Packs:       packs,
		Attribution: BuildAttribution(dialogue, quantized, packs),
		Truncations: truncations,
	}, nil
}

func ShardWorksForProject(project *Project, failShardIndex *int) []ShardWork {
	dialogues := sortedDialogues(project.Dialogues)
	works := make([]ShardWork, 0, len(dialogues))
	for _, d := range dialogues {
		dialogue := d
		works = append(works, ShardWork{
			ShardIndex: d.ShardIndex,
			DialogueID: d.ID,
			Task: func(ctx context.Context, buffer *StateBuffer) (any, error) {
				if failShardIndex != nil && dialogue.ShardIndex == *failShardIndex {
					_ = buffer.Push("injected_stage_failure")
					return nil, NewError(ECStage, dialogue.ID, dialogue.ShardIndex, true,
						"shard.task", "injected internal failure in shard %d", dialogue.ShardIndex)
				}
				_ = buffer.Push("quantize")
				dr, err := ProcessShard(project, dialogue)
				if err != nil {
					return nil, err
				}
				_ = buffer.Push("pack")
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}
				_ = buffer.Push("attribute")
				return dr, nil
			},
		})
	}
	return works
}

func BuildResultFromShards(project *Project, outcomes []ShardOutcome) (*FullResult, error) {
	byShard := map[int]*DialogueResult{}
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			return nil, outcome.Err
		}
		dr, ok := outcome.Value.(*DialogueResult)
		if !ok {
			return nil, NewError(ECStage, outcome.DialogueID, outcome.ShardIndex, true,
				"shard.result", "unexpected shard result type %T", outcome.Value)
		}
		byShard[outcome.ShardIndex] = dr
	}
	dialogues := sortedDialogues(project.Dialogues)
	result := &FullResult{
		ProjectID: project.ID,
		Recipe:    project.Recipe,
		Dialogues: make([]*DialogueResult, 0, len(dialogues)),
	}
	allPacks := []Pack{}
	for _, d := range dialogues {
		dr := byShard[d.ShardIndex]
		if dr == nil {
			return nil, NewError(ECStage, d.ID, d.ShardIndex, true,
				"shard.result", "missing processed shard result")
		}
		result.Dialogues = append(result.Dialogues, dr)
		result.Truncations = append(result.Truncations, dr.Truncations...)
		allPacks = append(allPacks, dr.Packs...)
	}
	result.Dispatch = DispatchPacks(allPacks, project.Recipe)
	return result, nil
}
