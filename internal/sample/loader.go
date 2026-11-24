package sample

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"iconbench/internal/model"
)

func LoadManifest(path string) (*model.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, Message: "manifest is not valid JSON: " + err.Error()}
	}
	return &m, nil
}

func LoadImage(dataDir string, rec model.ImageManifest) (image.Image, []byte, error) {
	path := filepath.Join(dataDir, filepath.FromSlash(rec.File))
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, &model.Error{Code: model.CodeNotFound, Message: "image not found: " + path, Field: "file", Actual: path}
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, nil, &model.Error{Code: model.CodeDecode, Message: "PNG header decode failed: " + err.Error()}
	}
	if cfg.Width != rec.Width {
		return nil, nil, &model.Error{
			Code: model.CodeMeta, Field: "width",
			Message:  "width mismatch between manifest and image file",
			Expected: map[string]any{"side": "manifest", "value": rec.Width},
			Actual:   map[string]any{"side": "file", "value": cfg.Width},
		}
	}
	if cfg.Height != rec.Height {
		return nil, nil, &model.Error{
			Code: model.CodeMeta, Field: "height",
			Message:  "height mismatch between manifest and image file",
			Expected: map[string]any{"side": "manifest", "value": rec.Height},
			Actual:   map[string]any{"side": "file", "value": cfg.Height},
		}
	}
	channels := colorModelChannels(cfg.ColorModel)
	if channels != rec.Channels {
		return nil, nil, &model.Error{
			Code: model.CodeMeta, Field: "channels",
			Message:  "channel count mismatch between manifest and image file",
			Expected: map[string]any{"side": "manifest", "value": rec.Channels},
			Actual:   map[string]any{"side": "file", "value": channels},
		}
	}
	sum := sha256.Sum256(b)
	if rec.SHA256 != "" && hex.EncodeToString(sum[:]) != rec.SHA256 {
		return nil, nil, &model.Error{Code: model.CodeDecode, Field: "sha256", Message: "image checksum does not match manifest"}
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, nil, &model.Error{Code: model.CodeDecode, Message: "PNG pixel decode failed: " + err.Error()}
	}
	return img, b, nil
}

func DecodeReferenceMask(rec model.ImageManifest) ([]byte, int, int, error) {
	pngBytes, err := base64.StdEncoding.DecodeString(rec.ReferenceMaskPNGBase64)
	if err != nil {
		return nil, 0, 0, &model.Error{Code: model.CodeDecode, Field: "reference_mask", Message: "reference mask base64 decode failed: " + err.Error()}
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, 0, 0, &model.Error{Code: model.CodeDecode, Field: "reference_mask", Message: "reference mask PNG decode failed: " + err.Error()}
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	mask := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, _, _, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if r > 0 {
				mask[y*w+x] = 1
			}
		}
	}
	sum := sha256.Sum256(mask)
	if rec.MaskSHA256 != "" && hex.EncodeToString(sum[:]) != rec.MaskSHA256 {
		return nil, 0, 0, &model.Error{Code: model.CodeDecode, Field: "mask_sha256", Message: "reference mask hash does not match manifest"}
	}
	return mask, w, h, nil
}

func colorModelChannels(m color.Model) int {
	switch m {
	case color.GrayModel, color.Gray16Model:
		return 1
	case color.RGBAModel, color.NRGBAModel, color.RGBA64Model, color.NRGBA64Model:
		return 4
	default:
		return 3
	}
}
