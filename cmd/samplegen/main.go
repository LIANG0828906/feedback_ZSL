package main

import (
	"flag"
	"fmt"
	"os"

	"edgerefine/internal/sample"
)

func main() {
	dir := flag.String("dir", "samples", "sample output directory")
	flag.Parse()
	manifests, err := sample.GenerateAll(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "samplegen failed: %v\n", err)
		os.Exit(1)
	}
	for _, m := range manifests {
		fmt.Printf("generated %s %dx%d edges=%d tile=%d seed=%d fp=%s\n",
			m.SampleID, m.Width, m.Height, len(m.Edges), m.Params.TileSize, m.Seed, m.TruthFingerprint[:12])
	}
}
