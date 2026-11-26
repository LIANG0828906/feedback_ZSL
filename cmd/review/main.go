package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"maskqc"
)

func main() {
	aPath := flag.String("a", "artifacts/workbench/path-live.json", "first path result JSON")
	bPath := flag.String("b", "artifacts/workbench/path-recovery.json", "second path result JSON")
	flag.Parse()

	a, err := loadResult(*aPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review: load -a: %v\n", err)
		os.Exit(2)
	}
	b, err := loadResult(*bPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review: load -b: %v\n", err)
		os.Exit(2)
	}
	mismatch := maskqc.CompareResults(a, b)
	if mismatch == nil {
		fmt.Printf("REVIEW OK: %s == %s on mask pixels, domain stats, cache keys and commit sequence\n", *aPath, *bPath)
		return
	}
	fmt.Printf("FIRST MISMATCH chunk=%d field=%s want=%q got=%q\n", mismatch.Domain, mismatch.Item, mismatch.Want, mismatch.Got)
	os.Exit(1)
}

func loadResult(path string) (*maskqc.RunResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r maskqc.RunResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
