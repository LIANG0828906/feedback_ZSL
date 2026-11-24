package vision

import (
	"fmt"
	"image"
	"math"
	"sort"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/font"
)

type Recognizer struct {
	Params        core.SegmentParams
	ImageID       string
	ParameterHash string
	Fault         func(lineIndex int) error
}

func (r Recognizer) Recognize(src image.Image, line core.TextLine) (core.LineResult, error) {
	if err := core.ValidateSegmentParams(r.Params); err != nil {
		return core.LineResult{}, err
	}
	if r.Fault != nil {
		if err := r.Fault(line.ID); err != nil {
			return core.LineResult{}, err
		}
	}
	result := core.LineResult{
		ImageID:       r.ImageID,
		Number:        line.ID,
		ReadingIndex:  line.ReadingIndex,
		TileIndex:     firstTile(line.SourceTiles),
		Box:           line.Box,
		Text:          "",
		Characters:    []core.CharResult{},
		Confidence:    0,
		IsEmpty:       false,
		StrokeWidth:   line.StrokeWidth,
		SegmentParams: r.Params,
		ParameterHash: r.ParameterHash,
	}
	gray := ToGray(src)
	roi := core.BoxBounds(line.Box)
	pad := int(math.Ceil(math.Max(3, line.StrokeWidth*3)))
	roi.X -= pad
	roi.Y -= pad
	roi.Width += 2 * pad
	roi.Height += 2 * pad
	roi = core.Rect{
		X:      maxInt(0, roi.X),
		Y:      maxInt(0, roi.Y),
		Width:  minInt(gray.Width, roi.Right()) - maxInt(0, roi.X),
		Height: minInt(gray.Height, roi.Bottom()) - maxInt(0, roi.Y),
	}
	if roi.Empty() {
		result.IsEmpty = true
		result.EmptyReason = "EMPTY_ROI"
		return result, nil
	}
	params := core.DefaultDetectorParams()
	params.MinComponentArea = r.Params.MinCharArea
	params.MinComponentPixels = r.Params.MinCharArea
	params.AdaptiveRadius = 2
	bin, err := Binarize(gray, roi, params.AdaptiveRadius)
	if err != nil {
		return core.LineResult{}, err
	}
	components := DetectComponents(bin, roi, params, result.TileIndex)
	if len(components) == 0 {
		result.IsEmpty = true
		result.EmptyReason = "NO_FOREGROUND_COMPONENTS"
		return result, nil
	}
	center := core.BoxCenter(line.Box)
	u := direction(line.Box[0], line.Box[1])
	v := direction(line.Box[0], line.Box[3])
	lineWidth := pointDistance(line.Box[0], line.Box[1])
	lineHeight := pointDistance(line.Box[0], line.Box[3])
	filtered := make([]glyphComponent, 0, len(components))
	for _, component := range components {
		localCX := (component.Center.X+0.5-center.X)*u.X + (component.Center.Y+0.5-center.Y)*u.Y
		localCY := (component.Center.X+0.5-center.X)*v.X + (component.Center.Y+0.5-center.Y)*v.Y
		if math.Abs(localCY) > lineHeight/2+line.StrokeWidth*1.5 {
			continue
		}
		if localCX < -lineWidth/2-line.StrokeWidth*2 || localCX > lineWidth/2+line.StrokeWidth*2 {
			continue
		}
		if float64(component.Rect.Width) > r.Params.MaxCharWidth {
			continue
		}
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		for _, pixel := range component.LocalPixels {
			lx := (pixel.X+0.5-center.X)*u.X + (pixel.Y+0.5-center.Y)*u.Y
			ly := (pixel.X+0.5-center.X)*v.X + (pixel.Y+0.5-center.Y)*v.Y
			minX, maxX = math.Min(minX, lx), math.Max(maxX, lx)
			minY, maxY = math.Min(minY, ly), math.Max(maxY, ly)
		}
		if len(component.LocalPixels) == 0 {
			continue
		}
		filtered = append(filtered, glyphComponent{
			component: component,
			localX:    localCX,
			localY:    localCY,
			minX:      minX,
			minY:      minY,
			maxX:      maxX,
			maxY:      maxY,
		})
	}
	sort.Slice(filtered, func(i, j int) bool {
		if math.Abs(filtered[i].localX-filtered[j].localX) > 1e-6 {
			return filtered[i].localX < filtered[j].localX
		}
		return filtered[i].localY < filtered[j].localY
	})
	groups := clusterGlyphFragments(filtered, line.StrokeWidth)
	type glyph struct {
		pixels []core.Point
		localX float64
		localY float64
		minX   float64
		minY   float64
		maxX   float64
		maxY   float64
	}
	glyphs := make([]glyph, 0, len(groups))
	for _, group := range groups {
		g := glyph{}
		g.minX, g.minY = math.Inf(1), math.Inf(1)
		g.maxX, g.maxY = math.Inf(-1), math.Inf(-1)
		for _, item := range group {
			g.pixels = append(g.pixels, item.component.LocalPixels...)
			g.localX += item.localX
			g.localY += item.localY
			g.minX, g.maxX = math.Min(g.minX, item.minX), math.Max(g.maxX, item.maxX)
			g.minY, g.maxY = math.Min(g.minY, item.minY), math.Max(g.maxY, item.maxY)
		}
		g.localX /= float64(len(group))
		g.localY /= float64(len(group))
		if len(g.pixels) == 0 || g.maxX-g.minX < 0.5 || g.maxY-g.minY < 0.5 {
			continue
		}
		glyphs = append(glyphs, g)
	}
	if len(glyphs) == 0 {
		result.IsEmpty = true
		result.EmptyReason = "NO_LINE_COMPONENTS"
		return result, nil
	}
	lineConfidenceSum := 0.0
	runeList := font.Runes()
	for index, glyph := range glyphs {
		cells := sampleGlyph(glyph.pixels, center, u, v)
		bestRune := rune('?')
		bestScore, secondScore := -1.0, -1.0
		for _, candidate := range runeList {
			score := font.Score(cells, candidate)
			if score > bestScore {
				secondScore = bestScore
				bestScore = score
				bestRune = candidate
			} else if score > secondScore {
				secondScore = score
			}
		}
		if secondScore < 0 {
			secondScore = 0
		}
		margin := core.Clamp01(bestScore - secondScore)
		confidence := ConfidenceFromScores(bestScore, secondScore, r.Params.MarginScale)
		if bestScore < r.Params.MatchThreshold {
			bestRune = '?'
		}
		charBox := globalGlyphBox(glyph.minX, glyph.minY, glyph.maxX, glyph.maxY, center, u, v)
		result.Characters = append(result.Characters, core.CharResult{
			Index:       index,
			Text:        string(bestRune),
			Confidence:  confidence,
			Box:         charBox,
			BestScore:   bestScore,
			SecondScore: secondScore,
			Margin:      margin,
		})
		lineConfidenceSum += confidence
		result.Text += string(bestRune)
	}
	result.Confidence = core.Clamp01(lineConfidenceSum / float64(len(result.Characters)))
	return result, nil
}

