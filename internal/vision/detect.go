package vision

import (
	"fmt"
	"math"
	"sort"

	"ocrworkbench/internal/core"
)

type TileDetection struct {
	Index      int             `json:"index"`
	Row        int             `json:"row"`
	Col        int             `json:"col"`
	Core       core.Rect       `json:"core"`
	Expanded   core.Rect       `json:"expanded"`
	Candidates []core.TextLine `json:"candidates"`
}

type Tile struct {
	Index    int
	Row      int
	Col      int
	Core     core.Rect
	Expanded core.Rect
}

func PlanTiles(width, height int, p core.DetectorParams) []Tile {
	var tiles []Tile
	index := 0
	for y := 0; y < height; y += p.TileHeight {
		for x := 0; x < width; x += p.TileWidth {
			coreRect := core.Rect{
				X:      x,
				Y:      y,
				Width:  minInt(p.TileWidth, width-x),
				Height: minInt(p.TileHeight, height-y),
			}
			expanded := core.Rect{
				X:      maxInt(0, x-p.Band),
				Y:      maxInt(0, y-p.Band),
				Width:  minInt(width, x+p.TileWidth+p.Band) - maxInt(0, x-p.Band),
				Height: minInt(height, y+p.TileHeight+p.Band) - maxInt(0, y-p.Band),
			}
			tiles = append(tiles, Tile{
				Index:    index,
				Row:      y / p.TileHeight,
				Col:      x / p.TileWidth,
				Core:     coreRect,
				Expanded: expanded,
			})
			index++
		}
	}
	return tiles
}

func DetectTile(gray *GrayImage, tile Tile, p core.DetectorParams) ([]core.TextLine, error) {
	bin, err := Binarize(gray, tile.Expanded, p.AdaptiveRadius)
	if err != nil {
		return nil, err
	}
	components := DetectComponents(bin, tile.Expanded, p, tile.Index)
	if len(components) == 0 {
		return nil, nil
	}
	groups := groupComponents(components, p)
	lines := make([]core.TextLine, 0, len(groups))
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		angle := estimateAngle(group)
		var points []core.Point
		strokeSum, count := 0.0, 0
		for _, component := range group {
			points = append(points, component.LocalPixels...)
			strokeSum += component.StrokeWidth
			count++
		}
		if len(points) == 0 {
			continue
		}
		box := core.ProjectBox(points, angle)
		if !core.ValidBox(box) {
			continue
		}
		tiles := make([]int, 0, len(group))
		for _, component := range group {
			tiles = append(tiles, component.SourceTile)
		}
		line := core.TextLine{
			Box:            box,
			AngleRadians:   angle,
			StrokeWidth:    strokeSum / float64(count),
			ComponentCount: len(group),
			SourceTiles:    core.SortedUniqueInts(tiles),
		}
		hash, hashErr := core.HashAny(struct {
			Box          core.Box `json:"box"`
			Angle        float64  `json:"angle"`
			Stroke       float64  `json:"stroke"`
			ComponentCnt int      `json:"component_count"`
		}{line.Box, line.AngleRadians, line.StrokeWidth, line.ComponentCount})
		if hashErr != nil {
			return nil, hashErr
		}
		line.CandidateHash = hash
		lines = append(lines, line)
	}
	sortLines(lines)
	for i := range lines {
		lines[i].ReadingIndex = i
		lines[i].ID = i
	}
	return lines, nil
}

func DetectWhole(gray *GrayImage, p core.DetectorParams) ([]core.TextLine, error) {
	tile := Tile{
		Index:    0,
		Core:     core.Rect{Width: gray.Width, Height: gray.Height},
		Expanded: core.Rect{Width: gray.Width, Height: gray.Height},
	}
	lines, err := DetectTile(gray, tile, p)
	if err != nil {
		return nil, err
	}
	return MergeTileCandidates(lines, p)
}

func groupComponents(components []core.Component, p core.DetectorParams) [][]core.Component {
	if len(components) == 0 {
		return nil
	}
	parent := make([]int, len(components))
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
	for i := 0; i < len(components); i++ {
		for j := i + 1; j < len(components); j++ {
			if compatibleComponents(components[i], components[j], p) {
				union(i, j)
			}
		}
	}
	groupsByRoot := map[int][]core.Component{}
	for i, component := range components {
		root := find(i)
		groupsByRoot[root] = append(groupsByRoot[root], component)
	}
	roots := make([]int, 0, len(groupsByRoot))
	for root := range groupsByRoot {
		roots = append(roots, root)
	}
	sort.Ints(roots)
	groups := make([][]core.Component, 0, len(roots))
	for _, root := range roots {
		group := groupsByRoot[root]
		sort.Slice(group, func(i, j int) bool {
			if group[i].Center.X != group[j].Center.X {
				return group[i].Center.X < group[j].Center.X
			}
			return group[i].Center.Y < group[j].Center.Y
		})
		groups = append(groups, group)
	}
	return groups
}

