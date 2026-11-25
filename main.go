package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/checkpoint"
	"github.com/liang/hapticfeedback/internal/haptic/diag"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/pipeline"
	"github.com/liang/hapticfeedback/internal/haptic/reference"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
	"github.com/liang/hapticfeedback/internal/haptic/verify"
)

func main() {
	if len(os.Args) < 2 {
		os.Args = append(os.Args, "verify")
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	case "review":
		err = runReview(os.Args[2:])
	case "inject":
		err = runInject(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		if he, ok := errs.As(err); ok {
			b, _ := json.MarshalIndent(he, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`haptic-feedback deterministic kernel

commands:
  generate [--dir samples] [--seed 420243]
  run      [--samples samples] [--db artifacts/run/haptic.db] [--resume] [--hang-after N]
  verify   [--samples samples] [--out artifacts/demo]
  review   --db artifacts/demo/resume.db [--truth samples/truth.json]
  inject   --kind corrupt|order|flag|missing_flag|bad_json [--branch 1]
  serve    [--addr 127.0.0.1:8088] [--samples samples] [--out artifacts/demo]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample output directory")
	_ = fs.Int64("seed", samplegen.DefaultSeed, "fixed seed (recorded for reproducibility)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := samplegen.Generate(*dir)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(b))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("samples", "samples", "deterministic sample directory")
	outDir := fs.String("out", "artifacts/demo", "run artifact directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	report, _, err := verify.Run(*sampleDir, *outDir)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "report.json"), data, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*outDir, "verification.md"), []byte(renderMarkdown(report)), 0o644); err != nil {
		return err
	}
	fmt.Println(string(data))
	if !report.OK {
		return errs.New(errs.EDiff, "acceptance verification failed; see report.json")
	}
	return nil
}

func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sampleDir := fs.String("samples", "samples", "sample directory")
	outDir := fs.String("out", "artifacts/run", "run output directory")
	dbPath := fs.String("db", filepath.Join("artifacts", "run", "haptic.db"), "SQLite checkpoint path")
	branch := fs.Int("branch", 1, "source branch to run")
	resume := fs.Bool("resume", false, "resume from validated SQLite checkpoint")
	hangAfter := fs.Int("hang-after", 0, "after N commits, hang until externally killed (crash recovery test)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	set, err := pipeline.LoadSampleSet(*sampleDir)
	if err != nil {
		set, err = pipeline.GenerateAndLoad(*sampleDir)
		if err != nil {
			return err
		}
	}
	result, err := pipeline.RunCheckpointed(set.Streams[*branch], set.Capability, *branch, pipeline.RunOptions{
		DBPath: *dbPath, Resume: *resume,
		OnCommitted: func(pkt model.Packet) {
			line, _ := json.Marshal(map[string]any{"event": "committed", "seq": pkt.GlobalSeq, "channel": pkt.Channel, "samples": pkt.SampleCount})
			fmt.Println(string(line))
			if *hangAfter > 0 && pkt.GlobalSeq+1 == *hangAfter {
				fmt.Println(`{"event":"hang_ready"}`)
				os.Stdout.Sync()
				select {}
			}
		},
	})
	if err != nil {
		return err
	}
	summary := map[string]any{
		"packets": len(result.Packets), "loaded_checkpoint": result.LoadedCheckpoint,
		"checkpoint_reset": result.CheckpointReset, "db": *dbPath,
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(*outDir, "run-summary.json"), data, 0o644)
	fmt.Println(string(data))
	return nil
}

func runReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	dbPath := fs.String("db", filepath.Join("artifacts", "demo", "resume.db"), "SQLite checkpoint path")
	truthPath := fs.String("truth", filepath.Join("samples", "truth.json"), "truth manifest path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	expectedFP := ""
	if truthBytes, err := os.ReadFile(*truthPath); err == nil {
		var truth samplegen.Truth
		if err := json.Unmarshal(truthBytes, &truth); err != nil {
			return err
		}
		expectedFP = truth.SemanticFingerprint
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	packets, err := store.LoadPackets()
	if err != nil {
		return err
	}
	violation := reference.ReviewRecords(packets, expectedFP)
	out := map[string]any{"packets": len(packets), "ok": violation == nil, "violation": violation}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if violation != nil {
		return violation.AsError()
	}
	return nil
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	kind := fs.String("kind", "corrupt", "injection kind")
	branch := fs.Int("branch", 1, "branch number")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := samplegen.InjectedBranchBytes(*kind, *branch)
	if err != nil {
		return err
	}
	stream, loadErr := branches.Load(data)
	if loadErr == nil && *kind == samplegen.InjectOrder {
		capability := samplegen.Capability()
		_, loadErr = pipeline.BuildPlan(stream, capability, *branch)
	}
	if loadErr != nil {
		he, _ := errs.As(loadErr)
		b, _ := json.MarshalIndent(he, "", "  ")
		fmt.Println(string(b))
		return loadErr
	}
	fmt.Printf("injection %s did not fail; events=%d\n", *kind, len(stream.Events))
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "HTTP listen address")
	sampleDir := fs.String("samples", "samples", "sample directory")
	outDir := fs.String("out", "artifacts/demo", "artifact directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	server, err := diag.NewServer(*sampleDir, *outDir)
	if err != nil {
		return err
	}
	defer server.Close()
	fmt.Printf("diagnostic console: http://%s\n", *addr)
	return server.ListenAndServe(*addr)
}

func renderMarkdown(r *verify.Report) string {
	out := "# 验证结果\n\n"
	out += fmt.Sprintf("- 总体结果: %v\n", r.OK)
	out += fmt.Sprintf("- 语义指纹: `%s`\n", r.SemanticFingerprint)
	out += fmt.Sprintf("- 真值指纹: `%s`\n", r.TruthFingerprint)
	out += fmt.Sprintf("- 包数/采样点: %d / %d\n", r.PacketCount, r.TotalSamples)
	out += fmt.Sprintf("- 独立复核: %v\n\n", r.IndependentReviewOK)
	out += "## 三路径\n\n"
	for _, p := range r.Paths {
		out += fmt.Sprintf("- %s: ok=%v packets=%d %s\n", p.Name, p.OK, p.PacketCount, p.Error)
	}
	out += "\n## 异常注入\n\n"
	for _, in := range r.Injections {
		out += fmt.Sprintf("- %s: ok=%v code=%s frame=%d channel=%s seq=%d\n", in.Kind, in.OK, in.Code, in.FrameNo, in.Channel, in.Seq)
	}
	return out
}
