package yuv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type splitMix64 struct {
	state uint64
}

func newSplitMix64(seed uint64) *splitMix64 {
	return &splitMix64{state: seed}
}

func (s *splitMix64) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (s *splitMix64) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(s.next() % uint64(n))
}

func (s *splitMix64) signed(amplitude int) int {
	if amplitude <= 0 {
		return 0
	}
	return s.intn(2*amplitude+1) - amplitude
}

type generatorSpec struct {
	Name            string
	Width           int
	Height          int
	BitDepth        int
	ChromaAlignment string
	Upsample        string
	Quant           Quantization
}

var defaultGeneratorSpecs = []generatorSpec{
	{Name: "game_tile_64x48_8bit_rgb", Width: 64, Height: 48, BitDepth: 8, ChromaAlignment: "left", Upsample: "bilinear", Quant: Quantization{Range: "full", Min: 0, Max: 255}},
	{Name: "vfx_96x64_10bit_video", Width: 96, Height: 64, BitDepth: 10, ChromaAlignment: "center", Upsample: "bilinear", Quant: Quantization{Range: "video", Min: 64, Max: 960}},
	{Name: "ui_128x72_16bit_full", Width: 128, Height: 72, BitDepth: 16, ChromaAlignment: "left", Upsample: "nearest", Quant: Quantization{Range: "full", Min: 0, Max: 65535}},
}

// GenerateSamples creates deterministic YUV 4:2:0 samples and a manifest with
// block-center and point-sample truth. Re-running with the same seed produces
// byte-identical plane files and manifest.
func GenerateSamples(dir string, seed int64) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("sample directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create sample directory: %w", err)
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Generator:     "splitmix64-v1",
		Seed:          seed,
		Images:        make([]ImageMeta, 0, len(defaultGeneratorSpecs)),
	}
	for index, spec := range defaultGeneratorSpecs {
		meta, err := generateImage(dir, spec, uint64(seed)+uint64(index)*0x517cc1b727220a95)
		if err != nil {
			return "", err
		}
		manifest.Images = append(manifest.Images, meta)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	return path, nil
}

