package stitch

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

type RenderResult struct {
	PNG         []byte
	Width       int
	Height      int
	SeamMaxGrad float64
	SeamP95Grad float64
	SeamAvgGrad float64
	SeamPoints  [][]model.Point
	Labels      []int32
	LayerMask   map[int][]bool
}

type layer struct {
	index int
	x0    int
	y0    int
	w     int
	h     int
	rgb   []float32
	mask  []bool
}

func (l *layer) at(x, y int) (float64, float64, float64, bool) {
	if l == nil || x < l.x0 || y < l.y0 || x >= l.x0+l.w || y >= l.y0+l.h {
		return 0, 0, 0, false
	}
	i := ((y-l.y0)*l.w + (x - l.x0)) * 3
	return float64(l.rgb[i]), float64(l.rgb[i+1]), float64(l.rgb[i+2]), l.mask[(y-l.y0)*l.w+(x-l.x0)]
}

func Render(reg *Registration, p model.Params, downsample int) (*RenderResult, error) {
	return RenderContext(context.Background(), reg, p, downsample)
}

func RenderContext(ctx context.Context, reg *Registration, p model.Params, downsample int) (*RenderResult, error) {
	if p.FusionLevels < 1 || p.FusionLevels > 6 {
		return nil, errs.Field(errs.EPara, "fusion levels out of range [1,6]", "fusion_levels")
	}
	if p.FusionSigma < 0.4 || p.FusionSigma > 8 {
		return nil, errs.Field(errs.EPara, "fusion sigma out of range [0.4,8]", "fusion_sigma")
	}
	if p.ExposureMaxGain < 1.05 || p.ExposureMaxGain > 4 {
		return nil, errs.Field(errs.EPara, "exposure max gain out of range [1.05,4]", "exposure_max_gain")
	}
	if p.SeamGradientWeight < 0 || p.SeamGradientWeight > 4 {
		return nil, errs.Field(errs.EPara, "seam gradient weight out of range [0,4]", "seam_gradient_weight")
	}
	if downsample < 1 {
		downsample = 1
	}
	if downsample > 8 {
		return nil, errs.Field(errs.EPara, "preview downsample out of range [1,8]", "preview_downsample")
	}
	cw := int(math.Ceil(float64(reg.Canvas.Width) / float64(downsample)))
	ch := int(math.Ceil(float64(reg.Canvas.Height) / float64(downsample)))
	if cw <= 0 || ch <= 0 {
		return nil, errs.New(errs.EHomo, "registered canvas is empty")
	}
	if int64(cw)*int64(ch) > 40_000_000 {
		return nil, errs.Detail(errs.EPara, "panorama exceeds local pixel boundary", map[string]any{"width": cw, "height": ch, "max_pixels": 40000000})
	}
	s := 1 / float64(downsample)
	layers := make([]*layer, 0, len(reg.images))
	for _, iw := range reg.images {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !iw.accepted {
			continue
		}
		shift := model.Matrix3{s, 0, -reg.CanvasOffset.X * s, 0, s, -reg.CanvasOffset.Y * s, 0, 0, 1}
		h := shift.Mul(iw.transform)
		l := warpLayer(iw, h, cw, ch, p.ExposureMaxGain)
		if l != nil {
			layers = append(layers, l)
		}
	}
	if len(layers) == 0 {
		return nil, errs.New(errs.EHomo, "no registered layers")
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].index < layers[j].index })
	labels := make([]int32, cw*ch)
	for i := range labels {
		labels[i] = -1
	}
	canvasColor := make([]float32, cw*ch*3)
	seams := make([][]model.Point, 0)
	for _, l := range layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if l.index == layers[0].index {
			assignAll(labels, l, cw)
		} else if overlapCount(labels, l, cw) > 40 {
			seam := chooseSeam(l, layers, labels, canvasColor, cw, ch, p.SeamGradientWeight)
			if len(seam) > 0 {
				seams = append(seams, seam)
			}
		} else {
			assignUnlabelled(labels, l, cw)
		}
		updateCanvasColor(labels, canvasColor, layers, cw, ch)
	}
	outPix, err := multibandBlend(ctx, layers, labels, cw, ch, p.FusionLevels, p.FusionSigma)
	if err != nil {
		return nil, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, cw, ch))
	for i := 0; i < cw*ch; i++ {
		img.Pix[i*4] = clamp8(outPix[i*3])
		img.Pix[i*4+1] = clamp8(outPix[i*3+1])
		img.Pix[i*4+2] = clamp8(outPix[i*3+2])
		img.Pix[i*4+3] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, errs.Wrap(errs.EInternal, "cannot encode panorama", err)
	}
	maxGrad, p95, avg := seamArtifact(img, layers, seams, labels, cw)
	return &RenderResult{PNG: buf.Bytes(), Width: cw, Height: ch, SeamMaxGrad: maxGrad, SeamP95Grad: p95, SeamAvgGrad: avg, SeamPoints: seams, Labels: labels}, nil
}

