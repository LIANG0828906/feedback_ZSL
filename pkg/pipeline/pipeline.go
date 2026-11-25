package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"lipsync.local/prototype/pkg/checkpoint"
	"lipsync.local/prototype/pkg/dependency"
	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/lip"
	"lipsync.local/prototype/pkg/merge"
	"lipsync.local/prototype/pkg/quant"
	"lipsync.local/prototype/pkg/schedule"
	"lipsync.local/prototype/pkg/workpool"
)

type Path string

const (
	PathRealtime Path = "realtime"
	PathResume   Path = "resume"
	PathFull     Path = "full"
)

type FaultKind string

const (
	FaultDecode FaultKind = "decode"
	FaultOrder  FaultKind = "order"
	FaultRate   FaultKind = "rate"
	FaultStage  FaultKind = "stage"
)

type Options struct {
	Path      Path
	Store     *checkpoint.Store
	StorePath string
	Pool      workpool.Pool
	Faults    map[string]FaultKind
}

type ShardResult struct {
	ShardID    string
	DialogueID string
	Quantized  domain.QuantizeResult
	Lip        domain.LipResult
	Dispatch   domain.DispatchResult
	Err        error
	Recovered  bool
}

type Report struct {
	Path           Path
	Shards         []ShardResult
	Artifacts      map[string]dependency.Artifact
	Graph          *dependency.Graph
	GlobalDispatch domain.DispatchResult
	CommitSequence []string
	Recovered      []string
	PoolEvents     []workpool.Event
	Duration       time.Duration
}

func (r *Report) Shard(dialogueID string) (ShardResult, bool) {
	for _, shard := range r.Shards {
		if shard.DialogueID == dialogueID {
			return shard, true
		}
	}
	return ShardResult{}, false
}

func (r *Report) FirstError() error {
	for _, shard := range r.Shards {
		if shard.Err != nil {
			return shard.Err
		}
	}
	return nil
}

