package sample

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"maskpreview/internal/core"
)

type LoadedSample struct {
	Spec      core.SampleSpec
	Image     *core.Image
	LabelMask []uint8
}

func LoadManifest(dir string) (*core.Manifest, error) {
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.manifest", Message: err.Error()}
	}
	var m core.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.manifest", Message: "invalid JSON: " + err.Error()}
	}
	if m.Schema != "maskpreview/samples/v1" {
		return nil, core.MetaError("sample.manifest", "schema", m.Schema, "maskpreview/samples/v1")
	}
	if len(m.Samples) == 0 {
		return nil, core.MetaError("sample.manifest", "samples", 0, ">0")
	}
	return &m, nil
}

func FindSample(m *core.Manifest, id string) (core.SampleSpec, bool) {
	for _, s := range m.Samples {
		if s.ID == id {
			return s, true
		}
	}
	return core.SampleSpec{}, false
}

func LoadSample(dir string, spec core.SampleSpec) (*LoadedSample, error) {
	imagePath := filepath.Join(dir, spec.ImageFile)
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.load_image", Message: err.Error()}
	}
	sum := sha256.Sum256(raw)
	if spec.ImageSHA256 != "" && hex.EncodeToString(sum[:]) != spec.ImageSHA256 {
		return nil, &core.Error{
			Code:    core.CodeDecode,
			Op:      "sample.load_image",
			Field:   "image_sha256",
			Got:     hex.EncodeToString(sum[:]),
			Want:    spec.ImageSHA256,
			Message: "image content hash mismatch (corrupt or replaced image)",
		}
	}
	if err := validatePNGHeader(raw); err != nil {
		return nil, err
	}
	actualW, actualH, actualChannels, err := pngHeader(raw)
	if err != nil {
		return nil, err
	}
	if actualW != spec.Width {
		return nil, core.MetaError("sample.load_image", "width", actualW, spec.Width)
	}
	if actualH != spec.Height {
		return nil, core.MetaError("sample.load_image", "height", actualH, spec.Height)
	}
	if actualChannels != spec.Channels {
		return nil, core.MetaError("sample.load_image", "channels", actualChannels, spec.Channels)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.load_image", Message: err.Error()}
	}
	if decoded.Bounds().Dx() != spec.Width || decoded.Bounds().Dy() != spec.Height {
		return nil, core.MetaError(
			"sample.load_image",
			"decoded_bounds",
			fmt.Sprintf("%dx%d", decoded.Bounds().Dx(), decoded.Bounds().Dy()),
			fmt.Sprintf("%dx%d", spec.Width, spec.Height),
		)
	}
	img := &core.Image{ID: spec.ID, Width: spec.Width, Height: spec.Height, Channels: 4, Pix: make([]uint8, spec.Width*spec.Height*4)}
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			r, g, b, a := decoded.At(decoded.Bounds().Min.X+x, decoded.Bounds().Min.Y+y).RGBA()
			off := (y*spec.Width + x) * 4
			img.Pix[off] = uint8(r >> 8)
			img.Pix[off+1] = uint8(g >> 8)
			img.Pix[off+2] = uint8(b >> 8)
			img.Pix[off+3] = uint8(a >> 8)
		}
	}
	if err := img.Validate(); err != nil {
		return nil, err
	}

	labels, err := loadReferenceMask(filepath.Join(dir, spec.ReferenceMaskFile), spec)
	if err != nil {
		return nil, err
	}
	bin := make([]byte, len(labels))
	for i, label := range labels {
		if label != 0 {
			bin[i] = 1
		}
	}
	if spec.MaskHash != "" && core.HashBytes(bin) != spec.MaskHash {
		return nil, core.MetaError("sample.load_reference", "mask_hash", core.HashBytes(bin), spec.MaskHash)
	}
	fp, err := truthFingerprint(spec.Width, spec.Height, labels, spec.Regions)
	if err != nil {
		return nil, err
	}
	if spec.TruthFingerprint != "" && fp != spec.TruthFingerprint {
		return nil, core.MetaError("sample.load_reference", "truth_fingerprint", fp, spec.TruthFingerprint)
	}
	return &LoadedSample{Spec: spec, Image: img, LabelMask: labels}, nil
}

