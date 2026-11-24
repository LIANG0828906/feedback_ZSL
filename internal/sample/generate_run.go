package sample

import (
	"encoding/json"
	"image"
	"image/color"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"maskpipe/internal/model"
	"maskpipe/internal/vision"
)

func Generate(root string, seed int64) (model.SampleManifest, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return model.SampleManifest{}, err
	}
	manifest := model.SampleManifest{
		SchemaVersion:    "1.0",
		Generator:        "maskpipe-local-deterministic-generator",
		Seed:             seed,
		AlgorithmVersion: vision.AlgorithmVersion,
	}

	for idx, spec := range defaultScenes() {
		rng := rand.New(rand.NewSource(seed + int64(idx)*9173 + 17))
		img := image.NewNRGBA(image.Rect(0, 0, spec.w, spec.h))
		truth := image.NewGray(image.Rect(0, 0, spec.w, spec.h))

		for y := 0; y < spec.h; y++ {
			t := float64(y) / float64(maxInt(1, spec.h-1))
			for x := 0; x < spec.w; x++ {
				base := [3]float64{}
				for c := 0; c < 3; c++ {
					base[c] = float64(spec.top[c])*(1-t) + float64(spec.bottom[c])*t
					base[c] += math.Sin(float64(x)*0.045+float64(idx)) * 5
				}
				label := 0
				rgb := base
				for si, shape := range spec.shapes {
					if insideShape(shape, x, y) {
						label = si + 1
						rgb = [3]float64{float64(shape.rgb[0]), float64(shape.rgb[1]), float64(shape.rgb[2])}
						break
					}
				}
				noise := rng.Intn(2*spec.noiseAmp+1) - spec.noiseAmp
				if label > 0 && rng.Intn(97) == 0 {
					noise += 38
				}
				if label == 0 && spec.id == "embedded-noise" && rng.Intn(180) == 0 {
					noise += rng.Intn(70) - 20
				}
				i := img.PixOffset(x, y)
				img.Pix[i] = clampByte(int(rgb[0]) + noise)
				img.Pix[i+1] = clampByte(int(rgb[1]) + noise)
				img.Pix[i+2] = clampByte(int(rgb[2]) + noise)
				img.Pix[i+3] = 255
				if label > 0 {
					truth.SetGray(x, y, color.Gray{Y: 255})
				}
			}
		}

		imageName := spec.id + ".png"
		truthName := spec.id + ".truth.png"
		if err := writePNG(filepath.Join(root, imageName), img); err != nil {
			return model.SampleManifest{}, err
		}
		if err := writePNG(filepath.Join(root, truthName), truth); err != nil {
			return model.SampleManifest{}, err
		}

		truthBytes := make([]byte, spec.w*spec.h)
		for i := range truthBytes {
			if truth.Pix[i] == 255 {
				truthBytes[i] = 1
			}
		}
		truthRegions := vision.AnalyzeMask(truthBytes, spec.w, spec.h, 1)
		seeds := make([]model.IntPoint, 0, len(truthRegions))
		for i := range truthRegions {
			if i < len(spec.shapes) {
				truthRegions[i].Label = spec.shapes[i].label
			}
			seeds = append(seeds, model.IntPoint{
				X: int(math.Round(truthRegions[i].Centroid.X - 0.5)),
				Y: int(math.Round(truthRegions[i].Centroid.Y - 0.5)),
			})
		}

		cols := (spec.w + spec.tw - 1) / spec.tw
		rows := (spec.h + spec.th - 1) / spec.th
		params := spec.params
		params.Seeds = seeds
		params = vision.WithVersion(params)

		scene := model.SceneManifest{
			ID: spec.id, SceneType: spec.sceneType,
			ImagePath: imageName, TruthMaskPath: truthName,
			Width: spec.w, Height: spec.h, Channels: 4,
			Tile: model.Tile{Width: spec.tw, Height: spec.th},
			Cols: cols, Rows: rows, ChunkCount: cols * rows,
			Declared: model.DeclaredLimits{
				Tile: model.Tile{Width: spec.tw, Height: spec.th},
				Budget: model.Budget{
					WorkerCount:            3,
					PixelBufferPerThread:   49152,
					PendingTaskQueueBudget: 2,
				},
				PreviewLatencyMS: 250,
				Quality: model.QualityTolerance{
					CentroidPixels: 2.0, AreaRelative: 0.08, BBoxPixels: 2.0,
				},
				Divergence: model.DivergenceTolerance{
					CentroidPixels: 2.5, AreaRelative: 0.1, PixelMismatch: 0.04,
				},
			},
			Seeds: seeds, TruthRegionCount: len(truthRegions),
			TruthRegions:     truthRegions,
			TruthFingerprint: fingerprintMask(truthBytes),
			Parameters:       params,
		}
		manifest.Samples = append(manifest.Samples, scene)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return model.SampleManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o644); err != nil {
		return model.SampleManifest{}, err
	}
	return manifest, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
