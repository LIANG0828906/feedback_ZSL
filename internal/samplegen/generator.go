package samplegen

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

type Config struct {
	OutDir string
	Seed   int64
	Sets   int
}

type setSpec struct {
	ID       string
	Desc     string
	Count    int
	Width    int
	Height   int
	Panorama image.Point
}

var specs = []setSpec{
	{"harbor-06", "six overlapping harbor panels, moderate resolution", 6, 640, 440, image.Pt(2100, 1300)},
	{"nebula-09", "nine rotated observatory panels, larger canvas", 9, 880, 620, image.Pt(3200, 1900)},
	{"ruins-14", "fourteen small exploration panels, long panorama", 14, 520, 380, image.Pt(3500, 1800)},
	{"monolith-05", "five high-resolution monolith panels", 5, 1320, 900, image.Pt(3300, 2000)},
	{"swarm-20", "twenty contact-sheet panels, high count", 20, 560, 400, image.Pt(4500, 1700)},
}

func Generate(cfg Config) error {
	if cfg.Seed == 0 {
		cfg.Seed = 42
	}
	if cfg.Sets <= 0 {
		cfg.Sets = len(specs)
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return err
	}
	for i := 0; i < cfg.Sets; i++ {
		sp := specs[i%len(specs)]
		if i >= len(specs) {
			sp.ID = fmt.Sprintf("%s-%02d", sp.ID, i+1)
			sp.Desc = fmt.Sprintf("%s (variant %d)", sp.Desc, i+1)
		}
		if err := generateSet(cfg.OutDir, cfg.Seed+int64(i)*977, sp); err != nil {
			return fmt.Errorf("%s: %w", sp.ID, err)
		}
	}
	return nil
}

