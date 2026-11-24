package loader

import (
	"bytes"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"unicode/utf8"

	"ocrworkbench/internal/core"
)

type LoadedImage struct {
	Image    image.Image
	Bytes    []byte
	Width    int
	Height   int
	Channels int
	Hash     string
	Truth    core.ImageTruth
}

func Load(datasetDir string, truth core.ImageTruth) (LoadedImage, error) {
	path := filepath.Join(datasetDir, truth.File)
	b, err := os.ReadFile(path)
	if err != nil {
		return LoadedImage{}, core.NewError(core.CodeDecode, fmt.Sprintf("read %s: %v", truth.File, err))
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return LoadedImage{}, core.NewError(core.CodeDecode, fmt.Sprintf("decode %s: %v", truth.File, err))
	}
	bounds := img.Bounds()
	loaded := LoadedImage{
		Image:    img,
		Bytes:    b,
		Width:    bounds.Dx(),
		Height:   bounds.Dy(),
		Channels: detectChannels(b, img),
		Hash:     core.HashBytes(b),
		Truth:    truth,
	}
	if err := validateMeta(loaded); err != nil {
		return LoadedImage{}, err
	}
	return loaded, nil
}

func detectChannels(encoded []byte, img image.Image) int {
	if len(encoded) >= 26 &&
		encoded[0] == 0x89 && encoded[1] == 'P' && encoded[2] == 'N' && encoded[3] == 'G' &&
		encoded[12] == 'I' && encoded[13] == 'H' && encoded[14] == 'D' && encoded[15] == 'R' {
		switch encoded[25] {
		case 0:
			return 1
		case 2:
			return 3
		case 3:
			return 3
		case 4:
			return 2
		case 6:
			return 4
		}
	}
	return ChannelCount(img)
}

func ChannelCount(img image.Image) int {
	switch img.(type) {
	case *image.Gray, *image.Gray16:
		return 1
	case *image.NRGBA, *image.RGBA, *image.NRGBA64, *image.RGBA64:
		return 4
	case *image.YCbCr:
		return 3
	case *image.CMYK:
		return 4
	case *image.Paletted:
		return 3
	default:
		return 4
	}
}

func validateMeta(loaded LoadedImage) error {
	truth := loaded.Truth
	if loaded.Width != truth.Width {
		return fieldMeta("width", fmt.Sprintf("decoded width %d does not match manifest width %d", loaded.Width, truth.Width))
	}
	if loaded.Height != truth.Height {
		return fieldMeta("height", fmt.Sprintf("decoded height %d does not match manifest height %d", loaded.Height, truth.Height))
	}
	if loaded.Channels != truth.Channels {
		return fieldMeta("channels", fmt.Sprintf("decoded channels %d does not match manifest channels %d", loaded.Channels, truth.Channels))
	}
	if truth.DeclaredRegionCount != len(truth.Regions) {
		return fieldMeta("declared_region_count", fmt.Sprintf("declared region count %d does not match %d region records", truth.DeclaredRegionCount, len(truth.Regions)))
	}
	if truth.SHA256 != "" && loaded.Hash != truth.SHA256 {
		return fieldMeta("sha256", fmt.Sprintf("content hash %s does not match manifest hash %s", loaded.Hash, truth.SHA256))
	}
	for i, region := range truth.Regions {
		if !core.ValidBox(region.Box) {
			return fieldMeta(fmt.Sprintf("regions[%d].box", i), "box has non-finite coordinates, zero area, or a degenerate edge")
		}
		if region.CharCount != utf8.RuneCountInString(region.Text) {
			return fieldMeta(fmt.Sprintf("regions[%d].char_count", i), fmt.Sprintf("declared %d but text %q has %d runes", region.CharCount, region.Text, utf8.RuneCountInString(region.Text)))
		}
		if region.AvgStrokeWidth <= 0 {
			return fieldMeta(fmt.Sprintf("regions[%d].avg_stroke_width", i), "average stroke width must be positive")
		}
	}
	return nil
}

func fieldMeta(field, message string) error {
	return core.FieldError(core.CodeMeta, field, "", message)
}
