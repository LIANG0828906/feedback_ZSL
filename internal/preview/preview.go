package preview

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"

	"texlab/internal/engine"
	"texlab/internal/model"
)

type View struct {
	DataURL  string `json:"data_url"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Depth    int    `json:"depth"`
	Channels int    `json:"channels"`
	Layout   string `json:"layout"`
	Alpha    string `json:"alpha"`
}

func WritePNG(path string, im model.Image, maxWidth, maxHeight int) error {
	img := toNRGBA(im, maxWidth, maxHeight)
	if maxWidth <= 0 {
		img = toNRGBA(im, im.Width, im.Height)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func DataURL(im model.Image, maxWidth, maxHeight int) (View, error) {
	img := toNRGBA(im, maxWidth, maxHeight)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return View{}, err
	}
	return View{
		DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		Width:   im.Width, Height: im.Height, Depth: im.Depth, Channels: im.Channels,
		Layout: im.Layout, Alpha: im.Alpha,
	}, nil
}

func SegmentsDataURL(res *engine.RunResult, entryID string, final bool, maxWidth, maxHeight int) (View, error) {
	width, height := res.Source.Width, res.Source.Height
	img := image.NewNRGBA(image.Rect(0, 0, scaled(width, height, maxWidth, maxHeight).dx, scaled(width, height, maxWidth, maxHeight).dy))
	scale := scaled(width, height, maxWidth, maxHeight)
	// Paint per segment so mixed-depth edited regions are scaled using their
	// own segment metadata.
	for _, c := range res.Segments {
		var sim model.Image
		if final {
			sim = c.Final
		} else {
			if st := res.StageIndex[entryID][c.Segment.Index]; st != nil {
				sim = st.Image
			} else {
				continue
			}
		}
		drawSegment(img, sim, c.Segment, scale, width, height)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return View{}, err
	}
	if final && len(res.Segments) > 0 {
		im := res.Segments[len(res.Segments)-1].Final
		return View{
			DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
			Width:   width, Height: height, Depth: im.Depth, Channels: im.Channels,
			Layout: im.Layout, Alpha: im.Alpha,
		}, nil
	}
	return View{
		DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		Width:   width, Height: height,
	}, nil
}

func Heatmap(before, after model.Image, maxWidth, maxHeight int) (View, error) {
	w, h := min(before.Width, after.Width), min(before.Height, after.Height)
	scale := scaled(w, h, maxWidth, maxHeight)
	img := image.NewNRGBA(image.Rect(0, 0, scale.dx, scale.dy))
	maxDiff := uint16(1)
	diffs := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := before.SemanticPixel(x, y)
			b := after.SemanticPixel(x, y)
			var d uint16
			for c := 0; c < 4; c++ {
				ad := abs16(a[c], b[c])
				if ad > d {
					d = ad
				}
			}
			diffs[y*w+x] = d
			if d > maxDiff {
				maxDiff = d
			}
		}
	}
	for py := 0; py < scale.dy; py++ {
		y := py * h / scale.dy
		for px := 0; px < scale.dx; px++ {
			x := px * w / scale.dx
			d := diffs[y*w+x]
			if d == 0 {
				img.SetNRGBA(px, py, color.NRGBA{8, 15, 22, 255})
				continue
			}
			t := float64(d) / float64(maxDiff)
			r := uint8(30 + 225*t)
			g := uint8(220 * (1 - t))
			img.SetNRGBA(px, py, color.NRGBA{r, g, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return View{}, err
	}
	return View{
		DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		Width:   w, Height: h, Depth: 8, Channels: 4, Layout: "RGBA", Alpha: "straight",
	}, nil
}

type scaleInfo struct {
	dx int
	dy int
}

func scaled(w, h, maxW, maxH int) scaleInfo {
	if maxW <= 0 || maxH <= 0 || (w <= maxW && h <= maxH) {
		return scaleInfo{dx: w, dy: h}
	}
	fx := float64(maxW) / float64(w)
	fy := float64(maxH) / float64(h)
	f := math.Min(fx, fy)
	return scaleInfo{dx: max(1, int(float64(w)*f)), dy: max(1, int(float64(h)*f))}
}

func drawSegment(dst *image.NRGBA, im model.Image, seg model.Segment, s scaleInfo, fullWidth, fullHeight int) {
	x0 := seg.X * s.dx / fullWidth
	x1 := (seg.X + seg.W) * s.dx / fullWidth
	y0 := seg.Y * s.dy / fullHeight
	y1 := (seg.Y + seg.H) * s.dy / fullHeight
	if x1 <= x0 {
		x1 = x0 + 1
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}
	for py := y0; py < y1 && py < s.dy; py++ {
		sy := (py - y0) * seg.H / (y1 - y0)
		if sy >= seg.H {
			sy = seg.H - 1
		}
		for px := x0; px < x1 && px < s.dx; px++ {
			sx := (px - x0) * seg.W / (x1 - x0)
			if sx >= seg.W {
				sx = seg.W - 1
			}
			p := im.SemanticPixel(sx, sy)
			dst.SetNRGBA(px, py, color.NRGBA{
				scale8(p[0], im.Depth), scale8(p[1], im.Depth), scale8(p[2], im.Depth), scale8(p[3], im.Depth),
			})
		}
	}
}

func toNRGBA(im model.Image, maxW, maxH int) *image.NRGBA {
	s := scaled(im.Width, im.Height, maxW, maxH)
	out := image.NewNRGBA(image.Rect(0, 0, s.dx, s.dy))
	for y := 0; y < s.dy; y++ {
		sy := y * im.Height / s.dy
		for x := 0; x < s.dx; x++ {
			sx := x * im.Width / s.dx
			p := im.SemanticPixel(sx, sy)
			out.SetNRGBA(x, y, color.NRGBA{scale8(p[0], im.Depth), scale8(p[1], im.Depth), scale8(p[2], im.Depth), scale8(p[3], im.Depth)})
		}
	}
	return out
}

func scale8(v uint16, depth int) uint8 {
	if depth == 8 {
		return uint8(v)
	}
	return uint8((uint32(v)*255 + 32767) / 65535)
}

func abs16(a, b uint16) uint16 {
	if a > b {
		return a - b
	}
	return b - a
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
