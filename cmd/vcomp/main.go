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
	"strconv"
	"strings"

	"virtualstudio/engine"
	"virtualstudio/formats"
	"virtualstudio/model"
	"virtualstudio/sample"
	"virtualstudio/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "load":
		err = cmdLoad(os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "export":
		err = cmdExport(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "workbench":
		err = cmdWorkbench(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var detailed *model.Error
		if errors.As(err, &detailed) {
			fmt.Fprintf(os.Stderr, "code=%s path=%s layer=%s\n", detailed.Code, detailed.Path, detailed.LayerID)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `vcomp - deterministic virtual-production layer stack compositor

Commands:
  gen       generate deterministic v1/v2/v3 samples, commands and truth
  load      load one version and print its canonical stack
  run       run realtime/incremental/full composition with SQLite checkpoints
  export    export descriptor and PNG frames with E_LOSSY report
  verify    run the full local acceptance matrix and print findings
  workbench interactive diagnostics (use --demo for a scripted terminal run)

Use "vcomp <command> -h" for command flags.
`)
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", sample.Seed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := sample.Generate(*out, *seed)
	if err != nil {
		return err
	}
	printJSON(manifest)
	return nil
}

func cmdLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	scenePath := fs.String("scene", "", "scene descriptor path")
	commandsPath := fs.String("commands", "", "optional commands path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scenePath == "" {
		return fmt.Errorf("--scene is required")
	}
	scene, err := formats.LoadScene(*scenePath)
	if err != nil {
		return err
	}
	out := struct {
		SourceVersion   string        `json:"source_version"`
		Dimensions      [3]int        `json:"dimensions"`
		FrameCount      int           `json:"frame_count"`
		FrameDurationNS int64         `json:"frame_duration_ns"`
		OrderExpression string        `json:"order_expression"`
		Layers          []layerReport `json:"layers"`
		CommandsChecked bool          `json:"commands_checked"`
	}{SourceVersion: scene.SourceVersion, Dimensions: [3]int{scene.Width, scene.Height, scene.Channels},
		FrameCount: scene.FrameCount, FrameDurationNS: scene.FrameDurationNS, OrderExpression: scene.OrderExpr}
	for _, layer := range scene.Layers {
		out.Layers = append(out.Layers, layerReport{
			ID: layer.ID, Name: layer.Name, Visible: layer.Visible, Opacity: layer.Opacity,
			SourceAlphaMode: string(layer.SourceAlphaMode), PixelHash: model.HashBytes(layer.Pixels),
		})
	}
	if *commandsPath != "" {
		if _, err := formats.LoadCommands(*commandsPath, scene); err != nil {
			return err
		}
		out.CommandsChecked = true
	}
	printJSON(out)
	return nil
}

type layerReport struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Visible         bool    `json:"visible"`
	Opacity         float64 `json:"opacity"`
	SourceAlphaMode string  `json:"source_alpha_mode"`
	PixelHash       string  `json:"pixel_hash"`
}

type loadedProject struct {
	Store    *store.Store
	Scene    *model.Scene
	Commands *model.CommandSet
	DataDir  string
	Version  string
}

func loadData(dataDir, version, scenePath, commandsPath string) (*loadedProject, error) {
	if version == "" {
		version = "v1"
	}
	if !supportedVersion(version) {
		return nil, model.NewError(model.CodeVersion, "version", fmt.Sprintf("unsupported version %q", version))
	}
	if scenePath == "" {
		scenePath = filepath.Join(dataDir, version, "scene.json")
	}
	if commandsPath == "" {
		commandsPath = filepath.Join(dataDir, "commands.json")
	}
	scene, err := formats.LoadScene(scenePath)
	if err != nil {
		return nil, err
	}
	commands, err := formats.LoadCommands(commandsPath, scene)
	if err != nil {
		return nil, err
	}
	return &loadedProject{Scene: scene, Commands: commands, DataDir: dataDir, Version: version}, nil
}

func supportedVersion(version string) bool {
	for _, v := range formats.SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

func openWorkspace(ctx context.Context, dataDir, version, scenePath, commandsPath, dbPath, projectID, target string, windowFrames int) (*loadedProject, *engine.Workspace, error) {
	lp, err := loadData(dataDir, version, scenePath, commandsPath)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	lp.Store = st
	ws, err := engine.OpenWorkspace(ctx, engine.WorkspaceOptions{
		Store: st, ProjectID: projectID, Scene: lp.Scene, Commands: lp.Commands,
		WindowFrames: windowFrames, TargetFormat: target,
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return lp, ws, nil
}

type runSummary struct {
	Mode                    string               `json:"mode"`
	ProjectID               string               `json:"project_id"`
	SourceVersion           string               `json:"source_version"`
	FrameCount              int                  `json:"frame_count"`
	WindowSize              int                  `json:"window_size"`
	Progress                engine.Progress      `json:"progress"`
	CommitSequence          []int                `json:"commit_sequence"`
	ReusedWindows           []int                `json:"reused_windows"`
	RecomputedWindows       []int                `json:"recomputed_windows"`
	FailedWindows           map[int]string       `json:"failed_windows,omitempty"`
	StateMatrix             string               `json:"state_matrix"`
	CacheInvalidated        bool                 `json:"cache_invalidated"`
	CacheInvalidationReason string               `json:"cache_invalidation_reason,omitempty"`
	BufferEvents            []engine.BufferEvent `json:"buffer_events,omitempty"`
	Suspended               bool                 `json:"suspended"`
	Canceled                bool                 `json:"canceled"`
}

func summarize(result *engine.RunResult) runSummary {
	return runSummary{
		Mode: result.Mode, ProjectID: result.ProjectID, SourceVersion: result.SourceVersion,
		FrameCount: len(result.Frames), WindowSize: result.WindowSize, Progress: result.Progress,
		CommitSequence: result.CommitSequence, ReusedWindows: result.ReusedWindows,
		RecomputedWindows: result.RecomputedWindows, FailedWindows: result.FailedWindows,
		StateMatrix:      stateMatrix(result.Windows, result.Progress.Total),
		CacheInvalidated: result.CacheInvalidated, CacheInvalidationReason: result.CacheInvalidationReason,
		BufferEvents: result.BufferEvents, Suspended: result.Suspended, Canceled: result.Canceled,
	}
}

func stateMatrix(windows []engine.WindowRun, total int) string {
	states := make([]string, total)
	for i := range states {
		states[i] = "."
	}
	for _, win := range windows {
		switch win.State {
		case engine.StateCommitted:
			states[win.Index] = "K"
		case engine.StateCompositing:
			states[win.Index] = "C"
		case engine.StateFailed:
			states[win.Index] = "F"
		case engine.StatePending:
			states[win.Index] = "P"
		}
	}
	return strings.Join(states, " ")
}

func writeResultFrames(dir string, frames []model.Frame) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, frame := range frames {
		data, err := composeFramePNG(frame)
		if err != nil {
			return err
		}
		name := filepath.Join(dir, fmt.Sprintf("frame_%04d.png", frame.Index))
		if err := os.WriteFile(name, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected 0/1 or true/false")
	}
}

func parseInt(raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", raw)
	}
	return v, nil
}
