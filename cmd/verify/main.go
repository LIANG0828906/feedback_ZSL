package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"maskpreview/internal/verifier"
)

func main() {
	manifestDir := flag.String("manifest-dir", "samples", "directory containing manifest.json")
	db := flag.String("db", "artifacts/full.db", "SQLite checkpoint database")
	id := flag.String("id", "", "optional sample id")
	asJSON := flag.Bool("json", false, "print machine-readable JSON")
	flag.Parse()

	report, err := verifier.Verify(*manifestDir, *db, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		os.Exit(2)
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(raw))
	} else {
		fmt.Printf("verification: %s\n", report.Status)
		for _, sample := range report.Samples {
			centroidDeviation, areaRelative := 0.0, 0.0
			if sample.TruthComparison != nil {
				centroidDeviation = sample.TruthComparison.MaxCentroidDeviation
				areaRelative = sample.TruthComparison.MaxAreaRelative
			}
			maskPrefix := sample.MaskHash
			if len(maskPrefix) > 16 {
				maskPrefix = maskPrefix[:16]
			}
			fmt.Printf("%s status=%s stages=%d mask=%s truth_mask_hash_match=%t regions=%d/%d centroid=%.4f area_rel=%.4f\n",
				sample.SampleID, sample.Status, sample.StageCount, maskPrefix,
				sample.MaskHashMatches, sample.RegionCount, sample.TruthCount,
				centroidDeviation, areaRelative)
			if sample.Error != "" {
				fmt.Printf("  error: %s\n", sample.Error)
			}
		}
	}
	if report.Status != "pass" {
		os.Exit(1)
	}
}
