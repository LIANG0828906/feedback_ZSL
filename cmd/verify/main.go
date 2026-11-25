package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"edgerefine/internal/sample"
	"edgerefine/internal/store"
	"edgerefine/internal/verify"
)

func main() {
	sampleDir := flag.String("sample-dir", "samples/step128", "sample directory containing truth.json")
	dbPath := flag.String("db", "checkpoints/edge.db", "SQLite checkpoint path")
	flag.Parse()

	truth, err := sample.LoadManifest(filepath.Join(*sampleDir, "truth.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load truth failed: %v\n", err)
		os.Exit(2)
	}
	fp := sample.Fingerprint(truth)
	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open checkpoint failed: %v\n", err)
		os.Exit(2)
	}
	defer st.Close()
	report, err := verify.Verify(st, truth, fp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
		os.Exit(2)
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	if !report.OK {
		os.Exit(1)
	}
	if report.Accuracy != nil && !report.Accuracy.Pass {
		os.Exit(3)
	}
}
