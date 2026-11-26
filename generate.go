package maskqa

import (
	"image"
	"image/color"
)

const GeneratorSeed int64 = 0x4D41534B5141 // "MASKQA"

type sampleSpec struct {
	id     string
	width  int
	height int
	seed   int64
}

// SampleSpecs returns the fixed multi-resolution sample set.
func SampleSpecs() []struct {
	ID     string
	Width  int
	Height int
	Seed   int64
} {
	return []struct {
		ID     string
		Width  int
		Height int
		Seed   int64
	}{
		{ID: "icon-64", Width: 96, Height: 64, Seed: GeneratorSeed + 64},
		{ID: "icon-80", Width: 128, Height: 80, Seed: GeneratorSeed + 80},
		{ID: "icon-96", Width: 160, Height: 96, Seed: GeneratorSeed + 96},
	}
}

// GenerateSamples deterministically creates all fixed samples.
func GenerateSamples() ([]*Project, error) {
	specs := SampleSpecs()
	out := make([]*Project, 0, len(specs))
	for i := range specs {
		project, err := GenerateSample(i)
		if err != nil {
			return nil, err
		}
		out = append(out, project)
	}
	return out, nil
}

// GenerateSample creates one deterministic engineering project by index.
func GenerateSample(index int) (*Project, error) {
	specs := SampleSpecs()
	if index < 0 || index >= len(specs) {
		return nil, paraError("sample_index", 0, int64(len(specs)-1), "sample index out of range")
	}
	spec := specs[index]
	w, h := spec.Width, spec.Height
	reference := image.NewGray(image.Rect(0, 0, w, h))

	topY := 20 + (h-64)/8
	horizontal := Stroke{
		ID: "stroke-h", Seq: 1, Group: "group-horizontal", Priority: 10,
		Start: Point{X: 16, Y: topY}, End: Point{X: w - 16, Y: topY},
		WidthPx: 10, HardnessPermyriad: 8800, WeightPermyriad: 8600,
		Direction: DirHorizontal,
	}
	diagStartY := topY + 12 + (h-64)/10
	diagEndX := w/2 - 12
	diagonal := Stroke{
		ID: "stroke-d", Seq: 2, Group: "group-diagonal", Priority: 20,
		Start: Point{X: 20, Y: diagStartY}, End: Point{X: diagEndX, Y: h - 18},
		WidthPx: 9, HardnessPermyriad: 8600, WeightPermyriad: 8400,
		Direction: DirDownRight,
	}
	strokes := []Stroke{horizontal, diagonal}

	ringCX := w - 32
	ringCY := h - 24
	const ringOuter = 12
	const ringInner = 6
	const ringMid = 9
	const ringSegments = 12
	ringIDs := make([]string, 0, ringSegments)
	for i := 0; i < ringSegments; i++ {
		a0 := circleAngle(i, ringSegments)
		a1 := circleAngle(i+1, ringSegments)
		id := "stroke-ring-" + itoa(i+1)
		ringIDs = append(ringIDs, id)
		strokes = append(strokes, Stroke{
			ID: id, Seq: 3 + i, Group: "group-ring", Priority: 30,
			Start:   Point{X: ringCX + circleX(ringMid, a0), Y: ringCY + circleY(ringMid, a0)},
			End:     Point{X: ringCX + circleX(ringMid, a1), Y: ringCY + circleY(ringMid, a1)},
			WidthPx: 7, HardnessPermyriad: 9000, WeightPermyriad: 9000,
			Direction: directionFromDelta(
				circleX(ringMid, a1)-circleX(ringMid, a0),
				circleY(ringMid, a1)-circleY(ringMid, a0),
			),
		})
	}
	for i := range strokes {
		strokes[i].DirectionCode = DirectionToCode(strokes[i].Direction)
		strokes[i].HasHardness = true
		strokes[i].HasDirection = true
	}

	drawHardStroke(reference, horizontal)
	drawHardStroke(reference, diagonal)
	drawAnnulus(reference, ringCX, ringCY, ringOuter, ringInner)
	for i := 2; i < len(strokes); i++ {
		drawHardStroke(reference, strokes[i])
	}

	topo := ExtractTopology(reference, DefaultThreshold)
	truth := buildTruth(topo, []truthSpec{
		{center: Point{X: (16 + w - 16) / 2, Y: topY}, direction: DirHorizontal, strokeIDs: []string{horizontal.ID}},
		{center: Point{X: (20 + diagEndX) / 2, Y: (diagStartY + h - 18) / 2}, direction: DirDownRight, strokeIDs: []string{diagonal.ID}},
		{center: Point{X: ringCX, Y: ringCY}, direction: DirClosedRing, strokeIDs: ringIDs},
	})
	candidate := makeCandidate(reference, spec.Seed, []Point{
		{X: w / 2, Y: topY},
		{X: (20 + diagEndX) / 2, Y: (diagStartY + h - 18) / 2},
	})
	icon := renderIcon(spec.Seed, w, h, reference, topo)

	return &Project{
		ID: spec.ID, Seed: spec.Seed, Width: w, Height: h, TileSize: DefaultTileSize,
		Icon: icon, Candidate: candidate, Reference: reference, Strokes: strokes,
		Truth: truth, Tolerances: DefaultTolerances(), SourceVersion: SchemaV3,
	}, nil
}

