package vpf

import (
	"context"
	"fmt"
	"sort"
)

type CommitInfo struct {
	ChunkIndex  int    `json:"chunk_index"`
	ChunkID     string `json:"chunk_id"`
	CommitSeq   int64  `json:"commit_seq"`
	CacheKey    string `json:"cache_key"`
	PixelDigest string `json:"pixel_digest"`
	Source      string `json:"source"`
	Attempts    int    `json:"attempts,omitempty"`
	WorkerID    int    `json:"worker_id,omitempty"`
}

type RunOptions struct {
	Path             string
	ChunkIndices     []int
	ReuseStored      bool
	FailBeforeRender func(chunkIndex, attempt int) error
}

type RunResult struct {
	Path           string           `json:"path"`
	FrameIndex     int              `json:"frame_index"`
	Frame          *FrameBuffer     `json:"-"`
	FrameDigest    string           `json:"frame_digest"`
	Commits        []CommitInfo     `json:"commits"`
	ReusedChunks   []int            `json:"reused_chunks"`
	RenderedChunks []int            `json:"rendered_chunks"`
	FailedChunks   []int            `json:"failed_chunks,omitempty"`
	Scheduler      *SchedulerReport `json:"scheduler,omitempty"`
	Outcomes       []ScheduledChunk `json:"-"`
}

type Runner struct {
	Project    *Project
	FrameIndex int
	Store      CheckpointStore
	Scheduler  *BoundedScheduler
}

func (r *Runner) Build(ctx context.Context, options RunOptions) (*RunResult, error) {
	if err := r.Store.Init(ctx); err != nil {
		return nil, err
	}
	indices := options.ChunkIndices
	if len(indices) == 0 {
		indices = make([]int, r.Project.ChunkCount())
		for i := range indices {
			indices[i] = i
		}
	}
	sort.Ints(indices)

	result := &RunResult{Path: options.Path, FrameIndex: r.FrameIndex}
	expectedKey := map[int]string{}
	chunkResults := map[int]*ChunkResult{}
	var toRender []int

	frame, ok := r.Project.FrameByIndex(r.FrameIndex)
	if !ok {
		return nil, schemaError(r.Project.SourceVersion, "$.frames", "frame %d not found", r.FrameIndex)
	}
	for _, index := range indices {
		bounds := r.Project.TileRect(index)
		key, _, _, _ := ComputeChunkCacheKey(r.Project, frame, index, bounds)
		expectedKey[index] = key
		if options.ReuseStored {
			record, found, err := r.Store.GetCheckpoint(ctx, r.FrameIndex, index)
			if err != nil {
				return result, err
			}
			if found && record.CacheKey == key && record.PixelDigest == SHA256Hex(record.Pix) && len(record.Pix) == bounds.W*bounds.H*4 {
				chunkResult := &ChunkResult{
					FrameIndex: r.FrameIndex, ChunkIndex: index, Bounds: bounds,
					Pix: record.Pix, PixelDigest: record.PixelDigest, CacheKey: record.CacheKey,
					ContentHash: record.ContentHash, ParamVersion: record.ParamVersion,
					PixelCount: bounds.W * bounds.H,
				}
				chunkResults[index] = chunkResult
				result.ReusedChunks = append(result.ReusedChunks, index)
				result.Commits = append(result.Commits, CommitInfo{
					ChunkIndex: index, ChunkID: record.ChunkID, CommitSeq: record.CommitSeq,
					CacheKey: record.CacheKey, PixelDigest: record.PixelDigest, Source: "persisted_reuse",
				})
				continue
			}
		}
		toRender = append(toRender, index)
	}

	outcomes, scheduleReport, err := r.Scheduler.Run(ctx, toRender, func(index, workerID int, buffer *PixelBuffer, attempt int) (*ChunkResult, error) {
		if options.FailBeforeRender != nil {
			if failErr := options.FailBeforeRender(index, attempt); failErr != nil {
				return nil, failErr
			}
		}
		chunk, renderErr := RenderChunkInto(r.Project, r.FrameIndex, index, buffer)
		if renderErr != nil {
			return nil, renderErr
		}
		chunk.WorkerID = workerID
		chunk.Attempts = attempt
		if chunk.CacheKey != expectedKey[index] {
			return nil, &Error{Code: CodeChunk, Message: "cache key mismatch after render", FieldPath: fmt.Sprintf("$.chunks[%d].cache_key", index)}
		}
		return chunk, nil
	})
	if err != nil {
		result.Scheduler = scheduleReport
		return result, err
	}
	result.Scheduler = scheduleReport
	result.Outcomes = outcomes

	var commitErr error
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			result.FailedChunks = append(result.FailedChunks, outcome.Index)
			commitErr = outcome.Err
			continue
		}
		chunk := outcome.Result
		record := CheckpointRecord{
			FrameIndex: r.FrameIndex, ChunkIndex: chunk.ChunkIndex,
			ChunkID: ChunkID(r.FrameIndex, chunk.ChunkIndex), Bounds: chunk.Bounds,
			Pix: chunk.Pix, PixelDigest: chunk.PixelDigest,
			StackSnapshotVersion: r.Project.StackSnapshotVersion,
			RippleSourceVersion:  r.Project.RippleSourceVersion,
			CacheKey:             chunk.CacheKey, ContentHash: chunk.ContentHash,
			ParamVersion: chunk.ParamVersion, AlgorithmVersion: AlgorithmVersion,
			PixelCount: chunk.PixelCount,
		}
		seq, putErr := r.Store.PutCheckpoint(ctx, record)
		if putErr != nil {
			commitErr = putErr
			result.FailedChunks = append(result.FailedChunks, chunk.ChunkIndex)
			continue
		}
		chunkResults[chunk.ChunkIndex] = chunk
		result.RenderedChunks = append(result.RenderedChunks, chunk.ChunkIndex)
		result.Commits = append(result.Commits, CommitInfo{
			ChunkIndex: chunk.ChunkIndex, ChunkID: record.ChunkID, CommitSeq: seq,
			CacheKey: chunk.CacheKey, PixelDigest: chunk.PixelDigest, Source: "rendered_committed",
			Attempts: outcome.Attempts, WorkerID: outcome.WorkerID,
		})
	}
	sort.Ints(result.FailedChunks)
	sort.Ints(result.ReusedChunks)
	sort.Ints(result.RenderedChunks)
	sort.Slice(result.Commits, func(i, j int) bool { return result.Commits[i].ChunkIndex < result.Commits[j].ChunkIndex })

	if len(result.FailedChunks) > 0 {
		return result, &Error{Code: CodeChunk, Message: fmt.Sprintf("failed chunks %v", result.FailedChunks), Details: commitErr.Error()}
	}

	if len(indices) == r.Project.ChunkCount() {
		chunksForAssembly := map[int]*ChunkResult{}
		for _, index := range indices {
			chunk, ok := chunkResults[index]
			if !ok {
				return result, &Error{Code: CodeChunk, Message: "chunk unavailable for assembly", FieldPath: fmt.Sprintf("$.chunks[%d]", index)}
			}
			chunksForAssembly[index] = chunk
		}
		frameBuffer, assembleErr := AssembleFrame(r.Project, r.FrameIndex, chunksForAssembly)
		if assembleErr != nil {
			return result, assembleErr
		}
		result.Frame = frameBuffer
		result.FrameDigest = FrameDigest(frameBuffer)
	}
	return result, nil
}

