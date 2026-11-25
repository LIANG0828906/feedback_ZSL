package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ms-workbench/internal/engine"
	"ms-workbench/internal/pipeline"
	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/server"
	"ms-workbench/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "samples":
		err = runSamples(os.Args[2:])
	case "run":
		err = runOnce(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Print(`SPECTRA multispectral / HDR workbench

Commands:
  serve    Start the interactive browser application
  demo     Run deterministic schema, truth, and three-path acceptance checks
  samples  Write v1/v2/v3 scene files and generated truth
  run      Run one scene without the HTTP application

Examples:
  go run ./cmd/workbench serve -addr 127.0.0.1:8080 -db-dir artifacts
  go run ./cmd/workbench demo -out artifacts/demo-report.json
  go run ./cmd/workbench samples -dir artifacts/samples
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbDir := fs.String("db-dir", "artifacts", "SQLite checkpoint directory")
	seed := fs.Uint64("seed", 20261125, "deterministic sample seed")
	workers := fs.Int("workers", 4, "maximum worker concurrency")
	pixelBuffer := fs.Int("pixel-buffer", 1024, "per-worker pixel buffer limit")
	chunk := fs.Int("chunk", 16, "square chunk size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := engine.DefaultConfig()
	cfg.MaxWorkers = *workers
	cfg.MaxPixelsPerWorker = *pixelBuffer
	cfg.ChunkWidth, cfg.ChunkHeight = *chunk, *chunk
	app, err := server.New(server.Options{DBDir: *dbDir, Seed: *seed, Config: cfg})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	fmt.Printf("SPECTRA workbench listening on http://%s\n", *addr)
	fmt.Printf("seed=%d chunk=%dx%d workers=%d pixelBuffer=%d db=%s\n", *seed, *chunk, *chunk, *workers, *pixelBuffer, *dbDir)
	err = httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

type schemaResult struct {
	Version          int                                `json:"version"`
	SourceVersionOK  bool                               `json:"sourceVersionPreserved"`
	ByteStable       bool                               `json:"byteStableExport"`
	BandCodesEqual   bool                               `json:"bandCodesEqual"`
	SettingsEqual    bool                               `json:"settingsEqual"`
	Width            int                                `json:"width"`
	Height           int                                `json:"height"`
	RegressionOffset map[string]scene.RegistrationTruth `json:"registration"`
}

type demoReport struct {
	GeneratorVersion string                     `json:"generatorVersion"`
	Seed             uint64                     `json:"seed"`
	SceneID          string                     `json:"sceneID"`
	Schemas          []schemaResult             `json:"schemas"`
	Interoperable    bool                       `json:"interoperable"`
	Verification     *engine.VerificationReport `json:"verification"`
	Passed           bool                       `json:"passed"`
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Uint64("seed", 20261125, "deterministic sample seed")
	out := fs.String("out", "artifacts/demo-report.json", "JSON report output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	packages, err := samples.DefaultPackages(*seed)
	if err != nil {
		return err
	}
	pkg := packages[0]
	report := demoReport{
		GeneratorVersion: samples.GeneratorVersion,
		Seed:             *seed,
		SceneID:          pkg.Key,
		Interoperable:    true,
	}
	var canonical *scene.Scene
	registration := map[string]scene.RegistrationTruth{}
	for _, r := range pkg.Scene.Truth.Registration {
		registration[r.Band] = r
	}
	for version := 1; version <= 3; version++ {
		s, err := scene.Decode(pkg.Files[version])
		if err != nil {
			return fmt.Errorf("decode schema v%d: %w", version, err)
		}
		if canonical == nil {
			canonical = s
		}
		exported, _, err := scene.ExportAllowLossy(s, version)
		if err != nil {
			return err
		}
		reExported, _, err := scene.ExportAllowLossy(s, version)
		if err != nil {
			return err
		}
		result := schemaResult{
			Version:          version,
			SourceVersionOK:  s.SourceVersion == version,
			ByteStable:       string(exported) == string(reExported),
			BandCodesEqual:   equalBands(canonical, s),
			SettingsEqual:    canonical.Settings == s.Settings,
			Width:            s.Width,
			Height:           s.Height,
			RegressionOffset: registration,
		}
		if !result.SourceVersionOK || !result.ByteStable || !result.BandCodesEqual || !result.SettingsEqual {
			report.Interoperable = false
		}
		report.Schemas = append(report.Schemas, result)
	}
	cfg := engine.DefaultConfig()
	cfg.ChunkDelayMS = 0
	verify, err := engine.Verify(context.Background(), canonical, cfg)
	if err != nil {
		return err
	}
	report.Verification = verify
	report.Passed = report.Interoperable && verify.Equal && allPatchesPass(verify.Patches) &&
		verify.PeakRadianceDelta <= canonical.Truth.PatchTolerance

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	printDemo(report)
	fmt.Printf("report: %s\n", *out)
	if !report.Passed {
		return fmt.Errorf("acceptance demo failed")
	}
	return nil
}

func printDemo(report demoReport) {
	fmt.Println("SPECTRA acceptance demo")
	fmt.Printf("generator=%s seed=%d scene=%s\n", report.GeneratorVersion, report.Seed, report.SceneID)
	fmt.Println("schema  source  bytes  bands  settings  size")
	for _, s := range report.Schemas {
		fmt.Printf("v%d      %-6t  %-5t  %-5t  %-8t  %dx%d\n",
			s.Version, s.SourceVersionOK, s.ByteStable, s.BandCodesEqual, s.SettingsEqual, s.Width, s.Height)
	}
	v := report.Verification
	if v != nil {
		passed := 0
		maxDelta := 0.0
		for _, p := range v.Patches {
			if p.Pass {
				passed++
			}
			if p.Delta > maxDelta {
				maxDelta = p.Delta
			}
		}
		fmt.Printf("patches %d/%d pass, max delta %.8f, peak truth %.6f (delta %.8f)\n",
			passed, len(v.Patches), maxDelta, v.TruthPeakRadiance, v.PeakRadianceDelta)
		fmt.Printf("paths live=%v resume=%v full=%v, exact=%t, max pixel delta=%.8f\n",
			v.Live.CommitSequence, v.Resume.CommitSequence, v.Full.CommitSequence, v.Equal, v.LiveVsFull.MaxAbsDelta)
		fmt.Printf("resume loaded checkpoint=%t, commit sequences equal=%t\n", v.Resume.ResumedFromStore, v.CommitSequencesEqual)
	}
	fmt.Printf("acceptance: %t\n", report.Passed)
}

func runSamples(args []string) error {
	fs := flag.NewFlagSet("samples", flag.ContinueOnError)
	seed := fs.Uint64("seed", 20261125, "deterministic sample seed")
	dir := fs.String("dir", "artifacts/samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	packages, err := samples.DefaultPackages(*seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	for _, pkg := range packages {
		for version := 1; version <= 3; version++ {
			name := filepath.Join(*dir, fmt.Sprintf("%s-v%d.json", pkg.Key, version))
			if err := os.WriteFile(name, pkg.Files[version], 0o644); err != nil {
				return err
			}
			fmt.Printf("wrote %s (%d bytes)\n", name, len(pkg.Files[version]))
		}
		truthName := filepath.Join(*dir, pkg.Key+"-truth.json")
		if err := os.WriteFile(truthName, pkg.TruthJSON, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", truthName, len(pkg.TruthJSON))
	}
	return nil
}

func runOnce(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	seed := fs.Uint64("seed", 20261125, "deterministic sample seed")
	sceneKey := fs.String("scene", "neon-foundry", "scene key")
	schemaVersion := fs.Int("schema", 3, "schema version 1..3")
	mode := fs.String("mode", "full", "live, resume, or full")
	dbPath := fs.String("db", "artifacts/run.db", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	packages, err := samples.DefaultPackages(*seed)
	if err != nil {
		return err
	}
	var data []byte
	for _, pkg := range packages {
		if pkg.Key == *sceneKey {
			data = pkg.Files[*schemaVersion]
		}
	}
	if data == nil {
		return fmt.Errorf("scene %q schema v%d not found", *sceneKey, *schemaVersion)
	}
	s, err := scene.Decode(data)
	if err != nil {
		return err
	}
	cp, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer cp.Close()
	cfg := engine.DefaultConfig()
	e, err := engine.New(s, cp, cfg)
	if err != nil {
		return err
	}
	runMode := engine.RunMode(*mode)
	report, err := e.Run(context.Background(), engine.RunOptions{Mode: runMode})
	if err != nil {
		return err
	}
	comp := e.Composite()
	patches := pipeline.MeasurePatches(s, comp.LinearRGB)
	passed := 0
	for _, p := range patches {
		if p.Pass {
			passed++
		}
	}
	fmt.Printf("scene=%s schema=v%d source=v%d mode=%s\n", s.SceneID, s.SchemaVersion, s.SourceVersion, runMode)
	fmt.Printf("segments=%d/%d sequence=%v peak=%.8f outOfGamut=%d\n",
		report.Committed, report.Total, report.CommitSequence, comp.PeakRadiance, len(comp.OutOfGamut))
	fmt.Printf("patches=%d/%d truthPeak=%.8f snapshotReset=%q\n", passed, len(patches), s.Truth.PeakRadiance, report.SnapshotReset)
	return nil
}

func equalBands(a, b *scene.Scene) bool {
	if a == nil || len(a.Bands) != len(b.Bands) {
		return false
	}
	for i := range a.Bands {
		ab, bb := a.Bands[i], b.Bands[i]
		if ab.ID != bb.ID || ab.CenterNM != bb.CenterNM || ab.ResponseXYZ != bb.ResponseXYZ ||
			ab.OffsetX != bb.OffsetX || ab.OffsetY != bb.OffsetY || len(ab.Exposures) != len(bb.Exposures) {
			return false
		}
		for j := range ab.Exposures {
			if ab.Exposures[j].DurationUS != bb.Exposures[j].DurationUS || len(ab.Exposures[j].Codes) != len(bb.Exposures[j].Codes) {
				return false
			}
			for k := range ab.Exposures[j].Codes {
				if ab.Exposures[j].Codes[k] != bb.Exposures[j].Codes[k] {
					return false
				}
			}
		}
	}
	return true
}

func allPatchesPass(patches []pipeline.PatchMeasurement) bool {
	for _, p := range patches {
		if !p.Pass {
			return false
		}
	}
	return true
}
