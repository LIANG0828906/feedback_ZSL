package textureconv

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type ImageSpec struct {
	ID       string
	Width    int
	Height   int
	BitDepth BitDepth
	Layout   Layout
}

type GenerateOptions struct {
	Seed   int64
	Images []ImageSpec
	Plan   *Plan
}

func DefaultImageSpecs() []ImageSpec {
	return []ImageSpec{
		{ID: "atlas_8_rgba", Width: 320, Height: 240, BitDepth: Depth8, Layout: LayoutRGBA},
		{ID: "atlas_16_bgra", Width: 768, Height: 512, BitDepth: Depth16, Layout: LayoutBGRA},
		{ID: "atlas_16_large", Width: 2048, Height: 1536, BitDepth: Depth16, Layout: LayoutRGBA},
	}
}

// SamplePlan is the deterministic conversion plan used by GenerateSample.
func SamplePlan() *Plan {
	roi := Rect{X: 17, Y: 13, W: 97, H: 71}
	return &Plan{
		SchemaVersion: SchemaVersion,
		Entries: []Entry{
			{
				Seq: 1, TargetDepth: Depth16, TargetChannels: 4,
				Permutation: []int{2, 1, 0, 3}, Fill: []FillSpec{}, Drop: []int{},
				Quantization: QuantRound, Alpha: AlphaPreserve,
			},
			{
				Seq: 2, TargetDepth: Depth16, TargetChannels: 4,
				Permutation: []int{0, 1, 2, 3}, Fill: []FillSpec{}, Drop: []int{},
				Quantization: QuantRound, Alpha: AlphaOpaque, Region: &roi,
			},
			{
				Seq: 3, TargetDepth: Depth8, TargetChannels: 4,
				Permutation: []int{3, 0, 1, 2}, Fill: []FillSpec{}, Drop: []int{},
				Quantization: QuantRound, Alpha: AlphaStraighten,
			},
			{
				Seq: 4, TargetDepth: Depth16, TargetChannels: 3,
				Permutation: []int{1, 2, 3}, Fill: []FillSpec{}, Drop: []int{0},
				Quantization: QuantRound, Alpha: AlphaPreserve,
			},
			{
				Seq: 5, TargetDepth: Depth16, TargetChannels: 4,
				Permutation: []int{0, 1, 2, -1}, Fill: []FillSpec{{Channel: 3, Value: 65535}}, Drop: []int{},
				Quantization: QuantRound, Alpha: AlphaPreserve,
			},
		},
	}
}

// GenerateSample writes deterministic source images, a plan, truth samples,
// and a manifest. It never downloads or reads external assets.
func GenerateSample(dir string, opts GenerateOptions) (*SampleBundle, error) {
	if opts.Seed == 0 {
		opts.Seed = 20261125
	}
	if len(opts.Images) == 0 {
		opts.Images = DefaultImageSpecs()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, ioErr("createSampleDir", err)
	}
	plan := clonePlan(opts.Plan)
	if plan == nil {
		plan = SamplePlan()
	}
	if err := plan.ValidateShape(); err != nil {
		return nil, err
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := WritePlanFile(planPath, plan); err != nil {
		return nil, err
	}
	truth := &Truth{SchemaVersion: SchemaVersion, PlanFingerprint: plan.Fingerprint()}
	m := SampleManifest{
		SchemaVersion: SchemaVersion,
		Seed:          opts.Seed,
		PlanPath:      "plan.json",
		TruthPath:     "truth.json",
	}
	for i, spec := range opts.Images {
		if spec.ID == "" {
			spec.ID = fmt.Sprintf("image_%02d", i)
		}
		if spec.Width < 8 || spec.Height < 8 {
			return nil, paramErr(0, "imageSpec", "image %q is too small", spec.ID)
		}
		if !spec.BitDepth.Valid() {
			return nil, paramErr(0, "bitDepth", "image %q has invalid bit depth %d", spec.ID, spec.BitDepth)
		}
		if !spec.Layout.Valid() {
			return nil, paramErr(0, "layout", "image %q has invalid layout %q", spec.ID, spec.Layout)
		}
		im := generateImage(spec, opts.Seed^int64(uint64(i+1)*0x9e3779b97f4a7c15))
		if err := plan.ValidateForImage(im); err != nil {
			return nil, err
		}
		name := spec.ID + ".ximg"
		hash, err := SaveImageFile(filepath.Join(dir, name), im)
		if err != nil {
			return nil, err
		}
		m.Images = append(m.Images, SourceRecord{
			ID: spec.ID, Path: name, Width: im.Width, Height: im.Height,
			BitDepth: im.BitDepth, Layout: im.Layout, SHA256: hash,
		})
		it := ImageTruth{
			ImageID: spec.ID, SourceSHA256: hash, Width: im.Width, Height: im.Height,
		}
		for _, pt := range truthCoordinates(im.Width, im.Height) {
			for _, e := range plan.Entries {
				sample, err := referenceSample(plan, im, e.Seq, pt.X, pt.Y)
				if err != nil {
					return nil, err
				}
				it.Samples = append(it.Samples, sample)
			}
		}
		truth.Images = append(truth.Images, it)
	}
	truth.Fingerprint = truthFingerprint(truth)
	m.TruthFingerprint = truth.Fingerprint
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, ioErr("marshalTruth", err)
	}
	truthData = append(truthData, '\n')
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthData, 0o644); err != nil {
		return nil, ioErr("writeTruth", err)
	}
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, ioErr("marshalManifest", err)
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestData, 0o644); err != nil {
		return nil, ioErr("writeManifest", err)
	}
	return LoadSample(dir)
}

