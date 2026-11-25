package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	lipsync "lipsync"
	"lipsync/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "inject":
		injectCmd(os.Args[2:])
	case "version":
		fmt.Println(lipsync.AlgorithmVersion)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lipsync <generate|verify|serve|inject|version> [flags]")
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	projectPath := fs.String("project", "data/project.json", "project JSON output")
	truthPath := fs.String("truth", "data/truth.json", "truth/report JSON output")
	seed := fs.Int64("seed", lipsync.DefaultGenerateOptions().Seed, "deterministic seed")
	count := fs.Int("dialogues", lipsync.DefaultGenerateOptions().DialogueCount, "dialogue/shard count")
	_ = fs.Parse(args)

	project := lipsync.GenerateProject(lipsync.GenerateOptions{Seed: *seed, DialogueCount: *count})
	result, err := lipsync.BuildFullResult(project)
	must(err)
	must(writeJSONFile(*projectPath, project))
	must(writeJSONFile(*truthPath, result))
	fmt.Printf("generated %s and %s: dialogues=%d packs=%d frames=%d\n",
		*projectPath, *truthPath, len(result.Dialogues), countPacks(result), result.Dispatch.FrameCount)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	workDir := fs.String("workdir", ".", "writable working directory for SQLite/checkpoints")
	reportPath := fs.String("report", "artifacts/acceptance-report.json", "JSON report path")
	_ = fs.Parse(args)

	report, err := lipsync.RunAcceptance(context.Background(), *workDir)
	if report != nil && *reportPath != "" {
		path := *reportPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(*workDir, path)
		}
		_ = writeJSONFile(path, report)
	}
	if err != nil {
		printDomainOrError(err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
	if !report.OK {
		os.Exit(1)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := fs.String("db", "data/lipsync.db", "SQLite checkpoint path")
	_ = fs.Parse(args)

	server, err := web.NewServer(*dbPath)
	must(err)
	defer server.Close()
	fmt.Printf("diagnostic UI: http://%s/\n", *addr)
	must(server.ListenAndServe(*addr))
}

func injectCmd(args []string) {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	kind := fs.String("kind", "order", "order|label|vis|stage")
	_ = fs.Parse(args)

	switch lipsync.InjectKind(*kind) {
	case lipsync.InjectOrder, lipsync.InjectLabel, lipsync.InjectVis:
		project := lipsync.GenerateProject(lipsync.GenerateOptions{Inject: lipsync.InjectKind(*kind)})
		_, err := lipsync.BuildFullResult(project)
		printDomainOrError(err)
		if err != nil {
			os.Exit(1)
		}
	case lipsync.InjectStage:
		fail := 3
		run, err := lipsync.RunShardPool(context.Background(),
			lipsync.PoolOptions{MaxParallelism: 2, PerWorkerBuffer: 8, QueueBudget: 4},
			lipsync.ShardWorksForProject(lipsync.GenerateProject(lipsync.DefaultGenerateOptions()), &fail),
		)
		if err != nil {
			printDomainOrError(err)
			os.Exit(1)
		}
		err = lipsync.FirstOutcomeError(run.Outcomes)
		printDomainOrError(err)
		if err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown injection kind %q\n", *kind)
		os.Exit(2)
	}
}

func writeJSONFile(path string, value any) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func must(err error) {
	if err != nil {
		printDomainOrError(err)
		os.Exit(1)
	}
}

func printDomainOrError(err error) {
	if err == nil {
		return
	}
	if domainErr, ok := lipsync.AsError(err); ok {
		data, _ := json.MarshalIndent(domainErr, "", "  ")
		fmt.Fprintln(os.Stderr, string(data))
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func countPacks(result *lipsync.FullResult) int {
	count := 0
	for _, dr := range result.Dialogues {
		count += len(dr.Packs)
	}
	return count
}
