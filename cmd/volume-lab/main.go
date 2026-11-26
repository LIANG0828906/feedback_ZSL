package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"volumekernel/internal/acceptance"
	"volumekernel/internal/generator"
	"volumekernel/internal/imageout"
	"volumekernel/internal/model"
	"volumekernel/internal/pipeline"
	"volumekernel/internal/scene"
	"volumekernel/internal/workbench"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"verify"}
	}
	var err error
	switch args[0] {
	case "verify":
		err = runVerify(args[1:])
	case "serve":
		err = runServe(args[1:])
	case "render":
		err = runRender(args[1:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "volume-lab: %v\n", err)
		if code := model.CodeOf(err); code != "" {
			fmt.Fprintf(os.Stderr, "error code: %s\n", code)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stdout, `volume-lab - deterministic transparent-media volumetric kernel

Commands:
  verify   generate the sample, run all acceptance checks, write artifacts
  serve    start the local diagnostic workbench
  render   render one frame to PNG

Run "volume-lab <command> -h" for command flags.`)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic sample seed")
	camera := fs.Int("camera", 2, "camera trajectory index")
	out := fs.String("out", "artifacts", "artifact output directory")
	work := fs.String("work", "", "checkpoint working directory (default: temp)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := acceptance.Run(acceptance.Options{
		Seed: *seed, CameraIndex: *camera, OutDir: *out, WorkDir: *work,
	})
	if err != nil {
		return err
	}
	fmt.Printf("scene=%s seed=%d snapshot=%s\n", report.SceneID, report.Seed, report.SnapshotVersion)
	fmt.Printf("exact paths: full=%d realtime=%d resume=%d pixels; identical=%t\n",
		report.FullPath.Pixels, report.RealtimePath.Pixels, report.ResumePath.Pixels, report.ThreePathsEqual)
	fmt.Printf("frame digest: full=%s realtime=%s resume=%s\n",
		short(report.FullPath.FrameHash), short(report.RealtimePath.FrameHash), short(report.ResumePath.FrameHash))
	fmt.Printf("preview: %.3f ms (deadline=%t), max error=%.9g, over=%.6f\n",
		report.Preview.ElapsedMS, report.PreviewDeadlineMet, report.PreviewCompare.MaxAbsError, report.PreviewCompare.OverToleranceRate)
	fmt.Printf("checkpoint blocks=%d, queue events=%d\n", report.CheckpointBlocks, len(report.QueueEvents))
	fmt.Printf("invalidation: affected=%d unaffected(cache hits)=%d/%d, graph edges=%d\n",
		report.Invalidation.AffectedBlocks, report.Invalidation.CacheHits, report.Invalidation.UnaffectedBlocks, report.Invalidation.GraphEdges)
	fmt.Printf("exception chains: ")
	for i, e := range report.Exceptions {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Printf("%s=%s", e.Case, e.Code)
	}
	fmt.Println()
	fmt.Printf("artifacts: %s\n", filepath.Join(*out, "report.md"))
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "workbench listen address")
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic sample seed")
	camera := fs.Int("camera", 2, "camera trajectory index")
	db := fs.String("db", filepath.Join(".volume-lab", "workbench.db"), "persistent SQLite cache path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv, err := workbench.New(workbench.Options{Addr: *addr, Seed: *seed, CameraIndex: *camera, DBPath: *db})
	if err != nil {
		return err
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("volume-lab diagnostics: http://%s\n", *addr)
	fmt.Println("Press Ctrl-C to stop.")
	return srv.Serve(ctx, *addr)
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic sample seed")
	camera := fs.Int("camera", 2, "camera trajectory index")
	mode := fs.String("mode", "exact", "exact or preview")
	out := fs.String("out", "render.png", "PNG output path")
	scale := fs.Int("scale", 8, "pixel scale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := generator.Generate(*seed)
	if err := scene.Validate(s); err != nil {
		return err
	}
	if *camera < 0 || *camera >= len(s.CameraTrack.Samples) {
		return model.ParamError("", "camera", "camera index %d out of range", *camera)
	}
	snap, err := model.NewSnapshot(s)
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := (&pipeline.Engine{}).Render(snap, pipeline.RenderOptions{
		CameraIndex: *camera, Mode: *mode, MaxParallel: s.Recipe.MaxParallelBlocks,
	})
	if err != nil {
		return err
	}
	if err := imageout.WriteFramePNG(*out, res.Frame, *scale); err != nil {
		return err
	}
	fmt.Printf("rendered %s frame: %d pixels in %.3f ms -> %s\n", *mode, len(res.Frame.Pixels), res.Frame.ElapsedMS, *out)
	fmt.Printf("wall time: %.3f ms, actual steps=%d, shadow samples=%d\n",
		float64(time.Since(start).Microseconds())/1000, res.Frame.ActualSteps, res.Frame.ActualShadows)
	return nil
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