// Run executes the complete deterministic pipeline. Realtime and resume use the
// bounded worker pool; full uses a single serial worker. All three paths share
// the same pure quantize/merge/lip/schedule functions and stable commit order.
func Run(ctx context.Context, project *domain.Project, opts Options) (*Report, error) {
	started := time.Now()
	if err := domain.ValidateProject(project); err != nil {
		return nil, err
	}
	if opts.Path == "" {
		opts.Path = PathFull
	}
	ownedStore := false
	if opts.Store == nil && opts.StorePath != "" {
		store, err := checkpoint.Open(opts.StorePath)
		if err != nil {
			return nil, err
		}
		opts.Store = store
		ownedStore = true
	}
	if opts.Store == nil {
		return nil, &domain.Error{Code: domain.ErrStage, Field: "store", Reason: "checkpoint store is required"}
	}
	if ownedStore {
		defer opts.Store.Close()
	}
	if opts.Pool.Workers == 0 {
		opts.Pool = workpool.Pool{Workers: 3, PerWorkerBuffer: 1, QueueBudget: 8}
	}
	dialogues := domain.SortedDialogues(project.Dialogues)
	shards := make([]ShardResult, len(dialogues))
	expectedHashes := make(map[string]string, len(dialogues))
	for i, d := range dialogues {
		shardID := fmt.Sprintf("shard-%03d", i)
		shards[i] = ShardResult{ShardID: shardID, DialogueID: d.ID}
		expectedHashes[shardID] = d.FullContentHash(project.Recipe)
	}
	recovered := make(map[string]checkpoint.Checkpoint)
	if opts.Path == PathResume {
		cp, err := opts.Store.Recover(ctx, expectedHashes, project.Recipe.Version, project.Recipe.AlgorithmVersion)
		if err != nil {
			return nil, err
		}
		for _, item := range cp {
			recovered[item.ShardID] = item
		}
	}

	var results []error
	var events []workpool.Event
	var poolErr error
	if opts.Path == PathFull {
		results = make([]error, len(dialogues))
		for i, d := range dialogues {
			results[i] = processDialogue(ctx, shards[i].ShardID, d, project.Recipe, opts.Faults)
			if results[i] == nil {
				results[i] = fillShard(ctx, &shards[i], d, project.Recipe)
			}
		}
	} else {
		ids := make([]string, len(dialogues))
		for i := range dialogues {
			ids[i] = shards[i].ShardID
		}
		results, events, poolErr = opts.Pool.Run(ctx, ids, func(ctx context.Context, index int, shardID string) error {
			if err := processDialogue(ctx, shardID, dialogues[index], project.Recipe, opts.Faults); err != nil {
				return err
			}
			return fillShard(ctx, &shards[index], dialogues[index], project.Recipe)
		})
	}
	if poolErr != nil {
		return nil, &domain.Error{Code: domain.ErrStage, Field: "worker_pool", Reason: poolErr.Error(), Cause: poolErr}
	}
	for i := range shards {
		if results[i] != nil {
			shards[i].Err = normalizeShardError(shards[i].ShardID, shards[i].DialogueID, results[i])
		}
	}
	allPackages := make([]domain.LipPackage, 0)
	for i := range shards {
		if shards[i].Err == nil {
			allPackages = append(allPackages, shards[i].Lip.Packages...)
		}
	}
	globalDispatch, err := schedule.Dispatch(allPackages, schedule.ConfigFromRecipe(project.Recipe))
	if err != nil {
		return nil, err
	}
	artifacts := make(map[string]dependency.Artifact)
	for i := range shards {
		if shards[i].Err != nil {
			continue
		}
		shards[i].Dispatch = schedule.ForDialogue(globalDispatch, shards[i].DialogueID)
		artifacts[shards[i].DialogueID] = dependency.Artifact{
			Dialogue: dialogues[i], Quantized: shards[i].Quantized,
			Lip: shards[i].Lip, Dispatch: shards[i].Dispatch,
		}
	}

	for i := range shards {
		if shards[i].Err != nil {
			continue
		}
		if recoveredCP, ok := recovered[shards[i].ShardID]; ok {
			if err := verifyRecoveredDispatch(shards[i], recoveredCP); err != nil {
				return nil, err
			}
			shards[i].Recovered = true
			continue
		}
		dispatchJSON, err := json.Marshal(shards[i].Dispatch)
		if err != nil {
			shards[i].Err = &domain.Error{
				Code: domain.ErrStage, ShardID: shards[i].ShardID, DialogueID: shards[i].DialogueID,
				Field: "dispatch_json", Reason: "marshal dispatch", Cause: err,
			}
			continue
		}
		summary := make([]string, 0, len(shards[i].Lip.Packages))
		for _, pkg := range shards[i].Lip.Packages {
			summary = append(summary, pkg.Summary())
		}
		cp := checkpoint.Checkpoint{
			ShardID: shards[i].ShardID, DialogueID: shards[i].DialogueID,
			PackageSummary: summary, Dispatch: dispatchJSON,
			ContentHash:   expectedHashes[shards[i].ShardID],
			RecipeVersion: project.Recipe.Version, AlgorithmVersion: project.Recipe.AlgorithmVersion,
		}
		if err := opts.Store.CommitCheckpoint(ctx, cp); err != nil {
			shards[i].Err = normalizeShardError(shards[i].ShardID, shards[i].DialogueID, err)
		}
	}
	committed, err := opts.Store.Committed(ctx, project.Recipe.Version, project.Recipe.AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	commitSeq := make([]string, 0, len(committed))
	for _, cp := range committed {
		commitSeq = append(commitSeq, fmt.Sprintf("%s#%d", cp.ShardID, cp.CommitSeq))
	}
	graph, err := dependency.Build(project, artifacts)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Path: opts.Path, Shards: shards, Artifacts: artifacts,
		Graph: graph, GlobalDispatch: globalDispatch,
		CommitSequence: commitSeq, PoolEvents: events,
		Duration: time.Since(started),
	}
	for _, shard := range shards {
		if shard.Recovered {
			report.Recovered = append(report.Recovered, shard.ShardID)
		}
	}
	sort.Strings(report.Recovered)
	return report, nil
}

