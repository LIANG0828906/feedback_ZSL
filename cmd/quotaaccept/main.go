package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"quotakernel/accept"
	"quotakernel/checkpoint"
	"quotakernel/engine"
	"quotakernel/generate"
	"quotakernel/model"
	"quotakernel/sampleio"
)

func main() {
	if len(os.Args) == 1 {
		if err := runAll(42, "samples/seed-42", "artifacts/acceptance-report.json", "artifacts/checkpoint-verify"); err != nil {
			fmt.Fprintln(os.Stderr, "acceptance failed:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "generate":
		must(runGenerate(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "all":
		os.Exit(runAllCommand(os.Args[2:]))
	case "run":
		must(runCheckpointRun(os.Args[2:]))
	case "recover":
		os.Exit(runRecover(os.Args[2:]))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`quotaaccept commands:
  generate [--seed 42] [--dir samples/seed-42]
  verify   [--dir samples/seed-42] [--report artifacts/acceptance-report.json] [--checkpoint artifacts/checkpoint-verify]
  all      [--seed 42] [--dir samples/seed-42] [--report ...]
  run      [--seed 42] [--checkpoint artifacts/crash-checkpoint] [--kill-at-tick 4] [--tick-delay 25ms] [--exit-report artifacts/exit-report.json]
  recover  [--dir samples/seed-42] [--checkpoint artifacts/crash-checkpoint] [--report artifacts/recovery-report.json]`)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	dir := fs.String("dir", "samples/seed-42", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := generate.Generate(*seed)
	if err != nil {
		return err
	}
	if err := sampleio.WriteBundle(*dir, bundle); err != nil {
		return err
	}
	fmt.Printf("generated seed=%d projects=%d dir=%s\n", *seed, len(bundle.Projects), *dir)
	return nil
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "samples/seed-42", "sample directory")
	reportPath := fs.String("report", "artifacts/acceptance-report.json", "JSON report path")
	checkpointDir := fs.String("checkpoint", "artifacts/checkpoint-verify", "recovery check directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	report, err := accept.VerifyDir(*dir, *checkpointDir)
	if report != nil {
		if writeErr := writeJSONFile(*reportPath, report); writeErr != nil {
			fmt.Fprintln(os.Stderr, "write report:", writeErr)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return 1
	}
	passed := 0
	for _, c := range report.Checks {
		status := "PASS"
		if !c.OK {
			status = "FAIL"
		} else {
			passed++
		}
		fmt.Printf("[%s] %s %s\n", status, c.Name, c.Detail)
	}
	fmt.Printf("report=%s checks=%d/%d ok=%v\n", *reportPath, passed, len(report.Checks), report.OK)
	if !report.OK {
		return 1
	}
	return 0
}

func runAllCommand(args []string) int {
	fs := flag.NewFlagSet("all", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "seed")
	dir := fs.String("dir", "samples/seed-42", "sample dir")
	report := fs.String("report", "artifacts/acceptance-report.json", "report")
	checkpointDir := fs.String("checkpoint", "artifacts/checkpoint-verify", "checkpoint dir")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := runAll(*seed, *dir, *report, *checkpointDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runAll(seed int64, dir, reportPath, checkpointDir string) error {
	if err := runGenerate([]string{"--seed", fmt.Sprint(seed), "--dir", dir}); err != nil {
		return err
	}
	code := runVerify([]string{"--dir", dir, "--report", reportPath, "--checkpoint", checkpointDir})
	if code != 0 {
		return fmt.Errorf("verification exited %d", code)
	}
	return nil
}

func runCheckpointRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "seed")
	checkpointDir := fs.String("checkpoint", "artifacts/crash-checkpoint", "checkpoint directory")
	killAtTick := fs.Int64("kill-at-tick", 0, "if >0, SIGKILL after tick entities are durable but before pointer commit")
	tickDelay := fs.Duration("tick-delay", 0, "optional delay between ticks for signal demo")
	exitReport := fs.String("exit-report", "artifacts/exit-report.json", "graceful signal report path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := generate.Generate(*seed)
	if err != nil {
		return err
	}
	project := findProject(bundle, "cluster-main")
	store := checkpoint.NewStore(*checkpointDir)
	if *killAtTick > 0 {
		store.AfterEntityWrite = func(tick model.Tick) {
			if int64(tick) == *killAtTick {
				fmt.Fprintf(os.Stderr, "crash hook: entity state for tick %d fsynced; sending SIGKILL before committed pointer\n", tick)
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	eng := engine.New(project)
	state := engine.NewState(project)
	fingerprint := engine.Fingerprint(project)
	for tick := model.Tick(1); tick <= eng.Horizon(); tick++ {
		if *tickDelay > 0 {
			select {
			case sig := <-signals:
				report := checkpoint.ExitReport{Signal: sig.String(), CompletedTick: state.Tick, Mode: "graceful-signal", OK: true, Message: "stopped at committed tick boundary"}
				_, _ = checkpoint.WriteExitReport(filepath.Dir(*exitReport), report)
				fmt.Printf("graceful signal %s; committed_tick=%d\n", sig, state.Tick)
				return nil
			case <-time.After(*tickDelay):
			}
		}
		if _, err := eng.RunFrom(state, tick); err != nil {
			return err
		}
		if _, err := store.Commit(state.Snapshot(fingerprint)); err != nil {
			return err
		}
		fmt.Printf("committed tick=%d hash=%s\n", tick, state.Hash()[:12])
	}
	report := checkpoint.ExitReport{CompletedTick: state.Tick, Mode: "completed", OK: true}
	_, _ = checkpoint.WriteExitReport(filepath.Dir(*exitReport), report)
	return nil
}

func runRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	dir := fs.String("dir", "samples/seed-42", "sample directory")
	checkpointDir := fs.String("checkpoint", "artifacts/crash-checkpoint", "checkpoint directory")
	reportPath := fs.String("report", "artifacts/recovery-report.json", "recovery report path")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	loaded, err := sampleio.LoadBundle(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	loadedProject, err := sampleio.LoadVersion(*dir, "cluster-main", model.SchemaV1)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var truth *model.Truth
	for _, p := range loaded.Projects {
		if p.Name == "cluster-main" {
			truth = p.Truth
		}
	}
	project := loadedProject.Project
	fingerprint := engine.Fingerprint(project)
	store := checkpoint.NewStore(*checkpointDir)
	snapshot, meta, err := store.Load(fingerprint)
	mode := "version-matched-checkpoint"
	var state *engine.State
	if errors.Is(err, checkpoint.ErrVersionMismatch) {
		mode = "full-recompute-version-mismatch"
		if err := store.Reset(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		state = engine.NewState(project)
	} else if err != nil {
		if errors.Is(err, checkpoint.ErrNoCheckpoint) {
			mode = "full-recompute-no-checkpoint"
			state = engine.NewState(project)
		} else {
			fmt.Fprintln(os.Stderr, "load checkpoint:", err)
			return 1
		}
	} else {
		state, err = engine.StateFromSnapshot(project, snapshot)
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore state:", err)
			return 1
		}
	}
	eng := engine.New(project)
	startTick := state.Tick
	result, err := eng.RunFrom(state, eng.Horizon())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	expectedFrom := int(startTick)
	truthCompared := 0
	if truth != nil {
		expected := truth.Ticks[startTick:]
		truthCompared = len(expected)
		if len(result.Reports) != len(expected) {
			fmt.Fprintf(os.Stderr, "recovery length mismatch got=%d want=%d\n", len(result.Reports), len(expected))
			return 1
		}
		for i := range expected {
			a, _ := json.Marshal(result.Reports[i])
			b, _ := json.Marshal(expected[i])
			if string(a) != string(b) {
				fmt.Fprintf(os.Stderr, "recovered tick %d mismatch\n", expected[i].Tick)
				return 1
			}
		}
	}
	report := map[string]any{
		"ok": true, "mode": mode, "loadedMetadata": meta,
		"startTick": startTick, "expectedTruthIndex": expectedFrom,
		"truthTicksCompared": truthCompared, "finalTick": state.Tick,
		"reportPath": *reportPath,
	}
	if err := writeJSONFile(*reportPath, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("recovery ok mode=%s committed_tick=%d recomputed=%d report=%s\n", mode, snapshot.CommittedTick, len(result.Reports), *reportPath)
	return 0
}

func findProject(b *generate.Bundle, name string) *model.Project {
	for _, p := range b.Projects {
		if p.Name == name {
			return p.Canonical
		}
	}
	if len(b.Projects) > 0 {
		return b.Projects[0].Canonical
	}
	panic("no generated project")
}

func writeJSONFile(path string, value any) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
