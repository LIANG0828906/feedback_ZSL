package maskqc

import (
	"fmt"
	"image"
	"image/color"
	"math/rand"
)

const DefaultSeed int64 = 0x42A8691E

// GenerateSamples creates count deterministic icon projects.
func GenerateSamples(count int) []*Project {
	if count <= 0 {
		count = 3
	}
	out := make([]*Project, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, GenerateProject(DefaultSeed+int64(i*9973), i))
	}
	return out
}

// GenerateProject creates one deterministic batch.
func GenerateProject(seed int64, batch int) *Project {
	rng := rand.New(rand.NewSource(seed))
	w, h := 96, 96
	shiftX := (batch % 2) * 2
	shiftY := (batch % 3)

	strokes := []Stroke{
		{Seq: 1, OrderPriority: 30, Start: Point{X: 18 + shiftX, Y: 30 + shiftY}, End: Point{X: 44 + shiftX, Y: 16 + shiftY}, WeightPermille: 1000, RadiusTick: 5, HardnessPermille: 860},
		{Seq: 2, OrderPriority: 10, Start: Point{X: 62, Y: 20 + shiftY}, End: Point{X: 80, Y: 20 + shiftY}, WeightPermille: 1000, RadiusTick: 3, HardnessPermille: 1000},
		{Seq: 3, OrderPriority: 11, Start: Point{X: 80, Y: 20 + shiftY}, End: Point{X: 80, Y: 40 + shiftY}, WeightPermille: 1000, RadiusTick: 3, HardnessPermille: 1000},
		{Seq: 4, OrderPriority: 12, Start: Point{X: 80, Y: 40 + shiftY}, End: Point{X: 62, Y: 40 + shiftY}, WeightPermille: 1000, RadiusTick: 3, HardnessPermille: 1000},
		{Seq: 5, OrderPriority: 13, Start: Point{X: 62, Y: 40 + shiftY}, End: Point{X: 62, Y: 20 + shiftY}, WeightPermille: 1000, RadiusTick: 3, HardnessPermille: 1000},
		{Seq: 6, OrderPriority: 20, Start: Point{X: 24 + shiftX, Y: 72 - shiftY}, End: Point{X: 72 + shiftX, Y: 64 + shiftY}, WeightPermille: 965, RadiusTick: 6, HardnessPermille: 780},
	}

	components := make(map[int]*CoverageComponent, len(strokes))
	strokeAlpha := make([]byte, w*h)
	ordered := OrderedStrokes(strokes)
	for _, st := range ordered {
		comp, err := RasterizeStroke(st, w, h)
		if err != nil {
			panic(fmt.Sprintf("generator raster failure: %v", err))
		}
		components[st.Seq] = comp
		ComposeRegionOver(strokeAlpha, Rect{MinX: 0, MinY: 0, MaxX: w, MaxY: h}, comp.Bounds, comp.Alpha)
	}

	candidate := buildCandidate(strokeAlpha, w, h, strokes, rng, batch)
	accum := append([]byte(nil), candidate.Pix...)
	for _, st := range ordered {
		comp := components[st.Seq]
		ComposeRegionOver(accum, Rect{MinX: 0, MinY: 0, MaxX: w, MaxY: h}, comp.Bounds, comp.Alpha)
	}
	truthMask := grayFromAlpha(accum, w, h)
	domains, err := AnalyzeGrayMask(truthMask, 128)
	if err != nil {
		panic(err)
	}
	hashes := make(map[int]string, len(components))
	for seq, comp := range components {
		hashes[seq] = comp.ContentHash
	}
	truth := &Truth{
		Mask:             truthMask,
		Domains:          domains,
		StrokeComponents: components,
		ReferenceHash:    hashBytes([]byte("truth-mask"), accum),
		ComponentHashes:  hashes,
	}

	channelCoverage := []int{980 - batch*10, 940 + batch*5, 880, 1000}
	p := &Project{
		ID:              fmt.Sprintf("icon-seed-%02d", batch+1),
		Seed:            seed,
		Width:           w,
		Height:          h,
		SourceVersion:   SchemaV3,
		Image:           paintImage(w, h, seed, strokes, components, rng),
		Candidate:       candidate,
		Strokes:         strokes,
		ChannelCoverage: channelCoverage,
		Truth:           truth,
	}
	return p
}

func buildCandidate(strokeAlpha []byte, w, h int, strokes []Stroke, rng *rand.Rand, batch int) *image.Gray {
	alpha := append([]byte(nil), strokeAlpha...)
	at := func(x, y int) int { return y*w + x }

	mid := midpoint(strokes[0].Start, strokes[0].End)
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			x, y := mid.X+dx, mid.Y+dy
			if x >= 0 && x < w && y >= 0 && y < h {
				alpha[at(x, y)] = 0
			}
		}
	}

	for i := 0; i < 10; i++ {
		x := 20 + rng.Intn(22)
		y := 18 + rng.Intn(14)
		if x >= 0 && x < w && y >= 0 && y < h {
			alpha[at(x, y)] = byte(40 + rng.Intn(55))
		}
	}

	spillPoints := []Point{{54, 22}, {55, 39}, {48, 69}, {76, 61}}
	for i, p := range spillPoints {
		if (i+batch)%3 == 0 {
			continue
		}
		for dy := 0; dy < 2; dy++ {
			for dx := 0; dx < 2; dx++ {
				x, y := p.X+dx, p.Y+dy
				if x < w && y < h {
					alpha[at(x, y)] = 168
				}
			}
		}
	}

	for i := 0; i < 6; i++ {
		x := 25 + i
		y := 71
		if x < w && y < h {
			alpha[at(x, y)] = 72
		}
	}
	return grayFromAlpha(alpha, w, h)
}

