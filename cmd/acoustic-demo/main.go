package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	acoustic "levelacoustic"
	"levelacoustic/internal/demo"
	"levelacoustic/internal/samplegen"
)

func main() {
	seed := flag.Int64("seed", samplegen.DefaultSeed, "deterministic sample seed")
	serve := flag.Bool("serve", false, "serve the local verification page after the run")
	addr := flag.String("addr", "127.0.0.1:8080", "loopback HTTP listen address")
	databaseDir := flag.String("db-dir", "", "directory for SQLite checkpoint files; empty uses a temporary directory")
	sampleOut := flag.String("sample-out", "", "optional path for the generated sample JSON")
	reportOut := flag.String("report-out", "", "optional path for the full three-path report JSON")
	workers := flag.Int("workers", 4, "maximum reflection worker concurrency")
	buffer := flag.Int("buffer", 2, "maximum result buffer per worker")
	flag.Parse()

	if err := run(*seed, *serve, *addr, *databaseDir, *sampleOut, *reportOut, acoustic.WorkerOptions{
		MaxWorkers:      *workers,
		PerWorkerBuffer: *buffer,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(seed int64, serve bool, addr, databaseDir, sampleOut, reportOut string, workers acoustic.WorkerOptions) error {
	project, err := samplegen.Sample(seed)
	if err != nil {
		return err
	}
	if sampleOut != "" {
		if err := os.MkdirAll(filepath.Dir(sampleOut), 0o755); err != nil && filepath.Dir(sampleOut) != "." {
			return err
		}
		if err := samplegen.WriteJSON(sampleOut, project); err != nil {
			return errors.New("write sample: " + err.Error())
		}
	}
	if databaseDir == "" {
		databaseDir, err = os.MkdirTemp("", "levelacoustic-demo-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(databaseDir)
	} else if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := demo.RunSuite(ctx, project, seed, databaseDir, workers)
	if err != nil {
		return err
	}
	if reportOut != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := os.WriteFile(reportOut, data, 0o644); err != nil {
			return err
		}
	}
	printSummary(report, databaseDir)
	if !report.AllPathsEqual {
		return &acoustic.Error{Code: acoustic.CodeTruth, Op: "three_paths", Message: "preview, resume and full recompute are not identical"}
	}
	if !report.Truth.Passed {
		return report.Truth.Error()
	}
	if !serve {
		return nil
	}
	return serveReport(report, addr)
}

func printSummary(report *demo.SuiteReport, databaseDir string) {
	fmt.Println("== deterministic sample ==")
	fmt.Printf("seed=%d paramsVersion=%d algorithm=%s\n", report.Seed, report.Project.ParamsVersion, report.Project.AlgorithmVersion)
	fmt.Printf("rooms=%d walls=%d doors=%d sources=%d reflectors=%d listeners=%d\n",
		len(report.Project.Rooms), len(report.Project.Walls), len(report.Project.Openings),
		len(report.Project.Sources), len(report.Project.Reflectors), len(report.Project.Listeners))
	fmt.Println("== three paths ==")
	for _, path := range []*demo.PathOutput{report.Preview, report.Resume, report.Full} {
		fmt.Printf("%-7s commits=%v validPaths=%d\n", path.Name, path.Result.CommitSequence, solutionPathCount(path))
		for _, stats := range path.Result.WorkerStats {
			fmt.Printf("        workers<=%d buffer<=%d tasks=%d observed=%d\n",
				stats.ConfiguredWorkers, stats.PerWorkerBuffer, stats.Tasks, stats.ObservedMaxWorkers)
		}
	}
	previewPaths := solutionPathCount(report.Preview)
	resumePaths := solutionPathCount(report.Resume)
	fullPaths := solutionPathCount(report.Full)
	fmt.Printf("path totals preview=%d resume=%d full=%d allEqual=%v\n", previewPaths, resumePaths, fullPaths, report.AllPathsEqual)
	if report.Resume.Restore != nil {
		fmt.Printf("resume loadedRooms=%v invalidated=%v fullRecompute=%v\n",
			report.Resume.Restore.LoadedRooms, report.Resume.Restore.Invalidated, report.Resume.Restore.FullRecompute)
	}
	for _, comparison := range report.Comparisons {
		fmt.Printf("compare %s-vs-%s equal=%v differences=%d\n", comparison.Left, comparison.Right, comparison.Equal, len(comparison.Differences))
	}
	fmt.Println("== truth and error bounds ==")
	fmt.Printf("truth passed=%v expectedPaths=%d actualPaths=%d\n", report.Truth.Passed, report.Truth.ExpectedPaths, report.Truth.ActualPaths)
	fmt.Printf("maxCumulativeUpperBoundDb=%.6f pass@1dB=%d pass@0.1dB=%d pass@0.01dB=%d\n",
		report.ErrorBounds.MaxCumulativeDB, report.ErrorBounds.PassAtOneDB,
		report.ErrorBounds.PassAtPointOneDB, report.ErrorBounds.PassAtPointZeroOneDB)
	fmt.Println("== invalidation ==")
	if report.Invalidation != nil {
		fmt.Printf("connectivityChanged=%v invalidatedRooms=%v reusedRooms=%v dependencyEdges=%d\n",
			report.Invalidation.ConnectivityChanged, report.Invalidation.InvalidatedRooms,
			report.Invalidation.ReusedRooms, len(report.Invalidation.Dependencies))
	} else {
		fmt.Println(report.InvalidationError)
	}
	fmt.Printf("databaseDir=%s\n", databaseDir)
}

func solutionPathCount(path *demo.PathOutput) int {
	if path == nil || path.Result == nil {
		return 0
	}
	pathCount := 0
	for _, solution := range path.Result.ListenerSolutions {
		pathCount += solution.ValidPathCount
	}
	return pathCount
}

func serveReport(report *demo.SuiteReport, addr string) error {
	if err := validateLoopbackAddr(addr); err != nil {
		return err
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           demo.NewServer(report).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("serving http://%s/\n", listener.Addr().String())
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalCh)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalCh:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
}

func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address must use loopback, got %q", host)
	}
	return nil
}
