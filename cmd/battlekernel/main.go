package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"battlekernel/internal/app"
	"battlekernel/internal/review"
	"battlekernel/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "verify":
			verifyCommand(os.Args[2:])
			return
		case "serve":
			serveCommand(os.Args[2:])
			return
		case "review":
			reviewCommand(os.Args[2:])
			return
		case "generate":
			verifyCommand(os.Args[2:])
			return
		case "-h", "--help", "help":
			usage()
			return
		}
	}
	verifyCommand(os.Args[1:])
}

func usage() {
	fmt.Println(`battlekernel commands:
  verify   Generate deterministic samples and verify all paths (default)
  serve    Start local net/http page
  review   Run independent SQLite/truth-fingerprint reviewer
  generate Alias for verify; sample files are written under --out`)
}

func verifyCommand(args []string) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	out := fs.String("out", "out", "output directory")
	count := fs.Int("count", 3, "number of deterministic battles")
	seed := fs.Int64("seed", 20261125, "fixed seed")
	window := fs.Int("window", 2, "round window size")
	parallel := fs.Int("parallel", 2, "max parallel battle workers")
	buffer := fs.Int("buffer", 1, "per-worker state buffer")
	jsonOut := fs.Bool("json", false, "print full JSON report")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := app.RunVerification(ctx, app.Options{OutDir: *out, Seed: *seed, SampleCount: *count, WindowSize: *window, Parallel: *parallel, BufferPerWorker: *buffer})
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification failed to run: %v\n", err)
		os.Exit(2)
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		printSummary(report)
	}
	if !report.OK {
		os.Exit(1)
	}
}

func printSummary(report *app.VerificationReport) {
	fmt.Printf("battlekernel verification: ok=%v samples=%d seed=%d window=%d\n", report.OK, report.SampleCount, report.Seed, report.WindowSize)
	for _, check := range report.Checks {
		mark := "PASS"
		if !check.OK {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s %s\n", mark, check.Name, check.Detail)
	}
	for _, p := range report.Projects {
		fmt.Printf("  project %s truth=%v realtime=%v resume=%v failure_retry=%v snapshot_reset=%v review=%v\n",
			p.ProjectID, p.TruthVerified, p.RealtimeCompare.Equal, p.ResumeCompare.Equal, p.FailureRetryCompare.Equal, p.SnapshotRecoveryCompare.Equal, p.Review.OK)
		if p.Review.Violation != nil {
			v := p.Review.Violation
			fmt.Printf("    review violation round=%d field=%s expected=%s got=%s\n", v.Round, v.Field, v.Expected, v.Got)
		}
		if p.RealtimeCompare.Difference != nil {
			d := p.RealtimeCompare.Difference
			fmt.Printf("    realtime diff round=%d field=%s expected=%s got=%s\n", d.Round, d.Field, d.Expected, d.Got)
		}
		if p.ResumeCompare.Difference != nil {
			d := p.ResumeCompare.Difference
			fmt.Printf("    resume diff round=%d field=%s expected=%s got=%s\n", d.Round, d.Field, d.Expected, d.Got)
		}
		if p.SnapshotRecoveryCompare.Difference != nil {
			d := p.SnapshotRecoveryCompare.Difference
			fmt.Printf("    snapshot recovery diff round=%d field=%s expected=%s got=%s\n", d.Round, d.Field, d.Expected, d.Got)
		}
	}
	if report.WeightInvalidation != nil {
		fmt.Printf("  weight invalidation: first_round=%d invalidations=%d reused=%v recomputed=%v\n", report.WeightInvalidation.FirstChangedRound, len(report.WeightInvalidation.Invalidations), report.WeightInvalidation.ReusedRounds, report.WeightInvalidation.RecomputedRounds)
	}
	if report.ReorderInvalidation != nil {
		fmt.Printf("  reorder invalidation: first_round=%d invalidations=%d reused=%v recomputed=%v\n", report.ReorderInvalidation.FirstChangedRound, len(report.ReorderInvalidation.Invalidations), report.ReorderInvalidation.ReusedRounds, report.ReorderInvalidation.RecomputedRounds)
	}
	fmt.Printf("  scheduler: workers=%d buffer=%d wait_events=%d max_wait_ms=%d\n", report.Scheduler.MaxWorkers, report.Scheduler.BufferPerWorker, report.Scheduler.WaitEvents, report.Scheduler.MaxWaitMS)
	fmt.Printf("  report: %s\n", filepath.Join(report.OutDir, "reports", "verification-report.json"))
}

func serveCommand(args []string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	out := fs.String("out", "out", "output directory")
	preverify := fs.Bool("preverify", true, "run verification before serving")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *preverify {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		report, err := app.RunVerification(ctx, app.DefaultOptions())
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "preverify error: %v\n", err)
			os.Exit(2)
		}
		printSummary(report)
		if !report.OK {
			os.Exit(1)
		}
	}
	server := &web.Server{OutDir: *out}
	fmt.Printf("serving local page at http://%s\n", *addr)
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := httpServer.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func reviewCommand(args []string) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	db := fs.String("db", "", "path to SQLite database")
	truth := fs.String("truth", filepath.Join("out", "truth-fingerprints.json"), "truth fingerprint JSON")
	projectID := fs.String("project-id", "", "project id; inferred when omitted")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *db == "" {
		fmt.Fprintln(os.Stderr, "--db is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := review.ReviewDatabase(ctx, *db, *truth, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review tool error: %v\n", err)
		os.Exit(2)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	if !report.OK {
		os.Exit(1)
	}
}
