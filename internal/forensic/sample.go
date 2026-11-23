package forensic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

type SampleSpec struct {
	ID            string
	Width         int
	Height        int
	Channels      int
	Seed          uint64
	TamperRegions []Rect
}

type GeneratedManifest struct {
	Manifest *Manifest
	Path     string
}

func DefaultSampleSpecs() []SampleSpec {
	return []SampleSpec{
		{ID: "game_tex_rgb_160x112", Width: 160, Height: 112, Channels: 3, Seed: 0x42A1, TamperRegions: []Rect{
			{X0: 32, Y0: 32, X1: 64, Y1: 64},
			{X0: 96, Y0: 48, X1: 128, Y1: 80},
		}},
		{ID: "game_tex_gray_96x96", Width: 96, Height: 96, Channels: 1, Seed: 0x42A2, TamperRegions: []Rect{
			{X0: 16, Y0: 48, X1: 48, Y1: 80},
			{X0: 64, Y0: 16, X1: 80, Y1: 32},
		}},
		{ID: "ui_atlas_rgb_128x80", Width: 128, Height: 80, Channels: 3, Seed: 0x42A3, TamperRegions: []Rect{
			{X0: 64, Y0: 16, X1: 96, Y1: 48},
		}},
		{ID: "mask_gray_64x64", Width: 64, Height: 64, Channels: 1, Seed: 0x42A4, TamperRegions: []Rect{
			{X0: 32, Y0: 32, X1: 48, Y1: 48},
		}},
	}
}

// GenerateSamples writes all files from a fixed seed set. It never uses the
// network and deliberately omits wall-clock values from the manifest.
func GenerateSamples(dir string) (*GeneratedManifest, error) {
	return GenerateSamplesWithSpecs(dir, DefaultSampleSpecs(), 0x5EED2026)
}

func GenerateSamplesWithSpecs(dir string, specs []SampleSpec, manifestSeed uint64) (*GeneratedManifest, error) {
	if dir == "" {
		return nil, ParamError("dir", "sample output directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, DecodeError("create sample directory", err)
	}
	m := &Manifest{Format: ManifestFormat, Seed: manifestSeed}
	for _, spec := range specs {
		entry, err := generateOne(dir, spec)
		if err != nil {
			return nil, err
		}
		m.Images = append(m.Images, entry)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, DecodeError("encode manifest", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "manifest.json")
	if err := atomicWrite(path, data, 0o644); err != nil {
		return nil, DecodeError("write manifest", err)
	}
	return &GeneratedManifest{Manifest: m, Path: path}, nil
}

func generateOne(dir string, spec SampleSpec) (SampleEntry, error) {
	if spec.ID == "" || spec.Width <= 0 || spec.Height <= 0 {
		return SampleEntry{}, ParamError("sample", "invalid sample identifier or dimensions")
	}
	if spec.Channels != 1 && spec.Channels != 3 {
		return SampleEntry{}, ParamError("channels", "only one and three channel samples are supported")
	}
	base := make([]uint8, spec.Width*spec.Height*spec.Channels)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			for c := 0; c < spec.Channels; c++ {
				v := uint8(88 + ((x*7 + y*3 + int(spec.Seed%29) + c*11) % 48))
				base[(y*spec.Width+x)*spec.Channels+c] = v
			}
		}
	}

	prims := make([]Primitive, 0, 4)
	prims = append(prims,
		Primitive{Kind: "rect", Rect: Rect{X0: spec.Width / 10, Y0: spec.Height / 8, X1: spec.Width / 3, Y1: spec.Height / 2}, Color: 196},
		Primitive{Kind: "rect", Rect: Rect{X0: spec.Width * 2 / 5, Y0: spec.Height * 3 / 5, X1: spec.Width * 3 / 4, Y1: spec.Height * 9 / 10}, Color: 54},
		Primitive{Kind: "rect", Rect: Rect{X0: spec.Width * 3 / 4, Y0: spec.Height / 6, X1: spec.Width * 9 / 10, Y1: spec.Height / 3}, Color: 226},
	)
	for _, p := range prims {
		fillRect(base, spec.Width, spec.Channels, p.Rect, p.Color)
	}

	baseline := append([]byte(nil), base...)
	mask := make([]uint8, spec.Width*spec.Height)
	regions := make([]TamperRegion, 0, len(spec.TamperRegions))
	for i, r := range spec.TamperRegions {
		r = Rect{X0: maxInt(0, r.X0), Y0: maxInt(0, r.Y0), X1: minInt(spec.Width, r.X1), Y1: minInt(spec.Height, r.Y1)}
		if r.Width() <= 0 || r.Height() <= 0 {
			return SampleEntry{}, ParamError("tamperRegions", "tamper region is outside the image")
		}
		regions = append(regions, TamperRegion{ID: fmt.Sprintf("tamper-%02d", i+1), Rect: r})
		for y := r.Y0; y < r.Y1; y++ {
			for x := r.X0; x < r.X1; x++ {
				mask[y*spec.Width+x] = 255
				for c := 0; c < spec.Channels; c++ {
					noise := int(splitmix64(spec.Seed^uint64(x)*0x9E3779B185EBCA87^uint64(y)*0xC2B2AE3D27D4EB4F^uint64(c)*0x165667B19E3779F9) % 121)
					idx := (y*spec.Width+x)*spec.Channels + c
					base[idx] = uint8(clampInt(int(base[idx])+noise-60, 0, 255))
				}
			}
		}
	}

	img := pixelsToImage(spec.Width, spec.Height, spec.Channels, base)
	baseImg := pixelsToImage(spec.Width, spec.Height, spec.Channels, baseline)
	maskImg := image.NewGray(image.Rect(0, 0, spec.Width, spec.Height))
	copy(maskImg.Pix, mask)

	imageName := spec.ID + ".png"
	baselineName := spec.ID + "_baseline.png"
	maskName := spec.ID + "_mask.png"
	imageBytes, err := encodePNG(img)
	if err != nil {
		return SampleEntry{}, DecodeError("encode sample image", err)
	}
	baselineBytes, err := encodePNG(baseImg)
	if err != nil {
		return SampleEntry{}, DecodeError("encode baseline image", err)
	}
	maskBytes, err := encodePNG(maskImg)
	if err != nil {
		return SampleEntry{}, DecodeError("encode tamper mask", err)
	}
	for name, data := range map[string][]byte{
		imageName: imageBytes, baselineName: baselineBytes, maskName: maskBytes,
	} {
		if err := atomicWrite(filepath.Join(dir, name), data, 0o644); err != nil {
			return SampleEntry{}, DecodeError("write sample file", err)
		}
	}
	return SampleEntry{
		ID:             spec.ID,
		ImagePath:      imageName,
		BaselinePath:   baselineName,
		MaskPath:       maskName,
		Width:          spec.Width,
		Height:         spec.Height,
		Channels:       spec.Channels,
		Seed:           spec.Seed,
		PrimitiveCount: len(prims),
		Primitives:     prims,
		TamperRegions:  regions,
		MaskWidth:      spec.Width,
		MaskHeight:     spec.Height,
		MaskTolerance:  0.08,
		ImageSHA256:    sha256Hex(imageBytes),
		BaselineSHA256: sha256Hex(baselineBytes),
	}, nil
}

