package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"virtualstudio/compose"
	"virtualstudio/engine"
	"virtualstudio/model"
)

type workbench struct {
	ctx      context.Context
	ws       *engine.Workspace
	result   *engine.RunResult
	previous *engine.RunResult
	frame    int
	outDir   string
	workers  int
	lastDiff string
}

func cmdWorkbench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("workbench", flag.ContinueOnError)
	dataDir := fs.String("data", "samples", "sample data directory")
	version := fs.String("version", "v1", "source version")
	dbPath := fs.String("db", filepath.Join("artifacts", "workbench.db"), "SQLite database")
	projectID := fs.String("project", "workbench", "project id")
	target := fs.String("target", "", "target export format (empty = preserve or v3)")
	outDir := fs.String("out", filepath.Join("artifacts", "workbench"), "diagnostic output directory")
	workers := fs.Int("workers", 3, "maximum parallel windows")
	demo := fs.Bool("demo", false, "run a scripted diagnostic session and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, ws, err := openWorkspace(ctx, *dataDir, *version, "", "", *dbPath, *projectID, *target, 0)
	if err != nil {
		return err
	}
	defer ws.Store().Close()
	wb := &workbench{ctx: ctx, ws: ws, outDir: *outDir, workers: *workers, frame: 0}
	if err := os.MkdirAll(wb.outDir, 0o755); err != nil {
		return err
	}
	fmt.Printf("vcomp workbench | source=%s project=%s target=%s\n", ws.Scene().SourceVersion, ws.ProjectID(), ws.TargetFormat())
	if err := wb.recompute(false); err != nil {
		return err
	}
	if *demo {
		script := []string{
			"matrix", "stack", "frame 4", "opacity prop 0.55", "visible character 0",
			"frame 10", "diff", "matrix", "export v1", "quit",
		}
		for _, line := range script {
			fmt.Println(">", line)
			stop, err := wb.execute(line)
			if err != nil {
				fmt.Println("error:", err)
			}
			if stop {
				break
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("vcomp> ")
		if !scanner.Scan() {
			break
		}
		stop, err := wb.execute(strings.TrimSpace(scanner.Text()))
		if err != nil {
			fmt.Println("error:", err)
		}
		if stop {
			break
		}
	}
	return scanner.Err()
}

func (w *workbench) execute(line string) (bool, error) {
	if line == "" {
		return false, nil
	}
	fields := strings.Fields(line)
	switch strings.ToLower(fields[0]) {
	case "quit", "exit", "q":
		return true, nil
	case "help", "?":
		w.help(os.Stdout)
	case "stack":
		w.printStack()
	case "matrix":
		w.printMatrix()
	case "frame":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: frame <number>")
		}
		n, err := parseInt(fields[1])
		if err != nil {
			return false, err
		}
		if n < 0 || n >= w.ws.Scene().FrameCount {
			return false, fmt.Errorf("frame must be in [0,%d)", w.ws.Scene().FrameCount)
		}
		w.frame = n
		w.printFrame()
	case "diff":
		if err := w.diff(); err != nil {
			return false, err
		}
	case "opacity":
		if len(fields) != 3 {
			return false, fmt.Errorf("usage: opacity <layer> <0..1>")
		}
		value, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return false, fmt.Errorf("invalid opacity")
		}
		inv, err := w.ws.EditLayer(fields[1], nil, &value)
		if err != nil {
			return false, err
		}
		w.printInvalidation(inv)
		return false, w.recompute(false)
	case "visible":
		if len(fields) != 3 {
			return false, fmt.Errorf("usage: visible <layer> <0|1>")
		}
		value, err := parseBool(fields[2])
		if err != nil {
			return false, err
		}
		inv, err := w.ws.EditLayer(fields[1], &value, nil)
		if err != nil {
			return false, err
		}
		w.printInvalidation(inv)
		return false, w.recompute(false)
	case "timebase":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: timebase <frame_duration_ns>")
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return false, fmt.Errorf("invalid nanoseconds")
		}
		inv, err := w.ws.SetFrameDurationNS(value)
		if err != nil {
			return false, err
		}
		w.printInvalidation(inv)
		return false, w.recompute(false)
	case "recompute":
		return false, w.recompute(false)
	case "full":
		return false, w.recompute(true)
	case "export":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: export <v1|v2|v3>")
		}
		return false, w.export(fields[1])
	case "target":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: target <v1|v2|v3>")
		}
		inv, err := w.ws.SetTargetFormat(fields[1])
		if err != nil {
			return false, err
		}
		w.printInvalidation(inv)
		fmt.Println("target format is now", w.ws.TargetFormat())
		return false, w.recompute(false)
	case "compare":
		if w.previous == nil || w.result == nil {
			return false, fmt.Errorf("no previous run to compare")
		}
		if err := compareRunResults(w.previous, w.result); err != nil {
			fmt.Println("MISMATCH:", err)
		} else {
			fmt.Println("previous and current runs are pixel-identical")
		}
	default:
		return false, fmt.Errorf("unknown command %q (use help)", fields[0])
	}
	return false, nil
}

func (w *workbench) help(out io.Writer) {
	fmt.Fprintln(out, `commands:
  stack                       show canonical layer stack and source version
  matrix                      show window state matrix
  frame <n>                   render ASCII preview and sample statistics
  diff                        write/display heatmap versus previous run
  opacity <layer> <0..1>      edit opacity and incrementally recompute
  visible <layer> <0|1>       edit visibility and incrementally recompute
  timebase <ns>               edit frame duration and invalidate timeline
  recompute                   rerun with persistent cache reuse
  full                        discard cache and recompute everything
  export <v1|v2|v3>           export descriptor/frames/E_LOSSY report
  target <v1|v2|v3>           change export target and invalidate export phases
  compare                     compare previous/current frames, stats and commits
  quit`)
}

