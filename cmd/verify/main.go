package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"iconbench/internal/verify"
)

func main() {
	dataDir := flag.String("data", "data", "directory containing manifest.json and generated samples")
	dbPath := flag.String("db", "", "SQLite checkpoint path (default: <data>/checkpoints.db)")
	imageID := flag.String("image", "", "image id; empty verifies every image")
	asJSON := flag.Bool("json", false, "emit machine-readable JSON")
	flag.Parse()

	var reports []*verify.Report
	var err error
	if *imageID == "" {
		reports, err = verify.VerifyAll(*dataDir, *dbPath)
	} else {
		reports, err = verifyOne(*dataDir, *dbPath, *imageID)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "E_VERIFY:", err)
		os.Exit(2)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reports)
	} else {
		for _, r := range reports {
			if r.OK {
				fmt.Printf("PASS %s mask=%s commits=%d iou=%.4f centroid=%.3f area=%.3f\n", r.ImageID, short(r.MaskHash), r.SequenceCount, r.Metrics.IoU, r.Metrics.MaxCentroidErrorPx, r.Metrics.MaxAreaRelativeError)
			} else if r.Violation != nil {
				v := r.Violation
				fmt.Printf("FAIL %s block=(%d,%d) layer=%s field=%s: %s [expected=%v actual=%v]\n", r.ImageID, v.BlockX, v.BlockY, v.Layer, v.Field, v.Message, v.Expected, v.Actual)
			} else {
				fmt.Printf("FAIL %s: unknown verification error\n", r.ImageID)
			}
		}
	}
	for _, r := range reports {
		if !r.OK {
			os.Exit(1)
		}
	}
}

func verifyOne(dataDir, dbPath, imageID string) ([]*verify.Report, error) {
	// VerifyAll is cheap on local deterministic samples and opens the DB once.
	all, err := verify.VerifyAll(dataDir, dbPath)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.ImageID == imageID {
			return []*verify.Report{r}, nil
		}
	}
	return nil, fmt.Errorf("image not found: %s", imageID)
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
