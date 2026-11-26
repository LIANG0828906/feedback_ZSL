package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	locklab "locklab"
)

const (
	projectV1 = "project-v1.json"
	projectV2 = "project-v2.json"
	projectV3 = "project-v3.json"
	truthFile = "truth.json"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = commandGenerate(os.Args[2:])
	case "verify":
		err = commandVerify(os.Args[2:])
	case "demo":
		err = commandDemo(os.Args[2:])
	case "resume":
		err = commandResume(os.Args[2:])
	case "incremental":
		err = commandIncremental(os.Args[2:])
	case "parallel":
		err = commandParallel(os.Args[2:])
	case "faults":
		err = commandFaults(os.Args[2:])
	case "checkpoint-sql":
		err = commandCheckpointSQL(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if code := locklab.ErrorCodeOf(err); code != "" {
			fmt.Fprintf(os.Stderr, "business_code: %s\n", code)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`locklab - deterministic transaction isolation and lock laboratory

Usage:
  locklab generate     --seed 20261127 --out samples
  locklab verify       --dir samples
  locklab demo         --dir samples --checkpoint .demo-checkpoint [--pace-ms 0]
  locklab resume       --dir samples --checkpoint .demo-checkpoint
  locklab incremental  --dir samples --tx T8 --edit lock-mode
  locklab parallel     --dir samples --workers 2 --memory 262144 --batch 3 --seed 9
  locklab faults       --dir samples [--checkpoint .demo-checkpoint]
  locklab checkpoint-sql --checkpoint .demo-checkpoint --out checkpoint.sql

Edit fields: lock-mode, isolation, order.
`)
}

func commandGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", 20261127, "deterministic generator seed")
	out := flags.String("out", "samples", "output directory")
	deadlock := flags.Bool("deadlock", true, "enable deadlock detection")
	if err := flags.Parse(args); err != nil {
		return err
	}
	options := locklab.DefaultGenerateOptions()
	options.DeadlockDetection = *deadlock
	project := locklab.GenerateProject(*seed, options)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for version := 1; version <= 3; version++ {
		data, err := locklab.EncodeProject(project, version)
		if err != nil {
			return err
		}
		name := filepath.Join(*out, fmt.Sprintf("project-v%d.json", version))
		if err := os.WriteFile(name, data, 0o644); err != nil {
			return err
		}
	}
	truth, err := locklab.GenerateTruthForProject(project)
	if err != nil {
		return err
	}
	truthData, err := locklab.EncodeTruth(truth)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, truthFile), truthData, 0o644); err != nil {
		return err
	}
	manifest := map[string]any{
		"project_id": project.ID, "seed": project.Seed,
		"canonical_digest":       project.CanonicalDigest(),
		"source_schema_versions": []int{1, 2, 3},
		"files":                  []string{projectV1, projectV2, projectV3, truthFile},
		"transactions":           len(project.Transactions),
		"data_items":             len(project.Items),
		"logical_steps":          len(locklab.BuildSchedule(project)),
		"truth_semantic_digest":  truth.SemanticDigest,
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), append(manifestData, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("generated project=%s seed=%d transactions=%d steps=%d\n",
		project.ID, project.Seed, len(project.Transactions), len(locklab.BuildSchedule(project)))
	fmt.Printf("canonical_digest=%s\n", project.CanonicalDigest())
	fmt.Printf("truth=%s\n", filepath.Join(*out, truthFile))
	return nil
}

func commandVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	projects, truth, err := loadSamples(*dir)
	if err != nil {
		return err
	}
	canonical := projects[0].CanonicalDigest()
	fmt.Printf("schema-loader: ")
	for version, project := range projects {
		if project.CanonicalDigest() != canonical {
			return fmt.Errorf("schema v%d canonical digest mismatch", version+1)
		}
	}
	fmt.Printf("PASS (v1=v2=v3 %s)\n", canonical)

	reference, err := locklab.Run(projects[0], locklab.RunOptions{})
	if err != nil {
		return err
	}
	for version, project := range projects {
		if err := locklab.VerifyTruth(project, truth); err != nil {
			return fmt.Errorf("schema v%d truth verification: %w", version+1, err)
		}
	}
	fmt.Printf("truth: PASS (steps=%d commits=%d rollbacks=%d reads=%d)\n",
		len(reference.Steps), len(reference.Commits), len(reference.Rollbacks), len(reference.Reads))
	fmt.Printf("deadlock-victims: %s\n", formatList(reference.DeadlockVictims))
	fmt.Printf("rollback-reasons: %s\n", formatRollbackReasons(reference.Rollbacks))
	fmt.Printf("anomalies: ")
	for _, anomaly := range reference.Anomalies {
		fmt.Printf("%s%s ", anomaly.Kind, anomaly.TxIDs)
	}
	fmt.Println()
	fmt.Printf("final-writes: %s\n", formatFinalWrites(reference.FinalWrites))

	incremental, err := verifyIncremental(projects[0])
	if err != nil {
		return err
	}
	fmt.Printf("incremental: PASS match=%t prefix=%d recomputed=%d suffix=%d resync=%d closure=%s\n",
		incremental.match, incremental.report.ReusedPrefixSteps, len(incremental.report.RecomputedSteps),
		incremental.report.ReusedSuffixSteps, incremental.report.ResyncStep,
		formatList(incremental.report.PropagationClosure))

	checkpoint, err := verifyCheckpoint(projects[0], reference)
	if err != nil {
		return err
	}
	fmt.Printf("checkpoint-resume: PASS last_commit=%d from_step=%d digest_match=%t\n",
		checkpoint.lastCommit, checkpoint.fromStep, checkpoint.match)

	parallel, err := verifyParallel(projects[0], reference)
	if err != nil {
		return err
	}
	fmt.Printf("parallel: PASS digest_match=%t workers=%d batches=%d observations=%d\n",
		parallel.match, parallel.workers, parallel.batches, parallel.observations)

	faults, err := locklab.RunFaultMatrix(projects[0], "")
	if err != nil {
		return err
	}
	fmt.Printf("fault-matrix: PASS\n")
	for _, observation := range faults {
		fmt.Printf("  %-32s %-34s %s source_unchanged=%t\n",
			observation.Kind, observation.Code, observation.Path, observation.SourceUnchanged)
	}
	fmt.Println("overall: PASS")
	return nil
}

func commandDemo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	checkpointDir := flags.String("checkpoint", ".demo-checkpoint", "checkpoint directory")
	pace := flags.Duration("pace-ms", 0, "pace between steps, e.g. 5ms")
	stopAfter := flags.Int("stop-after-commits", 0, "stop after N commits (0 = all)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := locklab.LoadProjectFile(filepath.Join(*dir, projectV1))
	if err != nil {
		return err
	}
	store, err := locklab.OpenCheckpointStore(*checkpointDir)
	if err != nil {
		return err
	}
	options := locklab.RunOptions{OnCommit: store.Observer(), StopAfterCommits: *stopAfter}
	contextWithSignals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	options.Context = contextWithSignals
	options.ObserveStep = func(observation locklab.StepObservation) {
		if *pace > 0 {
			time.Sleep(*pace)
		}
	}
	result, err := locklab.Run(project, options)
	if err != nil {
		if locklab.IsErrorCode(err, locklab.EInterrupted) {
			fmt.Printf("interrupted: last complete checkpoint is authoritative (next_step=%d)\n",
				result.FinalState.NextStep)
			fmt.Printf("resume with: go run ./cmd/locklab resume --dir %s --checkpoint %s\n",
				*dir, *checkpointDir)
			return nil
		}
		return err
	}
	printRun(result)
	checkpoint, err := store.Load(project)
	if err != nil {
		return err
	}
	fmt.Printf("checkpoint: commit_seq=%d last_committed_step=%d\n",
		checkpoint.CommitSeq, checkpoint.LastCommittedStep)
	if result.Partial {
		fmt.Printf("stopped after %d commits; run resume to continue\n", len(result.Commits))
	}
	return nil
}