type CommitSequenceItem struct {
	ChunkIndex  int    `json:"chunk_index"`
	CommitSeq   int64  `json:"commit_seq"`
	CacheKey    string `json:"cache_key"`
	PixelDigest string `json:"pixel_digest"`
}

func (r *RunResult) Sequence() []CommitSequenceItem {
	out := make([]CommitSequenceItem, 0, len(r.Commits))
	for _, commit := range r.Commits {
		out = append(out, CommitSequenceItem{
			ChunkIndex: commit.ChunkIndex, CommitSeq: commit.CommitSeq,
			CacheKey: commit.CacheKey, PixelDigest: commit.PixelDigest,
		})
	}
	return out
}

type ThreePathReport struct {
	RealtimePath      *RunResult           `json:"-"`
	RecoveryPath      *RunResult           `json:"-"`
	FullPath          *RunResult           `json:"-"`
	RealtimeDigest    string               `json:"realtime_digest"`
	RecoveryDigest    string               `json:"recovery_digest"`
	FullDigest        string               `json:"full_digest"`
	PixelIdentical    bool                 `json:"pixel_identical"`
	FirstMismatch     *PixelMismatch       `json:"first_mismatch,omitempty"`
	SequenceIdentical bool                 `json:"sequence_identical"`
	RealtimeSequence  []CommitSequenceItem `json:"realtime_sequence"`
	RecoverySequence  []CommitSequenceItem `json:"recovery_sequence"`
	FullSequence      []CommitSequenceItem `json:"full_sequence"`
	RecoveryReused    []int                `json:"recovery_reused"`
}

