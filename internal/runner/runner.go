package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"edgerefine/internal/edgeeval"
	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/pipeline"
	"edgerefine/internal/sample"
	"edgerefine/internal/state"
	"edgerefine/internal/store"
)

var ErrStopAfterCommit = errors.New("stopped after requested commit boundary")

type Options struct {
	Workers          int
	Params           model.Params
	Store            *store.Store
	TruthFingerprint string
	Resume           bool
	Snapshots        map[[2]int]model.ThresholdSnapshot
	ManualThreshold  *float64
	Gate             *Gate
	Ctx              context.Context
	StopAfterCommits int
	ElementStep      int
	StrictAccuracy   bool
	Persist          bool
	Matrix           *state.Matrix
}

type loadedCheckpoints struct {
	fg     map[[2]int]pipeline.ForegroundOutput
	edge   map[[2]int]pipeline.EdgeOutput
	refine map[[2]int]pipeline.RefineOutput
}

func tileList(m model.Manifest) []model.TileCoord {
	nx, ny := m.TileCounts()
	tiles := make([]model.TileCoord, 0, nx*ny)
	for ty := 0; ty < ny; ty++ {
		for tx := 0; tx < nx; tx++ {
			tiles = append(tiles, model.TileCoord{TX: tx, TY: ty})
		}
	}
	return tiles
}