func pixelsToImage(width, height, channels int, pix []uint8) image.Image {
	if channels == 1 {
		img := image.NewGray(image.Rect(0, 0, width, height))
		copy(img.Pix, pix)
		return img
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * channels
			img.SetNRGBA(x, y, color.NRGBA{R: pix[i], G: pix[i+1], B: pix[i+2], A: 255})
		}
	}
	return img
}

func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, DecodeError("read manifest", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, DecodeError("decode manifest", err)
	}
	if m.Format != ManifestFormat {
		return nil, MetaError("", "format", "unsupported manifest format")
	}
	if len(m.Images) == 0 {
		return nil, MetaError("", "images", "manifest contains no images")
	}
	seen := map[string]bool{}
	for i := range m.Images {
		e := &m.Images[i]
		if e.ID == "" {
			return nil, MetaError("", "id", "sample image identifier is required")
		}
		if seen[e.ID] {
			return nil, MetaError(e.ID, "id", "duplicate image identifier")
		}
		seen[e.ID] = true
		if e.Width <= 0 || e.Height <= 0 {
			return nil, MetaError(e.ID, "width/height", "image dimensions must be positive")
		}
		if e.Channels != 1 && e.Channels != 3 {
			return nil, MetaError(e.ID, "channels", "only one and three channel images are supported")
		}
		if e.PrimitiveCount != len(e.Primitives) {
			return nil, MetaError(e.ID, "primitiveCount", fmt.Sprintf("declared %d but found %d", e.PrimitiveCount, len(e.Primitives)))
		}
		if e.MaskWidth != e.Width || e.MaskHeight != e.Height {
			return nil, MetaError(e.ID, "maskWidth/maskHeight", "tamper mask dimensions do not match the image")
		}
		if e.MaskTolerance < 0 || e.MaskTolerance > 1 {
			return nil, MetaError(e.ID, "maskTolerance", "must be within [0,1]")
		}
		for _, p := range e.Primitives {
			if !validRectInside(p.Rect, e.Width, e.Height) {
				return nil, MetaError(e.ID, "primitives", "primitive boundary is outside the image")
			}
		}
		for _, r := range e.TamperRegions {
			if !validRectInside(r.Rect, e.Width, e.Height) {
				return nil, MetaError(e.ID, "tamperRegions", "tamper region is outside the image")
			}
		}
	}
	return &m, nil
}

