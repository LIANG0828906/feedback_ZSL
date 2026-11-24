package main

import (
	"flag"
	"fmt"
	"os"

	"cadpipe/internal/sample"
)

func main() {
	out := flag.String("out", "samples", "output directory for deterministic sample files")
	seed := flag.Int64("seed", sample.DefaultSeed, "fixed deterministic seed")
	flag.Parse()
	if _, err := sample.GenerateFiles(*out, *seed); err != nil {
		fmt.Fprintf(os.Stderr, "samplegen failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("generated deterministic CAD sample in %s (seed=%d)\n", *out, *seed)
}
