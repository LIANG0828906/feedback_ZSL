package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/diagnostics"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/incremental"
	"mvccworkbench/internal/workbench"
)

func main() {
	if len(os.Args) == 1 {
		runREPL()
		return
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "repl":
		runREPL()
	case "serve":
		err = serve(args)
	case "verify":
		err = verify(args)
	case "verify-all":
		err = verifyAll(args)
	case "doc":
		err = doc(args)
	case "truth":
		err = truth(args)
	case "inject":
		err = inject(args)
	case "checkpoint-demo":
		err = checkpointDemo(args)
	case "checkpoint-inject":
		err = checkpointInject(args)
	case "run-safe":
		err = runSafe(args)
	case "incremental-demo":
		err = incrementalDemo(args)
	case "help", "-h", "--help":
		printHelp()
	default:
		err = fmt.Errorf("unknown command %q", cmd)
		printHelp()
	}
	if err != nil {
		printError(err)
		os.Exit(1)
	}
}

func newService() (*workbench.Service, error) {
	return workbench.NewService(424242)
}

func resolveSchema(value string) string {
	switch value {
	case "", "canonical":
		return ""
	case "v1", "1":
		return domain.SchemaV1
	case "v2", "2":
		return domain.SchemaV2
	case "v3", "3":
		return domain.SchemaV3
	default:
		return value
	}
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	schemaName := fs.String("schema", "canonical", "canonical|v1|v2|v3")
	parallel := fs.Bool("parallel", false, "use bounded parallel scheduler")
	workers := fs.Int("workers", 4, "worker budget")
	buffer := fs.Int("buffer", 128, "per-item buffer limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	report, err := svc.Verify(context.Background(), *batch, resolveSchema(*schemaName), *parallel, engine.Options{
		Workers: *workers, ItemBufferLimit: *buffer, ChainCap: 1024,
	})
	if err != nil {
		return err
	}
	printJSON(report)
	if !report.OK {
		return fmt.Errorf("truth comparison failed with %d mismatches", len(report.Mismatches))
	}
	return nil
}

func verifyAll(args []string) error {
	fs := flag.NewFlagSet("verify-all", flag.ContinueOnError)
	parallel := fs.Bool("parallel", false, "parallel verification")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	schemas := []string{"", "v1", "v2", "v3"}
	failed := 0
	reports := []*workbench.VerifyReport{}
	for _, batch := range svc.BatchIDs() {
		for _, schemaName := range schemas {
			report, err := svc.Verify(context.Background(), batch, resolveSchema(schemaName), *parallel, engine.DefaultOptions())
			if err != nil {
				return err
			}
			reports = append(reports, report)
			if !report.OK {
				failed++
			}
		}
	}
	summary := map[string]any{"checked": len(reports), "failed": failed, "parallel": *parallel}
	printJSON(summary)
	if failed > 0 {
		for _, report := range reports {
			if !report.OK {
				printJSON(report)
			}
		}
		return fmt.Errorf("%d verification targets failed", failed)
	}
	return nil
}

