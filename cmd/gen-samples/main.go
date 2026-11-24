package main

import (
	"flag"
	"fmt"
	"os"

	"fixedsampler/internal/samplegen"
)

func main() {
	out := flag.String("out", "samples", "output directory for deterministic sample projects")
	seed := flag.Int64("seed", samplegen.DefaultSeed, "fixed deterministic seed")
	flag.Parse()

	manifests, err := samplegen.GenerateAll(*out, *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-samples failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d projects with seed=%d under %s\n", len(manifests), *seed, *out)
	for _, m := range manifests {
		fmt.Println(m)
	}
}
