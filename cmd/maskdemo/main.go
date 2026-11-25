package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"maskpreview/internal/demo"
	"maskpreview/internal/engine"
)

func main() {
	sampleDir := flag.String("samples", "samples", "generated sample directory")
	artifactDir := flag.String("artifacts", "artifacts", "database/artifact directory")
	reportPath := flag.String("report", "", "JSON report path (default <artifacts>/run-report.json)")
	seed := flag.Int64("seed", 424242, "deterministic generator seed")
	blockSize := flag.Int("block-size", 16, "fixed block size")
	maxParallel := flag.Int("max-parallel", 3, "maximum concurrent block workers")
	globalBytes := flag.Int64("global-bytes", 8<<20, "global stage buffer budget")
	perBlockBytes := flag.Int64("per-block-bytes", 1<<20, "per-block stage buffer budget")
	flag.Parse()

	runCfg := engine.DefaultRunConfig()
	runCfg.BlockSize = *blockSize
	runCfg.Budget = engine.Budget{
		MaxParallel:      *maxParallel,
		MaxPerBlockBytes: *perBlockBytes,
		GlobalBytes:      *globalBytes,
	}
	report, err := demo.Run(context.Background(), demo.Config{
		SampleDir:   *sampleDir,
		ArtifactDir: *artifactDir,
		ReportPath:  *reportPath,
		Seed:        *seed,
		Run:         runCfg,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "maskdemo: %v\n", err)
		os.Exit(2)
	}
	printSummary(report)
	if report.Status != "pass" {
		os.Exit(1)
	}
}

func printSummary(report *demo.Report) {
	fmt.Printf("maskpreview demo: %s (seed=%d, samples=%d)\n", report.Status, report.Seed, len(report.Samples))
	for _, sample := range report.Samples {
		maskPrefix := sample.Full.MaskHash
		if len(maskPrefix) > 16 {
			maskPrefix = maskPrefix[:16]
		}
		fmt.Printf(
			"%s %dx%d preview=%dx budget=%.3f/%dms regions=%d dev=%.3fpx area=%.2f%% pixel_equal=%t stages_equal=%t mask=%s\n",
			sample.ID, sample.Width, sample.Height, sample.Preview.DownsampleFactor,
			sample.Preview.ElapsedMS, sample.Preview.BudgetMS, sample.Preview.RegionCount,
			sample.Preview.CentroidDeviation, 100*sample.Preview.AreaRelative,
			sample.PixelConsistent, sample.StageSequenceConsistent, maskPrefix,
		)
	}
	for _, name := range []string{"full", "incremental"} {
		if vr := report.Verification[name]; vr != nil {
			fmt.Printf("verify[%s]=%s samples=%d\n", name, vr.Status, len(vr.Samples))
		}
	}
	for _, probe := range report.Exceptions {
		fmt.Printf("error[%s]=%s ok=%t\n", probe.Name, probe.ActualCode, probe.OK)
	}
	fmt.Printf("resource_budget completed=%d max_active=%d queued=%d ok=%t\n",
		report.ResourceBudget.CompletedJobs, report.ResourceBudget.MaxActive,
		len(report.ResourceBudget.QueuedEvents), report.ResourceBudget.OK)
	fmt.Printf("tamper_recovery reused=%t recovered_same_mask=%t\n",
		report.TamperRecovery.CheckpointReused,
		report.TamperRecovery.RecoveredMaskHash == report.TamperRecovery.ExpectedMaskHash)
	fmt.Printf("report=%s\n", report.ReportPath)
}
