package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lightprobe/internal/acceptance"
	"lightprobe/internal/scene"
)

func main() {
	workDir := flag.String("workdir", "artifacts/acceptance", "directory for SQLite checkpoints and reports")
	parallelism := flag.Int("parallelism", 4, "maximum probe worker parallelism")
	jsonOnly := flag.Bool("json", false, "print machine-readable JSON report to stdout")
	flag.Parse()

	report, err := acceptance.Run(acceptance.Options{
		WorkDir:        *workDir,
		MaxParallelism: *parallelism,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance run failed: %v\n", err)
		os.Exit(2)
	}

	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create workdir: %v\n", err)
		os.Exit(2)
	}
	reportData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal report: %v\n", err)
		os.Exit(2)
	}
	reportPath := filepath.Join(*workDir, "report.json")
	if err := os.WriteFile(reportPath, reportData, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(2)
	}

	sceneData, err := json.MarshalIndent(scene.Generate(), "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(*workDir, "scene.json"), sceneData, 0o644)
	}

	markdown := renderMarkdown(report)
	mdPath := filepath.Join(*workDir, "verification-report.md")
	_ = os.WriteFile(mdPath, []byte(markdown), 0o644)

	if *jsonOnly {
		fmt.Println(string(reportData))
	} else {
		fmt.Print(markdown)
		fmt.Printf("\nJSON report: %s\n", reportPath)
		fmt.Printf("Markdown report: %s\n", mdPath)
	}
	if !report.OK {
		os.Exit(1)
	}
}

func renderMarkdown(report *acceptance.Report) string {
	status := "PASS"
	if !report.OK {
		status = "FAIL"
	}
	md := fmt.Sprintf("# LightProbe acceptance report — %s\n\n", status)
	md += fmt.Sprintf("- Generated: %s\n", time.Now().Format(time.RFC3339))
	md += fmt.Sprintf("- Seed: `%d`\n", report.Seed)
	md += fmt.Sprintf("- Rooms/probes/pixels: `%d/%d/%d`\n", report.RoomCount, report.ProbeCount, report.PixelCount)
	md += fmt.Sprintf("- Workers/buffer/SH budget: `%d/%d/%d`\n",
		report.Config.MaxParallelism, report.Config.ProbeBufferPerWorker, report.Config.SHBufferCoeffBudget)
	if report.Precision != nil {
		md += fmt.Sprintf("- Max errors — probes `%.3g`, pixels `%.3g`, jumps `%.3g`\n",
			report.Precision.MaxProbeError, report.Precision.MaxPixelError, report.Precision.MaxJumpError)
	}
	md += fmt.Sprintf("- Budget queue events: `%d`, total wait: `%.3f ms`\n\n",
		report.Queue.Events, float64(report.Queue.WaitNanos)/1e6)
	md += "| Check | Result | Code/Detail |\n|---|---:|---|\n"
	for _, c := range report.Cases {
		icon := "PASS"
		if !c.Pass {
			icon = "FAIL"
		}
		detail := c.Detail
		if c.Code != "" {
			detail = "`" + c.Code + "` " + detail
		}
		if c.Mismatch != nil {
			detail += " — " + c.Mismatch.Error()
		}
		md += fmt.Sprintf("| `%s` | %s | %s |\n", c.Name, icon, detail)
	}
	return md
}
