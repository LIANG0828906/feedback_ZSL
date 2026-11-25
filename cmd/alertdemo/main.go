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

	"alertkernel/checkpoint"
	"alertkernel/demo"
	"alertkernel/engine"
	"alertkernel/errs"
	"alertkernel/sample"
	"alertkernel/schema"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var coded *errs.Error
		if errors.As(err, &coded) {
			fmt.Fprintf(os.Stderr, "%s at %s: %s\n", coded.Code, coded.Path, coded.Message)
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		args = []string{"all"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "all", "verify":
		return verifyCommand(ctx, args[1:])
	case "generate":
		return generateCommand(args[1:])
	case "run":
		return runCommand(ctx, args[1:])
	case "timeline":
		return timelineCommand(args[1:])
	case "crash-run":
		return crashRunCommand(ctx, args[1:])
	case "resume":
		return resumeCommand(ctx, args[1:])
	case "help", "-h", "--help":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func verifyCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "artifacts/demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := demo.VerifyAll(ctx, *out)
	if err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	timelinePath, err := demo.WriteTimeline(*out, bundle)
	if err != nil {
		return err
	}
	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	reportPath := filepath.Join(*out, "verification-report.json")
	if err := os.WriteFile(reportPath, reportJSON, 0o644); err != nil {
		return err
	}
	fmt.Printf("verification ok=%v seed=%d version=%s\n", report.OK, report.Seed, report.RuleVersion)
	fmt.Printf("schema core equal=%v report equal=%v concurrency equal=%v\n", report.SchemaCoreEqual, report.SchemaReportEqual, report.ConcurrencyEqual)
	passed := 0
	for _, check := range report.MilestoneChecks {
		if check.OK {
			passed++
		} else {
			fmt.Printf("FAIL milestone %s: %s\n", check.Name, check.Detail)
		}
	}
	fmt.Printf("milestones %d/%d passed\n", passed, len(report.MilestoneChecks))
	demo.PrintEditSummaries(report)
	fmt.Printf("recovery: %v (%s)\n", report.RecoveryCheck.OK, report.RecoveryCheck.Detail)
	fmt.Printf("version: %v (%s)\n", report.VersionMismatchCheck.OK, report.VersionMismatchCheck.Detail)
	fmt.Printf("corrupt: %v (%s)\n", report.CorruptCheck.OK, report.CorruptCheck.Detail)
	for _, check := range report.InjectionChecks {
		if !check.OK {
			fmt.Printf("FAIL injection %s: %s\n", check.Name, check.Detail)
		}
	}
	fmt.Printf("artifacts: %s, %s, %s\n", reportPath, timelinePath, filepath.Join(*out, "project-v1.json"))
	if !report.OK {
		return errs.New(errs.EValidation, "verification", "verification failed; see %s", reportPath)
	}
	return nil
}

func generateCommand(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts/demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	if err := demo.WriteGenerated(*out, bundle); err != nil {
		return err
	}
	_, err = demo.WriteTimeline(*out, bundle)
	return err
}

func timelineCommand(args []string) error {
	fs := flag.NewFlagSet("timeline", flag.ContinueOnError)
	out := fs.String("out", "artifacts/demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	path, err := demo.WriteTimeline(*out, bundle)
	if err != nil {
		return err
	}
	return printFile(path)
}

func crashRunCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crash-run", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/demo/checkpoints/kill.db", "sqlite path")
	after := fs.Int("crash-after", 2, "commit this logical t, then exit immediately")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	runner, err := engine.NewRunner(bundle.Project, bundle.Events, nil, 0)
	if err != nil {
		return err
	}
	for t := 0; t <= *after; t++ {
		frame, err := runner.Step(ctx, t)
		if err != nil {
			return err
		}
		if err := store.SaveTime(ctx, frame, bundle.Project.RuleVersion); err != nil {
			return err
		}
		fmt.Printf("committed t=%d\n", t)
		if t == *after {
			os.Exit(137)
		}
	}
	return store.Close()
}

func resumeCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/demo/checkpoints/kill.db", "sqlite path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	recovery, err := store.LoadRecovery(ctx, bundle.Project.RuleVersion)
	if err != nil {
		return err
	}
	if recovery.LastT < 0 {
		return errs.New(errs.ECheckpoint, *dbPath, "no committed cursor")
	}
	runner, err := engine.NewRunner(bundle.Project, bundle.Events, recovery.States, recovery.BasisSeq)
	if err != nil {
		return err
	}
	for t := recovery.LastT + 1; t < bundle.Project.Clock.Steps; t++ {
		frame, err := runner.Step(ctx, t)
		if err != nil {
			return err
		}
		if err := store.SaveTime(ctx, frame, bundle.Project.RuleVersion); err != nil {
			return err
		}
		fmt.Printf("resumed and committed t=%d visible=%d notifications=%d\n", t, len(frame.Visible), len(frame.Notifications))
	}
	fmt.Printf("recovery ok from t=%d states=%d version=%s\n", recovery.LastT, len(recovery.States), recovery.RuleVersion)
	return nil
}

func runCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dbPath := fs.String("db", "artifacts/demo/checkpoints/alertkernel.db", "sqlite checkpoint path")
	schemaName := fs.String("schema", "canonical", "canonical, v1, v2, or v3")
	stepSleep := fs.Duration("step-sleep", 0, "optional demo delay after a committed step; never used for duration math")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := sample.Generate()
	if err != nil {
		return err
	}
	project := bundle.Project
	events := bundle.Events
	if *schemaName != "canonical" {
		data, ok := bundle.Docs[*schemaName]
		if !ok {
			return errs.New(errs.EValidation, "schema", "unknown schema %q", *schemaName)
		}
		project, err = schema.Load(data)
		if err != nil {
			return err
		}
		if len(project.Silences) == 0 && len(bundle.Project.Silences) > 0 {
			events.Silences = append(events.Silences, bundle.Project.Silences...)
		}
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	recovery, err := store.LoadMatchingOrReset(ctx, project.RuleVersion)
	if err != nil {
		return err
	}
	if recovery.ResetReason != "" {
		fmt.Printf("checkpoint version mismatch handled: reset due to %s; full recompute\n", recovery.ResetReason)
	}
	runner, err := engine.NewRunner(project, events, recovery.States, recovery.BasisSeq)
	if err != nil {
		return err
	}
	lastCommitted := recovery.LastT
	for t := recovery.LastT + 1; t < project.Clock.Steps; t++ {
		frame, err := runner.Step(ctx, t)
		if errs.IsCode(err, errs.EInterrupted) {
			fmt.Printf("signal received before t=%d; last complete checkpoint t=%d\n", t, lastCommitted)
			return nil
		}
		if err != nil {
			return err
		}
		if err := store.SaveTime(ctx, frame, project.RuleVersion); err != nil {
			return err
		}
		lastCommitted = t
		fmt.Printf("committed t=%d states=%d visible=%d suppressions=%d notifications=%d\n",
			t, len(frame.States), len(frame.Visible), len(frame.SuppressionDetails), len(frame.Notifications))
		if *stepSleep > 0 && t < project.Clock.Steps-1 {
			select {
			case <-ctx.Done():
				fmt.Printf("signal received after commit t=%d; checkpoint is durable\n", lastCommitted)
				return nil
			case <-time.After(*stepSleep):
			}
		}
	}
	fmt.Printf("run complete through t=%d; db=%s version=%s\n", lastCommitted, *dbPath, project.RuleVersion)
	return nil
}

func printFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func printHelp() {
	fmt.Println(`alertdemo commands:
  all [--out DIR]                  Generate samples and run all local verifications (default)
  generate [--out DIR]             Write v1/v2/v3 projects, events and truth
  timeline [--out DIR]             Print deterministic per-time state/visibility/notification timeline
  run [--db PATH] [--schema NAME]  Commit logical steps; SIGINT/SIGTERM stops at last complete step
  crash-run --db PATH --crash-after N  Commit through N and exit 137 immediately
  resume --db PATH                 Reopen SQLite and resume after committed cursor`)
}
