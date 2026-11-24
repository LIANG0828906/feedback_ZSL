package main

import (
	"encoding/json"
	"flag"
	"fmt"

	"maskpipe/internal/sample"
)

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	root := fs.String("root", "samples/local-seed", "output sample directory")
	seed := fs.Int64("seed", 424242, "fixed deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := sample.Generate(*root, *seed)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true, "root": *root, "seed": *seed,
		"scenes": len(manifest.Samples), "manifest": *root + "/manifest.json",
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	common := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	loaded, err := loadScene(common)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true,
		"scene": map[string]any{
			"id": loaded.Scene.ID, "size": []int{loaded.Scene.Width, loaded.Scene.Height},
			"channels": loaded.Scene.Channels, "tile": loaded.Scene.Tile,
			"chunks": loaded.Scene.ChunkCount, "truth_regions": loaded.Scene.TruthRegionCount,
			"truth_fingerprint": loaded.Scene.TruthFingerprint,
			"param_version":     loaded.Scene.Parameters.ParamVersion,
		},
	}, "", "  ")
	fmt.Println(string(b))
	return nil
}
