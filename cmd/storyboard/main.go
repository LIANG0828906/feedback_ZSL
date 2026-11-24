package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"storyboardstudio/internal/engine"
	"storyboardstudio/internal/interop"
	"storyboardstudio/internal/model"
	"storyboardstudio/internal/sample"
	"storyboardstudio/internal/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generateCmd(os.Args[2:])
	case "verify":
		err = verifyCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "export":
		err = exportCmd(os.Args[2:])
	case "serve":
		err = serveCmd(os.Args[2:])
	case "negative-checks":
		err = negativeChecksCmd()
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var coded *model.Error
		if errors.As(err, &coded) {
			data, _ := json.MarshalIndent(coded, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`storyboardstudio commands:
  generate --out samples
  verify --samples samples [--json]
  run --file samples/project-252001.json
  export --file samples/project-252001.json --node n00_world --version 3.0 [--out out.json]
  negative-checks
  serve --addr 127.0.0.1:8787 --samples samples --db data/storyboard.db`)
}

func generateCmd(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := sample.GenerateAll(*out)
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Println("generated", path)
	}
	return nil
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("samples", "samples", "sample directory")
	asJSON := fs.Bool("json", false, "print full JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	allOK := true
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(*dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		project, err := model.LoadProject(data)
		if err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		dbDir, err := os.MkdirTemp("", "storyboard-verify-*")
		if err != nil {
			return err
		}
		report, err := workbench.VerifyProject(project, dbDir)
		_ = os.RemoveAll(dbDir)
		if err != nil {
			return fmt.Errorf("verify %s: %w", path, err)
		}
		if *asJSON {
			encoded, _ := json.MarshalIndent(report, "", "  ")
			fmt.Println(string(encoded))
		} else {
			fmt.Printf("%s seed=%d shots=%d interrupt=%d ok=%v\n", report.ProjectID, report.Seed, report.ShotCount, report.InterruptAt, report.OK)
			printMismatch("  truth", report.Truth)
			printMismatch("  determinism", report.Determinism)
			printMismatch("  resume-suffix", report.ResumeSuffix)
			printMismatch("  checkpoint-path", report.CheckpointPath)
			printMismatch("  three-paths", report.ThreePaths)
			for i, mismatch := range report.EditComparisons {
				printMismatch(fmt.Sprintf("  edit-%d", i), mismatch)
			}
		}
		if !report.OK {
			allOK = false
		}
	}
	if !allOK {
		return errors.New("one or more verification comparisons failed")
	}
	return nil
}

func printMismatch(label string, mismatch *engine.Mismatch) {
	if mismatch == nil {
		fmt.Println(label, "missing comparison")
		return
	}
	if mismatch.OK {
		fmt.Println(label, "MATCH")
		return
	}
	data, _ := json.Marshal(mismatch)
	fmt.Println(label, "MISMATCH", string(data))
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	file := fs.String("file", "", "project JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("--file is required")
	}
	wb, err := workbench.NewMemory()
	if err != nil {
		return err
	}
	defer wb.Close()
	project, err := wb.LoadFile(*file)
	if err != nil {
		return err
	}
	result, err := wb.RunContinuous(project.ID)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func exportCmd(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	file := fs.String("file", "", "project JSON file")
	nodeID := fs.String("node", "", "node id")
	version := fs.String("version", "3.0", "1.0, 2.0, or 3.0")
	out := fs.String("out", "", "optional output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	project, err := model.LoadProject(data)
	if err != nil {
		return err
	}
	var node *model.Node
	for i := range project.Nodes {
		if project.Nodes[i].ID == *nodeID {
			node = &project.Nodes[i]
		}
	}
	if node == nil {
		return &model.Error{Code: model.CodeRef, ConflictNode: *nodeID, Message: "export node not found"}
	}
	fullVersion := map[string]string{"1": interop.Version1, "1.0": interop.Version1, "2": interop.Version2, "2.0": interop.Version2, "3": interop.Version3, "3.0": interop.Version3}[*version]
	if fullVersion == "" {
		return &model.Error{Code: model.CodeVersion, Actual: *version, Message: "unknown export version"}
	}
	result, err := interop.Encode(&interop.Document{NodeID: node.ID, Shots: node.Shots}, fullVersion)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, result.Data, 0o644); err != nil {
			return err
		}
	} else if _, err := os.Stdout.Write(result.Data); err != nil {
		return err
	}
	if result.Lossy != nil {
		data, _ := json.MarshalIndent(result.Lossy, "", "  ")
		fmt.Fprintln(os.Stderr, string(data))
	}
	return nil
}

func negativeChecksCmd() error {
	cases := workbench.NegativeChecks()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(cases)
}
