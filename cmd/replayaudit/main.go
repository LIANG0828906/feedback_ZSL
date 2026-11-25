// Command replayaudit generates deterministic multi-format replay samples and
// runs the full audit-kernel acceptance harness.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"replayaudit/internal/audit"
	"replayaudit/internal/gen"
)

func main() {
	cmd := "all"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "gen":
		dir := "samples"
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		idx, err := gen.GenerateTree(dir)
		fatal(err)
		fmt.Printf("generated %d replays (seed=%d) under %s\n", len(idx.Replays), idx.Seed, dir)
	case "verify", "all", "":
		samplesDir := "samples"
		if v := os.Getenv("REPLAY_SAMPLES_DIR"); v != "" {
			samplesDir = v
		}
		reportPath := filepath.Join("artifacts", "verification-report.json")
		if v := os.Getenv("REPLAY_REPORT_PATH"); v != "" {
			reportPath = v
		}
		dbDir := filepath.Join("artifacts", "checkpoint-dbs")
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			fatal(err)
		}
		rep, err := audit.RunAll(samplesDir, dbDir)
		fatal(err)
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(reportPath, data, 0o644); err != nil {
			fatal(err)
		}
		printReport(rep, reportPath)
		if !rep.AllOK {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (use: all|gen [dir])\n", cmd)
		os.Exit(2)
	}
}

func printReport(rep *audit.VerificationReport, path string) {
	fmt.Println("== replay audit verification ==")
	for _, r := range rep.Replays {
		fmt.Printf("  %-16s events=%-2d segments=%-2d winner=%-5s truth=%v three_paths=%v pool_peak=%d\n",
			r.ReplayID, r.Events, r.Segments, orDash(r.Winner), r.TruthOK, r.ThreePathOK, r.PoolPeak)
	}
	passed := 0
	for _, c := range rep.Checks {
		mark := "PASS"
		if c.OK {
			passed++
		} else {
			mark = "FAIL"
		}
		line := fmt.Sprintf("  [%s] %s", mark, c.Name)
		if c.Code != "" {
			line += " (" + c.Code + ")"
		}
		fmt.Println(line)
		if c.Detail != "" {
			fmt.Println("         " + c.Detail)
		}
		if c.Mismatch != nil {
			fmt.Println("         mismatch: " + c.Mismatch.Error())
		}
	}
	fmt.Printf("checks: %d/%d passed; report=%s\n", passed, len(rep.Checks), path)
	if rep.AllOK {
		fmt.Println("ACCEPTANCE: OK")
	} else {
		fmt.Println("ACCEPTANCE: FAILED")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
