package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"textureconv"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "convert":
		err = cmdConvert(os.Args[2:], false)
	case "resume":
		err = cmdConvert(os.Args[2:], false)
	case "full":
		err = cmdConvert(os.Args[2:], true)
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if code := textureconv.CodeOf(err); code != "" {
			fmt.Fprintf(os.Stderr, "code: %s\n", code)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `texconv - deterministic texture channel/depth converter

usage:
  texconv generate --out sample [--seed N] [--preset default|small]
  texconv convert  --sample sample --image ID [--store path.db] [--out out] [--tiles N]
  texconv resume   --sample sample --image ID [--store path.db] [--out out] [--tiles N]
  texconv full     --sample sample --image ID [--store path.db] [--out out]
  texconv verify   --sample sample --image ID --store path.db [--params-version v1]
  texconv demo     --out demo-out [--workers N] [--tile 32]`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "sample", "output sample directory")
	seed := fs.Int64("seed", 20261125, "deterministic seed")
	preset := fs.String("preset", "default", "default or small")
	if err := fs.Parse(args); err != nil {
		return err
	}
	specs := textureconv.DefaultImageSpecs()
	if *preset == "small" {
		specs = []textureconv.ImageSpec{
			{ID: "atlas_8_rgba", Width: 320, Height: 240, BitDepth: textureconv.Depth8, Layout: textureconv.LayoutRGBA},
			{ID: "atlas_16_bgra", Width: 385, Height: 257, BitDepth: textureconv.Depth16, Layout: textureconv.LayoutBGRA},
		}
	} else if *preset != "default" {
		return fmt.Errorf("unknown preset %q", *preset)
	}
	b, err := textureconv.GenerateSample(*out, textureconv.GenerateOptions{Seed: *seed, Images: specs})
	if err != nil {
		return err
	}
	fmt.Printf("generated %d images in %s\n", len(b.Manifest.Images), *out)
	fmt.Printf("plan fingerprint: %s\n", b.Plan.Fingerprint())
	fmt.Printf("truth fingerprint: %s\n", b.Truth.Fingerprint)
	return nil
}

type convertReport struct {
	ImageID          string                     `json:"imageId"`
	PlanFingerprint  string                     `json:"planFingerprint"`
	SourceSHA256     string                     `json:"sourceSha256"`
	Run              textureconv.RunReport      `json:"run"`
	Summaries        []textureconv.ImageSummary `json:"summaries"`
	FinalHash        string                     `json:"finalHash,omitempty"`
	Export           string                     `json:"export,omitempty"`
	Verify           *textureconv.VerifyReport  `json:"verify,omitempty"`
	VerifySkipped    bool                       `json:"verifySkipped,omitempty"`
	VerifySkipReason string                     `json:"verifySkipReason,omitempty"`
}

