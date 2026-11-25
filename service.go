package lipsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type ServiceOptions struct {
	DBPath string
	RunID  string
	Pool   PoolOptions
}

type PackSummaryRecord struct {
	PackID        string `json:"packId"`
	Index         int    `json:"index"`
	StartFrame    int    `json:"startFrame"`
	EndFrame      int    `json:"endFrame"`
	VisibleFrames int    `json:"visibleFrames"`
	PhonemeCount  int    `json:"phonemeCount"`
	SummaryHash   string `json:"summaryHash"`
}

type ShardFrameRecord struct {
	Frame            int               `json:"frame"`
	BudgetLimit      int               `json:"budgetLimit"`
	BudgetUsed       int               `json:"budgetUsed"`
	ActivePackCount  int               `json:"activePackCount"`
	ShardActivePacks []string          `json:"shardActivePacks"`
	Dispatched       []Assignment      `json:"dispatched"`
	Deferred         []DeferralRecord  `json:"deferred"`
	Rejected         []RejectionRecord `json:"rejected"`
}

type Service struct {
	mu           sync.Mutex
	Options      ServiceOptions
	Project      *Project
	Store        *Store
	Cache        *Cache
	Engine       *Engine
	RunID        string
	LastResult   *FullResult
	LastManifest *InvalidationManifest
	QueueEvents  []QueueEvent
}

type RecoveryInfo struct {
	LatestRun         *RunRecord          `json:"latestRun,omitempty"`
	ForcedTermination bool                `json:"forcedTermination"`
	UserCancelled     bool                `json:"userCancelled"`
	CompletedCleanly  bool                `json:"completedCleanly"`
	ValidShards       []int               `json:"validShards"`
	InvalidatedShards []ShardInvalidation `json:"invalidatedShards"`
	PendingShards     []int               `json:"pendingShards"`
}

type ShardInvalidation struct {
	ShardIndex int    `json:"shardIndex"`
	DialogueID string `json:"dialogueId"`
	Reason     string `json:"reason"`
}

func NewService(project *Project, opts ServiceOptions) (*Service, error) {
	store, err := OpenStore(opts.DBPath)
	if err != nil {
		return nil, err
	}
	return NewServiceWithStore(project, opts, store, true)
}

func NewServiceWithStore(project *Project, opts ServiceOptions, store *Store, startRun bool) (*Service, error) {
	if opts.Pool.MaxParallelism <= 0 {
		opts.Pool = DefaultPoolOptions()
	}
	runID := opts.RunID
	if runID == "" {
		runID = fmt.Sprintf("run-%d", time.Now().UnixNano())
		opts.RunID = runID
	}
	if startRun {
		if err := store.StartRun(runID, project.ID); err != nil {
			return nil, err
		}
	}
	cache := NewCache(store)
	if err := cache.LoadPersistent(project.ID); err != nil {
		return nil, err
	}
	engine, err := NewEngine(project, store)
	if err != nil {
		return nil, err
	}
	svc := &Service{
		Options:     opts,
		Project:     project,
		Store:       store,
		Cache:       cache,
		Engine:      engine,
		RunID:       runID,
		LastResult:  engine.Snapshot.Result,
		QueueEvents: []QueueEvent{},
	}
	return svc, nil
}

func (s *Service) Close() error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.Close()
}

func (s *Service) MarkCompleted() error {
	return s.Store.FinishRun(s.RunID, "completed", `{"status":"completed"}`)
}

func (s *Service) ApplyEdit(edit Edit) (*FullResult, *InvalidationManifest, *Error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, manifest, err := s.Engine.ApplyEdit(edit)
	if err != nil {
		return nil, manifest, nil, err
	}
	s.Project = s.Engine.Project
	s.LastResult = result
	s.LastManifest = manifest
	full, err := BuildFullResult(s.Engine.Project)
	if err != nil {
		return result, manifest, nil, err
	}
	diff := DiffResults(full, result)
	return result, manifest, diff, nil
}

func (s *Service) MarkCancelled(report any) error {
	payload := `{"status":"cancelled"}`
	if report != nil {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		payload = string(data)
	}
	return s.Store.FinishRun(s.RunID, "cancelled", payload)
}

func (s *Service) SimulateForceKill() error {
	return s.Store.Close()
}

