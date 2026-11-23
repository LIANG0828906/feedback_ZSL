package workbench

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

type blockRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r blockRect) pixels() int {
	return r.W * r.H
}

type renderContext struct {
	scene       *Scene
	cfg         ProcessConfig
	register    *registerContext
	basis       spectralBasis
	xyzToRGB    mat3
	boundaryXY  []XY
	blockWidth  int
	blockHeight int
}

type blockResult struct {
	Rect         blockRect
	Fused        []float64
	LinearRGB    []float64
	Display      []uint8
	GamutMask    []uint8
	Peak         float64
	Mean         float64
	OutOfGamut   int
	OOFSamples   []PixelCoord
	ExposureCode float64
}

func newRenderContext(scene *Scene, cfg ProcessConfig, scale ScaleDecision) (*renderContext, error) {
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	toXYZ, fromXYZ, ok := gamutMatrices(scene.Meta.TargetGamut)
	if !ok {
		return nil, fieldErr(CodePara, "targetGamut", scene.Meta.TargetGamut, []string{"sRGB", "DisplayP3", "Rec2020"}, "target gamut is unsupported")
	}
	_ = toXYZ
	register, err := newRegisterContext(scene, cfg)
	if err != nil {
		return nil, err
	}
	for b := 0; b < scene.Meta.BandCount; b++ {
		if err := register.validateOffset(scene.Truth.Registration[b]); err != nil {
			return nil, err
		}
	}
	basis := buildSpectralBasis(scene.Meta.Bands)
	return &renderContext{
		scene:       scene,
		cfg:         cfg,
		register:    register,
		basis:       basis,
		xyzToRGB:    fromXYZ,
		boundaryXY:  gamutBoundary(scene.Meta.TargetGamut),
		blockWidth:  maxInt(1, scale.TileWidth),
		blockHeight: maxInt(1, scale.TileHeight),
	}, nil
}

func (ctx *renderContext) processPixel(x, y int) (float64, [3]float64, bool, int, error) {
	aligned, err := ctx.register.alignedPixel(x, y)
	if err != nil {
		return 0, [3]float64{}, false, -1, err
	}
	fused := fuseExposurePixel(ctx.scene, x, y, ctx.cfg)
	idx := y*ctx.scene.Meta.Width + x
	if ctx.cfg.DetailStrength != 0 {
		fused += ctx.cfg.DetailStrength * ctx.scene.Detail[idx]
	}
	fused = math.Max(0, fused)

	var meanBand float64
	for b := 0; b < ctx.scene.Meta.BandCount; b++ {
		meanBand += aligned[b]
	}
	meanBand /= float64(ctx.scene.Meta.BandCount)
	scale := 0.0
	if meanBand > 1e-12 {
		scale = fused / meanBand
	}
	var xyz [3]float64
	for b := 0; b < ctx.scene.Meta.BandCount; b++ {
		radiance := aligned[b] * scale
		if radiance < 0 {
			radiance = 0
		}
		xyz[0] += radiance * ctx.basis.bandXYZ[b][0]
		xyz[1] += radiance * ctx.basis.bandXYZ[b][1]
		xyz[2] += radiance * ctx.basis.bandXYZ[b][2]
	}
	xyz = adaptXYZ(xyz, ctx.scene.Meta.SourceWhitePoint, ctx.scene.Meta.WhitePoint)
	raw := mulMatVec(ctx.xyzToRGB, xyz)
	oog := false
	badChannel := -1
	for c, v := range raw {
		if !finite(v) {
			e := fieldErr(CodeGamut, fmt.Sprintf("linearRGB[%d]", c), v, "finite", "pixel cannot be expressed in target gamut")
			coord := PixelCoord{X: x, Y: y}
			e.Pixel = &coord
			e.Channel = c
			return 0, [3]float64{}, false, c, e
		}
		if v < 0 || v > 1 {
			oog = true
			if badChannel < 0 {
				badChannel = c
			}
		}
	}
	if oog && ctx.cfg.GamutIntent == IntentStrict {
		e := fieldErr(CodeGamut, fmt.Sprintf("linearRGB[%d]", badChannel), raw[badChannel], "0..1", "pixel is outside target gamut under strict intent")
		coord := PixelCoord{X: x, Y: y}
		e.Pixel = &coord
		e.Channel = badChannel
		return 0, [3]float64{}, false, badChannel, e
	}
	mapped, ok, channel := gamutMap(raw)
	if !ok {
		e := fieldErr(CodeGamut, fmt.Sprintf("linearRGB[%d]", channel), raw[channel], "representable", "pixel cannot be compressed into target gamut")
		coord := PixelCoord{X: x, Y: y}
		e.Pixel = &coord
		e.Channel = channel
		return 0, [3]float64{}, false, channel, e
	}
	return fused, mapped, oog, -1, nil
}

