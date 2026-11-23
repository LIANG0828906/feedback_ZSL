// Command pipeline runs one of the three evaluation paths (realtime, resume,
// full recompute) against a generated sample and a local checkpoint store.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"vqa/internal/cliutil"
	"vqa/internal/metrics"
	"vqa/internal/pipeline"
	"vqa/internal/sample"
)

func main() {
	dataDir := flag.String("data", "data", "sample data directory")
	storeDir := flag.String("store", "store/realtime", "checkpoint store directory")
	quality := flag.Int("quality", 70, "quality level")
	mode := flag.String("mode", "realtime", "realtime|resume|rebuild")
	window := flag.Int("window", 7, "SSIM window size")
	stride := flag.Int("stride", 2, "SSIM window stride")
	halo := flag.Int("halo", 8, "overlap band width per side")
	workers := flag.Int("workers", 2, "max parallel workers")
	maxBuf := flag.Int("max-pixel-buf", 64*1024, "max pooled pixel buffer")
	crashAfter := flag.Int("crash-after", 0, "simulate kill after N commits")
	cliutil.Parse()

	seq, err := sample.LoadSequence(*dataDir, *quality)
	if err != nil {
		cliutil.Fail(err)
	}
	params := metrics.DefaultParams(seq.Manifest.BlockSize, seq.Manifest.Width, seq.Manifest.Height, seq.Manifest.Channels)
	params.Window = *window
	params.Stride = *stride
	params.Halo = *halo

	run, ok := map[string]pipeline.Run{
		"realtime": pipeline.Realtime,
		"resume":   pipeline.Resume,
		"rebuild":  pipeline.Rebuild,
	}[*mode]
	if !ok {
		cliutil.Fail(errInvalidMode(*mode))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := &pipeline.Engine{
		Seq:         seq,
		Params:      params,
		StoreDir:    *storeDir,
		Workers:     *workers,
		MaxPixelBuf: *maxBuf,
		EdgeTol:     12,
		Crash:       pipeline.CrashPlan{AfterCommits: *crashAfter},
	}
	st, err := eng.Run(ctx, run)
	if err != nil {
		if pipeline.IsSimulatedCrash(err) {
			cliutil.EmitJSON(map[string]any{
				"status":  "terminated",
				"detail":  "simulated kill; only durable blocks are recoverable",
				"commits": *crashAfter,
				"store":   *storeDir,
			})
			os.Exit(0)
		}
		cliutil.Fail(err)
	}
	committed, total := st.Progress()
	frames := st.Frames()
	cliutil.EmitJSON(map[string]any{
		"status":        "completed",
		"mode":          *mode,
		"quality":       *quality,
		"committed":     committed,
		"total":         total,
		"frame_records": len(frames),
		"store":         *storeDir,
	})
}