func warpLayer(iw *imageWork, h model.Matrix3, cw, ch int, maxGain float64) *layer {
	inv, ok := h.Inverse()
	if !ok {
		return nil
	}
	corners := quad(iw.image.Width, iw.image.Height, h)
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range corners {
		minX, minY = math.Min(minX, p.X), math.Min(minY, p.Y)
		maxX, maxY = math.Max(maxX, p.X), math.Max(maxY, p.Y)
	}
	x0, y0 := maxInt(0, int(math.Floor(minX))), maxInt(0, int(math.Floor(minY)))
	x1, y1 := minInt(cw, int(math.Ceil(maxX))+1), minInt(ch, int(math.Ceil(maxY))+1)
	if x1 <= x0 || y1 <= y0 {
		return nil
	}
	l := &layer{index: iw.index, x0: x0, y0: y0, w: x1 - x0, h: y1 - y0}
	l.rgb = make([]float32, l.w*l.h*3)
	l.mask = make([]bool, l.w*l.h)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			sx, sy := inv.Apply(float64(x)+0.5, float64(y)+0.5)
			if sx < 0 || sy < 0 || sx > float64(iw.image.Width-1) || sy > float64(iw.image.Height-1) {
				continue
			}
			r, g, b, ok := imaging.SampleRGB(iw.image, sx, sy)
			if !ok {
				continue
			}
			li := ((y-l.y0)*l.w + (x - l.x0))
			gain := [3]float64{clampGain(iw.exposureG[0], maxGain), clampGain(iw.exposureG[1], maxGain), clampGain(iw.exposureG[2], maxGain)}
			off := [3]float64{clampOffset(iw.exposureO[0]), clampOffset(iw.exposureO[1]), clampOffset(iw.exposureO[2])}
			l.rgb[li*3] = clampF(r*gain[0]+off[0], 0, 255)
			l.rgb[li*3+1] = clampF(g*gain[1]+off[1], 0, 255)
			l.rgb[li*3+2] = clampF(b*gain[2]+off[2], 0, 255)
			l.mask[li] = true
		}
	}
	return l
}

func assignAll(labels []int32, l *layer, cw int) {
	for y := 0; y < l.h; y++ {
		for x := 0; x < l.w; x++ {
			if l.mask[y*l.w+x] {
				labels[(l.y0+y)*cw+l.x0+x] = int32(l.index)
			}
		}
	}
}

func assignUnlabelled(labels []int32, l *layer, cw int) {
	for y := 0; y < l.h; y++ {
		for x := 0; x < l.w; x++ {
			if l.mask[y*l.w+x] {
				idx := (l.y0+y)*cw + l.x0 + x
				if labels[idx] < 0 {
					labels[idx] = int32(l.index)
				}
			}
		}
	}
}

func overlapCount(labels []int32, l *layer, cw int) int {
	n := 0
	for y := 0; y < l.h; y++ {
		for x := 0; x < l.w; x++ {
			if l.mask[y*l.w+x] && labels[(l.y0+y)*cw+l.x0+x] >= 0 {
				n++
			}
		}
	}
	return n
}

