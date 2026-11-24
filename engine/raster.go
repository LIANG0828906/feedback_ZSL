package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
)

type PathCoverage struct {
	ID                string  `json:"id"`
	CoveredSamples    int64   `json:"coveredSamples"`
	TotalSamples      int64   `json:"totalSamples"`
	CoverageRatio     float64 `json:"coverageRatio"`
	CoverageBBoxRatio float64 `json:"coverageBBoxRatio"`
	PixelBounds       []int   `json:"pixelBounds"`
	Coverage          []byte  `json:"coverage"`
	FillCoverage      []byte  `json:"fillCoverage,omitempty"`
	StrokeCoverage    []byte  `json:"strokeCoverage,omitempty"`
}

type RenderedFrame struct {
	Time           int64           `json:"timeUs"`
	TimeMs         int64           `json:"timeMs"`
	Index          int             `json:"index"`
	Width          int             `json:"width"`
	Height         int             `json:"height"`
	RenderBounds   Rect            `json:"renderBounds"`
	GeometryBounds Rect            `json:"geometryBounds"`
	PixelBounds    []int           `json:"pixelBounds"`
	DimensionRule  string          `json:"dimensionRule"`
	Lines          []FlattenedPath `json:"lines"`
	LineCount      int             `json:"lineCount"`
	Paths          []PathCoverage  `json:"paths"`
	RGBA           []byte          `json:"-"`
	ContentHash    string          `json:"contentHash"`
	CommitSequence int             `json:"commitSequence"`
	Quality        QualityDecision `json:"quality"`
}

