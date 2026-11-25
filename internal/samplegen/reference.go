package samplegen

import (
	"fmt"

	"example.com/vectorcore"
)

// renderReference assembles truth through analytic interpolation, flattening,
// stroke-band construction, deterministic scanline coverage and composition.
// It intentionally bypasses Engine.RenderAll, caching and checkpoint state so
// that truth generation never observes the production execution pipeline.
func renderReference(scene *vectorcore.Scene, cfg vectorcore.Config) ([]*vectorcore.TimeResult, error) {
	var results []*vectorcore.TimeResult
	for ti := range scene.SampleTimes {
		eval, err := vectorcore.EvaluateFrame(scene, ti, cfg)
		if err != nil {
			return nil, err
		}
		type refGeom struct {
			eval   vectorcore.EvaluatedPath
			fill   vectorcore.Polyline
			stroke []vectorcore.Polyline
			bounds vectorcore.Bounds
			area   int64
		}
		geoms := make([]refGeom, len(eval.Paths))
		frameBounds := vectorcore.EmptyBounds()
		for pi, ep := range eval.Paths {
			flat, err := vectorcore.FlattenContour(ep.Contour, cfg.Tolerance)
			if err != nil {
				return nil, err
			}
			rg := refGeom{eval: ep, fill: flat}
			bounds := vectorcore.EmptyBounds()
			if ep.Paint.Fill {
				for _, p := range flat.Points {
					bounds.AddPoint(p)
				}
			}
			if ep.Paint.StrokeWidth > 0 {
				rg.stroke, err = vectorcore.BuildStrokeBand(flat, ep.Paint.StrokeWidth, ep.Paint.Join)
				if err != nil {
					return nil, err
				}
				for _, ring := range rg.stroke {
					for _, p := range ring.Points {
						bounds.AddPoint(p)
					}
				}
			}
			rg.bounds = bounds
			rg.area, err = vectorcore.ContourAreaExact(ep.Contour)
			if err != nil {
				return nil, err
			}
			geoms[pi] = rg
			frameBounds.Union(bounds)
		}
		vp, err := vectorcore.DeriveViewport(frameBounds, cfg)
		if err != nil {
			return nil, err
		}
		prev, err := vectorcore.NewBitmap(vp.Width, vp.Height)
		if err != nil {
			return nil, err
		}
		tr := &vectorcore.TimeResult{
			SceneID: scene.ID, TimeIndex: ti, TimeTicks: eval.TimeTicks, CommitSeq: int64(ti + 1),
			Viewport: vp,
		}
		for _, rg := range geoms {
			cov, err := referenceCoverage(rg.eval.Paint, rg.fill, rg.stroke, vp, cfg)
			if err != nil {
				return nil, err
			}
			next, err := vectorcore.CompositePath(prev, cov, rg.eval.Paint, rg.eval.PathID)
			if err != nil {
				return nil, err
			}
			tr.Paths = append(tr.Paths, vectorcore.PathFrame{
				PathID: rg.eval.PathID, Contour: rg.eval.Contour.Clone(), Flattened: rg.fill.Clone(),
				StrokeBand: cloneRings(rg.stroke), Bounds: rg.bounds, AnalyticArea: rg.area,
				Coverage: cov.Clone(), CompositeAfter: next.Clone(),
			})
			prev = next
		}
		tr.Bitmap = prev.Clone()
		tr.ContentHash = vectorcore.HashValue(struct {
			TimeTicks int64
			Viewport  vectorcore.Viewport
			Paths     []vectorcore.PathFrame
		}{tr.TimeTicks, tr.Viewport, tr.Paths})
		results = append(results, tr)
	}
	return results, nil
}

func referenceCoverage(paint vectorcore.Paint, fill vectorcore.Polyline, stroke []vectorcore.Polyline, vp vectorcore.Viewport, cfg vectorcore.Config) (vectorcore.Coverage, error) {
	var result vectorcore.Coverage
	if len(stroke) > 0 {
		strokeCfg := cfg
		strokeCfg.FillRule = vectorcore.FillNonZero
		cov, err := vectorcore.RasterizeCoverage(stroke, vp, strokeCfg)
		if err != nil {
			return vectorcore.Coverage{}, err
		}
		result = cov
	}
	if paint.Fill {
		cov, err := vectorcore.RasterizeCoverage([]vectorcore.Polyline{fill}, vp, cfg)
		if err != nil {
			return vectorcore.Coverage{}, err
		}
		if result.Values == nil {
			result = cov
		} else {
			for i, v := range cov.Values {
				if v > result.Values[i] {
					result.Values[i] = v
				}
			}
		}
	}
	if result.Values == nil {
		return vectorcore.Coverage{}, fmt.Errorf("reference path produced no coverage")
	}
	return result, nil
}

func cloneRings(rings []vectorcore.Polyline) []vectorcore.Polyline {
	out := make([]vectorcore.Polyline, len(rings))
	for i, ring := range rings {
		out[i] = ring.Clone()
	}
	return out
}