func doc(args []string) error {
	fs := flag.NewFlagSet("doc", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	schemaName := fs.String("schema", "v1", "v1|v2|v3")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	data, ok := svc.Docs[*batch][resolveSchema(*schemaName)]
	if !ok {
		return domain.NewError(domain.ESchemaUnknown, "document not found").With("batch", *batch).With("schema", *schemaName)
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}

func truth(args []string) error {
	fs := flag.NewFlagSet("truth", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	b, ok := svc.Suite.Batches[*batch]
	if !ok {
		return domain.NewError(domain.EInputInvalidRef, "unknown batch").With("batch", *batch)
	}
	printJSON(b.Truth)
	return nil
}

func inject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	kind := fs.String("kind", diagnostics.InjectDuplicateItem, "injection kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	project, _, err := svc.Project(*batch, "canonical")
	if err != nil {
		return err
	}
	if *kind == diagnostics.InjectUnknownSchema {
		err = domain.NewError(domain.ESchemaUnknown, "unrecognized schema version or feature combination").
			At("schema_version").With("schema_version", "mvcc.project/v9").
			With("supported", []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3})
		printError(err)
		return nil
	}
	_, injectErr := diagnostics.InjectInput(project, *kind)
	if injectErr == nil {
		printJSON(map[string]any{"ok": true, "code": "OK"})
		return nil
	}
	printError(injectErr)
	return nil
}

func checkpointDemo(args []string) error {
	fs := flag.NewFlagSet("checkpoint-demo", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	db := fs.String("db", "./mvcc_checkpoint.db", "sqlite database path")
	crashAt := fs.Int("crash-at", 7, "operation index after which simulated process is killed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	project, _, err := svc.Project(*batch, "canonical")
	if err != nil {
		return err
	}
	report, err := workbench.CrashAndResume(context.Background(), project, *db, *crashAt, engine.DefaultOptions())
	if err != nil {
		return err
	}
	printJSON(report)
	return nil
}

func runSafe(args []string) error {
	fs := flag.NewFlagSet("run-safe", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	db := fs.String("db", "./mvcc_signal_checkpoint.db", "sqlite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	project, _, err := svc.Project(*batch, "canonical")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, runErr := workbench.RunCheckpointed(ctx, project, *db, engine.DefaultOptions(), nil)
	if report != nil {
		printJSON(report)
	}
	if domain.CodeOf(runErr) == domain.EInterrupted {
		fmt.Fprintln(os.Stderr, "received stop signal; checkpoint flushed at last complete operation")
		return nil
	}
	return runErr
}

func checkpointInject(args []string) error {
	fs := flag.NewFlagSet("checkpoint-inject", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id used to create a good checkpoint")
	db := fs.String("db", "./mvcc-inject-checkpoint.db", "sqlite database path")
	kind := fs.String("kind", diagnostics.InjectTruncatedCkpt, "truncated_checkpoint|version_mismatched_snapshot")
	itemID := fs.String("item", "alpha", "item id for truncated chain injection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	project, _, err := svc.Project(*batch, "canonical")
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	runner := engine.NewStreamRunner(project, engine.DefaultOptions())
	if err := runner.RunAll(context.Background()); err != nil {
		return err
	}
	if err := runner.SaveCheckpoint(context.Background(), store); err != nil {
		return err
	}
	injectErr := diagnostics.InjectCheckpointFault(context.Background(), store, *kind, *itemID, "")
	printErrorOrOK(injectErr)
	return nil
}

func incrementalDemo(args []string) error {
	fs := flag.NewFlagSet("incremental-demo", flag.ContinueOnError)
	batch := fs.String("batch", "B1-visibility", "batch id")
	txnID := fs.Uint64("txn", 3, "transaction id")
	isolation := fs.String("isolation", string(domain.ReadCommitted), "new isolation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	project, _, err := svc.Project(*batch, "canonical")
	if err != nil {
		return err
	}
	service, err := incremental.NewService(project, engine.DefaultOptions())
	if err != nil {
		return err
	}
	analysis, err := service.ChangeIsolation(*txnID, domain.Isolation(*isolation))
	if err != nil {
		return err
	}
	printJSON(map[string]any{
		"edit_type":        analysis.EditType,
		"reused_reads":     analysis.ReusedReads,
		"recomputed_reads": analysis.RecomputedReads,
		"reused_gc":        analysis.ReusedGC,
		"recomputed_gc":    analysis.RecomputedGC,
		"read_results":     analysis.ReadResults,
		"gc_results":       analysis.GCResults,
	})
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := newService()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "MVCC workbench listening on http://%s\n", *addr)
	return workbench.ListenAndServe(*addr, svc)
}

func runREPL() {
	svc, err := newService()
	if err != nil {
		printError(err)
		os.Exit(1)
	}
	reader := bufio.NewScanner(os.Stdin)
	fmt.Println("多版本可见性诊断工作台 REPL")
	fmt.Println("命令: batches | verify [batch] [v1|v2|v3] [parallel] | doc <batch> <v1|v2|v3> | inject <batch> <kind> | checkpoint <batch> [crashAt] | help | exit")
	for {
		fmt.Print("mvcc> ")
		if !reader.Scan() {
			break
		}
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return
		}
		parts := strings.Fields(line)
		ctx := context.Background()
		switch parts[0] {
		case "help":
			fmt.Println("verify B1-visibility v2 parallel | doc B1-visibility v3 | inject B1-visibility duplicate_item | checkpoint B1-visibility 7")
		case "batches":
			printJSON(svc.BatchIDs())
		case "verify":
			batch := "B1-visibility"
			schemaName := "canonical"
			parallel := false
			for _, token := range parts[1:] {
				switch token {
				case "parallel":
					parallel = true
				case "v1", "v2", "v3", "canonical":
					schemaName = token
				default:
					batch = token
				}
			}
			report, err := svc.Verify(ctx, batch, resolveSchema(schemaName), parallel, engine.DefaultOptions())
			if err != nil {
				printError(err)
				continue
			}
			printJSON(map[string]any{"batch": report.BatchID, "schema": report.Schema, "ok": report.OK, "reads": report.ReadCount, "writes": report.WriteCount, "commits": report.CommitCount, "gc": report.GCCount, "mismatches": len(report.Mismatches)})
		case "doc":
			if len(parts) < 3 {
				fmt.Println("usage: doc <batch> <v1|v2|v3>")
				continue
			}
			data := svc.Docs[parts[1]][resolveSchema(parts[2])]
			fmt.Println(string(data))
		case "inject":
			if len(parts) < 3 {
				fmt.Println("usage: inject <batch> <kind>")
				continue
			}
			project, _, err := svc.Project(parts[1], "canonical")
			if err != nil {
				printError(err)
				continue
			}
			_, err = diagnostics.InjectInput(project, parts[2])
			printErrorOrOK(err)
		case "checkpoint":
			batch := "B1-visibility"
			crashAt := 7
			if len(parts) > 1 {
				batch = parts[1]
			}
			if len(parts) > 2 {
				_, _ = fmt.Sscanf(parts[2], "%d", &crashAt)
			}
			project, _, err := svc.Project(batch, "canonical")
			if err != nil {
				printError(err)
				continue
			}
			report, err := workbench.CrashAndResume(ctx, project, "./repl-checkpoint.db", crashAt, engine.DefaultOptions())
			if err != nil {
				printError(err)
				continue
			}
			printJSON(report)
		default:
			fmt.Println("unknown command; type help")
		}
	}
}

func printHelp() {
	fmt.Println(`mvccwb commands:
  verify --batch B1-visibility [--schema canonical|v1|v2|v3] [--parallel]
  verify-all [--parallel]
  doc --batch B1-visibility --schema v2
  truth --batch B2-conflict-gc
  inject --batch B1-visibility --kind duplicate_item|bad_delete_ref|time_out_of_range|unknown_schema
  checkpoint-demo --batch B1-visibility --db ./checkpoint.db --crash-at 7
  checkpoint-inject --db ./checkpoint.db --kind truncated_checkpoint
  run-safe --batch B1-visibility --db ./signal.db
  incremental-demo --batch B1-visibility --txn 3 --isolation READ_COMMITTED
  serve --addr 127.0.0.1:18080
  repl`)
}

func printJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(data))
}

func printErrorOrOK(err error) {
	if err == nil {
		printJSON(map[string]any{"ok": true, "code": "OK"})
		return
	}
	printError(err)
}

func printError(err error) {
	payload := map[string]any{"ok": false, "code": domain.CodeOf(err), "error": err.Error()}
	if de, ok := domain.AsError(err); ok {
		payload["path"] = de.Path
		payload["context"] = de.Context
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
}
