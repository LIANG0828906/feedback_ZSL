package pipeline

import (
	"encoding/json"

	"prosodyalign/internal/checkpoint"
	"prosodyalign/internal/dag"
	"prosodyalign/internal/fit"
	"prosodyalign/internal/model"
)

func (e *Engine) persistSegmentNodes(p *model.Project, result *model.DialogResult, paramHash string, old *model.DialogResult) ([]checkpoint.CacheEntry, map[int]string, map[int]string, error) {
	stretchHits := map[int]string{}
	rangeHits := map[int]string{}
	if e.FullRecompute || e.Store == nil {
		return nil, stretchHits, rangeHits, nil
	}

	contentKey := dag.ContentKey(p.ContentHash)
	entries := []checkpoint.CacheEntry{}
	stretchKeys := make([]string, len(result.Fit.Segments))
	for i, seg := range result.Fit.Segments {
		shareBasis := model.HashCanonical(struct {
			AddedSamples        int64 `json:"added_samples"`
			CoefficientPermille int64 `json:"coefficient_permille"`
			CapTriggered        bool  `json:"cap_triggered"`
		}{seg.AddedSamples, seg.CoefficientPermille, seg.CapTriggered})
		shareKey := "allocation-share:" + shareBasis[:16]
		stretchKeys[i] = dag.CacheKey(p.ContentHash, p.Phonemes[i].ID, fit.AlgorithmVersion, dag.NodeStretch, contentKey, shareKey)
		cacheKey := stretchKeys[i]
		_, directOK, err := e.Store.GetCache(cacheKey)
		if err != nil {
			return nil, nil, nil, err
		}
		if directOK {
			stretchHits[i] = cacheKey
		} else if oldSegment, ok := oldSegmentAt(old, i); ok && allocationEqual(oldSegment, seg) {
			if latest, found, err := e.Store.FindLatestCache(p.ID, string(dag.NodeStretch), i); err != nil {
				return nil, nil, nil, err
			} else if found {
				var cached model.SegmentFit
				if json.Unmarshal([]byte(latest.PayloadJSON), &cached) == nil && allocationEqual(cached, seg) {
					stretchHits[i] = latest.Key
				}
			}
		}
		if _, hit := stretchHits[i]; !hit || stretchHits[i] != cacheKey {
			entries = append(entries, checkpoint.CacheEntry{
				Key: cacheKey, DialogID: p.ID, PhonemeIndex: i, Kind: string(dag.NodeStretch),
				PayloadJSON: checkpoint.MustJSON(seg), ContentHash: p.ContentHash,
				ParameterHash: paramHash, AlgorithmVersion: fit.AlgorithmVersion,
			})
		}

		upstream := []string{stretchKeys[i]}
		if i > 0 {
			prevEnd := "range-end:" + model.HashCanonical(result.Fit.Segments[i-1].NewEnd)[:16]
			upstream = append(upstream, prevEnd)
		}
		rangeKey := dag.CacheKey(p.ContentHash, p.Phonemes[i].ID, fit.AlgorithmVersion, dag.NodeRange, upstream...)
		if entry, ok, err := e.Store.GetCache(rangeKey); err != nil {
			return nil, nil, nil, err
		} else if ok {
			rangeHits[i] = entry.Key
		} else if oldSegment, ok := oldSegmentAt(old, i); ok && oldSegment.NewStart == seg.NewStart && oldSegment.NewEnd == seg.NewEnd {
			if latest, found, err := e.Store.FindLatestCache(p.ID, string(dag.NodeRange), i); err != nil {
				return nil, nil, nil, err
			} else if found {
				var cached model.Range
				if json.Unmarshal([]byte(latest.PayloadJSON), &cached) == nil && cached.StartSample == seg.NewStart && cached.EndSample == seg.NewEnd {
					rangeHits[i] = latest.Key
				}
			}
		}
		if rangeHits[i] != rangeKey {
			entries = append(entries, checkpoint.CacheEntry{
				Key: rangeKey, DialogID: p.ID, PhonemeIndex: i, Kind: string(dag.NodeRange),
				PayloadJSON: checkpoint.MustJSON(model.Range{StartSample: seg.NewStart, EndSample: seg.NewEnd}),
				ContentHash: p.ContentHash, ParameterHash: paramHash, AlgorithmVersion: fit.AlgorithmVersion,
			})
		}
	}
	return entries, stretchHits, rangeHits, nil
}

func oldSegmentAt(old *model.DialogResult, i int) (model.SegmentFit, bool) {
	if old == nil || i < 0 || i >= len(old.Fit.Segments) {
		return model.SegmentFit{}, false
	}
	return old.Fit.Segments[i], true
}

func allocationEqual(a, b model.SegmentFit) bool {
	return a.AddedSamples == b.AddedSamples &&
		a.CoefficientPermille == b.CoefficientPermille &&
		a.CapTriggered == b.CapTriggered
}