func chooseSeam(l *layer, layers []*layer, labels []int32, canvasColor []float32, cw, ch int, gradWeight float64) []model.Point {
	_ = layers
	_ = ch
	minX, minY, maxX, maxY := l.x0+l.w, l.y0+l.h, l.x0, l.y0
	for y := 0; y < l.h; y++ {
		for x := 0; x < l.w; x++ {
			gx, gy := l.x0+x, l.y0+y
			if l.mask[y*l.w+x] && labels[gy*cw+gx] >= 0 {
				minX, minY = minInt(minX, gx), minInt(minY, gy)
				maxX, maxY = maxInt(maxX, gx), maxInt(maxY, gy)
			}
		}
	}
	if minX > maxX || minY > maxY {
		assignUnlabelled(labels, l, cw)
		return nil
	}
	ow, oh := maxX-minX+1, maxY-minY+1
	vertical := ow >= oh
	var path []model.Point
	if vertical {
		path = seamVertical(l, labels, canvasColor, cw, minX, minY, maxX, maxY, gradWeight)
	} else {
		path = seamHorizontal(l, labels, canvasColor, cw, minX, minY, maxX, maxY, gradWeight)
	}
	if len(path) == 0 {
		assignUnlabelled(labels, l, cw)
		return nil
	}
	newRight := float64(l.x0)+float64(l.w)/2 >= float64(minX+maxX)/2
	newBottom := float64(l.y0)+float64(l.h)/2 >= float64(minY+maxY)/2
	seamAt := make(map[int]int, len(path))
	for _, p := range path {
		if vertical {
			seamAt[int(math.Round(p.Y))] = int(math.Round(p.X))
		} else {
			seamAt[int(math.Round(p.X))] = int(math.Round(p.Y))
		}
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			idx := y*cw + x
			li := (y-l.y0)*l.w + (x - l.x0)
			if li < 0 || li >= len(l.mask) || !l.mask[li] || labels[idx] < 0 {
				continue
			}
			own := false
			if vertical {
				sx, ok := seamAt[y]
				if !ok {
					sx = (minX + maxX) / 2
				}
				if newRight {
					own = x >= sx
				} else {
					own = x <= sx
				}
			} else {
				sy, ok := seamAt[x]
				if !ok {
					sy = (minY + maxY) / 2
				}
				if newBottom {
					own = y >= sy
				} else {
					own = y <= sy
				}
			}
			if own {
				labels[idx] = int32(l.index)
			}
		}
	}
	assignUnlabelled(labels, l, cw)
	return path
}

func seamVertical(l *layer, labels []int32, canvasColor []float32, cw, minX, minY, maxX, maxY int, gradWeight float64) []model.Point {
	w, h := maxX-minX+1, maxY-minY+1
	if w <= 0 || h <= 0 {
		return nil
	}
	const inf = 1e18
	cost := make([]float64, w*h)
	prev := make([]int, w*h)
	for yy := 0; yy < h; yy++ {
		y := minY + yy
		for xx := 0; xx < w; xx++ {
			x := minX + xx
			c := seamPixelCost(l, labels, canvasColor, cw, x, y, gradWeight)
			if yy == 0 {
				cost[yy*w+xx] = c
				prev[yy*w+xx] = -1
				continue
			}
			best, bi := inf, xx
			for d := -1; d <= 1; d++ {
				px := xx + d
				if px < 0 || px >= w {
					continue
				}
				if cost[(yy-1)*w+px] < best {
					best, bi = cost[(yy-1)*w+px], px
				}
			}
			cost[yy*w+xx] = c + best
			prev[yy*w+xx] = bi
		}
	}
	bi := 0
	for x := 1; x < w; x++ {
		if cost[(h-1)*w+x] < cost[(h-1)*w+bi] {
			bi = x
		}
	}
	path := make([]model.Point, h)
	for yy := h - 1; yy >= 0; yy-- {
		path[yy] = model.Point{X: float64(minX + bi), Y: float64(minY + yy)}
		if yy > 0 {
			bi = prev[yy*w+bi]
		}
	}
	return path
}

