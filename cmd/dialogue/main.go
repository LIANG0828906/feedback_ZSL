package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	dialogue "dialoguecore"
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
	case "demo":
		generateCmd([]string{})
		verifyCmd([]string{})
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dialogue <generate|verify|serve|demo> [flags]")
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "samples", "directory for generated schema documents and truth")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	project, truth, err := dialogue.GenerateProject(dialogue.GeneratorOptions{})
	if err != nil {
		fatal(err)
	}
	docs, losses, err := dialogue.GenerateDocuments(project)
	if err != nil {
		fatal(err)
	}
	names := map[int]string{1: "dialogue-v1.json", 2: "dialogue-v2.json", 3: "dialogue-v3.json"}
	for version, data := range docs {
		path := filepath.Join(*out, names[version])
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatal(err)
		}
		lossData, _ := json.MarshalIndent(losses[version], "", "  ")
		lossPath := filepath.Join(*out, fmt.Sprintf("loss-v%d.json", version))
		if err := os.WriteFile(lossPath, lossData, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s\n", path)
	}
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		fatal(err)
	}
	truthPath := filepath.Join(*out, "truth.json")
	if err := os.WriteFile(truthPath, truthData, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s\n", truthPath)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	out := fs.String("out", "artifacts", "directory for verification JSON")
	_ = fs.Parse(args)
	data, err := dialogue.BuildDiagnosticData()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	reportPath := filepath.Join(*out, "verification-report.json")
	writeJSONFile(reportPath, data)
	diagPath := filepath.Join(*out, "diagnostic.json")
	writeJSONFile(diagPath, data)

	fmt.Printf("verification report: %s\n", reportPath)
	for _, check := range data.Checks {
		mark := "PASS"
		if !check.OK {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %s — %s\n", mark, check.Name, check.Detail)
	}
	if !data.OK {
		os.Exit(1)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:18086", "loopback listen address")
	_ = fs.Parse(args)
	server, err := dialogue.NewDiagnosticServer()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("dialogue diagnostics on http://%s\n", *addr)
	if err := server.ListenAndServe(*addr); err != nil {
		fatal(err)
	}
}

func writeJSONFile(path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	if be, ok := err.(*dialogue.Error); ok {
		fmt.Fprintf(os.Stderr, "%s: %s (%s %s)\n", be.Code, be.Message, be.SourceID, be.Path)
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(1)
}