func (f *RenderedFrame) PNG() ([]byte, error) {
	if f.Width <= 0 || f.Height <= 0 || len(f.RGBA) != f.Width*f.Height*4 {
		return nil, &Error{Code: CodeComp, Path: "bitmap", Message: "invalid RGBA channel count"}
	}
	img := &image.NRGBA{Pix: f.RGBA, Stride: f.Width * 4, Rect: image.Rect(0, 0, f.Width, f.Height)}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderFrame evaluates, flattens, rasterizes and composites a single timestamp.
func RenderFrame(scene *Scene, time int64, index int, quality QualityDecision) (*RenderedFrame, error) {
	if scene == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	work := scene.Clone()
	if quality.Tolerance > 0 {
		work.Tolerance = quality.Tolerance
	}
	if quality.Subsamples > 0 {
		work.Subsamples = quality.Subsamples
	}
	if quality.PrecisionBits > 0 {
		work.Interpolation.PrecisionBits = quality.PrecisionBits
	}
	if err := ValidateParameters(work); err != nil {
		return nil, err
	}
	lines, err := FlattenAt(work, time)
	if err != nil {
		return nil, err
	}
	geometryBounds := Rect{}
	renderBounds := Rect{}
	for i := range work.Paths {
		p := &work.Paths[i]
		geometryBounds = geometryBounds.Union(lines[i].Bounds)
		expanded := lines[i].Bounds
		if p.HasStroke {
			expanded = expanded.Expand(divRound(p.StrokeWidth, 2, RoundCeil))
		}
		renderBounds = renderBounds.Union(expanded)
	}
	if renderBounds.Empty() {
		return nil, &Error{Code: CodeSelf, Path: fmt.Sprintf("frame.%d", index), Message: "empty frame bounds"}
	}
	width := int(max64(1, PixelLength(renderBounds.Width(), work.Scale)))
	height := int(max64(1, PixelLength(renderBounds.Height(), work.Scale)))
	if width*height > 64*1024*1024 {
		return nil, &Error{Code: CodeScale, Path: "bitmap", Frame: index, Metric: "pixels", Actual: float64(width * height), Limit: 64 * 1024 * 1024, Message: "bitmap exceeds buffer limit"}
	}
	frame := &RenderedFrame{
		Time:           time,
		TimeMs:         ticksToMillis(time, RoundFloor),
		Index:          index,
		Width:          width,
		Height:         height,
		RenderBounds:   renderBounds,
		GeometryBounds: geometryBounds,
		DimensionRule:  "ceil(renderBounds*scale/(CoordScale^2)); aaGrid reduces visible sample stride, not logical bounds",
		Lines:          lines,
		Quality:        quality,
	}
	frame.PixelBounds = []int{0, 0, width, height}
	frame.RGBA = make([]byte, width*height*4)
	for i := range work.Paths {
		pc, err := rasterizePath(&work.Paths[i], lines[i], renderBounds, width, height, work.Subsamples, work.Scale, index)
		if err != nil {
			return nil, err
		}
		if work.Paths[i].HasFill {
			compositePath(frame.RGBA, pc.FillCoverage, work.Paths[i], work.Paths[i].Fill, width*height)
		}
		if work.Paths[i].HasStroke {
			compositePath(frame.RGBA, pc.StrokeCoverage, work.Paths[i], work.Paths[i].StrokeColor, width*height)
		}
		frame.Paths = append(frame.Paths, *pc)
		frame.LineCount += lines[i].SegmentCount
	}
	frame.ContentHash = frameHash(frame, work)
	return frame, nil
}

func rasterizePath(path *Path, line FlattenedPath, bounds Rect, width, height, grid int, scale int64, frameIndex int) (*PathCoverage, error) {
	if !path.HasFill && !path.HasStroke {
		return nil, &Error{Code: CodeComp, Path: "paths." + path.ID, Message: "path has no enabled paint"}
	}
	if !validBlend(path.Blend) {
		return nil, &Error{Code: CodeComp, Path: "paths." + path.ID, Message: "invalid blend mode for path " + path.ID}
	}
	if grid == 0 {
		grid = 1
	}
	if grid < 1 || grid > 16 {
		return nil, &Error{Code: CodePara, Path: "subsamples", Field: "subsamples", Message: "sub-sample grid must be in [1,16]"}
	}
	poly := make([]Vec2f, len(line.Points))
	for i, p := range line.Points {
		poly[i] = Vec2f{
			X: ScaledCoord(p.X, bounds.MinX, scale),
			Y: ScaledCoord(p.Y, bounds.MinY, scale),
		}
	}
	var strokePolys [][]Vec2f
	if path.HasStroke && path.StrokeWidth > 0 {
		strokePolys = buildStrokePolygons(poly, float64(path.StrokeWidth)*float64(scale)/float64(CoordScale*CoordScale), path.LineJoin)
	}
	cov := make([]byte, width*height)
	fillCov := make([]byte, width*height)
	strokeCov := make([]byte, width*height)
	var covered int64
	samplesPerPixel := int64(grid * grid)
	for y := 0; y < height; y++ {
		rowCounts := make([]int, width)
		rowFill := make([]int, width)
		rowStroke := make([]int, width)
		for sy := 0; sy < grid; sy++ {
			py := float64(y) + (float64(sy)+0.5)/float64(grid)
			var fillIntervals []interval
			if path.HasFill {
				fillIntervals = polygonIntervals(poly, path.FillRule, py)
			}
			var strokeIntervals []interval
			for _, sp := range strokePolys {
				strokeIntervals = append(strokeIntervals, polygonIntervals(sp, "evenodd", py)...)
			}
			strokeIntervals = mergeIntervals(strokeIntervals)
			if len(fillIntervals) == 0 && len(strokeIntervals) == 0 {
				continue
			}
			for x := 0; x < width; x++ {
				for sx := 0; sx < grid; sx++ {
					px := float64(x) + (float64(sx)+0.5)/float64(grid)
					f := pointInIntervals(fillIntervals, px)
					s := pointInIntervals(strokeIntervals, px)
					if f {
						rowFill[x]++
					}
					if s {
						rowStroke[x]++
					}
					if f || s {
						rowCounts[x]++
					}
				}
			}
		}
		for x := 0; x < width; x++ {
			value := divRound(int64(rowCounts[x])*255, samplesPerPixel, RoundNearest)
			cov[y*width+x] = byte(value)
			fillCov[y*width+x] = byte(divRound(int64(rowFill[x])*255, samplesPerPixel, RoundNearest))
			strokeCov[y*width+x] = byte(divRound(int64(rowStroke[x])*255, samplesPerPixel, RoundNearest))
			covered += int64(rowCounts[x])
		}
	}
	totalSamples := int64(width) * int64(height) * samplesPerPixel
	ratio := 0.0
	if totalSamples > 0 {
		ratio = float64(covered) / float64(totalSamples)
	}
	pb := pathPixelBounds(poly, width, height)
	bboxSamples := int64((pb[2]-pb[0])*(pb[3]-pb[1])) * samplesPerPixel
	var bboxCovered int64
	for y := pb[1]; y < pb[3]; y++ {
		for x := pb[0]; x < pb[2]; x++ {
			bboxCovered += int64(cov[y*width+x]) * samplesPerPixel / 255
		}
	}
	bboxRatio := 0.0
	if bboxSamples > 0 {
		bboxRatio = float64(bboxCovered) / float64(bboxSamples)
	}
	pc := &PathCoverage{
		ID:                path.ID,
		CoveredSamples:    covered,
		TotalSamples:      totalSamples,
		CoverageRatio:     ratio,
		CoverageBBoxRatio: bboxRatio,
		PixelBounds:       pb,
		Coverage:          cov,
		FillCoverage:      fillCov,
		StrokeCoverage:    strokeCov,
	}
	if covered < 0 || covered > totalSamples {
		return nil, &Error{Code: CodeComp, Path: "paths." + path.ID, Frame: frameIndex, Message: "coverage channel count mismatch"}
	}
	return pc, nil
}

type interval struct {
	lo float64
	hi float64
}

type crossing struct {
	x   float64
	dir int
}

func polygonIntervals(poly []Vec2f, fillRule string, py float64) []interval {
	if len(poly) < 3 {
		return nil
	}
	crossings := make([]crossing, 0, len(poly))
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		if (a.Y > py) == (b.Y > py) {
			continue
		}
		x := (b.X-a.X)*(py-a.Y)/(b.Y-a.Y) + a.X
		dir := 1
		if b.Y < a.Y {
			dir = -1
		}
		crossings = append(crossings, crossing{x: x, dir: dir})
	}
	if len(crossings) < 2 {
		return nil
	}
	sort.Slice(crossings, func(i, j int) bool { return crossings[i].x < crossings[j].x })
	var out []interval
	if fillRule == "evenodd" {
		for i := 0; i+1 < len(crossings); i += 2 {
			if crossings[i+1].x > crossings[i].x {
				out = append(out, interval{lo: crossings[i].x, hi: crossings[i+1].x})
			}
		}
		return out
	}
	winding := 0
	start := 0.0
	for _, c := range crossings {
		prev := winding
		winding += c.dir
		if prev == 0 && winding != 0 {
			start = c.x
		} else if prev != 0 && winding == 0 && c.x > start {
			out = append(out, interval{lo: start, hi: c.x})
		}
	}
	return out
}