type truthSpec struct {
	center    Point
	direction DirectionClass
	strokeIDs []string
}

func buildTruth(topo *TopologyReport, specs []truthSpec) []DomainTruth {
	out := make([]DomainTruth, 0, len(topo.Components))
	used := map[int]bool{}
	for _, desired := range specs {
		best := -1
		bestDist := int(^uint(0) >> 1)
		for _, comp := range topo.Components {
			if used[comp.DomainNo] {
				continue
			}
			cx := comp.CentroidXMilli / 1000
			cy := comp.CentroidYMilli / 1000
			dx := cx - desired.center.X
			dy := cy - desired.center.Y
			dist := dx*dx + dy*dy
			if dist < bestDist {
				bestDist = dist
				best = comp.DomainNo
			}
		}
		if best < 0 {
			continue
		}
		used[best] = true
		for _, comp := range topo.Components {
			if comp.DomainNo != best {
				continue
			}
			out = append(out, DomainTruth{
				DomainNo: comp.DomainNo, CentroidXMilli: comp.CentroidXMilli,
				CentroidYMilli: comp.CentroidYMilli, Area: comp.Area,
				MinX: comp.MinX, MinY: comp.MinY, MaxX: comp.MaxX, MaxY: comp.MaxY,
				Holes: comp.Holes, BandWidthMilli: comp.BandWidthMilli,
				Direction: desired.direction, DirectionCode: DirectionToCode(desired.direction),
				StrokeIDs: append([]string(nil), desired.strokeIDs...),
			})
		}
	}
	return out
}