func (w *workbench) recompute(forceFull bool) error {
	previous := w.result
	result, err := w.ws.Run(w.ctx, engine.RunOptions{
		Mode: "realtime", MaxWorkers: w.workers, ForceFull: forceFull, MaxRetries: 1,
	})
	if err != nil {
		return err
	}
	w.previous = previous
	w.result = result
	fmt.Printf("run mode=%s progress=%d/%d commits=%v reused=%v recomputed=%v suspended=%v\n",
		result.Mode, result.Progress.Committed, result.Progress.Total, result.CommitSequence,
		result.ReusedWindows, result.RecomputedWindows, result.Suspended)
	if result.CacheInvalidated {
		fmt.Println("cache invalidated:", result.CacheInvalidationReason)
	}
	if len(result.BufferEvents) > 0 {
		fmt.Println("buffer queues:", len(result.BufferEvents), "reason:", result.BufferEvents[0].Reason)
	}
	if len(result.FailedWindows) > 0 {
		fmt.Println("failed windows:", result.FailedWindows)
	}
	w.printMatrix()
	return nil
}

func (w *workbench) printStack() {
	scene := w.ws.Scene()
	fmt.Printf("source=%s dimensions=%dx%dx%d frames=%d duration_ns=%d order=%s\n",
		scene.SourceVersion, scene.Width, scene.Height, scene.Channels, scene.FrameCount, scene.FrameDurationNS, scene.OrderExpr)
	fmt.Println("idx id          visible opacity source_alpha pixels_sha256")
	for i, layer := range scene.Layers {
		fmt.Printf("%3d %-11s %-7v %-7.2f %-12s %s\n", i, layer.ID, layer.Visible, layer.Opacity,
			layer.SourceAlphaMode, model.HashBytes(layer.Pixels)[:16])
	}
}

func (w *workbench) printMatrix() {
	if w.result == nil {
		return
	}
	fmt.Printf("window states (%d/%d committed): %s   [P pending C compositing K committed F failed]\n",
		w.result.Progress.Committed, w.result.Progress.Total, matrixFor(w.result))
	for _, win := range w.result.Windows {
		fmt.Printf("  window %d frames %d..%d state=%s reused=%v", win.Index, win.Start, win.End, win.State, win.Reused)
		if win.Error != "" {
			fmt.Printf(" error=%s", win.Error)
		}
		fmt.Println()
	}
}

func (w *workbench) printFrame() {
	if w.result == nil {
		return
	}
	frame, ok := findFrame(w.result.Frames, w.frame)
	if !ok {
		fmt.Println("frame not committed")
		return
	}
	fmt.Printf("frame %d time_ns=%d hash=%s visible=%v\n", frame.Index, frame.TimeNS, frame.Hash[:16], frame.Stats.VisibleLayers)
	fmt.Print(compose.ASCII(frame, 72, 20))
	for _, sample := range frame.Stats.Samples {
		fmt.Printf("  sample (%d,%d) rgba=%v\n", sample.X, sample.Y, sample.RGBA)
	}
}

func (w *workbench) diff() error {
	if w.previous == nil || w.result == nil {
		return fmt.Errorf("need a previous and current run")
	}
	a, okA := findFrame(w.previous.Frames, w.frame)
	b, okB := findFrame(w.result.Frames, w.frame)
	if !okA || !okB {
		return fmt.Errorf("frame %d not available in both runs", w.frame)
	}
	hm, err := compose.DiffHeatmap(a, b)
	if err != nil {
		return err
	}
	path := filepath.Join(w.outDir, fmt.Sprintf("diff_frame_%04d.png", w.frame))
	if err := os.WriteFile(path, hm.PNG, 0o644); err != nil {
		return err
	}
	w.lastDiff = path
	fmt.Printf("diff frame %d: %s\n", w.frame, compose.DescribeDiff(hm))
	fmt.Print(compose.ASCIIHeatmap(hm, 72, 20))
	fmt.Println("heatmap:", path)
	return nil
}

func (w *workbench) export(target string) error {
	if w.result == nil {
		return fmt.Errorf("no composed frames")
	}
	dir := filepath.Join(w.outDir, "export-"+target)
	manifest, err := w.ws.Export(w.ctx, dir, target, w.result.Frames)
	if err != nil {
		return err
	}
	fmt.Printf("exported target=%s descriptor=%s frames=%d digest=%s\n", manifest.TargetVersion, manifest.Descriptor, len(manifest.Frames), manifest.FrameDigest[:16])
	for _, warning := range manifest.Warnings {
		fmt.Printf("  %s path=%s original=%s resolution=%s\n", warning.Code, warning.Path, warning.Original, warning.Resolution)
	}
	return nil
}

func (w *workbench) printInvalidation(inv *engine.Invalidation) {
	if inv == nil {
		return
	}
	fmt.Printf("invalidated windows=%v layers=%v exports=%v\n", invalidationWindows(inv), inv.LayerIDs, inv.Exports)
}

func findFrame(frames []model.Frame, index int) (model.Frame, bool) {
	for _, frame := range frames {
		if frame.Index == index {
			return frame, true
		}
	}
	return model.Frame{}, false
}
