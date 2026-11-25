package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"tlscheck/internal/app"
	"tlscheck/internal/bizerr"
	"tlscheck/internal/projectio"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "load":
		fs := flag.NewFlagSet("load", flag.ExitOnError)
		file := fs.String("file", "", "schema project JSON file")
		canonical := fs.Bool("canonical", false, "print canonical project JSON")
		_ = fs.Parse(os.Args[2:])
		if *file == "" {
			fail(bizerr.New(bizerr.EInvalidArgument, "load requires -file"))
		}
		data, err := os.ReadFile(*file)
		if err != nil {
			fail(bizerr.New(bizerr.EInvalidArgument, err.Error()).WithContext("file", *file))
		}
		p, err := projectio.Load(data)
		if err != nil {
			fail(err)
		}
		if *canonical {
			writeJSON(p)
		} else {
			hash, _ := p.CanonicalHash()
			writeJSON(map[string]any{
				"ok": true, "source_schema": p.SourceSchema, "canonical_hash": hash,
				"endpoints": len(p.Endpoints), "chains": len(p.Chains), "events": len(p.Events),
			})
		}
	case "gen":
		fs := flag.NewFlagSet("gen", flag.ExitOnError)
		out := fs.String("out", "samples", "sample output directory")
		_ = fs.Parse(os.Args[2:])
		b, err := app.GenerateSamples(*out)
		if err != nil {
			fail(err)
		}
		writeJSON(map[string]any{
			"ok": true, "out": *out, "project_id": b.Project.ProjectID,
			"files": []string{"project.v1.json", "project.v2.json", "project.v3.json", "truth.json"},
		})
	case "demo":
		fs := flag.NewFlagSet("demo", flag.ExitOnError)
		samples := fs.String("samples", "samples", "sample output directory")
		db := fs.String("db", "artifacts/tlscheck-demo.db", "SQLite checkpoint path")
		noSamples := fs.Bool("no-samples", false, "do not write sample JSON files")
		_ = fs.Parse(os.Args[2:])
		sampleDir := *samples
		if *noSamples {
			sampleDir = ""
		}
		summary, err := app.RunDemo(sampleDir, *db)
		if err != nil {
			fail(err)
		}
		writeJSON(summary)
		if !summary.OK {
			os.Exit(1)
		}
	case "verify":
		summary, err := app.Verify()
		if err != nil {
			fail(err)
		}
		writeJSON(summary)
		if !summary.OK {
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fail(bizerr.Newf(bizerr.EInvalidArgument, "unknown command %q", os.Args[1]))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go run ./cmd/tlscheck <load|gen|demo|verify> [flags]")
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fail(err)
	}
}

func fail(err error) {
	be := bizerr.As(err)
	payload := map[string]any{"ok": false}
	if be != nil {
		payload["error"] = be
	} else {
		payload["error"] = map[string]string{"code": "E_INTERNAL", "message": err.Error()}
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(b))
	os.Exit(1)
}