func LoadSample(manifestPath, imageID string) (*Image, error) {
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(manifestPath)
	for _, e := range m.Images {
		if e.ID != imageID {
			continue
		}
		imagePath := resolveRelative(baseDir, e.ImagePath)
		baselinePath := resolveRelative(baseDir, e.BaselinePath)
		maskPath := resolveRelative(baseDir, e.MaskPath)
		imageBytes, err := os.ReadFile(imagePath)
		if err != nil {
			return nil, DecodeError("read sample image "+e.ID, err)
		}
		if e.ImageSHA256 != "" && sha256Hex(imageBytes) != e.ImageSHA256 {
			return nil, DecodeError("sample image checksum mismatch "+e.ID, nil)
		}
		pix, width, height, channels, err := decodeCanonical(imageBytes)
		if err != nil {
			return nil, DecodeError("decode sample image "+e.ID, err)
		}
		if width != e.Width {
			return nil, MetaError(e.ID, "width", fmt.Sprintf("manifest=%d image=%d", e.Width, width))
		}
		if height != e.Height {
			return nil, MetaError(e.ID, "height", fmt.Sprintf("manifest=%d image=%d", e.Height, height))
		}
		if channels != e.Channels {
			return nil, MetaError(e.ID, "channels", fmt.Sprintf("manifest=%d image=%d", e.Channels, channels))
		}
		baselineBytes, err := os.ReadFile(baselinePath)
		if err != nil {
			return nil, DecodeError("read baseline image "+e.ID, err)
		}
		if e.BaselineSHA256 != "" && sha256Hex(baselineBytes) != e.BaselineSHA256 {
			return nil, DecodeError("baseline image checksum mismatch "+e.ID, nil)
		}
		baseline, bw, bh, bc, err := decodeCanonical(baselineBytes)
		if err != nil {
			return nil, DecodeError("decode baseline image "+e.ID, err)
		}
		if bw != e.Width || bh != e.Height || bc != e.Channels {
			return nil, MetaError(e.ID, "baseline", "baseline dimensions or channels differ from the manifest")
		}
		maskBytes, err := os.ReadFile(maskPath)
		if err != nil {
			return nil, DecodeError("read tamper mask "+e.ID, err)
		}
		mask, mw, mh, err := decodeMask(maskBytes)
		if err != nil {
			return nil, DecodeError("decode tamper mask "+e.ID, err)
		}
		if mw != e.MaskWidth || mh != e.MaskHeight {
			return nil, MetaError(e.ID, "maskWidth/maskHeight", fmt.Sprintf("manifest=%dx%d mask=%dx%d", e.MaskWidth, e.MaskHeight, mw, mh))
		}
		if mw != e.Width || mh != e.Height {
			return nil, MetaError(e.ID, "maskWidth/maskHeight", "mask does not cover the image")
		}
		return &Image{
			ID: e.ID, Width: width, Height: height, Channels: channels,
			Pix: pix, Baseline: baseline, Mask: mask, ImagePNG: imageBytes,
			Entry: e, ManifestPath: manifestPath,
		}, nil
	}
	return nil, MetaError(imageID, "id", "sample image was not found in the manifest")
}

func resolveRelative(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func decodeCanonical(data []byte) ([]uint8, int, int, int, error) {
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, 0, err
	}
	bounds := im.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	channels := 3
	switch im.(type) {
	case *image.Gray, *image.Gray16:
		channels = 1
	}
	if channels == 3 {
		opaque := true
		for y := bounds.Min.Y; y < bounds.Max.Y && opaque; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				_, _, _, a := im.At(x, y).RGBA()
				if a != 0xffff {
					opaque = false
					break
				}
			}
		}
		if !opaque {
			return nil, 0, 0, 0, fmt.Errorf("alpha channel is not supported")
		}
	}
	out := make([]uint8, w*h*channels)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			nrgba := color.NRGBAModel.Convert(im.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			i := (y*w + x) * channels
			if channels == 1 {
				out[i] = nrgba.R
			} else {
				out[i], out[i+1], out[i+2] = nrgba.R, nrgba.G, nrgba.B
			}
		}
	}
	return out, w, h, channels, nil
}

func decodeMask(data []byte) ([]uint8, int, int, error) {
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	bounds := im.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	out := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			gray := color.GrayModel.Convert(im.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.Gray)
			if gray.Y > 0 {
				out[y*w+x] = 255
			}
		}
	}
	return out, w, h, nil
}

func encodePNG(im image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, im); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fillRect(pix []uint8, width, channels int, r Rect, value int) {
	for y := r.Y0; y < r.Y1; y++ {
		for x := r.X0; x < r.X1; x++ {
			i := (y*width + x) * channels
			for c := 0; c < channels; c++ {
				pix[i+c] = uint8(clampInt(value+c*7, 0, 255))
			}
		}
	}
}

func validRectInside(r Rect, width, height int) bool {
	return r.X0 >= 0 && r.Y0 >= 0 && r.X1 > r.X0 && r.Y1 > r.Y0 && r.X1 <= width && r.Y1 <= height
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
