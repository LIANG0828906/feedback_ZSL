package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"

	"maskpipe/internal/model"
	"maskpipe/internal/vision"
)

type LoadedScene struct {
	Root       string
	Set        model.SampleManifest
	Scene      model.SceneManifest
	Image      *image.NRGBA
	TruthMask  []byte
	TruthImage *image.Gray
}

func LoadManifest(path string) (model.SampleManifest, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.SampleManifest{}, "", model.NewError(model.CodeMeta, "manifest", "cannot read manifest: %v", err)
	}
	var manifest model.SampleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return model.SampleManifest{}, "", model.NewError(model.CodeMeta, "manifest_json", "invalid JSON: %v", err)
	}
	if manifest.SchemaVersion == "" {
		return model.SampleManifest{}, "", model.NewError(model.CodeMeta, "schema_version", "field is missing")
	}
	if manifest.Seed == 0 {
		return model.SampleManifest{}, "", model.NewError(model.CodeMeta, "seed", "field is missing or zero")
	}
	if len(manifest.Samples) == 0 {
		return model.SampleManifest{}, "", model.NewError(model.CodeMeta, "samples", "field is missing or empty")
	}
	root := filepath.Dir(path)
	return manifest, root, nil
}

func LoadScene(root, sceneID string) (*LoadedScene, error) {
	manifestPath := filepath.Join(root, "manifest.json")
	set, manifestRoot, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if root == "" {
		root = manifestRoot
	}
	var scene *model.SceneManifest
	for i := range set.Samples {
		if set.Samples[i].ID == sceneID || sceneID == "" {
			scene = &set.Samples[i]
			break
		}
	}
	if scene == nil {
		return nil, model.NewError(model.CodeMeta, "id", "scene %q not found", sceneID)
	}
	if err := validateSceneFields(scene); err != nil {
		return nil, err
	}

	imagePath := filepath.Join(root, scene.ImagePath)
	rawImg, actualChannels, err := decodePNG(imagePath)
	if err != nil {
		return nil, model.NewError(model.CodeDecode, "image_path", "%s: %v", scene.ImagePath, err)
	}
	if actualChannels != scene.Channels {
		return nil, model.NewError(model.CodeMeta, "channels", "manifest says %d but PNG has %d channels", scene.Channels, actualChannels)
	}
	bounds := rawImg.Bounds()
	if bounds.Dx() != scene.Width {
		return nil, model.NewError(model.CodeMeta, "width", "manifest=%d actual=%d", scene.Width, bounds.Dx())
	}
	if bounds.Dy() != scene.Height {
		return nil, model.NewError(model.CodeMeta, "height", "manifest=%d actual=%d", scene.Height, bounds.Dy())
	}
	img := image.NewNRGBA(bounds)
	draw.Draw(img, bounds, rawImg, bounds.Min, draw.Src)

	truthPath := filepath.Join(root, scene.TruthMaskPath)
	truthImg, _, err := decodePNG(truthPath)
	if err != nil {
		return nil, model.NewError(model.CodeDecode, "truth_mask_path", "%s: %v", scene.TruthMaskPath, err)
	}
	if truthImg.Bounds().Dx() != scene.Width {
		return nil, model.NewError(model.CodeMeta, "truth_mask_path.width", "manifest width=%d truth width=%d", scene.Width, truthImg.Bounds().Dx())
	}
	if truthImg.Bounds().Dy() != scene.Height {
		return nil, model.NewError(model.CodeMeta, "truth_mask_path.height", "manifest height=%d truth height=%d", scene.Height, truthImg.Bounds().Dy())
	}
	truthMask := grayMaskFromImage(truthImg, scene.Width, scene.Height)
	sum := sha256.Sum256(truthMask)
	if scene.TruthFingerprint == "" {
		return nil, model.NewError(model.CodeMeta, "truth_fingerprint", "field is missing")
	}
	if hex.EncodeToString(sum[:]) != scene.TruthFingerprint {
		return nil, model.NewError(model.CodeMeta, "truth_fingerprint", "stored fingerprint does not match decoded reference mask")
	}
	regions := vision.AnalyzeMask(truthMask, scene.Width, scene.Height, 1)
	if scene.TruthRegionCount == 0 {
		return nil, model.NewError(model.CodeMeta, "truth_region_count", "field is missing or zero")
	}
	if scene.TruthRegionCount != len(regions) {
		return nil, model.NewError(model.CodeMeta, "truth_region_count", "manifest=%d decoded=%d", scene.TruthRegionCount, len(regions))
	}
	if len(scene.TruthRegions) != scene.TruthRegionCount {
		return nil, model.NewError(model.CodeMeta, "truth_regions", "length=%d truth_region_count=%d", len(scene.TruthRegions), scene.TruthRegionCount)
	}
	if err := vision.ValidateParameters(scene.Parameters); err != nil {
		return nil, err
	}

	gray := image.NewGray(image.Rect(0, 0, scene.Width, scene.Height))
	for i, v := range truthMask {
		if v != 0 {
			gray.Pix[i] = 255
		}
	}
	return &LoadedScene{Root: root, Set: set, Scene: *scene, Image: img, TruthMask: truthMask, TruthImage: gray}, nil
}

