package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/verify"
)

func main() {
	manifestPath := flag.String("manifest", "data/manifest.json", "path to the deterministic truth manifest")
	realtimePath := flag.String("realtime", "", "persisted realtime run JSON")
	resumePath := flag.String("resume", "", "persisted resumed run JSON")
	fullPath := flag.String("full", "", "persisted full-recompute run JSON")
	dbPath := flag.String("db", "", "SQLite checkpoint database to verify from persisted unit rows")
	snapshotID := flag.Int64("snapshot", 0, "optional explicit checkpoint snapshot id")
	jsonOut := flag.Bool("json", false, "print machine-readable issue list")
	flag.Parse()

	manifest, err := loadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ocrverify:", err)
		os.Exit(2)
	}
	var issues []verify.Issue
	checked := 0
	if *realtimePath != "" || *resumePath != "" || *fullPath != "" {
		runs := map[string]string{"realtime": *realtimePath, "resume": *resumePath, "full": *fullPath}
		loaded := map[string]core.RunResult{}
		for name, path := range runs {
			if path == "" {
				continue
			}
			run, err := verify.LoadRun(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ocrverify: load %s run: %v\n", name, err)
				os.Exit(2)
			}
			loaded[name] = run
			issues = append(issues, verify.VerifyRun(run, manifest)...)
			checked++
		}
		if realtime, ok := loaded["realtime"]; ok {
			if resumed, ok := loaded["resume"]; ok {
				issues = append(issues, verify.CompareRuns(realtime, resumed)...)
			}
			if full, ok := loaded["full"]; ok {
				issues = append(issues, verify.CompareRuns(realtime, full)...)
			}
		}
	}
	if *dbPath != "" {
		dbIssues, count, err := verifyDB(*dbPath, *snapshotID, manifest)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ocrverify:", err)
			os.Exit(2)
		}
		issues = append(issues, dbIssues...)
		checked += count
	}
	if checked == 0 {
		fmt.Fprintln(os.Stderr, "ocrverify: provide -db or at least one of -realtime/-resume/-full")
		os.Exit(2)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			OK      bool                             `json:"ok"`
			Checked int                              `json:"checked"`
			Issues  []verify.Issue                   `json:"issues"`
			Metrics map[string][]verify.ImageMetrics `json:"metrics,omitempty"`
		}{OK: len(issues) == 0, Checked: checked, Issues: issues, Metrics: metricMap(*realtimePath, *resumePath, *fullPath, manifest)})
	} else if len(issues) == 0 {
		fmt.Printf("OK: verified %d persisted run(s)\n", checked)
	} else {
		fmt.Println(verify.FormatIssues(issues))
	}
	if len(issues) > 0 {
		os.Exit(1)
	}
}

func metricMap(realtime, resume, full string, manifest core.Manifest) map[string][]verify.ImageMetrics {
	out := map[string][]verify.ImageMetrics{}
	for name, path := range map[string]string{"realtime": realtime, "resume": resume, "full": full} {
		if path == "" {
			continue
		}
		run, err := verify.LoadRun(path)
		if err == nil {
			out[name] = verify.Summarize(run, manifest)
		}
	}
	return out
}

func loadManifest(path string) (core.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return core.Manifest{}, err
	}
	var manifest core.Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return core.Manifest{}, err
	}
	return manifest, nil
}

func verifyDB(dbPath string, requested int64, manifest core.Manifest) ([]verify.Issue, int, error) {
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	ctx := context.Background()
	var issues []verify.Issue
	count := 0
	for _, truth := range manifest.Images {
		snapshotID := requested
		if snapshotID == 0 {
			var valid bool
			snapshotID, valid, err = db.LatestSnapshotID(ctx, truth.ID)
			if err != nil {
				return nil, count, err
			}
			if snapshotID == 0 || !valid {
				issues = append(issues, verify.Issue{ImageID: truth.ID, Line: -1, Field: "snapshot", Detail: "no valid snapshot found"})
				continue
			}
		}
		snapshot, err := db.LoadSnapshot(ctx, snapshotID)
		if err != nil {
			if errors.Is(err, store.ErrSnapshotInvalid) {
				issues = append(issues, verify.Issue{ImageID: truth.ID, Line: -1, Field: "snapshot", Detail: err.Error()})
				continue
			}
			return nil, count, fmt.Errorf("%s snapshot %d: %w", truth.ID, snapshotID, err)
		}
		if snapshot.Key.ImageID != truth.ID {
			issues = append(issues, verify.Issue{ImageID: truth.ID, Line: -1, Field: "snapshot_image", Detail: snapshot.Key.ImageID})
			continue
		}
		if !snapshot.Complete {
			issues = append(issues, verify.Issue{ImageID: truth.ID, Line: -1, Field: "snapshot_complete", Detail: "snapshot is not complete"})
		}
		result := verify.SnapshotRun(snapshot, truth)
		issues = append(issues, verify.VerifyRun(core.RunResult{
			RunKind: "snapshot", TemplateVersion: snapshot.Key.TemplateVersion,
			ParameterVersion: snapshot.Key.ParameterVersion, AlgorithmVersion: snapshot.Key.AlgorithmVersion,
			Images: []core.ImageResult{result},
		}, core.Manifest{Images: []core.ImageTruth{truth}, TruthFingerprint: manifest.TruthFingerprint})...)
		count++
	}
	return issues, count, nil
}
