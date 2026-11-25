package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/diagnostics"
	"iconseg/internal/runner"
	"iconseg/internal/sample"
	"iconseg/internal/verifier"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "run":
		err = runPipelineCommand(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = core.ParaError("command", "{generate,run,verify,serve}", "unknown command "+os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `iconseg: deterministic icon foreground separation

Commands:
  generate  write deterministic sample icons and truth manifests
  run       preview, exact segmentation, SQLite layer checkpoints, verifier
  verify    independently verify persisted block/layer products
  serve     loopback net/http diagnostics page with live preview

Run "iconseg <command> -h" for command flags.`)
}

type commonFlags struct {
	seed   int64
	params core.SegmentParams
}

func addParamFlags(fs *flag.FlagSet) *commonFlags {
	values := &commonFlags{params: core.DefaultParams()}
	fs.Int64Var(&values.seed, "seed", 20261125, "deterministic generator seed")
	fs.StringVar(&values.params.GradientOperator, "gradient", values.params.GradientOperator, "gradient operator: sobel|prewitt")
	fs.IntVar(&values.params.SmoothKernel, "smooth", values.params.SmoothKernel, "Gaussian smoothing kernel: 1|3|5")
	fs.Float64Var(&values.params.FloodThreshold, "flood", values.params.FloodThreshold, "watershed flood threshold [0,2048]")
	fs.IntVar(&values.params.MinRegionSize, "min-region", values.params.MinRegionSize, "minimum initial region size [1,1000000]")
	fs.Float64Var(&values.params.MergeCostLimit, "merge-cost", values.params.MergeCostLimit, "region merge cost limit [0,4096]")
	fs.IntVar(&values.params.MergeAreaDiff, "merge-area", values.params.MergeAreaDiff, "region merge area-difference limit [0,100000000]")
	fs.IntVar(&values.params.CleanupRadius, "cleanup", values.params.CleanupRadius, "open/close cleanup radius [0,8]")
	return values
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts/generated", "generated sample directory")
	count := fs.Int("count", 3, "number of generated icons (1..3)")
	common := addParamFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *count < 1 || *count > 3 {
		return core.ParaError("count", "[1,3]", "sample count is outside the legal interval")
	}
	if err := common.params.Validate(); err != nil {
		return err
	}
	icons := sample.Generate(common.seed)[:*count]
	if err := sample.WriteDataset(*out, icons); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"seed": common.seed, "output": *out, "count": len(icons),
		"icons": manifests(icons), "params": common.params,
	})
}

type runAggregate struct {
	Seed         int64              `json:"seed"`
	Params       core.SegmentParams `json:"params"`
	Database     string             `json:"database"`
	SampleDir    string             `json:"sample_dir"`
	ResultDir    string             `json:"result_dir"`
	Icons        []*runner.Record   `json:"icons"`
	Verification []verifier.Report  `json:"verification"`
}

func runPipelineCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	out := fs.String("out", "artifacts/run", "run output directory")
	dbPath := fs.String("db", "", "SQLite checkpoint path (default <out>/checkpoints.db)")
	pauseAfter := fs.Int("pause-after", 0, "pause after N new layer commits (0 = finish)")
	workers := fs.Int("workers", 0, "worker override (0 = manifest budget)")
	common := addParamFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.params.Validate(); err != nil {
		return err
	}
	if *pauseAfter < 0 {
		return core.ParaError("pause-after", "[0,inf)", "pause count cannot be negative")
	}
	if *workers < 0 || *workers > 32 {
		return core.ParaError("workers", "[0,32]", "worker override is outside the legal interval")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*out, "checkpoints.db")
	}
	sampleDir := filepath.Join(*out, "samples")
	resultDir := filepath.Join(*out, "results")
	icons := sample.Generate(common.seed)
	if err := sample.WriteDataset(sampleDir, icons); err != nil {
		return err
	}
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	aggregate := runAggregate{
		Seed: common.seed, Params: common.params, Database: *dbPath,
		SampleDir: sampleDir, ResultDir: resultDir,
	}
	for _, icon := range icons {
		snap, err := core.NewSnapshot(common.seed, icon.Manifest.ImageID, common.params)
		if err != nil {
			return err
		}
		record, err := runner.Run(context.Background(), store, icon, snap, runner.Options{
			PauseAfterCommits: *pauseAfter, WorkerOverride: *workers,
		})
		if err != nil {
			return err
		}
		aggregate.Icons = append(aggregate.Icons, record)
		iconDir := filepath.Join(resultDir, icon.Manifest.ImageID)
		if err := os.MkdirAll(iconDir, 0o755); err != nil {
			return err
		}
		if err := writeMaskPNG(filepath.Join(iconDir, "preview_mask.png"), record.PreviewMask,
			icon.Manifest.Width, icon.Manifest.Height); err != nil {
			return err
		}
		if err := writeMaskPNG(filepath.Join(iconDir, "exact_mask.png"), record.ExactMask,
			icon.Manifest.Width, icon.Manifest.Height); err != nil {
			return err
		}
		if err := writeMaskPNG(filepath.Join(iconDir, "truth_mask.png"), record.TruthMask,
			icon.Manifest.Width, icon.Manifest.Height); err != nil {
			return err
		}
		if err := writeJSONFile(filepath.Join(iconDir, "report.json"), record); err != nil {
			return err
		}
		if !record.Paused {
			verified, err := verifier.Verify(store, record.SnapshotID, icon.Manifest.ReferenceFingerprint)
			if err != nil {
				return err
			}
			aggregate.Verification = append(aggregate.Verification, verified)
		}
	}
	if err := writeJSONFile(filepath.Join(*out, "run-record.json"), aggregate); err != nil {
		return err
	}
	return printJSON(aggregate)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/run/checkpoints.db", "SQLite checkpoint path")
	snapshot := fs.String("snapshot", "", "snapshot id (empty = all complete snapshots)")
	truth := fs.String("truth", "", "expected truth fingerprint (empty = persisted fingerprint)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var ids []string
	if *snapshot != "" {
		ids = []string{*snapshot}
	} else {
		snapshots, err := store.AllSnapshots()
		if err != nil {
			return err
		}
		for _, item := range snapshots {
			if item.Status == "complete" {
				ids = append(ids, item.ID)
			}
		}
	}
	if len(ids) == 0 {
		return core.NewError(core.CodeVersion, "no complete snapshots are available for verification")
	}
	reports := make([]verifier.Report, 0, len(ids))
	for _, id := range ids {
		expected := *truth
		if expected == "" {
			snapshotRecord, err := store.GetSnapshot(id)
			if err != nil {
				return err
			}
			expected = snapshotRecord.TruthFingerprint
		}
		report, err := verifier.Verify(store, id, expected)
		if err != nil {
			return err
		}
		reports = append(reports, report)
	}
	return printJSON(map[string]any{"verified": reports, "count": len(reports)})
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "loopback listen address")
	dbPath := fs.String("db", "artifacts/serve/checkpoints.db", "SQLite checkpoint path")
	common := addParamFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := diagnostics.ValidateLoopbackAddress(*addr); err != nil {
		return err
	}
	if err := common.params.Validate(); err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	icons := sample.Generate(common.seed)
	server, err := diagnostics.New(store, icons, common.params, common.seed)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: server.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "diagnostics listening on http://%s/\n", listener.Addr().String())
	err = httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeMaskPNG(path string, mask []byte, w, h int) error {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i, value := range mask {
		if i >= len(img.Pix) {
			break
		}
		if value != 0 {
			img.Pix[i] = 255
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(f, img)
	closeErr := f.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

func writeJSONFile(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func manifests(icons []core.Icon) []core.TruthManifest {
	out := make([]core.TruthManifest, 0, len(icons))
	for _, icon := range icons {
		manifest := icon.Manifest
		manifest.ReferenceMask = nil
		out = append(out, manifest)
	}
	return out
}

func fail(err error) {
	app := core.AsAppError(err)
	_ = printJSON(app)
	os.Exit(1)
}
