package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"audiocheck/internal/generator"
	"audiocheck/internal/verify"
)

func main() {
	db := flag.String("db", "", "path to SQLite checkpoint")
	fingerprint := flag.String("truth-fingerprint", "", "expected truth fingerprint")
	truthPath := flag.String("truth", "", "optional truth.json path")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "--db is required")
		os.Exit(2)
	}
	fp := *fingerprint
	if *truthPath != "" {
		var truth generator.Truth
		b, err := os.ReadFile(*truthPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(b, &truth); err != nil {
			fatal(err)
		}
		fp = truth.Fingerprint
	}
	result, err := verify.VerifyDatabase(*db, fp)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
	if !result.OK {
		os.Exit(3)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "audio-verify:", err)
	os.Exit(1)
}
