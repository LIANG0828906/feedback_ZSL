package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"msworkbench/internal/checkpoint"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/pipeline"
	"msworkbench/internal/projectio"
	"msworkbench/internal/sample"
	"msworkbench/internal/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "all", "verify":
		err = cmdAll(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "crash":
		err = cmdCrash(os.Args[2:])
	case "resume":
		err = cmdResume(os.Args[2:])
	case "full-run":
		err = cmdFull(os.Args[2:])
	case "compare-reports":
		err = cmdCompare(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var coded *e.Error
		if errors.As(err, &coded) {
			b, _ := json.MarshalIndent(coded, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`msdemo commands:
  all --out artifacts/demo       generate samples, schemas, verification and workbench
  serve --dir artifacts/demo     serve static diagnostic workbench
  crash --state DIR [--after 5]  commit stages then SIGKILL this process
  resume --state DIR             verify and resume the killed run
  full-run --state DIR           produce a fresh full report
  compare-reports -want A -got B compare two JSON reports`)
}

func cmdAll(args []string) error {
	fs := flag.NewFlagSet("all", flag.ContinueOnError)
	out := fs.String("out", "artifacts/demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, d := range []string{*out, filepath.Join(*out, "samples"), filepath.Join(*out, "checkpoints"), filepath.Join(*out, "workbench")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(*out, "checkpoints", "block_stage.sql"), []byte(checkpoint.SQLiteSchema), 0o644); err != nil {
		return err
	}
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "samples", "sample-set.json"), set); err != nil {
		return err
	}
	checks := []workbench.SchemaCheck{}
	for _, ver := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		exp, err := projectio.Export(p, ver)
		if err != nil {
			return err
		}
		file := map[string]string{domain.SchemaV1: "project.v1.json", domain.SchemaV2: "project.v2.json", domain.SchemaV3: "project.v3.json"}[ver]
		if err := os.WriteFile(filepath.Join(*out, "samples", file), exp.Data, 0o644); err != nil {
			return err
		}
		loaded, err := projectio.Load(exp.Data)
		check := workbench.SchemaCheck{Version: ver, LossItems: lossLen(exp.LossReport)}
		if err != nil {
			if ce, ok := e.As(err); ok {
				check.ErrorCode = string(ce.Code)
				check.Error = ce.Message
			} else {
				check.Error = err.Error()
			}
			checks = append(checks, check)
			continue
		}
		check.OK = true
		check.CanonicalHash = loaded.Project.CanonicalHash()
		check.TraceCount = len(loaded.Project.Traces)
		if check.CanonicalHash != p.CanonicalHash() {
			return e.New(e.Diff, "schema %s canonical hash mismatch", ver)
		}
		checks = append(checks, check)
	}

	v, err := pipeline.RunVerification(filepath.Join(*out, "checkpoints"))
	if err != nil {
		return err
	}
	summary := workbench.BuildSummary(v, checks)
	if err := workbench.Write(filepath.Join(*out, "workbench"), summary, v); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "summary.json"), summary); err != nil {
		return err
	}
	fmt.Printf("OK verification=%s blocks=%d bands=%d output=%s\n", "PASS", summary.BlockCount, summary.BandCount, *out)
	fmt.Printf("workbench=%s\n", filepath.Join(*out, "workbench", "index.html"))
	return nil
}

func lossLen(r *domain.LossReport) int {
	if r == nil {
		return 0
	}
	return len(r.Items)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "artifacts/demo", "diagnostic output directory")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/workbench/index.html", http.StatusFound)
			return
		}
		http.FileServer(http.Dir(*dir)).ServeHTTP(w, r)
	})
	fmt.Printf("serving %s at http://%s/workbench/index.html\n", *dir, *addr)
	return http.ListenAndServe(*addr, mux)
}

func cmdCrash(args []string) error {
	fs := flag.NewFlagSet("crash", flag.ContinueOnError)
	state := fs.String("state", "artifacts/demo/subprocess", "state directory")
	after := fs.Int("after", 5, "committed stages before SIGKILL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*state, 0o755); err != nil {
		return err
	}
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		return err
	}
	store, err := checkpoint.OpenFile(filepath.Join(*state, "subprocess.db"))
	if err != nil {
		return err
	}
	_, err = pipeline.Execute(pipeline.Options{Project: p, Sample: set, Store: store, RunID: "subprocess-run", Profile: domain.ScaleDense, Parallelism: 2, PixelBufferPerWorker: 4096, QueueCapacity: 2, CrashAfterStages: *after})
	var crash *pipeline.CrashError
	if errors.As(err, &crash) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	return fmt.Errorf("expected crash error, got err=%v", err)
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	state := fs.String("state", "artifacts/demo/subprocess", "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		return err
	}
	store, err := checkpoint.OpenFile(filepath.Join(*state, "subprocess.db"))
	if err != nil {
		return err
	}
	r, err := pipeline.Execute(pipeline.Options{Project: p, Sample: set, Store: store, RunID: "subprocess-run", Profile: domain.ScaleDense, Parallelism: 2, PixelBufferPerWorker: 4096, QueueCapacity: 2, Resume: true})
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*state, "resume-report.json"), r); err != nil {
		return err
	}
	fmt.Printf("OK resumed blocks=%d records=%d\n", len(r.Blocks), len(r.Records))
	return nil
}

func cmdFull(args []string) error {
	fs := flag.NewFlagSet("full-run", flag.ContinueOnError)
	state := fs.String("state", "artifacts/demo/subprocess", "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := sample.NewCanonicalProject()
	set, err := sample.Generate(p)
	if err != nil {
		return err
	}
	store, err := checkpoint.OpenFile(filepath.Join(*state, "full.db"))
	if err != nil {
		return err
	}
	r, err := pipeline.Execute(pipeline.Options{Project: p, Sample: set, Store: store, RunID: "full-run", Profile: domain.ScaleDense, Parallelism: 2, PixelBufferPerWorker: 4096, QueueCapacity: 2})
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*state, "full-report.json"), r); err != nil {
		return err
	}
	fmt.Printf("OK full blocks=%d records=%d\n", len(r.Blocks), len(r.Records))
	return nil
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare-reports", flag.ContinueOnError)
	wantPath := fs.String("want", "", "wanted report JSON")
	gotPath := fs.String("got", "", "candidate report JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var want, got pipeline.Report
	if err := readJSON(*wantPath, &want); err != nil {
		return err
	}
	if err := readJSON(*gotPath, &got); err != nil {
		return err
	}
	if err := pipeline.CompareReports(&want, &got); err != nil {
		return err
	}
	fmt.Println("OK reports match block pixels, reconstruction, method and commit sequence")
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
