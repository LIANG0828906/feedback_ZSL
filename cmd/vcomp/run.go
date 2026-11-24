package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"virtualstudio/compose"
	"virtualstudio/engine"
	"virtualstudio/formats"
	"virtualstudio/model"
)

func composeFramePNG(frame model.Frame) ([]byte, error) {
	return compose.FramePNG(frame)
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dataDir := fs.String("data", "samples", "sample data directory")
	version := fs.String("version", "v1", "source version v1|v2|v3")
	scenePath := fs.String("scene", "", "override scene path")
	commandsPath := fs.String("commands", "", "override commands path")
	dbPath := fs.String("db", filepath.Join("artifacts", "workbench.db"), "SQLite database path")
	projectID := fs.String("project", "local-project", "project id")
	target := fs.String("target", "", "workspace target export format (empty = preserve or v3)")
	windowFrames := fs.Int("window", 0, "frames per window (0 = command default)")
	mode := fs.String("mode", "realtime", "realtime|resume|full")
	workers := fs.Int("workers", 3, "maximum parallel windows")
	buffer := fs.Int64("buffer", 0, "pixel buffer bytes per worker (0 = automatic)")
	bufferTotal := fs.Int64("buffer-total", 0, "shared pixel buffer bytes (0 = workers * per-worker)")
	retries := fs.Int("retries", 1, "per-window retry attempts")
	suspendAfter := fs.Int("suspend-after", 0, "suspend after N committed windows (0 = all)")
	cancelAfter := fs.Int("cancel-after", 0, "mark canceled after N committed windows (0 = disabled)")
	framesOut := fs.String("frames-out", "", "write composite PNG frames to this directory")
	exportDir := fs.String("export-dir", "", "optional export bundle directory")
	exportTarget := fs.String("export-target", "", "optional export target version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *mode {
	case "realtime", "resume", "full":
	default:
		return fmt.Errorf("--mode must be realtime, resume or full")
	}
	lp, ws, err := openWorkspace(ctx, *dataDir, *version, *scenePath, *commandsPath, *dbPath, *projectID, *target, *windowFrames)
	if err != nil {
		return err
	}
	defer lp.Store.Close()
	result, err := ws.Run(ctx, engine.RunOptions{
		Mode: *mode, MaxWorkers: *workers, PixelBufferBytesPerWorker: *buffer,
		PixelBufferTotalBytes: *bufferTotal,
		ForceFull:             *mode == "full", MaxRetries: *retries, SuspendAfter: *suspendAfter,
		CancelAfter: *cancelAfter,
	})
	if err != nil {
		return err
	}
	printJSON(summarize(result))
	if len(result.FailedWindows) > 0 {
		return fmt.Errorf("one or more windows failed; rerun to retry")
	}
	if *framesOut != "" {
		if err := writeResultFrames(*framesOut, result.Frames); err != nil {
			return err
		}
	}
	if *exportDir != "" {
		targetVersion := *exportTarget
		if targetVersion == "" {
			targetVersion = ws.TargetFormat()
		}
		manifest, err := ws.Export(ctx, *exportDir, targetVersion, result.Frames)
		if err != nil {
			return err
		}
		printJSON(manifest)
	}
	return nil
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dataDir := fs.String("data", "samples", "sample data directory")
	version := fs.String("version", "v1", "source version")
	scenePath := fs.String("scene", "", "override scene path")
	commandsPath := fs.String("commands", "", "override commands path")
	target := fs.String("target", "v1", "target format v1|v2|v3")
	out := fs.String("out", "artifacts/export", "bundle output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lp, err := loadData(*dataDir, *version, *scenePath, *commandsPath)
	if err != nil {
		return err
	}
	frames, err := compose.RenderAll(lp.Scene, lp.Commands, compose.RenderOptions{})
	if err != nil {
		return err
	}
	manifest, err := formatsWriteBundle(*out, lp.Scene, frames, *target)
	if err != nil {
		return err
	}
	_ = ctx
	printJSON(manifest)
	return nil
}

func formatsWriteBundle(dir string, scene *model.Scene, frames []model.Frame, target string) (*formats.BundleManifest, error) {
	return formats.WriteBundle(dir, scene, frames, target)
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
