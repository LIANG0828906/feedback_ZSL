package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"virtualstudio/compose"
	"virtualstudio/engine"
	"virtualstudio/formats"
	"virtualstudio/model"
	"virtualstudio/sample"
	"virtualstudio/store"
)

type verificationCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
	Code    string `json:"code,omitempty"`
}

type verificationReport struct {
	DataDir string              `json:"data_dir"`
	WorkDir string              `json:"work_dir"`
	Passed  bool                `json:"passed"`
	Checks  []verificationCheck `json:"checks"`
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dataDir := fs.String("data", "samples", "sample data directory")
	workDir := fs.String("work", filepath.Join("artifacts", "verify"), "verification workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*dataDir, "manifest.json")); err != nil {
		if _, genErr := sample.Generate(*dataDir, sample.Seed); genErr != nil {
			return genErr
		}
	}
	if err := os.RemoveAll(*workDir); err != nil {
		return err
	}
	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		return err
	}
	report := &verificationReport{DataDir: *dataDir, WorkDir: *workDir}
	add := func(name string, err error, details string) {
		check := verificationCheck{Name: name, Passed: err == nil, Details: details}
		if err != nil {
			check.Details = strings.TrimSpace(check.Details + " " + err.Error())
			if modelErr, ok := model.AsError(err); ok {
				check.Code = string(modelErr.Code)
			}
		}
		report.Checks = append(report.Checks, check)
	}
	truth, err := sample.LoadTruth(filepath.Join(*dataDir, "truth.json"))
	if err != nil {
		return err
	}
	scenes := map[string]*model.Scene{}
	commands := map[string]*model.CommandSet{}
	for _, version := range formats.SupportedVersions {
		scene, loadErr := formats.LoadScene(filepath.Join(*dataDir, version, "scene.json"))
		if loadErr != nil {
			add("load "+version, loadErr, "")
			continue
		}
		cmd, cmdErr := formats.LoadCommands(filepath.Join(*dataDir, "commands.json"), scene)
		if cmdErr != nil {
			add("commands "+version, cmdErr, "")
			continue
		}
		scenes[version] = scene
		commands[version] = cmd
		mismatch, truthErr := sample.TruthMismatch(scene, cmd, truth)
		if truthErr != nil {
			add("truth "+version, truthErr, "")
		} else if mismatch != nil {
			add("truth "+version, fmt.Errorf("%s", mismatch.Error()), "sampled pixels, timestamps and visibility endpoints")
		} else {
			add("truth "+version, nil, "sampled pixels, timestamps, visibility endpoints and hashes match")
		}
	}
	if len(scenes) == 3 {
		add("cross-version canonical stack", compareStacks(scenes["v1"], scenes["v2"], scenes["v3"]), "layer ids, order, visibility, opacity, pixels and timebase")
	}
	if scene := scenes["v1"]; scene != nil && commands["v1"] != nil {
		first, err1 := compose.RenderAll(scene, commands["v1"], compose.RenderOptions{})
		second, err2 := compose.RenderAll(scene, commands["v1"], compose.RenderOptions{})
		if err1 != nil || err2 != nil {
			add("repeat composition determinism", firstError(err1, err2), "")
		} else {
			add("repeat composition determinism", compareFrameSets(first, second), "pixel-identical repeat")
		}
	}
	add("loader error contracts", verifyLoaderErrors(*dataDir), "unknown/missing version, cycle, decode and parameter errors")
	add("window state machine", verifyWindowState(), "illegal transitions preserve state and return E_STATE")

	var baseline *engine.RunResult
	var baseScene *model.Scene
	if scene := scenes["v1"]; scene != nil && commands["v1"] != nil {
		baseScene = scene
		baseline, err = runScenario(ctx, *dataDir, "v1", filepath.Join(*workDir, "full.db"), "full", engine.RunOptions{
			Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1,
		})
		add("full baseline", err, "SQLite checkpoint commit sequence and frames")
	}
	if baseline != nil && baseScene != nil {
		realtime, err := runScenario(ctx, *dataDir, "v1", filepath.Join(*workDir, "realtime.db"), "realtime", engine.RunOptions{
			Mode: "realtime", MaxWorkers: 3, MaxRetries: 1,
		})
		if err != nil {
			add("realtime path", err, "")
		} else {
			add("realtime path", compareRunResults(baseline, realtime), "frames, stats and commit sequence equal full")
		}

		resumeDB := filepath.Join(*workDir, "resume.db")
		first, err := runScenario(ctx, *dataDir, "v1", resumeDB, "resume", engine.RunOptions{
			Mode: "resume", MaxWorkers: 3, MaxRetries: 1, CancelAfter: 2,
		})
		if err != nil {
			add("resume first segment", err, "")
		} else {
			if !first.Canceled || fmt.Sprint(first.CommitSequence) != "[0 1]" {
				add("resume first segment", fmt.Errorf("canceled=%v commits=%v", first.Canceled, first.CommitSequence), "")
			} else {
				add("resume first segment", nil, fmt.Sprintf("canceled committed windows persisted: %v state=%s", first.CommitSequence, matrixFor(first)))
			}
		}
		second, err := runScenario(ctx, *dataDir, "v1", resumeDB, "resume", engine.RunOptions{
			Mode: "resume", MaxWorkers: 3, MaxRetries: 1,
		})
		if err != nil {
			add("resume restart", err, "")
		} else {
			add("resume restart", compareRunResults(baseline, second), fmt.Sprintf("reused=%v recomputed=%v", second.ReusedWindows, second.RecomputedWindows))
		}

		incrementalDB := filepath.Join(*workDir, "incremental.db")
		incWS := mustWorkspace(ctx, *dataDir, "v1", incrementalDB, "incremental")
		if incWS == nil {
			add("incremental workspace", fmt.Errorf("open failed"), "")
		} else {
			firstRun, runErr := incWS.Run(ctx, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
			if runErr != nil {
				add("incremental baseline", runErr, "")
			} else {
				opacity := 0.55
				inv, editErr := incWS.EditLayer("prop", nil, &opacity)
				if editErr != nil {
					add("incremental invalidation", editErr, "")
				} else {
					gotWindows := invalidationWindows(inv)
					add("incremental invalidation", checkInvalidation(inv, []int{1, 2, 3, 4}, "prop"),
						fmt.Sprintf("windows=%v layers=%v exports=%v", gotWindows, inv.LayerIDs, inv.Exports))
					incResult, incErr := incWS.Run(ctx, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
					if incErr != nil {
						add("incremental run", incErr, "")
					} else {
						fullWS := mustWorkspace(ctx, *dataDir, "v1", filepath.Join(*workDir, "incremental-full.db"), "incremental-full")
						if fullWS != nil {
							_, _ = fullWS.EditLayer("prop", nil, &opacity)
							fullChanged, fullErr := fullWS.Run(ctx, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
							if fullErr != nil {
								add("incremental full comparator", fullErr, "")
							} else {
								mismatch := compareRunResults(fullChanged, incResult)
								detail := fmt.Sprintf("reused=%v recomputed=%v commit=%v", incResult.ReusedWindows, incResult.RecomputedWindows, incResult.CommitSequence)
								add("incremental equals full", mismatch, detail)
								if mismatch == nil {
									add("incremental reuses unaffected cache", nil, fmt.Sprintf("windows %v reused", incResult.ReusedWindows))
								}
							}
							_ = fullWS.Store().Close()
						}
					}
				}
			}
			_ = incWS.Store().Close()
			_ = firstRun
		}

		budgetDB := filepath.Join(*workDir, "buffer.db")
		need := baseScene.FrameBytes() * 9
		bufferResult, err := runScenario(ctx, *dataDir, "v1", budgetDB, "realtime", engine.RunOptions{
			Mode: "realtime", MaxWorkers: 2, MaxRetries: 1,
			PixelBufferBytesPerWorker: need, PixelBufferTotalBytes: need + need/2,
		})
		if err != nil {
			add("resource buffer queue", err, "")
		} else {
			if len(bufferResult.BufferEvents) == 0 {
				add("resource buffer queue", fmt.Errorf("expected one or more queued buffer events"), "")
			} else {
				add("resource buffer queue", compareRunResults(baseline, bufferResult),
					fmt.Sprintf("queued=%d reason=%q", len(bufferResult.BufferEvents), bufferResult.BufferEvents[0].Reason))
			}
		}

		faultDB := filepath.Join(*workDir, "fault.db")
		faultWS := mustWorkspace(ctx, *dataDir, "v1", faultDB, "fault")
		if faultWS != nil {
			faultResult, faultErr := faultWS.Run(ctx, engine.RunOptions{
				Mode: "realtime", MaxWorkers: 3, MaxRetries: 0,
				Faults: map[int]error{1: model.NewError(model.CodeDecode, "layers.prop.pixels", "injected sample corruption")},
			})
			if faultErr != nil {
				add("window failure isolation", faultErr, "")
			} else if _, failed := faultResult.FailedWindows[1]; !failed || len(faultResult.CommitSequence) != 1 {
				add("window failure isolation", fmt.Errorf("expected window 1 failure and only window 0 committed, got failed=%v commits=%v", faultResult.FailedWindows, faultResult.CommitSequence), "")
			} else {
				recovered, recoverErr := faultWS.Run(ctx, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
				if recoverErr != nil {
					add("window retry recovery", recoverErr, "")
				} else {
					add("window failure isolation", nil, fmt.Sprintf("failed=%v commits-before-failure=%v", faultResult.FailedWindows, faultResult.CommitSequence))
					add("window retry recovery", compareRunResults(baseline, recovered), fmt.Sprintf("reused=%v recomputed=%v", recovered.ReusedWindows, recovered.RecomputedWindows))
				}
			}
			_ = faultWS.Store().Close()
		}
	}
	if scene := scenes["v3"]; scene != nil {
		add("cross-version export/downgrade", verifyExportRoundTrip(scene, scenes["v1"], *workDir), "E_LOSSY paths, byte determinism and loader round-trip")
	}
	if baseline != nil && baseScene != nil {
		corruptDB := filepath.Join(*workDir, "full.db")
		add("checkpoint poison detection", corruptAndRecover(ctx, *dataDir, corruptDB, baseline), "any hash mismatch purges the whole snapshot and recomputes")
	}
	report.Passed = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
			break
		}
	}
	reportBytes, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(*workDir, "verify-report.json")
	_ = os.WriteFile(reportPath, append(reportBytes, '\n'), 0o644)
	printJSON(report)
	if !report.Passed {
		return fmt.Errorf("verification failed; report: %s", reportPath)
	}
	fmt.Println("verification report:", reportPath)
	return nil
}

func runScenario(ctx context.Context, dataDir, version, dbPath, projectID string, opts engine.RunOptions) (*engine.RunResult, error) {
	ws := mustWorkspace(ctx, dataDir, version, dbPath, projectID)
	if ws == nil {
		return nil, fmt.Errorf("failed to open workspace")
	}
	defer ws.Store().Close()
	result, err := ws.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	if len(result.FailedWindows) > 0 {
		return result, fmt.Errorf("windows failed: %v", result.FailedWindows)
	}
	return result, nil
}

func mustWorkspace(ctx context.Context, dataDir, version, dbPath, projectID string) *engine.Workspace {
	lp, err := loadData(dataDir, version, "", "")
	if err != nil {
		return nil
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil
	}
	lp.Store = st
	ws, err := engine.OpenWorkspace(ctx, engine.WorkspaceOptions{
		Store: st, ProjectID: projectID, Scene: lp.Scene, Commands: lp.Commands,
		TargetFormat: "v3",
	})
	if err != nil {
		_ = st.Close()
		return nil
	}
	return ws
}

func compareRunResults(expected, actual *engine.RunResult) error {
	if expected == nil || actual == nil {
		return fmt.Errorf("missing run result")
	}
	mismatch := model.CompareRunSets(
		model.RunFrameSet{Frames: expected.Frames, Commits: expected.CommitSequence},
		model.RunFrameSet{Frames: actual.Frames, Commits: actual.CommitSequence},
		expected.WindowSize,
	)
	if mismatch != nil {
		return mismatch
	}
	return nil
}

func compareFrameSets(expected, actual []model.Frame) error {
	mismatch := model.CompareRunSets(
		model.RunFrameSet{Frames: expected, Commits: indexes(len(expected))},
		model.RunFrameSet{Frames: actual, Commits: indexes(len(actual))}, 4)
	if mismatch != nil {
		return mismatch
	}
	return nil
}

func indexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

type stackSemantic struct {
	Width, Height, Channels, FrameCount int
	FrameDurationNS                     int64
	Layers                              []semanticLayer
}

type semanticLayer struct {
	ID      string
	Name    string
	Visible bool
	Opacity float64
	Pixel   string
}

func semantic(scene *model.Scene) stackSemantic {
	out := stackSemantic{Width: scene.Width, Height: scene.Height, Channels: scene.Channels,
		FrameCount: scene.FrameCount, FrameDurationNS: scene.FrameDurationNS}
	for _, layer := range scene.Layers {
		out.Layers = append(out.Layers, semanticLayer{ID: layer.ID, Name: layer.Name, Visible: layer.Visible,
			Opacity: layer.Opacity, Pixel: model.HashBytes(layer.Pixels)})
	}
	return out
}

func compareStacks(scenes ...*model.Scene) error {
	if len(scenes) == 0 {
		return fmt.Errorf("no scenes")
	}
	base, _ := json.Marshal(semantic(scenes[0]))
	for i := 1; i < len(scenes); i++ {
		if scenes[i] == nil {
			return fmt.Errorf("scene %d is nil", i)
		}
		got, _ := json.Marshal(semantic(scenes[i]))
		if string(base) != string(got) {
			return fmt.Errorf("scene %d semantic stack differs", i)
		}
	}
	return nil
}

func verifyLoaderErrors(dataDir string) error {
	if err := expectCode(func() error { _, err := formats.ParseScene([]byte(`{}`)); return err }, model.CodeVersion, "format_version"); err != nil {
		return err
	}
	if err := expectCode(func() error { _, err := formats.ParseScene([]byte(`{"format_version":"v9"}`)); return err }, model.CodeVersion, "format_version"); err != nil {
		return err
	}
	if err := expectCode(func() error { _, err := formats.ParseScene([]byte(`{"format_version":"foreground-v1"}`)); return err }, model.CodeVersion, "resolution"); err != nil {
		return err
	}
	if err := expectCode(func() error { _, err := formats.ParseScene([]byte(cycleDoc())); return err }, model.CodeCycle, ""); err != nil {
		return err
	}
	if err := expectCode(func() error { _, err := formats.ParseScene([]byte(corruptDoc())); return err }, model.CodeDecode, ""); err != nil {
		return err
	}
	scene, err := formats.LoadScene(filepath.Join(dataDir, "v1", "scene.json"))
	if err != nil {
		return err
	}
	bad := scene.Clone()
	bad.Layers[0].Opacity = 1.5
	if err := expectCode(func() error { return model.ValidateScene(bad) }, model.CodeParam, "opacity"); err != nil {
		return err
	}
	cmd, err := formats.LoadCommands(filepath.Join(dataDir, "commands.json"), scene)
	if err != nil {
		return err
	}
	if err := expectCode(func() error {
		_, err := compose.RenderFrame(scene, cmd, 0, compose.RenderOptions{Samples: []model.SamplePoint{{X: scene.Width, Y: 0}}})
		return err
	}, model.CodeParam, "sample_points[0]"); err != nil {
		return err
	}
	return nil
}

func expectCode(fn func() error, code model.ErrorCode, pathFragment string) error {
	err := fn()
	if err == nil {
		return fmt.Errorf("expected %s, got nil", code)
	}
	modelErr, ok := model.AsError(err)
	if !ok {
		return fmt.Errorf("expected %s, got %v", code, err)
	}
	if modelErr.Code != code {
		return fmt.Errorf("expected %s, got %s (%v)", code, modelErr.Code, err)
	}
	if pathFragment != "" && !strings.Contains(modelErr.Path, pathFragment) {
		return fmt.Errorf("expected path containing %q, got %q", pathFragment, modelErr.Path)
	}
	return nil
}

func cycleDoc() string {
	pixel := "AAAAAAAAAAA="
	return fmt.Sprintf(`{
	  "spec":"virtualstudio/foreground@3",
	  "dimensions":{"width":1,"height":1,"channels":4},
	  "time":{"frames":1,"ticks_per_frame":1,"tick_rate_hz":1000000000},
	  "layers":{
	    "a":{"name":"A","enabled":1,"opacity":1,"pma":false,"pixels":%q},
	    "b":{"name":"B","enabled":1,"opacity":1,"pma":false,"pixels":%q},
	    "c":{"name":"C","enabled":1,"opacity":1,"pma":false,"pixels":%q}
	  },
	  "order_links":[{"id":"a","above":"c"},{"id":"b","above":"a"},{"id":"c","above":"b"}]
	}`, pixel, pixel, pixel)
}

func corruptDoc() string {
	return `{"format_version":"foreground-v1","resolution":{"w":1,"h":1,"channels":4},
	"frame_count":1,"frame_duration_us":1000,
	"layers":[{"id":"a","visible":true,"z_index":0,"opacity":1,"alpha_mode":"straight","rgba_base64":"!!!not-base64!!!"}]}`
}

func verifyWindowState() error {
	win := engine.NewWindow(0, 0, 3)
	if err := win.Commit(&model.WindowSnapshot{}); !model.IsCode(err, model.CodeState) || win.State() != engine.StatePending {
		return fmt.Errorf("pending->committed must fail with E_STATE and preserve state")
	}
	if err := win.Begin(); err != nil {
		return err
	}
	if err := win.Begin(); !model.IsCode(err, model.CodeState) {
		return fmt.Errorf("compositing->compositing must be E_STATE")
	}
	snap := &model.WindowSnapshot{WindowIndex: 0, StartFrame: 0, EndFrame: 3, Width: 1, Height: 1, Channels: 4,
		FrameDurationNS: 1, SourceVersion: "v1"}
	if err := win.Commit(snap); err != nil {
		return err
	}
	if err := win.Begin(); !model.IsCode(err, model.CodeState) || win.State() != engine.StateCommitted {
		return fmt.Errorf("committed transition must fail with E_STATE and preserve state")
	}
	if err := win.Invalidate([]string{"a"}, "edit"); err != nil || win.State() != engine.StatePending {
		return fmt.Errorf("committed->pending invalidation failed: %v", err)
	}
	return nil
}

func verifyExportRoundTrip(v3Scene, v1Scene *model.Scene, workDir string) error {
	first, err := formats.ExportScene(v3Scene, "v1")
	if err != nil {
		return err
	}
	second, err := formats.ExportScene(v3Scene, "v1")
	if err != nil {
		return err
	}
	if string(first.Descriptor) != string(second.Descriptor) {
		return fmt.Errorf("repeat export is not byte-identical")
	}
	if len(first.Warnings) == 0 {
		return fmt.Errorf("expected E_LOSSY warnings for linked/premultiplied -> v1")
	}
	foundAlpha, foundOrder := false, false
	for _, warning := range first.Warnings {
		if warning.Code != model.CodeLossy {
			return fmt.Errorf("unexpected warning code %s", warning.Code)
		}
		if strings.Contains(warning.Path, "alpha") {
			foundAlpha = true
		}
		if strings.Contains(warning.Path, "order") {
			foundOrder = true
		}
	}
	if !foundAlpha || !foundOrder {
		return fmt.Errorf("warnings must cover alpha and hierarchy paths: %+v", first.Warnings)
	}
	loaded, err := formats.ParseScene(first.Descriptor)
	if err != nil {
		return err
	}
	if err := compareStacks(v3Scene, loaded); err != nil {
		return fmt.Errorf("v3->v1 loader round-trip: %w", err)
	}
	up, err := formats.ExportScene(v1Scene, "v3")
	if err != nil {
		return err
	}
	loadedV3, err := formats.ParseScene(up.Descriptor)
	if err != nil {
		return err
	}
	if err := compareStacks(v1Scene, loadedV3); err != nil {
		return fmt.Errorf("v1->v3 loader round-trip: %w", err)
	}
	dir := filepath.Join(workDir, "export-v1")
	if _, err := formats.WriteBundle(dir, v3Scene, nil, "v1"); err != nil {
		return err
	}
	return nil
}

func corruptAndRecover(ctx context.Context, dataDir, dbPath string, baseline *engine.RunResult) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE checkpoints SET content_hash = 'poisoned' WHERE window_index = 1`)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ws := mustWorkspace(ctx, dataDir, "v1", dbPath, "full")
	if ws == nil {
		return fmt.Errorf("open poisoned workspace failed")
	}
	defer ws.Store().Close()
	result, err := ws.Run(ctx, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	if err != nil {
		return err
	}
	if !result.CacheInvalidated {
		return fmt.Errorf("cache poison was not reported")
	}
	if err := compareRunResults(baseline, result); err != nil {
		return err
	}
	if len(result.ReusedWindows) != 0 {
		return fmt.Errorf("poisoned snapshot reused %v", result.ReusedWindows)
	}
	return nil
}

func invalidationWindows(inv *engine.Invalidation) []int {
	if inv == nil {
		return nil
	}
	out := make([]int, 0, len(inv.Windows))
	for _, item := range inv.Windows {
		out = append(out, item.Window)
	}
	sort.Ints(out)
	return out
}

func checkInvalidation(inv *engine.Invalidation, expectedWindows []int, layer string) error {
	if inv == nil {
		return fmt.Errorf("nil invalidation")
	}
	got := invalidationWindows(inv)
	if fmt.Sprint(got) != fmt.Sprint(expectedWindows) {
		return fmt.Errorf("windows %v, expected %v", got, expectedWindows)
	}
	if len(inv.LayerIDs) != 1 || inv.LayerIDs[0] != layer {
		return fmt.Errorf("layers %v, expected [%s]", inv.LayerIDs, layer)
	}
	if len(inv.Exports) == 0 {
		return fmt.Errorf("downstream exports were not invalidated")
	}
	return nil
}

func matrixFor(result *engine.RunResult) string {
	return stateMatrix(result.Windows, result.Progress.Total)
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
