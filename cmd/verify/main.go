package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"channelpipe/internal/independent"
)

func main() {
	sampleDir := flag.String("sample", "", "generated sample directory containing manifest.json and truth/truth.json")
	dbPath := flag.String("db", "", "SQLite checkpoint path")
	out := flag.String("out", "", "verification report JSON output path")
	flag.Parse()
	if *sampleDir == "" || *dbPath == "" {
		fmt.Fprintln(os.Stderr, "usage: verify --sample <dir> --db <sqlite> [--out report.json]")
		os.Exit(64)
	}
	report, err := independent.Verify(*sampleDir, *dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "E_VERIFY_RUN: %v\n", err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err == nil {
			_ = os.WriteFile(*out, data, 0o644)
		}
	}
	fmt.Println(string(data))
	if !report.OK {
		os.Exit(2)
	}
}
