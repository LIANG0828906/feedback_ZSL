package generator

import (
	"math"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
)

func drawBaseline(p *model.Project, spec model.ImageSpec) (*core.Frame, error) {
	f, err := core.NewFrame(p.Width, p.Height, p.Channels)
	if err != nil {
		return nil, err
	}
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			t := float64(y) / float64(max(1, f.Height-1))
			s := float64(x) / float64(max(1, f.Width-1))
			wave := math.Sin(float64(x+int(spec.Seed%97))/17.0)*9 +
				math.Cos(float64(y*3+spec.Frame)/13.0)*6
			r := 23 + 46*s + 20*t + wave
			g := 31 + 28*s + 34*(1-t) + wave*0.55
			b := 44 + 52*(1-t) + 12*math.Sin(float64(x+y)/23.0)
			f.Set(x, y, [3]uint8{clamp(r), clamp(g), clamp(b)})
		}
	}

	for _, d := range spec.Details {
		for y := d.BBox.Y; y < d.BBox.Bottom() && y < f.Height; y++ {
			for x := d.BBox.X; x < d.BBox.Right() && x < f.Width; x++ {
				if ((x-d.BBox.X)/max(1, d.Period/2)+(y-d.BBox.Y)/max(1, d.Period/2))%2 == 0 {
					base := f.At(x, y)
					f.Set(x, y, [3]uint8{
						addClamp(base[0], d.Delta),
						addClamp(base[1], d.Delta/2),
						addClamp(base[2], d.Delta),
					})
				}
			}
		}
	}

	for _, shape := range spec.Shapes {
		drawShape(f, shape)
	}
	drawWatermark(f, spec.Watermark, spec.Frame, spec.Seed)
	return f, nil
}

func drawShape(f *core.Frame, shape model.Shape) {
	r := shape.BBox
	if r.Empty() {
		return
	}
	cx := float64(r.X) + float64(r.W-1)/2
	cy := float64(r.Y) + float64(r.H-1)/2
	for y := r.Y; y < r.Bottom(); y++ {
		for x := r.X; x < r.Right(); x++ {
			inside := false
			switch shape.Kind {
			case "rect":
				inside = true
			case "ellipse":
				dx := (float64(x) - cx) / math.Max(1, float64(r.W)/2)
				dy := (float64(y) - cy) / math.Max(1, float64(r.H)/2)
				inside = dx*dx+dy*dy <= 1
			case "triangle":
				inside = pointInTriangle(float64(x), float64(y),
					float64(r.X), float64(r.Bottom()-1),
					float64(r.X+r.W/2), float64(r.Y),
					float64(r.Right()-1), float64(r.Bottom()-1))
			case "diamond":
				dx := math.Abs(float64(x)-cx) / math.Max(1, float64(r.W)/2)
				dy := math.Abs(float64(y)-cy) / math.Max(1, float64(r.H)/2)
				inside = dx+dy <= 1
			default:
				inside = true
			}
			if inside {
				tint := uint8(((x*7 + y*11) % 9))
				f.Set(x, y, [3]uint8{
					addClamp(shape.Color[0], int(tint)),
					addClamp(shape.Color[1], int(tint/2)),
					addClamp(shape.Color[2], int(tint)),
				})
			}
		}
	}
	edge := [3]uint8{
		addClamp(shape.Color[0], 38),
		addClamp(shape.Color[1], 38),
		addClamp(shape.Color[2], 38),
	}
	for x := r.X; x < r.Right(); x++ {
		f.Set(x, r.Y, edge)
		f.Set(x, r.Bottom()-1, edge)
	}
	for y := r.Y; y < r.Bottom(); y++ {
		f.Set(r.X, y, edge)
		f.Set(r.Right()-1, y, edge)
	}
}

func drawWatermark(f *core.Frame, rect core.Rect, frame int, seed int64) {
	r := rect.Clamp(f.Width, f.Height)
	if r.Empty() {
		return
	}
	for y := r.Y; y < r.Bottom(); y++ {
		for x := r.X; x < r.Right(); x++ {
			f.Set(x, y, [3]uint8{20, 24, 29})
		}
	}
	digits := []int{frame / 100 % 10, frame / 10 % 10, frame % 10}
	cell := 2
	startX := r.X + 4
	startY := r.Y + 3
	for _, d := range digits {
		pattern := sevenSeg[d]
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if pattern[row][col] == '1' {
					for yy := 0; yy < cell; yy++ {
						for xx := 0; xx < cell; xx++ {
							x := startX + col*cell + xx
							y := startY + row*cell + yy
							if x >= r.X && x < r.Right() && y >= r.Y && y < r.Bottom() {
								shade := uint8(205 + ((seed + int64(x+y)) & 15))
								f.Set(x, y, [3]uint8{shade, shade, addClamp(shade, 8)})
							}
						}
					}
				}
			}
		}
		startX += 9
	}
}

var sevenSeg = [10][5][3]byte{
	{{'1', '1', '1'}, {'1', '0', '1'}, {'1', '0', '1'}, {'1', '0', '1'}, {'1', '1', '1'}},
	{{'0', '0', '1'}, {'0', '0', '1'}, {'0', '0', '1'}, {'0', '0', '1'}, {'0', '0', '1'}},
	{{'1', '1', '1'}, {'0', '0', '1'}, {'1', '1', '1'}, {'1', '0', '0'}, {'1', '1', '1'}},
	{{'1', '1', '1'}, {'0', '0', '1'}, {'1', '1', '1'}, {'0', '0', '1'}, {'1', '1', '1'}},
	{{'1', '0', '1'}, {'1', '0', '1'}, {'1', '1', '1'}, {'0', '0', '1'}, {'0', '0', '1'}},
	{{'1', '1', '1'}, {'1', '0', '0'}, {'1', '1', '1'}, {'0', '0', '1'}, {'1', '1', '1'}},
	{{'1', '1', '1'}, {'1', '0', '0'}, {'1', '1', '1'}, {'1', '0', '1'}, {'1', '1', '1'}},
	{{'1', '1', '1'}, {'0', '0', '1'}, {'0', '0', '1'}, {'0', '0', '1'}, {'0', '0', '1'}},
	{{'1', '1', '1'}, {'1', '0', '1'}, {'1', '1', '1'}, {'1', '0', '1'}, {'1', '1', '1'}},
	{{'1', '1', '1'}, {'1', '0', '1'}, {'1', '1', '1'}, {'0', '0', '1'}, {'1', '1', '1'}},
}

func pointInTriangle(px, py, ax, ay, bx, by, cx, cy float64) bool {
	d1 := sign(px, py, ax, ay, bx, by)
	d2 := sign(px, py, bx, by, cx, cy)
	d3 := sign(px, py, cx, cy, ax, ay)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func sign(px, py, ax, ay, bx, by float64) float64 {
	return (px-bx)*(ay-by) - (ax-bx)*(py-by)
}

func clamp(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func addClamp(v uint8, n int) uint8 {
	x := int(v) + n
	if x < 0 {
		return 0
	}
	if x > 255 {
		return 255
	}
	return uint8(x)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