func compatibleComponents(a, b core.Component, p core.DetectorParams) bool {
	maxStroke := math.Max(a.StrokeWidth, b.StrokeWidth)
	if maxStroke <= 0 {
		return false
	}
	strokeDelta := math.Abs(a.StrokeWidth-b.StrokeWidth) / maxStroke
	if strokeDelta > p.StrokeRatioTolerance {
		return false
	}
	horizontalGap := math.Max(0, float64(maxInt(a.Rect.X, b.Rect.X)-minInt(a.Rect.Right(), b.Rect.Right())))
	verticalGap := math.Max(0, float64(maxInt(a.Rect.Y, b.Rect.Y)-minInt(a.Rect.Bottom(), b.Rect.Bottom())))
	maxHeight := float64(maxInt(a.Rect.Height, b.Rect.Height))
	centerDy := math.Abs(a.Center.Y - b.Center.Y)
	centerDx := math.Abs(a.Center.X - b.Center.X)
	nearHorizontal := horizontalGap <= math.Max(4.5*maxStroke, 0.85*maxHeight)
	aligned := centerDy <= 0.72*maxHeight+0.18*horizontalGap
	nearVertical := verticalGap <= 1.4*maxStroke && centerDx <= 1.2*maxHeight
	return (nearHorizontal && aligned) || nearVertical
}

func estimateAngle(group []core.Component) float64 {
	if len(group) < 2 {
		return 0
	}
	// Least-squares slope through component centers. This is stable for mixed
	// glyph heights because every connected component contributes one point.
	var sumX, sumY, sumXX, sumXY float64
	n := float64(len(group))
	for _, component := range group {
		x, y := component.Center.X, component.Center.Y
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
	}
	den := n*sumXX - sumX*sumX
	if math.Abs(den) < 1e-9 {
		return 0
	}
	slope := (n*sumXY - sumX*sumY) / den
	// A one-glyph-wide tile fragment can have an unstable extreme slope.
	angle := math.Atan(slope)
	if angle > 0.35 {
		angle = 0.35
	}
	if angle < -0.35 {
		angle = -0.35
	}
	return angle
}

func sortLines(lines []core.TextLine) {
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		ac, bc := core.BoxCenter(a.Box), core.BoxCenter(b.Box)
		// A 10-pixel row bucket keeps dense two-column layouts in row-major
		// order despite sub-pixel center variation from skew.
		aRow := int(math.Round(ac.Y / 10))
		bRow := int(math.Round(bc.Y / 10))
		if aRow != bRow {
			return aRow < bRow
		}
		if math.Abs(ac.X-bc.X) > 0.01 {
			return ac.X < bc.X
		}
		return a.CandidateHash < b.CandidateHash
	})
}

