package vectorcore

import "fmt"

type pathGeometry struct {
	Contour         Contour
	Paint           Paint
	Flattened       Polyline
	StrokeBand      []Polyline
	Bounds          Bounds
	AreaExact       int64
	KeyframeIndices []int
	ContentHash     string
}

func buildPathGeometry(path EvaluatedPath, cfg Config) (*pathGeometry, error) {
	flat, err := FlattenContour(path.Contour, cfg.Tolerance)
	if err != nil {
		return nil, err
	}
	geo := &pathGeometry{Contour: path.Contour, Paint: path.Paint, Flattened: flat, KeyframeIndices: append([]int(nil), path.KeyframeIndex...)}
	bounds := EmptyBounds()
	if path.Paint.Fill {
		for _, p := range flat.Points {
			bounds.AddPoint(p)
		}
	}
	if path.Paint.StrokeWidth > 0 {
		rings, err := BuildStrokeBand(flat, path.Paint.StrokeWidth, path.Paint.Join)
		if err != nil {
			return nil, err
		}
		geo.StrokeBand = rings
		for _, ring := range rings {
			for _, p := range ring.Points {
				bounds.AddPoint(p)
			}
		}
	}
	if bounds.Empty() {
		return nil, &Error{Code: CodeSchema, Op: "geometry", PathID: path.PathID, Field: "paint", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("path produced no fill or stroke geometry")}
	}
	geo.Bounds = bounds
	area, err := ContourAreaExact(path.Contour)
	if err != nil {
		return nil, &Error{Code: CodeOverflow, Op: "area", PathID: path.PathID, Field: "contour", Keyframe: -1, TimeIndex: -1, Err: err}
	}
	geo.AreaExact = area
	geo.ContentHash = HashValue(struct {
		Contour   Contour
		Fill      bool
		Width     int64
		Join      JoinMode
		Tolerance int64
		Keyframes []int
	}{
		Contour: path.Contour, Fill: path.Paint.Fill, Width: path.Paint.StrokeWidth,
		Join: path.Paint.Join, Tolerance: cfg.Tolerance, Keyframes: path.KeyframeIndex,
	})
	return geo, nil
}

func makePathCoverage(geo *pathGeometry, vp Viewport, cfg Config) (Coverage, error) {
	var result Coverage
	if geo.Contour.Segments == nil {
		return Coverage{}, &Error{Code: CodeSchema, Op: "coverage", Field: "contour", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("missing contour")}
	}
	if cfg.FillRule != FillEvenOdd {
		cfg.FillRule = FillNonZero
	}
	if len(geo.StrokeBand) > 0 {
		strokeCfg := cfg
		strokeCfg.FillRule = FillNonZero
		cov, err := RasterizeCoverage(geo.StrokeBand, vp, strokeCfg)
		if err != nil {
			return Coverage{}, err
		}
		result = cov
	}
	if geo.Paint.Fill {
		fillCfg := cfg
		cov, err := RasterizeCoverage([]Polyline{geo.Flattened}, vp, fillCfg)
		if err != nil {
			return Coverage{}, err
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
		return Coverage{}, &Error{Code: CodeSchema, Op: "coverage", Field: "paint", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("no coverage generated")}
	}
	return result, nil
}
