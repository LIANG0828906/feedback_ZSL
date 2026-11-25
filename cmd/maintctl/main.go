package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"maintenanceorchestrator/internal/app"
	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/generator"
	"maintenanceorchestrator/internal/schema"
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
	case "analyze":
		analyzeCmd(os.Args[2:])
	case "recover-demo":
		recoverDemoCmd(os.Args[2:])
	case "incremental-demo":
		incrementalDemoCmd(os.Args[2:])
	case "demo":
		demoCmd(os.Args[2:])
	case "fault":
		faultCmd(os.Args[2:])
	case "cases":
		printJSON(app.FaultCases())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`maintctl - deterministic maintenance migration kernel

Commands:
  generate [-dir samples] [-seed 424242]
  verify   [-dir samples]
  analyze  [-dir samples]
  recover-demo [-state-dir .recovery-demo]
  incremental-demo [-change lease|node|window]
  demo     [-dir samples] [-seed 424242]
  fault    [-work .fault-work] <case>
  cases
`)
}

func recoverDemoCmd(args []string) {
	fs := flag.NewFlagSet("recover-demo", flag.ExitOnError)
	dir := fs.String("state-dir", ".recovery-demo", "directory for SQLite checkpoint demo")
	_ = fs.Parse(reorderFlagsBeforePositionals(args))
	report, err := app.RecoveryDemo(*dir)
	if err != nil {
		fatal(err)
	}
	printJSON(report)
	if !report.MatchesFullPlan {
		os.Exit(1)
	}
}

func incrementalDemoCmd(args []string) {
	fs := flag.NewFlagSet("incremental-demo", flag.ExitOnError)
	target := fs.String("change", "lease", "change kind: lease, node, or window")
	_ = fs.Parse(reorderFlagsBeforePositionals(args))
	records, err := app.IncrementalDemo(*target)
	if err != nil {
		fatal(err)
	}
	printJSON(records)
}

func analyzeCmd(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	dir := fs.String("dir", "samples", "sample directory")
	_ = fs.Parse(args)
	p, err := schema.LoadFile((*dir) + "/project.v1.json")
	if err != nil {
		fatal(err)
	}
	a, err := app.Analyze(*p)
	if err != nil {
		fatal(err)
	}
	printJSON(a)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	dir := fs.String("dir", "samples", "sample output directory")
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)
	written, _, _, err := generator.WriteSample(*dir, *seed)
	if err != nil {
		fatal(err)
	}
	printJSON(written)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "samples", "sample directory")
	_ = fs.Parse(args)
	report, err := app.VerifyDirectory(*dir)
	if err != nil && report == nil {
		fatal(err)
	}
	printJSON(report)
	if err != nil || !report.OK {
		os.Exit(1)
	}
}

func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dir := fs.String("dir", "samples", "sample directory")
	seed := fs.Int64("seed", generator.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)
	if _, _, _, err := generator.WriteSample(*dir, *seed); err != nil {
		fatal(err)
	}
	report, err := app.VerifyDirectory(*dir)
	if err != nil {
		fatal(err)
	}
	printJSON(report)
	if !report.OK {
		os.Exit(1)
	}
}

func faultCmd(args []string) {
	fs := flag.NewFlagSet("fault", flag.ExitOnError)
	work := fs.String("work", ".fault-work", "working directory for checkpoint faults")
	_ = fs.Parse(reorderFlagsBeforePositionals(args))
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: maintctl fault <case>")
		os.Exit(2)
	}
	err := app.DiagnoseFault(fs.Arg(0), *work)
	if err == nil {
		fmt.Println(`{"ok":true}`)
		return
	}
	if de, ok := domain.AsError(err); ok {
		printJSON(de)
		os.Exit(1)
	}
	fatal(err)
}

func reorderFlagsBeforePositionals(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "--" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func fatal(err error) {
	if de, ok := domain.AsError(err); ok {
		printJSON(de)
	} else {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	}
	os.Exit(1)
}
