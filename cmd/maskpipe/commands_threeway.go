package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"maskpipe/internal/pipeline"
	"maskpipe/internal/verify"
)

func cmdThreeWay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("threeway", flag.ContinueOnError)
	common := bindCommon(fs)
	outDir := fs.String("out", "reports/threeway", "report output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	loaded, err := loadScene(common)
	if err != nil {
		return err
	}

	threeWay, err := pipeline.RunThreeWay(ctx, loaded, loaded.Scene.Parameters, filepath.Join(*outDir, "dbs"))
	if err != nil {
		return err
	}

	for name, result := range threeWay.Results {
		if result == nil {
			continue
		}
		if err := writeMaskPNG(filepath.Join(*outDir, name+".mask.png"), result.Mask, loaded.Scene.Width, loaded.Scene.Height); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(*outDir, name+".json"), result); err != nil {
			return err
		}
	}
	if threeWay.Partial != nil {
		_ = writeJSON(filepath.Join(*outDir, "partial-chunk0.json"), threeWay.Partial)
	}
	if err := writeJSON(filepath.Join(*outDir, "three-way.json"), threeWay); err != nil {
		return err
	}

	preview, err := pipeline.RunPreview(ctx, loaded, loaded.Scene.Parameters)
	if err != nil {
		return err
	}
	full := threeWay.Results["full"]
	preview.Comparison = pipeline.ComparePreview(
		full.Mask, preview.Mask, loaded.Scene.Width, loaded.Scene.Height,
		full.Regions, preview.Regions, loaded.Scene.Declared.Divergence)
	if err := writeMaskPNG(filepath.Join(*outDir, "preview.mask.png"), preview.Mask, loaded.Scene.Width, loaded.Scene.Height); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "preview.json"), preview); err != nil {
		return err
	}

	verification, err := verify.Run(ctx, threeWay.DBPaths["full"], "", &loaded.Scene)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "independent-verification.json"), verification); err != nil {
		return err
	}

	fmt.Printf("three-way ok=%v preview_ok=%v verify_ok=%v dir=%s\n",
		threeWay.Report.OK, preview.Comparison.OK, verification.OK, *outDir)
	if !threeWay.Report.OK {
		return fmt.Errorf("three path consistency check failed")
	}
	if preview.Comparison.Error != nil {
		return preview.Comparison.Error
	}
	if !verification.OK {
		return fmt.Errorf("independent verification failed")
	}
	return nil
}
