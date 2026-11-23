package sample

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"textureworkbench/internal/tex"
)

type SampleInfo struct {
	ID          string     `json:"id"`
	File        string     `json:"file"`
	Width       int        `json:"width"`
	Height      int        `json:"height"`
	BitDepth    int        `json:"bit_depth"`
	Layout      tex.Layout `json:"layout"`
	Channels    int        `json:"channels"`
	ContentHash string     `json:"content_hash"`
}

type Manifest struct {
	Seed        int64            `json:"seed"`
	Samples     []SampleInfo     `json:"samples"`
	Descriptors []tex.Descriptor `json:"descriptors"`
}

type sampleSpec struct {
	ID       string
	File     string
	Width    int
	Height   int
	BitDepth int
	Layout   tex.Layout
}

var sampleSpecs = []sampleSpec{
	{ID: "gray8", File: "gray8_96x64.twimg", Width: 96, Height: 64, BitDepth: 8, Layout: tex.LayoutGray},
	{ID: "rgb8", File: "rgb8_112x72.twimg", Width: 112, Height: 72, BitDepth: 8, Layout: tex.LayoutRGB},
	{ID: "rgba8", File: "rgba8_128x80.twimg", Width: 128, Height: 80, BitDepth: 8, Layout: tex.LayoutRGBA},
	{ID: "bgr8", File: "bgr8_104x68.twimg", Width: 104, Height: 68, BitDepth: 8, Layout: tex.LayoutBGR},
	{ID: "bgra8", File: "bgra8_120x76.twimg", Width: 120, Height: 76, BitDepth: 8, Layout: tex.LayoutBGRA},
	{ID: "gray16", File: "gray16_96x64.twimg", Width: 96, Height: 64, BitDepth: 16, Layout: tex.LayoutGray},
	{ID: "rgb16", File: "rgb16_112x72.twimg", Width: 112, Height: 72, BitDepth: 16, Layout: tex.LayoutRGB},
	{ID: "rgba16", File: "rgba16_128x80.twimg", Width: 128, Height: 80, BitDepth: 16, Layout: tex.LayoutRGBA},
	{ID: "bgr16", File: "bgr16_104x68.twimg", Width: 104, Height: 68, BitDepth: 16, Layout: tex.LayoutBGR},
	{ID: "bgra16", File: "bgra16_120x76.twimg", Width: 120, Height: 76, BitDepth: 16, Layout: tex.LayoutBGRA},
}