func (ctx *renderContext) renderBlock(rect blockRect) (*blockResult, error) {
	if rect.W <= 0 || rect.H <= 0 {
		return nil, fieldErr(CodePara, "block", []int{rect.W, rect.H}, ">0", "block dimensions must be positive")
	}
	if rect.X < 0 || rect.Y < 0 || rect.X+rect.W > ctx.scene.Meta.Width || rect.Y+rect.H > ctx.scene.Meta.Height {
		return nil, fieldErr(CodePara, "block", []int{rect.X, rect.Y, rect.W, rect.H}, "inside image", "block is outside the image")
	}
	n := rect.pixels()
	out := &blockResult{
		Rect:      rect,
		Fused:     make([]float64, n),
		LinearRGB: make([]float64, n*3),
		Display:   make([]uint8, n*3),
		GamutMask: make([]uint8, n),
	}
	var sumFused float64
	for ly := 0; ly < rect.H; ly++ {
		for lx := 0; lx < rect.W; lx++ {
			x := rect.X + lx
			y := rect.Y + ly
			fused, rgb, oog, channel, err := ctx.processPixel(x, y)
			if err != nil {
				if we, ok := err.(*WorkbenchError); ok && we.Pixel == nil {
					coord := PixelCoord{X: x, Y: y}
					we.Pixel = &coord
					if channel >= 0 {
						we.Channel = channel
					}
				}
				return nil, err
			}
			i := ly*rect.W + lx
			out.Fused[i] = fused
			sumFused += fused
			if fused > out.Peak {
				out.Peak = fused
			}
			out.LinearRGB[i*3] = rgb[0]
			out.LinearRGB[i*3+1] = rgb[1]
			out.LinearRGB[i*3+2] = rgb[2]
			for c := 0; c < 3; c++ {
				out.Display[i*3+c] = encodeDisplay(rgb[c], ctx.cfg.DisplayExposure)
			}
			if oog {
				out.GamutMask[i] = 1
				out.OutOfGamut++
				if len(out.OOFSamples) < 64 {
					out.OOFSamples = append(out.OOFSamples, PixelCoord{X: x, Y: y})
				}
			}
		}
	}
	if n > 0 {
		out.Mean = sumFused / float64(n)
	}
	return out, nil
}

func (ctx *renderContext) renderRect(rect blockRect) (*RenderResult, error) {
	start := nowMS()
	block, err := ctx.renderBlock(rect)
	if err != nil {
		return nil, err
	}
	result := &RenderResult{
		SceneID:          ctx.scene.ID,
		Width:            rect.W,
		Height:           rect.H,
		FusedRadiance:    block.Fused,
		LinearRGB:        block.LinearRGB,
		DisplayRGB:       block.Display,
		GamutMask:        block.GamutMask,
		PeakRadiance:     block.Peak,
		MeanRadiance:     block.Mean,
		OutOfGamut:       block.OutOfGamut,
		OutOfGamutSample: block.OOFSamples,
		GamutBoundaryXY:  append([]XY(nil), ctx.boundaryXY...),
		ElapsedMS:        nowMS() - start,
	}
	result.OutOfGamutHash = hashGamutMask(block.GamutMask, block.Rect.W)
	return result, nil
}

func hashGamutMask(mask []uint8, width int) string {
	h := sha256.New()
	var buf [8]byte
	for i, v := range mask {
		if v == 0 {
			continue
		}
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		_, _ = h.Write(buf[:])
	}
	if len(mask) == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func roundRect(x, y, w, h, maxW, maxH int) blockRect {
	if x+w > maxW {
		w = maxW - x
	}
	if y+h > maxH {
		h = maxH - y
	}
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	return blockRect{X: x, Y: y, W: w, H: h}
}
