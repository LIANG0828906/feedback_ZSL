package imaging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ocrworkbench/internal/domain"
)

func ReadManifest(dir string) (domain.Manifest, *domain.AppError) {
	var manifest domain.Manifest
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, domain.DecodeError("manifest is missing or unreadable", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, domain.DecodeError("manifest is not valid JSON", err)
	}
	if manifest.SchemaVersion != domain.ManifestSchema {
		return manifest, domain.MetaError("manifest.schema_version", domain.ManifestSchema, manifest.SchemaVersion)
	}
	return manifest, nil
}

func FindSample(manifest domain.Manifest, id string) (domain.SampleMeta, bool) {
	for _, sample := range manifest.Samples {
		if sample.ID == id {
			return sample, true
		}
	}
	return domain.SampleMeta{}, false
}

func LoadSample(dir, id string) (*domain.LoadedSample, *domain.AppError) {
	manifest, appErr := ReadManifest(dir)
	if appErr != nil {
		return nil, appErr
	}
	meta, ok := FindSample(manifest, id)
	if !ok {
		e := domain.DecodeError("sample is not present in the manifest", nil)
		e.Field = "sample_id"
		e.Actual = id
		return nil, e
	}
	imagePath := filepath.Join(dir, filepath.FromSlash(meta.Image))
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, domain.DecodeError("sample image is missing or unreadable", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return nil, domain.DecodeError("sample image cannot be decoded", err)
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	channels := channelsOf(decoded)
	if width != meta.Width {
		return nil, domain.MetaError("width", meta.Width, width)
	}
	if height != meta.Height {
		return nil, domain.MetaError("height", meta.Height, height)
	}
	if channels != meta.Channels {
		return nil, domain.MetaError("channels", meta.Channels, channels)
	}
	contentHash := HashImage(decoded)
	if meta.ImageHash != "" && meta.ImageHash != contentHash {
		return nil, domain.MetaError("image_hash", meta.ImageHash, contentHash)
	}

	truthPath := filepath.Join(dir, filepath.FromSlash(meta.Truth))
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		return nil, domain.DecodeError("truth file is missing or unreadable", err)
	}
	var truth domain.TruthFile
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		return nil, domain.DecodeError("truth file is not valid JSON", err)
	}
	if truth.SchemaVersion != domain.TruthSchema {
		return nil, domain.MetaError("truth.schema_version", domain.TruthSchema, truth.SchemaVersion)
	}
	if truth.ImageID != meta.ID {
		return nil, domain.MetaError("truth.image_id", meta.ID, truth.ImageID)
	}
	if truth.Width != width {
		return nil, domain.MetaError("truth.width", width, truth.Width)
	}
	if truth.Height != height {
		return nil, domain.MetaError("truth.height", height, truth.Height)
	}
	if truth.Channels != channels {
		return nil, domain.MetaError("truth.channels", channels, truth.Channels)
	}
	if truth.RegionCount != len(truth.Regions) {
		return nil, domain.MetaError("truth.region_count", len(truth.Regions), truth.RegionCount)
	}
	if meta.RegionCount != truth.RegionCount {
		return nil, domain.MetaError("region_count", truth.RegionCount, meta.RegionCount)
	}
	if meta.RegionCount != len(truth.Regions) {
		return nil, domain.MetaError("region_count", len(truth.Regions), meta.RegionCount)
	}
	for i, region := range truth.Regions {
		if region.CharCount != len([]rune(region.Text)) {
			return nil, domain.MetaError(
				fmt.Sprintf("truth.regions[%d].char_count", i),
				len([]rune(region.Text)),
				region.CharCount,
			)
		}
	}

	gray := ToGray(decoded)
	normalized := Normalize(gray)
	return &domain.LoadedSample{
		Meta:        meta,
		Manifest:    manifest,
		Truth:       truth,
		ImageBytes:  imageBytes,
		Image:       decoded,
		Gray:        normalized,
		Width:       width,
		Height:      height,
		Channels:    channels,
		ContentHash: contentHash,
		FilePath:    imagePath,
		TruthPath:   truthPath,
	}, nil
}

func channelsOf(img image.Image) int {
	switch img.(type) {
	case *image.Gray, *image.Gray16:
		return 1
	case *image.RGBA, *image.NRGBA, *image.RGBA64, *image.NRGBA64:
		return 4
	case *image.Paletted:
		return 3
	default:
		return 4
	}
}

func ToGray(src image.Image) *image.Gray {
	b := src.Bounds()
	dst := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			y8 := uint8((299*uint32(r/257) + 587*uint32(g/257) + 114*uint32(bl/257)) / 1000)
			dst.SetGray(x, y, color.Gray{Y: y8})
		}
	}
	return dst
}

