package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"spectralrender"
)

func main() {
	samples := flag.String("samples", "samples", "generated sample directory")
	checkpointDir := flag.String("checkpoints", "checkpoints", "fixed persistent checkpoint directory")
	samplingConstant := flag.Float64("sampling-constant", 2.0, "error-bound sampling coefficient")
	quantConstant := flag.Float64("quantization-constant", 8.0, "error-bound quantization coefficient")
	peakScale := flag.Float64("peak-scale", 2.0, "peak-radiance bound multiplier")
	chromaConstant := flag.Float64("chroma-constant", 4.0, "chromaticity bound multiplier")
	flag.Parse()

	bundle, err := spectralrender.LoadSampleBundle(*samples)
	if err != nil {
		fail(nil, err)
	}
	scene, sceneHash, truth := bundle.Scene, bundle.SceneHash, bundle.Truth
	model := spectralrender.BoundModel{
		SamplingConstant:     *samplingConstant,
		QuantizationConstant: *quantConstant,
		PeakScale:            *peakScale,
		ChromaticityConstant: *chromaConstant,
	}
	report, verr := spectralrender.VerifyCheckpoint(scene, sceneHash, truth, *checkpointDir, model)
	if report != nil {
		printJSON(report)
	}
	if verr != nil {
		fail(report, verr)
	}
	fmt.Fprintf(os.Stderr, "verify OK: %d tiles, bound linear=%.9g\n", report.TilesChecked, report.BoundCheck.Bound.LinearRGBAbs)
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func fail(report any, err error) {
	if e := spectralrender.AsError(err); e != nil {
		b, _ := json.MarshalIndent(e, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
