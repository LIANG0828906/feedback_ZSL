package morph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

type PathFrameStats struct {
	PathID             string    `json:"path_id"`
	SegmentCount       int       `json:"segment_count"`
	InterpolationCount int       `json:"interpolation_count"`
	CoverageArea       float64   `json:"coverage_area"`
	CoverageSum        float64   `json:"coverage_sum"`
	MeanCoverage       float64   `json:"mean_coverage"`
	PixelArea          int       `json:"pixel_area"`
	CoverageBBox       PixelRect `json:"coverage_bbox"`
	GeometryBBox       BBox      `json:"geometry_bbox"`
}

type PathRaster struct {
	PathID   string
	Rect     PixelRect
	Coverage []float32
	Alpha    []float32
	Premul   []float32
	Stats    PathFrameStats
}

type SampleResult struct {
	Index          int              `json:"index"`
	Time           float64          `json:"time"`
	Width          int              `json:"width"`
	Height         int              `json:"height"`
	Frame          FrameGeometry    `json:"frame"`
	Quality        Quality          `json:"quality"`
	Gray           []uint8          `json:"-"`
	Flattened      []*FlattenedPath `json:"-"`
	PathStats      []PathFrameStats `json:"path_stats"`
	PixelHash      string           `json:"pixel_hash"`
	ContentHash    string           `json:"content_hash"`
	Mode           string           `json:"mode"`
	Algorithm      string           `json:"algorithm"`
	ConfigVersion  string           `json:"config_version"`
	StyleVersion   string           `json:"style_version"`
	FromCheckpoint bool             `json:"from_checkpoint"`
}

type RasterOptions struct {
	Grid       int
	FailOnPath map[string]bool
}

func RasterizeSample(index int, t float64, scene *Scene, cfg Config, q Quality, failPlan map[string]bool) (*SampleResult, error) {
	if err := ValidateRenderConfig(RenderConfig{Grid: q.Grid, PaddingPixels: cfg.Render.PaddingPixels, MaxFramePixels: cfg.Render.MaxFramePixels}); err != nil {
		return nil, withSample(err.(*Error), index, t)
	}
	if err := ValidateEvalConfig(EvalConfig{Interpolation: cfg.Eval.Interpolation, Scale: cfg.Eval.Scale, MidTolerance: q.MidTolerance}, scene); err != nil {
		return nil, withSample(err.(*Error), index, t)
	}
	if !finite(t) {
		return nil, withSample(withField(newError(CodeParam, "sample time must be finite"), "sample_time"), index, t)
	}
	flattened := make([]*FlattenedPath, 0, len(scene.Paths))
	geometry := EmptyBBox()
	for pi := range scene.Paths {
		p := &scene.Paths[pi]
		flat, err := FlattenPath(p, t, EvalConfig{
			Interpolation: cfg.Eval.Interpolation,
			Scale:         cfg.Eval.Scale,
			MidTolerance:  q.MidTolerance,
		})
		if err != nil {
			if e, ok := AsError(err); ok {
				return nil, withSample(e, index, t)
			}
			return nil, withSample(withPath(newError(CodeParam, "path flattening failed"), p.ID), index, t)
		}
		flattened = append(flattened, flat)
		geometry = geometry.Union(flat.StrokeBBox)
	}
	frame, err := DeriveFrameGeometryAligned(geometry, cfg.Eval.Scale, cfg.Render.PaddingPixels, cfg.Render.MaxFramePixels, q.Grid)
	if err != nil {
		if e, ok := AsError(err); ok {
			return nil, withSample(e, index, t)
		}
		return nil, err
	}
	canvas := make([]float64, frame.Width*frame.Height)
	stats := make([]PathFrameStats, 0, len(scene.Paths))
	for pi := range scene.Paths {
		p := &scene.Paths[pi]
		if failPlan != nil && failPlan[p.ID] {
			return nil, withSample(withPath(withField(newError(CodeState, "injected path failure"), "coverage"), p.ID), index, t)
		}
		pr, err := RasterizePath(p, flattened[pi], frame, q.Grid)
		if err != nil {
			if e, ok := AsError(err); ok {
				return nil, withSample(e, index, t)
			}
			return nil, err
		}
		compositeRaster(canvas, frame.Width, frame.Height, p, pr)
		stats = append(stats, pr.Stats)
	}
	gray := make([]uint8, len(canvas))
	for i, v := range canvas {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		gray[i] = uint8(math.Round(v * 255))
	}
	res := &SampleResult{
		Index: index, Time: t, Width: frame.Width, Height: frame.Height, Frame: frame,
		Quality: q, Gray: gray, Flattened: flattened, PathStats: stats,
		PixelHash: HashBytes(gray), Mode: q.Mode, Algorithm: AlgorithmVersion,
		ConfigVersion: ConfigVersion(cfg), StyleVersion: SceneStyleVersion(scene),
	}
	res.ContentHash = sampleContentHash(res)
	return res, nil
}

