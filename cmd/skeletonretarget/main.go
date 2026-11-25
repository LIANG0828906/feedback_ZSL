package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"skeletonretarget/internal/demo"
	"skeletonretarget/internal/webui"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "verify":
		err = runVerify(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("usage: skeletonretarget <demo|serve|verify> [flags]")
	fmt.Println("  demo    build the deterministic sample and print path results")
	fmt.Println("  serve   run the loopback-only diagnostic page")
	fmt.Println("  verify  run the local acceptance comparison")
}

func runDemo(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic generator seed")
	workDir := flags.String("workdir", "", "persistent work directory (default: temp)")
	jsonOut := flags.Bool("json", false, "emit the diagnostic report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result, err := demo.Build(ctx, demo.Options{Seed: *seed, WorkDir: *workDir})
	if err != nil {
		return err
	}
	defer result.Cleanup()
	if *jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result.Data)
	}
	printSummary(result.Data)
	return nil
}

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic generator seed")
	addr := flags.String("addr", "127.0.0.1:0", "loopback listen address")
	workDir := flags.String("workdir", "", "persistent work directory (default: temp)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result, err := demo.Build(ctx, demo.Options{Seed: *seed, WorkDir: *workDir})
	if err != nil {
		return err
	}
	defer result.Cleanup()
	handler, err := webui.NewHandler(result.Data)
	if err != nil {
		return err
	}
	return webui.Serve(ctx, *addr, handler)
}

func runVerify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic generator seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result, err := demo.Build(ctx, demo.Options{Seed: *seed})
	if err != nil {
		return err
	}
	defer result.Cleanup()
	printSummary(result.Data)
	if !result.Data.Edit.Comparison.Equal {
		return fmt.Errorf("edited incremental output differs from full recomputation")
	}
	for _, comparison := range result.Data.Comparisons {
		if !comparison.Equal {
			return fmt.Errorf("%s vs %s differ: %v", comparison.Left, comparison.Right, comparison.Mismatches)
		}
	}
	for _, path := range result.Data.Paths {
		if !path.Complete {
			return fmt.Errorf("path %s is incomplete", path.Name)
		}
	}
	fmt.Println("verification: PASS")
	return nil
}

func printSummary(data *demo.Data) {
	fmt.Printf("seed=%d algorithm=%s interval=%d\n", data.Seed, data.AlgorithmVersion, data.GeneratedInterval)
	for _, format := range data.Formats {
		fmt.Printf("format=%s version=%s action=%s frames=%d conversions=%d\n",
			format.Name, format.SourceVersion, format.ActionHash[:12], format.Frames, format.Conversions)
	}
	for _, path := range data.Paths {
		fmt.Printf("path=%s complete=%t frames=%d hits=%d recomputed=%d recovered=%d digest=%s\n",
			path.Name, path.Complete, path.Frames, path.CacheHits, path.Recomputed, path.RecoveredTo, path.Digest[:12])
	}
	for _, comparison := range data.Comparisons {
		fmt.Printf("compare=%s/%s equal=%t\n", comparison.Left, comparison.Right, comparison.Equal)
	}
	fmt.Printf("edit invalidations=%d cacheHits=%d recomputed=%d fullEqualsIncremental=%t\n",
		len(data.Edit.Invalidations), data.Edit.Hits, data.Edit.Recomputed, data.Edit.Comparison.Equal)
}