func sampleGlyph(pixels []core.Point, center, u, v core.Point) [font.Height][font.Width]bool {
	var cells [font.Height][font.Width]bool
	if len(pixels) == 0 {
		return cells
	}
	type localPixel struct {
		x float64
		y float64
	}
	local := make([]localPixel, 0, len(pixels))
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, pixel := range pixels {
		lx := (pixel.X+0.5-center.X)*u.X + (pixel.Y+0.5-center.Y)*u.Y
		ly := (pixel.X+0.5-center.X)*v.X + (pixel.Y+0.5-center.Y)*v.Y
		local = append(local, localPixel{x: lx, y: ly})
		minX, maxX = math.Min(minX, lx), math.Max(maxX, lx)
		minY, maxY = math.Min(minY, ly), math.Max(maxY, ly)
	}
	width := maxX - minX + 1
	height := maxY - minY + 1
	if width <= 0 || height <= 0 {
		return cells
	}
	counts := [font.Height][font.Width]int{}
	for _, pixel := range local {
		tx := int((pixel.x - minX) / width * font.Width)
		ty := int((pixel.y - minY) / height * font.Height)
		tx = maxInt(0, minInt(font.Width-1, tx))
		ty = maxInt(0, minInt(font.Height-1, ty))
		counts[ty][tx]++
	}
	cellArea := width * height / float64(font.Width*font.Height)
	threshold := int(math.Ceil(cellArea * 0.38))
	if threshold < 2 {
		threshold = 2
	}
	for ty := 0; ty < font.Height; ty++ {
		for tx := 0; tx < font.Width; tx++ {
			if counts[ty][tx] >= threshold {
				cells[ty][tx] = true
			}
		}
	}
	return cells
}