func RasterizePath(p *Path, flat *FlattenedPath, frame FrameGeometry, grid int) (*PathRaster, error) {
	if grid < 1 || grid > 8 {
		return nil, withPath(withField(newError(CodeParam, "subsample grid must be in [1,8]"), "grid"), p.ID)
	}
	rect := pixelBBox(frame, flat.StrokeBBox)
	rect = rect.Intersect(PixelRect{0, 0, frame.Width, frame.Height})
	if rect.Empty() {
		return &PathRaster{PathID: p.ID, Stats: PathFrameStats{PathID: p.ID, SegmentCount: flat.SegmentCount, InterpolationCount: flat.InterpolationCount, GeometryBBox: flat.GeometryBBox}}, nil
	}
	area := rect.Area()
	fill := make([]float32, area)
	stroke := make([]float32, area)
	if p.Fill != nil {
		poly := make([]Vec2, len(flat.Points))
		for i, pt := range flat.Points {
			poly[i] = frame.WorldToPixel(pt)
		}
		rasterPolygon(fill, rect, poly, grid, p.FillRule)
	}
	if p.Stroke != nil {
		strokeComponents(p, flat, frame, rect, grid, stroke)
	}
	cov := make([]float32, area)
	alpha := make([]float32, area)
	premul := make([]float32, area)
	fillL := 0.0
	strokeL := 0.0
	if p.Fill != nil {
		fillL = luminance(p.Fill.Color)
	}
	if p.Stroke != nil {
		strokeL = luminance(p.Stroke.Color)
	}
	opacity := p.Opacity
	sum := 0.0
	meanBox := PixelRect{}
	for y := 0; y < rect.Height(); y++ {
		for x := 0; x < rect.Width(); x++ {
			i := y*rect.Width() + x
			fc := float64(fill[i])
			sc := float64(stroke[i])
			geom := math.Max(fc, sc)
			if p.Fill != nil && p.Stroke != nil {
				geom = 1 - (1-fc)*(1-sc)
			}
			cov[i] = float32(geom)
			sum += geom
			if geom > 1e-6 {
				px := rect.X0 + x
				py := rect.Y0 + y
				if meanBox.Empty() {
					meanBox = PixelRect{px, py, px + 1, py + 1}
				} else {
					meanBox = meanBox.Union(PixelRect{px, py, px + 1, py + 1})
				}
			}
			a := 0.0
			pre := 0.0
			if p.Fill != nil {
				a = fc * opacity
				pre = fillL * a
			}
			if p.Stroke != nil {
				sa := sc * opacity
				pre = strokeL*sa + pre*(1-sa)
				a = sa + a*(1-sa)
			}
			alpha[i] = float32(a)
			premul[i] = float32(pre)
		}
	}
	stats := PathFrameStats{
		PathID: p.ID, SegmentCount: flat.SegmentCount,
		InterpolationCount: flat.InterpolationCount,
		CoverageArea:       sum / (frame.Scale * frame.Scale),
		CoverageSum:        sum,
		PixelArea:          meanBox.Area(),
		CoverageBBox:       meanBox,
		GeometryBBox:       flat.GeometryBBox,
	}
	if !meanBox.Empty() {
		stats.MeanCoverage = sum / float64(meanBox.Area())
	}
	return &PathRaster{PathID: p.ID, Rect: rect, Coverage: cov, Alpha: alpha, Premul: premul, Stats: stats}, nil
}

func rasterPolygon(mask []float32, rect PixelRect, poly []Vec2, grid int, fillRule string) {
	if len(poly) < 3 || rect.Empty() {
		return
	}
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range poly {
		minY = math.Min(minY, p.Y)
		maxY = math.Max(maxY, p.Y)
	}
	y0 := maxInt(rect.Y0, int(math.Floor(minY)))
	y1 := minInt(rect.Y1, int(math.Ceil(maxY)))
	if y0 >= y1 {
		return
	}
	unit := 1.0 / float64(grid*grid)
	events := make([]crossEvent, 0, len(poly)+4)
	for py := y0; py < y1; py++ {
		for sy := 0; sy < grid; sy++ {
			y := float64(py) + (float64(sy)+0.5)/float64(grid)
			events = events[:0]
			for i := range poly {
				a := poly[i]
				b := poly[(i+1)%len(poly)]
				if (a.Y <= y && b.Y > y) || (b.Y <= y && a.Y > y) {
					t := (y - a.Y) / (b.Y - a.Y)
					x := a.X + t*(b.X-a.X)
					dir := 1
					if b.Y < a.Y {
						dir = -1
					}
					events = append(events, crossEvent{x: x, dir: dir})
				}
			}
			if len(events) < 2 {
				continue
			}
			sort.Slice(events, func(i, j int) bool {
				if events[i].x == events[j].x {
					return events[i].dir < events[j].dir
				}
				return events[i].x < events[j].x
			})
			if fillRule == "evenodd" {
				for i := 0; i+1 < len(events); i += 2 {
					addSpan(mask, rect, events[i].x, events[i+1].x, py, grid, unit)
				}
			} else {
				winding := 0
				spanStart := 0.0
				for _, ev := range events {
					prev := winding
					winding += ev.dir
					if prev == 0 && winding != 0 {
						spanStart = ev.x
					} else if prev != 0 && winding == 0 {
						addSpan(mask, rect, spanStart, ev.x, py, grid, unit)
					}
				}
			}
		}
	}
}