// VerifyThreePaths runs realtime, interrupted-recovery, and one-shot full
// recomputation against independent checkpoint stores.
func VerifyThreePaths(ctx context.Context, project *Project, frameIndex int, newStore func() CheckpointStore) (*ThreePathReport, error) {
	realtimeStore := newStore()
	realtimeRunner := &Runner{Project: project, FrameIndex: frameIndex, Store: realtimeStore, Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 4, QueueBudget: 1, MaxBufferedPixels: 2048, Retries: 1})}
	realtime, err := realtimeRunner.Build(ctx, RunOptions{Path: "realtime", ReuseStored: true})
	if err != nil {
		return nil, fmt.Errorf("realtime path: %w", err)
	}

	recoveryStore := newStore()
	prefixCount := minInt(2, project.ChunkCount())
	prefixRunner := &Runner{Project: project, FrameIndex: frameIndex, Store: recoveryStore, Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 1, QueueBudget: 1, MaxBufferedPixels: 2048})}
	prefix, err := prefixRunner.Build(ctx, RunOptions{Path: "simulated-crash-prefix", ChunkIndices: []int{0, 1}[:prefixCount], ReuseStored: false})
	if err != nil {
		return nil, fmt.Errorf("simulated breakpoint: %w", err)
	}
	if len(prefix.Commits) != prefixCount {
		return nil, fmt.Errorf("simulated breakpoint committed %d chunks, want %d", len(prefix.Commits), prefixCount)
	}
	recoveryRunner := &Runner{Project: project, FrameIndex: frameIndex, Store: recoveryStore, Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 3, QueueBudget: 2, MaxBufferedPixels: 2048, Retries: 1})}
	recovery, err := recoveryRunner.Build(ctx, RunOptions{Path: "breakpoint-recovery", ReuseStored: true})
	if err != nil {
		return nil, fmt.Errorf("recovery path: %w", err)
	}

	fullStore := newStore()
	fullRunner := &Runner{Project: project, FrameIndex: frameIndex, Store: fullStore, Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 1, QueueBudget: 4, MaxBufferedPixels: 2048, Retries: 0})}
	full, err := fullRunner.Build(ctx, RunOptions{Path: "one-shot-full-recompute", ReuseStored: false})
	if err != nil {
		return nil, fmt.Errorf("full recompute path: %w", err)
	}

	report := &ThreePathReport{
		RealtimePath: realtime, RecoveryPath: recovery, FullPath: full,
		RealtimeDigest: realtime.FrameDigest, RecoveryDigest: recovery.FrameDigest, FullDigest: full.FrameDigest,
		RealtimeSequence: realtime.Sequence(), RecoverySequence: recovery.Sequence(), FullSequence: full.Sequence(),
		RecoveryReused: recovery.ReusedChunks,
	}
	report.FirstMismatch = CompareFrames(project, realtime.Frame, recovery.Frame)
	if report.FirstMismatch == nil {
		report.FirstMismatch = CompareFrames(project, realtime.Frame, full.Frame)
	}
	report.PixelIdentical = report.FirstMismatch == nil && realtime.FrameDigest == recovery.FrameDigest && realtime.FrameDigest == full.FrameDigest
	report.SequenceIdentical = equalCommitSequences(report.RealtimeSequence, report.RecoverySequence) &&
		equalCommitSequences(report.RealtimeSequence, report.FullSequence)
	if !report.PixelIdentical {
		return report, &Error{Code: CodeChunk, Message: "three synthesis paths produced different pixels", Details: report.FirstMismatch}
	}
	if !report.SequenceIdentical {
		return report, &Error{Code: CodeChunk, Message: "three synthesis paths produced different checkpoint sequences"}
	}
	return report, nil
}

