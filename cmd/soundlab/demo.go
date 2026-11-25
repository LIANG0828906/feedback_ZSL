package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"soundlab/audiofp"
)

type DemoReport struct {
	OK      bool          `json:"ok"`
	Seed    int64         `json:"seed"`
	Batches []BatchReport `json:"batches"`
	Faults  []FaultReport `json:"faults"`
	Reports string        `json:"report_file"`
}

type BatchReport struct {
	BatchID              string                 `json:"batch_id"`
	Assets               int                    `json:"assets"`
	Edits                int                    `json:"edits"`
	Queries              int                    `json:"queries"`
	Frames               int                    `json:"frames"`
	Invalidations        []audiofp.Invalidation `json:"invalidations"`
	ReusedFrames         int                    `json:"incremental_reused_frames"`
	RecomputedFrames     int                    `json:"incremental_recomputed_frames"`
	IncrementalCacheHits int                    `json:"incremental_cache_hits"`
	IncrementalCacheNew  int                    `json:"incremental_cache_new"`
	QueueWaitEvents      int                    `json:"queue_wait_events"`
	ResumeFirstCommitted int                    `json:"resume_first_committed"`
	ResumeReusedShards   int                    `json:"resume_reused_shards"`
	VersionResetDetected bool                   `json:"version_reset_detected"`
	VersionResetReused   int                    `json:"version_reset_reused_shards"`
	DependencyNodes      int                    `json:"dependency_nodes"`
	TopologicalNodes     int                    `json:"topological_nodes"`
	Verified             audiofp.VerifyReport   `json:"verified"`
}

type FaultReport struct {
	Kind                 string `json:"kind"`
	ExpectedCode         string `json:"expected_code"`
	ActualCode           string `json:"actual_code"`
	AssetID              string `json:"asset_id,omitempty"`
	Shard                int    `json:"shard,omitempty"`
	Frame                int    `json:"frame,omitempty"`
	CommittedBeforeRetry int    `json:"committed_before_retry,omitempty"`
	ReusedAfterRetry     int    `json:"reused_after_retry,omitempty"`
	OK                   bool   `json:"ok"`
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	samples := fs.String("samples", "samples", "sample directory")
	artifacts := fs.String("artifacts", "artifacts/demo", "output directory")
	workers := fs.Int("workers", 4, "extraction worker count")
	buffer := fs.Int("buffer-samples", 8192, "global sampling buffer budget in samples")
	shardFrames := fs.Int("shard-frames", 24, "frames per checkpoint shard")
	resumeAfter := fs.Int("resume-after", 2, "cancel checkpoint build after N new shards")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.RemoveAll(*artifacts); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*artifacts, 0o755); err != nil {
		return fail(err)
	}
	manifests, err := filepath.Glob(filepath.Join(*samples, "batch-*", "project.json"))
	if err != nil || len(manifests) == 0 {
		return fail(fmt.Errorf("no generated batches under %s", *samples))
	}
	sort.Strings(manifests)
	report := DemoReport{Reports: filepath.Join(*artifacts, "report.json")}
	for i, manifest := range manifests {
		batchReport, err := demoBatch(manifest, *artifacts, *workers, *buffer, *shardFrames, *resumeAfter)
		if err != nil {
			report.OK = false
			_ = writeReport(report.Reports, report)
			return fail(err)
		}
		report.Seed = batchReportSeed(manifest)
		report.Batches = append(report.Batches, *batchReport)
		if i == 0 {
			faults, err := runFaults(manifest, *artifacts, *workers, *buffer)
			if err != nil {
				return fail(err)
			}
			report.Faults = faults
			if !faultsOK(faults) {
				report.OK = false
				_ = writeReport(report.Reports, report)
				return fail(fmt.Errorf("fault injection expectations were not met"))
			}
		}
	}
	report.OK = true
	if err := writeReport(report.Reports, report); err != nil {
		return fail(err)
	}
	printJSON(report)
	return 0
}

func batchReportSeed(manifest string) int64 {
	project, err := audiofp.LoadProject(manifest)
	if err != nil {
		return 0
	}
	return project.Manifest.Seed
}

