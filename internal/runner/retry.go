package runner

import (
	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/pipeline"
	"edgerefine/internal/sample"
	"edgerefine/internal/state"
	"edgerefine/internal/store"
)

func RetryTileRefinement(b *sample.Bundle, st *store.Store, tile model.TileCoord, p model.Params, truthFP string) (*model.TileResult, error) {
	if err := model.ValidateParams(p, b.Manifest.Width, b.Manifest.Height); err != nil {
		return nil, err
	}
	loaded, _, err := loadCheckpoints(st, b.Manifest.SampleID, truthFP)
	if err != nil {
		return nil, err
	}
	key := tile.Array()
	if _, committed := loaded.refine[key]; committed {
		return nil, errs.State(key, string(state.StageRefine), string(state.StatusCommitted), string(state.StatusProcessing))
	}
	fg, ok := loaded.fg[key]
	if !ok {
		return nil, errs.State(key, string(state.StageForeground), string(state.StatusPending), string(state.StatusProcessing))
	}
	edge, ok := loaded.edge[key]
	if !ok {
		return nil, errs.State(key, string(state.StageEdgeTrace), string(state.StatusPending), string(state.StatusProcessing))
	}
	r := pipeline.TileRect(p, tile)
	out, refineErr := pipeline.RefineStage(fg.Pixels, r, fg.Threshold, edge, p.IterBudget, 1, p.ConvergeTolerance, nil)
	tr := &model.TileResult{
		SampleID: b.Manifest.SampleID, Tile: tile, Rect: r, Threshold: fg.Threshold,
		SeparationMask: fg.Mask, CleanMask: edge.CleanMask, SkeletonMask: edge.SkeletonMask,
		RefinedMask: out.RefinedMask, DeviationMap: out.DeviationMap, EdgePoints: out.EdgePoints,
		Iterations: out.Iterations, FinalResidual: out.FinalResidual, Converged: out.Converged,
		ActualBudget: out.ActualBudget, ElementStep: out.ElementStep, DecisionBasis: out.DecisionBasis,
		ParamVersion: p.ParamVersion, AlgorithmVersion: model.AlgorithmVersion,
	}
	status := state.StatusCommitted
	if refineErr != nil {
		status = state.StatusFailed
	}
	conv := model.TileConvergence{Tile: tile, Converged: out.Converged, Residual: out.FinalResidual, CommittedStages: 3}
	if dbErr := st.CommitStage(store.StageRecord{
		SampleID: b.Manifest.SampleID, Tile: tile, Stage: state.StageRefine, Status: status,
		Summary: "manual tile retry: " + out.DecisionBasis, Convergence: conv, Output: out,
		ParamVersion: p.ParamVersion, IterBudget: p.IterBudget,
		AlgorithmVersion: model.AlgorithmVersion, TruthFingerprint: truthFP,
	}); dbErr != nil {
		return tr, dbErr
	}
	return tr, refineErr
}