func generateImage(spec ImageSpec, seed int64) *Image {
	limit := spec.BitDepth.Max()
	pix := make([]uint16, spec.Width*spec.Height*4)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			noise := patternNoise(x, y, seed)
			r := scaleU16(x, spec.Width-1, limit)
			g := scaleU16(y, spec.Height-1, limit)
			b := uint16(uint64(noise) * uint64(limit) / 65535)
			a := limit
			if y >= spec.Height*2/3 {
				step := max(1, spec.Width/8)
				level := (x / step) % 5
				a = uint16(uint64(level) * uint64(limit) / 4)
			} else if x >= spec.Width/3 && x < spec.Width*2/3 {
				a = scaleU16(spec.Width-x, max(1, spec.Width/3), limit)
			}
			writeSemanticPixel(pix, spec.Width, spec.Layout, x, y, [4]uint16{r, g, b, a})
		}
	}
	rng := uint64(seed) ^ 0xd1b54a32d192ed03
	blocks := 7
	for i := 0; i < blocks; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		bw := min(spec.Width/5, max(4, int(rng%uint64(max(1, spec.Width/4)))+4))
		rng = rng*6364136223846793005 + 1442695040888963407
		bh := min(spec.Height/5, max(4, int(rng%uint64(max(1, spec.Height/4)))+4))
		rng = rng*6364136223846793005 + 1442695040888963407
		bx := int(rng % uint64(max(1, spec.Width-bw)))
		rng = rng*6364136223846793005 + 1442695040888963407
		by := int(rng % uint64(max(1, spec.Height-bh)))
		rng = rng*6364136223846793005 + 1442695040888963407
		color := [4]uint16{
			scaleU16(int(rng&0xffff), 65535, limit),
			scaleU16(int((rng>>16)&0xffff), 65535, limit),
			scaleU16(int((rng>>32)&0xffff), 65535, limit),
			limit,
		}
		for y := by; y < by+bh; y++ {
			for x := bx; x < bx+bw; x++ {
				writeSemanticPixel(pix, spec.Width, spec.Layout, x, y, color)
			}
		}
	}
	bandY := spec.Height / 3
	bandH := max(2, spec.Height/32)
	for y := bandY; y < min(spec.Height, bandY+bandH); y++ {
		for x := 0; x < spec.Width; x++ {
			v := scaleU16(x, spec.Width-1, limit)
			writeSemanticPixel(pix, spec.Width, spec.Layout, x, y, [4]uint16{limit, v, uint16(uint64(v) / 2), limit})
		}
	}
	return &Image{Width: spec.Width, Height: spec.Height, BitDepth: spec.BitDepth, Layout: spec.Layout, Pixels: pix}
}

func writeSemanticPixel(pix []uint16, width int, layout Layout, x, y int, c [4]uint16) {
	off := (y*width + x) * 4
	if layout == LayoutBGRA {
		pix[off+0], pix[off+1], pix[off+2], pix[off+3] = c[2], c[1], c[0], c[3]
		return
	}
	copy(pix[off:off+4], c[:])
}

func readSemanticPixel(pix []uint16, width int, layout Layout, x, y int) [4]uint16 {
	off := (y*width + x) * 4
	if layout == LayoutBGRA {
		return [4]uint16{pix[off+2], pix[off+1], pix[off+0], pix[off+3]}
	}
	return [4]uint16{pix[off], pix[off+1], pix[off+2], pix[off+3]}
}

func patternNoise(x, y int, seed int64) uint16 {
	v := uint64(uint64(x)*0x9e3779b185ebca87) ^ uint64(uint64(y)*0xc2b2ae3d27d4eb4f) ^ uint64(seed)
	v ^= v >> 33
	v *= 0xff51afd7ed558ccd
	v ^= v >> 33
	return uint16(v)
}

