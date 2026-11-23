package ortho

import (
	"image"
	"image/color"
)

// DrawControlPoints 在图像副本上叠加控制点（红色十字）。
func DrawControlPoints(img image.Image, pts []ControlPoint) *image.RGBA {
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(r >> 8)
			out.Pix[i+1] = uint8(g >> 8)
			out.Pix[i+2] = uint8(bl >> 8)
			out.Pix[i+3] = 255
		}
	}
	red := color.RGBA{255, 32, 32, 255}
	for _, p := range pts {
		cx, cy := int(p.X+0.5), int(p.Y+0.5)
		for d := -4; d <= 4; d++ {
			setRGBA(out, cx+d, cy, red)
			setRGBA(out, cx, cy+d, red)
		}
	}
	return out
}

// Heatmap 生成两幅图像的差异热力图：一致像素为黑色，
// 差异越大越亮（红），并返回是否存在差异。
func Heatmap(a, b image.Image) (*image.RGBA, bool) {
	ba := a.Bounds()
	bb := b.Bounds()
	w, h := ba.Dx(), ba.Dy()
	if bb.Dx() < w {
		w = bb.Dx()
	}
	if bb.Dy() < h {
		h = bb.Dy()
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	diff := false
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dmax int
			for c := 0; c < Channels(a) && c < Channels(b); c++ {
				d := int(getChannel(a, x, y, c)) - int(getChannel(b, x, y, c))
				if d < 0 {
					d = -d
				}
				if d > dmax {
					dmax = d
				}
			}
			if dmax > 0 {
				diff = true
			}
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(dmax)
			out.Pix[i+1] = uint8(dmax / 4)
			out.Pix[i+2] = 0
			out.Pix[i+3] = 255
		}
	}
	return out, diff
}

func setRGBA(img *image.RGBA, x, y int, c color.RGBA) {
	if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
		return
	}
	img.SetRGBA(x, y, c)
}
