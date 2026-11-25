package synth

import (
	"fmt"
	"math"

	"qkernel/internal/imagex"
)

type Config struct {
	Width              int
	Height             int
	Frames             int
	Seed               int64
	Channels           int
	IncludeLowContrast bool
	Implants           []ImplantTruth
}

type Benchmark struct {
	Frame *imagex.Frame
	Truth *GroundTruth
}

type Set struct {
	Benchmarks []*Benchmark
}

func DefaultConfig() Config {
	return Config{Width: 320, Height: 192, Frames: 3, Seed: 0x51A7E, Channels: 3, IncludeLowContrast: true}
}

func GenerateSet(cfg Config) (*Set, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		cfg = DefaultConfig()
	}
	if cfg.Frames <= 0 {
		cfg.Frames = 1
	}
	if cfg.Channels == 0 {
		cfg.Channels = 3
	}
	out := &Set{}
	for i := 0; i < cfg.Frames; i++ {
		b, err := Generate(cfg, i)
		if err != nil {
			return nil, err
		}
		out.Benchmarks = append(out.Benchmarks, b)
	}
	return out, nil
}

func Generate(cfg Config, index int) (*Benchmark, error) {
	if cfg.Width < 96 || cfg.Height < 96 {
		return nil, fmt.Errorf("synth dimensions must be at least 96x96")
	}
	if cfg.Channels == 0 {
		cfg.Channels = 3
	}
	f := imagex.NewFrame(cfg.Width, cfg.Height, cfg.Channels)
	drawGradient(f, cfg.Seed+int64(index)*101)

	truth := &GroundTruth{
		Seed:  cfg.Seed,
		Frame: FrameMeta{ID: MustFrameID(index), Index: index, Width: cfg.Width, Height: cfg.Height, Channels: cfg.Channels},
	}

	r1 := imagex.R(cfg.Width*8/100, cfg.Height*28/100, cfg.Width*34/100, cfg.Height*62/100)
	fillRect(f, r1, 214, 76, 87)
	truth.Objects = append(truth.Objects, ObjectTruth{ID: "rect_main", Kind: "rect", BBox: r1, Color: [3]int{214, 76, 87}})

	cx, cy := cfg.Width*62/100, cfg.Height*38/100
	rad := cfg.Height * 13 / 100
	circle := imagex.R(cx-rad, cy-rad, cx+rad+1, cy+rad+1)
	fillCircle(f, cx, cy, rad, 72, 149, 239)
	truth.Objects = append(truth.Objects, ObjectTruth{ID: "circle_main", Kind: "circle", BBox: circle, Color: [3]int{72, 149, 239}})

	tri := [3][2]int{
		{cfg.Width * 10 / 100, cfg.Height * 90 / 100},
		{cfg.Width * 27 / 100, cfg.Height * 68 / 100},
		{cfg.Width * 45 / 100, cfg.Height * 90 / 100},
	}
	triBBox := imagex.R(tri[0][0], tri[1][1], tri[2][0]+1, tri[0][1]+1)
	fillTriangle(f, tri[0][0], tri[0][1], tri[1][0], tri[1][1], tri[2][0], tri[2][1], 250, 197, 66)
	truth.Objects = append(truth.Objects, ObjectTruth{ID: "triangle_main", Kind: "triangle", BBox: triBBox, Color: [3]int{250, 197, 66}})

	lineR := imagex.R(cfg.Width*55/100-3, cfg.Height*70/100-3, cfg.Width*92/100+4, cfg.Height*91/100+4)
	drawThickLine(f, cfg.Width*56/100, cfg.Height*72/100, cfg.Width*91/100, cfg.Height*89/100, 3, 80, 220, 170)
	truth.Objects = append(truth.Objects, ObjectTruth{ID: "line_main", Kind: "line", BBox: lineR, Color: [3]int{80, 220, 170}})

	if cfg.IncludeLowContrast {
		lc := imagex.R(cfg.Width*55/100, cfg.Height*55/100, cfg.Width*82/100, cfg.Height*64/100)
		fillRect(f, lc, 92, 104, 132)
		truth.Objects = append(truth.Objects, ObjectTruth{ID: "low_contrast_detail", Kind: "low_contrast_rect", BBox: lc, Color: [3]int{92, 104, 132}})
	}

	probe := imagex.R(0, cfg.Height*30/100, cfg.Width, cfg.Height*72/100)
	drawProbeTexture(f, probe)
	truth.Objects = append(truth.Objects, ObjectTruth{ID: "probe_texture", Kind: "probe_texture", BBox: probe, Color: [3]int{128, 128, 160}})

	wm := imagex.R(6, 4, 66, 28)
	fillRect(f, wm, 16, 20, 34)
	drawText(f, 11, 8, fmt.Sprintf("F%03d", index+1), 1, 235, 245, 255)
	truth.Watermark = wm

	sampleCoords := []struct {
		x, y  int
		label string
	}{
		{2, 2, "background_top_left"},
		{cfg.Width - 3, cfg.Height - 3, "background_bottom_right"},
		{(r1.MinX + r1.MaxX) / 2, (r1.MinY + r1.MaxY) / 2, "rect_center"},
		{cx, cy, "circle_center"},
		{13, 10, "watermark_ink"},
	}
	for _, sc := range sampleCoords {
		r, g, b := f.RGBAtClamped(sc.x, sc.y)
		truth.Samples = append(truth.Samples, SampleTruth{X: sc.x, Y: sc.y, Label: sc.label, Expected: [3]int{int(r), int(g), int(b)}})
	}
	truth.Implants = append([]ImplantTruth(nil), cfg.Implants...)
	truth.Seal()
	return &Benchmark{Frame: f, Truth: truth}, nil
}