type glyphComponent struct {
	component core.Component
	localX    float64
	localY    float64
	minX      float64
	minY      float64
	maxX      float64
	maxY      float64
}

func clusterGlyphFragments(items []glyphComponent, stroke float64) [][]glyphComponent {
	if len(items) == 0 {
		return nil
	}
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	tolerance := math.Max(1, stroke*0.5)
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			xGap := math.Max(0, math.Max(a.minX, b.minX)-math.Min(a.maxX, b.maxX))
			combinedWidth := math.Max(a.maxX, b.maxX) - math.Min(a.minX, b.minX)
			// Fragments caused by rotated rasterization overlap in the line's
			// local X axis but may be separated vertically. Adjacent glyphs
			// never overlap that axis within one font cell.
			if xGap <= tolerance && combinedWidth <= stroke*10 {
				union(i, j)
			}
		}
	}
	grouped := map[int][]glyphComponent{}
	for i, item := range items {
		root := find(i)
		grouped[root] = append(grouped[root], item)
	}
	roots := make([]int, 0, len(grouped))
	for root := range grouped {
		roots = append(roots, root)
	}
	sort.Ints(roots)
	out := make([][]glyphComponent, 0, len(roots))
	for _, root := range roots {
		group := grouped[root]
		sort.Slice(group, func(i, j int) bool { return group[i].localX < group[j].localX })
		out = append(out, group)
	}
	return out
}

func globalGlyphBox(minX, minY, maxX, maxY float64, center, u, v core.Point) core.Box {
	local := [][2]float64{
		{minX, minY}, {maxX, minY}, {maxX, maxY}, {minX, maxY},
	}
	points := make([]core.Point, 0, 4)
	for _, p := range local {
		points = append(points, core.Point{
			X: center.X + p[0]*u.X + p[1]*v.X,
			Y: center.Y + p[0]*u.Y + p[1]*v.Y,
		})
	}
	return core.ProjectBox(points, math.Atan2(u.Y, u.X))
}

func direction(a, b core.Point) core.Point {
	dx, dy := b.X-a.X, b.Y-a.Y
	length := math.Hypot(dx, dy)
	if length < 1e-9 {
		return core.Point{X: 1}
	}
	return core.Point{X: dx / length, Y: dy / length}
}

func pointDistance(a, b core.Point) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

func firstTile(tiles []int) int {
	if len(tiles) == 0 {
		return 0
	}
	return tiles[0]
}

func ConfidenceFromScores(best, second, marginScale float64) float64 {
	margin := core.Clamp01(best - second)
	return core.Clamp01(0.5*core.Clamp01(best) + 0.5*core.Clamp01(margin*marginScale))
}

func TemplateVersion() string { return font.Version }

func DebugGlyphScore(pattern rune, candidate [font.Height][font.Width]bool) (float64, error) {
	if _, ok := font.Pattern(pattern); !ok {
		return 0, fmt.Errorf("unknown pattern %q", pattern)
	}
	return font.Score(candidate, pattern), nil
}
