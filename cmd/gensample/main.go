package main

import (
	"flag"
	"fmt"
	"log"

	"audiofp/samplegen"
)

func main() {
	dir := flag.String("out", "data", "directory for deterministic PCM and manifest JSON")
	flag.Parse()

	material, pcmPath, manifestPath, err := samplegen.WriteDefaultDataset(*dir)
	if err != nil {
		log.Fatalf("generate sample failed: %v", err)
	}
	fmt.Printf("generated %s\n", pcmPath)
	fmt.Printf("generated %s\n", manifestPath)
	fmt.Printf("material=%s sample_rate=%d samples=%d events=%d seed=%d hash=%s\n",
		material.PCM.ID, material.PCM.SampleRate, material.PCM.SampleCount,
		len(material.Manifest.Events), material.PCM.Seed, material.ContentHash()[:16])
}
