package sample

import (
	"image"
	"image/color"
	"math"

	"maskqa/internal/model"
	"maskqa/internal/topology"
)

type Shape struct {
	Kind string
	CX   int
	CY   int
	RX   int
	RY   int
	X0   int
	Y0   int
	X1   int
	Y1   int
	R    int
	Band int
}

type Palette struct {
	FG  [3]byte
	BG0 [3]byte
	BG1 [3]byte
	Ink [3]byte
}

type Asset struct {
	Project *model.Project
	Image   *image.RGBA
}

type Spec struct {
	ID      string
	Width   int
	Height  int
	Chunk   int
	Seed    int64
	Palette Palette
	Shapes  []Shape
}

func DefaultSpecs(seed int64) []Spec {
	return []Spec{
		{
			ID: "glyph-48", Width: 48, Height: 48, Chunk: 12, Seed: seed + 11,
			Palette: Palette{FG: [3]byte{53, 104, 212}, BG0: [3]byte{248, 250, 255}, BG1: [3]byte{218, 231, 255}, Ink: [3]byte{255, 176, 64}},
			Shapes: []Shape{
				{Kind: "ellipse", CX: 15, CY: 24, RX: 10, RY: 15, Band: 7},
				{Kind: "roundrect", X0: 28, Y0: 10, X1: 43, Y1: 38, R: 6, Band: 6},
			},
		},
		{
			ID: "banner-64x48", Width: 64, Height: 48, Chunk: 16, Seed: seed + 23,
			Palette: Palette{FG: [3]byte{28, 166, 145}, BG0: [3]byte{246, 255, 252}, BG1: [3]byte{211, 244, 238}, Ink: [3]byte{255, 116, 96}},
			Shapes: []Shape{
				{Kind: "ellipse", CX: 18, CY: 24, RX: 14, RY: 17, Band: 8},
				{Kind: "roundrect", X0: 38, Y0: 8, X1: 59, Y1: 40, R: 7, Band: 7},
			},
		},
		{
			ID: "badge-64", Width: 64, Height: 64, Chunk: 16, Seed: seed + 37,
			Palette: Palette{FG: [3]byte{142, 72, 220}, BG0: [3]byte{252, 248, 255}, BG1: [3]byte{235, 220, 255}, Ink: [3]byte{48, 196, 214}},
			Shapes: []Shape{
				{Kind: "ellipse", CX: 20, CY: 32, RX: 15, RY: 22, Band: 8},
				{Kind: "roundrect", X0: 40, Y0: 12, X1: 60, Y1: 52, R: 8, Band: 8},
			},
		},
	}
}

func GenerateAll(seed int64) []Asset {
	specs := DefaultSpecs(seed)
	assets := make([]Asset, 0, len(specs))
	for _, spec := range specs {
		assets = append(assets, GenerateOne(spec))
	}
	return assets
}

func GenerateOne(spec Spec) Asset {
	reference := model.NewMask(spec.Width, spec.Height)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			if insideAnyShape(x, y, spec.Shapes) {
				reference.Pix[reference.Index(x, y)] = 255
			}
		}
	}

	strokes := decorativeStrokes(spec)
	strokes = append(strokes, fillStrokes(reference)...)
	candidate := makeCandidate(reference, spec.Seed)
	chunks := model.BuildChunks(spec.Width, spec.Height, spec.Chunk)
	truth := topology.Analyze(reference)

	p := &model.Project{
		ID:            spec.ID,
		Seed:          spec.Seed,
		Width:         spec.Width,
		Height:        spec.Height,
		ChunkSize:     spec.Chunk,
		SourceVersion: model.SchemaInternal,
		LoadedVersion: model.SchemaInternal,
		Strokes:       strokes,
		Chunks:        chunks,
		Candidate:     candidate,
		Reference:     reference,
		Truth:         truth,
	}
	for i := range p.Strokes {
		p.Strokes[i].ContentHash = hashStrokeForGenerator(p.Strokes[i])
	}
	img := renderIcon(spec, reference, p.Strokes[:2])
	return Asset{Project: p, Image: img}
}

func insideAnyShape(x, y int, shapes []Shape) bool {
	for _, sh := range shapes {
		if sh.Kind == "ellipse" && insideEllipseRing(x, y, sh) {
			return true
		}
		if sh.Kind == "roundrect" && insideRoundRectRing(x, y, sh) {
			return true
		}
	}
	return false
}

func insideEllipseRing(x, y int, sh Shape) bool {
	dx := x - sh.CX
	dy := y - sh.CY
	rx := float64(sh.RX)
	ry := float64(sh.RY)
	if rx <= 0 || ry <= 0 {
		return false
	}
	outer := float64(dx*dx)/(rx*rx) + float64(dy*dy)/(ry*ry)
	irx := math.Max(1, rx-float64(sh.Band))
	iry := math.Max(1, ry-float64(sh.Band))
	inner := float64(dx*dx)/(irx*irx) + float64(dy*dy)/(iry*iry)
	return outer <= 1.0 && inner >= 1.0
}

