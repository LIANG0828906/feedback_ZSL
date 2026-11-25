package sample

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"maskpreview/internal/core"
)

type GenerateConfig struct {
	Seed   int64
	OutDir string
	Sizes  []int
}

type palette struct {
	BgA      core.RGB
	BgDR     int
	BgDG     int
	BgDB     int
	Fg1      core.RGB
	Fg2      core.RGB
	LowDelta core.RGB
}

var palettes = []palette{
	{
		BgA: core.RGB{R: 24, G: 34, B: 54}, BgDR: 34, BgDG: 44, BgDB: 62,
		Fg1: core.RGB{R: 226, G: 72, B: 96}, Fg2: core.RGB{R: 48, G: 196, B: 162},
		LowDelta: core.RGB{R: 32, G: 32, B: 32},
	},
	{
		BgA: core.RGB{R: 18, G: 42, B: 46}, BgDR: 30, BgDG: 52, BgDB: 38,
		Fg1: core.RGB{R: 242, G: 146, B: 54}, Fg2: core.RGB{R: 70, G: 126, B: 230},
		LowDelta: core.RGB{R: 31, G: 32, B: 33},
	},
	{
		BgA: core.RGB{R: 42, G: 30, B: 52}, BgDR: 48, BgDG: 30, BgDB: 54,
		Fg1: core.RGB{R: 98, G: 214, B: 88}, Fg2: core.RGB{R: 232, G: 84, B: 176},
		LowDelta: core.RGB{R: 33, G: 31, B: 32},
	},
}

func Generate(cfg GenerateConfig) (*core.Manifest, error) {
	if cfg.OutDir == "" {
		return nil, fmt.Errorf("output directory is required")
	}
	if len(cfg.Sizes) == 0 {
		cfg.Sizes = []int{64, 96, 144}
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sample directory: %w", err)
	}
	manifest := &core.Manifest{
		Schema:           "maskpreview/samples/v1",
		GeneratorVersion: core.GeneratorVersion,
		Seed:             cfg.Seed,
		Samples:          make([]core.SampleSpec, 0, len(cfg.Sizes)),
	}
	for i, size := range cfg.Sizes {
		if size < 32 {
			return nil, fmt.Errorf("sample size %d is too small", size)
		}
		spec, err := generateOne(cfg, i, size)
		if err != nil {
			return nil, err
		}
		manifest.Samples = append(manifest.Samples, spec)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeAtomic(filepath.Join(cfg.OutDir, "manifest.json"), raw); err != nil {
		return nil, err
	}
	return manifest, nil
}

func generateOne(cfg GenerateConfig, index, size int) (core.SampleSpec, error) {
	p := palettes[index%len(palettes)]
	labels := make([]uint8, size*size)
	rgba := make([]uint8, size*size*4)

	// These normalized geometries deliberately stay disconnected at every
	// supported resolution. Truth below is derived from this analytic label
	// painting, not from the segmentation kernel.
	cx1, cy1 := int(0.28*float64(size)), int(0.69*float64(size))
	r1 := maxInt(5, int(0.145*float64(size)))
	cx2, cy2 := int(0.72*float64(size)), int(0.68*float64(size))
	r2 := maxInt(5, int(0.145*float64(size)))
	cx3, cy3 := int(0.71*float64(size)), int(0.25*float64(size))
	h3 := maxInt(4, int(0.095*float64(size)))

	lowBase := backgroundAt(p, cx3, cy3, size, size)
	lowColor := core.RGB{
		R: clamp8(int(lowBase.R) + int(p.LowDelta.R)),
		G: clamp8(int(lowBase.G) + int(p.LowDelta.G)),
		B: clamp8(int(lowBase.B) + int(p.LowDelta.B)),
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			base := backgroundAt(p, x, y, size, size)
			label := uint8(0)
			c := base
			switch {
			case inCircle(x, y, cx1, cy1, r1):
				label = 1
				c = p.Fg1
			case inDiamond(x, y, cx2, cy2, r2):
				label = 2
				c = p.Fg2
			case inRoundedBlock(x, y, cx3, cy3, h3, maxInt(1, h3/3)):
				label = 3
				c = lowColor
			}
			nr, ng, nb := noiseAt(cfg.Seed, uint64(index), uint64(x), uint64(y))
			c = core.RGB{
				R: clamp8(int(c.R) + nr),
				G: clamp8(int(c.G) + ng),
				B: clamp8(int(c.B) + nb),
			}
			labels[y*size+x] = label
			off := (y*size + x) * 4
			rgba[off] = c.R
			rgba[off+1] = c.G
			rgba[off+2] = c.B
			rgba[off+3] = 255
		}
	}
	// image/png legally omits an all-opaque alpha channel. Keep a single
	// near-opaque alpha sample so the generated asset is deterministically
	// RGBA (four channels), which is part of the sample metadata contract.
	rgba[3] = 254

	regions, err := truthRegions(labels, size, size, map[int]core.RGB{
		1: p.Fg1,
		2: p.Fg2,
		3: lowColor,
	})
	if err != nil {
		return core.SampleSpec{}, err
	}
	if len(regions) != 3 {
		return core.SampleSpec{}, fmt.Errorf("generator produced %d regions at %dx%d, want 3", len(regions), size, size)
	}

	id := fmt.Sprintf("icon_%d", size)
	imageFile := id + ".png"
	maskFile := id + "_truth.png"
	img := &image.RGBA{Pix: rgba, Stride: size * 4, Rect: image.Rect(0, 0, size, size)}
	var imageBuf bytes.Buffer
	if err := png.Encode(&imageBuf, img); err != nil {
		return core.SampleSpec{}, fmt.Errorf("encode sample image: %w", err)
	}
	if err := writeAtomic(filepath.Join(cfg.OutDir, imageFile), imageBuf.Bytes()); err != nil {
		return core.SampleSpec{}, err
	}

	ref := image.NewGray(image.Rect(0, 0, size, size))
	copy(ref.Pix, labels)
	var maskBuf bytes.Buffer
	if err := png.Encode(&maskBuf, ref); err != nil {
		return core.SampleSpec{}, fmt.Errorf("encode reference mask: %w", err)
	}
	if err := writeAtomic(filepath.Join(cfg.OutDir, maskFile), maskBuf.Bytes()); err != nil {
		return core.SampleSpec{}, err
	}

	bin := make([]byte, len(labels))
	for i, label := range labels {
		if label != 0 {
			bin[i] = 1
		}
	}
	fingerprint, err := truthFingerprint(size, size, labels, regions)
	if err != nil {
		return core.SampleSpec{}, err
	}
	imgSum := sha256.Sum256(imageBuf.Bytes())
	spec := core.SampleSpec{
		ID:                id,
		ImageFile:         imageFile,
		ReferenceMaskFile: maskFile,
		Width:             size,
		Height:            size,
		Channels:          4,
		ImageSHA256:       hex.EncodeToString(imgSum[:]),
		TruthFingerprint:  fingerprint,
		MaskHash:          core.HashBytes(bin),
		RegionCount:       len(regions),
		Regions:           regions,
		Seeds: []core.Point{
			{X: cx1, Y: cy1},
			{X: cx2, Y: cy2},
			{X: cx3, Y: cy3},
		},
		DefaultParams:    core.DefaultParams(),
		Quantization:     core.DefaultQuantization(),
		PreviewDelayMS:   core.DefaultPreviewDelayMS,
		PreviewTolerance: core.PreviewTolerance{CentroidPx: 3.0, AreaRelative: 0.22},
	}
	return spec, nil
}