func generateSet(root string, seed int64, sp setSpec) error {
	dir := filepath.Join(root, sp.ID)
	imgDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(seed))
	panorama := synthPanorama(sp.Panorama.X, sp.Panorama.Y, seed)
	manifest := model.Manifest{
		Schema:        "stitchworkbench.samples/v1",
		Seed:          seed,
		SetID:         sp.ID,
		Description:   sp.Desc,
		HomographyTol: 1.5,
		ResidualTol:   2.4,
		SeamTol:       220.0,
	}
	type absImage struct {
		truth model.ImageTruth
		h     model.Matrix3
		rel   model.Matrix3
	}
	abs := make([]absImage, 0, sp.Count)
	margin := 90.0
	left := float64(sp.Width)/2 + margin
	right := float64(sp.Panorama.X) - float64(sp.Width)/2 - margin
	step := (right - left) / math.Max(1, float64(sp.Count-1))
	for i := 0; i < sp.Count; i++ {
		cx := left + step*float64(i)
		cy := float64(sp.Panorama.Y)*0.52 + math.Sin(float64(i)*1.31+float64(seed%17))*float64(sp.Panorama.Y)*0.055
		angle := (rng.Float64()*2 - 1) * 0.095
		scale := 0.96 + rng.Float64()*0.10
		hAbs := placement(cx, cy, angle, scale, float64(sp.Width), float64(sp.Height))
		brightness := 0.84 + rng.Float64()*0.25
		offset := (rng.Float64()*2 - 1) * 7
		name := fmt.Sprintf("images/img_%02d.png", i)
		pix := renderCrop(panorama, sp.Panorama.X, sp.Panorama.Y, hAbs, sp.Width, sp.Height, brightness, offset, seed+int64(i+1)*7919)
		out := image.NewNRGBA(image.Rect(0, 0, sp.Width, sp.Height))
		copy(out.Pix, pix)
		path := filepath.Join(dir, filepath.FromSlash(name))
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		encErr := png.Encode(f, out)
		closeErr := f.Close()
		if encErr != nil {
			return encErr
		}
		if closeErr != nil {
			return closeErr
		}
		ch, err := imaging.PNGChannels(path)
		if err != nil {
			return err
		}
		truth := model.ImageTruth{
			File: name, Width: sp.Width, Height: sp.Height, Channels: ch,
			Brightness: brightness,
		}
		abs = append(abs, absImage{truth: truth, h: hAbs})
	}
	baseInv, ok := abs[0].h.Inverse()
	if !ok {
		return fmt.Errorf("base transform is singular")
	}
	for i := range abs {
		hRel := baseInv.Mul(abs[i].h).Normalized()
		abs[i].rel = hRel
		abs[i].truth.HomographyToBase = hRel.Slice()
		abs[i].truth.Correspondences = correspondences(hRel, sp.Width, sp.Height)
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	corners := []model.Point{{X: 0, Y: 0}, {X: float64(sp.Width), Y: 0}, {X: 0, Y: float64(sp.Height)}, {X: float64(sp.Width), Y: float64(sp.Height)}}
	for _, a := range abs {
		for _, p := range corners {
			x, y := a.rel.Apply(p.X, p.Y)
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
			if x > maxX {
				maxX = x
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	manifest.Canvas = model.Canvas{Width: int(math.Ceil(maxX) - math.Floor(minX)), Height: int(math.Ceil(maxY) - math.Floor(minY))}
	manifest.BaseImage = abs[0].truth.File
	for _, a := range abs {
		manifest.Images = append(manifest.Images, a.truth)
	}
	for i := 1; i < len(abs); i++ {
		n := 0
		for _, cp := range abs[i].truth.Correspondences {
			if cp.Base.X >= 0 && cp.Base.Y >= 0 && cp.Base.X < float64(sp.Width) && cp.Base.Y < float64(sp.Height) {
				n++
			}
		}
		manifest.Overlaps = append(manifest.Overlaps, model.OverlapTruth{Base: abs[0].truth.File, Image: abs[i].truth.File, Count: n})
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644)
}

func placement(cx, cy, angle, scale, w, h float64) model.Matrix3 {
	c, s := math.Cos(angle), math.Sin(angle)
	r := model.Matrix3{c * scale, -s * scale, 0, s * scale, c * scale, 0, 0, 0, 1}
	t1 := model.Matrix3{1, 0, -w / 2, 0, 1, -h / 2, 0, 0, 1}
	t2 := model.Matrix3{1, 0, cx, 0, 1, cy, 0, 0, 1}
	return t2.Mul(r).Mul(t1)
}

func correspondences(h model.Matrix3, w, hgt int) []model.Correspondence {
	out := make([]model.Correspondence, 0, 30)
	for gy := 1; gy <= 5; gy++ {
		for gx := 1; gx <= 6; gx++ {
			p := model.Point{X: float64(w) * float64(gx) / 7, Y: float64(hgt) * float64(gy) / 6}
			x, y := h.Apply(p.X, p.Y)
			out = append(out, model.Correspondence{Image: p, Base: model.Point{X: x, Y: y}})
		}
	}
	return out
}

func synthPanorama(w, h int, seed int64) []uint8 {
	rng := rand.New(rand.NewSource(seed))
	pix := make([]uint8, w*h*4)
	// Low-frequency field.
	phase1, phase2, phase3 := rng.Float64()*6.28, rng.Float64()*6.28, rng.Float64()*6.28
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			r := 34 + 20*math.Sin(fx*11+phase1) + 13*math.Sin(fy*17+phase2)
			g := 43 + 19*math.Sin((fx+fy)*15+phase2) + 11*math.Cos(fx*23)
			b := 52 + 23*math.Cos(fy*13+phase3) + 10*math.Sin((fx-fy)*19)
			i := (y*w + x) * 4
			pix[i], pix[i+1], pix[i+2], pix[i+3] = clamp8(r), clamp8(g), clamp8(b), 255
		}
	}
	// Tiled game-environment motifs create repeatable corners at several scales.
	for tileY := 0; tileY < h; tileY += 64 {
		for tileX := 0; tileX < w; tileX += 64 {
			if ((tileX/64)+(tileY/64))%5 == 0 {
				drawRect(pix, w, h, tileX+4, tileY+4, 56, 56, uint8(60+rng.Intn(90)), uint8(70+rng.Intn(100)), uint8(80+rng.Intn(110)))
			}
		}
	}
	for i := 0; i < 900; i++ {
		x, y := rng.Intn(w), rng.Intn(h)
		rw, rh := 4+rng.Intn(100), 4+rng.Intn(78)
		r := uint8(rng.Intn(230))
		g := uint8(rng.Intn(230))
		b := uint8(rng.Intn(230))
		drawRect(pix, w, h, x, y, rw, rh, r, g, b)
		if i%7 == 0 {
			drawCircle(pix, w, h, x, y, 5+rng.Intn(28), 255-r, 255-g, 255-b)
		}
	}
	for i := 0; i < 240; i++ {
		x0, y0 := rng.Intn(w), rng.Intn(h)
		x1, y1 := x0+(rng.Intn(200)-100), y0+(rng.Intn(160)-80)
		drawLine(pix, w, h, x0, y0, x1, y1, uint8(120+rng.Intn(136)), uint8(120+rng.Intn(136)), uint8(120+rng.Intn(136)))
	}
	// Smooth multi-scale value noise gives every tile a unique local signature.
	// This is representative of real game captures, and avoids a false texture
	// that is only composed of repeated primitives.
	for oi, cell := range []int{7, 15, 31, 63} {
		addValueNoise(pix, w, h, cell, seed+int64(oi)*104729, float64(18-oi*3))
	}
	// Fine deterministic texture shared by every crop.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			n := uint8((x*37 + y*73 + int(seed)) & 15)
			pix[i] = clamp8(float64(pix[i]) + float64(n)/3)
			pix[i+1] = clamp8(float64(pix[i+1]) + float64((n+5)&15)/3)
			pix[i+2] = clamp8(float64(pix[i+2]) + float64((n+9)&15)/3)
		}
	}
	return pix
}

