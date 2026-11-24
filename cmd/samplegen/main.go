package main

import (
	"flag"
	"fmt"
	"os"

	"spectralrender"
)

func main() {
	out := flag.String("out", "samples", "deterministic sample output directory")
	seed := flag.Int64("seed", 42, "fixed sample seed")
	flag.Parse()
	manifest, err := spectralrender.GenerateSamples(*out, *seed)
	if err != nil {
		if e := spectralrender.AsError(err); e != nil {
			fmt.Fprintf(os.Stderr, "%s\n", e)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	fmt.Printf("generated %s: scene=%s cmf=%s truth=%s scene_hash=%s cmf_hash=%s truth_hash=%s\n",
		*out, manifest.SceneFile, manifest.CMFFile, manifest.TruthFile,
		manifest.SceneHash, manifest.CMFHash, manifest.TruthHash)
}