func seamHorizontal(l *layer, labels []int32, canvasColor []float32, cw, minX, minY, maxX, maxY int, gradWeight float64) []model.Point {
	w, h := maxX-minX+1, maxY-minY+1
	if w <= 0 || h <= 0 {
		return nil
	}
	const inf = 1e18
	cost := make([]float64, w*h)
	prev := make([]int, w*h)
	for xx := 0; xx < w; xx++ {
		x := minX + xx
		for yy := 0; yy < h; yy++ {
			y := minY + yy
			c := seamPixelCost(l, labels, canvasColor, cw, x, y, gradWeight)
			if xx == 0 {
				cost[yy*w+xx] = c
				prev[yy*w+xx] = -1
				continue
			}
			best, bi := inf, yy
			for d := -1; d <= 1; d++ {
				py := yy + d
				if py < 0 || py >= h {
					continue
				}
				if cost[py*w+xx-1] < best {
					best, bi = cost[py*w+xx-1], py
				}
			}
			cost[yy*w+xx] = c + best
			prev[yy*w+xx] = bi
		}
	}
	bi := 0
	for y := 1; y < h; y++ {
		if cost[y*w+w-1] < cost[bi*w+w-1] {
			bi = y
		}
	}
	path := make([]model.Point, w)
	for xx := w - 1; xx >= 0; xx-- {
		path[xx] = model.Point{X: float64(minX + xx), Y: float64(minY + bi)}
		if xx > 0 {
			bi = prev[bi*w+xx]
		}
	}
	return path
}

func seamPixelCost(l *layer, labels []int32, canvasColor []float32, cw, x, y int, gradWeight float64) float64 {
	li := (y-l.y0)*l.w + (x - l.x0)
	if li < 0 || li >= len(l.mask) || !l.mask[li] {
		return 1e9
	}
	idx := y*cw + x
	if labels[idx] < 0 {
		return 1e9
	}
	nr, ng, nb := float64(l.rgb[li*3]), float64(l.rgb[li*3+1]), float64(l.rgb[li*3+2])
	er, eg, eb := float64(canvasColor[idx*3]), float64(canvasColor[idx*3+1]), float64(canvasColor[idx*3+2])
	diff := math.Abs(nr-er) + math.Abs(ng-eg) + math.Abs(nb-eb)
	if x > 0 && x+1 < cw {
		left, right := y*cw+x-1, y*cw+x+1
		gn := math.Abs(float64(canvasColor[right*3])-float64(canvasColor[left*3])) +
			math.Abs(float64(canvasColor[right*3+1])-float64(canvasColor[left*3+1])) +
			math.Abs(float64(canvasColor[right*3+2])-float64(canvasColor[left*3+2]))
		diff += gradWeight * gn
	}
	return diff
}

func updateCanvasColor(labels []int32, color []float32, layers []*layer, cw, ch int) {
	if len(layers) == 0 {
		return
	}
	byIndex := make(map[int]*layer, len(layers))
	for _, l := range layers {
		byIndex[l.index] = l
	}
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			li := labels[y*cw+x]
			if li < 0 {
				continue
			}
			if l := byIndex[int(li)]; l != nil {
				r, g, b, ok := l.at(x, y)
				if ok {
					i := (y*cw + x) * 3
					color[i], color[i+1], color[i+2] = float32(r), float32(g), float32(b)
				}
			}
		}
	}
}

