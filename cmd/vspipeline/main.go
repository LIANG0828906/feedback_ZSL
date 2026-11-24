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
	"syscall"
	"time"

	"virtualstage/app"
	"virtualstage/pipeline"
	"virtualstage/sample"
)

const defaultWorkDir = "work"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "sample":
		err = commandSample(os.Args[2:])
	case "validate":
		err = commandValidate(ctx, os.Args[2:])
	case "full":
		err = commandFull(ctx, os.Args[2:])
	case "incremental":
		err = commandIncremental(ctx, os.Args[2:])
	case "verify-incremental":
		err = commandVerifyIncremental(ctx, os.Args[2:])
	case "export":
		err = commandExport(ctx, os.Args[2:])
	case "inspect":
		err = commandInspect(os.Args[2:])
	case "resume":
		err = commandResume(ctx, os.Args[2:])
	case "png":
		err = commandPNG(os.Args[2:])
	case "serve":
		err = commandServe(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `virtual-stage pipeline

Commands:
  sample              generate deterministic v1/v2/v3 inputs, instructions and truth
  validate            load all versions and compare sampled frames to truth
  full                clear caches and composite every frame window
  incremental         apply a layer/time-base edit and recompute affected windows
  verify-incremental  compare incremental output with an empty-cache full run
  export              export model+frames and verify target-version round trip
  inspect             print model, window matrix and checkpoint state
  resume              restart and report restored/recomputed checkpoint state
  png                 write a committed frame to PNG
  serve               start the local interactive desktop UI in a browser

Common flags: --work DIR --workers N --max-frame-bytes N`)
}

func commonFlags(name string) (*flag.FlagSet, *string, *int, *int64) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	work := fs.String("work", defaultWorkDir, "working directory")
	workers := fs.Int("workers", 4, "maximum parallel window workers")
	maxBytes := fs.Int64("max-frame-bytes", 64<<20, "maximum frame buffer bytes per worker")
	return fs, work, workers, maxBytes
}

func commandSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	out := fs.String("out", defaultWorkDir, "output working directory")
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	frameCount := fs.Int("frames", sample.DefaultFrameCount, "frame count")
	windowSize := fs.Int("window", sample.DefaultWindowSize, "window size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := sample.Generate(sample.Options{OutDir: *out, Seed: *seed, FrameCount: *frameCount, WindowSize: *windowSize})
	if err != nil {
		return err
	}
	return writeJSONResult(map[string]any{"ok": true, "sample": result})
}

