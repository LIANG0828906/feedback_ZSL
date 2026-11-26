package service

import (
	"context"
	"fmt"

	"lip-pipeline-prototype/internal/checkpoint"
	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/pipeline"
	"lip-pipeline-prototype/internal/schedule"
)

type RunRequest struct {
	MaxParallelism           int    `json:"max_parallelism,omitempty"`
	CandidateBufferPerWorker int    `json:"candidate_buffer_per_worker,omitempty"`
	FrameQueueBudget         int    `json:"frame_queue_budget,omitempty"`
	FaultShardID             string `json:"fault_shard_id,omitempty"`
}

type RunResponse struct {
	Scheduler    *schedule.Report            `json:"scheduler"`
	Receipts     []*checkpoint.CommitReceipt `json:"receipts"`
	CommitOrder  []string                    `json:"commit_order"`
	FailedShards map[string]*domain.Error    `json:"failed_shards,omitempty"`
}

type ResumeResponse struct {
	Records     []domain.DispatchRecord `json:"records"`
	Recovered   []string                `json:"recovered"`
	Recomputed  []string                `json:"recomputed"`
	Invalidated []string                `json:"invalidated"`
	Run         *RunResponse            `json:"run,omitempty"`
}

func (s *Service) RunRealtime(ctx context.Context, req RunRequest) (*RunResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runShardsLocked(ctx, s.order, req)
}

func (s *Service) optionsLocked(req RunRequest) schedule.Options {
	opts := schedule.DefaultOptions()
	if req.MaxParallelism > 0 {
		opts.MaxParallelism = req.MaxParallelism
	}
	if req.CandidateBufferPerWorker > 0 {
		opts.CandidateBufferPerWorker = req.CandidateBufferPerWorker
	}
	if req.FrameQueueBudget > 0 {
		opts.FrameQueueBudget = req.FrameQueueBudget
	}
	opts.ChannelSlots = map[string]int{}
	for _, ch := range s.project.Channels {
		opts.ChannelSlots[ch.ID] = ch.Slots
	}
	return opts
}

