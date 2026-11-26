// Command wardrobe is the local deterministic demo and diagnostic entry point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/schema"
	"wardrobekernel/internal/xerr"
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
	case "validate":
		err = runValidate(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "incremental-demo":
		err = runIncrementalDemo(os.Args[2:])
	case "resume":
		err = runResume(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "version-check":
		err = runVersionCheck(os.Args[2:])
	case "diff-demo":
		err = runDiffDemo(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		_, _ = os.Stderr.Write(xerr.JSON(err))
		_, _ = os.Stderr.WriteString("\n")
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stdout, `wardrobe kernel commands:
  generate [--dir samples] [--seed 20261127]
  validate --project samples/project.json [--truth samples/truth.json]
  demo --project samples/project.json --truth samples/truth.json [--checkpoint artifacts/checkpoint.sqlite] [--kill-after-commit N]
  incremental-demo --project samples/project.json --truth samples/truth.json [--cache artifacts/cache.json]
  resume --project samples/project.json --checkpoint artifacts/checkpoint.sqlite
  verify --dir samples --work artifacts/verify
  version-check --checkpoint artifacts/checkpoint.sqlite --algorithm-version wrong
  diff-demo [--project samples/project.json]
`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample output directory")
	seed := fs.Int64("seed", 20261127, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := samplegen.WriteSampleDir(*dir, *seed); err != nil {
		return xerr.Wrap(xerr.EIO, "write samples", err)
	}
	fmt.Printf("generated deterministic samples in %s with seed %d\n", *dir, *seed)
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/project.json", "project JSON path")
	truthPath := fs.String("truth", "", "optional truth JSON path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := schema.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	if *truthPath != "" {
		if _, err := schema.LoadTruth(*truthPath); err != nil {
			return err
		}
	}
	writeJSONIndent(map[string]any{
		"ok": true, "parts": len(project.Parts), "attach_points": len(project.AttachPoints),
		"constraints": project.Constraints,
	})
	return nil
}

func writeJSONIndent(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))
}
