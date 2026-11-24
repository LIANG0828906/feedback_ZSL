package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"yuvench/errs"
	"yuvench/filters"
	"yuvench/workbench"
	"yuvench/yuv"
)

const usageText = `YUV 4:2:0 filtering workbench

Commands:
  generate  Generate deterministic local samples and truth manifest.
  inspect   Load and validate a sample image and its truth.
  run       Run the incremental/full/resumable filtering pipeline.
  verify    Run the acceptance verification across incremental, resume, and full paths.

Use "yuvworkbench <command> -h" for command flags.`

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usageText)
		return 2
	}
	switch args[0] {
	case "generate":
		return commandGenerate(args[1:])
	case "inspect":
		return commandInspect(args[1:])
	case "run":
		return commandRun(args[1:])
	case "verify":
		return commandVerify(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stdout, usageText)
		return 0
	default:
		return emitError(&errs.Error{Code: errs.CodeParameter, Message: "unknown command", Actual: args[0]})
	}
}

func commandGenerate(args []string) int {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	out := flags.String("out", "samples", "sample output directory")
	seed := flags.Int64("seed", 20261124, "deterministic sample seed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := yuv.GenerateSamples(*out, *seed)
	if err != nil {
		return emitError(err)
	}
	return emitJSON(map[string]any{"ok": true, "manifest": manifest, "seed": *seed, "images": 3})
}

func commandInspect(args []string) int {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	manifest := flags.String("manifest", "samples/manifest.json", "sample manifest")
	imageName := flags.String("image", "", "image name (default: first image)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	image, err := yuv.LoadImage(*manifest, *imageName)
	if err != nil {
		return emitError(err)
	}
	if err := yuv.VerifyTruth(image); err != nil {
		return emitError(err)
	}
	return emitJSON(map[string]any{
		"ok":         true,
		"name":       image.Meta.Name,
		"width":      image.Meta.Width,
		"height":     image.Meta.Height,
		"bitDepth":   image.Meta.BitDepth,
		"channels":   image.Meta.Channels,
		"planes":     map[string][2]int{"Y": {image.Y.Width, image.Y.Height}, "U": {image.U.Width, image.U.Height}, "V": {image.V.Width, image.V.Height}},
		"inputHash":  image.InputHash,
		"blocks":     image.Meta.Blocks,
		"samples":    image.Meta.Samples,
		"truthValid": true,
	})
}

func commandRun(args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	manifest := flags.String("manifest", "samples/manifest.json", "sample manifest")
	imageName := flags.String("image", "", "image name (default: first image)")
	dbPath := flags.String("db", "artifacts/run/workbench.db", "SQLite cache snapshot")
	outDir := flags.String("out", "artifacts/run", "artifact output directory")
	paramsPath := flags.String("params", "", "optional JSON parameter file")
	mode := flags.String("mode", "incremental", "incremental, full, or resume")
	maxParallel := flags.Int("max-parallel", 3, "maximum concurrent stage workers")
	maxBuffer := flags.Int64("max-buffer", 16<<20, "global working-buffer byte budget")
	perWorker := flags.Int64("per-worker-buffer", 1<<20, "per-worker working-buffer byte limit")
	chunkRows := flags.Int("chunk-rows", 16, "rows per chunk")
	pauseAfter := flags.Int("pause-after", 0, "pause after this many recomputed stage commits")
	retries := flags.Int("retries", 0, "retry count after E_STAGE")
	faultNode := flags.String("fault-node", "", "inject one failure at this node for testing")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	params := filters.DefaultParams()
	if *paramsPath != "" {
		data, err := os.ReadFile(*paramsPath)
		if err != nil {
			return emitError(&errs.Error{Code: errs.CodeIO, Message: "read parameter file", Cause: err.Error()})
		}
		if err := json.Unmarshal(data, &params); err != nil {
			return emitError(&errs.Error{Code: errs.CodeParameter, Field: "params", Message: "parse parameter file", Cause: err.Error()})
		}
	}
	image, err := yuv.LoadImage(*manifest, *imageName)
	if err != nil {
		return emitError(err)
	}
	limits := filters.Limits{MaxParallel: *maxParallel, MaxBufferBytes: *maxBuffer, PerWorkerBufferBytes: *perWorker, ChunkRows: *chunkRows}
	store, err := workbench.OpenStore(*dbPath)
	if err != nil {
		return emitError(err)
	}
	engine := workbench.NewEngine(store)
	defer engine.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	faulted := false
	fault := func(node string) error {
		if *faultNode != "" && node == *faultNode && !faulted {
			faulted = true
			return errors.New("injected stage failure")
		}
		return nil
	}
	attempts := *retries + 1
	var result *workbench.RunResult
	var runErr error
	for attempt := 0; attempt < attempts; attempt++ {
		result, runErr = engine.Run(ctx, image, params, limits, workbench.RunOptions{
			Mode:              *mode,
			PauseAfterCommits: *pauseAfter,
			Fault:             fault,
		})
		if runErr == nil || errs.CodeOf(runErr) != errs.CodeStage {
			break
		}
	}
	if runErr == nil && result != nil && result.Final != nil {
		rgba, heatStats, heatErr := workbench.Heatmap(result.Original, result.Final, params.Heatmap.Normalization)
		if heatErr != nil {
			runErr = heatErr
		} else {
			result.Heatmap = &heatStats
			if err := workbench.WriteArtifacts(*outDir, result, rgba); err != nil {
				runErr = err
			}
		}
	} else if result != nil {
		if err := workbench.WriteArtifacts(*outDir, result, nil); err != nil {
			runErr = err
		}
	}
	if result != nil {
		if err := writeReport(*outDir, result); err != nil && runErr == nil {
			runErr = err
		}
	}
	if runErr != nil {
		if result != nil {
			_ = emitJSON(result)
		}
		return emitError(runErr)
	}
	return emitJSON(result)
}

func commandVerify(args []string) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	manifest := flags.String("manifest", "samples/manifest.json", "sample manifest")
	imageName := flags.String("image", "", "image name (default: first image)")
	outDir := flags.String("out", "artifacts/verify", "verification output directory")
	seed := flags.Int64("seed", 20261124, "seed used when samples must be generated")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if _, err := os.Stat(*manifest); err != nil {
		sampleDir := filepath.Join("samples")
		generated, genErr := yuv.GenerateSamples(sampleDir, *seed)
		if genErr != nil {
			return emitError(genErr)
		}
		*manifest = generated
	}
	report, err := workbench.VerifySamples(*manifest, *imageName, *outDir, *seed)
	if report != nil {
		_ = emitJSON(map[string]any{
			"ok":     report.OK,
			"image":  report.Image,
			"checks": report.Checks,
			"report": filepath.Join(*outDir, "report.json"),
		})
	}
	if err != nil {
		return emitError(err)
	}
	return 0
}

func writeReport(dir string, result *workbench.RunResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := filepath.Join(dir, "report.json.tmp")
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(dir, "report.json"))
}

func emitJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

func emitError(err error) int {
	if err == nil {
		return 0
	}
	business, ok := errs.As(err)
	if !ok {
		business = &errs.Error{Code: errs.CodeIO, Message: err.Error()}
	}
	envelope := map[string]any{"ok": false, "code": business.Code, "error": business}
	encoder := json.NewEncoder(os.Stderr)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(envelope)
	code := 2
	if business.Code == errs.CodeCancel {
		code = 130
	}
	if business.Code == errs.CodeVerify {
		code = 3
	}
	return code
}
