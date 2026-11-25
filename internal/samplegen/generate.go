package samplegen

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"uvenhance/internal/apperr"
	"uvenhance/internal/yuv"
)

type Config struct {
	ID     string
	Width  int
	Height int
}

type blockSpec struct {
	Name string
	Rect yuv.Rect
	Y    int
	U    int
	V    int
	Low  bool
}

type prng struct{ state uint64 }

func (r *prng) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *prng) noise(limit int) int {
	if limit <= 0 {
		return 0
	}
	return int(r.next()%uint64(2*limit+1)) - limit
}

func Generate(ctx context.Context, outDir string, cfg Config, seed uint64) (*yuv.Metadata, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, apperr.Meta("width/height", "sample dimensions must be positive and even")
	}
	if cfg.ID == "" {
		return nil, apperr.Meta("id", "sample id is required")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, apperr.IO("create sample directory", err)
	}

	w, h := cfg.Width, cfg.Height
	yFull := make([]uint16, w*h)
	uFull := make([]uint16, w*h)
	vFull := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < w; x++ {
			i := y*w + x
			g := (x*96 + y*64) / max(1, w+h)
			yFull[i] = uint16(clamp(32+g, 0, 255))
			uFull[i] = uint16(clamp(116+x*18/max(1, w), 0, 255))
			vFull[i] = uint16(clamp(142+y*22/max(1, h), 0, 255))
		}
	}

	blocks := makeBlocks(w, h)
	// A wide low-contrast gradient band exercises local contrast and band limiting.
	bandY, bandH := h*3/4, max(16, h/16)
	for y := bandY; y < min(h, bandY+bandH); y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			step := x * 5 / max(1, w)
			yFull[i] = uint16(clamp(int(yFull[i])+step-2, 0, 255))
			uFull[i] = uint16(clamp(int(uFull[i])+step-2, 0, 255))
			vFull[i] = uint16(clamp(int(vFull[i])-step+2, 0, 255))
		}
	}
	for _, b := range blocks {
		for y := b.Rect.Y; y < b.Rect.Y+b.Rect.H; y++ {
			for x := b.Rect.X; x < b.Rect.X+b.Rect.W; x++ {
				i := y*w + x
				yFull[i], uFull[i], vFull[i] = uint16(b.Y), uint16(b.U), uint16(b.V)
			}
		}
		if b.Low {
			for y := b.Rect.Y; y < b.Rect.Y+b.Rect.H; y++ {
				for x := b.Rect.X; x < b.Rect.X+b.Rect.W; x++ {
					if ((x/3)+(y/3))%2 == 0 {
						i := y*w + x
						yFull[i] = uint16(clamp(int(yFull[i])+2, 0, 255))
						uFull[i] = uint16(clamp(int(uFull[i])-2, 0, 255))
						vFull[i] = uint16(clamp(int(vFull[i])+1, 0, 255))
					}
				}
			}
		}
	}

	rng := &prng{state: seed | 1}
	for y := 0; y < h; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < w; x++ {
			i := y*w + x
			yFull[i] = uint16(clamp(int(yFull[i])+rng.noise(2), 0, 255))
			uFull[i] = uint16(clamp(int(uFull[i])+rng.noise(1), 0, 255))
			vFull[i] = uint16(clamp(int(vFull[i])+rng.noise(1), 0, 255))
		}
	}

	yPlane := &yuv.Plane{
		Name: "Y", Width: w, Height: h, BitDepth: 8, QuantMin: 0, QuantMax: 255,
		Data: yFull,
	}
	uPlane := downsample("U", w, h, uFull, seed+11)
	vPlane := downsample("V", w, h, vFull, seed+29)

	md := yuv.Metadata{
		FormatVersion:   1,
		ID:              cfg.ID,
		Width:           w,
		Height:          h,
		BitDepth:        8,
		QuantMin:        0,
		QuantMax:        255,
		ChromaAlignment: yuv.AlignCenter,
		Upsample:        yuv.UpsampleBilin,
		Planes: map[string]yuv.PlaneMeta{
			"Y": {File: "Y.plane", Width: w, Height: h, BitDepth: 8},
			"U": {File: "U.plane", Width: w / 2, Height: h / 2, BitDepth: 8},
			"V": {File: "V.plane", Width: w / 2, Height: h / 2, BitDepth: 8},
		},
		InputHashes: map[string]string{
			"Y": yPlane.ContentHash(),
			"U": uPlane.ContentHash(),
			"V": vPlane.ContentHash(),
		},
	}

	for _, b := range blocks {
		cx, cy := b.Rect.X+b.Rect.W/2, b.Rect.Y+b.Rect.H/2
		md.Blocks = append(md.Blocks, yuv.BlockTruth{
			Name: b.Name, CenterX: cx, CenterY: cy, Rect: b.Rect,
			Y: yPlane.At(cx, cy), U: uPlane.At(cx/2, cy/2), V: vPlane.At(cx/2, cy/2),
			ChromaX: cx / 2, ChromaY: cy / 2, LowDetail: b.Low,
		})
	}

	lumaPoints := []struct{ x, y int }{
		{w / 2, h / 2}, {w / 5, h / 3}, {w * 4 / 5, h / 4},
		{blocks[0].Rect.X + 2, blocks[0].Rect.Y + 2},
		{blocks[len(blocks)-1].Rect.X + 3, blocks[len(blocks)-1].Rect.Y + 3},
		{w / 2, bandY}, {w / 3, bandY + bandH/2},
	}
	for _, pt := range lumaPoints {
		md.LumaSamples = append(md.LumaSamples, yuv.LumaTruth{
			X: pt.x, Y: pt.y, Expected: yPlane.At(pt.x, pt.y),
		})
	}
	for _, pt := range lumaPoints {
		cx, cy := pt.x/2, pt.y/2
		md.ChromaSamples = append(md.ChromaSamples,
			yuv.ChromaTruth{Plane: "U", X: cx, Y: cy, Expected: uPlane.At(cx, cy)},
			yuv.ChromaTruth{Plane: "V", X: cx, Y: cy, Expected: vPlane.At(cx, cy)},
		)
	}
	uUp, err := yuv.UpsampleChroma(ctx, uPlane, w, h, md.ChromaAlignment, md.Upsample)
	if err != nil {
		return nil, err
	}
	vUp, err := yuv.UpsampleChroma(ctx, vPlane, w, h, md.ChromaAlignment, md.Upsample)
	if err != nil {
		return nil, err
	}
	for _, pt := range lumaPoints {
		i := pt.y*w + pt.x
		md.Interleaved = append(md.Interleaved, yuv.InterleavedTruth{
			X: pt.x, Y: pt.y, Expected: yPlane.Data[i], U: uUp[i], V: vUp[i],
			Source: "raw_4:2:0_bilinear_center",
		})
	}

	if err := yuv.WritePlane(filepath.Join(outDir, "Y.plane"), yPlane); err != nil {
		return nil, err
	}
	if err := yuv.WritePlane(filepath.Join(outDir, "U.plane"), uPlane); err != nil {
		return nil, err
	}
	if err := yuv.WritePlane(filepath.Join(outDir, "V.plane"), vPlane); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return nil, apperr.IO("encode metadata", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "metadata.json"), raw, 0o644); err != nil {
		return nil, apperr.IO("write metadata", err)
	}
	return &md, nil
}

