package audiofp

import (
	"fmt"
	"math"
)

type Difference struct {
	AssetID string `json:"asset_id,omitempty"`
	FrameNo int    `json:"frame_no"`
	Field   string `json:"field"`
	QueryID string `json:"query_id,omitempty"`
	Message string `json:"message"`
}

func (d Difference) AsError() error {
	asset := d.AssetID
	frame := d.FrameNo
	field := d.Field
	if d.QueryID != "" {
		field = "query." + d.QueryID + "." + field
	}
	return DiffError(asset, frame, field, d.Message)
}

func diffValue(assetID string, frame int, field string, expected, actual any) error {
	return Difference{
		AssetID: assetID, FrameNo: frame, Field: field,
		Message: fmt.Sprintf("expected=%v actual=%v", expected, actual),
	}.AsError()
}

func CompareAnalyses(expected, actual []AssetAnalysis) error {
	exp := append([]AssetAnalysis(nil), expected...)
	act := append([]AssetAnalysis(nil), actual...)
	SortAnalyses(exp)
	SortAnalyses(act)
	if len(exp) != len(act) {
		return DiffError("", 0, "asset_count", fmt.Sprintf("expected=%d actual=%d", len(exp), len(act)))
	}
	for i := range exp {
		e, a := exp[i], act[i]
		if e.AssetID != a.AssetID {
			return diffValue(e.AssetID, 0, "asset_id", e.AssetID, a.AssetID)
		}
		if e.ContentHash != a.ContentHash {
			return diffValue(e.AssetID, 0, "content_hash", e.ContentHash, a.ContentHash)
		}
		if e.RecipeHash != a.RecipeHash {
			return diffValue(e.AssetID, 0, "recipe_hash", e.RecipeHash, a.RecipeHash)
		}
		if e.TotalSamples != a.TotalSamples {
			return diffValue(e.AssetID, 0, "total_samples", e.TotalSamples, a.TotalSamples)
		}
		if len(e.Frames) != len(a.Frames) {
			return diffValue(e.AssetID, 0, "frame_count", len(e.Frames), len(a.Frames))
		}
		for frame := range e.Frames {
			if err := compareFrame(e.Frames[frame], a.Frames[frame]); err != nil {
				return err
			}
		}
		if len(e.Index) != len(a.Index) {
			return diffValue(e.AssetID, 0, "index_count", len(e.Index), len(a.Index))
		}
		for frame := range e.Index {
			ei, ai := e.Index[frame], a.Index[frame]
			if ei.AssetID != ai.AssetID {
				return diffValue(e.AssetID, ei.FrameNo, "index.asset_id", ei.AssetID, ai.AssetID)
			}
			if ei.FrameNo != ai.FrameNo {
				return diffValue(e.AssetID, ei.FrameNo, "index.frame_no", ei.FrameNo, ai.FrameNo)
			}
			if ei.StartSample != ai.StartSample {
				return diffValue(e.AssetID, ei.FrameNo, "index.start_sample", ei.StartSample, ai.StartSample)
			}
			if ei.CacheKey != ai.CacheKey {
				return diffValue(e.AssetID, ei.FrameNo, "index.cache_key", ei.CacheKey, ai.CacheKey)
			}
			if ei.EntryDigest != ai.EntryDigest {
				return diffValue(e.AssetID, ei.FrameNo, "index.entry_digest", ei.EntryDigest, ai.EntryDigest)
			}
		}
	}
	return nil
}

