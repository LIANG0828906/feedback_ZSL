package main

import (
	"flag"
	"fmt"

	"terrainpipeline/internal/dashboard"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	report := fs.String("report", "artifacts/verification/verification-report.json", "verification report JSON")
	assets := fs.String("assets", "artifacts/verification/art-full", "deterministic artifact directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf("diagnostic dashboard: http://%s\n", *addr)
	return dashboard.ListenAndServe(dashboard.Config{Addr: *addr, ReportPath: *report, AssetsDir: *assets})
}