func drawHardStroke(mask *image.Gray, stroke Stroke) {
	stroke.HardnessPermyriad = 10000
	stroke.WeightPermyriad = 10000
	bounds := StrokeBounds(stroke).Intersect(Rect{
		MaxX: mask.Bounds().Dx(), MaxY: mask.Bounds().Dy(),
	})
	for y := bounds.MinY; y < bounds.MaxY; y++ {
		for x := bounds.MinX; x < bounds.MaxX; x++ {
			if strokeCoverageAt(stroke, x, y) > 0 {
				mask.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
}

func drawAnnulus(mask *image.Gray, cx, cy, outer, inner int) {
	for y := cy - outer - 1; y <= cy+outer+1; y++ {
		for x := cx - outer - 1; x <= cx+outer+1; x++ {
			dx := x - cx
			dy := y - cy
			d2 := dx*dx + dy*dy
			if d2 <= outer*outer && d2 >= inner*inner {
				mask.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
}

func makeCandidate(reference *image.Gray, seed int64, holeCenters []Point) *image.Gray {
	w, h := reference.Bounds().Dx(), reference.Bounds().Dy()
	candidate := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range reference.Pix {
		candidate.Pix[i] = v
	}
	// Deterministic boundary inset.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if reference.GrayAt(x, y).Y == 0 || !isBoundaryPixel(reference, x, y) {
				continue
			}
			if hashPercent(seed, x, y, 1) < 34 {
				candidate.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
	// Enclosed candidate-mask holes.
	for i, center := range holeCenters {
		radius := 2
		if i == 1 {
			radius = 1
		}
		for y := center.Y - radius; y <= center.Y+radius; y++ {
			for x := center.X - radius; x <= center.X+radius; x++ {
				dx, dy := x-center.X, y-center.Y
				if dx*dx+dy*dy <= radius*radius && image.Pt(x, y).In(reference.Bounds()) {
					candidate.SetGray(x, y, color.Gray{Y: 0})
				}
			}
		}
	}
	// Sparse outward spill around the reference boundary.
	beforeSpill := image.NewGray(reference.Rect)
	copy(beforeSpill.Pix, candidate.Pix)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if candidate.GrayAt(x, y).Y != 0 || !nearForeground(beforeSpill, x, y, 1) {
				continue
			}
			if hashPercent(seed, x, y, 2) < 28 {
				candidate.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	fillSmallHoles(candidate, 24)
	for i, center := range holeCenters {
		radius := 2
		if i == 1 {
			radius = 1
		}
		for y := center.Y - radius; y <= center.Y+radius; y++ {
			for x := center.X - radius; x <= center.X+radius; x++ {
				dx, dy := x-center.X, y-center.Y
				if dx*dx+dy*dy <= radius*radius && image.Pt(x, y).In(reference.Bounds()) {
					candidate.SetGray(x, y, color.Gray{Y: 0})
				}
			}
		}
	}
	return candidate
}

func fillSmallHoles(mask *image.Gray, maxArea int) {
	b := mask.Bounds()
	w, h := b.Dx(), b.Dy()
	visited := make([]byte, w*h)
	type point struct{ x, y int }
	for start := 0; start < w*h; start++ {
		if visited[start] != 0 || mask.GrayAt(b.Min.X+start%w, b.Min.Y+start/w).Y != 0 {
			continue
		}
		queue := []int{start}
		visited[start] = 1
		component := []int{}
		touchesBorder := false
		for len(queue) > 0 {
			idx := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			component = append(component, idx)
			x, y := idx%w, idx/w
			if x == 0 || y == 0 || x == w-1 || y == h-1 {
				touchesBorder = true
			}
			for _, d := range []point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d.x, y+d.y
				if nx < 0 || ny < 0 || nx >= w || ny >= h {
					touchesBorder = true
					continue
				}
				nidx := ny*w + nx
				if visited[nidx] == 0 && mask.GrayAt(b.Min.X+nx, b.Min.Y+ny).Y == 0 {
					visited[nidx] = 1
					queue = append(queue, nidx)
				}
			}
		}
		if !touchesBorder && len(component) <= maxArea {
			for _, idx := range component {
				mask.SetGray(b.Min.X+idx%w, b.Min.Y+idx/w, color.Gray{Y: 255})
			}
		}
	}
}

func isBoundaryPixel(mask *image.Gray, x, y int) bool {
	return GrayAt(mask, x-1, y) == 0 || GrayAt(mask, x+1, y) == 0 ||
		GrayAt(mask, x, y-1) == 0 || GrayAt(mask, x, y+1) == 0
}

func nearForeground(mask *image.Gray, x, y, radius int) bool {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if GrayAt(mask, x+dx, y+dy) > 0 {
				return true
			}
		}
	}
	return false
}

func renderIcon(seed int64, w, h int, reference *image.Gray, topo *TopologyReport) *image.RGBA {
	icon := image.NewRGBA(image.Rect(0, 0, w, h))
	palette := []color.RGBA{
		{R: 47, G: 107, B: 255, A: 255},
		{R: 255, G: 122, B: 26, A: 255},
		{R: 24, G: 160, B: 88, A: 255},
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := (x*37 + y*63) % 100
			r := uint8(226 - t/12)
			g := uint8(233 - t/14)
			b := uint8(255 - t/20)
			noise := int(hashPercent(seed, x, y, 7))%17 - 8
			icon.SetRGBA(x, y, color.RGBA{
				R: clampByte(int(r) + noise), G: clampByte(int(g) + noise),
				B: clampByte(int(b) + noise/2), A: 255,
			})
		}
	}
	if topo != nil {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				idx := y*w + x
				if reference.GrayAt(x, y).Y == 0 || idx >= len(topo.Labels) {
					continue
				}
				label := topo.Labels[idx]
				if label <= 0 {
					continue
				}
				c := palette[(label-1)%len(palette)]
				blendRGBA(icon, x, y, c, 230)
				if isBoundaryPixel(reference, x, y) {
					blendRGBA(icon, x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255}, 70)
				}
			}
		}
	}
	// Embedded deterministic sensor noise.
	for i := 0; i < w*h/24; i++ {
		x := int(hashUint(seed, i, 11) % uint64(w))
		y := int(hashUint(seed, i, 13) % uint64(h))
		v := 40 + int(hashUint(seed, i, 17)%180)
		icon.SetRGBA(x, y, color.RGBA{R: uint8(v), G: uint8(v), B: clampByte(v + 30), A: 220})
	}
	return icon
}

