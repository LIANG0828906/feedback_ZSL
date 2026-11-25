package audiofp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type BuildOptions struct {
	ShardFrames          int
	MaxShardRetries      int
	CancelAfterNewShards int
	Extraction           ExtractionOptions
}

type BuildResult struct {
	ProjectID      string            `json:"project_id"`
	Analyses       []AssetAnalysis   `json:"analyses"`
	Commits        []CommitRef       `json:"commits"`
	PlannedCommits []CommitRef       `json:"planned_commits"`
	ReusedShards   int               `json:"reused_shards"`
	NewShards      int               `json:"new_shards"`
	RetriedShards  int               `json:"retried_shards"`
	SnapshotReset  bool              `json:"snapshot_reset"`
	ResetReason    string            `json:"reset_reason,omitempty"`
	Stats          []ExtractionStats `json:"stats"`
	Canceled       bool              `json:"canceled"`
}

func (o BuildOptions) normalized() BuildOptions {
	if o.ShardFrames <= 0 {
		o.ShardFrames = 32
	}
	if o.ShardFrames > 4096 {
		o.ShardFrames = 4096
	}
	if o.MaxShardRetries < 0 {
		o.MaxShardRetries = 0
	}
	return o
}

func SnapshotForProject(projectID string, assets []PCMAsset, r Recipe, shardFrames int) SnapshotMeta {
	ordered := append([]PCMAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	parts := make([]string, 0, len(ordered))
	for _, asset := range ordered {
		parts = append(parts, asset.ID+":"+asset.ContentHash()+":"+itoa(len(asset.Samples))+":"+itoa(asset.SampleRate))
	}
	projectHash := digestString("project|" + strings.Join(parts, "|") + "|recipe=" + RecipeHash(r))
	if shardFrames <= 0 {
		shardFrames = 32
	}
	return SnapshotMeta{
		ProjectID: projectID, ProjectHash: projectHash,
		RecipeVersion: RecipeHash(r), AlgorithmVersion: AlgorithmVersion,
		LayoutVersion: fmt.Sprintf("shard-frames=%d", shardFrames),
	}
}

type buildTask struct {
	index      int
	asset      PCMAsset
	shardNo    int
	firstFrame int
	lastFrame  int
	cacheKey   string
}

type buildTaskResult struct {
	task    buildTask
	commit  ShardCommit
	cached  bool
	stats   []ExtractionStats
	retried bool
	err     error
}

// BuildCheckpoint extracts and commits shards in canonical order. Extraction
// is parallel; checkpoint commits are serialized by Store and by this
// coordinator.
func BuildCheckpoint(ctx context.Context, store *Store, projectID string, assets []PCMAsset,
	r Recipe, mode SnapshotMode, opts BuildOptions) (BuildResult, error) {
	opts = opts.normalized()
	if err := ValidateRecipe(r); err != nil {
		return BuildResult{}, err
	}
	ordered := append([]PCMAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seen := make(map[string]bool, len(ordered))
	for _, asset := range ordered {
		if seen[asset.ID] {
			return BuildResult{}, schemaError("asset_id", "duplicate asset "+asset.ID)
		}
		seen[asset.ID] = true
		if len(asset.Samples) < r.FrameLen {
			return BuildResult{}, parameterError("frame_len", "asset "+asset.ID+" is shorter than one frame")
		}
	}
	meta := SnapshotForProject(projectID, ordered, r, opts.ShardFrames)
	prep, err := store.PrepareSnapshot(meta, mode)
	if err != nil {
		return BuildResult{}, err
	}

	var tasks []buildTask
	seq := 0
	for _, asset := range ordered {
		count := frameCount(len(asset.Samples), r)
		contentHash := asset.ContentHash()
		for first := 0; first < count; first += opts.ShardFrames {
			last := first + opts.ShardFrames
			if last > count {
				last = count
			}
			tasks = append(tasks, buildTask{
				index: seq, asset: asset, shardNo: first / opts.ShardFrames,
				firstFrame: first, lastFrame: last,
				cacheKey: shardCacheKey(contentHash, r, first, last),
			})
			seq++
		}
	}
	result := BuildResult{
		ProjectID: projectID, SnapshotReset: prep.Reset, ResetReason: prep.ResetReason,
	}
	for _, task := range tasks {
		result.PlannedCommits = append(result.PlannedCommits, CommitRef{
			Seq: task.index, AssetID: task.asset.ID, ShardNo: task.shardNo,
			CacheKey: task.cacheKey, FirstFrame: task.firstFrame, LastFrame: task.lastFrame,
		})
	}

	results := make([]*buildTaskResult, len(tasks))
	var misses []buildTask
	for _, task := range tasks {
		expected := ShardCommit{
			ProjectID: projectID, Seq: task.index, AssetID: task.asset.ID, ShardNo: task.shardNo,
			CacheKey: task.cacheKey, ContentHash: task.asset.ContentHash(),
			RecipeVersion: RecipeHash(r), AlgorithmVersion: AlgorithmVersion,
			FirstFrame: task.firstFrame, LastFrame: task.lastFrame,
		}
		if prep.AllowCacheReuse {
			cached, err := store.LoadCachedShard(ctx, expected)
			if err != nil {
				// A corrupt/mismatched persisted shard invalidates the whole
				// snapshot. Rebuild from an empty store to forbid mixing.
				if clearErr := store.ClearAll(); clearErr != nil {
					return result, clearErr
				}
				prep, err = store.PrepareSnapshot(meta, SnapshotFresh)
				if err != nil {
					return result, err
				}
				result.SnapshotReset = true
				result.ResetReason = "persisted shard failed validation: " + err.Error()
				results = make([]*buildTaskResult, len(tasks))
				misses = append([]buildTask(nil), tasks...)
				goto compute
			}
			if cached != nil {
				commit := expected
				commit.Payload = cached.Payload
				results[task.index] = &buildTaskResult{task: task, commit: commit, cached: true}
				continue
			}
		}
		misses = append(misses, task)
	}

compute:
	if len(misses) > 0 {
		if err := runBuildWorkers(ctx, projectID, r, opts, misses, results); err != nil {
			return result, err
		}
	}

	// Commit in canonical sequence order. A failed shard is skipped without
	// poisoning successful shards; sequence gaps remain recoverable on retry.
	var firstErr error
	newCommitted := 0
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			result.Canceled = true
			break
		}
		tr := results[task.index]
		if tr == nil {
			if firstErr == nil {
				firstErr = stageError(task.asset.ID, task.shardNo, task.firstFrame,
					errors.New("shard produced no result"))
			}
			continue
		}
		if tr.err != nil {
			if firstErr == nil {
				firstErr = tr.err
			}
			continue
		}
		if err := store.CommitShard(ctx, tr.commit); err != nil {
			return result, err
		}
		if tr.cached {
			result.ReusedShards++
		} else {
			result.NewShards++
			newCommitted++
		}
		if tr.retried {
			result.RetriedShards++
		}
		if opts.CancelAfterNewShards > 0 && newCommitted >= opts.CancelAfterNewShards {
			result.Canceled = true
			break
		}
	}
	commits, err := store.CommitSequence(projectID)
	if err != nil {
		return result, err
	}
	result.Commits = commits
	if result.Canceled {
		return result, context.Canceled
	}
	if firstErr != nil {
		return result, firstErr
	}
	if len(commits) != len(tasks) {
		return result, stageError("", 0, 0, fmt.Errorf("checkpoint has %d/%d shards", len(commits), len(tasks)))
	}
	for i, c := range commits {
		if c.Seq != i {
			return result, stageError(c.AssetID, c.ShardNo, c.FirstFrame,
				fmt.Errorf("checkpoint sequence gap at %d", i))
		}
	}
	analyses, err := AnalysesFromCommits(commits, results, tasks)
	if err != nil {
		return result, err
	}
	result.Analyses = analyses
	for _, tr := range results {
		if tr != nil {
			result.Stats = append(result.Stats, tr.stats...)
		}
	}
	return result, nil
}

