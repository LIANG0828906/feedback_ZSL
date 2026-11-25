package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"volfog/diag"
	"volfog/fog"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "local diagnostic listen address")
	dbPath := flag.String("db", filepath.Join("artifacts", "volfog-checkpoint.sqlite"), "SQLite checkpoint path")
	verify := flag.Bool("verify", false, "run deterministic truth and three-path verification, then exit")
	verifyDir := flag.String("verify-dir", filepath.Join("artifacts", "verify-three-paths"), "directory for verification SQLite files")
	flag.Parse()

	if *verify {
		os.Exit(runVerification(*verifyDir))
	}

	if dir := filepath.Dir(*dbPath); dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	service, err := diag.NewService(*dbPath)
	if err != nil {
		log.Fatalf("start diagnostic service: %v", err)
	}
	defer service.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("VolFog diagnostic workbench: http://%s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func runVerification(dir string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = os.MkdirAll(dir, 0o755)

	report := map[string]any{"dir": dir}
	sc, err := fog.Generate(fog.DefaultSeed)
	if err != nil {
		report["ok"] = false
		report["generate_error"] = err.Error()
		writeVerificationJSON(report)
		return 1
	}
	truth, err := fog.VerifyTruth(sc)
	report["truth"] = map[string]any{"report": truth, "error": errorValue(err), "ok": err == nil}
	if err != nil {
		report["ok"] = false
		writeVerificationJSON(report)
		return 2
	}
	paths, err := fog.VerifyThreePaths(ctx, dir)
	report["three_paths"] = map[string]any{"report": paths, "error": errorValue(err), "ok": err == nil}
	if err != nil {
		report["ok"] = false
		writeVerificationJSON(report)
		return 3
	}
	report["ok"] = true
	writeVerificationJSON(report)
	_, _ = fmt.Fprintln(os.Stderr, "VERIFY OK: truth tolerances, precise invalidation, and three-path consistency passed")
	return 0
}

func errorValue(err error) any {
	if err != nil {
		var fe *fog.Error
		if errors.As(err, &fe) {
			return fe
		}
		return err.Error()
	}
	return nil
}

func writeVerificationJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}
