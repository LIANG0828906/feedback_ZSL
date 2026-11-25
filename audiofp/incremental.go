package audiofp

import (
	"context"
	"sort"
)

type IncrementalSession struct {
	Assets            map[string]PCMAsset
	Recipe            Recipe
	analyses          map[string]AssetAnalysis
	options           ExtractionOptions
	Invalidations     []Invalidation
	ReusedFrames      int
	RecomputedFrames  int
	ReusedEntries     int
	RecomputedEntries int
	Graph             *DependencyGraph
	recomputed        map[string]bool
	lastLogicalTime   int64
	hasLogicalTime    bool
}

func NewIncrementalSession(ctx context.Context, assets []PCMAsset, r Recipe, opts ExtractionOptions) (*IncrementalSession, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, err
	}
	analysisList, _, err := AnalyzeAssets(ctx, assets, r, opts)
	if err != nil {
		return nil, err
	}
	session := &IncrementalSession{
		Assets: make(map[string]PCMAsset, len(assets)), Recipe: r,
		analyses: make(map[string]AssetAnalysis, len(analysisList)), options: opts,
		recomputed: make(map[string]bool),
	}
	for _, asset := range assets {
		session.Assets[asset.ID] = asset.Clone()
	}
	for _, analysis := range analysisList {
		session.analyses[analysis.AssetID] = analysis
	}
	session.Graph = BuildDependencyGraph(assets, r)
	return session, nil
}

// Apply applies one edit and recomputes only affected assets. Query results are
// always invalidated when content or a query-affecting recipe field changes.
func (s *IncrementalSession) Apply(ctx context.Context, op EditOp, replacements map[int]PCMAsset) ([]Invalidation, error) {
	if s.hasLogicalTime && op.LogicalTime < s.lastLogicalTime {
		return nil, orderError(op.AssetID, 0, 0, "edit logical time is not monotonic")
	}
	beforeRecipe := s.Recipe
	beforeAssets := make(map[string]PCMAsset, len(s.Assets))
	for id, asset := range s.Assets {
		beforeAssets[id] = asset
	}
	if _, err := ApplyEdit(s.Assets, replacements, &s.Recipe, op); err != nil {
		return nil, err
	}
	s.lastLogicalTime = op.LogicalTime
	s.hasLogicalTime = true
	var affectedIDs []string
	frameAffected := false
	switch op.Kind {
	case "replace", "trim":
		affectedIDs = []string{op.AssetID}
		frameAffected = true
	case "set_param":
		frameAffected = recipeIsFrameAffecting(beforeRecipe, s.Recipe)
		if frameAffected {
			for id := range s.Assets {
				affectedIDs = append(affectedIDs, id)
			}
		}
	}
	sort.Strings(affectedIDs)
	s.Graph = BuildDependencyGraph(assetsFromMap(s.Assets), s.Recipe)
	if len(s.Graph.TopologicalOrder()) != len(s.Graph.Nodes) {
		return nil, orderError("", 0, 0, "dependency graph contains a cycle")
	}
	var invalidations []Invalidation
	reason := op.Kind + ":" + formatOp(op)
	if frameAffected {
		for _, id := range affectedIDs {
			old := beforeAssets[id]
			inv := invalidateAsset(old, beforeRecipe, "fingerprint+index", reason)
			invalidations = append(invalidations, inv)
		}
	}
	if recipeIsQueryAffecting(beforeRecipe, s.Recipe) || op.Kind == "replace" || op.Kind == "trim" {
		ids := make([]string, 0, len(s.Assets))
		for id := range s.Assets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			invalidations = append(invalidations, queryInvalidation(id, "retrieval results invalidated by "+reason))
		}
	}
	s.Invalidations = append(s.Invalidations, invalidations...)
	if !frameAffected {
		for id, analysis := range s.analyses {
			analysis.RecipeHash = RecipeHash(s.Recipe)
			s.analyses[id] = analysis
		}
		s.refreshStats()
		return invalidations, nil
	}
	for _, id := range affectedIDs {
		s.recomputed[id] = true
		analysis, _, err := AnalyzeAsset(ctx, s.Assets[id], s.Recipe, s.options)
		if err != nil {
			return invalidations, err
		}
		s.analyses[id] = analysis
	}
	for id, analysis := range s.analyses {
		analysis.RecipeHash = RecipeHash(s.Recipe)
		s.analyses[id] = analysis
	}
	s.refreshStats()
	return invalidations, nil
}

func (s *IncrementalSession) refreshStats() {
	s.ReusedFrames, s.RecomputedFrames = 0, 0
	s.ReusedEntries, s.RecomputedEntries = 0, 0
	for id, analysis := range s.analyses {
		if s.recomputed[id] {
			s.RecomputedFrames += len(analysis.Frames)
			s.RecomputedEntries += len(analysis.Index)
		} else {
			s.ReusedFrames += len(analysis.Frames)
			s.ReusedEntries += len(analysis.Index)
		}
	}
}

func (s *IncrementalSession) Analyses() []AssetAnalysis {
	out := make([]AssetAnalysis, 0, len(s.analyses))
	for _, analysis := range s.analyses {
		out = append(out, analysis)
	}
	SortAnalyses(out)
	return out
}

func assetsFromMap(assets map[string]PCMAsset) []PCMAsset {
	ids := make([]string, 0, len(assets))
	for id := range assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]PCMAsset, 0, len(ids))
	for _, id := range ids {
		out = append(out, assets[id])
	}
	return out
}

func formatOp(op EditOp) string {
	switch op.Kind {
	case "set_param":
		if op.Value != nil {
			return op.Param + "=" + formatFloat(*op.Value)
		}
		return op.Param
	case "replace", "trim":
		return op.AssetID + "[" + itoa(op.StartSample) + "," + itoa(op.EndSample) + ")"
	default:
		return op.Kind
	}
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return itoa(int(v))
	}
	return "non-integer"
}
