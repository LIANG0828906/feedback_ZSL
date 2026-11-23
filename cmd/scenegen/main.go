package main

import (
	"flag"
	"fmt"
	"os"

	"spatialpick"
)

func main() {
	out := flag.String("out", "data", "directory for scene.json and truth.json")
	flag.Parse()
	scene, truth, err := spatialpick.WriteSample(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("generated %s/scene.json and %s/truth.json\n", *out, *out)
	fmt.Printf("nodes=%d geometries=%d rays=%d boxes=%d points=%d batches=%d content_hash=%s\n",
		len(scene.Nodes), len(scene.Geometries), len(scene.RayQueries), len(scene.BoxQueries),
		len(scene.PointQueries), len(scene.EditSequence), truth.ContentHash)
}
