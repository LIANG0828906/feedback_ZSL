package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"screenverify/internal/review"
)

func main() {
	db := flag.String("db", filepath.Join("artifacts", "experiment-dbs", "realtime.db"), "persistent SQLite evidence database")
	truth := flag.String("truth", filepath.Join("data", "sample", "noise", "truth.json"), "truth manifest used only to verify fingerprint and tolerance")
	out := flag.String("out", filepath.Join("artifacts", "review-noise.json"), "review result JSON")
	flag.Parse()

	result, err := review.Run(*db, *truth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "E_REVIEW_IO:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err == nil {
			_ = os.WriteFile(*out, b, 0o644)
		}
	}
	fmt.Println(string(b))
	if result.Violation != nil {
		os.Exit(2)
	}
}
