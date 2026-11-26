package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	quantforensics "quantforensics"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "diagnose":
		diagnose(os.Args[2:])
	case "serve":
		serve(os.Args[2:])
	case "version":
		fmt.Println("quantforensics 1.0.0 (algo=" + quantforensics.AlgoVersion + ")")
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: forensics <diagnose|serve|version> [flags]")
}

func diagnose(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	out := fs.String("out", filepath.Join("artifacts", "diagnostics"), "diagnostic HTML/JSON output directory")
	db := fs.String("db", filepath.Join("artifacts", "checkpoints"), "writable SQLite checkpoint directory")
	_ = fs.Parse(args)

	report, err := quantforensics.WriteDiagnostics(*out, *db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnose failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("diagnostic report: %s\n", filepath.Join(*out, "diagnostic-report.json"))
	fmt.Printf("diagnostic panel:  %s\n", filepath.Join(*out, "index.html"))
	fmt.Printf("schema interop:    %d/3 semantic round trips equal\n", len(report.SchemaInterop))
	fmt.Printf("three paths:       baseline=%v edited=%v\n", report.BaselineThreePaths.Consistent, report.EditedThreePaths.Consistent)
	fmt.Printf("blocks/images:     %d/%d\n", report.BlockCount, report.ImageCount)
	for _, step := range report.Invalidation.Steps {
		fmt.Printf("invalidation %-18s affected=%d reused=%d computed=%d\n", step.Name, len(step.Report.AffectedBlocks), len(step.Report.ReusedBlocks), len(step.ComputedBlocks))
	}
	fmt.Printf("fault retry ok:    %v\n", report.FaultRetry.OK)
	if !report.OK {
		for _, e := range report.ValidationErrors {
			fmt.Fprintf(os.Stderr, "validation error: %s\n", e)
		}
		os.Exit(1)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join("artifacts", "diagnostics"), "diagnostic output directory")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	_ = fs.Parse(args)

	index := filepath.Join(*dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		fmt.Fprintf(os.Stderr, "diagnostic index not found at %s; run diagnose first\n", index)
		os.Exit(1)
	}
	fmt.Printf("serving diagnostic interface at http://%s (dir=%s)\n", *addr, *dir)
	if err := http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))); err != nil {
		fmt.Fprintf(os.Stderr, "serve failed: %v\n", err)
		os.Exit(1)
	}
}