func mergeIntervals(in []interval) []interval {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].lo == in[j].lo {
			return in[i].hi < in[j].hi
		}
		return in[i].lo < in[j].lo
	})
	out := in[:1]
	for _, cur := range in[1:] {
		last := &out[len(out)-1]
		if cur.lo <= last.hi {
			if cur.hi > last.hi {
				last.hi = cur.hi
			}
			continue
		}
		out = append(out, cur)
	}
	return out
}

func pointInIntervals(intervals []interval, x float64) bool {
	i := sort.Search(len(intervals), func(i int) bool { return intervals[i].hi >= x })
	return i < len(intervals) && intervals[i].lo <= x
}

type Vec2f struct {
	X float64
	Y float64
}

func pointInPolygon(p Vec2f, poly []Vec2f, fillRule string) bool {
	if len(poly) < 3 {
		return false
	}
	if fillRule == "evenodd" {
		inside := false
		for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
			a, b := poly[i], poly[j]
			if (a.Y > p.Y) != (b.Y > p.Y) {
				x := (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y) + a.X
				if p.X < x {
					inside = !inside
				}
			}
		}
		return inside
	}
	winding := 0
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		side := (b.X-a.X)*(p.Y-a.Y) - (p.X-a.X)*(b.Y-a.Y)
		if a.Y <= p.Y {
			if b.Y > p.Y && side > 0 {
				winding++
			}
		} else if b.Y <= p.Y && side < 0 {
			winding--
		}
	}
	return winding != 0
}

