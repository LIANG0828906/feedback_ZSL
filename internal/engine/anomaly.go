package engine

import (
	"context"

	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/render"
)

const (
	AnomalyMissingControl = "missing-control-point"
	AnomalyKnotOrder      = "knot-nonmonotonic"
	AnomalyDensity        = "density-out-of-range"
	AnomalyStage          = "tile-stage-failure"
)

type AnomalyResult struct {
	Kind       string            `json:"kind"`
	Diagnostic *model.Diagnostic `json:"diagnostic"`
	PatchID    int               `json:"patchId"`
	TileID     int               `json:"tileId"`
}

func (e *Engine) InjectAnomaly(ctx context.Context, kind string, tileID int) (AnomalyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureContext(ctx); err != nil {
		return AnomalyResult{}, err
	}
	base := e.state.Patches[0].Source
	res := AnomalyResult{Kind: kind, PatchID: base.ID, TileID: tileID}
	switch kind {
	case AnomalyMissingControl:
		p := base
		p.Control = append([]model.ControlPoint(nil), base.Control[:len(base.Control)-1]...)
		res.Diagnostic = model.ValidatePatch(p)
	case AnomalyKnotOrder:
		p := base
		p.UKnots = append([]float64(nil), base.UKnots...)
		p.UKnots[3] = 1.5
		res.Diagnostic = model.ValidatePatch(p)
	case AnomalyDensity:
		res.Diagnostic = geom.ValidateTessOptions(geom.TessOptions{
			Density: 0, CurvatureScale: e.state.Config.CurvatureScale,
			NormalTolDegrees: e.state.Config.NormalTolerance,
		})
		res.Diagnostic.PatchID = base.ID
		if res.Diagnostic.Field == "" {
			res.Diagnostic.Field = "density"
		}
	case AnomalyStage:
		if tileID < 0 || tileID >= e.state.Tiles.Count() {
			return AnomalyResult{}, model.DiagTile(model.CodePara, -1, tileID, "tileId", "tile id out of range")
		}
		for _, patch := range e.exact.Patches {
			for _, id := range render.PatchCoverage(e.exact, patch.PatchID, nil) {
				if id == tileID {
					res.PatchID = patch.PatchID
					break
				}
			}
		}
		oldFail := e.failTile
		e.failTile = tileID
		_, err := render.RenderTile(e.exact, tileID, render.RenderOptions{Context: ctx, FailTile: tileID})
		e.failTile = oldFail
		if err != nil {
			res.Diagnostic = diagnosticsFromErr(err)
			if res.Diagnostic.PatchID < 0 {
				res.Diagnostic.PatchID = res.PatchID
			}
		} else {
			res.Diagnostic = model.DiagTile(model.CodeStage, res.PatchID, tileID, "tileRaster", "stage failure was not observed")
		}
	default:
		return AnomalyResult{}, model.Diag(model.CodePara, "unknown anomaly kind")
	}
	if res.Diagnostic != nil {
		if res.Diagnostic.PatchID < 0 {
			res.Diagnostic.PatchID = res.PatchID
		}
		if res.Diagnostic.TileID < 0 && kind == AnomalyStage {
			res.Diagnostic.TileID = tileID
		}
	}
	return res, nil
}

func (e *Engine) RetryTile(ctx context.Context, tileID int) (*render.Frame, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureContext(ctx); err != nil {
		return nil, err
	}
	if tileID < 0 || tileID >= e.state.Tiles.Count() {
		return nil, model.DiagTile(model.CodePara, -1, tileID, "tileId", "tile id out of range")
	}
	e.failTile = -1
	tile, err := render.RenderTile(e.exact, tileID, render.RenderOptions{Context: ctx, FailTile: -1})
	if err != nil {
		return nil, err
	}
	tile.Path = render.PathIncremental
	tile.Version = e.state.Version
	e.cache[tileID] = tile
	frame := render.NewFrame(e.exact, render.PathIncremental, 0, mapTiles(e.cache))
	frame.CacheHits = len(e.cache) - 1
	frame.Rendered = 1
	if e.reference != nil {
		if d := render.CompareFrames(e.reference, frame); d != nil {
			return nil, d
		}
	}
	e.latestIncremental = frame
	meta := e.stateMeta(render.PathIncremental)
	if err := e.store.CommitTile(meta, tile, tileID); err != nil {
		return nil, model.DiagTile(model.CodeRecover, -1, tileID, "checkpoint", err.Error())
	}
	return frame, nil
}
