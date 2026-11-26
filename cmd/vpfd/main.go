package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	vpf "vpfstack"
	_ "vpfstack/internal/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generateCommand(os.Args[2:])
	case "verify":
		err = verifyCommand(os.Args[2:])
	case "serve":
		err = serveCommand(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vpfd: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`vpfd commands:
  generate [-out samples/seed-20261127]
  verify   [-report artifacts/verification.json] [-sqlite artifacts/vpf-checkpoints.sqlite]
  serve    [-addr 127.0.0.1:18080]`)
}

func generateCommand(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := flags.String("out", filepath.Join("samples", "seed-20261127"), "sample output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	bundle, err := vpf.GenerateSample(vpf.DefaultSeed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	names := map[int]string{1: "project.v1.json", 2: "project.v2.json", 3: "project.v3.json"}
	for version, name := range names {
		path := filepath.Join(*out, name)
		if err := os.WriteFile(path, bundle.Documents[version], 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	truthData, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	truthPath := filepath.Join(*out, "truth.json")
	if err := os.WriteFile(truthPath, truthData, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", truthPath)
	for version := vpf.SchemaV1; version <= vpf.SchemaV3; version++ {
		if bundle.ExportLosses[version] != nil {
			data, _ := json.MarshalIndent(bundle.ExportLosses[version], "", "  ")
			path := filepath.Join(*out, fmt.Sprintf("loss-v%d.json", version))
			_ = os.WriteFile(path, data, 0o644)
			fmt.Println("wrote", path)
		}
	}
	return nil
}

func verifyCommand(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	reportPath := flags.String("report", filepath.Join("artifacts", "verification.json"), "JSON report output path")
	sqlitePath := flags.String("sqlite", filepath.Join("artifacts", "vpf-checkpoints.sqlite"), "SQLite checkpoint path")
	skipSQLite := flags.Bool("no-sqlite", false, "skip database/sql SQLite verification")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sqliteConfig *vpf.SQLiteConfig
	if !*skipSQLite {
		if !driverRegistered(vpfSQLiteDriverName) {
			fmt.Fprintln(os.Stderr, "vpf_sqlite driver unavailable; continuing with durable in-memory verification")
		} else {
			if err := os.MkdirAll(filepath.Dir(*sqlitePath), 0o755); err != nil {
				return err
			}
			_ = os.Remove(*sqlitePath)
			sqliteConfig = &vpf.SQLiteConfig{
				Driver:        vpfSQLiteDriverName,
				CheckpointDSN: *sqlitePath,
				ThreePathOpener: func() (*sql.DB, error) {
					return sql.Open(vpfSQLiteDriverName, ":memory:")
				},
				IncrementalOpener: func() (*sql.DB, error) {
					return sql.Open(vpfSQLiteDriverName, *sqlitePath)
				},
			}
		}
	}

	report, err := vpf.RunLocalVerification(ctx, sqliteConfig)
	if report != nil && *reportPath != "" {
		_ = os.MkdirAll(filepath.Dir(*reportPath), 0o755)
		data, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr == nil {
			_ = os.WriteFile(*reportPath, data, 0o644)
		}
	}
	if err != nil {
		return err
	}
	summary := map[string]any{
		"ok": true, "report": *reportPath,
		"pixel_identical":    report.ThreePaths.PixelIdentical,
		"sequence_identical": report.ThreePaths.SequenceIdentical,
		"realtime_digest":    report.ThreePaths.RealtimeDigest,
		"incremental": map[string][]int{
			"opacity_badge":    report.Incremental["opacity_badge"].Plan.InvalidChunks,
			"group_visibility": report.Incremental["group_visibility"].Plan.InvalidChunks,
			"ripple_source":    report.Incremental["ripple_source"].Plan.InvalidChunks,
		},
		"sqlite_verified": report.SQLite != nil,
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(data))
	if report.SQLite != nil {
		fmt.Println("sqlite checkpoint file:", *sqlitePath)
	}
	return nil
}

func serveCommand(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:18080", "diagnostics listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	view, err := vpf.BuildDiagnosticsView(context.Background())
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *addr, Handler: vpf.DiagnosticsHandler(view), ReadHeaderTimeout: 5 * time.Second}
	fmt.Println("VPF diagnostics listening on http://" + *addr)
	return server.ListenAndServe()
}

const vpfSQLiteDriverName = "vpf_sqlite"

func driverRegistered(name string) bool {
	for _, candidate := range sql.Drivers() {
		if candidate == name {
			return true
		}
	}
	return false
}