func demoBatch(manifest, artifacts string, workers, bufferSamples, shardFrames, resumeAfter int) (*BatchReport, error) {
	ctx := context.Background()
	project, err := audiofp.LoadProject(manifest)
	if err != nil {
		return nil, err
	}
	finalAssets, finalRecipe, events, err := project.ApplyEdits()
	if err != nil {
		return nil, err
	}
	if err := project.ValidateTruthAgainstFinal(finalAssets, finalRecipe); err != nil {
		return nil, err
	}
	initial := assetsFromMap(project.Initial)
	opts := audiofp.ExtractionOptions{
		Workers: workers, SamplingBufferPerWorker: 64 * 1024,
		MaxSamplingBufferSamples: bufferSamples,
	}

	// Incremental in-memory path.
	session, err := audiofp.NewIncrementalSession(ctx, initial, project.Manifest.Recipe, opts)
	if err != nil {
		return nil, err
	}
	var invalidations []audiofp.Invalidation
	for _, op := range project.Manifest.Edits {
		inv, err := session.Apply(ctx, op, project.Replacements)
		if err != nil {
			return nil, err
		}
		invalidations = append(invalidations, inv...)
	}
	incrementalAnalyses := session.Analyses()

	// Independent full extraction.
	fullAnalyses, fullStats, err := audiofp.AnalyzeAssets(ctx, finalAssets, finalRecipe, opts)
	if err != nil {
		return nil, err
	}
	truthAnalyses := analysesFromTruth(project.Manifest.Truth, finalRecipe)
	if err := audiofp.CompareAnalyses(truthAnalyses, fullAnalyses); err != nil {
		return nil, err
	}
	if err := audiofp.CompareAnalyses(fullAnalyses, incrementalAnalyses); err != nil {
		return nil, err
	}
	if _, err := compareQueries(ctx, project, fullAnalyses, incrementalAnalyses); err != nil {
		return nil, err
	}

	projectID := project.Manifest.BatchID
	batchDir := filepath.Join(artifacts, projectID)
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		return nil, err
	}

	// Persistent cache: build the initial state, edit the recipe/material, then
	// rebuild the final snapshot in Incremental mode so unchanged shards hit.
	incStore, err := audiofp.OpenStore(filepath.Join(batchDir, "incremental.db"))
	if err != nil {
		return nil, err
	}
	defer incStore.Close()
	buildOpts := audiofp.BuildOptions{
		ShardFrames: shardFrames, Extraction: opts,
	}
	if _, err := audiofp.BuildCheckpoint(ctx, incStore, projectID, initial, project.Manifest.Recipe,
		audiofp.SnapshotFresh, buildOpts); err != nil {
		return nil, err
	}
	incBuild, err := audiofp.BuildCheckpoint(ctx, incStore, projectID, finalAssets, finalRecipe,
		audiofp.SnapshotIncremental, buildOpts)
	if err != nil {
		return nil, err
	}
	if err := audiofp.CompareAnalyses(fullAnalyses, incBuild.Analyses); err != nil {
		return nil, err
	}
	if err := compareBuildQueries(project, fullAnalyses, incBuild.Analyses, finalRecipe); err != nil {
		return nil, err
	}

	// Full SQLite rebuild from an empty database.
	fullStore, err := audiofp.OpenStore(filepath.Join(batchDir, "full.db"))
	if err != nil {
		return nil, err
	}
	defer fullStore.Close()
	fullBuild, err := audiofp.BuildCheckpoint(ctx, fullStore, projectID, finalAssets, finalRecipe,
		audiofp.SnapshotFresh, buildOpts)
	if err != nil {
		return nil, err
	}
	if err := audiofp.CompareBuildResults(fullBuild, incBuild); err != nil {
		return nil, err
	}
	if err := compareBuildQueries(project, incBuild.Analyses, fullBuild.Analyses, finalRecipe); err != nil {
		return nil, err
	}
	verifyReport, err := audiofp.VerifyPersistedSnapshot(ctx, fullStore, projectID, finalRecipe, project.Manifest.Truth)
	if err != nil {
		return nil, err
	}

	// Cancellation and resume path.
	resumeStore, err := audiofp.OpenStore(filepath.Join(batchDir, "resume.db"))
	if err != nil {
		return nil, err
	}
	cutoff := resumeAfter
	if cutoff <= 0 || cutoff >= len(fullBuild.PlannedCommits) {
		cutoff = len(fullBuild.PlannedCommits) / 2
	}
	if cutoff < 1 {
		cutoff = 1
	}
	cancelOpts := buildOpts
	cancelOpts.CancelAfterNewShards = cutoff
	firstResumeBuild, firstErr := audiofp.BuildCheckpoint(ctx, resumeStore, projectID, finalAssets, finalRecipe,
		audiofp.SnapshotFresh, cancelOpts)
	if !errors.Is(firstErr, context.Canceled) {
		_ = resumeStore.Close()
		return nil, fmt.Errorf("expected cancellation after %d shards, got %v", cutoff, firstErr)
	}
	committedBefore := len(firstResumeBuild.Commits)
	if err := resumeStore.Close(); err != nil {
		return nil, err
	}
	resumeStore, err = audiofp.OpenStore(filepath.Join(batchDir, "resume.db"))
	if err != nil {
		return nil, err
	}
	defer resumeStore.Close()
	resumeBuild, err := audiofp.BuildCheckpoint(ctx, resumeStore, projectID, finalAssets, finalRecipe,
		audiofp.SnapshotResume, buildOpts)
	if err != nil {
		return nil, err
	}
	if resumeBuild.ReusedShards < committedBefore || committedBefore < 1 {
		return nil, fmt.Errorf("resume reused %d shards, expected at least %d", resumeBuild.ReusedShards, committedBefore)
	}
	if err := audiofp.CompareBuildResults(fullBuild, resumeBuild); err != nil {
		return nil, err
	}
	if err := compareBuildQueries(project, fullBuild.Analyses, resumeBuild.Analyses, finalRecipe); err != nil {
		return nil, err
	}

	// A changed recipe version must invalidate the whole snapshot. Seed a
	// partial snapshot with the original recipe, then resume with a changed
	// candidate limit and require a zero-reuse rebuild.
	mismatchPath := filepath.Join(batchDir, "mismatch.db")
	mismatchStore, err := audiofp.OpenStore(mismatchPath)
	if err != nil {
		return nil, err
	}
	partialOpts := buildOpts
	partialOpts.CancelAfterNewShards = cutoff
	if _, err := audiofp.BuildCheckpoint(ctx, mismatchStore, projectID, finalAssets, finalRecipe,
		audiofp.SnapshotFresh, partialOpts); !errors.Is(err, context.Canceled) {
		_ = mismatchStore.Close()
		return nil, fmt.Errorf("expected mismatch seed cancellation, got %v", err)
	}
	if err := mismatchStore.Close(); err != nil {
		return nil, err
	}
	mismatchStore, err = audiofp.OpenStore(mismatchPath)
	if err != nil {
		return nil, err
	}
	mismatchedRecipe := finalRecipe
	mismatchedRecipe.CandidateLimit++
	versionReset, err := audiofp.BuildCheckpoint(ctx, mismatchStore, projectID, finalAssets, mismatchedRecipe,
		audiofp.SnapshotResume, buildOpts)
	if err != nil {
		_ = mismatchStore.Close()
		return nil, err
	}
	_ = mismatchStore.Close()
	if !versionReset.SnapshotReset || versionReset.ReusedShards != 0 {
		return nil, fmt.Errorf("version mismatch did not invalidate the whole snapshot")
	}
	expectedStore, err := audiofp.OpenStore(filepath.Join(batchDir, "mismatch-full.db"))
	if err != nil {
		return nil, err
	}
	expectedReset, err := audiofp.BuildCheckpoint(ctx, expectedStore, projectID, finalAssets, mismatchedRecipe,
		audiofp.SnapshotFresh, buildOpts)
	_ = expectedStore.Close()
	if err != nil {
		return nil, err
	}
	if err := audiofp.CompareBuildResults(expectedReset, versionReset); err != nil {
		return nil, err
	}

	frameCount := 0
	queueWaits := 0
	for _, analysis := range fullAnalyses {
		frameCount += len(analysis.Frames)
	}
	for _, st := range fullStats {
		queueWaits += st.QueueWaitEvents
	}
	return &BatchReport{
		BatchID: project.Manifest.BatchID, Assets: len(finalAssets),
		Edits: len(events), Queries: len(project.Manifest.Truth.Queries), Frames: frameCount,
		Invalidations: invalidations,
		ReusedFrames:  session.ReusedFrames, RecomputedFrames: session.RecomputedFrames,
		IncrementalCacheHits: incBuild.ReusedShards, IncrementalCacheNew: incBuild.NewShards,
		QueueWaitEvents: queueWaits, ResumeFirstCommitted: committedBefore,
		ResumeReusedShards:   resumeBuild.ReusedShards,
		VersionResetDetected: versionReset.SnapshotReset,
		VersionResetReused:   versionReset.ReusedShards,
		DependencyNodes:      len(session.Graph.Nodes),
		TopologicalNodes:     len(session.Graph.TopologicalOrder()),
		Verified:             verifyReport,
	}, nil
}

