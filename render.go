package vpf

import (
	"fmt"
	"strings"
)

const linearQ = int64(65535)

type FrameBuffer struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
	Pix      []byte `json:"-"`
}

type PixelBuffer struct {
	MaxPixels int
	RGBA      []byte
	LinR      []int64
	LinG      []int64
	LinB      []int64
	AlphaBP   []int
}

func NewPixelBuffer(maxPixels int) *PixelBuffer {
	if maxPixels <= 0 {
		maxPixels = 1024
	}
	return &PixelBuffer{
		MaxPixels: maxPixels,
		RGBA:      make([]byte, maxPixels*4),
		LinR:      make([]int64, maxPixels),
		LinG:      make([]int64, maxPixels),
		LinB:      make([]int64, maxPixels),
		AlphaBP:   make([]int, maxPixels),
	}
}

func (b *PixelBuffer) use(pixelCount int) error {
	if pixelCount > b.MaxPixels {
		return &Error{Code: CodeBudget, Message: "chunk exceeds per-thread pixel buffer", Details: map[string]int{"requested_pixels": pixelCount, "buffer_pixels": b.MaxPixels}}
	}
	return nil
}

type ChunkResult struct {
	FrameIndex   int      `json:"frame_index"`
	ChunkIndex   int      `json:"chunk_index"`
	Bounds       Rect     `json:"bounds"`
	Pix          []byte   `json:"-"`
	PixelDigest  string   `json:"pixel_digest"`
	CacheKey     string   `json:"cache_key"`
	LayerKeys    []string `json:"layer_keys"`
	ContentHash  string   `json:"content_hash"`
	ParamVersion int      `json:"param_version"`
	PixelCount   int      `json:"pixel_count"`
	Attempts     int      `json:"attempts,omitempty"`
	WorkerID     int      `json:"worker_id,omitempty"`
}

// srgbToLinearQ uses the declared gamma-2 integer linearization. It has no
// floating-point accumulation and is stable across architectures.
func srgbToLinearQ(channel uint8) int64 {
	c := int64(channel)
	return c * c * linearQ / (255 * 255)
}

