package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"terrainpatch/internal/engine"
	"terrainpatch/internal/loader"
	"terrainpatch/internal/model"
	"terrainpatch/internal/sample"
	"terrainpatch/internal/store"
	"terrainpatch/internal/verifier"
	"terrainpatch/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
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
	case "help", "-h", "--help":
		printUsage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
		printUsage()
	}
	if err != nil {
		if ae, ok := err.(*model.AppError); ok {
			b, _ := json.MarshalIndent(ae, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`terrainpatch commands:
  generate [-dir samples/local] [-seed 42]
  verify   [-samples samples/local] [-db data/verify.db]
  serve    [-addr :8080] [-samples samples/local] [-db data/terrain.db] [-workers 2] [-vertex-buffer 4096] [-memory-mb 16]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join("samples", "local"), "sample output directory")
	seed := fs.Int64("seed", 42, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, ae := sample.Generate(*dir, *seed)
	if ae != nil {
		return ae
	}
	fmt.Printf("generated %d tiles, %d boundaries, %d truth faults in %s (seed=%d)\n",
		len(p.Tiles), len(p.Boundaries), len(p.Truth.Faults), *dir, *seed)
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	samplesDir := fs.String("samples", filepath.Join("samples", "local"), "sample directory")
	dbPath := fs.String("db", filepath.Join("data", "verify.db"), "sqlite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*samplesDir, "project.json")); err != nil {
		if _, ae := sample.Generate(*samplesDir, 42); ae != nil {
			return ae
		}
	}
	_ = os.MkdirAll(filepath.Dir(*dbPath), 0o755)
	report, ae := verifier.VerifySample(context.Background(), *samplesDir, *dbPath)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(report); encErr != nil {
		return encErr
	}
	if ae != nil {
		return ae
	}
	if !report.OK {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	samplesDir := fs.String("samples", filepath.Join("samples", "local"), "sample directory")
	dbPath := fs.String("db", filepath.Join("data", "terrain.db"), "sqlite database path")
	workers := fs.Int("workers", 2, "max parallel tile workers")
	vertexBuffer := fs.Int("vertex-buffer", 4096, "max vertices per worker buffer")
	memoryMB := fs.Int64("memory-mb", 16, "mesh memory budget in MiB")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*samplesDir, "project.json")); err != nil {
		if _, ae := sample.Generate(*samplesDir, 42); ae != nil {
			return ae
		}
	}
	p, ae := loader.LoadProject(*samplesDir)
	if ae != nil {
		return ae
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.NewSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	eng := engine.NewEngine(p, engine.Options{
		Store: st, Workers: *workers, VertexBufferCap: *vertexBuffer,
		MemoryBudgetBytes: *memoryMB << 20,
	})
	recovery, re := eng.Recover(context.Background())
	if re != nil {
		return re
	}
	if recovery.Reset || len(recovery.Missing) > 0 || len(recovery.Recovered) < len(p.Tiles) {
		if _, re := eng.RebuildAll(context.Background(), "startup-full"); re != nil {
			return re
		}
	}
	base, le := loader.LoadProject(*samplesDir)
	if le != nil {
		base = engine.CloneProject(p)
	}
	verifyDB := filepath.Join(filepath.Dir(*dbPath), "three-way-web.db")
	server := &web.Server{Engine: eng, BaseProject: base, VerifyDBPath: verifyDB}
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("terrainpatch diagnostics: http://localhost%s\n", normalizeAddr(*addr))
	fmt.Printf("samples=%s db=%s recovery=%+v\n", *samplesDir, *dbPath, recovery)
	return httpServer.ListenAndServe()
}

func normalizeAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return addr
	}
	return ":" + addr
}
