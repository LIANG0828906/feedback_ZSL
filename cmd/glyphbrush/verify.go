package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/export"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
)

type CheckResult struct {
	Name     string `json:"name"`
	GlyphID  string `json:"glyphId,omitempty"`
	Passed   bool   `json:"passed"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Duration string `json:"duration"`
}

type GlyphVerification struct {
	GlyphID        string                       `json:"glyphId"`
	Contours       int                          `json:"contours"`
	Segments       int                          `json:"segments"`
	SkeletonPoints int                          `json:"skeletonPoints"`
	TruthBounds    string                       `json:"truthBounds"`
	TruthArea      float64                      `json:"truthArea"`
	Grid           string                       `json:"grid"`
	ParamRedrawn   []int                        `json:"paramRedrawn"`
	ParamReused    []int                        `json:"paramReused"`
	ControlRedrawn []int                        `json:"controlRedrawn"`
	ControlReused  []int                        `json:"controlReused"`
	Invalidation   *pipeline.InvalidationReport `json:"invalidation,omitempty"`
	CompositeHash  string                       `json:"compositeHash"`
	ExportDir      string                       `json:"exportDir"`
}

type VerificationReport struct {
	Status           string              `json:"status"`
	StartedAt        string              `json:"startedAt"`
	Seed             int64               `json:"seed"`
	Algorithm        string              `json:"algorithmVersion"`
	CheckpointSchema string              `json:"checkpointSchema"`
	SamplesDir       string              `json:"samplesDir"`
	OutputDir        string              `json:"outputDir"`
	Checks           []CheckResult       `json:"checks"`
	Glyphs           []GlyphVerification `json:"glyphs"`
	Failure          string              `json:"failure,omitempty"`
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	r := addRenderFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := loadBundle(r.sample, true)
	if err != nil {
		return err
	}
	report := VerificationReport{
		Status:           "running",
		StartedAt:        time.Now().UTC().Format(time.RFC3339),
		Seed:             bundle.Document.Seed,
		Algorithm:        pipeline.AlgorithmVersion,
		CheckpointSchema: checkpoint.SchemaVersion,
		SamplesDir:       r.sample.dir,
		OutputDir:        r.out,
	}
	addCheck := func(name, glyphID string, fn func() error) bool {
		start := time.Now()
		err := fn()
		check := CheckResult{Name: name, GlyphID: glyphID, Passed: err == nil, Duration: time.Since(start).String()}
		if err != nil {
			check.Code = errorCode(err)
			check.Detail = err.Error()
		}
		report.Checks = append(report.Checks, check)
		return err == nil
	}

	addCheck("sample-validation", "", func() error {
		if len(bundle.Glyphs) == 0 {
			return errs.New(errs.CodeSchema, "sample bundle has no glyphs")
		}
		return nil
	})

	ctx := context.Background()
	var finalErrors []*errs.Error
	for _, glyph := range bundle.Glyphs {
		cfg := r.config(bundle)
		engine, err := pipeline.New(glyph, cfg)
		if err != nil {
			addCheck("engine-init", glyph.Glyph.ID, func() error { return err })
			continue
		}
		warm, err := engine.Warm(ctx)
		if !addCheck("warm-render", glyph.Glyph.ID, func() error { return err }) {
			if e, ok := errs.As(err); ok {
				finalErrors = append(finalErrors, e)
			}
			continue
		}
		entry := GlyphVerification{
			GlyphID:       glyph.Glyph.ID,
			Contours:      len(glyph.Glyph.Contours),
			Segments:      len(warm.Segments),
			TruthBounds:   boundsText(glyph),
			TruthArea:     glyph.Truth.ClosedArea,
			Grid:          fmt.Sprintf("%dx%d", warm.Grid.Width, warm.Grid.Height),
			CompositeHash: warm.CompositeHash,
			ExportDir:     filepath.Join(r.out, glyph.Glyph.ID),
		}
		for _, contour := range glyph.Skeletons {
			entry.SkeletonPoints += len(contour.Points)
		}

		addCheck("diff-localization", glyph.Glyph.ID, func() error {
			px, py, owner := firstOwnedPixel(warm)
			if owner == 0 {
				return fmt.Errorf("warm result has no owned pixel")
			}
			tampered := *warm
			tampered.Mask = warm.Mask
			tampered.Mask.Data = append([]byte(nil), warm.Mask.Data...)
			tampered.Mask.Data[py*tampered.Mask.Width+px] ^= 0xff
			diff, err := pipeline.Compare(&tampered, warm)
			if !errs.IsCode(err, errs.CodeDiff) {
				return fmt.Errorf("expected E_DIFF, got %v", err)
			}
			if diff.PixelX != px || diff.PixelY != py {
				return fmt.Errorf("localized (%d,%d), want (%d,%d)", diff.PixelX, diff.PixelY, px, py)
			}
			if diff.SegmentID != owner {
				return fmt.Errorf("localized segment %d, want %d", diff.SegmentID, owner)
			}
			e, _ := errs.As(err)
			if e == nil || e.PixelX == nil || e.PixelY == nil || e.SegmentID == 0 {
				return fmt.Errorf("E_DIFF lacks pixel/segment context")
			}
			return nil
		})

		addCheck("checkpoint-restore", glyph.Glyph.ID, func() error {
			restored, err := pipeline.New(glyph, cfg)
			if err != nil {
				return err
			}
			ok, err := restored.Restore()
			if err != nil || !ok {
				return fmt.Errorf("restore: ok=%v err=%v", ok, err)
			}
			refreshed := restored.Refresh()
			if _, err := pipeline.Compare(refreshed, warm); err != nil {
				return err
			}
			if refreshed.CompositeHash != warm.CompositeHash {
				return errs.New(errs.CodeDiff, "restored composite hash differs from warm render")
			}
			return nil
		})

		next := engine.Params()
		next.Width += 6
		incremental, incErr := engine.UpdateParams(ctx, next)
		entry.ParamRedrawn = incremental.Redrawn
		entry.ParamReused = incremental.Reused
		entry.Invalidation = incremental.Validation
		addCheck("parameter-incremental", glyph.Glyph.ID, func() error {
			if incErr != nil {
				return incErr
			}
			if len(incremental.Redrawn) != len(warm.Segments) {
				return fmt.Errorf("width change redrew %d/%d segments", len(incremental.Redrawn), len(warm.Segments))
			}
			full, err := engine.FullRender(ctx)
			if err != nil {
				return err
			}
			if _, err := pipeline.Compare(incremental, full); err != nil {
				return err
			}
			return nil
		})

		controlResult, controlErr := engine.EditControl(ctx, pipeline.ControlEdit{
			ContourIndex: 0, SegmentIndex: 0, ControlIndex: 0, DX: 2, DY: 1,
		})
		if controlResult != nil {
			entry.ControlRedrawn = controlResult.Redrawn
			entry.ControlReused = controlResult.Reused
			entry.Invalidation = controlResult.Validation
			entry.CompositeHash = controlResult.CompositeHash
		}
		addCheck("control-incremental", glyph.Glyph.ID, func() error {
			if controlErr != nil {
				return controlErr
			}
			if len(controlResult.Reused) == 0 {
				return fmt.Errorf("control edit reused no segments")
			}
			if len(controlResult.Redrawn) >= len(warm.Segments) {
				return fmt.Errorf("control edit redrew all segments; expected partial invalidation")
			}
			full, err := engine.FullRender(ctx)
			if err != nil {
				return err
			}
			if _, err := pipeline.Compare(controlResult, full); err != nil {
				return err
			}
			return nil
		})

		target := 2
		if target > len(warm.Segments) {
			target = len(warm.Segments)
		}
		faults := []struct {
			name string
			kind pipeline.FaultKind
			code errs.Code
		}{
			{"fault-self", pipeline.FaultSelf, errs.CodeSelf},
			{"fault-zlen", pipeline.FaultZero, errs.CodeZero},
			{"fault-dens", pipeline.FaultDensity, errs.CodeDens},
			{"fault-stage", pipeline.FaultStage, errs.CodeStage},
		}
		for _, fault := range faults {
			fault := fault
			addCheck(fault.name, glyph.Glyph.ID, func() error {
				if err := engine.SetFaults([]pipeline.Fault{{SegmentID: target, Kind: fault.kind}}); err != nil {
					return err
				}
				failed, err := engine.Retry(ctx, target)
				if !errs.IsCode(err, fault.code) {
					return fmt.Errorf("expected %s, got %v", fault.code, err)
				}
				e, _ := errs.As(err)
				if e == nil || e.SegmentID != target || e.GlyphID != glyph.Glyph.ID || e.Param == "" {
					return fmt.Errorf("fault error context is incomplete: %+v", err)
				}
				if failed == nil || len(failed.Segments) != len(warm.Segments) {
					return fmt.Errorf("failed retry polluted segment cache")
				}
				if fault.kind != pipeline.FaultStage {
					engine.ClearFault(target)
				}
				retried, retryErr := engine.Retry(ctx, target)
				if retryErr != nil {
					return fmt.Errorf("retry after fault failed: %w", retryErr)
				}
				full, err := engine.FullRender(ctx)
				if err != nil {
					return err
				}
				if _, err := pipeline.Compare(retried, full); err != nil {
					return err
				}
				return nil
			})
		}

		addCheck("checkpoint-version-invalidation", glyph.Glyph.ID, func() error {
			mismatchRoot := filepath.Join(r.out, "mismatch-checkpoints", glyph.Glyph.ID)
			mismatchCfg := cfg
			mismatchCfg.Store = checkpoint.New(mismatchRoot)
			first, err := pipeline.New(glyph, mismatchCfg)
			if err != nil {
				return err
			}
			if _, err := first.Warm(ctx); err != nil {
				return err
			}
			mismatchCfg.Params.Width += 9
			second, err := pipeline.New(glyph, mismatchCfg)
			if err != nil {
				return err
			}
			ok, restoreErr := second.Restore()
			if ok || !errors.Is(restoreErr, checkpoint.ErrVersionMismatch) {
				return fmt.Errorf("expected whole-snapshot mismatch, ok=%v err=%v", ok, restoreErr)
			}
			if _, statErr := os.Stat(filepath.Join(mismatchRoot, glyph.Glyph.ID)); !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("mismatched snapshot was not discarded: %v", statErr)
			}
			return nil
		})

		exportResult := controlResult
		if exportResult == nil {
			exportResult = incremental
		}
		if exportResult != nil {
			if _, err := export.WriteGlyph(entry.ExportDir, glyph, exportResult); err != nil {
				addCheck("export", glyph.Glyph.ID, func() error { return err })
			}
		}
		report.Glyphs = append(report.Glyphs, entry)
	}

	report.Status = "pass"
	for _, check := range report.Checks {
		if !check.Passed {
			report.Status = "fail"
			report.Failure = fmt.Sprintf("%s (%s): %s", check.Name, check.Code, check.Detail)
			break
		}
	}
	if err := os.MkdirAll(r.out, 0o755); err != nil {
		return err
	}
	if err := writeReport(filepath.Join(r.out, "verification.json"), report); err != nil {
		return err
	}
	if err := writeReport(filepath.Join(r.out, "latest.json"), report); err != nil {
		return err
	}
	printJSON(report)
	if report.Status != "pass" {
		if len(finalErrors) > 0 {
			return &errs.BatchError{Errors: finalErrors}
		}
		return fmt.Errorf("verification failed: %s", report.Failure)
	}
	return nil
}

func boundsText(glyph loader.Glyph) string {
	b := glyph.Truth.Bounds
	return fmt.Sprintf("[%d,%d]-[%d,%d]", b.MinX, b.MinY, b.MaxX, b.MaxY)
}

func firstOwnedPixel(result *pipeline.RunResult) (int, int, int) {
	for y := 0; y < result.Mask.Height; y++ {
		for x := 0; x < result.Mask.Width; x++ {
			if result.Mask.At(x, y) == 0 {
				continue
			}
			for _, segment := range result.Segments {
				if segment.Mask.At(x, y) != 0 {
					return x, y, segment.SegmentID
				}
			}
		}
	}
	return 0, 0, 0
}

func writeReport(path string, report VerificationReport) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := jsonMarshalIndent(report)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func jsonMarshalIndent(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
