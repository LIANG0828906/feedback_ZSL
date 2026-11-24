package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"economycore/internal/samples"
)

func main() {
	out := flag.String("out", "data/samples", "output directory")
	seed := flag.Uint64("seed", 0x5eedc0de, "deterministic game seed")
	flag.Parse()

	manifest, err := samples.Generate(*out, *seed, samples.DefaultProfiles())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(manifest)
}
