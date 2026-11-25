package audiofp

import (
	"sort"
)

// BuildIndexEntries derives the per-frame index records from fingerprints.
func BuildIndexEntries(assetID, contentHash string, r Recipe, frames []FrameFingerprint) []IndexEntry {
	out := make([]IndexEntry, 0, len(frames))
	for _, frame := range frames {
		frameKey := frameCacheKey(contentHash, frame.FrameNo, r)
		indexKey := indexCacheKey(frameKey, frame.FrameNo, frame.StartSample)
		out = append(out, IndexEntry{
			AssetID: assetID, FrameNo: frame.FrameNo, StartSample: frame.StartSample,
			CacheKey: indexKey, EntryDigest: indexEntryDigest(assetID, frame.FrameNo, frame.StartSample, indexKey),
		})
	}
	return out
}

func validateQuery(query []FrameFingerprint, r Recipe, threshold float64, limit int) error {
	if err := ValidateRecipe(Recipe{
		FrameLen: r.FrameLen, FrameShift: r.FrameShift, Bands: r.Bands, QuantBits: r.QuantBits,
		SimilarityThreshold: threshold, CandidateLimit: limit,
	}); err != nil {
		return err
	}
	if len(query) == 0 {
		return parameterError("query", "must contain at least one frame")
	}
	for i, frame := range query {
		if frame.Bands != r.Bands || frame.BitWidth != r.QuantBits {
			return parameterError("query.bands", "query frame dimensions do not match recipe")
		}
		if len(frame.Levels) != r.Bands {
			return parameterError("query.levels", "query frame level count does not match bands")
		}
		if i > 0 && frame.StartSample <= query[i-1].StartSample {
			return orderError(frame.AssetID, 0, i, "query frame start samples must increase")
		}
	}
	return nil
}

// SearchQuery evaluates the query frame sequence against all indexed assets.
// It is a pure function of its inputs.
func SearchQuery(queryID string, query []FrameFingerprint, analyses []AssetAnalysis, r Recipe, threshold float64, limit int) (SearchResult, error) {
	if err := validateQuery(query, r, threshold, limit); err != nil {
		return SearchResult{}, err
	}
	ordered := append([]AssetAnalysis(nil), analyses...)
	SortAnalyses(ordered)
	var matches []Match
	denom := int64(len(query) * r.Bands * maxLevel(r.QuantBits))
	if denom == 0 {
		return SearchResult{}, parameterError("quant_bits", "similarity denominator is zero")
	}
	for _, asset := range ordered {
		if len(asset.Frames) < len(query) {
			continue
		}
		for startIdx := 0; startIdx+len(query) <= len(asset.Frames); startIdx++ {
			dist := int64(0)
			valid := true
			for q := range query {
				f := asset.Frames[startIdx+q]
				if f.Bands != r.Bands || len(f.Levels) != r.Bands {
					valid = false
					break
				}
				for band := 0; band < r.Bands; band++ {
					d := int(query[q].Levels[band]) - int(f.Levels[band])
					if d < 0 {
						d = -d
					}
					dist += int64(d)
				}
			}
			if !valid {
				continue
			}
			similarity := roundTiny(1 - float64(dist)/float64(denom))
			if similarity+1e-15 < threshold {
				continue
			}
			start := asset.Frames[startIdx].StartSample
			end := start + (len(query)-1)*r.FrameShift + r.FrameLen
			matches = append(matches, Match{
				AssetID: asset.AssetID, StartSample: start, EndSample: end,
				Similarity: similarity, FrameStart: asset.Frames[startIdx].FrameNo, FrameCount: len(query),
			})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Similarity != matches[j].Similarity {
			return matches[i].Similarity > matches[j].Similarity
		}
		if matches[i].AssetID != matches[j].AssetID {
			return matches[i].AssetID < matches[j].AssetID
		}
		if matches[i].StartSample != matches[j].StartSample {
			return matches[i].StartSample < matches[j].StartSample
		}
		return matches[i].FrameStart < matches[j].FrameStart
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	for i := range matches {
		matches[i].Rank = i + 1
	}
	if matches == nil {
		matches = []Match{}
	}
	return SearchResult{
		QueryID: queryID, Threshold: threshold, CandidateLimit: limit, Matches: matches,
	}, nil
}

// QueryFramesFromTruth returns the truth frames covered by a query. It is used
// by the independent verifier and never invokes extraction.
func QueryFramesFromTruth(assetTruth AssetTruth, q QueryTruth, r Recipe) ([]FrameFingerprint, error) {
	if q.StartSample%r.FrameShift != 0 {
		return nil, parameterError("query.start_sample", "must be aligned to frame_shift")
	}
	startFrame := q.StartSample / r.FrameShift
	if startFrame < 0 || startFrame+q.FrameCount > len(assetTruth.Frames) {
		return nil, parameterError("query.frame_count", "query range is outside truth frames")
	}
	out := make([]FrameFingerprint, q.FrameCount)
	for i := 0; i < q.FrameCount; i++ {
		out[i] = TruthToFingerprint(assetTruth.AssetID, assetTruth.Frames[startFrame+i])
	}
	return out, nil
}
