package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
	"multiplexgw/internal/parallel"
	"multiplexgw/internal/sample"
)

func Verify(dataDir, artifactDir string) (*VerificationReport, error) {
	return VerifyWithOptions(dataDir, artifactDir, 2, 8)
}

func VerifyWithOptions(dataDir, artifactDir string, workers, buffer int) (*VerificationReport, error) {
	rep := &VerificationReport{DataDir: dataDir, ArtifactDir: artifactDir, OutputFiles: map[string]string{}}
	if _, err := os.Stat(filepath.Join(dataDir, "truth.json")); err != nil {
		if _, genErr := Generate(dataDir); genErr != nil {
			return rep, genErr
		}
	}
	truth, err := loadTruth(dataDir)
	if err != nil {
		return rep, err
	}
	projects, cross, err := loadThree(dataDir)
	rep.CrossVersion = cross
	if err != nil {
		return rep, err
	}

	var primaryResult *model.RunResult
	truthOK := true
	for i, p := range projects {
		res, runErr := engine.Run(p, engineOptions())
		if runErr != nil {
			truthOK = false
			rep.Truth.Mismatches = append(rep.Truth.Mismatches, fmt.Sprintf("schema %s run: %v", p.SchemaVersion, runErr))
			continue
		}
		if i == 0 {
			primaryResult = res
		}
		mismatches := compareTraces(truth.EventTruth, res.Traces)
		if len(mismatches) > 0 {
			truthOK = false
			rep.Truth.Mismatches = append(rep.Truth.Mismatches, fmt.Sprintf("schema %s:", p.SchemaVersion))
			rep.Truth.Mismatches = append(rep.Truth.Mismatches, mismatches...)
		}
	}
	rep.Truth.Match = truthOK
	rep.Truth.TraceCount = len(truth.EventTruth)
	rep.Truth.SendCount = len(truth.ScheduleTruth)
	if primaryResult != nil {
		rep.Allocations, rep.AllocationMatch = buildAllocationReport(primaryResult, truth)
	}

	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return rep, err
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return rep, bizerr.Wrap(bizerr.Internal, artifactDir, err, "create artifact directory")
	}
	rep.Numerical = verifyNumerical(bundle.NumericalCases)
	rep.Exceptions = runExceptions(bundle, artifactDir)
	incSummary, incReports := verifyIncremental(bundle.Canonical)
	rep.Incremental = &incSummary
	cfg := parallel.Config{Workers: workers, MaxPendingFrames: buffer}
	if err := cfg.Validate(); err != nil {
		return rep, err
	}
	rep.Parallel = verifyParallel(bundle.Canonical, cfg)
	rep.Checkpoint = verifyCheckpoint(bundle.Canonical, artifactDir)

	rep.OK = rep.CrossVersion.Equal && rep.Truth.Match && rep.Numerical.Match &&
		rep.AllocationMatch && rep.Incremental.AllMatch && rep.Parallel.AllOK && rep.Checkpoint.Match
	for _, ex := range rep.Exceptions {
		if !ex.Match {
			rep.OK = false
		}
	}

	reportPath := filepath.Join(artifactDir, "verification-report.json")
	if err := writeJSON(reportPath, rep); err != nil {
		return rep, err
	}
	rep.OutputFiles["verification_report"] = reportPath

	incPath := filepath.Join(artifactDir, "incremental-report.json")
	if err := writeJSON(incPath, incReports); err != nil {
		return rep, err
	}
	rep.OutputFiles["incremental_report"] = incPath
	fixedPath := filepath.Join(artifactDir, "fixed-point-report.json")
	fixedReport := map[string]any{"cases": rep.Numerical.Cases, "allocations": rep.Allocations}
	if err := writeJSON(fixedPath, fixedReport); err != nil {
		return rep, err
	}
	rep.OutputFiles["fixed_point_report"] = fixedPath

	if primaryResult != nil {
		tracesPath := filepath.Join(artifactDir, "demo-traces-v1.json")
		if err := writeJSON(tracesPath, primaryResult); err != nil {
			return rep, err
		}
		rep.OutputFiles["traces"] = tracesPath
		schedulePath := filepath.Join(artifactDir, "schedule.jsonl")
		if err := writeJSONL(schedulePath, primaryResult.Sends); err != nil {
			return rep, err
		}
		rep.OutputFiles["schedule_jsonl"] = schedulePath
	}
	// Rewrite report so output file map is persisted.
	if err := writeJSON(reportPath, rep); err != nil {
		return rep, err
	}

	if !rep.OK {
		return rep, bizerr.New(bizerr.Internal, "app.verify", "one or more acceptance checks failed")
	}
	return rep, nil
}