func addValueNoise(pix []uint8, w, h, cell int, seed int64, amp float64) {
	rng := rand.New(rand.NewSource(seed))
	gw, gh := w/cell+2, h/cell+2
	grid := make([]float64, gw*gh*3)
	for i := range grid {
		grid[i] = rng.Float64()*2 - 1
	}
	for y := 0; y < h; y++ {
		fy := float64(y) / float64(cell)
		y0 := int(fy)
		fyy := fy - float64(y0)
		fyy = fyy * fyy * (3 - 2*fyy)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(cell)
			x0 := int(fx)
			fxx := fx - float64(x0)
			fxx = fxx * fxx * (3 - 2*fxx)
			i := (y*w + x) * 4
			for c := 0; c < 3; c++ {
				a := grid[(y0*gw+x0)*3+c]*(1-fxx) + grid[(y0*gw+x0+1)*3+c]*fxx
				b := grid[((y0+1)*gw+x0)*3+c]*(1-fxx) + grid[((y0+1)*gw+x0+1)*3+c]*fxx
				v := a*(1-fyy) + b*fyy
				pix[i+c] = clamp8(float64(pix[i+c]) + v*amp)
			}
		}
	}
}

func renderCrop(src []uint8, sw, sh int, h model.Matrix3, w, hgt int, brightness, offset float64, seed int64) []uint8 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]uint8, w*hgt*4)
	for y := 0; y < hgt; y++ {
		for x := 0; x < w; x++ {
			wx, wy := h.Apply(float64(x)+0.5, float64(y)+0.5)
			r, g, b, ok := bilinear(src, sw, sh, wx, wy)
			if !ok {
				r, g, b = 8, 10, 14
			}
			vx := (float64(x)/float64(w) - 0.5)
			vy := (float64(y)/float64(hgt) - 0.5)
			vignette := 1 - 0.08*(vx*vx+vy*vy)
			noise := (rng.Float64()*2 - 1) * 1.8
			i := (y*w + x) * 4
			out[i] = clamp8((r*brightness+offset+vignette)*vignette + noise)
			out[i+1] = clamp8((g*brightness+offset)*vignette + noise)
			out[i+2] = clamp8((b*brightness+offset)*vignette + noise)
			out[i+3] = 255
		}
	}
	return out
}

func bilinear(pix []uint8, w, h int, x, y float64) (float64, float64, float64, bool) {
	if x < 0 || y < 0 || x >= float64(w-1) || y >= float64(h-1) {
		return 0, 0, 0, false
	}
	x0, y0 := int(x), int(y)
	fx, fy := x-float64(x0), y-float64(y0)
	get := func(c int) float64 {
		i00 := (y0*w + x0) * 4
		i10 := (y0*w + x0 + 1) * 4
		i01 := ((y0+1)*w + x0) * 4
		i11 := ((y0+1)*w + x0 + 1) * 4
		a := float64(pix[i00+c])*(1-fx) + float64(pix[i10+c])*fx
		b := float64(pix[i01+c])*(1-fx) + float64(pix[i11+c])*fx
		return a*(1-fy) + b*fy
	}
	return get(0), get(1), get(2), true
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func drawRect(pix []uint8, w, h, x, y, rw, rh int, r, g, b uint8) {
	for yy := y; yy < y+rh && yy < h; yy++ {
		if yy < 0 {
			continue
		}
		for xx := x; xx < x+rw && xx < w; xx++ {
			if xx < 0 {
				continue
			}
			i := (yy*w + xx) * 4
			pix[i], pix[i+1], pix[i+2], pix[i+3] = r, g, b, 255
		}
	}
}

func drawCircle(pix []uint8, w, h, cx, cy, rad int, r, g, b uint8) {
	for y := cy - rad; y <= cy+rad; y++ {
		for x := cx - rad; x <= cx+rad; x++ {
			if x < 0 || y < 0 || x >= w || y >= h {
				continue
			}
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rad*rad {
				i := (y*w + x) * 4
				pix[i], pix[i+1], pix[i+2], pix[i+3] = r, g, b, 255
			}
		}
	}
}

func drawLine(pix []uint8, w, h, x0, y0, x1, y1 int, r, g, b uint8) {
	dx, dy := x1-x0, y1-y0
	steps := int(math.Max(math.Abs(float64(dx)), math.Abs(float64(dy))))
	if steps == 0 {
		return
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		if x < 0 || y < 0 || x >= w || y >= h {
			continue
		}
		idx := (y*w + x) * 4
		pix[idx], pix[idx+1], pix[idx+2], pix[idx+3] = r, g, b, 255
	}
}

var _ = color.NRGBA{}
