package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "generate":
		code = runGenerate(os.Args[2:])
	case "demo":
		code = runDemo(os.Args[2:])
	case "inject":
		code = runInject(os.Args[2:])
	case "verify":
		code = runVerify(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `soundlab - deterministic audio fingerprint and similarity search demo

commands:
  generate  generate deterministic PCM fixtures and truth
  demo      run extraction, search, incremental/full, resume, verifier and fault demos
  inject    run the four fault-injection chains only
  verify    independently verify a SQLite snapshot against manifest truth

run "soundlab <command> -h" for command flags`)
}

func runGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", 20261125, "deterministic seed")
	batches := fs.Int("batches", 4, "number of batches")
	assets := fs.Int("assets", 3, "assets per batch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	index, err := generate(*out, *seed, *batches, *assets)
	if err != nil {
		return fail(err)
	}
	printJSON(index)
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "project manifest")
	db := fs.String("db", "", "SQLite checkpoint database")
	projectID := fs.String("project-id", "", "project id in the database")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" || *db == "" || *projectID == "" {
		fmt.Fprintln(os.Stderr, "verify requires -manifest, -db and -project-id")
		return 2
	}
	report, err := verifySnapshot(*manifest, *db, *projectID)
	if err != nil {
		return fail(err)
	}
	printJSON(report)
	return 0
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(data))
}

func fail(err error) int {
	envelope := map[string]any{"ok": false}
	if typed, ok := asError(err); ok {
		envelope["error"] = typed
	} else {
		envelope["error"] = map[string]string{"code": "E_INTERNAL", "message": err.Error()}
	}
	printJSON(envelope)
	return 1
}

func writeReport(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