func midpoint(a, b Point) Point {
	return Point{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
}

func grayFromAlpha(alpha []byte, w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	copy(img.Pix, alpha)
	return img
}

func paintImage(w, h int, seed int64, strokes []Stroke, comps map[int]*CoverageComponent, rng *rand.Rand) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	top := color.NRGBA{R: 235, G: 242, B: 250, A: 255}
	bottom := color.NRGBA{R: 214, G: 226, B: 240, A: 255}
	for y := 0; y < h; y++ {
		t := float64(y) / float64(h-1)
		base := blendNRGBA(top, bottom, t)
		for x := 0; x < w; x++ {
			n := int(rng.Intn(17)) - 8
			c := color.RGBA{
				R: clampByte(int(base.R) + n),
				G: clampByte(int(base.G) + n),
				B: clampByte(int(base.B) + n + rng.Intn(5) - 2),
				A: 255,
			}
			img.SetRGBA(x, y, c)
		}
	}

	blockColors := []color.RGBA{
		{R: 64, G: 120, B: 210, A: 210},
		{R: 236, G: 144, B: 48, A: 205},
		{R: 42, G: 167, B: 145, A: 210},
	}
	fillRoundedRect(img, Rect{MinX: 10, MinY: 10, MaxX: 50, MaxY: 38}, 8, withAlpha(blockColors[0], 90))
	fillRoundedRect(img, Rect{MinX: 55, MinY: 14, MaxX: 87, MaxY: 47}, 10, withAlpha(blockColors[1], 95))
	fillRoundedRect(img, Rect{MinX: 16, MinY: 57, MaxX: 80, MaxY: 82}, 10, withAlpha(blockColors[2], 90))

	highlight := color.RGBA{R: 255, G: 255, B: 255, A: 120}
	for _, st := range strokes {
		comp := comps[st.Seq]
		if comp == nil {
			continue
		}
		for y := comp.Bounds.MinY; y < comp.Bounds.MaxY; y++ {
			for x := comp.Bounds.MinX; x < comp.Bounds.MaxX; x++ {
				a := comp.Alpha[(y-comp.Bounds.MinY)*comp.Bounds.Width()+(x-comp.Bounds.MinX)] / 3
				if a == 0 {
					continue
				}
				blendPixel(img, x, y, highlight.R, highlight.G, highlight.B, a)
			}
		}
	}
	return img
}

func fillRoundedRect(img *image.RGBA, r Rect, radius int, c color.RGBA) {
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			dx := 0
			dy := 0
			if x < r.MinX+radius {
				dx = r.MinX + radius - x - 1
			} else if x >= r.MaxX-radius {
				dx = x - (r.MaxX - radius)
			}
			if y < r.MinY+radius {
				dy = r.MinY + radius - y - 1
			} else if y >= r.MaxY-radius {
				dy = y - (r.MaxY - radius)
			}
			if dx > 0 && dy > 0 && dx*dx+dy*dy > radius*radius {
				continue
			}
			blendPixel(img, x, y, c.R, c.G, c.B, c.A)
		}
	}
}

func blendPixel(img *image.RGBA, x, y int, r, g, b, a uint8) {
	if x < 0 || x >= img.Bounds().Dx() || y < 0 || y >= img.Bounds().Dy() {
		return
	}
	i := img.PixOffset(x, y)
	sa := uint32(a)
	inv := 255 - sa
	img.Pix[i+0] = uint8((uint32(r)*sa + uint32(img.Pix[i+0])*inv + 127) / 255)
	img.Pix[i+1] = uint8((uint32(g)*sa + uint32(img.Pix[i+1])*inv + 127) / 255)
	img.Pix[i+2] = uint8((uint32(b)*sa + uint32(img.Pix[i+2])*inv + 127) / 255)
	img.Pix[i+3] = 255
}

func withAlpha(c color.RGBA, a uint8) color.RGBA {
	c.A = a
	return c
}

func blendNRGBA(a, b color.NRGBA, t float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(a.R)*(1-t) + float64(b.R)*t),
		G: uint8(float64(a.G)*(1-t) + float64(b.G)*t),
		B: uint8(float64(a.B)*(1-t) + float64(b.B)*t),
		A: 255,
	}
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// OrderedStrokes returns the unique construction order: priority then seq.
func OrderedStrokes(strokes []Stroke) []Stroke {
	out := append([]Stroke(nil), strokes...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].OrderPriority < out[i].OrderPriority ||
				(out[j].OrderPriority == out[i].OrderPriority && out[j].Seq < out[i].Seq) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// AdjudicateOrder records why each adjacent stroke occupies its position.
func AdjudicateOrder(strokes []Stroke) ([]Stroke, []TransformRecord) {
	ordered := OrderedStrokes(strokes)
	records := make([]TransformRecord, 0, len(ordered))
	for i, st := range ordered {
		reason := "lower order_priority first"
		if i > 0 && ordered[i-1].OrderPriority == st.OrderPriority {
			reason = "equal priority resolved by lower stroke seq"
		}
		records = append(records, TransformRecord{
			Path:        fmt.Sprintf("strokes[%d]", st.Seq),
			Field:       "composition_order",
			Op:          "order_adjudication",
			New:         i,
			Reason:      reason,
			TruncatedAt: fmt.Sprintf("seq=%d,priority=%d", st.Seq, st.OrderPriority),
		})
	}
	return ordered, records
}
