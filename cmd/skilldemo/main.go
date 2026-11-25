package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"skillbudget/diagnostics"
	"skillbudget/samplegen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "verify"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "verify":
		return verify(args)
	case "generate":
		return generate(args)
	case "serve":
		return serve(args)
	case "demo":
		return demo(args)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func verify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	asJSON := flags.Bool("json", false, "print the full report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := diagnostics.BuildReport(*seed)
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		printReport(report)
	}
	if !report.Pass() {
		return fmt.Errorf("%d validation checks failed", len(report.FailedChecks()))
	}
	return nil
}

func generate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	dir := flags.String("dir", "", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		target = filepath.Join("artifacts", fmt.Sprintf("seed-%d", *seed))
	}
	bundle, err := samplegen.Generate(*seed)
	if err != nil {
		return err
	}
	if err := bundle.Write(target); err != nil {
		return err
	}
	fmt.Println("generated", bundle.Summary())
	fmt.Println("output:", target)
	return nil
}

func demo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	dir := flags.String("dir", "", "sample output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		target = filepath.Join("artifacts", fmt.Sprintf("demo-seed-%d", *seed))
	}
	bundle, err := samplegen.Generate(*seed)
	if err != nil {
		return err
	}
	if err := bundle.Write(target); err != nil {
		return err
	}
	report, err := diagnostics.BuildReport(*seed)
	if err != nil {
		return err
	}
	printReport(report)
	if !report.Pass() {
		return fmt.Errorf("%d validation checks failed", len(report.FailedChecks()))
	}
	fmt.Println("sample files:", target)
	return nil
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := flags.Int64("seed", 42, "deterministic sample seed")
	address := flags.String("addr", "127.0.0.1:0", "loopback listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := diagnostics.BuildReport(*seed)
	if err != nil {
		return err
	}
	listener, err := diagnostics.ListenLoopback(*address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := diagnostics.NewServer(report)
	fmt.Printf("diagnostics: http://%s/\n", listener.Addr().String())
	fmt.Println("loopback only; press Ctrl+C to stop")
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func printReport(report *diagnostics.Report) {
	fmt.Printf("skillbudget verification seed=%d ruleHash=%s ticks=%d windows=%d intents=%d decisions=%d\n",
		report.Seed, report.RuleHash, report.TotalTicks, report.WindowCount, report.IntentCount, report.DecisionCount)
	for _, check := range report.Checks {
		status := "PASS"
		if !check.Pass {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-36s %s\n", status, check.Name, check.Detail)
	}
	status := "PASS"
	if !report.Pass() {
		status = "FAIL"
	}
	fmt.Println("overall:", status)
}

func printUsage() {
	fmt.Println(`skilldemo - deterministic skill cooldown/resource budget kernel demo

Usage:
  skilldemo verify   [-seed 42] [-json]
  skilldemo generate [-seed 42] [-dir artifacts/seed-42]
  skilldemo demo     [-seed 42] [-dir artifacts/demo-seed-42]
  skilldemo serve    [-seed 42] [-addr 127.0.0.1:0]

The serve command refuses non-loopback bind addresses.`)
}