func commandValidate(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("validate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	validation, err := service.ValidateTruth(ctx)
	if err != nil {
		return err
	}
	if err := writeReport(*work, "validate-report.json", validation); err != nil {
		return err
	}
	result := map[string]any{
		"ok": validation.OK, "validation": validation,
		"model": service.Engine().Model(), "windows": service.Engine().Windows(),
		"checkpoint_recovery": service.Engine().Recovery(),
	}
	if err := writeJSONResult(result); err != nil {
		return err
	}
	if !validation.OK {
		return fmt.Errorf("truth validation failed with %d errors", len(validation.Errors))
	}
	return nil
}

func commandFull(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("full")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	start := time.Now()
	snapshot, err := service.RunFull(ctx)
	if err != nil {
		return err
	}
	report := map[string]any{
		"ok": snapshot.Committed == snapshot.Total, "elapsed_ms": time.Since(start).Milliseconds(),
		"snapshot": snapshot,
	}
	if err := writeReport(*work, "full-report.json", report); err != nil {
		return err
	}
	if err := writeJSONResult(report); err != nil {
		return err
	}
	if snapshot.Committed != snapshot.Total {
		return fmt.Errorf("only %d/%d windows committed", snapshot.Committed, snapshot.Total)
	}
	return nil
}

func commandIncremental(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("incremental")
	kind := fs.String("kind", string(pipeline.EditLayerParameter), "layer_parameter, layer_visibility or time_base")
	layer := fs.String("layer", "prop", "layer id")
	opacity := fs.Float64("opacity", -1, "new opacity")
	alpha := fs.Float64("alpha", -1, "new alpha")
	visible := fs.String("visible", "", "true or false")
	color := fs.String("color", "", "comma-separated linear/sRGB triple")
	periodNum := fs.Int64("period-num", 0, "time-base period numerator")
	periodDen := fs.Int64("period-den", 0, "time-base period denominator")
	unit := fs.String("unit", "", "time-base unit: second or tick")
	if err := fs.Parse(args); err != nil {
		return err
	}
	edit := pipeline.EditRequest{Kind: pipeline.EditKind(*kind), LayerID: *layer}
	if *opacity >= 0 {
		edit.Opacity = opacity
	}
	if *alpha >= 0 {
		edit.Alpha = alpha
	}
	if *visible != "" {
		value := *visible == "true"
		if *visible != "true" && *visible != "false" {
			return fmt.Errorf("--visible must be true or false")
		}
		edit.Visible = &value
	}
	if *color != "" {
		var parsed [3]float64
		if _, err := fmt.Sscanf(*color, "%g,%g,%g", &parsed[0], &parsed[1], &parsed[2]); err != nil {
			return fmt.Errorf("invalid --color: %w", err)
		}
		edit.Color = &parsed
	}
	if *periodNum > 0 || *periodDen > 0 || *unit != "" {
		edit.TimeBase = &pipeline.TimeBase{Unit: *unit, FramePeriod: pipeline.NewRational(*periodNum, *periodDen)}
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	start := time.Now()
	list, err := service.ApplyIncremental(ctx, edit)
	if err != nil {
		return err
	}
	snapshot := service.Snapshot()
	report := map[string]any{
		"ok": snapshot.Committed == snapshot.Total, "elapsed_ms": time.Since(start).Milliseconds(),
		"invalidation": list, "snapshot": snapshot,
	}
	if err := writeReport(*work, "incremental-report.json", report); err != nil {
		return err
	}
	return writeJSONResult(report)
}

func commandVerifyIncremental(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("verify-incremental")
	layer := fs.String("layer", "prop", "layer id")
	opacity := fs.Float64("opacity", 0.61, "incremental opacity edit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	verification, err := service.VerifyIncremental(ctx, pipeline.EditRequest{
		Kind: pipeline.EditLayerParameter, LayerID: *layer, Opacity: opacity,
	})
	if err != nil {
		return err
	}
	report := map[string]any{"ok": verification.Match, "verification": verification}
	if err := writeReport(*work, "incremental-verification.json", report); err != nil {
		return err
	}
	if err := writeJSONResult(report); err != nil {
		return err
	}
	if !verification.Match {
		return errors.New(verification.Comparison.First.Error())
	}
	return nil
}

func commandExport(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("export")
	version := fs.Int("version", 1, "target format version 1, 2 or 3")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	verification, err := service.ExportAndVerify(ctx, *version)
	if err != nil {
		return err
	}
	report := map[string]any{"ok": verification.RoundTripOK && verification.Deterministic, "verification": verification}
	if err := writeReport(*work, fmt.Sprintf("export-v%d-report.json", *version), report); err != nil {
		return err
	}
	return writeJSONResult(report)
}

func commandInspect(args []string) error {
	fs, work, workers, maxBytes := commonFlags("inspect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	return writeJSONResult(map[string]any{"ok": true, "snapshot": service.Snapshot()})
}

func commandResume(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("resume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	snapshot := service.Snapshot()
	restored := make([]int, 0, len(snapshot.Recovery.Restored))
	for _, window := range snapshot.Recovery.Restored {
		restored = append(restored, window.WindowIndex)
	}
	report := map[string]any{
		"ok": true, "restored_windows": restored, "recovery": snapshot.Recovery,
		"committed": snapshot.Committed, "total": snapshot.Total,
		"state_matrix": mapWindowStates(snapshot.Windows),
	}
	if err := writeReport(*work, "resume-report.json", report); err != nil {
		return err
	}
	return writeJSONResult(report)
}

func commandPNG(args []string) error {
	fs, work, workers, maxBytes := commonFlags("png")
	window := fs.Int("window", 0, "window index")
	frame := fs.Int("frame", -1, "frame number (default first in window)")
	out := fs.String("out", "", "output PNG path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()
	result, err := service.Engine().ReadWindow(*window)
	if err != nil {
		return err
	}
	if len(result.Frames) == 0 {
		return fmt.Errorf("no frames")
	}
	selected := result.Frames[0]
	if *frame >= 0 {
		selected = nil
		for _, candidate := range result.Frames {
			if candidate.Frame == *frame {
				selected = candidate
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("frame %d not in window %d", *frame, *window)
		}
	}
	data, err := selected.PNG()
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(*work, "exports", fmt.Sprintf("frame-%06d.png", selected.Frame))
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	return writeJSONResult(map[string]any{"ok": true, "path": *out, "frame": selected.Frame, "hash": pipeline.FrameHash(selected)})
}

func mapWindowStates(windows []pipeline.Window) []string {
	out := make([]string, len(windows))
	for i, window := range windows {
		state := string(window.State)
		if window.Invalidated {
			state += ":invalid"
		} else if window.Reused {
			state += ":reused"
		}
		out[i] = state
	}
	return out
}

func writeReport(work, name string, value any) error {
	path := filepath.Join(work, "reports", name)
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func writeJSONResult(value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(raw, '\n'))
	return err
}

func writeError(err error) {
	payload := map[string]any{"ok": false, "error": map[string]any{"code": "E_PARA", "message": err.Error()}}
	var pipelineErr *pipeline.Error
	if errors.As(err, &pipelineErr) {
		payload["error"] = pipelineErr
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	os.Stderr.Write(append(raw, '\n'))
}