func multibandBlend(ctx context.Context, layers []*layer, labels []int32, cw, ch, levels int, sigma float64) ([]float32, error) {
	accColor := make([][]float32, levels)
	accWeight := make([][]float32, levels)
	dims := make([][2]int, levels)
	for l := 0; l < levels; l++ {
		w, h := maxInt(1, cw>>l), maxInt(1, ch>>l)
		dims[l] = [2]int{w, h}
		accColor[l] = make([]float32, w*h*3)
		accWeight[l] = make([]float32, w*h)
	}
	for _, lay := range layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		img := &colorImage{w: lay.w, h: lay.h, pix: make([]float32, lay.w*lay.h*3)}
		mask := &weightImage{w: lay.w, h: lay.h, pix: make([]float32, lay.w*lay.h)}
		any := false
		for y := 0; y < lay.h; y++ {
			for x := 0; x < lay.w; x++ {
				li := y*lay.w + x
				global := (lay.y0+y)*cw + lay.x0 + x
				if lay.mask[li] && labels[global] == int32(lay.index) {
					copy(img.pix[li*3:li*3+3], lay.rgb[li*3:li*3+3])
					mask.pix[li] = 1
					any = true
				}
			}
		}
		if !any {
			continue
		}
		gp := buildColorPyramid(img, levels, sigma)
		wp := buildWeightPyramid(mask, levels, sigma)
		lp := laplacianPyramid(gp)
		for level := 0; level < len(lp) && level < levels; level++ {
			aw, ah := dims[level][0], dims[level][1]
			x0, y0 := lay.x0>>level, lay.y0>>level
			lw, lh := lp[level].w, lp[level].h
			for y := 0; y < lh; y++ {
				gy := y0 + y
				if gy < 0 || gy >= ah {
					continue
				}
				for x := 0; x < lw; x++ {
					gx := x0 + x
					if gx < 0 || gx >= aw {
						continue
					}
					di := (gy*aw + gx) * 3
					si := (y*lw + x) * 3
					wgt := float32(0)
					if level < len(wp) && x < wp[level].w && y < wp[level].h {
						wgt = wp[level].pix[y*wp[level].w+x]
					}
					accWeight[level][gy*aw+gx] += wgt
					accColor[level][di] += lp[level].pix[si] * wgt
					accColor[level][di+1] += lp[level].pix[si+1] * wgt
					accColor[level][di+2] += lp[level].pix[si+2] * wgt
				}
			}
		}
	}
	blended := make([]*colorImage, levels)
	for l := 0; l < levels; l++ {
		w, h := dims[l][0], dims[l][1]
		blended[l] = &colorImage{w: w, h: h, pix: make([]float32, w*h*3)}
		for i := 0; i < w*h; i++ {
			wt := accWeight[l][i]
			if wt > 1e-8 {
				for c := 0; c < 3; c++ {
					blended[l].pix[i*3+c] = accColor[l][i*3+c] / wt
				}
			}
		}
	}
	cur := blended[levels-1]
	for l := levels - 2; l >= 0; l-- {
		up := upsampleColor(cur, dims[l][0], dims[l][1])
		for i := range up.pix {
			up.pix[i] += blended[l].pix[i]
		}
		cur = up
	}
	return cur.pix, nil
}

func seamArtifact(img *image.NRGBA, layers []*layer, seams [][]model.Point, labels []int32, cw int) (float64, float64, float64) {
	var vals []float64
	for _, path := range seams {
		vertical := true
		if len(path) >= 2 {
			dx := math.Abs(path[len(path)-1].X - path[0].X)
			dy := math.Abs(path[len(path)-1].Y - path[0].Y)
			vertical = dy >= dx
		}
		for _, p := range path {
			x, y := int(math.Round(p.X)), int(math.Round(p.Y))
			if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			if y*cw+x >= len(labels) || labels[y*cw+x] < 0 {
				continue
			}
			d := [2]int{1, 0}
			if !vertical {
				d = [2]int{0, 1}
			}
			if x-d[0] < 0 || y-d[1] < 0 || x+d[0] >= img.Bounds().Dx() || y+d[1] >= img.Bounds().Dy() {
				continue
			}
			i1, i2 := img.PixOffset(x-d[0], y-d[1]), img.PixOffset(x+d[0], y+d[1])
			gOut := colorGradient(img.Pix, i1, i2)
			var refs []float64
			for _, l := range layers {
				r1, g1, b1, ok1 := l.at(x-d[0], y-d[1])
				r2, g2, b2, ok2 := l.at(x+d[0], y+d[1])
				if !ok1 || !ok2 {
					continue
				}
				refs = append(refs, math.Abs(r1-r2)+math.Abs(g1-g2)+math.Abs(b1-b2))
			}
			if len(refs) > 0 {
				sort.Float64s(refs)
				ref := refs[len(refs)/2]
				vals = append(vals, math.Abs(gOut-ref))
			}
		}
	}
	if len(vals) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(vals)
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return vals[len(vals)-1], vals[int(float64(len(vals)-1)*0.95)], sum / float64(len(vals))
}

func colorGradient(pix []uint8, i, j int) float64 {
	return math.Abs(float64(pix[i])-float64(pix[j])) +
		math.Abs(float64(pix[i+1])-float64(pix[j+1])) +
		math.Abs(float64(pix[i+2])-float64(pix[j+2]))
}

func clampGain(v, max float64) float64 {
	if math.IsNaN(v) || v <= 0 {
		return 1
	}
	if v > max {
		return max
	}
	if v < 1/max {
		return 1 / max
	}
	return v
}

func clampOffset(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(-80, math.Min(80, v))
}

func clampF(v, lo, hi float64) float32 {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return float32(v)
}

func clamp8(v float32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

var _ = color.NRGBA{}
