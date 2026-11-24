package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
	"maskpipe/internal/store"
	"maskpipe/internal/vision"
)

type runReport struct {
	*pipeline.Result
	Preview    *pipeline.PreviewResult `json:"preview,omitempty"`
	MaskPNG    string                  `json:"mask_png,omitempty"`
	OverlayPNG string                  `json:"overlay_png,omitempty"`
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	common := bindCommon(fs)
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	out := fs.String("out", "", "JSON report output path")
	paramsPath := fs.String("params", "", "optional parameter JSON override")
	modeName := fs.String("mode", "full", "incremental, resume, or full")
	workers := fs.Int("workers", 0, "override declared worker budget")
	queueBudget := fs.Int("queue", 0, "override pending task queue budget")
	preview := fs.Bool("preview", false, "run coarse preview and compare with full result")
	maskOut := fs.String("mask-out", "", "output mask PNG path")
	overlayOut := fs.String("overlay-out", "", "output overlay PNG path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	loaded, err := loadScene(common)
	if err != nil {
		return err
	}
	params := loaded.Scene.Parameters
	if *paramsPath != "" {
		data, err := os.ReadFile(*paramsPath)
		if err != nil {
			return err
		}
		var override model.ParameterSet
		if err := json.Unmarshal(data, &override); err != nil {
			return model.NewError(model.CodePara, "params_json", "%v", err)
		}
		if len(override.Seeds) == 0 {
			override.Seeds = params.Seeds
		}
		if override.ParamVersion == "" {
			override = vision.WithVersion(override)
		}
		params = override
	}
	if *workers > 0 {
		loaded.Scene.Declared.Budget.WorkerCount = *workers
	}
	if *queueBudget > 0 {
		loaded.Scene.Declared.Budget.PendingTaskQueueBudget = *queueBudget
	}
	if *dbPath == "" {
		*dbPath = filepath.Join("reports", loaded.Scene.ID+".db")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	svc, err := pipeline.NewService(loaded, st, &params, pipeline.NewControl())
	if err != nil {
		return err
	}
	mode := pipeline.RunMode(*modeName)
	result, runErr := svc.Run(ctx, mode, nil)
	if result == nil {
		return runErr
	}

	report := &runReport{Result: result}
	if *preview && runErr == nil {
		previewResult, err := pipeline.RunPreview(ctx, loaded, params)
		if err != nil {
			return err
		}
		previewResult.Comparison = pipeline.ComparePreview(
			result.Mask, previewResult.Mask, loaded.Scene.Width, loaded.Scene.Height,
			result.Regions, previewResult.Regions, loaded.Scene.Declared.Divergence)
		report.Preview = previewResult
		if previewResult.Comparison.Error != nil {
			runErr = previewResult.Comparison.Error
		}
	}

	maskPath := *maskOut
	if maskPath == "" {
		if *out != "" {
			maskPath = strings.TrimSuffix(*out, ".json") + ".mask.png"
		} else {
			maskPath = filepath.Join("reports", loaded.Scene.ID+".mask.png")
		}
	}
	if err := writeMaskPNG(maskPath, result.Mask, loaded.Scene.Width, loaded.Scene.Height); err != nil {
		return err
	}
	report.MaskPNG = maskPath
	overlayPath := *overlayOut
	if overlayPath == "" {
		overlayPath = strings.TrimSuffix(maskPath, ".png") + ".overlay.png"
	}
	if err := writeOverlayPNG(overlayPath, loaded.Image, result.Mask, loaded.Scene.Width, loaded.Scene.Height); err == nil {
		report.OverlayPNG = overlayPath
	}
	if writeErr := writeJSON(*out, report); writeErr != nil {
		return writeErr
	}
	return runErr
}
