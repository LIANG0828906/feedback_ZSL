package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"audioforge/internal/app"
	"audioforge/internal/config"
	"audioforge/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "path to local runtime configuration")
	diagnose := flag.Bool("diagnose", false, "run the complete local diagnosis and print JSON, then exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner, err := app.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer runner.Close()

	if *diagnose {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(runner.Report())
	}

	dashboard := web.New(runner.Report)
	dashboardURL, err := dashboard.Start(cfg.UIPort)
	if err != nil {
		return err
	}
	defer dashboard.Close()

	fmt.Printf("AudioForge diagnostics: %s\n", dashboardURL)
	fmt.Printf("Loopback device:        %s\n", runner.Report().DeviceURL)
	fmt.Printf("Generated samples:      %s\n", cfg.SampleDir)
	fmt.Printf("SQLite checkpoints:     %s\n", cfg.DBPath)
	fmt.Println("Press Ctrl+C to stop.")
	<-ctx.Done()
	return nil
}
