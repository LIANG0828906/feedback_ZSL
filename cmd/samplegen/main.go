package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"layercache"
)

func main() {
	dir := flag.String("dir", "samples", "output directory")
	seed := flag.Int64("seed", layercache.DefaultSeed, "fixed generator seed")
	flag.Parse()
	fixture, err := layercache.GenerateFixture(*seed)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := fixture.WriteSamples(*dir); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"v1.json", "v2.json", "v3.json", "truth.json"} {
		info, err := os.Stat(filepath.Join(*dir, name))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s %d bytes\n", filepath.Join(*dir, name), info.Size())
	}
}