func loadReferenceMask(path string, spec core.SampleSpec) ([]uint8, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.load_reference", Message: err.Error()}
	}
	w, h, channels, err := pngHeader(raw)
	if err != nil {
		return nil, err
	}
	if w != spec.Width {
		return nil, core.MetaError("sample.load_reference", "reference.width", w, spec.Width)
	}
	if h != spec.Height {
		return nil, core.MetaError("sample.load_reference", "reference.height", h, spec.Height)
	}
	if channels != 1 {
		return nil, core.MetaError("sample.load_reference", "reference.channels", channels, 1)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, &core.Error{Code: core.CodeDecode, Op: "sample.load_reference", Message: err.Error()}
	}
	labels := make([]uint8, spec.Width*spec.Height)
	type refAccum struct {
		area       int
		sumX, sumY float64
		x0, y0     int
		x1, y1     int
	}
	seen := make(map[uint8]*refAccum)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			gray := imageToGray8(decoded.At(decoded.Bounds().Min.X+x, decoded.Bounds().Min.Y+y))
			labels[y*spec.Width+x] = gray
			if gray > 0 {
				a := seen[gray]
				if a == nil {
					a = &refAccum{x0: x, y0: y, x1: x, y1: y}
					seen[gray] = a
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
	}
	if len(seen) != spec.RegionCount {
		return nil, core.MetaError("sample.load_reference", "region_count", len(seen), spec.RegionCount)
	}
	if len(spec.Regions) != spec.RegionCount {
		return nil, core.MetaError("sample.load_reference", "regions.length", len(spec.Regions), spec.RegionCount)
	}
	for i, region := range spec.Regions {
		a, ok := seen[uint8(region.Label)]
		if !ok {
			return nil, &core.Error{
				Code:    core.CodeMeta,
				Op:      "sample.load_reference",
				Field:   "regions.label",
				Got:     region.Label,
				Want:    "label present in reference mask",
				Message: fmt.Sprintf("region %d label not found", i+1),
			}
		}
		if a.area != region.Area {
			return nil, core.MetaError("sample.load_reference", fmt.Sprintf("regions[%d].area", i), a.area, region.Area)
		}
		centroid := core.PointF{X: a.sumX / float64(a.area), Y: a.sumY / float64(a.area)}
		if !closeFloat(centroid.X, region.Centroid.X) || !closeFloat(centroid.Y, region.Centroid.Y) {
			return nil, core.MetaError("sample.load_reference", fmt.Sprintf("regions[%d].centroid", i), centroid, region.Centroid)
		}
		bbox := core.BBox{X0: a.x0, Y0: a.y0, X1: a.x1, Y1: a.y1}
		if bbox != region.BBox {
			return nil, core.MetaError("sample.load_reference", fmt.Sprintf("regions[%d].bbox", i), bbox, region.BBox)
		}
	}
	return labels, nil
}

func closeFloat(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

func pngHeader(raw []byte) (int, int, int, error) {
	if err := validatePNGHeader(raw); err != nil {
		return 0, 0, 0, err
	}
	if len(raw) < 26 {
		return 0, 0, 0, &core.Error{Code: core.CodeDecode, Op: "sample.png_header", Message: "truncated PNG header"}
	}
	width := int(binary.BigEndian.Uint32(raw[16:20]))
	height := int(binary.BigEndian.Uint32(raw[20:24]))
	colorType := raw[25]
	channels := 0
	switch colorType {
	case 0:
		channels = 1
	case 2:
		channels = 3
	case 3:
		channels = 1
	case 4:
		channels = 2
	case 6:
		channels = 4
	default:
		return 0, 0, 0, &core.Error{Code: core.CodeDecode, Op: "sample.png_header", Message: fmt.Sprintf("unknown PNG color type %d", colorType)}
	}
	return width, height, channels, nil
}

func validatePNGHeader(raw []byte) error {
	signature := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	if len(raw) < 33 || !bytes.Equal(raw[:8], signature) || string(raw[12:16]) != "IHDR" {
		return &core.Error{Code: core.CodeDecode, Op: "sample.png_header", Message: "invalid or truncated PNG"}
	}
	return nil
}

func imageToGray8(c interface {
	RGBA() (uint32, uint32, uint32, uint32)
}) uint8 {
	r, _, _, _ := c.RGBA()
	// Reference masks are encoded as grayscale, so all channels equal the label.
	return uint8(r >> 8)
}
