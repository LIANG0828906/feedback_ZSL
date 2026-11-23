//go:build fyne

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"glyphbrush/internal/loader"
	"glyphbrush/internal/samplegen"
	"glyphbrush/internal/ui"
)

func main() {
	samples := flag.String("samples", "./samples", "directory containing glyphs.json and truth.json")
	checkpoints := flag.String("checkpoints", "./artifacts/ui/checkpoints", "checkpoint directory")
	exportRoot := flag.String("export", "./artifacts/ui/export", "PNG/JSON export directory")
	maxParallel := flag.Int("max-parallel", 4, "maximum segment render parallelism")
	polygonLimit := flag.Int("polygon-buffer", 200000, "maximum polygon points per segment")
	seed := flag.Int64("seed", samplegen.DefaultSeed, "seed used if samples must be generated")
	flag.Parse()

	if _, err := os.Stat(filepath.Join(*samples, "glyphs.json")); os.IsNotExist(err) {
		if _, _, err := samplegen.Write(*samples, *seed); err != nil {
			fail(err)
		}
	}
	bundle, err := loader.Load(*samples)
	if err != nil {
		fail(err)
	}
	ui.Run(bundle, ui.Options{
		CheckpointDir: *checkpoints,
		ExportRoot:    *exportRoot,
		MaxParallel:   *maxParallel,
		PolygonLimit:  *polygonLimit,
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
