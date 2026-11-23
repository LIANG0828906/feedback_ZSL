// Package viz renders deterministic diagnostic images for the CLI exporter and
// the Fyne panel.
package viz

import (
	"image"
	"image/color"
	"math"

	"glyphbrush/internal/geom"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
	"glyphbrush/internal/raster"
)

var segmentPalette = []color.RGBA{
	{64, 196, 255, 255},
	{255, 176, 64, 255},
	{120, 230, 140, 255},
	{230, 112, 220, 255},
	{255, 114, 114, 255},
	{120, 144, 255, 255},
}

// Outline draws contour outlines and the flattened skeleton.
func Outline(g loader.Glyph, result *pipeline.RunResult) *image.RGBA {
	grid := result.Grid
	img := image.NewRGBA(image.Rect(0, 0, grid.Width, grid.Height))
	fillBackground(img, color.RGBA{18, 20, 28, 255})
	for _, contour := range g.Skeletons {
		for _, run := range contour.Runs {
			var last image.Point
			for i, sp := range run.Points {
				x, y := grid.WorldToPixel(sp.Position)
				p := image.Pt(int(math.Round(x)), int(math.Round(y)))
				if i > 0 {
					drawLine(img, last.X, last.Y, p.X, p.Y, color.RGBA{210, 220, 240, 255}, 2)
				}
				last = p
				drawDisc(img, p.X, p.Y, 1, color.RGBA{80, 220, 255, 255})
			}
		}
	}
	return img
}

// Bands draws semi-transparent stroke bands over the skeleton.
func Bands(g loader.Glyph, result *pipeline.RunResult) *image.RGBA {
	grid := result.Grid
	img := image.NewRGBA(image.Rect(0, 0, grid.Width, grid.Height))
	fillBackground(img, color.RGBA{16, 18, 24, 255})
	for i, segment := range result.Segments {
		if segment.Band == nil {
			continue
		}
		c := segmentPalette[i%len(segmentPalette)]
		fillPolygon(img, grid, segment.Band.Polygon, color.RGBA{c.R, c.G, c.B, 90})
		drawPolygon(img, grid, segment.Band.Polygon, color.RGBA{c.R, c.G, c.B, 230}, 1)
	}
	for _, contour := range g.Skeletons {
		for _, run := range contour.Runs {
			for i := 1; i < len(run.Points); i++ {
				ax, ay := grid.WorldToPixel(run.Points[i-1].Position)
				bx, by := grid.WorldToPixel(run.Points[i].Position)
				drawLine(img, int(ax), int(ay), int(bx), int(by), color.RGBA{255, 255, 255, 150}, 1)
			}
		}
	}
	return img
}

// MaskImage converts the grayscale stroke mask to an image.
func MaskImage(m raster.Mask) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, m.Width, m.Height))
	copy(img.Pix, m.Data)
	return img
}

// Heatmap draws the mask as a dark backdrop and density samples as heat dots.
func Heatmap(result *pipeline.RunResult) *image.RGBA {
	grid := result.Grid
	img := image.NewRGBA(image.Rect(0, 0, grid.Width, grid.Height))
	fillBackground(img, color.RGBA{10, 12, 18, 255})
	for y := 0; y < result.Mask.Height; y++ {
		for x := 0; x < result.Mask.Width; x++ {
			v := result.Mask.At(x, y)
			if v != 0 {
				img.SetRGBA(x, y, color.RGBA{R: 45, G: 52, B: 66, A: 255})
			}
		}
	}
	for _, sample := range result.Densities {
		c := heatColor(sample.DensityPPM)
		radius := 1 + sample.DensityPPM/450
		drawDisc(img, sample.PixelX, sample.PixelY, radius, c)
		if len(sample.Boundary) >= 2 {
			a := worldPoint(grid, sample.Boundary[0])
			b := worldPoint(grid, sample.Boundary[1])
			drawLine(img, a.X, a.Y, b.X, b.Y, color.RGBA{220, 220, 255, 110}, 1)
		}
	}
	return img
}

