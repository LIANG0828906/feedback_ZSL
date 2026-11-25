package audiofp

import (
	"context"
	"fmt"
	"sort"
)

// AnalyzeAsset runs extraction and index construction for one PCM asset.
func AnalyzeAsset(ctx context.Context, asset PCMAsset, r Recipe, opts ExtractionOptions) (AssetAnalysis, ExtractionStats, error) {
	frames, stats, err := ExtractAsset(ctx, asset, r, opts)
	if err != nil {
		return AssetAnalysis{}, stats, err
	}
	return AssetAnalysis{
		AssetID: asset.ID, ContentHash: asset.ContentHash(), RecipeHash: RecipeHash(r),
		TotalSamples: len(asset.Samples), Frames: frames,
		Index: BuildIndexEntries(asset.ID, asset.ContentHash(), r, frames),
	}, stats, nil
}

// AnalyzeAssets is the deterministic full-analysis path.
func AnalyzeAssets(ctx context.Context, assets []PCMAsset, r Recipe, opts ExtractionOptions) ([]AssetAnalysis, []ExtractionStats, error) {
	ordered := append([]PCMAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seen := make(map[string]bool, len(ordered))
	out := make([]AssetAnalysis, 0, len(ordered))
	var stats []ExtractionStats
	for _, asset := range ordered {
		if seen[asset.ID] {
			return nil, nil, schemaError("asset_id", "duplicate asset id "+asset.ID)
		}
		seen[asset.ID] = true
		if len(asset.Samples) < r.FrameLen {
			return nil, nil, parameterError("frame_len", fmt.Sprintf("asset %s is shorter than one frame", asset.ID))
		}
		analysis, st, err := AnalyzeAsset(ctx, asset, r, opts)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, analysis)
		stats = append(stats, st)
	}
	SortAnalyses(out)
	return out, stats, nil
}