func parallelDo(n, workers int, fn func(i int)) {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

func loadCheckpoints(st *store.Store, sampleID, fp string) (*loadedCheckpoints, []store.StoredStage, error) {
	out := &loadedCheckpoints{
		fg:     map[[2]int]pipeline.ForegroundOutput{},
		edge:   map[[2]int]pipeline.EdgeOutput{},
		refine: map[[2]int]pipeline.RefineOutput{},
	}
	if st == nil {
		return out, nil, nil
	}
	rows, valid, err := st.LoadValidStages(sampleID, fp)
	if err != nil {
		return nil, nil, err
	}
	if !valid {
		return out, nil, nil
	}
	for _, row := range rows {
		key := row.Tile.Array()
		switch row.Stage {
		case state.StageForeground:
			var v pipeline.ForegroundOutput
			if err := json.Unmarshal(row.OutputJSON, &v); err != nil {
				return nil, nil, err
			}
			out.fg[key] = v
		case state.StageEdgeTrace:
			var v pipeline.EdgeOutput
			if err := json.Unmarshal(row.OutputJSON, &v); err != nil {
				return nil, nil, err
			}
			out.edge[key] = v
		case state.StageRefine:
			var v pipeline.RefineOutput
			if err := json.Unmarshal(row.OutputJSON, &v); err != nil {
				return nil, nil, err
			}
			out.refine[key] = v
		}
	}
	return out, rows, nil
}

func checkGate(ctx context.Context, g *Gate) error {
	if g != nil {
		return g.Check(ctx)
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

func Run(b *sample.Bundle, opts Options) (*model.RunReport, error) {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	p := opts.Params
	if p.TileSize == 0 {
		p = b.Manifest.Params
	}
	if opts.ElementStep == 0 {
		opts.ElementStep = 1
	}
	if err := model.ValidateParams(p, b.Manifest.Width, b.Manifest.Height); err != nil {
		return nil, err
	}
	tiles := tileList(b.Manifest)
	mat := opts.Matrix
	if mat == nil {
		mat = state.NewMatrix(tiles)
	}
	loaded, _, err := loadCheckpoints(opts.Store, b.Manifest.SampleID, opts.TruthFingerprint)
	if err != nil {
		return nil, err
	}

	rep := &model.RunReport{
		SampleID: b.Manifest.SampleID, AlgorithmVersion: model.AlgorithmVersion,
		ParamVersion: p.ParamVersion, Workers: opts.Workers,
		TilesX: b.Manifest.Width / p.TileSize, TilesY: b.Manifest.Height / p.TileSize,
		TotalStages: len(tiles) * 3, TotalTiles: len(tiles),
	}

	for _, stageName := range state.Stages() {
		for _, t := range tiles {
			key := t.Array()
			var ok bool
			switch stageName {
			case state.StageForeground:
				_, ok = loaded.fg[key]
			case state.StageEdgeTrace:
				_, ok = loaded.edge[key]
			case state.StageRefine:
				_, ok = loaded.refine[key]
			}
			if !ok {
				continue
			}
			_ = mat.Transition(t, stageName, state.StatusProcessing)
			_ = mat.Transition(t, stageName, state.StatusCommitted)
		}
	}

	fgOut := make([]pipeline.ForegroundOutput, len(tiles))
	edgeOut := make([]pipeline.EdgeOutput, len(tiles))
	refineOut := make([]pipeline.RefineOutput, len(tiles))
	stageErrs := make([]error, len(tiles))
	committedWrites := 0

	commit := func(t model.TileCoord, st state.Stage, summary string, conv any, out any, budget int) error {
		if opts.Store == nil || !opts.Persist {
			return nil
		}
		if err := opts.Store.SetTruthFingerprint(b.Manifest.SampleID, opts.TruthFingerprint); err != nil {
			return err
		}
		err := opts.Store.CommitStage(store.StageRecord{
			SampleID: b.Manifest.SampleID, Tile: t, Stage: st, Status: state.StatusCommitted,
			Summary: summary, Convergence: conv, Output: out, ParamVersion: p.ParamVersion,
			IterBudget: budget, AlgorithmVersion: model.AlgorithmVersion,
			TruthFingerprint: opts.TruthFingerprint,
		})
		if err != nil {
			return err
		}
		committedWrites++
		if opts.StopAfterCommits > 0 && committedWrites >= opts.StopAfterCommits {
			return ErrStopAfterCommit
		}
		return nil
	}

	for i, t := range tiles {
		key := t.Array()
		if v, ok := loaded.fg[key]; ok {
			fgOut[i] = v
			continue
		}
		if err := mat.Transition(t, state.StageForeground, state.StatusProcessing); err != nil {
			return rep, err
		}
	}
	if err := checkGate(ctx, opts.Gate); err != nil {
		return rep, err
	}
	parallelDo(len(tiles), opts.Workers, func(i int) {
		t := tiles[i]
		if _, ok := loaded.fg[t.Array()]; ok {
			return
		}
		r := pipeline.TileRect(p, t)
		var snap model.ThresholdSnapshot
		if opts.Snapshots != nil {
			if v, ok := opts.Snapshots[t.Array()]; ok {
				snap = v
			}
		}
		if snap.SampleID == "" {
			v, err := pipeline.EstimateThreshold(b.Manifest.SampleID, b.Image, t, r, p, opts.ManualThreshold)
			if err != nil {
				stageErrs[i] = err
				return
			}
			snap = v
		}
		fgOut[i] = pipeline.ForegroundStage(snap, b.Image, r)
	})
	for i, t := range tiles {
		if stageErrs[i] != nil {
			_ = mat.Transition(t, state.StageForeground, state.StatusFailed)
			return rep, stageErrs[i]
		}
		if _, ok := loaded.fg[t.Array()]; ok {
			continue
		}
		if err := mat.Transition(t, state.StageForeground, state.StatusCommitted); err != nil {
			return rep, err
		}
		summary := fmt.Sprintf("threshold=%.4f bg=%d fg=%d", fgOut[i].Threshold.Threshold, fgOut[i].Threshold.BGLevel, fgOut[i].Threshold.FGLevel)
		if err := commit(t, state.StageForeground, summary, nil, fgOut[i], p.IterBudget); err != nil {
			return rep, err
		}
	}

	for i, t := range tiles {
		key := t.Array()
		if v, ok := loaded.edge[key]; ok {
			edgeOut[i] = v
			continue
		}
		if err := mat.Transition(t, state.StageEdgeTrace, state.StatusProcessing); err != nil {
			return rep, err
		}
	}
	if err := checkGate(ctx, opts.Gate); err != nil {
		return rep, err
	}
	radius := p.Radius
	globalFG := make([]byte, b.Manifest.Width*b.Manifest.Height)
	for i, t := range tiles {
		r := pipeline.TileRect(p, t)
		for y := 0; y < r.H; y++ {
			copy(globalFG[(r.Y+y)*b.Manifest.Width+r.X:(r.Y+y)*b.Manifest.Width+r.X+r.W], fgOut[i].Mask[y*r.W:(y+1)*r.W])
		}
	}
	parallelDo(len(tiles), opts.Workers, func(i int) {
		t := tiles[i]
		if _, ok := loaded.edge[t.Array()]; ok {
			return
		}
		r := pipeline.TileRect(p, t)
		edgeOut[i] = pipeline.EdgeStagePadded(globalFG, b.Manifest.Width, b.Manifest.Height, r, radius)
	})
	for i, t := range tiles {
		if _, ok := loaded.edge[t.Array()]; ok {
			continue
		}
		if err := mat.Transition(t, state.StageEdgeTrace, state.StatusCommitted); err != nil {
			return rep, err
		}
		if err := commit(t, state.StageEdgeTrace, fmt.Sprintf("radius=%d skeleton_pixels=%d", radius, countBytes(edgeOut[i].SkeletonMask)), nil, edgeOut[i], p.IterBudget); err != nil {
			return rep, err
		}
	}

	for i, t := range tiles {
		key := t.Array()
		if v, ok := loaded.refine[key]; ok {
			refineOut[i] = v
			continue
		}
		if err := mat.Transition(t, state.StageRefine, state.StatusProcessing); err != nil {
			return rep, err
		}
	}
	stageErrs = make([]error, len(tiles))
	parallelDo(len(tiles), opts.Workers, func(i int) {
		t := tiles[i]
		if _, ok := loaded.refine[t.Array()]; ok {
			return
		}
		if err := checkGate(ctx, opts.Gate); err != nil {
			stageErrs[i] = err
			return
		}
		r := pipeline.TileRect(p, t)
		pix := pipeline.TilePixels(b.Image, r)
		out, err := pipeline.RefineStage(pix, r, fgOut[i].Threshold, edgeOut[i], p.IterBudget, opts.ElementStep, p.ConvergeTolerance, func() error {
			return checkGate(ctx, opts.Gate)
		})
		refineOut[i] = out
		stageErrs[i] = err
	})
	var firstIterErr error
	var firstCtxErr error
	for i, t := range tiles {
		if _, ok := loaded.refine[t.Array()]; ok {
			continue
		}
		if stageErrs[i] != nil {
			if errors.Is(stageErrs[i], context.Canceled) {
				_ = mat.Transition(t, state.StageRefine, state.StatusPending)
				if firstCtxErr == nil {
					firstCtxErr = stageErrs[i]
				}
				continue
			}
			if ee, ok := errs.As(stageErrs[i]); ok && ee.Code == errs.EIter {
				_ = mat.Transition(t, state.StageRefine, state.StatusFailed)
				if firstIterErr == nil {
					firstIterErr = stageErrs[i]
				}
				continue
			}
			_ = mat.Transition(t, state.StageRefine, state.StatusFailed)
			return rep, stageErrs[i]
		}
		if err := mat.Transition(t, state.StageRefine, state.StatusCommitted); err != nil {
			return rep, err
		}
		conv := model.TileConvergence{Tile: t, Converged: refineOut[i].Converged, Residual: refineOut[i].FinalResidual, CommittedStages: 3}
		summary := fmt.Sprintf("budget=%d used=%d residual=%.4f converged=%v", p.IterBudget, refineOut[i].ActualBudget, refineOut[i].FinalResidual, refineOut[i].Converged)
		if err := commit(t, state.StageRefine, summary, conv, refineOut[i], p.IterBudget); err != nil {
			return rep, err
		}
	}
	if firstCtxErr != nil {
		return rep, firstCtxErr
	}

	globalMask := make([]byte, b.Manifest.Width*b.Manifest.Height)
	globalDev := make([]float64, b.Manifest.Width*b.Manifest.Height)
	converged := 0
	for i, t := range tiles {
		r := pipeline.TileRect(p, t)
		ro := refineOut[i]
		tr := model.TileResult{
			SampleID: b.Manifest.SampleID, Tile: t, Rect: r, Threshold: fgOut[i].Threshold,
			SeparationMask: fgOut[i].Mask, CleanMask: edgeOut[i].CleanMask, SkeletonMask: edgeOut[i].SkeletonMask,
			RefinedMask: ro.RefinedMask, DeviationMap: ro.DeviationMap, EdgePoints: ro.EdgePoints,
			Iterations: ro.Iterations, FinalResidual: ro.FinalResidual, Converged: ro.Converged,
			ActualBudget: ro.ActualBudget, ElementStep: ro.ElementStep, DecisionBasis: ro.DecisionBasis,
			ParamVersion: p.ParamVersion, AlgorithmVersion: model.AlgorithmVersion,
		}
		rep.Tiles = append(rep.Tiles, tr)
		rep.Convergence = append(rep.Convergence, model.TileConvergence{Tile: t, Converged: ro.Converged, Residual: ro.FinalResidual})
		if ro.Converged && stageErrs[i] == nil {
			converged++
		}
		for y := 0; y < r.H; y++ {
			gi := (r.Y+y)*b.Manifest.Width + r.X
			li := y * r.W
			copy(globalMask[gi:gi+r.W], ro.RefinedMask[li:li+r.W])
			for x := 0; x < r.W; x++ {
				globalDev[gi+x] = ro.DeviationMap[li+x]
			}
		}
	}
	rep.CommittedStages = mat.CommittedStages()
	rep.Progress = float64(rep.CommittedStages) / float64(rep.TotalStages)
	rep.ConvergedTiles = converged
	rep.ConvergedTileRatio = float64(converged) / float64(len(tiles))
	rep.MaskHash = model.HashBytes("global-refined-mask-v1:", globalMask)
	rep.DeviationHash = model.CanonicalHash(globalDev)
	rep.CommitSequence = mat.Sequence()
	rep.Reference = CompareReference(b.Reference, globalMask, b.Manifest.ReferenceMaskHash)

	if len(b.Manifest.Edges) > 0 {
		allPoints := pipeline.ExtractGlobalPoints(globalMask, b.Manifest.Width, b.Manifest.Height)
		acc := edgeeval.Evaluate(allPoints, b.Manifest.Edges, p.EdgeTolerance, p.EdgeUpperBound)
		rep.Accuracy = &acc
		if opts.StrictAccuracy {
			if err := edgeeval.AsEdgeError(acc); err != nil {
				return rep, err
			}
		}
	}
	if firstIterErr != nil {
		return rep, firstIterErr
	}
	if opts.StopAfterCommits > 0 && committedWrites >= opts.StopAfterCommits {
		return rep, ErrStopAfterCommit
	}
	return rep, nil
}

func countBytes(b []byte) int {
	n := 0
	for _, v := range b {
		if v != 0 {
			n++
		}
	}
	return n
}
