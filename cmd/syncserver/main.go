package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"syncforge"
)

func main() {
	samplePath := flag.String("sample", "samples/demo.json", "local deterministic sample")
	dbPath := flag.String("db", "artifacts/demo-checkpoints.db", "SQLite checkpoint path")
	addr := flag.String("addr", "127.0.0.1:8080", "loopback listen address")
	verify := flag.Bool("verify", false, "run three-path verification and exit")
	regenerate := flag.Bool("generate", false, "regenerate the sample before serving")
	seed := flag.Uint64("seed", 0x5eedc0de, "seed used with -generate")
	flag.Parse()

	if *regenerate {
		sample, err := syncgame.Generate(syncgame.GeneratorOptions{
			Seed:      *seed,
			Players:   4,
			MaxTicks:  180,
			TickRate:  30,
			RenderFPS: 30,
		})
		if err != nil {
			fail(err)
		}
		if err := writeSample(*samplePath, sample); err != nil {
			fail(err)
		}
	}

	sample, err := loadSample(*samplePath)
	if err != nil {
		fail(err)
	}
	if *verify {
		report, err := syncgame.VerifyThreePaths(sample, syncgame.VerificationOptions{
			RunOptions: syncgame.RunOptions{
				Parallel:    true,
				Workers:     4,
				WindowTicks: 20,
			},
		})
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
		if err != nil {
			fail(err)
		}
		if report == nil || !report.Passed {
			os.Exit(2)
		}
		return
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fail(err)
	}
	store, err := syncgame.OpenStore(*dbPath)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	demo, err := syncgame.NewDemoServer(sample, store)
	if err != nil {
		fail(err)
	}
	server, listener, err := syncgame.NewLoopbackServer(demo.Handler(), *addr)
	if err != nil {
		fail(err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	fmt.Printf("sync demo listening on http://%s (sample=%s, db=%s)\n", listener.Addr(), *samplePath, *dbPath)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fail(err)
	}
}

func loadSample(path string) (*syncgame.Sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sample: %w", err)
	}
	sample, err := syncgame.ParseSample(data)
	if err != nil {
		return nil, err
	}
	if err := syncgame.ValidateTruth(sample); err != nil {
		return nil, fmt.Errorf("truth verification: %w", err)
	}
	return sample, nil
}

func writeSample(path string, sample *syncgame.Sample) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(sample)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