func MergeTileCandidates(candidates []core.TextLine, p core.DetectorParams) ([]core.TextLine, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	parent := make([]int, len(candidates))
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
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			a, b := candidates[i], candidates[j]
			maxStroke := math.Max(a.StrokeWidth, b.StrokeWidth)
			if maxStroke <= 0 || math.Abs(a.StrokeWidth-b.StrokeWidth)/maxStroke > p.MergeStrokeRatio {
				continue
			}
			iou := core.BoxIoU(a.Box, b.Box)
			if iou >= p.MergeIoU {
				union(i, j)
				continue
			}
			intersection := core.BoxIntersectionArea(a.Box, b.Box)
			minArea := math.Min(core.BoxArea(a.Box), core.BoxArea(b.Box))
			if minArea > 0 && intersection/minArea >= 0.72 {
				union(i, j)
				continue
			}
			ac, bc := core.BoxCenter(a.Box), core.BoxCenter(b.Box)
			ar, br := core.BoxArea(a.Box), core.BoxArea(b.Box)
			minArea = math.Min(ar, br)
			distance := math.Hypot(ac.X-bc.X, ac.Y-bc.Y)
			if minArea > 0 && distance < 0.22*math.Sqrt(minArea) {
				union(i, j)
			}
		}
	}
	clusters := map[int][]core.TextLine{}
	for i, candidate := range candidates {
		root := find(i)
		clusters[root] = append(clusters[root], candidate)
	}
	roots := make([]int, 0, len(clusters))
	for root := range clusters {
		roots = append(roots, root)
	}
	sort.Ints(roots)
	merged := make([]core.TextLine, 0, len(roots))
	for _, root := range roots {
		cluster := clusters[root]
		if len(cluster) == 0 {
			continue
		}
		var points []core.Point
		var sourceTiles []int
		componentCount := 0
		aabb := core.Rect{}
		first := true
		representative := cluster[0]
		representativeWidth := pointDistance(representative.Box[0], representative.Box[1])
		representativeArea := core.BoxArea(representative.Box)
		for _, line := range cluster {
			points = append(points, line.Box[:]...)
			sourceTiles = append(sourceTiles, line.SourceTiles...)
			componentCount = maxInt(componentCount, line.ComponentCount)
			width := pointDistance(line.Box[0], line.Box[1])
			area := core.BoxArea(line.Box)
			if width > representativeWidth+1e-9 ||
				(math.Abs(width-representativeWidth) <= 1e-9 && area < representativeArea) {
				representative = line
				representativeWidth = width
				representativeArea = area
			}
			box := line.AxisAligned()
			if first {
				aabb = box
				first = false
			} else {
				aabb = unionRect(aabb, box)
			}
		}
		// Use the fragment with the most components as the orientation
		// representative. Averaging a one-glyph boundary fragment with a full
		// line biases a tilted box toward horizontal and inflates its area.
		angle := representative.AngleRadians
		stroke := representative.StrokeWidth
		box := core.ProjectBox(points, angle)
		if len(cluster) > 1 {
			// Keep the representative slope, union the longitudinal extent and
			// average the perpendicular center/height weighted by candidate
			// width. This avoids both a boundary fragment's noisy corner
			// inflation and a hard switch between two full tile candidates.
			u := core.Point{X: math.Cos(angle), Y: math.Sin(angle)}
			v := core.Point{X: -math.Sin(angle), Y: math.Cos(angle)}
			minU, maxU := math.Inf(1), math.Inf(-1)
			centerVSum, heightSum, weightSum := 0.0, 0.0, 0.0
			for _, line := range cluster {
				lineMinU, lineMaxU := math.Inf(1), math.Inf(-1)
				for _, point := range line.Box {
					pu := point.X*u.X + point.Y*u.Y
					lineMinU, lineMaxU = math.Min(lineMinU, pu), math.Max(lineMaxU, pu)
					minU, maxU = math.Min(minU, pu), math.Max(maxU, pu)
				}
				width := math.Max(1, lineMaxU-lineMinU)
				center := core.BoxCenter(line.Box)
				centerVSum += (center.X*v.X + center.Y*v.Y) * width
				heightSum += perpendicularExtent(line.Box, v) * width
				weightSum += width
			}
			centerV := centerVSum / weightSum
			height := heightSum / weightSum
			centerU := (minU + maxU) / 2
			cx := centerU*u.X + centerV*v.X
			cy := centerU*u.Y + centerV*v.Y
			box = core.RotatedRectBox(cx, cy, maxU-minU, height, angle)
		}
		if !core.ValidBox(box) {
			box = core.AxisBox(aabb)
		}
		line := core.TextLine{
			Box:            box,
			AngleRadians:   angle,
			StrokeWidth:    stroke,
			ComponentCount: componentCount,
			SourceTiles:    core.SortedUniqueInts(sourceTiles),
		}
		hash, err := core.HashAny(struct {
			Box          core.Box `json:"box"`
			Angle        float64  `json:"angle"`
			Stroke       float64  `json:"stroke"`
			ComponentCnt int      `json:"component_count"`
			Sources      []int    `json:"sources"`
		}{line.Box, line.AngleRadians, line.StrokeWidth, line.ComponentCount, line.SourceTiles})
		if err != nil {
			return nil, err
		}
		line.CandidateHash = hash
		merged = append(merged, line)
	}
	sortLines(merged)
	for i := range merged {
		merged[i].ID = i
		merged[i].ReadingIndex = i
	}
	return merged, nil
}

func perpendicularExtent(box core.Box, v core.Point) float64 {
	minV, maxV := math.Inf(1), math.Inf(-1)
	for _, point := range box {
		pv := point.X*v.X + point.Y*v.Y
		minV, maxV = math.Min(minV, pv), math.Max(maxV, pv)
	}
	return math.Max(1, maxV-minV)
}

func unionRect(a, b core.Rect) core.Rect {
	x0 := minInt(a.X, b.X)
	y0 := minInt(a.Y, b.Y)
	x1 := maxInt(a.Right(), b.Right())
	y1 := maxInt(a.Bottom(), b.Bottom())
	return core.Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

func ValidateTileConsistency(whole, tiled []core.TextLine, tolerance int, minIoU float64) error {
	if absInt(len(whole)-len(tiled)) > tolerance {
		return fmt.Errorf("tiled line count %d differs from whole-image count %d by more than tolerance %d", len(tiled), len(whole), tolerance)
	}
	for _, t := range tiled {
		best := 0.0
		for _, w := range whole {
			best = math.Max(best, core.BoxIoU(t.Box, w.Box))
		}
		if best < minIoU {
			return fmt.Errorf("tiled line %d has best IoU %.3f below %.3f", t.ID, best, minIoU)
		}
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