func equalCommitSequences(a, b []CommitSequenceItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type IncrementalReport struct {
	Edit             Change            `json:"edit"`
	Plan             *InvalidationPlan `json:"plan"`
	InitialDigest    string            `json:"initial_digest"`
	FinalDigest      string            `json:"final_digest"`
	ReusedChunks     []int             `json:"reused_chunks"`
	RenderedChunks   []int             `json:"rendered_chunks"`
	UnaffectedStable bool              `json:"unaffected_stable"`
	StableChunks     []int             `json:"stable_chunks"`
	ChangedChunks    []int             `json:"changed_chunks"`
	Run              *RunResult        `json:"-"`
}

// VerifyIncrementalEdit builds once, applies edit, propagates invalidation,
// and verifies that unaffected persisted checkpoint rows are unchanged.
func VerifyIncrementalEdit(ctx context.Context, project *Project, frameIndex int, store CheckpointStore, editFn func(*Project) (Change, error)) (*IncrementalReport, error) {
	runner := &Runner{Project: project, FrameIndex: frameIndex, Store: store, Scheduler: NewBoundedScheduler(DefaultSchedulerOptions())}
	initial, err := runner.Build(ctx, RunOptions{Path: "initial", ReuseStored: true})
	if err != nil {
		return nil, err
	}
	before := map[int]CheckpointRecord{}
	for i := 0; i < project.ChunkCount(); i++ {
		record, found, err := store.GetCheckpoint(ctx, frameIndex, i)
		if err != nil || !found {
			return nil, fmt.Errorf("initial checkpoint %d missing: %w", i, err)
		}
		before[i] = record
	}
	oldGraph, err := BuildDependencyGraph(project, frameIndex)
	if err != nil {
		return nil, err
	}

	change, err := editFn(project)
	if err != nil {
		return nil, err
	}
	newGraph, err := BuildDependencyGraph(project, frameIndex)
	if err != nil {
		return nil, err
	}
	oldPlan := oldGraph.Invalidate(change.DependencyID)
	newPlan := newGraph.Invalidate(change.DependencyID)
	if len(newPlan.UnknownChanges) > 0 && len(oldPlan.UnknownChanges) > 0 {
		return nil, &Error{Code: CodeChunk, Message: "edit dependency is absent from graph", Details: newPlan.UnknownChanges}
	}
	plan := mergeInvalidationPlans(oldPlan, newPlan, project.ChunkCount())

	after, err := runner.Build(ctx, RunOptions{Path: "incremental", ReuseStored: true})
	if err != nil {
		return nil, err
	}
	report := &IncrementalReport{
		Edit: change, Plan: plan, InitialDigest: initial.FrameDigest, FinalDigest: after.FrameDigest,
		ReusedChunks: after.ReusedChunks, RenderedChunks: after.RenderedChunks, Run: after,
	}
	for _, chunkIndex := range plan.ReusableChunks {
		old := before[chunkIndex]
		current, found, err := store.GetCheckpoint(ctx, frameIndex, chunkIndex)
		if err != nil || !found {
			return report, fmt.Errorf("reused checkpoint %d missing after edit: %w", chunkIndex, err)
		}
		if current.CommitSeq != old.CommitSeq || current.CacheKey != old.CacheKey || current.PixelDigest != old.PixelDigest {
			report.UnaffectedStable = false
			return report, &Error{Code: CodeChunk, Message: "unaffected chunk was rewritten", FieldPath: fmt.Sprintf("$.chunks[%d]", chunkIndex)}
		}
		report.StableChunks = append(report.StableChunks, chunkIndex)
	}
	for _, chunkIndex := range plan.InvalidChunks {
		current, found, err := store.GetCheckpoint(ctx, frameIndex, chunkIndex)
		if err != nil || !found {
			return report, fmt.Errorf("invalid chunk %d not committed: %w", chunkIndex, err)
		}
		if current.CacheKey == before[chunkIndex].CacheKey {
			return report, &Error{Code: CodeChunk, Message: "edited chunk cache key did not change", FieldPath: fmt.Sprintf("$.chunks[%d].cache_key", chunkIndex)}
		}
		report.ChangedChunks = append(report.ChangedChunks, chunkIndex)
	}
	report.UnaffectedStable = true
	return report, nil
}

func mergeInvalidationPlans(a, b *InvalidationPlan, chunkCount int) *InvalidationPlan {
	invalidSet := map[int]bool{}
	reasons := map[int]string{}
	visited := map[string]bool{}
	for _, plan := range []*InvalidationPlan{a, b} {
		if plan == nil {
			continue
		}
		for _, chunk := range plan.InvalidChunks {
			invalidSet[chunk] = true
			if reason := plan.ReasonByChunk[chunk]; reason != "" {
				reasons[chunk] = reason
			}
		}
		for _, node := range plan.VisitedNodes {
			visited[node] = true
		}
	}
	invalid := make([]int, 0, len(invalidSet))
	for chunk := range invalidSet {
		invalid = append(invalid, chunk)
	}
	sort.Ints(invalid)
	reusable := make([]int, 0, chunkCount-len(invalid))
	for chunk := 0; chunk < chunkCount; chunk++ {
		if !invalidSet[chunk] {
			reusable = append(reusable, chunk)
		}
	}
	visitedNodes := make([]string, 0, len(visited))
	for node := range visited {
		visitedNodes = append(visitedNodes, node)
	}
	sort.Strings(visitedNodes)
	return &InvalidationPlan{
		Changed:      append(append([]string(nil), a.Changed...), b.Changed...),
		VisitedNodes: visitedNodes, InvalidChunks: invalid,
		ReusableChunks: reusable, ReasonByChunk: reasons,
	}
}
