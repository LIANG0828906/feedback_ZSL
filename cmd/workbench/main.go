package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"textureworkbench/internal/engine"
	"textureworkbench/internal/sample"
	"textureworkbench/internal/server"
	"textureworkbench/internal/tex"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "sample":
		err = runSample(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "preflight":
		err = runPreflight(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`TEXEL FORGE — local bit-depth and channel-layout workbench

Commands:
  sample     Generate deterministic image samples and target descriptors
  serve      Start the local visual workbench (default http://127.0.0.1:8787)
  preflight  Check local read/write permissions and runtime versions
  verify     Run the acceptance checks for conversion, recovery and consistency
  validate   Parse and validate local target descriptor JSON files

Examples:
  go run ./cmd/workbench sample --seed 42 --out ./data
  go run ./cmd/workbench serve --data ./data --checkpoints ./data/checkpoints
  go run ./cmd/workbench verify --data ./data
  go run ./cmd/workbench validate --descriptors ./data/descriptors`)
}

func runSample(args []string) error {
	flags := flag.NewFlagSet("sample", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	out := flags.String("out", "data", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := sample.Generate(*out, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d deterministic samples and %d target descriptors in %s (seed=%d)\n",
		len(manifest.Samples), len(manifest.Descriptors), *out, *seed)
	for _, info := range manifest.Samples {
		fmt.Printf("  %-8s %3d×%-3d %2d-bit %-5s %s\n", info.ID, info.Width, info.Height, info.BitDepth, info.Layout, info.File)
	}
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8787", "local listen address")
	data := flags.String("data", "data", "deterministic sample directory")
	checkpoints := flags.String("checkpoints", filepath.Join("data", "checkpoints"), "checkpoint directory")
	seed := flags.Int64("seed", 42, "seed used when --auto-generate is enabled")
	autoGenerate := flags.Bool("auto-generate", true, "generate samples when the manifest is missing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*data, "manifest.json")); os.IsNotExist(err) && *autoGenerate {
		fmt.Printf("sample manifest missing; generating deterministic samples in %s\n", *data)
		if _, err := sample.Generate(*data, *seed); err != nil {
			return err
		}
	}
	workbench, err := server.New(*data, *checkpoints)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           workbench.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	fmt.Printf("TEXEL FORGE listening on http://%s\n", *addr)
	fmt.Printf("data=%s checkpoints=%s algorithm=%s go=%s\n", *data, *checkpoints, tex.AlgorithmVersion, runtime.Version())
	return httpServer.ListenAndServe()
}

func runPreflight(args []string) error {
	flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
	data := flags.String("data", "data", "sample directory")
	checkpoints := flags.String("checkpoints", filepath.Join("data", "checkpoints"), "checkpoint directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		return fmt.Errorf("data directory is not writable: %w", err)
	}
	if err := os.MkdirAll(*checkpoints, 0o755); err != nil {
		return fmt.Errorf("checkpoint directory is not writable: %w", err)
	}
	if _, err := os.ReadDir(*data); err != nil {
		return fmt.Errorf("workspace read check failed: %w", err)
	}
	if _, err := os.ReadDir(*checkpoints); err != nil {
		return fmt.Errorf("checkpoint read check failed: %w", err)
	}
	testData := filepath.Join(*data, ".preflight")
	if err := os.WriteFile(testData, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("workspace write check failed: %w", err)
	}
	_ = os.Remove(testData)
	testCheckpoint := filepath.Join(*checkpoints, ".preflight")
	if err := os.WriteFile(testCheckpoint, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("checkpoint write check failed: %w", err)
	}
	_ = os.Remove(testCheckpoint)
	fmt.Printf("preflight OK\n")
	fmt.Printf("  go: %s\n", runtime.Version())
	fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  workspace readable/writable: %s\n", *data)
	fmt.Printf("  checkpoint readable/writable: %s\n", *checkpoints)
	return nil
}

func runValidate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	descriptorPath := flags.String("descriptor", "", "single descriptor JSON file")
	descriptorDir := flags.String("descriptors", "", "directory containing descriptor JSON files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var paths []string
	if *descriptorPath != "" {
		paths = append(paths, *descriptorPath)
	}
	if *descriptorDir != "" {
		matches, err := filepath.Glob(filepath.Join(*descriptorDir, "*.json"))
		if err != nil {
			return err
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return fmt.Errorf("provide --descriptor or --descriptors")
	}
	sort.Strings(paths)
	for _, path := range paths {
		descriptor, err := tex.LoadDescriptorFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Printf("VALID  %-40s %s -> %s/%d bit perm=%v fill=%s\n",
			descriptor.ID, descriptor.SourceLayout, descriptor.TargetLayout,
			descriptor.TargetBitDepth, descriptor.Permutation, descriptor.Fill)
	}
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	data := flags.String("data", "data", "deterministic sample directory")
	seed := flags.Int64("seed", 42, "seed used if samples are generated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*data, "manifest.json")); os.IsNotExist(err) {
		if _, err := sample.Generate(*data, *seed); err != nil {
			return err
		}
	}
	manifest, err := sample.LoadManifest(*data)
	if err != nil {
		return err
	}
	cases := []struct {
		sampleID     string
		descriptorID string
	}{
		{"rgb8", "rgb8-to-bgra8-zero"},
		{"rgba8", "rgba8-to-rgb8-drop"},
		{"rgb16", "rgb16-to-rgba16-repeat"},
		{"bgra16", "bgra16-to-rgb8-drop"},
	}
	cfg := engine.Config{
		BlockWidth:            32,
		BlockHeight:           32,
		MaxWorkers:            4,
		BufferPixelsPerWorker: 1024,
		QueueDepth:            2,
		ParameterVersion:      "acceptance-v1",
		CheckpointRoot:        filepath.Join(*data, "checkpoints"),
	}
	fmt.Println("== three-path consistency ==")
	for _, tc := range cases {
		info, _ := manifest.Sample(tc.sampleID)
		desc, _ := manifest.Descriptor(tc.descriptorID)
		image, err := tex.DecodeImageFile(filepath.Join(*data, info.File))
		if err != nil {
			return err
		}
		image.ID = info.ID
		image.EnsureHash()
		comparison, err := engine.CompareThreePaths(image, desc, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.descriptorID, err)
		}
		fmt.Printf("  MATCH  %-24s %s -> %s/%d bit  paths=%d\n",
			tc.descriptorID, info.Layout, desc.TargetLayout, desc.TargetBitDepth, len(comparison.Paths))
		for _, path := range comparison.Paths {
			fmt.Printf("         %-20s recompute=%-4d cache=%-4d time=%.3fms\n",
				path.Path, path.BlockRecomputes, path.CacheHits, path.TotalTimeMS)
		}
	}

	fmt.Println("== version mismatch invalidates whole snapshot ==")
	info, _ := manifest.Sample("rgba8")
	desc, _ := manifest.Descriptor("rgba8-to-bgra16-repeat")
	image, err := tex.DecodeImageFile(filepath.Join(*data, info.File))
	if err != nil {
		return err
	}
	image.ID = info.ID
	image.EnsureHash()
	tempRoot, err := os.MkdirTemp("", "twb-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)
	cfg.CheckpointRoot = tempRoot
	cfg.SessionKey = "verify-invalidation"
	first, err := engine.OpenSession(image, desc, cfg)
	if err != nil {
		return err
	}
	if err := first.RunSegmented(engine.RunOptions{MaxBlocks: 3}); err != nil {
		return err
	}
	first.Close()
	changed := desc
	changed.ID = "verify-changed-target"
	changed.TargetBitDepth = 8
	changed.TargetLayout = tex.LayoutRGB
	changed.Permutation = []int{0, 1, 2, 3}
	changed.Fill = tex.FillZero
	second, err := engine.OpenSession(image, changed, cfg)
	if err != nil {
		return err
	}
	defer second.Close()
	snapshot := second.Snapshot()
	if !snapshot.Restore.Invalidated || snapshot.Progress.Committed != 0 {
		return fmt.Errorf("expected whole-snapshot invalidation, got restore=%+v progress=%+v",
			snapshot.Restore, snapshot.Progress)
	}
	fmt.Printf("  INVALIDATED reasons=%v restored=%d committed=%d\n",
		snapshot.Restore.Reasons, snapshot.Restore.RestoredBlocks, snapshot.Progress.Committed)
	fmt.Println("VERIFY OK")
	return nil
}
