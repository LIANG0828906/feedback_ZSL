package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"presentkernel/internal/diagnostic"
	"presentkernel/internal/domain"
	"presentkernel/internal/generator"
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
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "version":
		fmt.Println("presentkernel", domain.AlgorithmVersion)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if de, ok := domain.AsError(err); ok {
			b, _ := json.MarshalIndent(de, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  presentkernel generate [--out samples]
  presentkernel verify [--db artifacts/verify-checkpoint.sqlite] [--report artifacts/verification-report.json]
  presentkernel serve [--addr 127.0.0.1:8080] [--port 8080] [--db artifacts/diagnostic.sqlite]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := generator.GenerateSampleSet()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"subtitle.project.v1.json": set.V1,
		"subtitle.project.v2.json": set.V2,
		"subtitle.project.v3.json": set.V3,
		"truth.json":               set.Truth,
	}
	for name, data := range files {
		path := filepath.Join(*out, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path, len(data), "bytes")
	}
	for _, v := range []string{domain.SchemaV1, domain.SchemaV2} {
		rep := set.ExportReports[v]
		b, err := json.MarshalIndent(rep.Lossy, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(*out, "lossy-"+v+".json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	db := fs.String("db", "artifacts/verify-checkpoint.sqlite", "sqlite checkpoint path")
	reportPath := fs.String("report", "artifacts/verification-report.json", "JSON report output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		return err
	}
	state, err := diagnostic.BuildState(*db)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		return err
	}
	report, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*reportPath, report, 0o644); err != nil {
		return err
	}

	failed := false
	for _, c := range state.CrossVersion {
		fmt.Printf("schema %s: load=%t truth=%t fields=%d fills=%d lossy=%d\n", c.Version, c.LoadError == nil, c.MatchesTruth, c.TraceFields, c.DefaultFills, c.ExportLossy)
		if !c.OK {
			failed = true
		}
	}
	if state.Verification != nil {
		for _, step := range state.Verification.Steps {
			invalid, reused := 0, 0
			if step.Invalidation != nil {
				invalid = len(step.Invalidation.Invalid)
				reused = len(step.Invalidation.Reused)
			}
			fmt.Printf("path step %d %s: consistent=%t invalid=%d reused=%d\n", step.Step, step.Label, step.OK, invalid, reused)
			if !step.OK {
				failed = true
			}
		}
	} else {
		failed = true
	}
	for _, f := range state.Faults {
		fmt.Printf("fault %-32s expected=%s observed=%s ok=%t\n", f.Name, f.ExpectedCode, codeOrNone(f.Error), f.OK)
		if !f.OK {
			failed = true
		}
	}
	fmt.Println("report written:", *reportPath)
	if failed {
		return fmt.Errorf("acceptance verification failed; see %s", *reportPath)
	}
	fmt.Println("ACCEPTANCE_OK")
	return nil
}

func codeOrNone(e *domain.Error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Code
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address; defaults to local-only")
	port := fs.Int("port", -1, "optional port; 0 selects an ephemeral local port")
	db := fs.String("db", "artifacts/diagnostic.sqlite", "sqlite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	listenAddr := *addr
	if *port >= 0 {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", *port)
	}
	srv, err := diagnostic.NewServer(*db)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	fmt.Println("diagnostic console: http://" + listener.Addr().String() + "/")
	fmt.Println("api state: http://" + listener.Addr().String() + "/api/state")
	return http.Serve(listener, srv)
}