func Normalize(src *image.Gray) *image.Gray {
	b := src.Bounds()
	if b.Empty() {
		return image.NewGray(image.Rect(0, 0, 0, 0))
	}
	minV, maxV := uint8(255), uint8(0)
	for _, v := range src.Pix {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	dst := image.NewGray(b)
	if maxV == minV {
		copy(dst.Pix, src.Pix)
		return dst
	}
	scale := 255.0 / float64(maxV-minV)
	for i, v := range src.Pix {
		dst.Pix[i] = uint8(domain.Clamp01(float64(v-minV)*scale/255.0) * 255.0)
	}
	return dst
}

type BinaryView struct {
	Width     int
	Height    int
	Mask      []bool
	Threshold int
	InkIsDark bool
}

func Binarize(src *image.Gray) BinaryView {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	threshold := Otsu(src.Pix)
	below, above := 0, 0
	for _, v := range src.Pix {
		if int(v) <= threshold {
			below++
		} else {
			above++
		}
	}
	inkIsDark := below <= above
	mask := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := int(src.Pix[y*w+x])
			if inkIsDark {
				mask[y*w+x] = v <= threshold
			} else {
				mask[y*w+x] = v > threshold
			}
		}
	}
	return BinaryView{Width: w, Height: h, Mask: mask, Threshold: threshold, InkIsDark: inkIsDark}
}

func Otsu(pixels []uint8) int {
	var hist [256]int
	for _, v := range pixels {
		hist[v]++
	}
	total := len(pixels)
	if total == 0 {
		return 128
	}
	var sum float64
	for i, count := range hist {
		sum += float64(i * count)
	}
	var sumB, weightB float64
	bestVariance, bestThreshold := -1.0, 128
	for t := 0; t < 255; t++ {
		weightB += float64(hist[t])
		if weightB == 0 {
			continue
		}
		weightF := float64(total) - weightB
		if weightF == 0 {
			break
		}
		sumB += float64(t * hist[t])
		meanB := sumB / weightB
		meanF := (sum - sumB) / weightF
		variance := weightB * weightF * (meanB - meanF) * (meanB - meanF)
		if variance > bestVariance {
			bestVariance = variance
			bestThreshold = t
		}
	}
	return bestThreshold
}

func EstimateAverageStrokeWidth(src *image.Gray) float64 {
	view := Binarize(src)
	if view.Width == 0 || view.Height == 0 {
		return 1
	}
	runs := make([]int, 0, 512)
	for y := 0; y < view.Height; y++ {
		run := 0
		for x := 0; x < view.Width; x++ {
			if view.Mask[y*view.Width+x] {
				run++
			} else if run > 0 {
				runs = append(runs, run)
				run = 0
			}
		}
		if run > 0 {
			runs = append(runs, run)
		}
	}
	for x := 0; x < view.Width; x++ {
		run := 0
		for y := 0; y < view.Height; y++ {
			if view.Mask[y*view.Width+x] {
				run++
			} else if run > 0 {
				runs = append(runs, run)
				run = 0
			}
		}
		if run > 0 {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return 1
	}
	sort.Ints(runs)
	index := int(float64(len(runs)-1) * 0.35)
	estimate := float64(runs[index])
	if estimate < 1 {
		estimate = 1
	}
	return domain.Round4(estimate)
}

func HashImage(img image.Image) string {
	b := img.Bounds()
	h := sha256.New()
	var header [16]byte
	w, ht := b.Dx(), b.Dy()
	header[0] = byte(w >> 24)
	header[1] = byte(w >> 16)
	header[2] = byte(w >> 8)
	header[3] = byte(w)
	header[4] = byte(ht >> 24)
	header[5] = byte(ht >> 16)
	header[6] = byte(ht >> 8)
	header[7] = byte(ht)
	h.Write(header[:])
	buf := make([]byte, 0, 4096)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			buf = append(buf, byte(r>>8), byte(g>>8), byte(bl>>8), byte(a>>8))
			if len(buf) >= 4096 {
				h.Write(buf)
				buf = buf[:0]
			}
		}
	}
	if len(buf) > 0 {
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func SampleFileExists(dir, id string) bool {
	manifest, err := ReadManifest(dir)
	if err != nil {
		return false
	}
	meta, ok := FindSample(manifest, id)
	if !ok {
		return false
	}
	for _, part := range []string{meta.Image, meta.Truth} {
		info, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(part)))
		if statErr != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func SafeID(id string) string {
	id = strings.TrimSpace(id)
	replacer := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_")
	return replacer.Replace(id)
}