func buildStrokePolygons(points []Vec2f, width float64, join JoinKind) [][]Vec2f {
	if len(points) < 2 || width <= 0 {
		return nil
	}
	hw := width / 2
	out := make([][]Vec2f, 0, len(points)*2)
	for i := range points {
		a, b := points[i], points[(i+1)%len(points)]
		dx, dy := b.X-a.X, b.Y-a.Y
		length := math.Hypot(dx, dy)
		if length < 1e-9 {
			continue
		}
		nx, ny := -dy/length*hw, dx/length*hw
		out = append(out, []Vec2f{
			{a.X + nx, a.Y + ny},
			{b.X + nx, b.Y + ny},
			{b.X - nx, b.Y - ny},
			{a.X - nx, a.Y - ny},
		})
	}
	for i := range points {
		v := points[(i+1)%len(points)]
		switch join {
		case JoinRound:
			poly := make([]Vec2f, 12)
			for k := range poly {
				ang := 2 * math.Pi * float64(k) / float64(len(poly))
				poly[k] = Vec2f{X: v.X + hw*math.Cos(ang), Y: v.Y + hw*math.Sin(ang)}
			}
			out = append(out, poly)
		case JoinBevel:
			prev := points[i]
			next := points[(i+2)%len(points)]
			pn := unitNormal(prev, v)
			nn := unitNormal(v, next)
			out = append(out, []Vec2f{
				v,
				{X: v.X + pn.X*hw, Y: v.Y + pn.Y*hw},
				{X: v.X + nn.X*hw, Y: v.Y + nn.Y*hw},
			})
			out = append(out, []Vec2f{
				v,
				{X: v.X - pn.X*hw, Y: v.Y - pn.Y*hw},
				{X: v.X - nn.X*hw, Y: v.Y - nn.Y*hw},
			})
		case JoinMiter:
			prev := points[i]
			next := points[(i+2)%len(points)]
			pn := unitNormal(prev, v)
			nn := unitNormal(v, next)
			miter := 2 * hw
			out = append(out, []Vec2f{
				{X: v.X + pn.X*hw, Y: v.Y + pn.Y*hw},
				{X: v.X + (pn.X+nn.X)*miter, Y: v.Y + (pn.Y+nn.Y)*miter},
				{X: v.X + nn.X*hw, Y: v.Y + nn.Y*hw},
			})
		}
	}
	return out
}

func unitNormal(a, b Vec2f) Vec2f {
	dx, dy := b.X-a.X, b.Y-a.Y
	l := math.Hypot(dx, dy)
	if l < 1e-9 {
		return Vec2f{}
	}
	return Vec2f{X: -dy / l, Y: dx / l}
}

func pathPixelBounds(poly []Vec2f, width, height int) []int {
	minX, minY := float64(width), float64(height)
	maxX, maxY := 0.0, 0.0
	for _, p := range poly {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
		maxX = math.Max(maxX, p.X)
		maxY = math.Max(maxY, p.Y)
	}
	x0 := maxInt(0, int(math.Floor(minX)))
	y0 := maxInt(0, int(math.Floor(minY)))
	x1 := minInt(width, int(math.Ceil(maxX)))
	y1 := minInt(height, int(math.Ceil(maxY)))
	return []int{x0, y0, x1, y1}
}