func compareFrame(e, a FrameFingerprint) error {
	if e.AssetID != a.AssetID {
		return diffValue(e.AssetID, e.FrameNo, "frame.asset_id", e.AssetID, a.AssetID)
	}
	if e.FrameNo != a.FrameNo {
		return diffValue(e.AssetID, e.FrameNo, "frame.frame_no", e.FrameNo, a.FrameNo)
	}
	if e.StartSample != a.StartSample {
		return diffValue(e.AssetID, e.FrameNo, "frame.start_sample", e.StartSample, a.StartSample)
	}
	if e.Bands != a.Bands {
		return diffValue(e.AssetID, e.FrameNo, "frame.bands", e.Bands, a.Bands)
	}
	if e.BitWidth != a.BitWidth {
		return diffValue(e.AssetID, e.FrameNo, "frame.bit_width", e.BitWidth, a.BitWidth)
	}
	if e.Zero != a.Zero {
		return diffValue(e.AssetID, e.FrameNo, "frame.zero", e.Zero, a.Zero)
	}
	if e.PackedHex != a.PackedHex {
		return diffValue(e.AssetID, e.FrameNo, "frame.packed_hex", e.PackedHex, a.PackedHex)
	}
	if e.Summary != a.Summary {
		return diffValue(e.AssetID, e.FrameNo, "frame.summary", e.Summary, a.Summary)
	}
	if len(e.Levels) != len(a.Levels) {
		return diffValue(e.AssetID, e.FrameNo, "frame.levels", len(e.Levels), len(a.Levels))
	}
	for i := range e.Levels {
		if e.Levels[i] != a.Levels[i] {
			return diffValue(e.AssetID, e.FrameNo, fmt.Sprintf("frame.levels[%d]", i), e.Levels[i], a.Levels[i])
		}
	}
	return nil
}

func CompareCommitSequences(expected, actual []CommitRef) error {
	if len(expected) != len(actual) {
		return DiffError("", 0, "commit_sequence.length", fmt.Sprintf("expected=%d actual=%d", len(expected), len(actual)))
	}
	for i := range expected {
		e, a := expected[i], actual[i]
		if e.Seq != a.Seq {
			return diffValue(e.AssetID, e.FirstFrame, "commit_sequence.seq", e.Seq, a.Seq)
		}
		if e.AssetID != a.AssetID {
			return diffValue(e.AssetID, e.FirstFrame, "commit_sequence.asset_id", e.AssetID, a.AssetID)
		}
		if e.ShardNo != a.ShardNo {
			return diffValue(e.AssetID, e.FirstFrame, "commit_sequence.shard_no", e.ShardNo, a.ShardNo)
		}
		if e.CacheKey != a.CacheKey {
			return diffValue(e.AssetID, e.FirstFrame, "commit_sequence.cache_key", e.CacheKey, a.CacheKey)
		}
		if e.FirstFrame != a.FirstFrame || e.LastFrame != a.LastFrame {
			return diffValue(e.AssetID, e.FirstFrame, "commit_sequence.frame_range",
				fmt.Sprintf("%d:%d", e.FirstFrame, e.LastFrame), fmt.Sprintf("%d:%d", a.FirstFrame, a.LastFrame))
		}
	}
	return nil
}

func CompareSearchResults(expected, actual SearchResult) error {
	if expected.QueryID != actual.QueryID {
		return diffValue("", 0, "query_id", expected.QueryID, actual.QueryID)
	}
	if len(expected.Matches) != len(actual.Matches) {
		return Difference{QueryID: expected.QueryID, Field: "match_count",
			Message: fmt.Sprintf("expected=%d actual=%d", len(expected.Matches), len(actual.Matches))}.AsError()
	}
	for i := range expected.Matches {
		e, a := expected.Matches[i], actual.Matches[i]
		if e.Rank != a.Rank {
			return Difference{AssetID: e.AssetID, QueryID: expected.QueryID, Field: "rank",
				Message: fmt.Sprintf("expected=%d actual=%d", e.Rank, a.Rank)}.AsError()
		}
		if e.AssetID != a.AssetID || e.StartSample != a.StartSample || e.EndSample != a.EndSample {
			return Difference{AssetID: e.AssetID, QueryID: expected.QueryID, Field: "candidate",
				Message: fmt.Sprintf("expected=%s@%d-%d actual=%s@%d-%d",
					e.AssetID, e.StartSample, e.EndSample, a.AssetID, a.StartSample, a.EndSample)}.AsError()
		}
		if math.Abs(e.Similarity-a.Similarity) > 1e-12 {
			return Difference{AssetID: e.AssetID, QueryID: expected.QueryID, Field: "similarity",
				Message: fmt.Sprintf("expected=%.12f actual=%.12f", e.Similarity, a.Similarity)}.AsError()
		}
	}
	return nil
}

func CompareBuildResults(expected, actual BuildResult) error {
	if err := CompareAnalyses(expected.Analyses, actual.Analyses); err != nil {
		return err
	}
	if err := CompareCommitSequences(expected.Commits, actual.Commits); err != nil {
		return err
	}
	return nil
}