func heatColor(ppm int) color.RGBA {
	if ppm < 0 {
		ppm = 0
	}
	if ppm > 1000 {
		ppm = 1000
	}
	t := float64(ppm) / 1000
	r := uint8(40 + 215*t)
	g := uint8(230 * (1 - math.Abs(t-0.5)*1.6))
	b := uint8(255 * (1 - t))
	return color.RGBA{R: r, G: g, B: b, A: 220}
}

func worldPoint(grid *raster.Grid, p geom.Point) image.Point {
	x, y := grid.WorldToPixel(p)
	return image.Pt(int(math.Round(x)), int(math.Round(y)))
}

func fillBackground(img *image.RGBA, c color.RGBA) {
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func drawPolygon(img *image.RGBA, grid *raster.Grid, poly []geom.Point, c color.RGBA, width int) {
	if len(poly) < 2 {
		return
	}
	last := worldPoint(grid, poly[0])
	for i := 1; i <= len(poly); i++ {
		p := worldPoint(grid, poly[i%len(poly)])
		drawLine(img, last.X, last.Y, p.X, p.Y, c, width)
		last = p
	}
}

func fillPolygon(img *image.RGBA, grid *raster.Grid, poly []geom.Point, c color.RGBA) {
	if len(poly) < 3 {
		return
	}
	px := make([]geom.Point, len(poly))
	minY, maxY := math.Inf(1), math.Inf(-1)
	for i, p := range poly {
		x, y := grid.WorldToPixel(p)
		px[i] = geom.Point{X: x, Y: y}
		minY = math.Min(minY, y)
		maxY = math.Max(maxY, y)
	}
	startY := int(math.Max(0, math.Floor(minY)))
	endY := int(math.Min(float64(img.Bounds().Dy()-1), math.Ceil(maxY)))
	for y := startY; y <= endY; y++ {
		scan := float64(y) + 0.5
		var xs []float64
		for i := range px {
			a, b := px[i], px[(i+1)%len(px)]
			if (a.Y <= scan && b.Y > scan) || (b.Y <= scan && a.Y > scan) {
				t := (scan - a.Y) / (b.Y - a.Y)
				xs = append(xs, a.X+(b.X-a.X)*t)
			}
		}
		sortFloats(xs)
		for i := 0; i+1 < len(xs); i += 2 {
			x0 := int(math.Ceil(xs[i] - 0.5))
			x1 := int(math.Floor(xs[i+1] - 0.5))
			if x0 < 0 {
				x0 = 0
			}
			if x1 >= img.Bounds().Dx() {
				x1 = img.Bounds().Dx() - 1
			}
			for x := x0; x <= x1; x++ {
				blend(img, x, y, c)
			}
		}
	}
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func blend(img *image.RGBA, x, y int, c color.RGBA) {
	if !image.Pt(x, y).In(img.Bounds()) {
		return
	}
	old := img.RGBAAt(x, y)
	alpha := float64(c.A) / 255
	img.SetRGBA(x, y, color.RGBA{
		R: uint8(float64(c.R)*alpha + float64(old.R)*(1-alpha)),
		G: uint8(float64(c.G)*alpha + float64(old.G)*(1-alpha)),
		B: uint8(float64(c.B)*alpha + float64(old.B)*(1-alpha)),
		A: 255,
	})
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, width int) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx - dy
	for {
		drawDisc(img, x0, y0, width, c)
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func drawDisc(img *image.RGBA, cx, cy, radius int, c color.RGBA) {
	if radius < 1 {
		radius = 1
	}
	r2 := radius * radius
	for y := -radius; y <= radius; y++ {
		for x := -radius; x <= radius; x++ {
			if x*x+y*y <= r2 {
				blend(img, cx+x, cy+y, c)
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