func compositePath(dst, coverage []byte, path Path, src Color, pixels int) {
	opacity := float64(path.Opacity) / 255
	if !path.HasFill && !path.HasStroke {
		return
	}
	srcA := float64(src.A) / 255 * opacity
	for i := 0; i < pixels; i++ {
		cov := float64(coverage[i]) / 255
		if cov == 0 {
			continue
		}
		sa := srcA * cov
		di := i * 4
		da := float64(dst[di+3]) / 255
		sr, sg, sb := float64(src.R)/255, float64(src.G)/255, float64(src.B)/255
		dr, dg, db := float64(dst[di])/255, float64(dst[di+1])/255, float64(dst[di+2])/255
		br := (1-da)*sr + da*blendChannel(path.Blend, dr, sr)
		bg := (1-da)*sg + da*blendChannel(path.Blend, dg, sg)
		bb := (1-da)*sb + da*blendChannel(path.Blend, db, sb)
		outA := sa + da*(1-sa)
		if outA <= 1e-12 {
			continue
		}
		dst[di] = uint8(math.Round(math.Max(0, math.Min(1, (br*sa+dr*da*(1-sa))/outA)) * 255))
		dst[di+1] = uint8(math.Round(math.Max(0, math.Min(1, (bg*sa+dg*da*(1-sa))/outA)) * 255))
		dst[di+2] = uint8(math.Round(math.Max(0, math.Min(1, (bb*sa+db*da*(1-sa))/outA)) * 255))
		dst[di+3] = uint8(math.Round(math.Max(0, math.Min(1, outA)) * 255))
	}
}

func blendChannel(mode BlendMode, dst, src float64) float64 {
	switch mode {
	case BlendMultiply:
		return dst * src
	case BlendScreen:
		return 1 - (1-dst)*(1-src)
	case BlendReplace:
		return src
	default:
		return src
	}
}

func frameHash(frame *RenderedFrame, scene *Scene) string {
	payload := struct {
		Time          int64           `json:"time"`
		Width         int             `json:"width"`
		Height        int             `json:"height"`
		RenderBounds  Rect            `json:"renderBounds"`
		Lines         []FlattenedPath `json:"lines"`
		Coverage      []string        `json:"coverage"`
		Tolerance     int64           `json:"tolerance"`
		PrecisionBits int             `json:"precisionBits"`
		Interpolation string          `json:"interpolation"`
		Subsamples    int             `json:"subsamples"`
		Blend         []BlendMode     `json:"blend"`
		BlendVersion  string          `json:"blendVersion"`
		AlgoVersion   string          `json:"algoVersion"`
		RGBA          []byte          `json:"rgba"`
	}{
		Time:          frame.Time,
		Width:         frame.Width,
		Height:        frame.Height,
		RenderBounds:  frame.RenderBounds,
		Lines:         frame.Lines,
		Tolerance:     scene.Tolerance,
		PrecisionBits: scene.Interpolation.PrecisionBits,
		Interpolation: scene.Interpolation.Mode,
		Subsamples:    scene.Subsamples,
		BlendVersion:  scene.BlendVersion,
		AlgoVersion:   scene.AlgorithmVersion,
		RGBA:          frame.RGBA,
	}
	for _, p := range frame.Paths {
		payload.Coverage = append(payload.Coverage, hex.EncodeToString(p.Coverage))
		payload.Coverage = append(payload.Coverage, hex.EncodeToString(p.FillCoverage))
		payload.Coverage = append(payload.Coverage, hex.EncodeToString(p.StrokeCoverage))
	}
	for _, p := range scene.Paths {
		payload.Blend = append(payload.Blend, p.Blend)
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
