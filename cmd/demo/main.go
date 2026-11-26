package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"downsample"
)

func main() {
	out := flag.String("out", "artifacts/demo", "demo output directory")
	inject := flag.String("inject", "", "run one injection case; use 'list' to list cases")
	verify := flag.Bool("verify", false, "exit non-zero if any demonstration check fails")
	signalDemo := flag.Bool("signal-demo", false, "stream points slowly until SIGINT/SIGTERM")
	flag.Parse()

	if *inject == "list" {
		for _, name := range downsample.InjectionCases() {
			fmt.Println(name)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *signalDemo {
		if err := runSignalDemo(ctx, *out); err != nil {
			coded := downsample.AsError(err)
			writeJSON(coded.InjectionResult("signal_demo"))
			os.Exit(1)
		}
		return
	}

	if *inject != "" {
		bundle, err := downsample.GenerateBundle()
		if err != nil {
			fatal(err)
		}
		ierr := downsample.RunInjection(bundle, *inject, filepath.Join(*out, "injections"))
		if ierr == nil {
			writeJSON(map[string]any{"case": *inject, "code": downsample.CodeOK})
			return
		}
		coded := downsample.AsError(ierr)
		writeJSON(coded.InjectionResult(*inject))
		if coded.Code == downsample.CodeInjectionUnknown {
			os.Exit(2)
		}
		return
	}

	report, err := downsample.RunDemo(*out)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("demo output: %s\n", *out)
	fmt.Printf("windows=%d injections=%d all_passed=%v\n", report.WindowCount, len(report.Injections), report.AllPassed)
	fmt.Printf("report: %s\n", filepath.Join(*out, "report.md"))
	for _, check := range report.Checks {
		fmt.Println(check)
	}
	if *verify && !report.AllPassed {
		os.Exit(1)
	}
}

func runSignalDemo(ctx context.Context, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	bundle, err := downsample.GenerateBundle()
	if err != nil {
		return err
	}
	path := filepath.Join(outDir, "signal-stream.sqlite")
	_ = os.Remove(path)
	session, coded := downsample.NewSession(bundle.Project, downsample.DefaultBudget(), path)
	if coded != nil {
		return coded
	}
	defer session.Close()

	points := make([]downsample.Point, 0, 60)
	base := bundle.BaseTimeMS
	for i := 0; i < 60; i++ {
		points = append(points, downsample.Point{
			MetricID:    "cpu.usage",
			TimestampMS: base + (9 * 60_000) + int64(i)*1000,
			Value:       50 + float64(i%10),
			Weight:      1000 + int64(i%5)*500,
		})
	}

	done := make(chan struct{})
	var report *downsample.RecomputeReport
	var appendErr *downsample.Error
	go func() {
		defer close(done)
		for i := range points {
			if ctx.Err() != nil {
				appendErr = downsample.NewError(downsample.CodeStreamInterrupted, "signal received; stopped before next commit").
					WithContext("committed_seq", session.Engine.GlobalSequence())
				return
			}
			report, appendErr = session.AppendAndCheckpoint(ctx, points[i])
			if appendErr != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	<-done
	seq := session.Engine.GlobalSequence()
	result := map[string]any{
		"code":          downsample.CodeOK,
		"checkpoint":    path,
		"committed_seq": seq,
		"message":       "stream stopped at last complete commit point",
	}
	if appendErr != nil {
		result["code"] = appendErr.Code
		result["error"] = appendErr.Error()
		result["context"] = appendErr.Context
	}
	if report != nil {
		result["last_recomputed"] = len(report.Recomputed)
	}
	writeJSON(result)
	if appendErr != nil && appendErr.Code != downsample.CodeStreamInterrupted {
		return appendErr
	}
	return nil
}

func writeJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func fatal(err error) {
	coded := downsample.AsError(err)
	data, _ := json.MarshalIndent(coded, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
	os.Exit(1)
}