func (s *Service) ProcessAndCommitAll(
	ctx context.Context,
	revision int,
	failShardIndex *int,
) (*FullResult, []ShardCommitRecord, []QueueEvent, error) {
	return s.ProcessAndCommitShards(ctx, revision, failShardIndex, nil)
}

func (s *Service) ProcessAllAndCommitPrefix(
	ctx context.Context,
	revision, prefixCount int,
) (*FullResult, []ShardCommitRecord, []QueueEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ValidateProject(s.Project); err != nil {
		return nil, nil, nil, err
	}
	EnsureDialogueHashes(s.Project)
	poolRun, err := RunShardPool(ctx, s.Options.Pool, ShardWorksForProject(s.Project, nil))
	s.QueueEvents = append(s.QueueEvents, poolRun.Events...)
	if err != nil {
		return nil, nil, poolRun.Events, err
	}
	if stageErr := FirstOutcomeError(poolRun.Outcomes); stageErr != nil {
		return nil, nil, poolRun.Events, stageErr
	}
	result, err := BuildResultFromShards(s.Project, poolRun.Outcomes)
	if err != nil {
		return nil, nil, poolRun.Events, err
	}
	committed := []ShardCommitRecord{}
	for _, dr := range result.Dialogues {
		if dr.Dialogue.ShardIndex >= prefixCount {
			continue
		}
		record, err := s.commitDialogueResult(dr, result.Dispatch, revision)
		if err != nil {
			return result, committed, poolRun.Events, err
		}
		committed = append(committed, record)
	}
	s.LastResult = result
	return result, committed, poolRun.Events, nil
}

func (s *Service) ProcessAndCommitShards(
	ctx context.Context,
	revision int,
	failShardIndex *int,
	onlyShards map[int]bool,
) (*FullResult, []ShardCommitRecord, []QueueEvent, error) {
	return s.processAndCommitShards(ctx, revision, failShardIndex, onlyShards, nil)
}

func (s *Service) processAndCommitShards(
	ctx context.Context,
	revision int,
	failShardIndex *int,
	onlyShards map[int]bool,
	existing []ShardOutcome,
) (*FullResult, []ShardCommitRecord, []QueueEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ValidateProject(s.Project); err != nil {
		return nil, nil, nil, err
	}
	EnsureDialogueHashes(s.Project)
	works := ShardWorksForProject(s.Project, failShardIndex)
	if onlyShards != nil {
		filtered := make([]ShardWork, 0, len(onlyShards))
		for _, work := range works {
			if onlyShards[work.ShardIndex] {
				filtered = append(filtered, work)
			}
		}
		works = filtered
	}
	poolRun, err := RunShardPool(ctx, s.Options.Pool, works)
	s.QueueEvents = append(s.QueueEvents, poolRun.Events...)
	if err != nil {
		if ctx.Err() != nil {
			_ = s.MarkCancelled(map[string]any{"reason": "user_cancelled", "error": err.Error()})
		}
		return nil, nil, poolRun.Events, err
	}
	if stageErr := FirstOutcomeError(poolRun.Outcomes); stageErr != nil {
		return nil, nil, poolRun.Events, stageErr
	}

	allOutcomes := append(append([]ShardOutcome{}, existing...), poolRun.Outcomes...)
	result, err := BuildResultFromShards(s.Project, allOutcomes)
	if err != nil {
		return nil, nil, poolRun.Events, err
	}
	committed := []ShardCommitRecord{}
	for _, dr := range result.Dialogues {
		if onlyShards != nil && !onlyShards[dr.Dialogue.ShardIndex] {
			continue
		}
		record, err := s.commitDialogueResult(dr, result.Dispatch, revision)
		if err != nil {
			return result, committed, poolRun.Events, err
		}
		committed = append(committed, record)
	}
	s.LastResult = result
	if s.Engine != nil {
		s.Engine.Snapshot.Result = result
	}
	if onlyShards == nil {
		if err := s.MarkCompleted(); err != nil {
			return result, committed, poolRun.Events, err
		}
	}
	return result, committed, poolRun.Events, nil
}

