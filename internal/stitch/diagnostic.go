package stitch

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

func HeatmapPNG(pairs []model.PairStats, w, h int) []byte {
	if w <= 0 {
		w = 800
	}
	if h <= 0 {
		h = 360
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{10, 14, 22, 255})
		}
	}
	if len(pairs) == 0 {
		return encodePNG(img)
	}
	cols := int(math.Ceil(math.Sqrt(float64(len(pairs)))))
	rows := int(math.Ceil(float64(len(pairs)) / float64(cols)))
	cw, ch := w/cols, h/rows
	maxErr := 1e-9
	for _, p := range pairs {
		if p.HomographyErr > maxErr {
			maxErr = p.HomographyErr
		}
	}
	for i, p := range pairs {
		cx, cy := i%cols, i/cols
		ratio := p.HomographyErr / maxErr
		if p.Decision {
			ratio = ratio*0.75 + (1-p.InlierRate)*0.25
		} else {
			ratio = 1
		}
		r := uint8(30 + 220*ratio)
		g := uint8(210 - 170*ratio)
		b := uint8(80 - 40*ratio)
		for y := cy*ch + 2; y < (cy+1)*ch-2 && y < h; y++ {
			for x := cx*cw + 2; x < (cx+1)*cw-2 && x < w; x++ {
				img.SetNRGBA(x, y, color.NRGBA{r, g, b, 255})
			}
		}
	}
	return encodePNG(img)
}

func MatchOverlayPNG(set *imaging.Set, reg *Registration, pairIndex int) []byte {
	if pairIndex < 0 || pairIndex >= len(set.Images) {
		pairIndex = 1
	}
	if pairIndex >= len(set.Images) {
		pairIndex = 0
	}
	ew := chooseDiagnosticEdge(reg, pairIndex)
	if ew == nil || len(ew.matches) == 0 {
		return encodePNG(image.NewNRGBA(image.Rect(0, 0, 640, 360)))
	}
	a, b := set.Images[ew.a], set.Images[ew.b]
	h := 360
	aw := int(float64(a.Width) * float64(h) / float64(a.Height))
	bw := int(float64(b.Width) * float64(h) / float64(b.Height))
	w := aw + bw + 12
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{8, 10, 14, 255})
		}
	}
	drawScaled(img, a, 0, 0, aw, h)
	drawScaled(img, b, aw+12, 0, bw, h)
	limit := len(ew.matches)
	if limit > 300 {
		limit = 300
	}
	for i := 0; i < limit; i++ {
		m := ew.matches[i]
		x1 := int(m.Source.X * float64(aw) / float64(a.Width))
		y1 := int(m.Source.Y * float64(h) / float64(a.Height))
		x2 := aw + 12 + int(m.Target.X*float64(bw)/float64(b.Width))
		y2 := int(m.Target.Y * float64(h) / float64(b.Height))
		inlier := true
		if i < len(ew.inliers) {
			inlier = ew.inliers[i]
		}
		c := color.NRGBA{80, 220, 255, 190}
		if !inlier {
			c = color.NRGBA{255, 90, 70, 140}
		}
		drawLine(img, x1, y1, x2, y2, c)
	}
	return encodePNG(img)
}

func RegistrationOverlayPNG(set *imaging.Set, reg *Registration, pairIndex int) []byte {
	if pairIndex <= 0 || pairIndex >= len(set.Images) {
		pairIndex = 1
	}
	if pairIndex >= len(set.Images) {
		return encodePNG(image.NewNRGBA(image.Rect(0, 0, 640, 360)))
	}
	base := set.Images[0]
	src := set.Images[pairIndex]
	hsrc := reg.images[pairIndex].transform
	cw, ch := base.Width, base.Height
	inv, _ := hsrc.Inverse()
	img := image.NewNRGBA(image.Rect(0, 0, cw, ch))
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			checker := ((x/40)+(y/40))%2 == 0
			var r, g, b float64
			if checker {
				r, g, b, _ = imaging.SampleRGB(base, float64(x), float64(y))
			} else {
				sx, sy := inv.Apply(float64(x), float64(y))
				r, g, b, _ = imaging.SampleRGB(src, sx, sy)
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(r), uint8(g), uint8(b), 255})
		}
	}
	return encodePNG(img)
}

func SeamOverlayPNG(rendered *RenderResult) []byte {
	img, err := png.Decode(bytes.NewReader(rendered.PNG))
	if err != nil {
		return rendered.PNG
	}
	n := imaging.ToNRGBA(img)
	for _, path := range rendered.SeamPoints {
		for i, p := range path {
			if i%2 != 0 {
				continue
			}
			x, y := int(math.Round(p.X)), int(math.Round(p.Y))
			for dy := 0; dy <= 1; dy++ {
				for dx := 0; dx <= 1; dx++ {
					if x+dx < n.Bounds().Dx() && y+dy < n.Bounds().Dy() {
						n.SetNRGBA(x+dx, y+dy, color.NRGBA{255, 45, 55, 255})
					}
				}
			}
		}
	}
	return encodePNG(n)
}

func chooseDiagnosticEdge(reg *Registration, pairIndex int) *edgeWork {
	for i := pairIndex; i >= 0; i-- {
		im := reg.images[i]
		if len(im.path) >= 2 {
			if ew := reg.edges[edgeKey(im.path[len(im.path)-2], im.path[len(im.path)-1])]; ew != nil {
				return ew
			}
		}
	}
	for _, ew := range reg.edges {
		if ew != nil && len(ew.matches) > 0 {
			return ew
		}
	}
	return nil
}

func drawScaled(dst *image.NRGBA, src *imaging.LoadedImage, x0, y0, w, h int) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx := float64(x) * float64(src.Width) / float64(w)
			sy := float64(y) * float64(src.Height) / float64(h)
			r, g, b, ok := imaging.SampleRGB(src, sx, sy)
			if !ok {
				continue
			}
			dst.SetNRGBA(x0+x, y0+y, color.NRGBA{uint8(r), uint8(g), uint8(b), 255})
		}
	}
}

func drawLine(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	dx, dy := x1-x0, y1-y0
	steps := maxInt(absInt(dx), absInt(dy))
	if steps == 0 {
		if image.Pt(x0, y0).In(img.Bounds()) {
			img.SetNRGBA(x0, y0, c)
		}
		return
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		if image.Pt(x, y).In(img.Bounds()) {
			img.SetNRGBA(x, y, c)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
