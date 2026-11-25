package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"lightshow"
	"lightshow/sample"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: stagectl <generate|verify> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		if err := generate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "verify":
		if err := verify(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func generate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 421337, "deterministic sample seed")
	out := fs.String("out", "artifacts/samples", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	generated, err := sample.Generate(*out, *seed)
	if err != nil {
		return err
	}
	size := map[string]int{}
	for name, data := range generated.Files {
		size[name] = len(data)
	}
	data, _ := json.MarshalIndent(map[string]any{
		"seed": *seed, "out": *out, "project_id": generated.Show.ID, "files": size,
	}, "", "  ")
	fmt.Println(string(data))
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	samples := fs.String("samples", "artifacts/samples", "sample directory")
	dbPath := fs.String("db", "artifacts/verify-checkpoints.db", "checkpoint database")
	reportPath := fs.String("json", "artifacts/verification.json", "verification report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runVerification(*samples, *dbPath, *reportPath)
}

type checkResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type verificationReport struct {
	Passed           bool              `json:"passed"`
	Seed             int64             `json:"seed"`
	AlgorithmVersion string            `json:"algorithm_version"`
	SampleDir        string            `json:"sample_dir"`
	CheckpointDB     string            `json:"checkpoint_db"`
	Checks           []checkResult     `json:"checks"`
	Metrics          map[string]any    `json:"metrics"`
	ErrorCodes       map[string]string `json:"error_codes"`
}

func runVerification(sampleDir, dbPath, reportPath string) error {
	seed := int64(421337)
	report := verificationReport{
		Seed: seed, AlgorithmVersion: lightshow.AlgorithmVersion,
		SampleDir: sampleDir, CheckpointDB: dbPath,
		Metrics: map[string]any{}, ErrorCodes: map[string]string{},
	}
	add := func(name string, err error, detail string) {
		if err != nil {
			detail = err.Error()
		}
		report.Checks = append(report.Checks, checkResult{Name: name, Pass: err == nil, Detail: detail})
	}

	truthBytes, truthErr := os.ReadFile(filepath.Join(sampleDir, "truth.json"))
	if truthErr != nil {
		generated, err := sample.Generate(sampleDir, seed)
		if err != nil {
			return err
		}
		truthBytes = generated.Files["truth.json"]
		add("generate deterministic three-version sample", nil,
			fmt.Sprintf("seed=%d, v1=%d bytes, v2=%d bytes, v3=%d bytes",
				seed, len(generated.Files["show.v1.json"]), len(generated.Files["show.v2.json"]), len(generated.Files["show.v3.json"])))
	} else {
		add("load existing deterministic three-version sample", nil, sampleDir)
	}
	var truth sample.Truth
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		return err
	}
	report.Seed = truth.Seed
	shows := map[string]*lightshow.Show{}
	schedules := map[string]*lightshow.ScheduleResult{}
	for _, version := range []lightshow.Version{lightshow.Version1, lightshow.Version2, lightshow.Version3} {
		name := version.String()
		show, err := lightshow.LoadFile(filepath.Join(sampleDir, "show."+name+".json"))
		add("load "+name, err, "source version preserved")
		if err != nil {
			continue
		}
		schedule, err := lightshow.Schedule(show)
		add("schedule "+name, err, fmt.Sprintf("%d frames", len(schedule.Frames)))
		if err != nil {
			continue
		}
		err = sample.VerifySchedule(version, schedule, truth)
		detail := fmt.Sprintf("%d frames, %d decisions, color tolerance %d",
			len(schedule.Frames), len(schedule.Decisions), lightshow.ColorTolerance(version))
		add("truth "+name, err, detail)
		shows[name] = show
		schedules[name] = schedule
		report.Metrics["version_frames_"+name] = len(schedule.Frames)
	}
	if shows["v3"] == nil {
		return fmt.Errorf("v3 sample could not be loaded")
	}
	show := shows["v3"]

	cleanDB := func(path string) error {
		for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	if err := cleanDB(dbPath); err != nil {
		return err
	}
	realtimeStore, err := lightshow.OpenStore(dbPath)
	if err != nil {
		return err
	}
	realtime, runErr := lightshow.Run(context.Background(), show, realtimeStore, lightshow.RunRealtime)
	realtimeDetail := "realtime run failed"
	if realtime != nil {
		realtimeDetail = fmt.Sprintf("%d windows committed", len(realtime.Windows))
	}
	add("realtime schedule", runErr, realtimeDetail)
	if runErr != nil {
		return runErr
	}
	auxDir, err := os.MkdirTemp("", "stagectl-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(auxDir)

	fullPath := filepath.Join(auxDir, "full.db")
	if err := cleanDB(fullPath); err != nil {
		return err
	}
	fullStore, err := lightshow.OpenStore(fullPath)
	if err != nil {
		return err
	}
	full, err := lightshow.Run(context.Background(), show, fullStore, lightshow.RunFullRecompute)
	add("full recompute equals realtime", compareErr(realtime, full), "180 frames and all decisions identical")
	_ = fullStore.Close()

	resumePath := filepath.Join(auxDir, "resume.db")
	if err := cleanDB(resumePath); err != nil {
		return err
	}
	resumeStore, err := lightshow.OpenStore(resumePath)
	if err != nil {
		return err
	}
	canceled, cancelErr := lightshow.RunWithOptions(context.Background(), show, resumeStore, lightshow.RunOptions{
		Mode: lightshow.RunRealtime, CancelAfter: 3,
	})
	if lightshow.IsCode(cancelErr, lightshow.CodeCanceled) {
		report.ErrorCodes[lightshow.CodeCanceled] = cancelErr.Error()
	}
	add("E_CANCELED observed", requireCode(cancelErr, lightshow.CodeCanceled),
		fmt.Sprintf("canceled after %d committed windows", len(canceled.Windows)))
	resumed, err := lightshow.Run(context.Background(), show, resumeStore, lightshow.RunResume)
	resumeDetail := "resume failed"
	if resumed != nil {
		resumeDetail = fmt.Sprintf("reused %d committed windows", resumed.ReusedWindows)
		report.Metrics["cancel_resume_reused_windows"] = resumed.ReusedWindows
	}
	add("resume equals full recompute", compareErr(realtime, errWithRun(err, resumed)), resumeDetail)
	_ = resumeStore.Close()

	incPath := filepath.Join(auxDir, "incremental.db")
	if err := cleanDB(incPath); err != nil {
		return err
	}
	incStore, err := lightshow.OpenStore(incPath)
	if err != nil {
		return err
	}
	if _, err := lightshow.Run(context.Background(), show, incStore, lightshow.RunRealtime); err != nil {
		return err
	}
	engine, err := lightshow.NewIncrementalEngine(incStore, show)
	if err != nil {
		return err
	}
	incremental, err := engine.EditChange("C30_L4", func(change *lightshow.Change) error {
		position := [3]int{400, -400, 600}
		change.Values.Position = &position
		return nil
	})
	if err != nil {
		return err
	}
	fullAfterPath := filepath.Join(auxDir, "after-edit.db")
	if err := cleanDB(fullAfterPath); err != nil {
		return err
	}
	fullAfterStore, err := lightshow.OpenStore(fullAfterPath)
	if err != nil {
		return err
	}
	fullAfter, err := lightshow.Run(context.Background(), engine.Show, fullAfterStore, lightshow.RunFullRecompute)
	if err != nil {
		return err
	}
	_ = fullAfterStore.Close()
	incrementalRun, err := lightshow.Run(context.Background(), engine.Show, incStore, lightshow.RunResume)
	comparison := compareErr(fullAfter, errWithRun(err, incrementalRun))
	add("incremental equals full recompute", comparison,
		fmt.Sprintf("invalidated=%v reused=%d exports=%v",
			incremental.Report.Invalidated, len(incremental.Report.Reused), incremental.Report.Exports))
	report.Metrics["incremental_invalidated"] = incremental.Report.Invalidated
	report.Metrics["incremental_reused"] = len(incremental.Report.Reused)
	report.Metrics["incremental_exports"] = incremental.Report.Exports
	_ = incStore.Close()

	stateErr := realtimeStore.Transition("window-0000", lightshow.StateScheduling)
	add("E_STATE observed", requireCode(stateErr, lightshow.CodeState), errorDetail(stateErr))
	if stateErr != nil {
		report.ErrorCodes[lightshow.CodeState] = stateErr.Error()
	}
	if state, _ := realtimeStore.State("window-0000"); state != lightshow.StateCommitted {
		add("illegal E_STATE preserves committed state", fmt.Errorf("state=%s", state), "")
	}
	_ = realtimeStore.Close()

	var v3Root map[string]any
	if err := json.Unmarshal(showBytes(sampleDir), &v3Root); err != nil {
		return err
	}
	unknown := cloneJSONMap(v3Root)
	unknown["formatVersion"] = 99
	_, err = lightshow.Load(mustJSON(unknown))
	add("E_VERSION observed", requireCode(err, lightshow.CodeVersion), errorDetail(err))
	report.ErrorCodes[lightshow.CodeVersion] = errorDetail(err)

	order := cloneJSONMap(v3Root)
	changes := order["changes"].([]any)
	changes[1].(map[string]any)["at_ms"] = 6000
	_, err = lightshow.Load(mustJSON(order))
	add("E_ORDER observed", requireCode(err, lightshow.CodeOrder), errorDetail(err))
	report.ErrorCodes[lightshow.CodeOrder] = errorDetail(err)

	cycle := cloneJSONMap(v3Root)
	for _, raw := range cycle["controlGroups"].([]any) {
		group := raw.(map[string]any)
		if group["id"] == "G_FRONT" {
			group["members"].(map[string]any)["groups"] = []any{"G_ALL"}
		}
	}
	_, err = lightshow.Load(mustJSON(cycle))
	add("E_GROUP observed", requireCode(err, lightshow.CodeGroup), errorDetail(err))
	report.ErrorCodes[lightshow.CodeGroup] = errorDetail(err)

	lossy, lossErr := show.ExportStrict(lightshow.Version1)
	add("E_LOSSY observed", requireCode(lossErr, lightshow.CodeLossy), errorDetail(lossErr))
	report.ErrorCodes[lightshow.CodeLossy] = errorDetail(lossErr)
	_ = lossy

	decodePath := filepath.Join(auxDir, "decode.db")
	if err := cleanDB(decodePath); err != nil {
		return err
	}
	decodeStore, err := lightshow.OpenStore(decodePath)
	if err != nil {
		return err
	}
	_, err = lightshow.Run(context.Background(), show, decodeStore, lightshow.RunRealtime)
	if err != nil {
		return err
	}
	_ = decodeStore.Close()
	rawDB, err := sql.Open("sqlite", decodePath)
	if err != nil {
		return err
	}
	_, err = rawDB.Exec(`UPDATE outputs SET color_r = color_r + 1 WHERE window_id = 'window-0000' AND frame = 0 AND fixture_id = 'L1'`)
	if err != nil {
		return err
	}
	_ = rawDB.Close()
	decodeStore, err = lightshow.OpenStore(decodePath)
	if err != nil {
		return err
	}
	err = decodeStore.ValidateCommitted()
	add("E_DECODE observed", requireCode(err, lightshow.CodeDecode), errorDetail(err))
	report.ErrorCodes[lightshow.CodeDecode] = errorDetail(err)
	_ = decodeStore.Close()

	report.Passed = true
	for _, check := range report.Checks {
		if !check.Pass {
			report.Passed = false
		}
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		return err
	}
	if !report.Passed {
		for _, check := range report.Checks {
			if !check.Pass {
				fmt.Fprintf(os.Stderr, "FAIL %s: %s\n", check.Name, check.Detail)
			}
		}
		return fmt.Errorf("verification failed")
	}
	fmt.Printf("verification passed: %d checks, report=%s\n", len(report.Checks), reportPath)
	return nil
}

func compareErr(a, b *lightshow.RunResult) error {
	if a == nil || b == nil {
		return fmt.Errorf("nil run result")
	}
	mismatch := lightshow.CompareRuns(a, b)
	if mismatch == nil {
		return nil
	}
	return mismatch
}

func errWithRun(err error, run *lightshow.RunResult) *lightshow.RunResult {
	if err != nil {
		return nil
	}
	return run
}

func requireCode(err error, code string) error {
	if !lightshow.IsCode(err, code) {
		return fmt.Errorf("got %v, want %s", err, code)
	}
	return nil
}

func errorDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func showBytes(dir string) []byte {
	data, _ := os.ReadFile(filepath.Join(dir, "show.v3.json"))
	return data
}

func cloneJSONMap(in map[string]any) map[string]any {
	data, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
