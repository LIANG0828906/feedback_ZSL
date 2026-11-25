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

	"volumefog"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "diagnostic workbench listen address")
	dbPath := flag.String("db", filepath.Join("artifacts", "workbench.db"), "SQLite checkpoint path")
	verify := flag.Bool("verify", false, "run deterministic local verification and write JSON report")
	reportPath := flag.String("report", filepath.Join("artifacts", "verification-report.json"), "verification report output path")
	flag.Parse()

	if *verify {
		if err := runVerify(*reportPath); err != nil {
			fmt.Fprintln(os.Stderr, "verify failed:", err)
			os.Exit(1)
		}
		return
	}

	workbench, err := fog.NewWorkbench(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start workbench failed:", err)
		os.Exit(1)
	}
	defer workbench.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           workbench.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("volume fog diagnostic workbench: http://%s\n", *addr)
	fmt.Printf("sqlite checkpoint: %s\n", *dbPath)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

func runVerify(reportPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := fog.RunVerificationReport(ctx, "artifacts")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		return err
	}
	file, err := os.Create(reportPath)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	fmt.Printf("verification success=%v report=%s\n", report.Success, reportPath)
	fmt.Printf("precision normalized deviation=%.6f monotonic=%v\n", report.Precision.FullBudget.NormalizedDeviation, report.Precision.Monotonic)
	fmt.Printf("three-way equal=%v scheduler_tiles=%d queue_records=%d\n", report.ThreeWay.Equal, report.SchedulerTiles, report.QueueRecords)
	for code, check := range report.Injections {
		fmt.Printf("injection %s passed=%v actual=%s\n", code, check.Passed, check.ActualCode)
	}
	if !report.Success {
		if report.Failure != nil {
			fmt.Fprintln(os.Stderr, report.Failure.Error())
		}
		os.Exit(2)
	}
	return nil
}