func scaleU16(v, maxV int, limit uint16) uint16 {
	if maxV <= 0 {
		return 0
	}
	if v > maxV {
		v = maxV
	}
	return uint16(uint64(v) * uint64(limit) / uint64(maxV))
}

func truthCoordinates(w, h int) []Point {
	pts := []Point{
		{0, 0}, {w - 1, h - 1}, {w / 2, h / 2}, {w / 4, h / 4},
		{23, 17}, {51, 31}, {120, 90}, {9, h - 3},
	}
	seen := map[Point]bool{}
	out := make([]Point, 0, len(pts))
	for _, p := range pts {
		if p.X < 0 || p.Y < 0 || p.X >= w || p.Y >= h || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// referenceSample intentionally contains a standalone pixel transform. It is
// the generator's truth implementation, independent from ConvertEntry.
func referenceSample(plan *Plan, im *Image, upto, x, y int) (TruthSample, error) {
	values := readSemanticPixel(im.Pixels, im.Width, im.Layout, x, y)
	// Convert semantic RGBA into packed source order.
	src := make([]uint16, 4)
	if im.Layout == LayoutBGRA {
		copy(src, []uint16{values[2], values[1], values[0], values[3]})
	} else {
		copy(src, values[:])
	}
	depth := im.BitDepth
	alpha := 3
	channels := 4
	for _, e := range plan.Entries {
		out, outAlpha := referenceEntry(e, src, depth, channels, alpha, x, y)
		src = out
		depth = e.TargetDepth
		channels = e.TargetChannels
		alpha = outAlpha
		if e.Seq == upto {
			tol := uint16(0)
			if e.TargetDepth != im.BitDepth || (x >= 17 && y >= 13 && x < 114 && y < 84) {
				tol = 1
			}
			return TruthSample{
				Entry: e.Seq, X: x, Y: y, BitDepth: e.TargetDepth,
				Channels: append([]uint16(nil), src...), Tolerance: tol,
			}, nil
		}
	}
	return TruthSample{}, schemaErr(upto, "entry", "unknown entry")
}

func referenceEntry(e Entry, src []uint16, srcDepth BitDepth, srcChannels, srcAlpha, x, y int) ([]uint16, int) {
	if e.Region != nil && !e.Region.Contains(x, y) {
		cp := append([]uint16(nil), src...)
		return cp, srcAlpha
	}
	work := append([]uint16(nil), src...)
	maxV := uint64(srcDepth.Max())
	if srcAlpha >= 0 && srcAlpha < len(work) {
		switch e.Alpha {
		case AlphaOpaque:
			work[srcAlpha] = uint16(maxV)
		case AlphaPremultiply:
			a := uint64(work[srcAlpha])
			for c := range work {
				if c != srcAlpha {
					work[c] = uint16((uint64(work[c])*a + maxV/2) / maxV)
				}
			}
		case AlphaStraighten:
			a := uint64(work[srcAlpha])
			for c := range work {
				if c != srcAlpha {
					if a == 0 {
						work[c] = 0
					} else {
						v := (uint64(work[c])*maxV + a/2) / a
						if v > maxV {
							v = maxV
						}
						work[c] = uint16(v)
					}
				}
			}
		}
	}
	fill := make(map[int]uint16, len(e.Fill))
	for _, f := range e.Fill {
		fill[f.Channel] = f.Value
	}
	out := make([]uint16, e.TargetChannels)
	for dst, s := range e.Permutation {
		if s == -1 {
			out[dst] = fill[dst]
		} else {
			out[dst] = referenceQuantize(work[s], srcDepth, e.TargetDepth, e.Quantization)
		}
	}
	outAlpha := -1
	if srcAlpha >= 0 {
		for dst, s := range e.Permutation {
			if s == srcAlpha {
				outAlpha = dst
				break
			}
		}
	}
	return out, outAlpha
}

func referenceQuantize(v uint16, from, to BitDepth, q Quantization) uint16 {
	if from == to {
		return v
	}
	if from == Depth8 && to == Depth16 {
		return uint16(uint32(v) * math.MaxUint16 / math.MaxUint8)
	}
	dstMax := uint64(to.Max())
	srcMax := uint64(from.Max())
	n := uint64(v) * dstMax
	switch q {
	case QuantCeil:
		return uint16((n + srcMax - 1) / srcMax)
	case QuantFloor:
		return uint16(n / srcMax)
	default:
		return uint16((n + srcMax/2) / srcMax)
	}
}