func truthRegions(labels []uint8, width, height int, colors map[int]core.RGB) ([]core.TruthRegion, error) {
	type accum struct {
		area           int
		sumX, sumY     float64
		x0, y0, x1, y1 int
	}
	acc := map[int]*accum{}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			label := int(labels[y*width+x])
			if label == 0 {
				continue
			}
			a := acc[label]
			if a == nil {
				a = &accum{area: 0, x0: x, y0: y, x1: x, y1: y}
				acc[label] = a
			}
			a.area++
			a.sumX += float64(x) + 0.5
			a.sumY += float64(y) + 0.5
			if x < a.x0 {
				a.x0 = x
			}
			if y < a.y0 {
				a.y0 = y
			}
			if x > a.x1 {
				a.x1 = x
			}
			if y > a.y1 {
				a.y1 = y
			}
		}
	}
	ids := make([]int, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]core.TruthRegion, 0, len(ids))
	for i, id := range ids {
		a := acc[id]
		out = append(out, core.TruthRegion{
			ID:       i + 1,
			Label:    id,
			Color:    colors[id],
			Area:     a.area,
			Centroid: core.PointF{X: a.sumX / float64(a.area), Y: a.sumY / float64(a.area)},
			BBox:     core.BBox{X0: a.x0, Y0: a.y0, X1: a.x1, Y1: a.y1},
		})
	}
	return out, nil
}

func truthFingerprint(width, height int, labels []uint8, regions []core.TruthRegion) (string, error) {
	payload := struct {
		Width   int                `json:"width"`
		Height  int                `json:"height"`
		Labels  string             `json:"labels_sha256"`
		Regions []core.TruthRegion `json:"regions"`
	}{
		Width:   width,
		Height:  height,
		Labels:  core.HashBytes(labels),
		Regions: regions,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal truth fingerprint payload: %w", err)
	}
	return core.HashBytes(raw), nil
}

func backgroundAt(p palette, x, y, width, height int) core.RGB {
	denX := maxInt(1, width-1)
	denY := maxInt(1, height-1)
	return core.RGB{
		R: clamp8(int(p.BgA.R) + p.BgDR*x/denX + p.BgDB*y/denY/3),
		G: clamp8(int(p.BgA.G) + p.BgDG*x/denX + p.BgDR*y/denY/4),
		B: clamp8(int(p.BgA.B) + p.BgDB*x/denX + p.BgDG*y/denY/4),
	}
}

func inCircle(x, y, cx, cy, radius int) bool {
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

func inDiamond(x, y, cx, cy, radius int) bool {
	return absInt(x-cx)+absInt(y-cy) <= radius
}

func inRoundedBlock(x, y, cx, cy, half, radius int) bool {
	dx, dy := absInt(x-cx), absInt(y-cy)
	if dx > half || dy > half {
		return false
	}
	cut := radius
	if dx <= half-cut || dy <= half-cut {
		return true
	}
	ox, oy := dx-(half-cut), dy-(half-cut)
	return ox*ox+oy*oy <= cut*cut
}

func noiseAt(seed int64, index, x, y uint64) (int, int, int) {
	z := uint64(seed) + 0x9e3779b97f4a7c15*(index+1) + 0xbf58476d1ce4e5b9*x + 0x94d049bb133111eb*y
	z = splitmix64(z)
	r := int(z&7) - 3
	g := int((z>>8)&7) - 3
	b := int((z>>16)&7) - 3
	return r, g, b
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
