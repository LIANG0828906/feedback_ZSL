package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"spectralrender"
)

func main() {
	samples := flag.String("samples", "samples", "generated sample directory")
	checkpointDir := flag.String("checkpoints", "checkpoints", "fixed persistent checkpoint directory")
	outDir := flag.String("out", "out", "output directory")
	parallel := flag.Int("parallel", 4, "maximum concurrent tiles")
	tileSize := flag.Int("tile-size", 16, "tile edge in pixels")
	sampleCount := flag.Int("samples-count", 35, "wavelength sample count")
	integration := flag.String("integration", "trapezoid", "rectangle|trapezoid|simpson")
	bounces := flag.Int("bounces", 2, "indirect bounce approximation count")
	exposure := flag.Float64("exposure", 1.0, "tone-map exposure")
	curve := flag.String("curve", "aces", "linear|reinhard|aces")
	gamma := flag.Float64("gamma", 2.2, "display gamma")
	budget := flag.Int64("memory-budget", 32<<20, "tile buffer budget in bytes")
	version := flag.Uint64("version", 1, "parameter snapshot version")
	flag.Parse()

	bundle, err := spectralrender.LoadSampleBundle(*samples)
	if err != nil {
		fail(err)
	}
	scene, sceneHash, truth := bundle.Scene, bundle.SceneHash, bundle.Truth
	params := spectralrender.DefaultRenderParams()
	params.Version = *version
	params.SampleCount = *sampleCount
	params.Integration = spectralrender.IntegrationMethod(*integration)
	params.Bounces = *bounces
	params.Exposure = *exposure
	params.ToneCurve = spectralrender.ToneCurve(*curve)
	params.Gamma = *gamma
	params.TileSize = *tileSize
	params.MaxParallel = *parallel
	params.MemoryBudgetBytes = *budget
	if verr := params.Validate(); verr != nil {
		fail(verr)
	}
	store, serr := spectralrender.OpenCheckpointStore(*checkpointDir)
	if serr != nil {
		fail(serr)
	}
	frame, report, err := spectralrender.Render(scene, sceneHash, params, spectralrender.RenderOptions{Store: store})
	if err != nil {
		if report != nil {
			printJSON(report)
		}
		fail(err)
	}
	truthCheck, truthErr := spectralrender.CheckTruthFrame(frame, truth)
	if truthErr != nil {
		fail(truthErr)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fail(err)
	}
	if err := writePNG(filepath.Join(*outDir, "frame.png"), frame); err != nil {
		fail(err)
	}
	combined := struct {
		Render *spectralrender.RenderReport     `json:"render"`
		Truth  *spectralrender.TruthCheckReport `json:"truth"`
	}{Render: report, Truth: truthCheck}
	if err := writeJSON(filepath.Join(*outDir, "report.json"), combined); err != nil {
		fail(err)
	}
	fmt.Printf("rendered %dx%d version=%d encoded_sha256=%s tiles=%d resumed=%d rendered=%d\n",
		frame.Width, frame.Height, frame.ParamVersion, frame.EncodedSHA256,
		report.TileCount, len(report.ResumedTiles), len(report.RenderedTiles))
	fmt.Printf("truth: pass=%v max_linear_error=%.9g max_chroma_error=%.9g\n",
		truthCheck.Pass, truthCheck.MaxLinearRGB, truthCheck.MaxChromaticity)
	if report.Invalidation != nil {
		fmt.Printf("checkpoint invalidated: %s\n", report.Invalidation.Reason)
	}
	if !truthCheck.Pass {
		os.Exit(2)
	}
}

func writePNG(path string, frame *spectralrender.Frame) error {
	img := image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			i := (y*frame.Width + x) * 3
			img.SetRGBA(x, y, color.RGBA{
				R: frame.EncodedRGB[i],
				G: frame.EncodedRGB[i+1],
				B: frame.EncodedRGB[i+2],
				A: 255,
			})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func fail(err error) {
	e := spectralrender.AsError(err)
	b, _ := json.MarshalIndent(e, "", "  ")
	if e != nil {
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
