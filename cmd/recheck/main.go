package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"dialoguefit/checkpoint"
	"dialoguefit/projectio"
	"dialoguefit/recheck"
)

func main() {
	db := flag.String("db", "artifacts/local-demo/checkpoints.db", "SQLite segment checkpoint path")
	truth := flag.String("truth", "artifacts/local-demo/final/truth.json", "truth manifest path")
	jsonOut := flag.Bool("json", false, "emit full JSON report")
	flag.Parse()

	truthFile, err := projectio.LoadTruthFile(*truth)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer store.Close()

	report := recheck.Run(context.Background(), store, truthFile)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Printf("ok=%v dialogues=%d frames=%d stress=%d segments=%d fingerprints=%d\n",
			report.OK, report.CheckedDialogues, report.CheckedFrames,
			report.CheckedStressMarks, report.CheckedSegments, report.FingerprintsRecomputed)
	}
	if !report.OK {
		if report.FirstViolation != nil {
			fmt.Fprintf(os.Stderr, "first violation dialogue_id=%s field=%s: %s\n",
				report.FirstViolation.DialogueID, report.FirstViolation.Field, report.FirstViolation.Message)
		}
		os.Exit(1)
	}
}
