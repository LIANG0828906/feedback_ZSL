package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"samplingkernel/internal/demo"
	"samplingkernel/internal/exceptions"
	"samplingkernel/internal/loader"
	"samplingkernel/internal/model"
	"samplingkernel/internal/sample"
	"samplingkernel/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		runDemo(nil)
		return
	}
	command := os.Args[1]
	switch command {
	case "demo":
		fs := flag.NewFlagSet("demo", flag.ExitOnError)
		out := fs.String("out", "artifacts/demo", "directory for generated JSON and SQLite files")
		db := fs.String("db", "", "SQLite checkpoint path")
		_ = fs.Parse(os.Args[2:])
		if err := demo.Run(os.Stdout, *out, *db); err != nil {
			fatal(err)
		}
	case "verify":
		report, err := verify.Run()
		if err != nil {
			fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report.Checks)
		if !report.OK() {
			os.Exit(1)
		}
	case "inject":
		fs := flag.NewFlagSet("inject", flag.ExitOnError)
		dir := fs.String("dir", "artifacts/exceptions-cli", "directory for temporary checkpoint files")
		_ = fs.Parse(os.Args[2:])
		bundle, err := sample.Generate()
		if err != nil {
			fatal(err)
		}
		results, err := exceptions.All(context.Background(), bundle.Project, bundle.Docs[model.SchemaV1], *dir)
		if err != nil {
			fatal(err)
		}
		for _, result := range results {
			fmt.Printf("%s\t%s\t%s\n", result.Code, result.Name, result.Path)
		}
	case "load":
		if len(os.Args) < 3 {
			fatal(fmt.Errorf("usage: samplingkernel load <project.json>"))
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			fatal(err)
		}
		project, err := loader.Load(data)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("loaded %s: services=%d edges=%d events=%d allocations=%d\n", project.SchemaVersion, len(project.Services), len(project.Edges), len(project.Events), len(project.Allocations))
	case "help", "-h", "--help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		printHelp()
		os.Exit(2)
	}
}

func runDemo(args []string) {
	if err := demo.Run(os.Stdout, "artifacts/demo", ""); err != nil {
		fatal(err)
	}
}

func printHelp() {
	fmt.Println(`samplingkernel - deterministic sampling and trace-merge kernel

Usage:
  go run . demo [--out DIR] [--db PATH]
  go run . verify
  go run . inject [--dir DIR]
  go run . load <project.json>

The default command is demo.`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