var descriptorSpecs = []tex.Descriptor{
	{ID: "rgb8-to-bgra8-zero", SourceImage: "rgb8", SourceLayout: tex.LayoutRGB, TargetLayout: tex.LayoutBGRA, TargetBitDepth: 8, Permutation: []int{2, 1, 0}, Fill: tex.FillZero},
	{ID: "rgba8-to-rgb8-drop", SourceImage: "rgba8", SourceLayout: tex.LayoutRGBA, TargetLayout: tex.LayoutRGB, TargetBitDepth: 8, Permutation: []int{0, 1, 2, 3}, Fill: tex.FillZero},
	{ID: "rgba8-to-bgra16-repeat", SourceImage: "rgba8", SourceLayout: tex.LayoutRGBA, TargetLayout: tex.LayoutBGRA, TargetBitDepth: 16, Permutation: []int{2, 1, 0, 3}, Fill: tex.FillRepeatAlpha},
	{ID: "gray8-to-rgba16-repeat", SourceImage: "gray8", SourceLayout: tex.LayoutGray, TargetLayout: tex.LayoutRGBA, TargetBitDepth: 16, Permutation: []int{0}, Fill: tex.FillRepeatAlpha},
	{ID: "gray16-to-gray8", SourceImage: "gray16", SourceLayout: tex.LayoutGray, TargetLayout: tex.LayoutGray, TargetBitDepth: 8, Permutation: []int{0}, Fill: tex.FillZero},
	{ID: "rgb16-to-rgba16-repeat", SourceImage: "rgb16", SourceLayout: tex.LayoutRGB, TargetLayout: tex.LayoutRGBA, TargetBitDepth: 16, Permutation: []int{0, 1, 2}, Fill: tex.FillRepeatAlpha},
	{ID: "bgra16-to-rgb8-drop", SourceImage: "bgra16", SourceLayout: tex.LayoutBGRA, TargetLayout: tex.LayoutRGB, TargetBitDepth: 8, Permutation: []int{0, 1, 2, 3}, Fill: tex.FillZero},
	{ID: "bgr8-to-bgra8-zero", SourceImage: "bgr8", SourceLayout: tex.LayoutBGR, TargetLayout: tex.LayoutBGRA, TargetBitDepth: 8, Permutation: []int{0, 1, 2}, Fill: tex.FillZero},
	{ID: "rgb8-to-bgr8-swap", SourceImage: "rgb8", SourceLayout: tex.LayoutRGB, TargetLayout: tex.LayoutBGR, TargetBitDepth: 8, Permutation: []int{2, 1, 0}, Fill: tex.FillZero},
	{ID: "bgra8-to-gray8-drop", SourceImage: "bgra8", SourceLayout: tex.LayoutBGRA, TargetLayout: tex.LayoutGray, TargetBitDepth: 8, Permutation: []int{0, 1, 2, 3}, Fill: tex.FillZero},
	{ID: "rgba16-to-rgba8", SourceImage: "rgba16", SourceLayout: tex.LayoutRGBA, TargetLayout: tex.LayoutRGBA, TargetBitDepth: 8, Permutation: []int{0, 1, 2, 3}, Fill: tex.FillZero},
	{ID: "rgba16-to-bgra16-repeat", SourceImage: "rgba16", SourceLayout: tex.LayoutRGBA, TargetLayout: tex.LayoutBGRA, TargetBitDepth: 16, Permutation: []int{2, 1, 0, 3}, Fill: tex.FillRepeatAlpha},
}

func Both() []tex.Descriptor {
	out := make([]tex.Descriptor, len(descriptorSpecs))
	copy(out, descriptorSpecs)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func Generate(outDir string, seed int64) (*Manifest, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sample directory: %w", err)
	}
	descriptorDir := filepath.Join(outDir, "descriptors")
	if err := os.MkdirAll(descriptorDir, 0o755); err != nil {
		return nil, fmt.Errorf("create descriptor directory: %w", err)
	}
	manifest := &Manifest{Seed: seed}
	for _, spec := range sampleSpecs {
		img := generateImage(spec, seed)
		path := filepath.Join(outDir, spec.File)
		temp, err := os.CreateTemp(outDir, ".sample-*")
		if err != nil {
			return nil, err
		}
		tempName := temp.Name()
		if err := tex.EncodeImage(temp, img); err != nil {
			temp.Close()
			os.Remove(tempName)
			return nil, err
		}
		if err := temp.Sync(); err != nil {
			temp.Close()
			os.Remove(tempName)
			return nil, err
		}
		if err := temp.Close(); err != nil {
			os.Remove(tempName)
			return nil, err
		}
		if err := os.Rename(tempName, path); err != nil {
			os.Remove(tempName)
			return nil, err
		}
		manifest.Samples = append(manifest.Samples, SampleInfo{
			ID:          spec.ID,
			File:        spec.File,
			Width:       spec.Width,
			Height:      spec.Height,
			BitDepth:    spec.BitDepth,
			Layout:      spec.Layout,
			Channels:    spec.Layout.ChannelCount(),
			ContentHash: img.ContentHash,
		})
	}
	for _, descriptor := range descriptorSpecs {
		descriptor = descriptor.Normalize()
		if err := descriptor.Validate(); err != nil {
			return nil, fmt.Errorf("generated descriptor %s is invalid: %w", descriptor.ID, err)
		}
		manifest.Descriptors = append(manifest.Descriptors, descriptor)
		data, err := json.MarshalIndent(descriptor, "", "  ")
		if err != nil {
			return nil, err
		}
		data = append(data, '\n')
		path := filepath.Join(descriptorDir, descriptor.ID+".json")
		if err := atomicWrite(path, data); err != nil {
			return nil, err
		}
	}
	sort.Slice(manifest.Descriptors, func(i, j int) bool {
		return manifest.Descriptors[i].ID < manifest.Descriptors[j].ID
	})
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := atomicWrite(filepath.Join(outDir, "manifest.json"), data); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read sample manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode sample manifest: %w", err)
	}
	if len(manifest.Samples) == 0 {
		return nil, fmt.Errorf("sample manifest contains no samples")
	}
	return &manifest, nil
}