func insideRoundRect(x, y, x0, y0, x1, y1, r int) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	if r <= 0 {
		return true
	}
	cx := clampInt(x, x0+r, x1-r)
	cy := clampInt(y, y0+r, y1-r)
	dx := x - cx
	dy := y - cy
	return dx*dx+dy*dy <= r*r
}

func insideRoundRectRing(x, y int, sh Shape) bool {
	outer := insideRoundRect(x, y, sh.X0, sh.Y0, sh.X1, sh.Y1, sh.R)
	if !outer {
		return false
	}
	b := sh.Band
	innerR := maxInt(1, sh.R-b/2)
	inner := insideRoundRect(x, y, sh.X0+b, sh.Y0+b, sh.X1-b, sh.Y1-b, innerR)
	return !inner
}

func fillStrokes(ref model.Mask) []model.Stroke {
	var strokes []model.Stroke
	no := 100
	for y := 0; y < ref.Height; y++ {
		x := 0
		for x < ref.Width {
			if ref.Pix[ref.Index(x, y)] == 0 {
				x++
				continue
			}
			x0 := x
			for x < ref.Width && ref.Pix[ref.Index(x, y)] != 0 {
				x++
			}
			x1 := x - 1
			no++
			strokes = append(strokes, model.Stroke{
				ID:            fillStrokeID(y, x0, x1),
				No:            no,
				Priority:      30,
				From:          model.Point{X: x0, Y: y},
				To:            model.Point{X: x1, Y: y},
				WidthPx:       1,
				WeightBP:      10000,
				HardnessBP:    10000,
				HasHardness:   true,
				DeclaredIndex: len(strokes) + 2,
				SourceVersion: model.SchemaInternal,
				ParamVersion:  model.ParamsVersion,
			})
		}
	}
	return strokes
}

func fillStrokeID(y, x0, x1 int) string {
	return "fill-y" + pad2(y) + "-x" + pad2(x0) + "-" + pad2(x1)
}

func pad2(v int) string {
	if v < 10 {
		return "0" + string(rune('0'+v))
	}
	if v < 100 {
		return string(rune('0'+v/10)) + string(rune('0'+v%10))
	}
	return string(rune('0'+v%100/10)) + string(rune('0'+v%10))
}

func decorativeStrokes(spec Spec) []model.Stroke {
	w, h := spec.Width, spec.Height
	return []model.Stroke{
		{
			ID: "tone-a", No: 1, Priority: 20,
			From:    model.Point{X: w * 22 / 100, Y: h * 24 / 100},
			To:      model.Point{X: w * 36 / 100, Y: h * 74 / 100},
			WidthPx: 5, WeightBP: 1200, HardnessBP: 5500, HasHardness: true,
			DeclaredIndex: 0, SourceVersion: model.SchemaInternal, ParamVersion: model.ParamsVersion,
		},
		{
			ID: "tone-b", No: 2, Priority: 10,
			From:    model.Point{X: w * 17 / 100, Y: h * 56 / 100},
			To:      model.Point{X: w * 42 / 100, Y: h * 42 / 100},
			WidthPx: 4, WeightBP: 1600, HardnessBP: 7000, HasHardness: true,
			DeclaredIndex: 1, SourceVersion: model.SchemaInternal, ParamVersion: model.ParamsVersion,
		},
	}
}

func makeCandidate(ref model.Mask, seed int64) model.Mask {
	cand := model.NewMask(ref.Width, ref.Height)
	for y := 1; y < ref.Height-1; y++ {
		for x := 1; x < ref.Width-1; x++ {
			idx := ref.Index(x, y)
			if ref.Pix[idx] == 0 {
				continue
			}
			all := true
			for dy := -1; dy <= 1 && all; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if ref.At(x+dx, y+dy) == 0 {
						all = false
						break
					}
				}
			}
			if all {
				cand.Pix[idx] = 255
			}
		}
	}

	var opaque []int
	for i, v := range cand.Pix {
		if v == 255 {
			opaque = append(opaque, i)
		}
	}
	rng := newLCG(seed)
	for k := 0; k < 4 && len(opaque) > 0; k++ {
		idx := opaque[rng.intn(len(opaque))]
		cand.Pix[idx] = 0
	}

	var boundary []model.Point
	for y := 0; y < ref.Height; y++ {
		for x := 0; x < ref.Width; x++ {
			if ref.At(x, y) == 0 {
				continue
			}
			if ref.At(x-1, y) == 0 || ref.At(x+1, y) == 0 || ref.At(x, y-1) == 0 || ref.At(x, y+1) == 0 {
				boundary = append(boundary, model.Point{X: x, Y: y})
			}
		}
	}
	for k := 1; k <= 3 && len(boundary) > 0; k++ {
		p := boundary[len(boundary)*k/4]
		dirs := [4]model.Point{{X: 1, Y: 0}, {X: -1, Y: 0}, {X: 0, Y: 1}, {X: 0, Y: -1}}
		for _, d := range dirs {
			nx, ny := p.X+d.X, p.Y+d.Y
			if ref.InBounds(nx, ny) && ref.At(nx, ny) == 0 {
				spill(cand, ref, nx, ny)
				break
			}
		}
	}
	return cand
}

