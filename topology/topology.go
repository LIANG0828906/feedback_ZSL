package topology

import (
	"fmt"
	"math"
	"sort"

	"maskqc/model"
)

type Analysis struct {
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	MaskHash   string   `json:"maskHash"`
	Foreground int      `json:"foreground"`
	Domains    []Domain `json:"domains"`
}

type Domain struct {
	ID        string               `json:"id"`
	Label     int                  `json:"label"`
	CentroidX float64              `json:"centroidX"`
	CentroidY float64              `json:"centroidY"`
	Area      int                  `json:"area"`
	BBox      model.Rect           `json:"bbox"`
	Holes     int                  `json:"holes"`
	RingWidth int                  `json:"ringWidth"`
	Perimeter int                  `json:"perimeter"`
	Direction model.Direction      `json:"direction"`
	Segments  []model.TruthSegment `json:"segments"`
	Mask      []byte               `json:"-"`
}

type Tolerance struct {
	CentroidDistance int `json:"centroidDistance"`
	AreaPercent      int `json:"areaPercent"`
	BBoxPixels       int `json:"bboxPixels"`
	Holes            int `json:"holes"`
	RingWidth        int `json:"ringWidth"`
	PixelPercent     int `json:"pixelPercent"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		CentroidDistance: 8,
		AreaPercent:      35,
		BBoxPixels:       3,
		Holes:            1,
		RingWidth:        3,
		PixelPercent:     40,
	}
}

func Analyze(mask []byte, width, height int, strokes []model.Stroke) (*Analysis, error) {
	if width <= 0 || height <= 0 || len(mask) != width*height {
		return nil, &model.Error{
			Code: model.CodeParam, Path: "mask", Field: "mask",
			Message: "mask dimensions are inconsistent",
		}
	}
	labels := make([]int, len(mask))
	type component struct {
		label  int
		pixels []int
		bbox   model.Rect
		area   int
		sumX   int64
		sumY   int64
	}
	components := make([]component, 0)
	queue := make([]int, 0, width*height)
	foreground := 0
	for index, value := range mask {
		if value < model.ForegroundThreshold || labels[index] != 0 {
			continue
		}
		label := len(components) + 1
		queue = queue[:0]
		queue = append(queue, index)
		labels[index] = label
		pixels := make([]int, 0, 64)
		bbox := model.Rect{MinX: width, MinY: height, MaxX: -1, MaxY: -1}
		var sumX, sumY int64
		for head := 0; head < len(queue); head++ {
			current := queue[head]
			x, y := current%width, current/width
			pixels = append(pixels, current)
			foreground++
			bbox.MinX = min(bbox.MinX, x)
			bbox.MinY = min(bbox.MinY, y)
			bbox.MaxX = max(bbox.MaxX, x)
			bbox.MaxY = max(bbox.MaxY, y)
			sumX += int64(x)
			sumY += int64(y)
			neighbors := [4]int{-1, 0, 0, 1}
			steps := [4]int{0, -1, 1, 0}
			for direction := 0; direction < 4; direction++ {
				nx := x + neighbors[direction]
				ny := y + steps[direction]
				if nx < 0 || nx >= width || ny < 0 || ny >= height {
					continue
				}
				next := ny*width + nx
				if labels[next] == 0 && mask[next] >= model.ForegroundThreshold {
					labels[next] = label
					queue = append(queue, next)
				}
			}
		}
		components = append(components, component{label: label, pixels: pixels, bbox: bbox, area: len(pixels), sumX: sumX, sumY: sumY})
	}
	sort.SliceStable(components, func(i, j int) bool {
		if components[i].bbox.MinY != components[j].bbox.MinY {
			return components[i].bbox.MinY < components[j].bbox.MinY
		}
		return components[i].bbox.MinX < components[j].bbox.MinX
	})
	analysis := &Analysis{Width: width, Height: height, MaskHash: model.HashBytes(mask), Foreground: foreground}
	for index, comp := range components {
		domain := Domain{
			ID: fmt.Sprintf("d%03d", index+1), Label: comp.label,
			CentroidX: float64(comp.sumX) / float64(comp.area),
			CentroidY: float64(comp.sumY) / float64(comp.area),
			Area:      comp.area, BBox: comp.bbox,
			Mask: make([]byte, len(mask)),
		}
		for _, pixel := range comp.pixels {
			domain.Mask[pixel] = 255
		}
		domain.Holes = countHoles(domain.Mask, comp.bbox, width, height)
		domain.RingWidth = averageRingWidth(domain.Mask, comp.bbox, width, height)
		domain.Segments = boundarySegments(domain.Mask, comp.bbox, width, height, strokes)
		domain.Perimeter = len(domain.Segments)
		domain.Direction = majorityDirection(domain.Segments)
		analysis.Domains = append(analysis.Domains, domain)
	}
	return analysis, nil
}

func countHoles(mask []byte, bbox model.Rect, width, height int) int {
	localWidth := bbox.Width()
	localHeight := bbox.Height()
	if localWidth <= 0 || localHeight <= 0 {
		return 0
	}
	visited := make([]bool, localWidth*localHeight)
	background := func(localX, localY int) bool {
		return mask[(bbox.MinY+localY)*width+bbox.MinX+localX] < model.ForegroundThreshold
	}
	queue := make([]int, 0, localWidth*localHeight)
	for y := 0; y < localHeight; y++ {
		for x := 0; x < localWidth; x++ {
			if x != 0 && x != localWidth-1 && y != 0 && y != localHeight-1 {
				continue
			}
			pixel := y*localWidth + x
			if !visited[pixel] && background(x, y) {
				visited[pixel] = true
				queue = append(queue, pixel)
			}
		}
	}
	flood := func() {
		for head := 0; head < len(queue); head++ {
			pixel := queue[head]
			x, y := pixel%localWidth, pixel/localWidth
			for _, delta := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				nx, ny := x+delta[0], y+delta[1]
				if nx < 0 || nx >= localWidth || ny < 0 || ny >= localHeight {
					continue
				}
				next := ny*localWidth + nx
				if !visited[next] && background(nx, ny) {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	flood()
	holes := 0
	for y := 0; y < localHeight; y++ {
		for x := 0; x < localWidth; x++ {
			pixel := y*localWidth + x
			if visited[pixel] || !background(x, y) {
				continue
			}
			holes++
			visited[pixel] = true
			queue = queue[:0]
			queue = append(queue, pixel)
			flood()
		}
	}
	return holes
}

func averageRingWidth(mask []byte, bbox model.Rect, width, height int) int {
	localWidth := bbox.Width()
	localHeight := bbox.Height()
	if localWidth <= 0 || localHeight <= 0 {
		return 0
	}
	distance := make([]int, localWidth*localHeight)
	queue := make([]int, 0, localWidth*localHeight)
	for y := 0; y < localHeight; y++ {
		for x := 0; x < localWidth; x++ {
			pixel := y*localWidth + x
			distance[pixel] = -1
			if mask[(bbox.MinY+y)*width+bbox.MinX+x] < model.ForegroundThreshold {
				continue
			}
			edge := false
			for _, delta := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
				nx, ny := x+delta[0], y+delta[1]
				if nx < 0 || nx >= localWidth || ny < 0 || ny >= localHeight ||
					mask[(bbox.MinY+ny)*width+bbox.MinX+nx] < model.ForegroundThreshold {
					edge = true
					break
				}
			}
			if edge {
				distance[pixel] = 1
				queue = append(queue, pixel)
			}
		}
	}
	for head := 0; head < len(queue); head++ {
		pixel := queue[head]
		x, y := pixel%localWidth, pixel/localWidth
		for _, delta := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nx, ny := x+delta[0], y+delta[1]
			if nx < 0 || nx >= localWidth || ny < 0 || ny >= localHeight {
				continue
			}
			next := ny*localWidth + nx
			if distance[next] != -1 || mask[(bbox.MinY+ny)*width+bbox.MinX+nx] < model.ForegroundThreshold {
				continue
			}
			distance[next] = distance[pixel] + 1
			queue = append(queue, next)
		}
	}
	sum := 0
	count := 0
	for _, value := range distance {
		if value > 0 {
			sum += value
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return int(math.Round(2 * float64(sum) / float64(count)))
}

func boundarySegments(mask []byte, bbox model.Rect, width, height int, strokes []model.Stroke) []model.TruthSegment {
	type edge struct {
		nx, ny int
		normal model.Direction
	}
	edges := []edge{
		{nx: -1, ny: 0, normal: model.DirectionWest},
		{nx: 1, ny: 0, normal: model.DirectionEast},
		{nx: 0, ny: -1, normal: model.DirectionNorth},
		{nx: 0, ny: 1, normal: model.DirectionSouth},
	}
	segments := make([]model.TruthSegment, 0)
	for y := bbox.MinY; y <= bbox.MaxY; y++ {
		for x := bbox.MinX; x <= bbox.MaxX; x++ {
			if mask[y*width+x] < model.ForegroundThreshold {
				continue
			}
			for _, current := range edges {
				nx, ny := x+current.nx, y+current.ny
				if nx >= 0 && nx < width && ny >= 0 && ny < height && mask[ny*width+nx] >= model.ForegroundThreshold {
					continue
				}
				stroke := closestStroke(int64(x), int64(y), strokes)
				segments = append(segments, model.TruthSegment{
					X: x, Y: y, NX: current.nx, NY: current.ny, Normal: current.normal,
					Direction: classifyConsistency(current.nx, current.ny, stroke),
				})
			}
		}
	}
	return segments
}

func closestStroke(x, y int64, strokes []model.Stroke) model.Stroke {
	var best model.Stroke
	bestDistance := int64(math.MaxInt64)
	for _, stroke := range strokes {
		distance := pointSegmentDistanceSquared(x, y, int64(stroke.X0), int64(stroke.Y0), int64(stroke.X1), int64(stroke.Y1))
		if distance < bestDistance || (distance == bestDistance && (best.ID == "" || stroke.Seq < best.Seq)) {
			best = stroke
			bestDistance = distance
		}
	}
	return best
}

func classifyConsistency(nx, ny int, stroke model.Stroke) model.Direction {
	if stroke.ID == "" {
		return model.DirectionUnknown
	}
	dx := int64(stroke.X1 - stroke.X0)
	dy := int64(stroke.Y1 - stroke.Y0)
	lengthSquared := dx*dx + dy*dy
	if lengthSquared == 0 {
		return model.DirectionPerpendicular
	}
	dot := int64(nx)*dx + int64(ny)*dy
	if dot*dot*4 <= lengthSquared {
		return model.DirectionPerpendicular
	}
	if dot > 0 {
		return model.DirectionCoDirectional
	}
	return model.DirectionCounter
}

func majorityDirection(segments []model.TruthSegment) model.Direction {
	counts := map[model.Direction]int{}
	for _, segment := range segments {
		counts[segment.Direction]++
	}
	order := []model.Direction{
		model.DirectionPerpendicular, model.DirectionCoDirectional,
		model.DirectionCounter, model.DirectionUnknown,
	}
	best := model.DirectionUnknown
	bestCount := -1
	for _, direction := range order {
		if counts[direction] > bestCount {
			best = direction
			bestCount = counts[direction]
		}
	}
	return best
}

func Compare(analysis *Analysis, truth []model.DomainTruth, tolerance Tolerance) model.Errors {
	var problems model.Errors
	if analysis == nil {
		return model.Errors{model.NewError(model.CodeTopology, "analysis", "", "analysis is nil")}
	}
	used := make([]bool, len(analysis.Domains))
	pairs := make([]struct {
		truthIndex  int
		domainIndex int
	}, 0, len(truth))
	for truthIndex := range truth {
		best := -1
		bestDistance := math.MaxFloat64
		for domainIndex := range analysis.Domains {
			if used[domainIndex] {
				continue
			}
			dx := analysis.Domains[domainIndex].CentroidX - truth[truthIndex].CentroidX
			dy := analysis.Domains[domainIndex].CentroidY - truth[truthIndex].CentroidY
			distance := dx*dx + dy*dy
			if distance < bestDistance {
				best = domainIndex
				bestDistance = distance
			}
		}
		if best >= 0 {
			used[best] = true
			pairs = append(pairs, struct {
				truthIndex  int
				domainIndex int
			}{truthIndex: truthIndex, domainIndex: best})
		} else {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d]", truthIndex),
				ObjectID: truth[truthIndex].ID, Domain: truthIndex + 1,
				Message: "no measured domain can be paired with truth domain",
			})
		}
	}
	for domainIndex, usedDomain := range used {
		if !usedDomain {
			domain := analysis.Domains[domainIndex]
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.domains[%d]", domainIndex),
				ObjectID: domain.ID, Domain: domainIndex + 1,
				Message: "measured domain has no truth counterpart", Measured: fmt.Sprintf("centroid=(%.2f,%.2f) area=%d", domain.CentroidX, domain.CentroidY, domain.Area),
			})
		}
	}
	for _, pair := range pairs {
		expected := truth[pair.truthIndex]
		actual := analysis.Domains[pair.domainIndex]
		domainNumber := pair.truthIndex + 1
		dx := actual.CentroidX - expected.CentroidX
		dy := actual.CentroidY - expected.CentroidY
		if dx*dx+dy*dy > float64(tolerance.CentroidDistance*tolerance.CentroidDistance) {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].centroid", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: "centroid is outside tolerance", Measured: fmt.Sprintf("(%.2f,%.2f)", actual.CentroidX, actual.CentroidY),
			})
		}
		areaDelta := absInt(actual.Area - expected.Area)
		allowedArea := max(1, expected.Area*tolerance.AreaPercent/100)
		if areaDelta > allowedArea {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].area", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: fmt.Sprintf("area delta exceeds %d%%", tolerance.AreaPercent), Measured: fmt.Sprint(actual.Area),
			})
		}
		if absInt(actual.BBox.MinX-expected.BBox.MinX) > tolerance.BBoxPixels ||
			absInt(actual.BBox.MinY-expected.BBox.MinY) > tolerance.BBoxPixels ||
			absInt(actual.BBox.MaxX-expected.BBox.MaxX) > tolerance.BBoxPixels ||
			absInt(actual.BBox.MaxY-expected.BBox.MaxY) > tolerance.BBoxPixels {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].bbox", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: "bounding box is outside tolerance", Measured: fmt.Sprintf("%+v", actual.BBox),
			})
		}
		if absInt(actual.Holes-expected.Holes) > tolerance.Holes {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].holes", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: "hole count is outside tolerance", Measured: fmt.Sprint(actual.Holes),
			})
		}
		if absInt(actual.RingWidth-expected.RingWidth) > tolerance.RingWidth {
			problems = append(problems, &model.Error{
				Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].ringWidth", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: "ring width is outside tolerance", Measured: fmt.Sprint(actual.RingWidth),
			})
		}
		if len(actual.Mask) == len(expected.ReferenceMask) && len(actual.Mask) > 0 {
			mismatch := 0
			union := 0
			for index := range actual.Mask {
				a := actual.Mask[index] >= model.ForegroundThreshold
				b := expected.ReferenceMask[index] >= model.ForegroundThreshold
				if a != b {
					mismatch++
				}
				if a || b {
					union++
				}
			}
			percent := 0
			if union > 0 {
				percent = (mismatch*100 + union/2) / union
			}
			if percent > tolerance.PixelPercent {
				problems = append(problems, &model.Error{
					Code: model.CodeTopology, Path: fmt.Sprintf("$.truth[%d].refMask", pair.truthIndex),
					ObjectID: expected.ID, Domain: domainNumber,
					Message: fmt.Sprintf("pixel mismatch exceeds %d%%", tolerance.PixelPercent), Measured: fmt.Sprintf("%d%%", percent),
				})
			}
		}
		if actual.Direction != expected.Direction {
			problems = append(problems, &model.Error{
				Code: model.CodeDirection, Path: fmt.Sprintf("$.truth[%d].direction", pair.truthIndex),
				ObjectID: expected.ID, Domain: domainNumber,
				Message: "direction category does not match truth", Measured: string(actual.Direction),
			})
		}
	}
	return problems
}

func pointSegmentDistanceSquared(px, py, x0, y0, x1, y1 int64) int64 {
	dx := x1 - x0
	dy := y1 - y0
	if dx == 0 && dy == 0 {
		return square(px-x0) + square(py-y0)
	}
	lengthSquared := dx*dx + dy*dy
	t := ((px-x0)*dx + (py-y0)*dy) * 1024 / lengthSquared
	t = max(0, min(1024, t))
	nx := x0 + dx*t/1024
	ny := y0 + dy*t/1024
	return square(px-nx) + square(py-ny)
}

func square(value int64) int64 {
	return value * value
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