func (s *Service) commitDialogueResult(
	dr *DialogueResult,
	dispatch *DispatchResult,
	revision int,
) (ShardCommitRecord, error) {
	d := dr.Dialogue
	tempCache := NewCache(nil)
	_, err := warmCaches(s.Project, &FullResult{
		ProjectID: s.Project.ID, Recipe: s.Project.Recipe,
		Dialogues: []*DialogueResult{dr}, Dispatch: dispatch,
	}, tempCache, revision)
	if err != nil {
		return ShardCommitRecord{}, err
	}
	entries := tempCache.EntriesForDialogue(d.ID)
	for _, entry := range entries {
		s.Cache.PutMemory(entry)
	}

	summaries := make([]PackSummaryRecord, 0, len(dr.Packs))
	for _, pack := range dr.Packs {
		summaries = append(summaries, PackSummaryRecord{
			PackID: pack.PackID, Index: pack.Index,
			StartFrame: pack.StartFrame, EndFrame: pack.EndFrame,
			VisibleFrames: pack.VisibleFrames, PhonemeCount: len(pack.PhonemeIDs),
			SummaryHash: pack.SummaryHash,
		})
	}
	frameRecords := BuildShardFrameRecords(dispatch, d.ID)
	checkpoint := &ShardCheckpoint{
		ShardCommitRecord: ShardCommitRecord{
			RunID:            s.RunID,
			ProjectID:        s.Project.ID,
			ShardIndex:       d.ShardIndex,
			DialogueID:       d.ID,
			Revision:         revision,
			ContentHash:      d.ContentHash,
			RecipeVersion:    s.Project.Recipe.RecipeVersion,
			AlgorithmVersion: s.Project.AlgorithmVersion,
			PackCount:        len(dr.Packs),
			FrameCount:       len(dr.Attribution),
		},
		PackSummaryJSON:  string(CanonicalJSON(summaries)),
		FrameRecordsJSON: string(CanonicalJSON(frameRecords)),
		DispatchJSON:     string(CanonicalJSON(dispatch)),
		ResultJSON:       string(CanonicalJSON(dr)),
	}
	seq, err := s.Store.CommitShard(checkpoint, entries)
	if err != nil {
		return ShardCommitRecord{}, err
	}
	checkpoint.CommitSeq = seq
	return checkpoint.ShardCommitRecord, nil
}

func BuildShardFrameRecords(dispatch *DispatchResult, dialogueID string) []ShardFrameRecord {
	if dispatch == nil {
		return []ShardFrameRecord{}
	}
	owner := map[string]string{}
	for _, assignment := range dispatch.Assignments {
		owner[assignment.PackID] = assignment.DialogueID
	}
	records := make([]ShardFrameRecord, 0, len(dispatch.Frames))
	for _, frame := range dispatch.Frames {
		record := ShardFrameRecord{
			Frame: frame.Frame, BudgetLimit: frame.BudgetLimit, BudgetUsed: frame.BudgetUsed,
			ActivePackCount:  frame.ActivePackCount,
			ShardActivePacks: []string{}, Dispatched: []Assignment{},
			Deferred: []DeferralRecord{}, Rejected: []RejectionRecord{},
		}
		for _, assignment := range frame.Dispatched {
			if assignment.DialogueID == dialogueID {
				record.Dispatched = append(record.Dispatched, assignment)
			}
		}
		for _, deferral := range frame.DeferredThisFrame {
			if deferral.DialogueID == dialogueID {
				record.Deferred = append(record.Deferred, deferral)
			}
		}
		for _, rejection := range frame.RejectedThisFrame {
			if rejection.DialogueID == dialogueID {
				record.Rejected = append(record.Rejected, rejection)
			}
		}
		for _, channel := range frame.Channels {
			for _, packID := range channel.PackIDs {
				if owner[packID] == dialogueID {
					record.ShardActivePacks = append(record.ShardActivePacks, packID)
				}
			}
		}
		records = append(records, record)
	}
	return records
}

