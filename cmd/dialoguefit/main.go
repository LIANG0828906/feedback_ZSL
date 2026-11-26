package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"dialoguefit/checkpoint"
	"dialoguefit/demo"
	"dialoguefit/model"
	"dialoguefit/projectio"
	"dialoguefit/recheck"
	"dialoguefit/web"
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
	case "recheck":
		recheckCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`dialoguefit commands:
  generate --out artifacts/sample
  demo     --out artifacts/local-demo [--addr 127.0.0.1:8096] [--serve]
  serve    --out artifacts/local-demo [--addr 127.0.0.1:8096]
  recheck  --db artifacts/local-demo/checkpoints.db --truth artifacts/local-demo/final/truth.json`)
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	out := fs.String("out", "artifacts/sample", "output directory")
	_ = fs.Parse(args)
	bundle, err := demo.Run(*out)
	if err != nil {
		log.Fatalf("generate/demo verification failed: %v", err)
	}
	fmt.Printf("sample and truth written under %s\n", *out)
	_ = bundle.LiveStore.Close()
}

func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	out := fs.String("out", "artifacts/local-demo", "artifact output directory")
	addr := fs.String("addr", "127.0.0.1:8096", "diagnostic page address")
	serve := fs.Bool("serve", false, "start HTTP diagnostic page after verification")
	_ = fs.Parse(args)
	bundle, err := demo.Run(*out)
	if err != nil {
		log.Fatalf("demo failed: %v", err)
	}
	fmt.Printf("verification passed: %s/report.json\n", *out)
	fmt.Printf("independent recheck: ok=%v dialogues=%d frames=%d\n",
		bundle.Report.IndependentRecheck.OK,
		bundle.Report.IndependentRecheck.CheckedDialogues,
		bundle.Report.IndependentRecheck.CheckedFrames)
	if !*serve {
		_ = bundle.LiveStore.Close()
		return
	}
	startServer(bundle, *addr)
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	out := fs.String("out", "artifacts/local-demo", "artifact output directory")
	addr := fs.String("addr", "127.0.0.1:8096", "diagnostic page address")
	_ = fs.Parse(args)
	bundle, err := demo.Run(*out)
	if err != nil {
		log.Fatalf("serve verification failed: %v", err)
	}
	startServer(bundle, *addr)
}

func startServer(bundle *demo.Bundle, addr string) {
	server := web.New(web.Config{
		Report: bundle.Report, Truth: bundle.FinalTruth,
		Snapshot:  bundle.LiveEngine.SnapshotResults,
		ApplyEdit: bundle.LiveEngine.ApplyEdit,
		Retry: func(dialogueID string) (*model.InvalidationReport, error) {
			return bundle.LiveEngine.RetryDialogue(context.Background(), dialogueID)
		},
		Inject: func(dialogueID string) error {
			bundle.LiveEngine.InjectedFailures[dialogueID] = true
			return nil
		},
	})
	log.Printf("diagnostic page: http://%s/", addr)
	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func recheckCmd(args []string) {
	fs := flag.NewFlagSet("recheck", flag.ExitOnError)
	db := fs.String("db", "artifacts/local-demo/checkpoints.db", "SQLite checkpoint path")
	truth := fs.String("truth", "artifacts/local-demo/final/truth.json", "truth manifest path")
	_ = fs.Parse(args)
	truthFile, err := projectio.LoadTruthFile(*truth)
	if err != nil {
		log.Fatal(err)
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	report := recheck.Run(context.Background(), store, truthFile)
	if !report.OK {
		log.Fatalf("recheck failed: first violation %+v", report.FirstViolation)
	}
	fmt.Printf("recheck ok: dialogues=%d frames=%d stress=%d segments=%d fingerprints=%d\n",
		report.CheckedDialogues, report.CheckedFrames, report.CheckedStressMarks,
		report.CheckedSegments, report.FingerprintsRecomputed)
}
