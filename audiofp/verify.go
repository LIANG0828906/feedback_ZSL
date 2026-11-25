package audiofp

import (
	"context"
	"fmt"
)

type VerifyReport struct {
	ProjectID      string `json:"project_id"`
	Algorithm      string `json:"algorithm_version"`
	AssetsChecked  int    `json:"assets_checked"`
	FramesChecked  int    `json:"frames_checked"`
	IndexChecked   int    `json:"index_checked"`
	QueriesChecked int    `json:"queries_checked"`
	OK             bool   `json:"ok"`
}

// VerifyPersistedSnapshot independently recomputes digests, index entries, and
// query rankings from SQLite rows and generator truth. It intentionally does
// not import or invoke the PCM extraction path.
func VerifyPersistedSnapshot(ctx context.Context, store *Store, projectID string, r Recipe, truth Truth) (VerifyReport, error) {
	report := VerifyReport{ProjectID: projectID, Algorithm: AlgorithmVersion}
	meta, ok, err := store.SnapshotMeta(projectID)
	if err != nil {
		return report, err
	}
	if !ok {
		return report, schemaError("snapshot", "project snapshot does not exist")
	}
	if meta.RecipeVersion != RecipeHash(r) {
		return report, DiffError("", 0, "snapshot.recipe_version", "SQLite recipe version does not match truth recipe")
	}
	if meta.AlgorithmVersion != AlgorithmVersion {
		return report, DiffError("", 0, "snapshot.algorithm_version", "SQLite algorithm version is unsupported")
	}
	actual, commits, err := AnalysesFromStore(ctx, store, projectID)
	if err != nil {
		return report, err
	}
	for i, c := range commits {
		if c.Seq != i {
			return report, DiffError(c.AssetID, c.FirstFrame, "commit_sequence",
				fmt.Sprintf("expected seq %d actual %d", i, c.Seq))
		}
	}
	expected := make([]AssetAnalysis, 0, len(truth.Assets))
	for _, at := range truth.Assets {
		frames := make([]FrameFingerprint, 0, len(at.Frames))
		for _, ft := range at.Frames {
			frames = append(frames, TruthToFingerprint(at.AssetID, ft))
		}
		expected = append(expected, AssetAnalysis{
			AssetID: at.AssetID, ContentHash: at.ContentHash, RecipeHash: RecipeHash(r),
			TotalSamples: at.TotalSamples, Frames: frames,
			Index: BuildIndexEntries(at.AssetID, at.ContentHash, r, frames),
		})
	}
	SortAnalyses(expected)
	if err := CompareAnalyses(expected, actual); err != nil {
		return report, err
	}
	framesChecked := 0
	indexChecked := 0
	for _, analysis := range actual {
		for _, frame := range analysis.Frames {
			packed := PackLevels(frame.Levels, frame.BitWidth)
			if packed != frame.PackedHex {
				return report, DiffError(analysis.AssetID, frame.FrameNo, "frame.packed_hex",
					"packed fingerprint does not recompute from persisted levels")
			}
			summary := FrameSummary(frame.StartSample, frame.Levels, frame.BitWidth, frame.Zero)
			if summary != frame.Summary {
				return report, DiffError(analysis.AssetID, frame.FrameNo, "frame.summary",
					"frame summary does not recompute from persisted levels")
			}
			if int(frame.Bands) != r.Bands || frame.BitWidth != r.QuantBits {
				return report, DiffError(analysis.AssetID, frame.FrameNo, "frame.bands",
					"persisted frame dimensions do not match recipe")
			}
			framesChecked++
		}
		indexChecked += len(analysis.Index)
	}
	truthAssets := make(map[string]AssetTruth, len(truth.Assets))
	for _, at := range truth.Assets {
		truthAssets[at.AssetID] = at
	}
	for _, q := range truth.Queries {
		at, ok := truthAssets[q.AssetID]
		if !ok {
			return report, schemaError("truth.queries.asset_id", "query source asset is missing")
		}
		queryFrames, err := QueryFramesFromTruth(at, q, r)
		if err != nil {
			return report, err
		}
		got, err := SearchQuery(q.QueryID, queryFrames, actual, r, q.Threshold, q.CandidateLimit)
		if err != nil {
			return report, err
		}
		want := SearchResult{
			QueryID: q.QueryID, Threshold: q.Threshold, CandidateLimit: q.CandidateLimit,
			Matches: make([]Match, 0, len(q.Expected)),
		}
		for i, m := range q.Expected {
			want.Matches = append(want.Matches, Match{
				Rank: i + 1, AssetID: m.AssetID, StartSample: m.StartSample,
				EndSample: m.EndSample, Similarity: m.Similarity,
			})
		}
		if err := CompareSearchResults(want, got); err != nil {
			return report, err
		}
	}
	report.AssetsChecked = len(actual)
	report.FramesChecked = framesChecked
	report.IndexChecked = indexChecked
	report.QueriesChecked = len(truth.Queries)
	report.OK = true
	return report, nil
}