func InspectAndInvalidateCheckpoints(store *Store, project *Project) (*RecoveryInfo, []ShardCheckpoint, error) {
	info := &RecoveryInfo{ValidShards: []int{}, InvalidatedShards: []ShardInvalidation{}, PendingShards: []int{}}
	latestRun, err := store.LatestRun(project.ID)
	if err != nil {
		return nil, nil, err
	}
	info.LatestRun = latestRun
	if latestRun != nil {
		info.ForcedTermination = latestRun.Status == "running"
		info.UserCancelled = latestRun.Status == "cancelled"
		info.CompletedCleanly = latestRun.Status == "completed"
	}
	checkpoints, err := store.LatestShards(project.ID)
	if err != nil {
		return info, nil, err
	}
	byShard := map[int]*Dialogue{}
	for _, d := range project.Dialogues {
		byShard[d.ShardIndex] = d
	}
	valid := []ShardCheckpoint{}
	for _, cp := range checkpoints {
		d := byShard[cp.ShardIndex]
		reason := ""
		switch {
		case d == nil:
			reason = "shard_not_in_current_project"
		case cp.ContentHash != d.ContentHash:
			reason = fmt.Sprintf("content_hash_mismatch checkpoint=%s current=%s", cp.ContentHash, d.ContentHash)
		case cp.RecipeVersion != project.Recipe.RecipeVersion:
			reason = fmt.Sprintf("recipe_version_mismatch checkpoint=%s current=%s", cp.RecipeVersion, project.Recipe.RecipeVersion)
		case cp.AlgorithmVersion != project.AlgorithmVersion:
			reason = fmt.Sprintf("algorithm_version_mismatch checkpoint=%s current=%s", cp.AlgorithmVersion, project.AlgorithmVersion)
		}
		if reason != "" {
			info.InvalidatedShards = append(info.InvalidatedShards, ShardInvalidation{
				ShardIndex: cp.ShardIndex, DialogueID: cp.DialogueID, Reason: reason,
			})
			if err := store.InvalidateShard(project.ID, cp.ShardIndex, cp.DialogueID); err != nil {
				return info, nil, err
			}
			continue
		}
		info.ValidShards = append(info.ValidShards, cp.ShardIndex)
		valid = append(valid, cp)
	}
	validSet := map[int]bool{}
	for _, shard := range info.ValidShards {
		validSet[shard] = true
	}
	for _, d := range sortedDialogues(project.Dialogues) {
		if !validSet[d.ShardIndex] {
			info.PendingShards = append(info.PendingShards, d.ShardIndex)
		}
	}
	sort.Ints(info.ValidShards)
	sort.Ints(info.PendingShards)
	return info, valid, nil
}

func RecoverAndComplete(
	ctx context.Context,
	project *Project,
	opts ServiceOptions,
) (*Service, *RecoveryInfo, *FullResult, []ShardCommitRecord, []QueueEvent, error) {
	store, err := OpenStore(opts.DBPath)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	info, checkpoints, err := InspectAndInvalidateCheckpoints(store, project)
	if err != nil {
		_ = store.Close()
		return nil, info, nil, nil, nil, err
	}
	if opts.RunID == "" {
		opts.RunID = fmt.Sprintf("recovery-%d", time.Now().UnixNano())
	}
	svc, err := NewServiceWithStore(project, opts, store, true)
	if err != nil {
		_ = store.Close()
		return nil, info, nil, nil, nil, err
	}

	outcomes := []ShardOutcome{}
	recoveredShards := map[int]bool{}
	for _, cp := range checkpoints {
		dr := &DialogueResult{}
		if err := json.Unmarshal([]byte(cp.ResultJSON), dr); err != nil {
			return svc, info, nil, nil, nil, fmt.Errorf("decode checkpoint result shard %d: %w", cp.ShardIndex, err)
		}
		outcomes = append(outcomes, ShardOutcome{
			ShardIndex: cp.ShardIndex, DialogueID: cp.DialogueID,
			WorkerID: -1, Value: dr,
		})
		recoveredShards[cp.ShardIndex] = true
	}

	pendingSet := map[int]bool{}
	for _, shard := range info.PendingShards {
		pendingSet[shard] = true
	}
	result, committed, events, err := svc.processAndCommitShards(ctx, 1, nil, pendingSet, outcomes)
	if err != nil {
		return svc, info, nil, nil, nil, err
	}
	if err := svc.MarkCompleted(); err != nil {
		return svc, info, result, committed, events, err
	}

	records, err := store.ListCommitRecords(project.ID)
	if err != nil {
		return svc, info, result, committed, events, err
	}
	svc.LastResult = result
	return svc, info, result, records, events, nil
}

func CommitOrder(records []ShardCommitRecord) []int {
	out := make([]int, 0, len(records))
	for _, record := range records {
		out = append(out, record.ShardIndex)
	}
	return out
}