func processDialogue(ctx context.Context, shardID string, d domain.Dialogue, recipe domain.Recipe, faults map[string]FaultKind) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &domain.Error{
				Code: domain.ErrStage, ShardID: shardID, DialogueID: d.ID,
				Field: "worker", Reason: fmt.Sprintf("panic: %v", p),
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return &domain.Error{Code: domain.ErrStage, ShardID: shardID, DialogueID: d.ID, Field: "context", Reason: err.Error(), Cause: err}
	}
	switch faults[d.ID] {
	case FaultDecode:
		return &domain.Error{Code: domain.ErrDecode, ShardID: shardID, DialogueID: d.ID, Field: "pcm", Reason: "injected corrupt dialogue"}
	case FaultOrder:
		return &domain.Error{Code: domain.ErrOrder, ShardID: shardID, DialogueID: d.ID, Field: "phoneme_timing", Reason: "injected reversed phoneme timing"}
	case FaultRate:
		return &domain.Error{Code: domain.ErrRate, ShardID: shardID, DialogueID: d.ID, Field: "frame_rate", Reason: "injected unsupported frame rate"}
	case FaultStage:
		return &domain.Error{Code: domain.ErrStage, ShardID: shardID, DialogueID: d.ID, Field: "shard_stage", Reason: "injected shard failure"}
	}
	return nil
}

func fillShard(ctx context.Context, shard *ShardResult, d domain.Dialogue, recipe domain.Recipe) error {
	if err := ctx.Err(); err != nil {
		return &domain.Error{Code: domain.ErrStage, ShardID: shard.ShardID, DialogueID: d.ID, Field: "context", Reason: err.Error(), Cause: err}
	}
	q, err := quant.QuantizeDialogue(d, recipe.FrameRate, recipe.Quantize)
	if err != nil {
		return err
	}
	packages, err := merge.Merge(d.ID, d.Channel, q.Phonemes, merge.Config{
		AffinityThreshold: recipe.AffinityThreshold, MaxPhonemesPerPackage: recipe.MaxPhonemesPerPackage,
	})
	if err != nil {
		return err
	}
	lipResult, err := lip.RenderFrames(d.ID, q.Phonemes, packages, recipe.TransitionTolerance)
	if err != nil {
		return err
	}
	shard.Quantized = q
	shard.Lip = lipResult
	return nil
}

func normalizeShardError(shardID, dialogueID string, err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if ok := asDomainError(err, &de); ok {
		if de.ShardID == "" {
			de.ShardID = shardID
		}
		if de.DialogueID == "" {
			de.DialogueID = dialogueID
		}
		return de
	}
	return &domain.Error{
		Code: domain.ErrStage, ShardID: shardID, DialogueID: dialogueID,
		Field: "shard_stage", Reason: err.Error(), Cause: err,
	}
}

func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func verifyRecoveredDispatch(shard ShardResult, cp checkpoint.Checkpoint) error {
	recomputed, err := json.Marshal(shard.Dispatch)
	if err != nil {
		return &domain.Error{
			Code: domain.ErrStage, ShardID: shard.ShardID, DialogueID: shard.DialogueID,
			Field: "dispatch_json", Reason: "marshal recomputed dispatch", Cause: err,
		}
	}
	var expected, actual any
	if err := json.Unmarshal(recomputed, &expected); err != nil {
		return err
	}
	if err := json.Unmarshal(cp.Dispatch, &actual); err != nil {
		return &domain.Error{
			Code: domain.ErrDecode, ShardID: shard.ShardID, DialogueID: shard.DialogueID,
			Field: "dispatch_json", Reason: "decode recovered dispatch", Cause: err,
		}
	}
	if !jsonEqual(expected, actual) {
		return &domain.Error{
			Code: domain.ErrDiff, ShardID: shard.ShardID, DialogueID: shard.DialogueID,
			Field: "dispatch_json", Reason: "recovered dispatch differs from recomputation",
		}
	}
	return nil
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
