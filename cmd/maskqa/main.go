package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"maskqa"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "demo":
		demoCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`maskqa commands:
  generate [-out artifacts/dataset.json]
  demo     [-dir artifacts]
  verify   [-dir artifacts]
  serve    [-addr 127.0.0.1:8080] [-db artifacts/workbench.sqlite]`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", filepath.Join("artifacts", "dataset.json"), "dataset JSON output")
	_ = fs.Parse(args)
	d := maskqa.GenerateDataset()
	if err := maskqa.SaveDataset(*out, d); err != nil {
		fatal(err)
	}
	fmt.Printf("generated %d deterministic samples at %s\n", len(d.Samples), *out)
}

func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dir := fs.String("dir", "artifacts", "artifact directory")
	_ = fs.Parse(args)
	result, _, err := maskqa.RunDemo(*dir)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
	if !result.ThreePathsIdentical {
		os.Exit(1)
	}
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "artifacts", "artifact directory")
	_ = fs.Parse(args)
	d, err := maskqa.LoadDataset(filepath.Join(*dir, "dataset.json"))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("E_SCHEMA validation passed: %d samples, seed=%d\n", len(d.Samples), d.Seed)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	db := fs.String("db", filepath.Join("artifacts", "workbench.sqlite"), "SQLite checkpoint path")
	_ = fs.Parse(args)
	app := maskqa.NewWebApp(*db)
	fmt.Printf("MaskQA workbench: http://%s (db=%s)\n", *addr, *db)
	if err := app.ListenAndServe(*addr); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	if qe, ok := err.(*maskqa.QAError); ok {
		b, _ := json.MarshalIndent(qe, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
