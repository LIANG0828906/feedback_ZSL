package colortrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Budget bounds scheduler parallelism and in-memory work.
type Budget struct {
	MaxParallel      int `json:"max_parallel"`
	PerWorkerSamples int `json:"per_worker_samples"`
	PendingTasks     int `json:"pending_tasks"`
}

func (b Budget) normalized() Budget {
	if b.MaxParallel <= 0 {
		b.MaxParallel = 1
	}
	if b.PerWorkerSamples <= 0 {
		b.PerWorkerSamples = 1
	}
	if b.PendingTasks <= 0 {
		b.PendingTasks = 1
	}
	return b
}

// Scheduler executes chunk stages against a checkpoint store.
type Scheduler struct {
	Store              CheckpointStore
	Budget             Budget
	Force              bool
	CancelAfterCommits int
	Inject             func(stage int, chunkID string) error
	Gate               func(ctx context.Context, stage int, chunkID string) <-chan struct{}

	mu                  sync.Mutex
	queueWaits          []QueueWait
	bufferFlushes       int
	maxObservedParallel int32
	active              int32
	commits             int
}

// RunReport contains outputs and scheduling evidence.
type RunReport struct {
	Result              *PipelineResult `json:"result"`
	CommitSequence      []CommitEntry   `json:"commit_sequence"`
	QueueWaits          []QueueWait     `json:"queue_waits"`
	Skipped             []string        `json:"skipped"`
	Processed           []string        `json:"processed"`
	BufferFlushes       int             `json:"buffer_flushes"`
	MaxObservedParallel int             `json:"max_observed_parallel"`
}

type chunkJob struct {
	index int
	chunk ChunkCoord
}

type chunkOutcome struct {
	index         int
	chunk         ChunkCoord
	result        *ChunkResult
	err           error
	bufferFlushes int
}

// Run executes all stages. Committed chunks are reused unless Force is set.
func (s *Scheduler) Run(ctx context.Context, project *Project) (*RunReport, error) {
	if s.Store == nil {
		return nil, schemaError("store", "checkpoint store is required")
	}
	budget := s.Budget.normalized()
	truthFingerprint := ""
	if project.Truth != nil {
		truthFingerprint = project.Truth.Fingerprint
	}
	meta := NewSnapshotMeta(project, truthFingerprint)
	if err := s.Store.InitSnapshot(meta); err != nil {
		return nil, err
	}
	artifactProject := *project
	artifactProject.Truth = nil
	projectJSON, err := json.Marshal(&artifactProject)
	if err != nil {
		return nil, err
	}
	if err := s.Store.PutArtifact("project.json", projectJSON); err != nil {
		return nil, err
	}

	chunkStates := NewChunkStateMachine()
	stageStates := NewStageStateMachine()
	report := &RunReport{Result: &PipelineResult{}}
	var previous *Sample
	var previousChunks map[string]*ChunkResult

	for stageIdx := range project.Stages {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if _, err := stageStates.Transition(stageIdx, StateRunning); err != nil {
			return report, err
		}
		first := project.Planes[0]
		width, height, err := StagePlaneDimensions(project, stageIdx, first.Name)
		if err != nil {
			return report, err
		}
		chunks := AllChunks(width, height, project.ChunkSize)
		currentChunks := map[string]*ChunkResult{}
		jobs := []chunkJob{}

		for _, chunk := range chunks {
			id := ChunkID(chunk)
			if !s.Force {
				record, err := s.Store.Unit(stageIdx, id)
				if err != nil {
					return report, err
				}
				if record != nil && record.State == StateCommitted &&
					RecordMatchesSnapshot(*record, meta) && record.ParamVersion == project.Stages[stageIdx].ParamVersion {
					var cached ChunkResult
					if len(record.OutputJSON) > 0 {
						if err := json.Unmarshal(record.OutputJSON, &cached); err != nil {
							return report, err
						}
						currentChunks[id] = &cached
						report.Skipped = append(report.Skipped, UnitKey(stageIdx, id))
						continue
					}
				}
			}
			jobs = append(jobs, chunkJob{index: len(jobs), chunk: chunk})
		}

		stageErr := s.runJobs(ctx, project, meta, budget, stageIdx, jobs, previous, previousChunks, currentChunks, chunkStates, report)
		if stageErr != nil {
			_, _ = stageStates.Transition(stageIdx, StateFailed)
			seq, _ := s.Store.CommitLog(project.ProjectID)
			report.CommitSequence = seq
			s.snapshotStats(report)
			return report, stageErr
		}
		if err := ctx.Err(); err != nil {
			seq, _ := s.Store.CommitLog(project.ProjectID)
			report.CommitSequence = seq
			s.snapshotStats(report)
			return report, err
		}

		sample, err := AssembleChunks(project, stageIdx, currentChunks)
		if err != nil {
			return report, err
		}
		report.Result.Samples = append(report.Result.Samples, sample)
		report.Result.Stages = append(report.Result.Stages, &StageResult{
			Config: project.Stages[stageIdx], Sample: sample, Chunks: currentChunks,
		})
		if _, err := stageStates.Transition(stageIdx, StateCommitted); err != nil {
			return report, err
		}
		previous = sample
		previousChunks = currentChunks
	}

	band, err := AnalyzeFirstBand(project, report.Result.Samples)
	report.Result.Band = band
	seq, logErr := s.Store.CommitLog(project.ProjectID)
	if logErr != nil {
		return report, logErr
	}
	report.CommitSequence = seq
	s.snapshotStats(report)
	if err != nil {
		return report, err
	}
	return report, nil
}