func linearQToSRGB(value int64) uint8 {
	if value <= 0 {
		return 0
	}
	if value >= linearQ {
		return 255
	}
	radicand := value * 255 * 255 / linearQ
	v := isqrt64(radicand)
	floorSquare := v * v
	nextSquare := (v + 1) * (v + 1)
	if radicand-floorSquare > nextSquare-radicand {
		v++
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func coverageWithOpacity(baseAlpha255 uint8, opacityBP int) int {
	coverage := int64(baseAlpha255) * int64(opacityBP) / OpacityScale
	if coverage < 0 {
		return 0
	}
	if coverage > 255 {
		return 255
	}
	return int(coverage)
}

// RenderChunk renders one tile purely from the canonical project and frame.
func RenderChunk(p *Project, frameIndex, chunkIndex int) (*ChunkResult, error) {
	return RenderChunkInto(p, frameIndex, chunkIndex, nil)
}

func RenderChunkInto(p *Project, frameIndex, chunkIndex int, buffer *PixelBuffer) (*ChunkResult, error) {
	frame, ok := p.FrameByIndex(frameIndex)
	if !ok {
		return nil, schemaError(p.SourceVersion, fmt.Sprintf("$.frames[%d]", frameIndex), "frame index not found")
	}
	if chunkIndex < 0 || chunkIndex >= p.ChunkCount() {
		return nil, schemaError(p.SourceVersion, "$.tile", "chunk index %d outside 0..%d", chunkIndex, p.ChunkCount()-1)
	}
	bounds := p.TileRect(chunkIndex)
	pixelCount := bounds.W * bounds.H
	if buffer == nil {
		buffer = NewPixelBuffer(pixelCount)
	}
	if err := buffer.use(pixelCount); err != nil {
		return nil, err
	}
	for i := 0; i < pixelCount; i++ {
		buffer.LinR[i] = 0
		buffer.LinG[i] = 0
		buffer.LinB[i] = 0
		buffer.AlphaBP[i] = 0
	}

	for _, layer := range p.Stack {
		if !layer.Rect.Intersects(bounds) {
			continue
		}
		for py := 0; py < bounds.H; py++ {
			y := bounds.Y + py
			for px := 0; px < bounds.W; px++ {
				x := bounds.X + px
				offset := py*bounds.W + px
				if !layer.Rect.Contains(x, y) {
					continue
				}
				color, coverage255 := shadeLayerPixel(p, frame, layer, x, y)
				coverage255 = applyLayerOpacity(coverage255, layer.OpacityBP)
				if coverage255 == 0 {
					continue
				}
				compositeLinearPixel(buffer, offset, color, coverage255, layer.Blend)
			}
		}
	}

	pix := buffer.RGBA[:pixelCount*4]
	for i := 0; i < pixelCount; i++ {
		alphaBP := buffer.AlphaBP[i]
		alpha255 := uint8(int64(alphaBP) * 255 / OpacityScale)
		straightR := int64(0)
		straightG := int64(0)
		straightB := int64(0)
		if alphaBP > 0 {
			straightR = buffer.LinR[i] * OpacityScale / int64(alphaBP)
			straightG = buffer.LinG[i] * OpacityScale / int64(alphaBP)
			straightB = buffer.LinB[i] * OpacityScale / int64(alphaBP)
		}
		rgbaOffset := i * 4
		pix[rgbaOffset+0] = linearQToSRGB(straightR)
		pix[rgbaOffset+1] = linearQToSRGB(straightG)
		pix[rgbaOffset+2] = linearQToSRGB(straightB)
		pix[rgbaOffset+3] = alpha255
	}
	// Copy because scheduler-owned buffers are reused by other chunks.
	stablePixels := append([]byte(nil), pix...)
	cacheKey, layerKeys, contentHash, paramVersion := ComputeChunkCacheKey(p, frame, chunkIndex, bounds)
	return &ChunkResult{
		FrameIndex:   frameIndex,
		ChunkIndex:   chunkIndex,
		Bounds:       bounds,
		Pix:          stablePixels,
		PixelDigest:  SHA256Hex(stablePixels),
		CacheKey:     cacheKey,
		LayerKeys:    layerKeys,
		ContentHash:  contentHash,
		ParamVersion: paramVersion,
		PixelCount:   pixelCount,
	}, nil
}

func applyLayerOpacity(coverage255, opacityBP int) int {
	return int(int64(coverage255) * int64(opacityBP) / OpacityScale)
}

func compositeLinearPixel(buffer *PixelBuffer, offset int, color RGBA, coverage255 int, blend BlendMode) {
	sourceAlphaBP := int(int64(coverage255) * OpacityScale / 255)
	sourceR := srgbToLinearQ(color.R)
	sourceG := srgbToLinearQ(color.G)
	sourceB := srgbToLinearQ(color.B)

	dstAlphaBP := buffer.AlphaBP[offset]
	if dstAlphaBP > 0 && blend != BlendNormal {
		dstR := buffer.LinR[offset] * OpacityScale / int64(dstAlphaBP)
		dstG := buffer.LinG[offset] * OpacityScale / int64(dstAlphaBP)
		dstB := buffer.LinB[offset] * OpacityScale / int64(dstAlphaBP)
		switch blend {
		case BlendAdd:
			sourceR = min64(linearQ, sourceR+dstR)
			sourceG = min64(linearQ, sourceG+dstG)
			sourceB = min64(linearQ, sourceB+dstB)
		case BlendMultiply:
			sourceR = sourceR * dstR / linearQ
			sourceG = sourceG * dstG / linearQ
			sourceB = sourceB * dstB / linearQ
		}
	}

	// Lin* stores premultiplied linear color at linearQ scale, with alphaBP scale.
	srcPremultR := sourceR * int64(sourceAlphaBP) / OpacityScale
	srcPremultG := sourceG * int64(sourceAlphaBP) / OpacityScale
	srcPremultB := sourceB * int64(sourceAlphaBP) / OpacityScale
	backdropFactor := int64(OpacityScale - sourceAlphaBP)
	buffer.LinR[offset] = srcPremultR + buffer.LinR[offset]*backdropFactor/OpacityScale
	buffer.LinG[offset] = srcPremultG + buffer.LinG[offset]*backdropFactor/OpacityScale
	buffer.LinB[offset] = srcPremultB + buffer.LinB[offset]*backdropFactor/OpacityScale
	buffer.AlphaBP[offset] = sourceAlphaBP + int(int64(dstAlphaBP)*backdropFactor/OpacityScale)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func shadeLayerPixel(p *Project, frame Frame, layer Layer, x, y int) (RGBA, int) {
	base := layer.Color
	coverage := int(base.A)
	switch layer.Kind {
	case KindGround:
		color := base
		if x%24 == 0 || y%24 == 0 {
			color.R = scaleByte(color.R, 7000)
			color.G = scaleByte(color.G, 7000)
			color.B = scaleByte(color.B, 7000)
		} else if ((x/8)+(y/8))%2 == 0 {
			color.R = scaleByte(color.R, 11200)
			color.G = scaleByte(color.G, 11200)
			color.B = scaleByte(color.B, 11200)
		}
		return color, coverage
	case KindSolid:
		color := base
		edge := x == layer.Rect.X || y == layer.Rect.Y ||
			x == layer.Rect.X+layer.Rect.W-1 || y == layer.Rect.Y+layer.Rect.H-1
		if edge {
			color.R = scaleByte(color.R, 7000)
			color.G = scaleByte(color.G, 7000)
			color.B = scaleByte(color.B, 7000)
		}
		return color, coverage
	case KindGuide:
		if x%16 == 0 || y%16 == 0 {
			return base, coverage
		}
		return base, 0
	case KindRipple:
		sources := EffectiveRipples(p, layer, frame)
		totalWeight := 0
		for _, source := range sources {
			_, weight := RippleWeightBP(source, x, y)
			totalWeight += weight
			if totalWeight > OpacityScale {
				totalWeight = OpacityScale
			}
		}
		maskCoverage := int(int64(base.A) * int64(totalWeight) / OpacityScale)
		color := base
		if totalWeight > 0 {
			boost := 10000 + totalWeight/4
			color.R = scaleByte(base.R, boost)
			color.G = scaleByte(base.G, boost)
			color.B = scaleByte(base.B, boost)
		}
		return color, maskCoverage
	default:
		return base, coverage
	}
}

func scaleByte(value uint8, bp int) uint8 {
	scaled := int64(value) * int64(bp) / OpacityScale
	if scaled < 0 {
		return 0
	}
	if scaled > 255 {
		return 255
	}
	return uint8(scaled)
}

func ComputeChunkCacheKey(p *Project, frame Frame, chunkIndex int, bounds Rect) (string, []string, string, int) {
	upstream := HashStrings(
		"chunk-genesis-v1",
		AlgorithmVersion,
		p.ProjectID,
		fmt.Sprintf("frame=%d", frame.Index),
		fmt.Sprintf("at_ns=%d", frame.AtNS),
		fmt.Sprintf("chunk=%d", chunkIndex),
		fmt.Sprintf("bounds=%d,%d,%d,%d", bounds.X, bounds.Y, bounds.W, bounds.H),
	)
	layerKeys := []string{}
	contentHashes := []string{}
	paramVersion := 1
	for _, layer := range p.Stack {
		if !layer.Rect.Intersects(bounds) {
			continue
		}
		chunkParamVersion := layer.ParamVersion
		extras := []string{
			fmt.Sprintf("opacity=%d", layer.OpacityBP),
			fmt.Sprintf("blend=%s", layer.Blend),
		}
		if layer.Kind == KindRipple {
			sources := EffectiveRipples(p, layer, frame)
			for _, source := range sources {
				influence := Rect{
					X: source.Center.X - source.Radius - 1,
					Y: source.Center.Y - source.Radius - 1,
					W: 2*source.Radius + 3,
					H: 2*source.Radius + 3,
				}
				if !influence.Intersects(bounds) {
					continue
				}
				sourceHash := RippleSourceHash(source)
				contentHashes = append(contentHashes, sourceHash)
				extras = append(extras,
					"ripple_id="+source.ID,
					"ripple_hash="+sourceHash,
					fmt.Sprintf("ripple_param=%d", source.ParamVersion),
				)
				if source.ParamVersion > chunkParamVersion {
					chunkParamVersion = source.ParamVersion
				}
			}
		}
		upstream = HashStrings(
			"layer-chunk-v2",
			upstream,
			layer.ID,
			layer.ContentHash,
			fmt.Sprintf("param=%d", layer.ParamVersion),
			strings.Join(extras, "|"),
		)
		layerKeys = append(layerKeys, upstream)
		contentHashes = append(contentHashes, layer.ContentHash)
		if chunkParamVersion > paramVersion {
			paramVersion = chunkParamVersion
		}
	}
	cacheKey := HashStrings("chunk-final-v1", upstream, "rgba8-linear-gamma2")
	contentHash := HashStrings(contentHashes...)
	return cacheKey, layerKeys, contentHash, paramVersion
}

func AssembleFrame(p *Project, frameIndex int, chunks map[int]*ChunkResult) (*FrameBuffer, error) {
	frame, ok := p.FrameByIndex(frameIndex)
	if !ok {
		return nil, schemaError(p.SourceVersion, "$.frame", "frame %d not found", frameIndex)
	}
	_ = frame
	out := &FrameBuffer{Width: p.Width, Height: p.Height, Channels: p.Channels, Pix: make([]byte, p.Width*p.Height*p.Channels)}
	for index := 0; index < p.ChunkCount(); index++ {
		chunk, ok := chunks[index]
		if !ok {
			return nil, &Error{Code: CodeChunk, Message: "missing chunk during frame assembly", FieldPath: fmt.Sprintf("$.chunks[%d]", index)}
		}
		if chunk.Bounds != p.TileRect(index) {
			return nil, &Error{Code: CodeChunk, Message: "chunk bounds do not match model tile", FieldPath: fmt.Sprintf("$.chunks[%d]", index)}
		}
		for y := 0; y < chunk.Bounds.H; y++ {
			for x := 0; x < chunk.Bounds.W; x++ {
				src := (y*chunk.Bounds.W + x) * 4
				globalX := chunk.Bounds.X + x
				globalY := chunk.Bounds.Y + y
				dst := (globalY*p.Width + globalX) * p.Channels
				copy(out.Pix[dst:dst+4], chunk.Pix[src:src+4])
			}
		}
	}
	return out, nil
}

type PixelMismatch struct {
	ChunkIndex int     `json:"chunk_index"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	Expected   [4]byte `json:"expected"`
	Actual     [4]byte `json:"actual"`
}

func CompareFrames(p *Project, expected, actual *FrameBuffer) *PixelMismatch {
	if expected.Width != actual.Width || expected.Height != actual.Height || expected.Channels != actual.Channels || len(expected.Pix) != len(actual.Pix) {
		return &PixelMismatch{X: -1, Y: -1}
	}
	for i := 0; i < len(expected.Pix); i += 4 {
		if expected.Pix[i] != actual.Pix[i] || expected.Pix[i+1] != actual.Pix[i+1] || expected.Pix[i+2] != actual.Pix[i+2] || expected.Pix[i+3] != actual.Pix[i+3] {
			pixelIndex := i / 4
			y := pixelIndex / expected.Width
			x := pixelIndex % expected.Width
			cols, _ := p.TileGrid()
			chunkX := x / p.TileWidth
			chunkY := y / p.TileHeight
			return &PixelMismatch{
				ChunkIndex: chunkY*cols + chunkX, X: x, Y: y,
				Expected: [4]byte{expected.Pix[i], expected.Pix[i+1], expected.Pix[i+2], expected.Pix[i+3]},
				Actual:   [4]byte{actual.Pix[i], actual.Pix[i+1], actual.Pix[i+2], actual.Pix[i+3]},
			}
		}
	}
	return nil
}

func FrameDigest(frame *FrameBuffer) string {
	return SHA256Hex(frame.Pix)
}