func blendRGBA(img *image.RGBA, x, y int, c color.RGBA, alpha int) {
	old := img.RGBAAt(x, y)
	a := alpha
	na := 255 - a
	img.SetRGBA(x, y, color.RGBA{
		R: uint8((int(c.R)*a + int(old.R)*na) / 255),
		G: uint8((int(c.G)*a + int(old.G)*na) / 255),
		B: uint8((int(c.B)*a + int(old.B)*na) / 255),
		A: 255,
	})
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func hashPercent(seed int64, x, y, salt int) int {
	return int(hashUint(seed, x*1000003+salt, y*9176+salt*31) % 100)
}

func hashUint(seed int64, a, b int) uint64 {
	h := uint64(seed)
	h ^= uint64(a) + 0x9e3779b97f4a7c15 + (h << 6) + (h >> 2)
	h ^= uint64(b) + 0xbf58476d1ce4e5b9 + (h << 6) + (h >> 2)
	h = (h ^ (h >> 30)) * 0xbf58476d1ce4e5b9
	h = (h ^ (h >> 27)) * 0x94d049bb133111eb
	return h ^ (h >> 31)
}

func circleAngle(i, n int) int {
	_ = n
	return (i * 30) % 360
}

func circleX(radius, degrees int) int {
	table := map[int]float64{
		0: 1, 30: 0.866, 60: 0.5, 90: 0, 120: -0.5, 150: -0.866, 180: -1,
		210: -0.866, 240: -0.5, 270: 0, 300: 0.5, 330: 0.866,
	}
	return int(float64(radius) * table[degrees])
}

func circleY(radius, degrees int) int {
	table := map[int]float64{
		0: 0, 30: 0.5, 60: 0.866, 90: 1, 120: 0.866, 150: 0.5, 180: 0,
		210: -0.5, 240: -0.866, 270: -1, 300: -0.866, 330: -0.5,
	}
	return int(float64(radius) * table[degrees])
}

func directionFromDelta(dx, dy int) DirectionClass {
	ax, ay := absInt(dx), absInt(dy)
	switch {
	case ax > 2*ay:
		return DirHorizontal
	case ay > 2*ax:
		return DirVertical
	case dx*dy >= 0:
		return DirDownRight
	default:
		return DirDownLeft
	}
}

// DirectionToCode maps direction categories to the numeric v2 encoding.
func DirectionToCode(d DirectionClass) int {
	switch d {
	case DirHorizontal:
		return 1
	case DirVertical:
		return 2
	case DirDownRight:
		return 3
	case DirDownLeft:
		return 4
	case DirClosedRing:
		return 5
	default:
		return 0
	}
}

// CodeToDirection maps numeric direction codes to categories.
func CodeToDirection(code int) DirectionClass {
	switch code {
	case 1:
		return DirHorizontal
	case 2:
		return DirVertical
	case 3:
		return DirDownRight
	case 4:
		return DirDownLeft
	case 5:
		return DirClosedRing
	default:
		return DirUnknown
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