func runBuildWorkers(ctx context.Context, projectID string, r Recipe, opts BuildOptions, tasks []buildTask,
	results []*buildTaskResult) error {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan buildTask)
	var wg sync.WaitGroup
	workers := opts.Extraction.Workers
	if workers <= 0 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers > 32 {
		workers = 32
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				if workerCtx.Err() != nil {
					return
				}
				tr := extractShardWithRetry(workerCtx, projectID, r, opts, task)
				results[task.index] = tr
			}
		}()
	}
	for _, task := range tasks {
		select {
		case jobs <- task:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func extractShardWithRetry(ctx context.Context, projectID string, r Recipe, opts BuildOptions, task buildTask) *buildTaskResult {
	attempts := 1 + opts.MaxShardRetries
	var lastErr error
	var allStats []ExtractionStats
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return &buildTaskResult{task: task, err: ctx.Err()}
		}
		injection := opts.Extraction.Inject
		if injection != nil && injection.Kind == "stage" &&
			(injection.AssetID == "" || injection.AssetID == task.asset.ID) &&
			injection.Shard == task.shardNo && (!injection.FailOnce || attempt == 0) {
			lastErr = stageError(task.asset.ID, task.shardNo, task.firstFrame,
				fmt.Errorf("injected single-shard extraction failure"))
			continue
		}
		extractOpts := opts.Extraction
		if injection != nil {
			copyInjection := *injection
			copyInjection.Shard = task.shardNo
			extractOpts.Inject = &copyInjection
		}
		frames, stats, err := ExtractFrameRange(ctx, task.asset, r, task.firstFrame, task.lastFrame, extractOpts)
		allStats = append(allStats, stats)
		if err != nil {
			lastErr = err
			if CodeOf(err) == CodeOrder || CodeOf(err) == CodeParameter || CodeOf(err) == CodeSchema {
				return &buildTaskResult{task: task, stats: allStats, err: err}
			}
			continue
		}
		contentHash := task.asset.ContentHash()
		payload := ShardPayload{
			AssetID: task.asset.ID, TotalSamples: len(task.asset.Samples),
			FirstFrame: task.firstFrame, LastFrame: task.lastFrame,
			Frames: frames, Index: BuildIndexEntries(task.asset.ID, contentHash, r, frames),
		}
		return &buildTaskResult{
			task: task,
			commit: ShardCommit{
				ProjectID: projectID, Seq: task.index, AssetID: task.asset.ID, ShardNo: task.shardNo,
				CacheKey: task.cacheKey, ContentHash: contentHash, RecipeVersion: RecipeHash(r),
				AlgorithmVersion: AlgorithmVersion, FirstFrame: task.firstFrame, LastFrame: task.lastFrame,
				Payload: payload,
			},
			stats: allStats, retried: attempt > 0,
		}
	}
	if CodeOf(lastErr) == "" {
		lastErr = stageError(task.asset.ID, task.shardNo, task.firstFrame, lastErr)
	}
	return &buildTaskResult{task: task, stats: allStats, err: lastErr}
}