func commandResume(args []string) error {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	checkpointDir := flags.String("checkpoint", ".demo-checkpoint", "checkpoint directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := locklab.LoadProjectFile(filepath.Join(*dir, projectV1))
	if err != nil {
		return err
	}
	store, err := locklab.OpenCheckpointStore(*checkpointDir)
	if err != nil {
		return err
	}
	result, info, err := locklab.ResumeOrRecompute(project, store, locklab.RunOptions{
		OnCommit: store.Observer(),
	})
	if err != nil {
		return err
	}
	printRun(result)
	fmt.Printf("recovery: resumed=%t from_step=%d commit_seq=%d fallback=%s\n",
		info.Resumed, info.FromStep, info.FromCommitSeq, info.FallbackCode)
	return nil
}

func commandIncremental(args []string) error {
	flags := flag.NewFlagSet("incremental", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	txID := flags.String("tx", "T8", "transaction to edit")
	edit := flags.String("edit", "lock-mode", "lock-mode, isolation, or order")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := locklab.LoadProjectFile(filepath.Join(*dir, projectV1))
	if err != nil {
		return err
	}
	edited, err := editProject(project, *txID, *edit)
	if err != nil {
		return err
	}
	full, err := locklab.Run(edited, locklab.RunOptions{})
	if err != nil {
		return err
	}
	incremental, err := locklab.RunIncremental(project, edited, *txID)
	if err != nil {
		return err
	}
	match := incremental.Result.SemanticDigest() == full.SemanticDigest()
	fmt.Printf("edit=%s tx=%s fields=%s\n", *edit, *txID, incremental.Report.EditedFields)
	fmt.Printf("result: match=%t prefix_reused=%d recomputed=%d suffix_reused=%d resync_step=%d\n",
		match, incremental.Report.ReusedPrefixSteps, len(incremental.Report.RecomputedSteps),
		incremental.Report.ReusedSuffixSteps, incremental.Report.ResyncStep)
	fmt.Printf("closure=%s\n", formatList(incremental.Report.PropagationClosure))
	for _, interval := range incremental.Report.AffectedQueueIntervals {
		fmt.Printf("queue-interval resource=%s steps=%d..%d waiters=%s\n",
			interval.Resource, interval.FromStep, interval.ToStep, formatList(interval.Waiters))
	}
	return nil
}

func commandParallel(args []string) error {
	flags := flag.NewFlagSet("parallel", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	workers := flags.Int("workers", 2, "worker budget")
	memory := flags.Int64("memory", 262144, "per-worker memory budget in bytes")
	batch := flags.Int("batch", 3, "preparation batch size")
	seed := flags.Int64("seed", 9, "schedule shuffle seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := locklab.LoadProjectFile(filepath.Join(*dir, projectV1))
	if err != nil {
		return err
	}
	reference, err := locklab.Run(project, locklab.RunOptions{})
	if err != nil {
		return err
	}
	result, err := locklab.RunParallel(project, locklab.ParallelOptions{
		Workers: *workers, MemoryBudgetBytes: *memory, BatchSize: *batch, ScheduleSeed: *seed,
	})
	if err != nil {
		return err
	}
	fmt.Printf("parallel: digest_match=%t workers=%d batch=%d peak_in_flight=%d peak_memory=%d\n",
		result.Run.SemanticDigest() == reference.SemanticDigest(),
		result.Diagnostics.WorkersUsed, result.Diagnostics.BatchSize,
		result.Diagnostics.PeakInFlight, result.Diagnostics.PeakWorkerMemory)
	for _, observation := range result.Diagnostics.StepObservations {
		fmt.Printf("observe step=%02d tx=%s kind=%s elapsed_us=%d outcome=%s\n",
			observation.GlobalStep, observation.TxID, observation.Kind,
			observation.ElapsedMicros, observation.Outcome)
	}
	return nil
}

func commandFaults(args []string) error {
	flags := flag.NewFlagSet("faults", flag.ContinueOnError)
	dir := flags.String("dir", "samples", "sample directory")
	checkpointDir := flags.String("checkpoint", "", "existing checkpoint directory (optional)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	project, err := locklab.LoadProjectFile(filepath.Join(*dir, projectV1))
	if err != nil {
		return err
	}
	observations, err := locklab.RunFaultMatrix(project, *checkpointDir)
	if err != nil {
		return err
	}
	for _, observation := range observations {
		fmt.Printf("%-32s code=%-34s path=%s source_unchanged=%t\n",
			observation.Kind, observation.Code, observation.Path, observation.SourceUnchanged)
	}
	return nil
}

func commandCheckpointSQL(args []string) error {
	flags := flag.NewFlagSet("checkpoint-sql", flag.ContinueOnError)
	checkpointDir := flags.String("checkpoint", ".demo-checkpoint", "checkpoint directory")
	out := flags.String("out", "checkpoint.sql", "SQL output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := locklab.OpenCheckpointStore(*checkpointDir)
	if err != nil {
		return err
	}
	if err := store.ExportSQL(*out); err != nil {
		return err
	}
	fmt.Printf("wrote SQLite-compatible checkpoint SQL to %s\n", *out)
	return nil
}

func loadSamples(dir string) ([]*locklab.Project, *locklab.Truth, error) {
	files := []string{projectV1, projectV2, projectV3}
	projects := make([]*locklab.Project, 0, len(files))
	for _, name := range files {
		project, err := locklab.LoadProjectFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		projects = append(projects, project)
	}
	truthData, err := os.ReadFile(filepath.Join(dir, truthFile))
	if err != nil {
		return nil, nil, err
	}
	truth, err := locklab.DecodeTruth(truthData)
	if err != nil {
		return nil, nil, err
	}
	return projects, truth, nil
}

type incrementalVerification struct {
	match  bool
	report locklab.IncrementalReport
}

func verifyIncremental(project *locklab.Project) (incrementalVerification, error) {
	edited := project.Clone()
	edited.Transactions[7].Ops[3].Mode = locklab.ModeS
	full, err := locklab.Run(edited, locklab.RunOptions{})
	if err != nil {
		return incrementalVerification{}, err
	}
	incremental, err := locklab.RunIncremental(project, edited, "T8")
	if err != nil {
		return incrementalVerification{}, err
	}
	return incrementalVerification{
		match:  incremental.Result.SemanticDigest() == full.SemanticDigest(),
		report: incremental.Report,
	}, nil
}

type checkpointVerification struct {
	match      bool
	lastCommit uint64
	fromStep   int
}

func verifyCheckpoint(project *locklab.Project, reference *locklab.RunResult) (checkpointVerification, error) {
	dir, err := os.MkdirTemp("", "locklab-verify-checkpoint-*")
	if err != nil {
		return checkpointVerification{}, err
	}
	defer os.RemoveAll(dir)
	store, err := locklab.OpenCheckpointStore(dir)
	if err != nil {
		return checkpointVerification{}, err
	}
	if _, err := locklab.Run(project, locklab.RunOptions{
		OnCommit: store.Observer(), StopAfterCommits: 3,
	}); err != nil {
		return checkpointVerification{}, err
	}
	checkpoint, err := store.Load(project)
	if err != nil {
		return checkpointVerification{}, err
	}
	resumed, info, err := locklab.ResumeOrRecompute(project, store, locklab.RunOptions{})
	if err != nil {
		return checkpointVerification{}, err
	}
	return checkpointVerification{
		match:      resumed.SemanticDigest() == reference.SemanticDigest(),
		lastCommit: checkpoint.CommitSeq, fromStep: info.FromStep,
	}, nil
}

type parallelVerification struct {
	match        bool
	workers      int
	batches      int
	observations int
}

func verifyParallel(project *locklab.Project, reference *locklab.RunResult) (parallelVerification, error) {
	result, err := locklab.RunParallel(project, locklab.ParallelOptions{
		Workers: 4, BatchSize: 7, ScheduleSeed: 42,
	})
	if err != nil {
		return parallelVerification{}, err
	}
	return parallelVerification{
		match:   result.Run.SemanticDigest() == reference.SemanticDigest(),
		workers: result.Diagnostics.WorkersUsed, batches: result.Diagnostics.BatchSize,
		observations: len(result.Diagnostics.StepObservations),
	}, nil
}

func editProject(project *locklab.Project, txID, field string) (*locklab.Project, error) {
	edited := project.Clone()
	txIndex := -1
	for i, tx := range edited.Transactions {
		if tx.ID == txID {
			txIndex = i
			break
		}
	}
	if txIndex < 0 {
		return nil, fmt.Errorf("transaction %s not found", txID)
	}
	tx := &edited.Transactions[txIndex]
	switch field {
	case "lock-mode":
		for i := range tx.Ops {
			if tx.Ops[i].Mode == locklab.ModeX {
				tx.Ops[i].Mode = locklab.ModeS
				if err := locklab.ValidateProject(edited); err != nil {
					return nil, err
				}
				return edited, nil
			}
		}
		for i := range tx.Ops {
			if tx.Ops[i].Mode == locklab.ModeS {
				tx.Ops[i].Mode = locklab.ModeX
				if err := locklab.ValidateProject(edited); err != nil {
					return nil, err
				}
				return edited, nil
			}
		}
		return nil, fmt.Errorf("transaction %s has no S/X lock to edit", txID)
	case "isolation":
		if tx.Isolation == locklab.RepeatableRead {
			tx.Isolation = locklab.ReadCommitted
		} else {
			tx.Isolation = locklab.RepeatableRead
		}
	case "order":
		swapped := false
		for i := 0; i+1 < len(tx.Ops); i++ {
			if tx.Ops[i].IsTerminal() || tx.Ops[i+1].IsTerminal() {
				continue
			}
			leftAt, rightAt := tx.Ops[i].AtMs, tx.Ops[i+1].AtMs
			tx.Ops[i], tx.Ops[i+1] = tx.Ops[i+1], tx.Ops[i]
			tx.Ops[i].LocalSeq = i + 1
			tx.Ops[i+1].LocalSeq = i + 2
			tx.Ops[i].AtMs = leftAt
			tx.Ops[i+1].AtMs = rightAt
			swapped = true
			break
		}
		if !swapped {
			return nil, fmt.Errorf("transaction %s has no adjacent editable operations", txID)
		}
	default:
		return nil, fmt.Errorf("unknown edit %q", field)
	}
	if err := locklab.ValidateProject(edited); err != nil {
		return nil, err
	}
	return edited, nil
}

func printRun(result *locklab.RunResult) {
	for _, step := range result.Steps {
		if step.Outcome == "" {
			continue
		}
		fmt.Printf("[%03d] %-3s %-6s at=%-4d outcome=%-17s",
			step.GlobalStep, step.TxID, step.Kind, step.ScheduledAtMs, step.Outcome)
		if step.LockDecision != "" && step.LockDecision != locklab.LockNone {
			fmt.Printf(" lock=%-8s", step.LockDecision)
		}
		if step.WaitSeat > 0 {
			fmt.Printf(" seat=%d", step.WaitSeat)
		}
		if len(step.ConflictWith) > 0 {
			fmt.Printf(" conflict=%s", formatList(step.ConflictWith))
		}
		if step.EventuallyGranted {
			fmt.Printf(" granted_at_step=%d", step.ExecutedStep)
		}
		if step.WaitOutcome != "" && step.WaitOutcome != locklab.WaitGranted {
			fmt.Printf(" wait=%s", step.WaitOutcome)
		}
		if step.CommitSeq > 0 {
			fmt.Printf(" commit_seq=%d", step.CommitSeq)
		}
		if step.RollbackReason != "" {
			fmt.Printf(" reason=%s", step.RollbackReason)
		}
		if step.Read != nil {
			fmt.Printf(" read=%s v%d writer=%s dirty=%t",
				step.Read.Value, step.Read.VersionNumber, step.Read.WriterTx, step.Read.Dirty)
		}
		if step.Write != nil {
			fmt.Printf(" write=%s", step.Write.Value)
		}
		fmt.Println()
	}
	fmt.Printf("commits=%s\n", formatCommits(result.Commits))
	fmt.Printf("rollbacks=%s\n", formatRollbackReasons(result.Rollbacks))
	fmt.Printf("deadlock-victims=%s\n", formatList(result.DeadlockVictims))
	fmt.Printf("final-writes=%s\n", formatFinalWrites(result.FinalWrites))
	for _, anomaly := range result.Anomalies {
		fmt.Printf("anomaly=%s tx=%s item=%s detail=%s\n",
			anomaly.Kind, formatList(anomaly.TxIDs), anomaly.ItemID, anomaly.Detail)
	}
}

func formatList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return "[" + strings.Join(values, ",") + "]"
}

func formatCommits(commits []locklab.CommitEvent) string {
	values := make([]string, 0, len(commits))
	for _, commit := range commits {
		values = append(values, fmt.Sprintf("%s:%d", commit.TxID, commit.CommitSeq))
	}
	sort.Strings(values)
	return formatList(values)
}

func formatRollbackReasons(rollbacks []locklab.RollbackEvent) string {
	values := make([]string, 0, len(rollbacks))
	for _, rollback := range rollbacks {
		values = append(values, fmt.Sprintf("%s:%s", rollback.TxID, rollback.Reason))
	}
	sort.Strings(values)
	return formatList(values)
}

func formatFinalWrites(writes map[string]locklab.VersionRecord) string {
	keys := make([]string, 0, len(writes))
	for key := range writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		version := writes[key]
		values = append(values, fmt.Sprintf("%s=%s(%s@%d)", key, version.Value, version.WriterTx, version.CommitSeq))
	}
	return formatList(values)
}
