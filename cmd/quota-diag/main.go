package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"quotakernel/internal/kernel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "generate":
		err = runGenerate(args)
	case "verify":
		err = runVerify(args)
	case "diagnose":
		err = runDiagnose(args)
	case "all", "demo":
		err = runAll(args)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		if be, ok := kernel.AsBizError(err); ok {
			fmt.Fprintf(os.Stderr, "%s: %s (%s)\n", be.Code, be.Message, be.Path)
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`quota-diag <generate|verify|diagnose|all> [flags]

  generate  Write deterministic v1/v2/v3 samples and truth JSON.
  verify    Run loader, path equivalence, incremental and checkpoint checks.
  diagnose  Run the stable E_xxx exception injection suite.
  all       Generate samples then run full verification.`)
}

func commonFlags(args []string) (seed int64, out, artifacts string, err error) {
	fs := flag.NewFlagSet("quota-diag", flag.ContinueOnError)
	fs.Int64Var(&seed, "seed", 42, "deterministic generator seed")
	fs.StringVar(&out, "out", "samples", "sample output directory")
	fs.StringVar(&artifacts, "artifacts", "artifacts", "verification/checkpoint output directory")
	err = fs.Parse(args)
	return seed, out, artifacts, err
}

func runGenerate(args []string) error {
	seed, out, _, err := commonFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	paths, err := WriteSamples(seed, out)
	if err != nil {
		return err
	}
	fmt.Printf("generated seed=%d:\n", seed)
	for _, p := range paths {
		fmt.Println(" -", p)
	}
	return nil
}

func runVerify(args []string) error {
	seed, _, artifacts, err := commonFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	report, err := kernel.RunFullVerification(seed, artifacts)
	if err != nil {
		return err
	}
	path := filepath.Join(artifacts, "verification-report.json")
	if err := writeJSON(path, report); err != nil {
		return err
	}
	printVerification(report)
	if !report.AllOK {
		return fmt.Errorf("verification failed; see %s", path)
	}
	fmt.Println("verification report:", path)
	return nil
}

func runDiagnose(args []string) error {
	seed, _, artifacts, err := commonFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	dir := filepath.Join(artifacts, "diagnostics")
	report, err := kernel.RunDiagnostics(seed, dir)
	if err != nil {
		return err
	}
	path := filepath.Join(artifacts, "diagnostic-report.json")
	if err := writeJSON(path, report); err != nil {
		return err
	}
	for _, c := range report.Cases {
		status := "PASS"
		if !c.OK {
			status = "FAIL"
		}
		fmt.Printf("%-4s %-38s expected=%-32s got=%s\n", status, c.Name, c.ExpectedCode, c.Code)
	}
	fmt.Println("diagnostic report:", path)
	if !report.AllOK {
		return fmt.Errorf("diagnostic suite failed")
	}
	return nil
}

func runAll(args []string) error {
	seed, out, artifacts, err := commonFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if _, err := WriteSamples(seed, out); err != nil {
		return err
	}
	return runVerify([]string{"--seed", fmt.Sprint(seed), "--out", out, "--artifacts", artifacts})
}

func WriteSamples(seed int64, out string) ([]string, error) {
	bundle, err := kernel.GenerateBundle(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	files := []struct {
		name string
		v    any
	}{
		{"project-v1.json", bundle.V1},
		{"project-v2.json", bundle.V2},
		{"project-v3.json", bundle.V3},
		{"truth.json", bundle.Truth},
		{"critical-cases.json", bundle.Critical},
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := filepath.Join(out, f.name)
		if err := writeJSON(path, f.v); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	summary := tickSummary(bundle.Truth)
	summaryPath := filepath.Join(out, "tick-summary.txt")
	if err := os.WriteFile(summaryPath, []byte(summary), 0o644); err != nil {
		return nil, err
	}
	paths = append(paths, summaryPath)
	return paths, nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tickSummary(r *kernel.RunReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project=%s path=%s ticks=%d\n", r.ProjectID, r.Path, len(r.Ticks))
	for _, t := range r.Ticks {
		admitted, queued, rejected := 0, 0, 0
		for _, a := range t.Allocations {
			switch a.Decision {
			case kernel.DecisionAdmit:
				admitted++
			case kernel.DecisionQueue:
				queued++
			case kernel.DecisionReject:
				rejected++
			}
		}
		fmt.Fprintf(&b, "tick=%d order=%d allocations=%d admit=%d queue=%d reject=%d queued_items=%d\n",
			t.Tick, len(t.AdmissionOrder), len(t.Allocations), admitted, queued, rejected, len(t.Queues))
	}
	return b.String()
}

func printVerification(r *kernel.VerificationReport) {
	fmt.Printf("schema_equivalent=%v\n", r.SchemaEquivalent)
	if r.PathEquivalence != nil {
		fmt.Printf("path_equivalence exact/pre/parallel=%v/%v/%v allocations=%d\n",
			true, r.PathEquivalence.PreDeductEqual, r.PathEquivalence.ParallelEqual, r.PathEquivalence.AllocationCount)
	}
	for _, c := range r.Incremental {
		fmt.Printf("incremental %-18s equal=%v recomputed=%v reused=%d closure=%d\n",
			c.Name, c.EqualFullRecompute, c.Invalidation.RecomputedTicks, len(c.Invalidation.ReusedTicks), len(c.Invalidation.Closure))
	}
	if r.Checkpoint != nil {
		fmt.Printf("checkpoint tick=%d roundtrip=%v suffix_resume=%v path=%s\n",
			r.Checkpoint.SavedTick, r.Checkpoint.RoundTripOK, r.Checkpoint.ResumeSuffixOK, r.Checkpoint.Path)
	}
	if r.Diagnostics != nil {
		pass := 0
		for _, c := range r.Diagnostics.Cases {
			if c.OK {
				pass++
			}
		}
		fmt.Printf("diagnostics=%d/%d passing\n", pass, len(r.Diagnostics.Cases))
	}
	fmt.Printf("ALL_OK=%v\n", r.AllOK)
}