func spill(cand, ref model.Mask, cx, cy int) {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			x, y := cx+dx, cy+dy
			if !cand.InBounds(x, y) || ref.At(x, y) != 0 {
				continue
			}
			if dx*dx+dy*dy <= 2 {
				cand.Pix[cand.Index(x, y)] = 64
			}
		}
	}
}

func renderIcon(spec Spec, ref model.Mask, decor []model.Stroke) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, spec.Width, spec.Height))
	rng := newLCG(spec.Seed + 999)
	for y := 0; y < spec.Height; y++ {
		t := float64(y) / math.Max(1, float64(spec.Height-1))
		for x := 0; x < spec.Width; x++ {
			base := mixColor(spec.Palette.BG0, spec.Palette.BG1, t)
			n := int(rng.uint64()%19) - 9
			c := clampByte(int(base.R)+n, int(base.G)+n, int(base.B)+n)
			img.SetRGBA(x, y, c)
		}
	}
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			if ref.At(x, y) == 0 {
				continue
			}
			shade := 0.82 + 0.18*float64(y)/float64(spec.Height)
			fg := spec.Palette.FG
			c := color.RGBA{
				R: uint8(clampFloat(float64(fg[0]) * shade)),
				G: uint8(clampFloat(float64(fg[1]) * shade)),
				B: uint8(clampFloat(float64(fg[2]) * shade)),
				A: 255,
			}
			img.SetRGBA(x, y, c)
		}
	}
	for _, s := range decor {
		for _, p := range bresenham(s.From, s.To) {
			if !imagePointIn(img, p) {
				continue
			}
			blendPixel(img, p, spec.Palette.Ink, 105)
		}
	}
	return img
}

func imagePointIn(img *image.RGBA, p model.Point) bool {
	b := img.Bounds()
	return p.X >= b.Min.X && p.X < b.Max.X && p.Y >= b.Min.Y && p.Y < b.Max.Y
}

func blendPixel(img *image.RGBA, p model.Point, rgb [3]byte, alpha uint8) {
	i := img.PixOffset(p.X, p.Y)
	a := uint32(alpha)
	img.Pix[i+0] = uint8((uint32(rgb[0])*a + uint32(img.Pix[i+0])*(255-a)) / 255)
	img.Pix[i+1] = uint8((uint32(rgb[1])*a + uint32(img.Pix[i+1])*(255-a)) / 255)
	img.Pix[i+2] = uint8((uint32(rgb[2])*a + uint32(img.Pix[i+2])*(255-a)) / 255)
	img.Pix[i+3] = 255
}

func bresenham(a, b model.Point) []model.Point {
	dx := absInt(b.X - a.X)
	dy := -absInt(b.Y - a.Y)
	sx, sy := -1, -1
	if a.X < b.X {
		sx = 1
	}
	if a.Y < b.Y {
		sy = 1
	}
	err := dx + dy
	x, y := a.X, a.Y
	var pts []model.Point
	for {
		pts = append(pts, model.Point{X: x, Y: y})
		if x == b.X && y == b.Y {
			return pts
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x += sx
		}
		if e2 <= dx {
			err += dx
			y += sy
		}
	}
}

type lcg struct{ state uint64 }

func newLCG(seed int64) *lcg { return &lcg{state: uint64(seed) | 1} }

func (l *lcg) uint64() uint64 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return l.state
}

func (l *lcg) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(l.uint64() % uint64(n))
}

func mixColor(a, b [3]byte, t float64) color.RGBA {
	return color.RGBA{R: uint8(clampFloat(float64(a[0]) + (float64(b[0])-float64(a[0]))*t)), G: uint8(clampFloat(float64(a[1]) + (float64(b[1])-float64(a[1]))*t)), B: uint8(clampFloat(float64(a[2]) + (float64(b[2])-float64(a[2]))*t)), A: 255}
}

func clampByte(r, g, b int) color.RGBA {
	return color.RGBA{R: uint8(clampInt(r, 0, 255)), G: uint8(clampInt(g, 0, 255)), B: uint8(clampInt(b, 0, 255)), A: 255}
}

func clampFloat(v float64) int { return clampInt(int(math.Round(v)), 0, 255) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