type crossEvent struct {
	x   float64
	dir int
}

func addSpan(mask []float32, rect PixelRect, xa, xb float64, py, grid int, unit float64) {
	if xb < xa {
		xa, xb = xb, xa
	}
	x0 := maxInt(rect.X0, int(math.Floor(xa)))
	x1 := minInt(rect.X1, int(math.Ceil(xb)))
	if x0 >= x1 {
		return
	}
	k0 := int(math.Ceil(xa*float64(grid) - 0.5))
	k1 := int(math.Ceil(xb*float64(grid)-0.5)) - 1
	width := rect.Width()
	row := (py - rect.Y0) * width
	for px := x0; px < x1; px++ {
		a := maxInt(k0, px*grid)
		b := minInt(k1, (px+1)*grid-1)
		if b < a {
			continue
		}
		coverage := float64(b-a+1) * unit
		idx := row + px - rect.X0
		v := float64(mask[idx]) + coverage
		if v > 1 {
			v = 1
		}
		mask[idx] = float32(v)
	}
}

func strokeComponents(p *Path, flat *FlattenedPath, frame FrameGeometry, rect PixelRect, grid int, mask []float32) {
	pts := flat.Points
	if len(pts) < 2 {
		return
	}
	scratch := make([]float32, rect.Area())
	r := p.Stroke.Width * 0.5
	scale := frame.Scale
	sign := 1.0
	if PolylineSignedArea(pts) < 0 {
		sign = -1
	}
	for i := 0; i+1 < len(pts); i++ {
		a, b := frame.WorldToPixel(pts[i]), frame.WorldToPixel(pts[i+1])
		ab := b.Sub(a)
		l := ab.Len()
		if l < 1e-9 {
			continue
		}
		n := Vec2{ab.Y / l, -ab.X / l}.Mul(r * scale)
		quad := []Vec2{a.Add(n), b.Add(n), b.Sub(n), a.Sub(n)}
		rasterComponentMax(mask, scratch, rect, quad, grid, "nonzero")
	}
	joinKinds := p.Stroke.Join
	if joinKinds == "" {
		joinKinds = "round"
	}
	for i := 1; i+1 < len(pts); i++ {
		prev := frame.WorldToPixel(pts[i-1])
		cur := frame.WorldToPixel(pts[i])
		next := frame.WorldToPixel(pts[i+1])
		d0 := cur.Sub(prev).Normalized()
		d1 := next.Sub(cur).Normalized()
		if d0.Len() < 0.5 || d1.Len() < 0.5 {
			continue
		}
		cross := d0.Cross(d1) * sign
		if cross <= 1e-9 {
			continue
		}
		out0 := Vec2{d0.Y, -d0.X}.Mul(sign * r * scale)
		out1 := Vec2{d1.Y, -d1.X}.Mul(sign * r * scale)
		a := cur.Add(out0)
		b := cur.Add(out1)
		switch p.Stroke.Join {
		case "bevel":
			rasterComponentMax(mask, scratch, rect, []Vec2{cur, a, b}, grid, "nonzero")
		case "miter":
			m := lineIntersection(a, d0, b, d1)
			limit := p.Stroke.MiterLimit
			if limit <= 0 {
				limit = 4
			}
			if m.Distance(cur) <= limit*r*scale {
				rasterComponentMax(mask, scratch, rect, []Vec2{cur, a, m, b}, grid, "nonzero")
			} else {
				rasterComponentMax(mask, scratch, rect, []Vec2{cur, a, b}, grid, "nonzero")
			}
		default:
			circle := circlePolygon(cur, r*scale, maxInt(16, grid*4))
			rasterComponentMax(mask, scratch, rect, circle, grid, "nonzero")
		}
	}
}