func downsample(name string, w, h int, full []uint16, seed uint64) *yuv.Plane {
	p := &yuv.Plane{
		Name: name, Width: w / 2, Height: h / 2, BitDepth: 8, QuantMin: 0, QuantMax: 255,
		Data: make([]uint16, w/2*h/2),
	}
	rng := &prng{state: seed | 1}
	for y := 0; y < h/2; y++ {
		for x := 0; x < w/2; x++ {
			i0 := (2*y)*w + 2*x
			i1 := i0 + 1
			i2 := i0 + w
			i3 := i2 + 1
			sum := int(full[i0]) + int(full[i1]) + int(full[i2]) + int(full[i3])
			// Preserve deterministic sub-LSB texture while keeping the 2x2 box average.
			v := (sum + 2) / 4
			if rng.next()%5 == 0 {
				v += rng.noise(1)
			}
			p.Data[y*p.Width+x] = uint16(clamp(v, 0, 255))
		}
	}
	return p
}

func makeBlocks(w, h int) []blockSpec {
	return []blockSpec{
		{Name: "warm_red", Rect: yuv.Rect{X: w / 16, Y: h / 10, W: max(24, w/9), H: max(16, h/7)}, Y: 68, U: 188, V: 178},
		{Name: "cool_blue", Rect: yuv.Rect{X: w * 3 / 5, Y: h / 9, W: max(24, w/8), H: max(16, h/6)}, Y: 92, U: 82, V: 188},
		{Name: "green", Rect: yuv.Rect{X: w / 9, Y: h * 2 / 5, W: max(24, w/7), H: max(16, h/5)}, Y: 136, U: 72, V: 58},
		{Name: "magenta", Rect: yuv.Rect{X: w * 2 / 3, Y: h * 2 / 5, W: max(24, w/8), H: max(16, h/5)}, Y: 104, U: 176, V: 64},
		{Name: "low_detail", Rect: yuv.Rect{X: w / 3, Y: h * 2 / 3, W: max(32, w/6), H: max(20, h/8)}, Y: 126, U: 124, V: 132, Low: true},
		{Name: "gray_step", Rect: yuv.Rect{X: w * 3 / 4, Y: h * 3 / 5, W: max(24, w/10), H: max(16, h/7)}, Y: 148, U: 128, V: 128},
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Summary(md *yuv.Metadata) string {
	return fmt.Sprintf("%s %dx%d depth=%d align=%s upsample=%s blocks=%d",
		md.ID, md.Width, md.Height, md.BitDepth, md.ChromaAlignment, md.Upsample, len(md.Blocks))
}