func (m *Manifest) Sample(id string) (SampleInfo, bool) {
	for _, item := range m.Samples {
		if item.ID == id {
			return item, true
		}
	}
	return SampleInfo{}, false
}

func (m *Manifest) Descriptor(id string) (tex.Descriptor, bool) {
	for _, item := range m.Descriptors {
		if item.ID == id {
			return item, true
		}
	}
	return tex.Descriptor{}, false
}

func generateImage(spec sampleSpec, seed int64) *tex.Image {
	channels := spec.Layout.ChannelCount()
	max := uint32(255)
	if spec.BitDepth == 16 {
		max = 65535
	}
	pixels := make([]uint16, spec.Width*spec.Height*channels)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			r, g, b, a := semanticPixel(x, y, spec.Width, spec.Height, max, seed)
			values := map[string]uint32{"R": r, "G": g, "B": b, "A": a}
			if spec.Layout == tex.LayoutGray {
				values["Y"] = uint32((uint64(r)*30 + uint64(g)*59 + uint64(b)*11 + 50) / 100)
			}
			offset := (y*spec.Width + x) * channels
			for c, name := range spec.Layout.Channels() {
				pixels[offset+c] = uint16(values[name])
			}
		}
	}
	img := &tex.Image{
		ID:       spec.ID,
		Width:    spec.Width,
		Height:   spec.Height,
		BitDepth: spec.BitDepth,
		Layout:   spec.Layout,
		Pixels:   pixels,
	}
	img.EnsureHash()
	return img
}

func semanticPixel(x, y, width, height int, max uint32, seed int64) (uint32, uint32, uint32, uint32) {
	band := height / 4
	if band < 1 {
		band = 1
	}
	phase := uint32((seed%997 + 997) % 997)
	scale := func(v uint32) uint32 {
		return uint32((uint64(v)*uint64(max) + 127) / 255)
	}
	switch {
	case y < band:
		return scale(32), scale(176), scale(240), max
	case y < band*2:
		return scale(240), scale(64), scale(32), scale(192)
	case y < band*3:
		// A deterministic gradient with independent channel phases.
		spanX := uint32(1)
		if width > 1 {
			spanX = uint32(width - 1)
		}
		spanY := uint32(1)
		if band > 1 {
			spanY = uint32(band - 1)
		}
		gy := uint32(y - band*2)
		r := (uint32(x)*max/spanX + phase) % (max + 1)
		g := (gy*max/spanY + phase*3) % (max + 1)
		b := ((uint32(x)+gy)*max/(spanX+spanY) + phase*7) % (max + 1)
		a := max
		if max == 255 {
			a = uint32(255 - (x*3+y*2)%128)
		} else {
			a = max - (uint32(x*257+y*131+int(phase)))%(max/3+1)
		}
		return r, g, b, a
	default:
		// Checkerboard of known blocks plus a small deterministic texture.
		cell := 8
		cx, cy := x/cell, y/cell
		texture := uint32((uint64(cx*37+cy*19) + uint64(phase)) % 23)
		if (cx+cy)%2 == 0 {
			return scale(16) + texture, scale(48) + texture, scale(160) + texture, scale(224)
		}
		return scale(208) + texture, scale(uint32(112 + cy%32)), scale(24) + texture, scale(uint32(96 + cx%64))
	}
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