func verifyNumerical(cases []sample.NumericalCase) NumericalReport {
	out := NumericalReport{Match: true, Cases: cases}
	for i := range out.Cases {
		c := &out.Cases[i]
		if c.ToleranceQ == 0 && (c.Name == "one-third-weight-share" || c.Name == "two-third-allocation-share") {
			c.ToleranceQ = fixedpoint.DefaultTolerance
		}
		if c.InputPermille > 0 && c.ExpectedWeight > 0 {
			got, err := fixedpoint.PermilleToPermyriad(c.InputPermille)
			if err != nil || got != c.ExpectedWeight {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("%s conversion got=%d err=%v", c.Name, got, err))
				c.WithinTolerance = false
			}
		}
		if c.TotalWeight > 0 {
			truthQ, err := fixedpoint.Share(c.InputPermyriad, c.TotalWeight)
			if err != nil {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("%s share: %v", c.Name, err))
				continue
			}
			computedQ, err := fixedpoint.AllocatedShare(c.AllocatedBytes, c.TotalBytes)
			if err != nil {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("%s allocation: %v", c.Name, err))
				continue
			}
			c.TruthShareQ = truthQ
			c.ComputedShareQ = computedQ
			diff := truthQ - computedQ
			if diff < 0 {
				diff = -diff
			}
			c.DiffQ = diff
			c.WithinTolerance = fixedpoint.WithinTolerance(computedQ, truthQ, fixedpoint.DefaultTolerance)
			if !c.WithinTolerance {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("%s diff=%d tol=%d", c.Name, diff, fixedpoint.DefaultTolerance))
			}
		}
		if c.RepeatCount > 0 && c.IncrementBytes > 0 && c.ExpectedWindow > 0 {
			got := c.StartWindow
			for j := 0; j < c.RepeatCount; j++ {
				if c.CapWindow > 0 {
					next, _ := fixedpoint.SaturatingAddWindow(got, c.IncrementBytes, c.CapWindow)
					got = next
				} else {
					next, ok := fixedpoint.CheckedAdd(got, c.IncrementBytes)
					if !ok {
						out.Match = false
						out.Failures = append(out.Failures, c.Name+" overflow")
						break
					}
					got = next
				}
			}
			if got != c.ExpectedWindow {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("%s window got=%d want=%d", c.Name, got, c.ExpectedWindow))
			}
		}
		if c.Name == "weight-roundtrip-100-adjustments" {
			v, _ := fixedpoint.PermilleToPermyriad(c.InputPermille)
			for j := 0; j < c.RepeatCount; j++ {
				back := v / 10
				v, _ = fixedpoint.PermilleToPermyriad(back)
			}
			if v != c.ExpectedWeight {
				out.Match = false
				out.Failures = append(out.Failures, fmt.Sprintf("weight roundtrip got=%d want=%d", v, c.ExpectedWeight))
			}
		}
	}
	return out
}

// MarshalReport is a small helper for CLIs that want JSON on stdout.
func MarshalReport(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"code":"E_INTERNAL","message":%q}`, err.Error())
	}
	return string(b)
}
