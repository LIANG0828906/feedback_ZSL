package ocrflow

import (
	"fmt"
	"sort"
	"strings"
)

// UpdateRecipe atomically swaps the recipe after recomputing its parameter
// identity. fields drives precise dependency invalidation.
func (s *Service) UpdateRecipe(mutate func(*Recipe), fields ...string) ([]InvalidationItem, error) {
	items, err := s.InvalidateRecipeFields(fields...)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if mutate != nil {
		mutate(s.recipe)
		s.recipe.ParamVersion = ComputeParamVersion(s.recipe)
	}
	err = s.recipe.ValidateRuntimeParameters()
	s.mu.Unlock()
	if err != nil {
		return items, err
	}
	return items, nil
}

func (s *Service) InvalidateRecipeFields(fields ...string) ([]InvalidationItem, error) {
	report := s.LastReport()
	if report == nil {
		return nil, fmt.Errorf("cannot plan invalidation before first run")
	}
	field := strings.Join(fields, ",")
	items := make([]InvalidationItem, 0)
	keysToDelete := make([]string, 0)
	for _, shot := range report.Shots {
		for _, line := range shot.Lines {
			if line.Recognition == nil && line.Status == "failed" {
				continue
			}
			stages := affectedStages(fields)
			keys := keysForStages(line.Keys, stages)
			if len(keys) == 0 {
				continue
			}
			reason := "recipe_field_changed"
			item := InvalidationItem{
				ShotID: shot.ShotID, LineNo: line.LineNo, LineID: line.LineID,
				Reason: reason, Field: field, Stage: stages[len(stages)-1], InvalidatedStages: stages, CacheKeys: keys,
			}
			items = append(items, item)
			keysToDelete = append(keysToDelete, keys...)
		}
	}
	if err := s.store.DeleteCache(keysToDelete); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.invalidations = append(s.invalidations, items...)
	s.mu.Unlock()
	return items, nil
}

func (s *Service) InvalidateRegion(shotID string, lineNo int, reason string) (*InvalidationItem, error) {
	report := s.LastReport()
	if report == nil {
		return nil, fmt.Errorf("cannot invalidate region before first run")
	}
	for _, shot := range report.Shots {
		if shot.ShotID != shotID {
			continue
		}
		for _, line := range shot.Lines {
			if line.LineNo != lineNo {
				continue
			}
			stages := []string{"line_candidate", "segmentation", "recognition"}
			keys := keysForStages(line.Keys, stages)
			if reason == "" {
				reason = "region_replaced"
			}
			item := &InvalidationItem{
				ShotID: shotID, LineNo: lineNo, LineID: line.LineID, Reason: reason,
				Stage: "recognition", InvalidatedStages: stages, CacheKeys: keys,
			}
			if err := s.store.DeleteCache(keys); err != nil {
				return nil, err
			}
			s.mu.Lock()
			s.invalidations = append(s.invalidations, *item)
			s.mu.Unlock()
			return item, nil
		}
	}
	return nil, fmt.Errorf("line %s:%d not found in last report", shotID, lineNo)
}

func affectedStages(fields []string) []string {
	joined := strings.ToLower(strings.Join(fields, ","))
	switch {
	case strings.HasPrefix(joined, "preprocess"):
		return []string{"preprocess", "detection", "line_candidate", "segmentation", "recognition"}
	case strings.HasPrefix(joined, "detection") || strings.Contains(joined, "threshold") || strings.Contains(joined, "overlap") || strings.Contains(joined, "stroke"):
		return []string{"detection", "line_candidate", "segmentation", "recognition"}
	case strings.Contains(joined, "sequence_split") || strings.Contains(joined, "segment"):
		return []string{"segmentation", "recognition"}
	case strings.HasPrefix(joined, "recognition"):
		return []string{"recognition"}
	default:
		return []string{"recognition"}
	}
}

func keysForStages(k LineKeys, stages []string) []string {
	out := make([]string, 0, len(stages))
	for _, stage := range stages {
		switch stage {
		case "preprocess":
			out = append(out, k.PreprocessKey)
		case "detection":
			out = append(out, k.DetectionKey)
		case "line_candidate":
			out = append(out, k.CandidateKey)
		case "segmentation":
			out = append(out, k.SegmentationKey)
		case "recognition":
			out = append(out, k.RecognitionKey)
		}
	}
	return out
}

func (s *Service) BuildExpectations() ([]ChunkExpectation, error) {
	recipe := s.Recipe()
	expected := make([]ChunkExpectation, 0)
	prev := ""
	for _, shot := range s.shots {
		prep, err := PreprocessImage(shot.RGBA, recipe.Preprocess)
		if err != nil {
			return nil, err
		}
		det, err := DetectLines(shot, recipe, prep)
		if err != nil {
			return nil, err
		}
		chunks := chunkLines(det.Lines, s.cfg.ChunkLineSize)
		for ci, lines := range chunks {
			id := fmt.Sprintf("chunk:%s:%02d", shot.ShotID, ci+1)
			lineHashes := map[int]string{}
			for _, line := range lines {
				lk := BuildLineKeys(shot, line, recipe, s.lib)
				lineHashes[line.LineNo] = lk.RegionHash
			}
			expected = append(expected, ChunkExpectation{
				ChunkID: id, ShotID: shot.ShotID, PredecessorID: prev, ShotHash: shot.ContentHash,
				RecipeVersion: recipe.RecipeVersion, ParamVersion: recipe.ParamVersion,
				AlgorithmVersion: RecipeAlgorithmVersion, LineHashes: lineHashes,
			})
			prev = id
		}
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].ChunkID < expected[j].ChunkID })
	// Sorting above is lexical and stable for zero-padded indexes; rebuild chain.
	prev = ""
	for i := range expected {
		expected[i].PredecessorID = prev
		prev = expected[i].ChunkID
	}
	return expected, nil
}

func (s *Service) RecoverCurrentCheckpoints() (*RecoveryReport, error) {
	expected, err := s.BuildExpectations()
	if err != nil {
		return nil, err
	}
	rec, err := s.store.Recover(expected)
	if err != nil {
		return nil, err
	}
	s.SetRecoveryReport(rec)
	return rec, nil
}