func (s *Service) prepareMachineLocked(ctx context.Context, shardID string, shardIndex int) (*checkpoint.Machine, error) {
	m, err := s.store.LoadMachine(ctx, shardID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = checkpoint.NewMachine(shardID, shardIndex)
	}
	m.ShardIndex = shardIndex

	switch m.State {
	case checkpoint.StateCommitted:
		if err := m.Transition(checkpoint.StatePending, "explicit rerun invalidates in-memory execution"); err != nil {
			return nil, err
		}
	case checkpoint.StateFailed:
		if err := m.Transition(checkpoint.StatePending, "retry failed shard"); err != nil {
			return nil, err
		}
	case checkpoint.StatePending:
	case checkpoint.StateQuantizing, checkpoint.StateMerging, checkpoint.StateDelivering:
		if err := m.Transition(checkpoint.StateFailed, "interrupted before atomic commit"); err != nil {
			return nil, err
		}
		if err := s.store.SaveMachine(ctx, *m, ""); err != nil {
			return nil, err
		}
		if err := m.Transition(checkpoint.StatePending, "restart from last committed point"); err != nil {
			return nil, err
		}
	}
	for _, target := range []checkpoint.State{checkpoint.StateQuantizing, checkpoint.StateMerging, checkpoint.StateDelivering} {
		if err := m.Transition(target, "pipeline advance"); err != nil {
			return nil, err
		}
		if err := s.store.SaveMachine(ctx, *m, ""); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (s *Service) runShardsLocked(ctx context.Context, shardIDs []string, req RunRequest) (*RunResponse, error) {
	opts := s.optionsLocked(req)
	var candidates []schedule.Candidate
	machines := map[string]*checkpoint.Machine{}

	for _, shardID := range shardIDs {
		result := s.results[shardID]
		if result == nil {
			return nil, &domain.Error{Code: domain.CodeNotFound, Message: "result missing", ShardID: shardID}
		}
		machine, err := s.prepareMachineLocked(ctx, shardID, result.ShardIndex)
		if err != nil {
			return nil, err
		}
		machines[shardID] = machine

		for _, pack := range result.Packs {
			records := []domain.DispatchRecord{}
			for _, frame := range result.Frames {
				if frame.PackIndex == pack.Index {
					records = append(records, domain.DispatchRecord{
						DialogueID: result.DialogueID, ShardID: result.ShardID, FrameIndex: frame.Index,
						PackIndex: frame.PackIndex, ChannelID: result.ChannelID, Mouth: frame.Mouth, Kind: frame.Kind,
					})
				}
			}
			packIndex := pack.Index
			candidates = append(candidates, schedule.Candidate{
				ID: fmt.Sprintf("%s:p%02d", shardID, packIndex), DialogueID: result.DialogueID,
				ShardID: shardID, ShardIndex: result.ShardIndex, PackIndex: packIndex,
				ChannelID: result.ChannelID, FrameCount: len(records),
				Work: func(ctx context.Context, emit func(domain.DispatchRecord)) error {
					if req.FaultShardID == shardID && packIndex == 0 {
						idx := result.ShardIndex
						frame := int64(0)
						return &domain.Error{
							Code: domain.CodeStage, Message: "injected intra-shard dispatch failure",
							DialogueID: result.DialogueID, ShardID: shardID, ShardIndex: &idx,
							PackIndex: &packIndex, Frame: &frame, Path: "fault_injection.stage_failure",
						}
					}
					for _, record := range records {
						select {
						case <-ctx.Done():
							return ctx.Err()
						default:
						}
						emit(record)
					}
					return nil
				},
			})
		}
	}

	schedReport, err := schedule.Run(ctx, candidates, opts)
	if err != nil {
		return nil, err
	}
	failed := map[string]*domain.Error{}
	for _, outcome := range schedReport.Outcomes {
		if outcome.Err == nil {
			continue
		}
		if _, exists := failed[outcome.ShardID]; exists {
			continue
		}
		if de, ok := domain.AsError(outcome.Err); ok {
			failed[outcome.ShardID] = de
		} else {
			idx := outcome.ShardIndex
			failed[outcome.ShardID] = &domain.Error{
				Code: domain.CodeStage, Message: outcome.Err.Error(), ShardID: outcome.ShardID, ShardIndex: &idx,
			}
		}
	}

	resp := &RunResponse{Scheduler: schedReport, FailedShards: failed}
	for _, shardID := range shardIDs {
		result := s.results[shardID]
		machine := machines[shardID]
		if fault, bad := failed[shardID]; bad {
			_ = machine.Transition(checkpoint.StateFailed, fault.Message)
			_ = s.store.SaveMachine(ctx, *machine, result.DialogueID)
			continue
		}
		if err := machine.Transition(checkpoint.StateCommitted, "atomic shard checkpoint"); err != nil {
			return nil, err
		}
		receipt, err := s.store.CommitShard(ctx, result, result.SourceVersion)
		if err != nil {
			_ = machine.Transition(checkpoint.StateFailed, err.Error())
			_ = s.store.SaveMachine(ctx, *machine, result.DialogueID)
			return nil, err
		}
		resp.Receipts = append(resp.Receipts, receipt)
	}
	order, err := s.store.CommitOrder(ctx)
	if err != nil {
		return nil, err
	}
	resp.CommitOrder = order
	return resp, nil
}

func (s *Service) FullRecompute() ([]domain.DispatchRecord, []*domain.ShardResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	results := []*domain.ShardResult{}
	records := []domain.DispatchRecord{}
	for _, shardID := range s.order {
		dlg, shard, err := s.findShard(shardID)
		if err != nil {
			return nil, nil, err
		}
		result, err := pipeline.BuildShard(*shard, s.project.Params, dlg.ContentHash, dlg.SourceVersion)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, result)
		records = append(records, schedule.FramesFromResult(result)...)
	}
	return schedule.CanonicalRecords(records), results, nil
}

func (s *Service) Resume(ctx context.Context, req RunRequest) (*ResumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ResumeResponse{}
	missing := []string{}
	for _, shardID := range s.order {
		result := s.results[shardID]
		recovery, err := s.store.Recover(ctx, shardID, result.ContentHash, result.ParamVersion, domain.AlgorithmVersion, result.SourceVersion)
		if err != nil {
			return nil, err
		}
		switch {
		case recovery != nil && recovery.Result != nil:
			resp.Recovered = append(resp.Recovered, shardID)
			for _, frame := range recovery.Result.Frames {
				resp.Records = append(resp.Records, domain.DispatchRecord{
					DialogueID: result.DialogueID, ShardID: shardID, FrameIndex: frame.Index,
					PackIndex: frame.PackIndex, ChannelID: result.ChannelID, Mouth: frame.Mouth, Kind: frame.Kind,
				})
			}
		case recovery != nil && recovery.Invalidated:
			resp.Invalidated = append(resp.Invalidated, shardID)
			missing = append(missing, shardID)
		default:
			missing = append(missing, shardID)
		}
	}
	resp.Recomputed = missing
	if len(missing) > 0 {
		run, err := s.runShardsLocked(ctx, missing, req)
		if err != nil {
			return nil, err
		}
		resp.Run = run
		resp.Records = append(resp.Records, run.Scheduler.Records...)
	}
	resp.Records = schedule.CanonicalRecords(resp.Records)
	return resp, nil
}

func CompareDispatchSequences(a, b []domain.DispatchRecord) error {
	a = schedule.CanonicalRecords(a)
	b = schedule.CanonicalRecords(b)
	if len(a) != len(b) {
		return &domain.Error{Code: domain.CodeDiverge, Message: "dispatch sequence length differs", Expected: len(b), Actual: len(a)}
	}
	for i := range a {
		if a[i] != b[i] {
			frame := a[i].FrameIndex
			pack := a[i].PackIndex
			return &domain.Error{
				Code: domain.CodeDiverge, Message: "dispatch sequences differ at canonical position",
				Frame: &frame, PackIndex: &pack, Expected: b[i], Actual: a[i],
			}
		}
	}
	return nil
}
