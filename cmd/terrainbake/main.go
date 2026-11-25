package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"terrainpipeline/internal/artifact"
	"terrainpipeline/internal/budget"
	"terrainpipeline/internal/checkpoint"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/pipeline"
	"terrainpipeline/internal/projectio"
	"terrainpipeline/internal/samplegen"
	"terrainpipeline/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "bake":
		err = runBake(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "all":
		err = runAll(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if te, ok := model.AsError(err); ok {
			data, _ := json.MarshalIndent(te, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
			os.Exit(10)
		}
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`terrainbake commands:
  generate --out artifacts/sample
  bake --project artifacts/sample --out artifacts/demo [--workers 3] [--pixel-buffer 16] [--memory-bytes 4096]
  verify --work artifacts/verification
  all --out artifacts
  serve --report artifacts/verification/verification-report.json --addr 127.0.0.1:8787`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts/sample", "sample output directory")
	seed := fs.Int64("seed", model.FixedSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project := samplegen.Generate(*seed)
	if err := projectio.WriteSample(*out, project); err != nil {
		return err
	}
	fmt.Printf("generated %d blocks, %d seams at %s (seed=%d)\n", len(project.Blocks), len(project.Seams), *out, *seed)
	return nil
}

func runBake(args []string) error {
	fs := flag.NewFlagSet("bake", flag.ContinueOnError)
	projectDir := fs.String("project", "artifacts/sample", "generated project directory")
	out := fs.String("out", "artifacts/demo", "bake output directory")
	workers := fs.Int("workers", 3, "maximum parallel block workers")
	pixelBuffer := fs.Int("pixel-buffer", 64, "per-worker pixel buffer cap")
	memoryBytes := fs.Int64("memory-bytes", 16<<20, "normal map memory budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := projectio.LoadProject(*projectDir)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(filepath.Join(*out, "terrain.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	svc := pipeline.NewService(filepath.Join(*out, "cache"), store)
	outcome, err := svc.Run(context.Background(), project, pipeline.RunOptions{
		Budget: budget.Options{MaxWorkers: *workers, PixelBufferPixels: *pixelBuffer, NormalMapMemoryBytes: *memoryBytes},
	})
	if err != nil {
		return err
	}
	for _, result := range outcome.Results {
		if err := artifact.ExportBlockArtifacts(*out, result); err != nil {
			return err
		}
	}
	if err := artifact.ExportSeams(*out, outcome.Seams); err != nil {
		return err
	}
	if err := artifact.ExportJSON(filepath.Join(*out, "bake-report.json"), outcome); err != nil {
		return err
	}
	fmt.Printf("baked %d blocks -> %s; seam reports=%d; queue_events=%d\n", len(outcome.Results), *out, len(outcome.Seams), len(outcome.QueueEvents))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	work := fs.String("work", "artifacts/verification", "verification workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := verify.RunAll(*work)
	if err != nil {
		return err
	}
	if err := artifact.ExportJSON(filepath.Join(*work, "verification-report.json"), report); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*work, "verification-report.md"), []byte(renderMarkdown(report)), 0o644); err != nil {
		return err
	}
	passed, total := countChecks(report)
	fmt.Printf("verification: %d/%d checks passed; report=%s\n", passed, total, filepath.Join(*work, "verification-report.json"))
	if !report.Passed {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func runAll(args []string) error {
	fs := flag.NewFlagSet("all", flag.ContinueOnError)
	out := fs.String("out", "artifacts", "artifact root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := runGenerate([]string{"--out", filepath.Join(*out, "sample")}); err != nil {
		return err
	}
	if err := runBake([]string{"--project", filepath.Join(*out, "sample"), "--out", filepath.Join(*out, "demo")}); err != nil {
		return err
	}
	return runVerify([]string{"--work", filepath.Join(*out, "verification")})
}

func countChecks(r *verify.Report) (int, int) {
	total := len(r.Checks) + len(r.TruthChecks) + len(r.ExceptionMatrix)
	passed := 0
	for _, c := range r.Checks {
		if c.Passed {
			passed++
		}
	}
	for _, c := range r.TruthChecks {
		if c.Passed {
			passed++
		}
	}
	for _, c := range r.ExceptionMatrix {
		if c.Passed {
			passed++
		}
	}
	return passed, total
}

func renderMarkdown(r *verify.Report) string {
	var b strings.Builder
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(&b, "# 地形材质管线验证报告：%s\n\n", status)
	fmt.Fprintf(&b, "- 工作目录：`%s`\n", r.WorkDir)
	fmt.Fprintf(&b, "- 接缝归因：`%v`\n", r.SeamAttribution)
	fmt.Fprintf(&b, "- 预算事件：`%v`\n\n", r.Budget)
	writeChecks := func(title string, checks []verify.Check) {
		fmt.Fprintf(&b, "## %s\n\n", title)
		fmt.Fprintln(&b, "| 结果 | 检查 | 代码 | 说明 |")
		fmt.Fprintln(&b, "|---|---|---|---|")
		for _, c := range checks {
			mark := "✅"
			if !c.Passed {
				mark = "❌"
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s |\n", mark, c.Name, c.Code, strings.ReplaceAll(c.Detail, "|", "\\|"))
		}
		b.WriteString("\n")
	}
	writeChecks("一致性与失效检查", r.Checks)
	writeChecks("解析真值", r.TruthChecks)
	writeChecks("异常码矩阵", r.ExceptionMatrix)
	b.WriteString("## 提交序列\n\n")
	for name, seq := range r.CommitSequences {
		fmt.Fprintf(&b, "- **%s**: `%s`\n", name, strings.Join(seq, " → "))
	}
	return b.String()
}