func (s *Scheduler) runJobs(
	ctx context.Context,
	project *Project,
	meta SnapshotMeta,
	budget Budget,
	stageIdx int,
	jobs []chunkJob,
	previous *Sample,
	previousChunks map[string]*ChunkResult,
	currentChunks map[string]*ChunkResult,
	states *ChunkStateMachine,
	report *RunReport,
) error {
	if len(jobs) == 0 {
		return nil
	}
	jobCh := make(chan chunkJob, budget.PendingTasks)
	outcomes := make(chan chunkOutcome, len(jobs))
	var wg sync.WaitGroup
	for worker := 0; worker < budget.MaxParallel; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				atomic.AddInt32(&s.active, 1)
				s.updateMaxParallel()
				id := ChunkID(job.chunk)
				_, _ = states.Transition(stageIdx, id, StateRunning)
				outcome := chunkOutcome{index: job.index, chunk: job.chunk}
				if err := ctx.Err(); err != nil {
					outcome.err = err
				} else if s.Gate != nil {
					wait := s.Gate(ctx, stageIdx, id)
					if wait != nil {
						select {
						case <-wait:
						case <-ctx.Done():
							outcome.err = ctx.Err()
						}
					}
				}
				if outcome.err == nil && s.Inject != nil {
					outcome.err = s.Inject(stageIdx, id)
				}
				if outcome.err == nil {
					result, err := ProcessChunk(project, stageIdx, previous, previousChunks, job.chunk)
					outcome.err = err
					if err == nil {
						outcome.result = result
						outcome.bufferFlushes = drainSampleBuffer(result, budget.PerWorkerSamples)
					}
				}
				atomic.AddInt32(&s.active, -1)
				outcomes <- outcome
			}
		}()
	}
	go func() {
		for _, job := range jobs {
			if len(jobCh) == cap(jobCh) {
				started := time.Now()
				jobCh <- job
				s.mu.Lock()
				s.queueWaits = append(s.queueWaits, QueueWait{
					Stage: stageIdx, ChunkID: ChunkID(job.chunk),
					Reason: "pending_queue_budget", Waited: time.Since(started), At: started,
				})
				s.mu.Unlock()
			} else {
				jobCh <- job
			}
		}
		close(jobCh)
	}()
	go func() {
		wg.Wait()
		close(outcomes)
	}()

	pending := map[int]chunkOutcome{}
	next := jobs[0].index
	var firstErr error
	commitStopped := false
	cancelRequested := false
	for outcome := range outcomes {
		pending[outcome.index] = outcome
		s.mu.Lock()
		s.bufferFlushes += outcome.bufferFlushes
		s.mu.Unlock()
		for {
			outcome, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			id := ChunkID(outcome.chunk)
			if outcome.err != nil {
				_, _ = states.Transition(stageIdx, id, StateFailed)
				record := UnitRecord{
					ProjectID: project.ProjectID, Stage: stageIdx, ChunkID: id,
					ChunkX: outcome.chunk.X, ChunkY: outcome.chunk.Y, State: StateFailed,
					StageSequenceFingerprint: meta.StageSequenceFingerprint,
					ParamVersion:             project.Stages[stageIdx].ParamVersion,
					AlgorithmVersion:         AlgorithmVersion,
					LastError:                outcome.err.Error(), UpdatedAt: time.Now().UTC(),
				}
				_ = s.Store.SaveUnitState(record)
				report.Processed = append(report.Processed, UnitKey(stageIdx, id)+":failed")
				commitStopped = true
				if firstErr == nil && !errors.Is(outcome.err, context.Canceled) {
					firstErr = outcome.err
				}
				continue
			}
			if commitStopped || ctx.Err() != nil {
				commitStopped = true
				continue
			}
			if _, err := states.Transition(stageIdx, id, StateCommitted); err != nil {
				outcome.err = err
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			outputJSON, err := json.Marshal(outcome.result)
			if err != nil {
				return err
			}
			truncCount, truncTotal, truncHash := truncationEvidence(outcome.result)
			record := UnitRecord{
				ProjectID: project.ProjectID, Stage: stageIdx, ChunkID: id,
				ChunkX: outcome.chunk.X, ChunkY: outcome.chunk.Y, State: StateCommitted,
				OutputDigest: outcome.result.ContentHash, TruncationDigest: truncHash,
				TruncationCount: truncCount, TruncationAbsoluteTotal: truncTotal,
				InputContentHash: outcome.result.InputHash, ContentHash: outcome.result.ContentHash,
				CacheKey:                 outcome.result.CacheKey,
				StageSequenceFingerprint: meta.StageSequenceFingerprint,
				ParamVersion:             project.Stages[stageIdx].ParamVersion,
				AlgorithmVersion:         AlgorithmVersion, OutputJSON: outputJSON,
				UpdatedAt: time.Now().UTC(),
			}
			if err := s.Store.CommitUnit(record); err != nil {
				return err
			}
			currentChunks[id] = outcome.result
			report.Processed = append(report.Processed, UnitKey(stageIdx, id))
			s.mu.Lock()
			s.commits++
			cancelAfter := s.CancelAfterCommits
			s.mu.Unlock()
			if cancelAfter > 0 && s.commits >= cancelAfter {
				commitStopped = true
				cancelRequested = true
			}
		}
	}
	if cancelRequested {
		return context.Canceled
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func (s *Scheduler) updateMaxParallel() {
	active := atomic.LoadInt32(&s.active)
	for {
		max := atomic.LoadInt32(&s.maxObservedParallel)
		if active <= max || atomic.CompareAndSwapInt32(&s.maxObservedParallel, max, active) {
			break
		}
	}
}

func (s *Scheduler) snapshotStats(report *RunReport) {
	s.mu.Lock()
	report.QueueWaits = append([]QueueWait{}, s.queueWaits...)
	report.BufferFlushes = s.bufferFlushes
	s.mu.Unlock()
	report.MaxObservedParallel = int(atomic.LoadInt32(&s.maxObservedParallel))
}

func drainSampleBuffer(result *ChunkResult, capSamples int) int {
	if capSamples <= 0 {
		capSamples = 1
	}
	buffer := make([]PointLedger, 0, capSamples)
	flushes := 0
	for _, plane := range result.Planes {
		for _, ledger := range plane.Ledgers {
			if len(buffer) == cap(buffer) {
				flushes++
				buffer = buffer[:0]
			}
			buffer = append(buffer, ledger)
		}
	}
	return flushes
}

func truncationEvidence(result *ChunkResult) (int, int64, string) {
	all := []TruncationRecord{}
	var total int64
	for _, name := range result.PlaneNames {
		plane := result.Planes[name]
		if plane == nil {
			continue
		}
		for _, record := range plane.Truncations {
			all = append(all, record)
			total += abs64Int(record.Remainder) + abs64Int(record.RoundingDelta)
		}
	}
	return len(all), total, HashCanonical("truncations-v1", all)
}

// NewSnapshotMeta builds the restart-sensitive snapshot identity.
func NewSnapshotMeta(project *Project, truthFingerprint string) SnapshotMeta {
	paramVersions := map[int]string{}
	for _, stage := range project.Stages {
		paramVersions[stage.Index] = stage.ParamVersion
	}
	return SnapshotMeta{
		ProjectID:                project.ProjectID,
		StageSequenceFingerprint: SequenceFingerprint(project.Stages),
		ParamVersionFingerprint:  ParamVersionFingerprint(project.Stages),
		AlgorithmVersion:         AlgorithmVersion,
		TruthFingerprint:         truthFingerprint,
		ParamVersions:            paramVersions,
	}
}

var _ = fmt.Sprintf
