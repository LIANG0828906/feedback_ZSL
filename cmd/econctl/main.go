package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"economycore/checkpoint"
	"economycore/demo"
	"economycore/fixed"
	"economycore/format"
	"economycore/model"
	"economycore/runner"
	"economycore/sample"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = commandDemo(os.Args[2:])
	case "gen":
		err = commandGen(os.Args[2:])
	case "run":
		err = commandRun(os.Args[2:])
	case "convert":
		err = commandConvert(os.Args[2:])
	case "validate":
		err = commandValidate(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `econctl - fixed-point economy kernel

Usage:
  econctl demo    --dir artifacts/demo --seed 42
  econctl gen     --out artifacts/samples --seed 42
  econctl validate --in project.json
  econctl convert --in project.json --to v1 --out v1.json [--allow-lossy]
  econctl run     --in project.json --db state.db --out report.json
`)
}

func commandDemo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	dir := flags.String("dir", "artifacts/demo", "output directory")
	seed := flags.Int64("seed", 42, "deterministic seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := demo.Run(context.Background(), *dir, *seed)
	if report != nil {
		fmt.Print(report.Summary())
		fmt.Printf("report=%s\n", filepath.Join(*dir, "validation-report.json"))
	}
	if err != nil {
		return err
	}
	if !report.OverallPass {
		return fmt.Errorf("demo validation failed")
	}
	return nil
}

func commandGen(args []string) error {
	flags := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := flags.String("out", "artifacts/samples", "output directory")
	seed := flags.Int64("seed", 42, "deterministic seed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := sample.Generate(context.Background(), *out, *seed)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(data))
	return nil
}

func commandValidate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	input := flags.String("in", "", "input project")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return fmt.Errorf("--in is required")
	}
	m, err := format.LoadFile(*input)
	if err != nil {
		return err
	}
	hash, err := m.ContentHash()
	if err != nil {
		return err
	}
	fmt.Printf("version=%s project=%s cycles=%d nodes=%d intents=%d hash=%s\n", m.SourceVersion, m.ID, m.TotalCycles(), len(m.Nodes), len(m.Intents), hash)
	return nil
}

func commandConvert(args []string) error {
	flags := flag.NewFlagSet("convert", flag.ContinueOnError)
	input := flags.String("in", "", "input project")
	target := flags.String("to", model.SchemaV3, "target version (v1, v2, v3)")
	out := flags.String("out", "", "output project")
	allowLossy := flags.Bool("allow-lossy", false, "write a target document using recorded loss dispositions")
	round := flags.String("round", "floor", "floor, ceil or nearest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || *out == "" {
		return fmt.Errorf("--in and --out are required")
	}
	mode, err := fixed.ParseRoundMode(*round)
	if err != nil {
		return err
	}
	m, err := format.LoadFile(*input)
	if err != nil {
		return err
	}
	result, exportErr := format.Export(m, *target, format.ExportOptions{AllowLossy: *allowLossy, RoundMode: mode})
	if result != nil {
		if len(result.Traces) > 0 {
			traces, _ := json.MarshalIndent(result.Traces, "", "  ")
			fmt.Printf("upgrade traces:\n%s\n", traces)
		}
		if len(result.Losses) > 0 {
			losses, _ := json.MarshalIndent(result.Losses, "", "  ")
			fmt.Printf("loss list:\n%s\n", losses)
		}
	}
	if exportErr != nil {
		return exportErr
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(result.Data, '\n'), 0o644); err != nil {
		return model.NewError(model.EIO, *out, err.Error()).WithCause(err)
	}
	fmt.Printf("wrote %s (%s, lossy=%v)\n", *out, result.Version, result.Lossy)
	return nil
}

func commandRun(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	input := flags.String("in", "", "input project")
	db := flags.String("db", "artifacts/economy.db", "SQLite checkpoint database")
	out := flags.String("out", "artifacts/report.json", "report JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return fmt.Errorf("--in is required")
	}
	m, err := format.LoadFile(*input)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	r, err := runner.New(m, store, runner.Options{})
	if err != nil {
		return err
	}
	report, runErr := r.RunAll(context.Background())
	if report == nil {
		return runErr
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return model.NewError(model.EIO, *out, err.Error()).WithCause(err)
	}
	fmt.Printf("complete=%v committed=%d/%d cycles=%d report=%s\n", report.Complete, report.CommittedWindows, report.TotalWindows, len(report.Cycles), *out)
	return runErr
}