func assetsFromMap(assets map[string]audiofp.PCMAsset) []audiofp.PCMAsset {
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]audiofp.PCMAsset, 0, len(ids))
	for _, id := range ids {
		out = append(out, assets[id])
	}
	return out
}

func analysesFromTruth(truth audiofp.Truth, recipe audiofp.Recipe) []audiofp.AssetAnalysis {
	out := make([]audiofp.AssetAnalysis, 0, len(truth.Assets))
	for _, at := range truth.Assets {
		frames := make([]audiofp.FrameFingerprint, 0, len(at.Frames))
		for _, ft := range at.Frames {
			frames = append(frames, audiofp.TruthToFingerprint(at.AssetID, ft))
		}
		out = append(out, audiofp.AssetAnalysis{
			AssetID: at.AssetID, ContentHash: at.ContentHash, RecipeHash: audiofp.RecipeHash(recipe),
			TotalSamples: at.TotalSamples, Frames: frames,
			Index: audiofp.BuildIndexEntries(at.AssetID, at.ContentHash, recipe, frames),
		})
	}
	audiofp.SortAnalyses(out)
	return out
}

func expectedSearch(q audiofp.QueryTruth) audiofp.SearchResult {
	result := audiofp.SearchResult{
		QueryID: q.QueryID, Threshold: q.Threshold, CandidateLimit: q.CandidateLimit,
		Matches: make([]audiofp.Match, 0, len(q.Expected)),
	}
	for i, m := range q.Expected {
		result.Matches = append(result.Matches, audiofp.Match{
			Rank: i + 1, AssetID: m.AssetID, StartSample: m.StartSample,
			EndSample: m.EndSample, Similarity: m.Similarity,
		})
	}
	return result
}

