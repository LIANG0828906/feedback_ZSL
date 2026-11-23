// Command generator produces the deterministic original/reconstructed frame
// sequences and the truth manifest under a local data directory.
package main

import (
	"flag"
	"time"

	"vqa/internal/cliutil"
	"vqa/internal/sample"
)

func main() {
	dataDir := flag.String("data", "data", "output data directory")
	seed := flag.Int64("seed", 20261124, "deterministic generator seed")
	width := flag.Int("width", 64, "frame width")
	height := flag.Int("height", 64, "frame height")
	channels := flag.Int("channels", 1, "channel count")
	frames := flag.Int("frames", 6, "frame count")
	blockSize := flag.Int("block", 8, "block size")
	cliutil.Parse()

	cfg := sample.GenConfig{
		Seed:         *seed,
		Width:        *width,
		Height:       *height,
		Channels:     *channels,
		Frames:       *frames,
		BlockSize:    *blockSize,
		Qualities:    append([]int(nil), sample.SupportedQualities...),
		SampleFrames: sampleFrames(*frames),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	gen, err := sample.Generate(cfg)
	if err != nil {
		cliutil.Fail(err)
	}
	if err := sample.WritePersisted(*dataDir, gen); err != nil {
		cliutil.Fail(err)
	}
	cliutil.EmitJSON(map[string]any{
		"status":        "generated",
		"data_dir":      *dataDir,
		"seed":          *seed,
		"frames":        *frames,
		"width":         *width,
		"height":        *height,
		"channels":      *channels,
		"block_size":    *blockSize,
		"qualities":     cfg.Qualities,
		"sample_frames": cfg.SampleFrames,
		"manifest":      *dataDir + "/" + sample.ManifestName,
	})
}

func sampleFrames(total int) []int {
	// Deterministic first/middle/last sampling, clamped to available frames.
	last := total - 1
	mid := total / 2
	seen := map[int]bool{}
	var out []int
	for _, v := range []int{0, mid, last} {
		if v >= 0 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