func validateSceneFields(s *model.SceneManifest) error {
	required := []struct {
		name string
		ok   bool
	}{
		{"id", s.ID != ""},
		{"image_path", s.ImagePath != ""},
		{"truth_mask_path", s.TruthMaskPath != ""},
		{"width", s.Width > 0},
		{"height", s.Height > 0},
		{"channels", s.Channels > 0},
		{"tile.width", s.Tile.Width > 0},
		{"tile.height", s.Tile.Height > 0},
		{"declared.preview_latency_ms", s.Declared.PreviewLatencyMS > 0},
		{"declared.budget.worker_count", s.Declared.Budget.WorkerCount > 0},
		{"declared.budget.pixel_buffer_per_thread", s.Declared.Budget.PixelBufferPerThread > 0},
	}
	for _, field := range required {
		if !field.ok {
			return model.NewError(model.CodeMeta, field.name, "field is missing or invalid")
		}
	}
	cols := (s.Width + s.Tile.Width - 1) / s.Tile.Width
	rows := (s.Height + s.Tile.Height - 1) / s.Tile.Height
	if s.Cols != cols {
		return model.NewError(model.CodeMeta, "cols", "manifest=%d required=%d for tile coverage", s.Cols, cols)
	}
	if s.Rows != rows {
		return model.NewError(model.CodeMeta, "rows", "manifest=%d required=%d for tile coverage", s.Rows, rows)
	}
	if s.ChunkCount != cols*rows {
		return model.NewError(model.CodeMeta, "chunk_count", "manifest=%d required=%d", s.ChunkCount, cols*rows)
	}
	if s.Declared.Tile.Width != s.Tile.Width || s.Declared.Tile.Height != s.Tile.Height {
		return model.NewError(model.CodeMeta, "declared.tile", "declared tile %dx%d differs from tile %dx%d",
			s.Declared.Tile.Width, s.Declared.Tile.Height, s.Tile.Width, s.Tile.Height)
	}
	return nil
}

func decodePNG(path string) (image.Image, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return nil, 0, err
	}
	channels := 4
	switch cfg.ColorModel {
	case color.GrayModel, color.Gray16Model:
		channels = 1
	default:
		channels = 4
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, 0, err
	}
	img, err := png.Decode(f)
	if err != nil {
		return nil, 0, err
	}
	return img, channels, nil
}

func grayMaskFromImage(img image.Image, w, h int) []byte {
	out := make([]byte, w*h)
	b := img.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257.0
			if a > 0 && lum > 100 {
				out[y*w+x] = 1
			}
		}
	}
	return out
}
