package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/engine"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
	"gameintegrity/internal/sample"
	"gameintegrity/internal/workbench"
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
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if e := apperr.As(err); e != nil {
			data, _ := json.MarshalIndent(map[string]any{"error": e}, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `verifytool - deterministic recording integrity verifier

Usage:
  verifytool generate -out data/samples -seed 20261124
  verifytool verify   -data data/samples -sample small [-mode auto|fine|candidate]
                      [-path full|realtime|resume] [-work artifacts/work/small]
                      [-budget 24] [-block 8] [-json-out report.json]
  verifytool demo     -data data/samples -out artifacts/demo-results.json
  verifytool compare  -left full.json -right realtime.json
  verifytool serve    -data data/samples -addr 127.0.0.1:8090
`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data/samples", "output sample root")
	seed := fs.Int64("seed", 20261124, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cat, err := sample.Generate(*out, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d samples in %s (seed=%d)\n", len(cat.Samples), *out, cat.Seed)
	for _, s := range cat.Samples {
		fmt.Printf("  %-15s %4dx%-4d frames=%-4d replacement=%d..%d anomaly=%s\n",
			s.ID, s.Width, s.Height, s.FrameCount, s.Replacement.Start, s.Replacement.End, s.Anomaly)
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	data := fs.String("data", "data/samples", "sample root")
	id := fs.String("sample", "small", "sample id")
	mode := fs.String("mode", "auto", "auto|fine|candidate")
	path := fs.String("path", "full", "full|realtime|resume")
	work := fs.String("work", "", "checkpoint work directory")
	budget := fs.Int("budget", 0, "sampling budget (0 uses manifest)")
	block := fs.Int("block", 0, "block size (0 uses manifest)")
	residual := fs.Float64("residual", -1, "residual threshold")
	noise := fs.Float64("noise", -1, "noise threshold")
	bbox := fs.Float64("bbox", -1, "bounding box shift threshold")
	hashJump := fs.Float64("hash-jump", -1, "hash block jump ratio")
	history := fs.Int("history", 0, "previous/next frame history")
	workers := fs.Int("workers", 0, "maximum worker count")
	bufferMB := fs.Int64("buffer-mb", 0, "pixel buffer budget in MiB")
	jsonOut := fs.String("json-out", "", "write full report JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := frameio.Open(*data, *id)
	if err != nil {
		return err
	}
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	if *budget > 0 {
		cfg.SamplingBudget = *budget
	}
	if *block > 0 {
		cfg.BlockSize = *block
	}
	if *residual >= 0 {
		cfg.ResidualThreshold = *residual
	}
	if *noise >= 0 {
		cfg.NoiseThreshold = *noise
	}
	if *bbox >= 0 {
		cfg.BBoxShiftThreshold = *bbox
	}
	if *hashJump >= 0 {
		cfg.HashJumpRatio = *hashJump
	}
	if *history > 0 {
		cfg.History = *history
	}
	if *workers > 0 {
		cfg.MaxWorkers = *workers
	}
	if *bufferMB > 0 {
		cfg.MaxPixelBufferBytes = *bufferMB << 20
	}
	if *work == "" {
		*work = filepath.Join("artifacts", "work", *id)
	}
	report, err := engine.Verify(context.Background(), ds, cfg, engine.Options{
		Path: *path, Mode: *mode, WorkDir: *work,
	})
	if err != nil {
		return err
	}
	if *jsonOut != "" {
		if err := writeJSONFile(*jsonOut, report); err != nil {
			return err
		}
	}
	printReport(report)
	return nil
}

func printReport(r *model.RunReport) {
	fmt.Printf("sample=%s path=%s mode=%s algorithm=%s\n", r.SampleID, r.Path, r.Mode, r.AlgorithmVersion)
	fmt.Printf("progress=%d/%d replacement=%d..%d truth=%d..%d tolerance=%d\n",
		r.CommittedSegments, r.TotalSegments,
		r.Replacement.Detected.Start, r.Replacement.Detected.End,
		r.Replacement.Truth.Start, r.Replacement.Truth.End, r.Replacement.ToleranceFrames)
	fmt.Printf("scale stride=%d downsample=%d refined=%d skipped_ranges=%d buffer_slots=%d waits=%d duration_ms=%d\n",
		r.Scale.Stride, r.Scale.BlockDownsample, len(r.Scale.RefinedFrames), len(r.Scale.SkippedRanges),
		r.Scale.PixelBufferSlots, r.Scale.PixelBufferWaits, r.DurationMS)
	fmt.Printf("counts total=%d intact=%d replaced=%d suspect=%d invalid=%d\n",
		r.Counts.Total, r.Counts.Intact, r.Counts.Replaced, r.Counts.Suspect, r.Counts.Invalid)
	if r.Invalidation != nil {
		fmt.Printf("checkpoint_invalidation fields=%v reasons=%v\n", r.Invalidation.Fields, r.Invalidation.Reasons)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", "data/samples", "sample root")
	addr := fs.String("addr", "127.0.0.1:8090", "listen address")
	work := fs.String("work", "artifacts/work", "checkpoint root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := workbench.New(*data, *work)
	fmt.Printf("workbench listening on http://%s (data=%s work=%s)\n", *addr, *data, *work)
	return httpListenAndServe(*addr, srv.Handler())
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	leftPath := fs.String("left", "", "left report JSON")
	rightPath := fs.String("right", "", "right report JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *leftPath == "" || *rightPath == "" {
		return apperr.New(apperr.Para, "both -left and -right report paths are required")
	}
	left, err := readReport(*leftPath)
	if err != nil {
		return err
	}
	right, err := readReport(*rightPath)
	if err != nil {
		return err
	}
	if err := engine.Compare(left, right); err != nil {
		return err
	}
	fmt.Printf("reports are frame-for-frame equal: %s <> %s (%d frames)\n", *leftPath, *rightPath, len(left.Frames))
	return nil
}

func readReport(path string) (*model.RunReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.Decode, "cannot read report", err)
	}
	var report model.RunReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, apperr.Wrap(apperr.Decode, "report is not valid JSON", err)
	}
	return &report, nil
}
