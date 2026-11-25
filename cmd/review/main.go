package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"battlekernel/internal/review"
)

func main() {
	db := flag.String("db", "", "persistent SQLite checkpoint path")
	truth := flag.String("truth", "out/truth-fingerprints.json", "truth fingerprint file")
	projectID := flag.String("project-id", "", "project ID (optional for single-project DB)")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "usage: review --db <sqlite-path> [--truth fingerprints.json] [--project-id id]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := review.ReviewDatabase(ctx, *db, *truth, *projectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "E_REVIEW: %v\n", err)
		os.Exit(2)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	if report.OK {
		fmt.Println("REVIEW_OK: snapshot hashes, independent recalculation and truth fingerprints match")
		return
	}
	v := report.Violation
	if v != nil {
		fmt.Printf("E_REVIEW_FIRST_VIOLATION round=%d field=%s source=%s expected=%s got=%s\n", v.Round, v.Field, v.Source, v.Expected, v.Got)
	}
	os.Exit(1)
}