func compareQueries(ctx context.Context, project *audiofp.Project, full, incremental []audiofp.AssetAnalysis) ([]audiofp.SearchResult, error) {
	var out []audiofp.SearchResult
	for _, q := range project.Manifest.Truth.Queries {
		queryAsset, ok := project.Queries[q.QueryID]
		if !ok {
			return nil, fmt.Errorf("query %s is not loaded", q.QueryID)
		}
		frames, _, err := audiofp.ExtractAsset(ctx, queryAsset, project.Manifest.Truth.FinalRecipe,
			audiofp.ExtractionOptions{Workers: 2})
		if err != nil {
			return nil, err
		}
		fullResult, err := audiofp.SearchQuery(q.QueryID, frames, full, project.Manifest.Truth.FinalRecipe, q.Threshold, q.CandidateLimit)
		if err != nil {
			return nil, err
		}
		incResult, err := audiofp.SearchQuery(q.QueryID, frames, incremental, project.Manifest.Truth.FinalRecipe, q.Threshold, q.CandidateLimit)
		if err != nil {
			return nil, err
		}
		if err := audiofp.CompareSearchResults(expectedSearch(q), fullResult); err != nil {
			return nil, err
		}
		if err := audiofp.CompareSearchResults(fullResult, incResult); err != nil {
			return nil, err
		}
		out = append(out, fullResult)
	}
	return out, nil
}

func compareBuildQueries(project *audiofp.Project, expectedAnalyses, actualAnalyses []audiofp.AssetAnalysis, recipe audiofp.Recipe) error {
	ctx := context.Background()
	for _, q := range project.Manifest.Truth.Queries {
		queryAsset := project.Queries[q.QueryID]
		frames, _, err := audiofp.ExtractAsset(ctx, queryAsset, recipe, audiofp.ExtractionOptions{Workers: 2})
		if err != nil {
			return err
		}
		expected, err := audiofp.SearchQuery(q.QueryID, frames, expectedAnalyses, recipe, q.Threshold, q.CandidateLimit)
		if err != nil {
			return err
		}
		actual, err := audiofp.SearchQuery(q.QueryID, frames, actualAnalyses, recipe, q.Threshold, q.CandidateLimit)
		if err != nil {
			return err
		}
		if err := audiofp.CompareSearchResults(expected, actual); err != nil {
			return err
		}
	}
	return nil
}
