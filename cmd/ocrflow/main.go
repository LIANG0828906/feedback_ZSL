package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"ocrflow"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if code := ocrflow.ErrorCode(err); code != "" {
			fmt.Fprintf(os.Stderr, "%s: %v\n", code, err)
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "generate":
		return generateCmd(args[1:])
	case "verify":
		return verifyCmd(args[1:])
	case "serve":
		return serveCmd(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Println(`ocrflow <generate|verify|serve>

  generate [--out artifacts/samples] [--seed 42]
  verify   [--work artifacts/demo]
  serve    [--work artifacts/demo] [--addr 127.0.0.1:18080]`)
}

func generateCmd(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", filepath.Join("artifacts", "samples"), "sample output directory")
	seed := fs.Int64("seed", ocrflow.FixedSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := ocrflow.WriteSampleSet(*out, *seed)
	if err != nil {
		return err
	}
	if _, err := ocrflow.VerifySampleSet(*out, *seed); err != nil {
		return err
	}
	fmt.Printf("generated %d shots, %d files, %d recipes in %s (seed=%d)\n", len(manifest.Shots), len(manifest.Files), len(manifest.Recipes), *out, *seed)
	fmt.Printf("manifest: %s\n", filepath.Join(*out, "manifest.json"))
	return nil
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	work := fs.String("work", filepath.Join("artifacts", "demo"), "verification work directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := ocrflow.RunVerification(*work)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	outPath := filepath.Join(*work, "verification.json")
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	for _, c := range report.Checks {
		mark := "PASS"
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %s — %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("verification report: %s\n", outPath)
	if !report.OK {
		return fmt.Errorf("acceptance verification failed")
	}
	return nil
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	work := fs.String("work", filepath.Join("artifacts", "demo"), "local demo directory")
	addr := fs.String("addr", "127.0.0.1:18080", "listen address")
	reset := fs.Bool("reset", false, "reset demo SQLite database before serving")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		return err
	}
	sampleDir := filepath.Join(*work, "samples")
	if _, err := ocrflow.WriteSampleSet(sampleDir, ocrflow.FixedSeed); err != nil {
		return err
	}
	shots, err := ocrflow.GenerateSampleSet(ocrflow.FixedSeed)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(*work, "serve.db")
	if *reset {
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	}
	svc, err := ocrflow.NewService(ocrflow.DefaultConfig(dbPath), ocrflow.DefaultRecipe(), ocrflow.DefaultTemplateLibrary(), shots)
	if err != nil {
		return err
	}
	defer svc.Close()
	if _, err := svc.Run("serve-start"); err != nil {
		return err
	}
	recipe := svc.Recipe()
	if det, err := detectFirstShot(shots, recipe); err == nil {
		if _, ok := ocrflow.ToggleBackgroundBit(shots[1], det, 1); ok {
			_, _ = svc.InvalidateRegion(shots[1].ShotID, 1, "startup_demo_region_replace")
			_, _ = svc.Run("serve-demo-incremental")
		}
	}
	fmt.Printf("OCRFlow diagnostics: http://%s/\n", *addr)
	fmt.Printf("health: http://%s/healthz; state: http://%s/api/state\n", *addr, *addr)
	return svc.ListenAndServe(*addr)
}

func detectFirstShot(shots []*ocrflow.Screenshot, recipe *ocrflow.Recipe) (*ocrflow.DetectionResult, error) {
	prep, err := ocrflow.PreprocessImage(shots[1].RGBA, recipe.Preprocess)
	if err != nil {
		return nil, err
	}
	return ocrflow.DetectLines(shots[1], recipe, prep)
}