// rasterComponentMax rasterizes one polygon with normal coverage accumulation,
// then merges it into a union mask with max. Taking max per subrow directly into
// the union mask would under-count coverage.
func rasterComponentMax(dst, scratch []float32, rect PixelRect, poly []Vec2, grid int, fillRule string) {
	if len(poly) < 3 || rect.Empty() {
		return
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range poly {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
		maxX = math.Max(maxX, p.X)
		maxY = math.Max(maxY, p.Y)
	}
	clip := PixelRect{
		maxInt(rect.X0, int(math.Floor(minX))-1),
		maxInt(rect.Y0, int(math.Floor(minY))-1),
		minInt(rect.X1, int(math.Ceil(maxX))+1),
		minInt(rect.Y1, int(math.Ceil(maxY))+1),
	}.Intersect(rect)
	if clip.Empty() {
		return
	}
	width := rect.Width()
	for y := clip.Y0; y < clip.Y1; y++ {
		row := (y - rect.Y0) * width
		for x := clip.X0; x < clip.X1; x++ {
			scratch[row+x-rect.X0] = 0
		}
	}
	rasterPolygon(scratch, rect, poly, grid, fillRule)
	for y := clip.Y0; y < clip.Y1; y++ {
		row := (y - rect.Y0) * width
		for x := clip.X0; x < clip.X1; x++ {
			i := row + x - rect.X0
			if scratch[i] > dst[i] {
				dst[i] = scratch[i]
			}
		}
	}
}

func lineIntersection(a Vec2, da Vec2, b Vec2, db Vec2) Vec2 {
	den := da.Cross(db)
	if math.Abs(den) < 1e-9 {
		return a.Add(b).Mul(0.5)
	}
	t := b.Sub(a).Cross(db) / den
	return a.Add(da.Mul(t))
}

func circlePolygon(c Vec2, r float64, n int) []Vec2 {
	if n < 8 {
		n = 8
	}
	out := make([]Vec2, n)
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		out[i] = Vec2{c.X + r*math.Cos(a), c.Y + r*math.Sin(a)}
	}
	return out
}

func pixelBBox(frame FrameGeometry, b BBox) PixelRect {
	if b.IsEmpty() {
		return PixelRect{}
	}
	p0 := frame.WorldToPixel(Vec2{b.Min.X, b.Max.Y})
	p1 := frame.WorldToPixel(Vec2{b.Max.X, b.Min.Y})
	x0 := int(math.Floor(p0.X)) - 1
	y0 := int(math.Floor(p0.Y)) - 1
	x1 := int(math.Ceil(p1.X)) + 1
	y1 := int(math.Ceil(p1.Y)) + 1
	return PixelRect{maxInt(0, x0), maxInt(0, y0), minInt(frame.Width, x1), minInt(frame.Height, y1)}
}

func compositeRaster(canvas []float64, w, h int, p *Path, pr *PathRaster) {
	if pr == nil || len(pr.Alpha) == 0 {
		return
	}
	for y := 0; y < pr.Rect.Height(); y++ {
		for x := 0; x < pr.Rect.Width(); x++ {
			i := y*pr.Rect.Width() + x
			a := float64(pr.Alpha[i])
			if a <= 1e-7 {
				continue
			}
			src := float64(pr.Premul[i])
			di := (pr.Rect.Y0+y)*w + pr.Rect.X0 + x
			dst := canvas[di]
			var out float64
			switch p.BlendMode {
			case "add":
				out = math.Min(1, dst+src)
			case "multiply":
				// Straight source color is src/a. Multiply blend then source-over.
				out = dst*(1-a) + dst*(src/a)*a
				out = dst*(1-a) + dst*src
			case "screen":
				c := src / a
				blend := 1 - (1-c)*(1-dst)
				out = dst*(1-a) + a*blend
			case "replace":
				out = src + dst*(1-a)
			default:
				out = src + dst*(1-a)
			}
			if out < 0 {
				out = 0
			}
			if out > 1 {
				out = 1
			}
			canvas[di] = out
		}
	}
}

func luminance(c [3]float64) float64 {
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func sampleContentHash(s *SampleResult) string {
	type hashPayload struct {
		Algorithm string
		SceneMode string
		Quality   Quality
		Width     int
		Height    int
		Frame     FrameGeometry
		Stats     []PathFrameStats
		PixelHash string
	}
	payload := hashPayload{
		Algorithm: AlgorithmVersion, SceneMode: s.Mode, Quality: s.Quality,
		Width: s.Width, Height: s.Height, Frame: s.Frame,
		Stats: s.PathStats, PixelHash: s.PixelHash,
	}
	data, _ := json.Marshal(payload)
	return HashBytes(data)
}
