// Command verify is the independent verification entry point. It opens the
// checkpoint read-only and recomputes metrics, bounds and boundary merges using
// only persisted block records and the truth manifest.
package main

import (
	"flag"

	"vqa/internal/cliutil"
	"vqa/internal/sample"
	"vqa/internal/store"
	"vqa/internal/verifier"
)

func main() {
	dataDir := flag.String("data", "data", "sample data directory (manifest only)")
	storeDir := flag.String("store", "store/realtime", "checkpoint directory")
	quality := flag.Int("quality", 70, "quality level")
	cliutil.Parse()

	manifest, err := sample.LoadManifest(*dataDir)
	if err != nil {
		cliutil.Fail(err)
	}
	st, err := store.OpenReadOnly(*storeDir)
	if err != nil {
		cliutil.Fail(err)
	}
	report, err := verifier.Verify(manifest, st, *quality)
	if err != nil {
		cliutil.Fail(err)
	}
	cliutil.EmitJSON(map[string]any{
		"status":         "verified",
		"quality":        *quality,
		"frames_checked": report.FramesChecked,
		"blocks_checked": report.BlocksChecked,
		"violations":     report.Violations,
		"frame_psnr_db":  report.FramePSNR,
		"frame_ssim_q16": report.FrameSSIMQ16,
		"store":          *storeDir,
	})
}
