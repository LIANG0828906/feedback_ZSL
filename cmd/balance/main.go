package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"battlebalance"
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "generate":
		return commandGenerate(args[1:], stdout, stderr)
	case "validate":
		return commandValidate(args[1:], stdout, stderr)
	case "run":
		return commandRun(args[1:], stdout, stderr)
	case "compare":
		return commandCompare(args[1:], stdout, stderr)
	case "verify":
		return commandVerify(args[1:], stdout, stderr)
	case "demo":
		return commandDemo(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  balance generate --out project.json --seed 42
  balance validate --project project.json
  balance run --project project.json --mode full|realtime|resume --state checkpoint.json --report report.json
  balance compare --expected full.json --actual resumed.json
  balance verify --project project.json --state checkpoint.json
  balance demo --dir artifacts/demo --seed 42

run injection flags:
  --inject unit|cooldown|resource|internal [--inject-plan ID] [--inject-scenario ID] [--inject-match N] [--inject-turn N]`)
}

func commandGenerate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "project.json", "output project path")
	seed := flags.Int64("seed", 42, "deterministic generator seed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	project := balance.GenerateProject(*seed)
	if err := balance.ValidateProject(project); err != nil {
		return printError(stdout, stderr, err)
	}
	if err := balance.SaveProject(*out, project); err != nil {
		return printError(stdout, stderr, &balance.AppError{Code: balance.CodeIO, Object: *out, Detail: err.Error()})
	}
	return printSuccess(stdout, map[string]any{
		"ok":          true,
		"project":     *out,
		"projectHash": balance.ProjectContentHash(project),
		"seed":        *seed,
	})
}

func commandValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	projectPath := flags.String("project", "", "project JSON path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *projectPath == "" {
		fmt.Fprintln(stderr, "--project is required")
		return 2
	}
	project, err := balance.LoadProject(*projectPath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	return printSuccess(stdout, map[string]any{
		"ok":          true,
		"projectHash": balance.ProjectContentHash(project),
		"skills":      len(project.Skills),
		"builds":      len(project.Builds),
		"scenarios":   len(project.Scenarios),
		"plans":       len(project.Plans),
	})
}

func commandRun(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	projectPath := flags.String("project", "", "project JSON path")
	modeValue := flags.String("mode", "full", "full, realtime, or resume")
	statePath := flags.String("state", "", "checkpoint path")
	reportPath := flags.String("report", "", "report output path")
	stopAfter := flags.Int("stop-after", 0, "commit at most N new combinations, then pause")
	inject := flags.String("inject", "", "unit, cooldown, resource, or internal")
	injectPlan := flags.String("inject-plan", "", "fault injection plan ID")
	injectScenario := flags.String("inject-scenario", "", "fault injection scenario ID")
	injectMatch := flags.Int("inject-match", 0, "fault injection match index (0 means any)")
	injectTurn := flags.Int("inject-turn", 3, "fault injection action turn")
	budgetSlots := flags.Int("budget-slots", -1, "override simulation slots")
	budgetPerRound := flags.Int("budget-max-per-round", -1, "override matches per round")
	budgetMemory := flags.Int("budget-memory", -1, "override per-worker memory buffer")
	budgetRounds := flags.Int("budget-max-rounds", -1, "override maximum budget rounds")
	budgetTarget := flags.Int("budget-target", -1, "override target samples per combination")
	budgetTotal := flags.Int("budget-max-total", -1, "override total match budget")
	budgetPriority := flags.String("budget-priority", "", "override priority_first or equal_coverage")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *projectPath == "" {
		fmt.Fprintln(stderr, "--project is required")
		return 2
	}
	mode, err := parseMode(*modeValue)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	if (mode == balance.ModeRealtime || mode == balance.ModeResume) && *statePath == "" {
		fmt.Fprintln(stderr, "--state is required for realtime and resume modes")
		return 2
	}
	if err := validateInjection(*inject); err != nil {
		return printError(stdout, stderr, err)
	}
	project, err := balance.LoadProject(*projectPath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	applyBudgetOverrides(project, *budgetSlots, *budgetPerRound, *budgetMemory, *budgetRounds, *budgetTarget, *budgetTotal, *budgetPriority)
	if err := balance.ValidateProject(project); err != nil {
		return printError(stdout, stderr, err)
	}
	options := balance.RunOptions{
		Mode:             mode,
		CheckpointPath:   *statePath,
		StopAfterCommits: *stopAfter,
		Injection: balance.Injection{
			Kind:       *inject,
			PlanID:     *injectPlan,
			ScenarioID: *injectScenario,
			MatchIndex: *injectMatch,
			Turn:       *injectTurn,
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	report, runErr := balance.Run(ctx, project, options)
	if report != nil && *reportPath != "" {
		if err := writeJSONFile(*reportPath, report); err != nil {
			return printError(stdout, stderr, err)
		}
	}
	if runErr != nil {
		return printErrorWithContext(stdout, stderr, runErr, reportPath, report)
	}
	if report == nil {
		return printError(stdout, stderr, &balance.AppError{Code: balance.CodeInternal, Object: "report", Detail: "run returned no report"})
	}
	if *reportPath == "" {
		return printSuccess(stdout, report)
	}
	return printSuccess(stdout, map[string]any{
		"ok":             true,
		"report":         *reportPath,
		"mode":           mode,
		"partial":        report.Partial,
		"projectHash":    report.ProjectHash,
		"commitSequence": report.Checkpoint.CommitSequence,
	})
}

func commandCompare(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	expectedPath := flags.String("expected", "", "expected report JSON")
	actualPath := flags.String("actual", "", "actual report JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *expectedPath == "" || *actualPath == "" {
		fmt.Fprintln(stderr, "--expected and --actual are required")
		return 2
	}
	expected, err := loadReport(*expectedPath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	actual, err := loadReport(*actualPath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	if err := balance.CompareReports(expected, actual); err != nil {
		return printError(stdout, stderr, err)
	}
	return printSuccess(stdout, map[string]any{"ok": true, "equal": true})
}

func commandVerify(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	projectPath := flags.String("project", "", "project JSON path")
	statePath := flags.String("state", "", "checkpoint JSON path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *projectPath == "" || *statePath == "" {
		fmt.Fprintln(stderr, "--project and --state are required")
		return 2
	}
	project, err := balance.LoadProject(*projectPath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	checkpoint, err := balance.ReadCheckpointFile(*statePath)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	report, err := balance.VerifyCheckpoint(project, checkpoint)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	if err := printJSON(stdout, report); err != nil {
		return 1
	}
	if !report.OK {
		return 2
	}
	return 0
}

func commandDemo(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "artifacts/demo", "demo output directory")
	seed := flags.Int64("seed", 42, "deterministic generator seed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return printError(stdout, stderr, err)
	}
	projectPath := filepath.Join(*dir, "project.json")
	project := balance.GenerateProject(*seed)
	if err := balance.SaveProject(projectPath, project); err != nil {
		return printError(stdout, stderr, &balance.AppError{Code: balance.CodeIO, Object: projectPath, Detail: err.Error()})
	}
	ctx := context.Background()

	fullReport, err := balance.Run(ctx, project, balance.RunOptions{Mode: balance.ModeFull})
	if err != nil {
		return printError(stdout, stderr, err)
	}
	fullPath := filepath.Join(*dir, "full-report.json")
	if err := writeJSONFile(fullPath, fullReport); err != nil {
		return printError(stdout, stderr, err)
	}

	realtimeCheckpoint := filepath.Join(*dir, "realtime-checkpoint.json")
	realtimeReport, err := balance.Run(ctx, project, balance.RunOptions{
		Mode:           balance.ModeRealtime,
		CheckpointPath: realtimeCheckpoint,
	})
	if err != nil {
		return printError(stdout, stderr, err)
	}
	realtimePath := filepath.Join(*dir, "realtime-report.json")
	if err := writeJSONFile(realtimePath, realtimeReport); err != nil {
		return printError(stdout, stderr, err)
	}

	resumeCheckpoint := filepath.Join(*dir, "resume-checkpoint.json")
	partialReport, err := balance.Run(ctx, project, balance.RunOptions{
		Mode:             balance.ModeRealtime,
		CheckpointPath:   resumeCheckpoint,
		StopAfterCommits: 3,
	})
	if err != nil {
		return printError(stdout, stderr, err)
	}
	partialPath := filepath.Join(*dir, "partial-report.json")
	if err := writeJSONFile(partialPath, partialReport); err != nil {
		return printError(stdout, stderr, err)
	}
	resumedReport, err := balance.Run(ctx, project, balance.RunOptions{
		Mode:           balance.ModeResume,
		CheckpointPath: resumeCheckpoint,
	})
	if err != nil {
		return printError(stdout, stderr, err)
	}
	resumedPath := filepath.Join(*dir, "resumed-report.json")
	if err := writeJSONFile(resumedPath, resumedReport); err != nil {
		return printError(stdout, stderr, err)
	}

	realtimeDiff := balance.CompareReports(fullReport, realtimeReport)
	resumedDiff := balance.CompareReports(fullReport, resumedReport)
	checkpoint, err := balance.ReadCheckpointFile(resumeCheckpoint)
	if err != nil {
		return printError(stdout, stderr, err)
	}
	verification, err := balance.VerifyCheckpoint(project, checkpoint)
	if err != nil {
		return printError(stdout, stderr, err)
	}

	summary := map[string]any{
		"ok":             realtimeDiff == nil && resumedDiff == nil && verification.OK,
		"project":        projectPath,
		"projectHash":    balance.ProjectContentHash(project),
		"fullReport":     fullPath,
		"realtimeReport": realtimePath,
		"partialReport":  partialPath,
		"resumedReport":  resumedPath,
		"realtimeEqual":  realtimeDiff == nil,
		"resumedEqual":   resumedDiff == nil,
		"verification":   verification,
	}
	if realtimeDiff != nil {
		summary["realtimeDiff"] = balance.AsAppError(realtimeDiff)
	}
	if resumedDiff != nil {
		summary["resumedDiff"] = balance.AsAppError(resumedDiff)
	}
	if err := writeJSONFile(filepath.Join(*dir, "summary.json"), summary); err != nil {
		return printError(stdout, stderr, err)
	}
	if realtimeDiff != nil {
		return printError(stdout, stderr, realtimeDiff)
	}
	if resumedDiff != nil {
		return printError(stdout, stderr, resumedDiff)
	}
	if !verification.OK {
		_ = printJSON(stdout, summary)
		return 2
	}
	return printSuccess(stdout, summary)
}

func applyBudgetOverrides(project *balance.Project, slots, perRound, memory, rounds, target, total int, priority string) {
	if slots >= 0 {
		project.Budget.Slots = slots
	}
	if perRound >= 0 {
		project.Budget.MaxMatchesPerRound = perRound
	}
	if memory >= 0 {
		project.Budget.MemoryBuffer = memory
	}
	if rounds >= 0 {
		project.Budget.MaxRounds = rounds
	}
	if target >= 0 {
		project.Budget.TargetMatchesPerCombo = target
	}
	if total >= 0 {
		project.Budget.MaxTotalMatches = total
	}
	if priority != "" {
		project.Budget.PriorityRule = priority
	}
}

func parseMode(value string) (balance.RunMode, error) {
	switch balance.RunMode(value) {
	case balance.ModeFull, balance.ModeRealtime, balance.ModeResume:
		return balance.RunMode(value), nil
	default:
		return "", &balance.AppError{Code: balance.CodeSchema, Object: "mode", Field: "mode", Detail: "unknown run mode " + value}
	}
}

func validateInjection(value string) error {
	switch value {
	case "", "unit", "cooldown", "resource", "internal":
		return nil
	default:
		return &balance.AppError{Code: balance.CodeSchema, Object: "inject", Field: "inject", Detail: "unknown injection " + value}
	}
}

func loadReport(path string) (*balance.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &balance.AppError{Code: balance.CodeIO, Object: path, Detail: err.Error()}
	}
	var report balance.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, &balance.AppError{Code: balance.CodeSchema, Object: path, Detail: err.Error()}
	}
	return &report, nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &balance.AppError{Code: balance.CodeIO, Object: path, Detail: err.Error()}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return &balance.AppError{Code: balance.CodeIO, Object: path, Detail: err.Error()}
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return &balance.AppError{Code: balance.CodeIO, Object: path, Detail: err.Error()}
	}
	return nil
}

func printJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return nil
}

func printSuccess(writer io.Writer, value any) int {
	if err := printJSON(writer, value); err != nil {
		return 1
	}
	return 0
}

func printError(stdout, stderr io.Writer, err error) int {
	_ = printJSON(stdout, errorEnvelope(err))
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
	}
	return 1
}

func printErrorWithContext(stdout, stderr io.Writer, err error, reportPath *string, report *balance.Report) int {
	envelope := errorEnvelope(err)
	if reportPath != nil && *reportPath != "" {
		envelope["report"] = *reportPath
	}
	if report != nil {
		envelope["partial"] = report.Partial
		envelope["projectHash"] = report.ProjectHash
	}
	_ = printJSON(stdout, envelope)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
	}
	return 1
}

func errorEnvelope(err error) map[string]any {
	var appErr *balance.AppError
	if errors.As(err, &appErr) {
		return map[string]any{"ok": false, "error": appErr}
	}
	return map[string]any{
		"ok": false,
		"error": &balance.AppError{
			Code:   balance.CodeInternal,
			Object: "cli",
			Detail: strings.TrimSpace(fmt.Sprint(err)),
		},
	}
}