func drawGradient(f *imagex.Frame, seed int64) {
	w, h := f.Width, f.Height
	phase := float64(seed%97) * 0.01
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			u := float64(x) / float64(max(w-1, 1))
			v := float64(y) / float64(max(h-1, 1))
			wave := math.Sin((u+v+phase)*math.Pi*2) * 7
			r := clampByte(28 + 118*u + 24*v + wave)
			g := clampByte(34 + 78*v + 42*(1-u) + wave*0.4)
			b := clampByte(92 + 96*(u*0.5+v*0.5) + wave*0.7)
			f.SetRGB(x, y, r, g, b)
		}
	}
}

func fillRect(f *imagex.Frame, r imagex.Rect, rr, gg, bb byte) {
	r = r.Canonical().Intersect(f.Bounds())
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			f.SetRGB(x, y, rr, gg, bb)
		}
	}
}

func fillCircle(f *imagex.Frame, cx, cy, rad int, rr, gg, bb byte) {
	rad2 := rad * rad
	for y := cy - rad; y <= cy+rad; y++ {
		for x := cx - rad; x <= cx+rad; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad2 {
				f.SetRGB(x, y, rr, gg, bb)
			}
		}
	}
}

func fillTriangle(f *imagex.Frame, x1, y1, x2, y2, x3, y3 int, rr, gg, bb byte) {
	minY := minInt(y1, minInt(y2, y3))
	maxY := maxInt(y1, maxInt(y2, y3))
	minX := minInt(x1, minInt(x2, x3))
	maxX := maxInt(x1, maxInt(x2, x3))
	sign := func(px, py, ax, ay, bx, by int) int {
		return (px-bx)*(ay-by) - (ax-bx)*(py-by)
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d1 := sign(x, y, x1, y1, x2, y2)
			d2 := sign(x, y, x2, y2, x3, y3)
			d3 := sign(x, y, x3, y3, x1, y1)
			hasNeg := d1 < 0 || d2 < 0 || d3 < 0
			hasPos := d1 > 0 || d2 > 0 || d3 > 0
			if !(hasNeg && hasPos) {
				f.SetRGB(x, y, rr, gg, bb)
			}
		}
	}
}

func drawThickLine(f *imagex.Frame, x0, y0, x1, y1, thick int, rr, gg, bb byte) {
	steps := maxInt(absInt(x1-x0), absInt(y1-y0))
	if steps == 0 {
		fillCircle(f, x0, y0, thick, rr, gg, bb)
		return
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(float64(x0) + float64(x1-x0)*t))
		y := int(math.Round(float64(y0) + float64(y1-y0)*t))
		fillCircle(f, x, y, thick, rr, gg, bb)
	}
}

var font3x5 = map[rune][5]string{
	'0': {"111", "101", "101", "101", "111"},
	'1': {"010", "110", "010", "010", "111"},
	'2': {"111", "001", "111", "100", "111"},
	'3': {"111", "001", "111", "001", "111"},
	'4': {"101", "101", "111", "001", "001"},
	'5': {"111", "100", "111", "001", "111"},
	'6': {"111", "100", "111", "101", "111"},
	'7': {"111", "001", "010", "010", "010"},
	'8': {"111", "101", "111", "101", "111"},
	'9': {"111", "101", "111", "001", "111"},
	'F': {"111", "100", "110", "100", "100"},
}

func drawText(f *imagex.Frame, x, y int, text string, scale int, rr, gg, bb byte) {
	if scale < 1 {
		scale = 1
	}
	cx := x
	for _, ch := range text {
		glyph, ok := font3x5[ch]
		if !ok {
			cx += 4 * scale
			continue
		}
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if glyph[row][col] != '1' {
					continue
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						f.SetRGB(cx+col*scale+sx, y+row*scale+sy, rr, gg, bb)
					}
				}
			}
		}
		cx += 4 * scale
	}
}

func drawProbeTexture(f *imagex.Frame, r imagex.Rect) {
	r = r.Intersect(f.Bounds())
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			if ((x/4)+(y/4))%2 == 0 {
				f.SetRGB(x, y, 184, 194, 218)
			} else {
				f.SetRGB(x, y, 54, 62, 94)
			}
		}
	}
}

func clampByte(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(math.Round(v))
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

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