func cmdConvert(args []string, forceFull bool) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	sampleDir := fs.String("sample", "", "sample directory")
	imageID := fs.String("image", "", "image id")
	dbPath := fs.String("store", "", "SQLite checkpoint path")
	outDir := fs.String("out", "out", "report/export directory")
	planPath := fs.String("plan", "", "optional replacement plan JSON")
	tileSize := fs.Int("tile", 64, "tile size")
	workers := fs.Int("workers", 4, "worker count")
	bufferBytes := fs.Int("buffer-bytes", 8<<20, "per-thread transient pixel buffer bytes")
	paramsVersion := fs.String("params-version", "v1", "parameter version")
	maxTiles := fs.Int("tiles", 0, "process at most N tiles, then stop")
	doFull := fs.Bool("full", false, "discard all checkpoints and recompute")
	doVerify := fs.Bool("verify", true, "verify persisted result against truth")
	exportFinal := fs.String("export-final", "", "optional .tstage final image export")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sampleDir == "" {
		return fmt.Errorf("--sample is required")
	}
	b, err := textureconv.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *imageID == "" {
		*imageID = b.Manifest.Images[0].ID
	}
	rec, ok := b.Source(*imageID)
	if !ok {
		return fmt.Errorf("unknown image %q", *imageID)
	}
	im := b.Images[*imageID]
	plan := b.Plan
	if *planPath != "" {
		plan, err = textureconv.LoadPlan(*planPath)
		if err != nil {
			return err
		}
		if err := plan.ValidateForImage(im); err != nil {
			return err
		}
	}
	if *outDir == "" {
		*outDir = "out"
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*outDir, "checkpoints.db")
	}
	store, err := textureconv.OpenStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	engine, err := textureconv.NewEngine(im, plan, textureconv.EngineOptions{
		ImageID: *imageID, SourceHash: rec.SHA256, ParamsVersion: *paramsVersion,
		TileSize: *tileSize, Workers: *workers, BufferBytesPerThread: *bufferBytes,
		Store: store,
	})
	if err != nil {
		return err
	}
	ctx := context.Background()
	var run textureconv.RunReport
	switch {
	case forceFull || *doFull:
		run, err = engine.FullRecompute(ctx)
	case *maxTiles > 0:
		run, err = engine.ProcessLimit(ctx, *maxTiles)
	default:
		run, err = engine.Process(ctx)
	}
	snap, snapErr := engine.Snapshot()
	report := convertReport{
		ImageID: *imageID, PlanFingerprint: plan.Fingerprint(), SourceSHA256: rec.SHA256, Run: run,
	}
	if snapErr == nil {
		for _, e := range snap.Entries {
			report.Summaries = append(report.Summaries, e.Summary)
		}
		if snap.Final != nil {
			report.FinalHash = textureconv.HashStageImage(snap.Final)
		}
	}
	if *exportFinal != "" && snap != nil && snap.Final != nil {
		if err := textureconv.SaveStageFile(*exportFinal, snap.Final); err != nil {
			return err
		}
		report.Export = *exportFinal
	}
	if err != nil {
		_ = writeJSON(filepath.Join(*outDir, "report.json"), report)
		return err
	}
	if *doVerify {
		if snapErr != nil {
			report.VerifySkipped = true
			report.VerifySkipReason = "checkpoint is incomplete; run resume before verification"
		} else if plan.Fingerprint() == b.Plan.Fingerprint() {
			vr, verr := textureconv.VerifyPersisted(b, *imageID, store, textureconv.VerifyOptions{
				ExpectedParamsVersion: *paramsVersion, ExpectedAlgorithmVersion: textureconv.AlgorithmVersion,
			})
			if verr != nil {
				_ = writeJSON(filepath.Join(*outDir, "report.json"), report)
				return verr
			}
			report.Verify = vr
		} else {
			report.VerifySkipped = true
			report.VerifySkipReason = "custom plan has no matching truth fingerprint in this sample"
		}
	}
	if err := writeJSON(filepath.Join(*outDir, "report.json"), report); err != nil {
		return err
	}
	fmt.Printf("image=%s plan=%s progress=%d/%d cacheHits=%d cacheMisses=%d\n",
		*imageID, report.PlanFingerprint, run.Progress.CommittedTiles, run.Progress.TotalTiles,
		run.CacheHits, run.CacheMisses)
	if report.Verify != nil {
		fmt.Printf("verified samples=%d maxDeviation=%d\n", report.Verify.SamplesChecked, report.Verify.MaxDeviation)
	}
	fmt.Printf("report: %s\n", filepath.Join(*outDir, "report.json"))
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("sample", "", "sample directory")
	imageID := fs.String("image", "", "image id")
	dbPath := fs.String("store", "", "SQLite checkpoint path")
	paramsVersion := fs.String("params-version", "v1", "expected parameter version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sampleDir == "" || *dbPath == "" {
		return fmt.Errorf("--sample and --store are required")
	}
	b, err := textureconv.LoadSample(*sampleDir)
	if err != nil {
		return err
	}
	if *imageID == "" {
		*imageID = b.Manifest.Images[0].ID
	}
	store, err := textureconv.OpenStore(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	r, err := textureconv.VerifyPersisted(b, *imageID, store, textureconv.VerifyOptions{
		ExpectedParamsVersion: *paramsVersion, ExpectedAlgorithmVersion: textureconv.AlgorithmVersion,
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "demo-out", "demo output directory")
	seed := fs.Int64("seed", 20261125, "deterministic seed")
	workers := fs.Int("workers", 4, "parallel worker count")
	tile := fs.Int("tile", 32, "tile size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := textureconv.RunDemo(textureconv.DemoOptions{
		Dir: *out, Seed: *seed, Workers: *workers, TileSize: *tile,
	})
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "demo-report.json"), r); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
