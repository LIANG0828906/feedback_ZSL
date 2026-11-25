package samplegen

import "soundlab/audiofp"

type Config struct {
	Seed           int64
	Batches        int
	AssetsPerBatch int
	OutputDir      string
}

func (c Config) withDefaults() Config {
	if c.Seed == 0 {
		c.Seed = 20261125
	}
	if c.Batches <= 0 {
		c.Batches = 4
	}
	if c.AssetsPerBatch <= 0 {
		c.AssetsPerBatch = 3
	}
	if c.OutputDir == "" {
		c.OutputDir = "samples"
	}
	return c
}

type Index struct {
	SchemaVersion int            `json:"schema_version"`
	Seed          int64          `json:"seed"`
	Batches       []BatchSummary `json:"batches"`
}

type BatchSummary struct {
	BatchID     string         `json:"batch_id"`
	Manifest    string         `json:"manifest"`
	AssetCount  int            `json:"asset_count"`
	EditCount   int            `json:"edit_count"`
	QueryCount  int            `json:"query_count"`
	FrameCount  int            `json:"frame_count"`
	PCMBytes    int64          `json:"pcm_bytes"`
	FinalRecipe audiofp.Recipe `json:"final_recipe"`
}