func generateImage(dir string, spec generatorSpec, seed uint64) (ImageMeta, error) {
	meta := ImageMeta{
		Name:            spec.Name,
		Width:           spec.Width,
		Height:          spec.Height,
		BitDepth:        spec.BitDepth,
		Channels:        3,
		ChromaAlignment: spec.ChromaAlignment,
		Upsample:        spec.Upsample,
		Endianness:      "little",
		Layout:          "yuv420p",
		Quantization:    spec.Quant,
		Planes:          make(map[string]PlaneSpec, 3),
	}
	scale := 1
	if spec.BitDepth > 8 {
		scale = 1 << (spec.BitDepth - 8)
	}
	maxCode := int(meta.MaxCode())
	lowY, highY := 0, maxCode
	lowC, highC := 0, maxCode
	if spec.Quant.Range == "video" {
		lowY, highY = 16*scale, 235*scale
		lowC, highC = 16*scale, 240*scale
	}
	rng := newSplitMix64(seed)
	yPlane := make([]uint16, spec.Width*spec.Height)
	uPlane := make([]uint16, (spec.Width/2)*(spec.Height/2))
	vPlane := make([]uint16, (spec.Width/2)*(spec.Height/2))
	yRange := highY - lowY
	cRange := highC - lowC
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			checker := ((x/8 + y/8) % 2) * yRange / 9
			gradient := (x*yRange/spec.Width + y*yRange/spec.Height) / 4
			noise := rng.signed(max(1, yRange/70))
			value := lowY + checker + gradient + noise
			if value < lowY {
				value = lowY
			}
			if value > highY {
				value = highY
			}
			yPlane[y*spec.Width+x] = uint16(value)
		}
	}
	cw, ch := spec.Width/2, spec.Height/2
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			u := lowC + (x*7+y*3)*cRange/max(1, cw+ch) + rng.signed(max(0, cRange/80))
			v := lowC + (cw-x+y*5)*cRange/max(1, cw+ch) + rng.signed(max(0, cRange/80))
			if u < lowC {
				u = lowC
			}
			if u > highC {
				u = highC
			}
			if v < lowC {
				v = lowC
			}
			if v > highC {
				v = highC
			}
			uPlane[y*cw+x] = uint16(u)
			vPlane[y*cw+x] = uint16(v)
		}
	}

	blockCount := 4
	blockWidth := max(8, spec.Width/7)
	blockHeight := max(6, spec.Height/6)
	type generatedBlock struct {
		x, y, width, height       int
		yTarget, uTarget, vTarget int
	}
	generatedBlocks := make([]generatedBlock, 0, blockCount)
	for blockIndex := 0; blockIndex < blockCount; blockIndex++ {
		bx := 3 + rng.intn(max(1, spec.Width-blockWidth-6))
		by := 3 + rng.intn(max(1, spec.Height-blockHeight-6))
		yTarget := lowY + (2*blockIndex+1)*yRange/(2*blockCount+1) + rng.signed(yRange/16)
		uTarget := lowC + (blockIndex+1)*cRange/(blockCount+1) + rng.signed(cRange/12)
		vTarget := lowC + (blockCount-blockIndex)*cRange/(blockCount+1) + rng.signed(cRange/12)
		if yTarget < lowY {
			yTarget = lowY
		}
		if yTarget > highY {
			yTarget = highY
		}
		if uTarget < lowC {
			uTarget = lowC
		}
		if uTarget > highC {
			uTarget = highC
		}
		if vTarget < lowC {
			vTarget = lowC
		}
		if vTarget > highC {
			vTarget = highC
		}
		for yy := by; yy < by+blockHeight; yy++ {
			for xx := bx; xx < bx+blockWidth; xx++ {
				edge := xx == bx || yy == by || xx == bx+blockWidth-1 || yy == by+blockHeight-1
				detail := 0
				if edge {
					detail = -yRange / 18
				}
				value := yTarget + detail + rng.signed(max(0, yRange/90))
				yPlane[yy*spec.Width+xx] = uint16(clampInt(value, lowY, highY))
			}
		}
		cx0, cy0 := bx/2, by/2
		cx1, cy1 := (bx+blockWidth-1)/2, (by+blockHeight-1)/2
		for yy := cy0; yy <= cy1; yy++ {
			for xx := cx0; xx <= cx1; xx++ {
				uPlane[yy*cw+xx] = uint16(clampInt(uTarget+rng.signed(max(0, cRange/80)), lowC, highC))
				vPlane[yy*cw+xx] = uint16(clampInt(vTarget+rng.signed(max(0, cRange/80)), lowC, highC))
			}
		}
		generatedBlocks = append(generatedBlocks, generatedBlock{
			x: bx, y: by, width: blockWidth, height: blockHeight,
			yTarget: yTarget, uTarget: uTarget, vTarget: vTarget,
		})
	}
	for blockIndex, block := range generatedBlocks {
		centerX := block.x + block.width/2
		centerY := block.y + block.height/2
		yPlane[centerY*spec.Width+centerX] = uint16(block.yTarget)
		uPlane[(centerY/2)*cw+centerX/2] = uint16(block.uTarget)
		vPlane[(centerY/2)*cw+centerX/2] = uint16(block.vTarget)
		meta.Blocks = append(meta.Blocks, BlockTruth{
			Name:    fmt.Sprintf("block_%d", blockIndex),
			X:       block.x,
			Y:       block.y,
			Width:   block.width,
			Height:  block.height,
			CenterX: centerX,
			CenterY: centerY,
			YValue:  yPlane[centerY*spec.Width+centerX],
			UValue:  uPlane[(centerY/2)*cw+centerX/2],
			VValue:  vPlane[(centerY/2)*cw+centerX/2],
		})
	}

	for i := 0; i < 8; i++ {
		x := rng.intn(spec.Width)
		y := rng.intn(spec.Height)
		cx, cy := x/2, y/2
		meta.Samples = append(meta.Samples, SampleTruth{
			X:       x,
			Y:       y,
			ChromaX: cx,
			ChromaY: cy,
			YValue:  yPlane[y*spec.Width+x],
			UValue:  uPlane[cy*cw+cx],
			VValue:  vPlane[cy*cw+cx],
		})
	}

	planes := []struct {
		name   string
		width  int
		height int
		data   []uint16
	}{
		{"Y", spec.Width, spec.Height, yPlane},
		{"U", cw, ch, uPlane},
		{"V", cw, ch, vPlane},
	}
	for _, step := range planes {
		file := fmt.Sprintf("%s_%s.raw", spec.Name, step.name)
		raw := encodePlaneRaw(step.data, spec.BitDepth)
		if err := os.WriteFile(filepath.Join(dir, file), raw, 0o644); err != nil {
			return ImageMeta{}, fmt.Errorf("write %s plane: %w", step.name, err)
		}
		meta.Planes[step.name] = PlaneSpec{
			File:     file,
			Width:    step.width,
			Height:   step.height,
			Channels: 1,
			BitDepth: spec.BitDepth,
		}
	}
	return meta, nil
}

func encodePlaneRaw(data []uint16, bitDepth int) []byte {
	if bitDepth <= 8 {
		out := make([]byte, len(data))
		for i, value := range data {
			out[i] = byte(value)
		}
		return out
	}
	out := make([]byte, len(data)*2)
	for i, value := range data {
		out[i*2] = byte(value)
		out[i*2+1] = byte(value >> 8)
	}
	return out
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
