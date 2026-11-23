package session

import (
	"fmt"
	"time"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/stitch"
)

func (s *SetSession) Preview(version int64) (*model.PreviewResult, error) {
	latest := s.snapshots.Latest()
	if version == 0 {
		version = latest.Version
	}
	if version != latest.Version {
		return nil, &errs.Error{
			Code: errs.EVersion, Message: "preview only accepts the latest parameter snapshot",
			Actual: version, Threshold: latest.Version, Field: "version",
		}
	}
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	return s.previewLocked(version, latest.Params, true)
}

func (s *SetSession) previewLocked(version int64, params model.Params, requireLatest bool) (*model.PreviewResult, error) {
	if requireLatest {
		if latest := s.snapshots.Latest(); version != latest.Version {
			return nil, &errs.Error{Code: errs.EVersion, Message: "preview snapshot became stale", Actual: version, Threshold: latest.Version}
		}
	}
	s.mu.Lock()
	if cached := s.previews[version]; cached != nil {
		s.mu.Unlock()
		return cached.Result, nil
	}
	committed := make(map[int]model.PairStats, len(s.commits))
	for k, v := range s.commits {
		committed[k] = v
	}
	s.mu.Unlock()
	start := time.Now()
	reg, err := stitch.Register(stitch.RegisterOptions{
		Set: s.Set, Params: params, Version: version, Coarse: true,
		Downsample: params.PreviewDownsample, Committed: committed,
	})
	if err != nil {
		return nil, err
	}
	reg.Canvas = s.Set.Manifest.Canvas
	rendered, err := stitch.Render(reg, params, params.PreviewDownsample)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("preview-%d.png", version)
	s.putArtifact(key, rendered.PNG)
	pairs := copyPairs(reg.Pairs)
	for i := range pairs {
		if pairs[i].PairIndex > 0 && !pairs[i].Committed {
			pairs[i].PreviewDownsample = params.PreviewDownsample
			pairs[i].PreviewFeature = params.PreviewFeatureBudget
		}
	}
	coverage := make(map[string]float64, len(pairs))
	for _, p := range pairs {
		coverage[p.PairID] = p.OverlapCoverage
	}
	elapsed := time.Since(start)
	res := &model.PreviewResult{
		SetID: s.ID, ParamVersion: version, ParamsHash: params.Hash(),
		ContentHash: s.Set.ContentHash, Downsample: params.PreviewDownsample,
		FeatureBudget: params.PreviewFeatureBudget, DurationMs: elapsed.Milliseconds(),
		Pairs: pairs, Canvas: reg.Canvas,
		PanoramaURL:     fmt.Sprintf("/api/sets/%s/artifact/%s", s.ID, key),
		DiagnosticsURL:  fmt.Sprintf("/api/sets/%s/artifact/%s", s.ID, key),
		ElapsedBudgetMs: 5000, ImageWidth: rendered.Width, ImageHeight: rendered.Height,
		CoverageByPair: coverage,
	}
	cache := &previewCache{Result: res, Pairs: pairMap(pairs)}
	s.mu.Lock()
	s.previews[version] = cache
	s.mu.Unlock()
	return res, nil
}

func (s *SetSession) getOrCreatePreview(version int64, params model.Params) (*previewCache, error) {
	s.mu.Lock()
	if p := s.previews[version]; p != nil {
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	s.mu.Lock()
	if p := s.previews[version]; p != nil {
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	if _, err := s.previewLocked(version, params, false); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previews[version], nil
}

func copyPairs(in []model.PairStats) []model.PairStats {
	out := make([]model.PairStats, len(in))
	copy(out, in)
	for i := range out {
		out[i].Homography = append([]float64(nil), in[i].Homography...)
		out[i].TruthHomography = append([]float64(nil), in[i].TruthHomography...)
		out[i].ExposureGain = append([]float64(nil), in[i].ExposureGain...)
		out[i].ExposureOffset = append([]float64(nil), in[i].ExposureOffset...)
		out[i].Path = append([]string(nil), in[i].Path...)
	}
	return out
}

func pairMap(pairs []model.PairStats) map[int]model.PairStats {
	out := make(map[int]model.PairStats, len(pairs))
	for _, p := range pairs {
		out[p.PairIndex] = p
	}
	return out
}
