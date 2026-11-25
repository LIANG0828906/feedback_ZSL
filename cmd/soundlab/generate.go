package main

import (
	"soundlab/samplegen"
)

func generate(out string, seed int64, batches, assets int) (*samplegen.Index, error) {
	return samplegen.Generate(out, samplegen.Config{
		Seed: seed, Batches: batches, AssetsPerBatch: assets, OutputDir: out,
	})
}