func AnalysesFromCommits(commits []CommitRef, results []*buildTaskResult, tasks []buildTask) ([]AssetAnalysis, error) {
	byAsset := make(map[string]*AssetAnalysis)
	contentByAsset := make(map[string]string)
	recipeHash := ""
	for _, task := range tasks {
		contentByAsset[task.asset.ID] = task.asset.ContentHash()
	}
	for _, c := range commits {
		tr := results[c.Seq]
		if tr == nil || tr.err != nil {
			continue
		}
		assetID := tr.commit.AssetID
		analysis := byAsset[assetID]
		if analysis == nil {
			analysis = &AssetAnalysis{
				AssetID: assetID, ContentHash: contentByAsset[assetID],
				RecipeHash: tr.commit.RecipeVersion, TotalSamples: len(tr.task.asset.Samples),
			}
			byAsset[assetID] = analysis
		}
		analysis.Frames = append(analysis.Frames, tr.commit.Payload.Frames...)
		analysis.Index = append(analysis.Index, tr.commit.Payload.Index...)
		recipeHash = tr.commit.RecipeVersion
	}
	out := make([]AssetAnalysis, 0, len(byAsset))
	for _, analysis := range byAsset {
		analysis.RecipeHash = recipeHash
		out = append(out, *analysis)
	}
	SortAnalyses(out)
	return out, nil
}

// AnalysesFromStore reconstructs analyses from persisted shard payloads without
// invoking extraction.
func AnalysesFromStore(ctx context.Context, store *Store, projectID string) ([]AssetAnalysis, []CommitRef, error) {
	shards, err := store.LoadCommittedShards(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	commits, err := store.CommitSequence(projectID)
	if err != nil {
		return nil, nil, err
	}
	if len(shards) != len(commits) {
		return nil, nil, stageError("", 0, 0, fmt.Errorf("shard/commit count mismatch"))
	}
	byAsset := make(map[string]*AssetAnalysis)
	for i, shard := range shards {
		c := commits[i]
		if c.AssetID != shard.Payload.AssetID || c.CacheKey != shard.CacheKey {
			return nil, nil, DiffError(c.AssetID, c.FirstFrame, "commit_sequence",
				"persisted shard does not match commit row")
		}
		a := byAsset[shard.Payload.AssetID]
		if a == nil {
			a = &AssetAnalysis{
				AssetID: shard.Payload.AssetID, ContentHash: shard.ContentHash,
				RecipeHash: shard.RecipeVersion, TotalSamples: shard.Payload.TotalSamples,
			}
			byAsset[shard.Payload.AssetID] = a
		}
		a.Frames = append(a.Frames, shard.Payload.Frames...)
		a.Index = append(a.Index, shard.Payload.Index...)
	}
	out := make([]AssetAnalysis, 0, len(byAsset))
	for _, a := range byAsset {
		out = append(out, *a)
	}
	SortAnalyses(out)
	return out, commits, nil
}
