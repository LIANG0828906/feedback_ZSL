package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	leasekit "leasekit"
)

func main() {
	out := flag.String("out", "samples/generated", "directory for deterministic schemas and truth")
	seed := flag.Int64("seed", leasekit.FixedSeed, "fixed generator seed")
	flag.Parse()

	if err := run(*out, *seed); err != nil {
		fmt.Fprintf(os.Stderr, "leasegen: %v\n", err)
		os.Exit(1)
	}
}

func run(out string, seed int64) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	p := leasekit.GenerateProject(seed)
	truth, err := leasekit.GenerateTruth(p)
	if err != nil {
		return err
	}
	v1, err := leasekit.ExportV1(p)
	if err != nil {
		return err
	}
	v2, err := leasekit.ExportV2(p)
	if err != nil {
		return err
	}
	v3, err := leasekit.ExportV3(p)
	if err != nil {
		return err
	}
	canonical, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	truthJSON, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"project-canonical.json":         canonical,
		"lease-v1-tick-flat.json":        v1,
		"lease-v2-ms-nested.json":        v2,
		"lease-v3-seconds-absolute.json": v3,
		"truth.json":                     truthJSON,
	}
	for name, body := range files {
		path := filepath.Join(out, name)
		if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("generated %d files in %s with seed=%d\n", len(files), out, seed)
	return nil
}
