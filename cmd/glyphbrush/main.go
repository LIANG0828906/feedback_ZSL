package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/export"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/samplegen"
	"glyphbrush/internal/stroke"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "generate":
		return cmdGenerate(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "render":
		return cmdRender(args[1:])
	case "incremental":
		return cmdIncremental(args[1:])
	case "inject":
		return cmdInject(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "ui":
		fmt.Println("Run: go run -tags fyne ./cmd/glyphbrush-ui -samples ./samples")
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Print(`glyphbrush deterministic glyph stroke pipeline

Commands:
  generate    write glyphs.json and truth.json from a fixed seed
  validate    validate contours and compare against truth
  render      full render, export PNG/JSON and write checkpoints
  incremental change stroke parameters and verify pixel-exact consistency
  inject      run one of E_SELF/E_ZLEN/E_DENS/E_STAGE fault paths
  verify      run the complete deterministic acceptance suite
  ui          print the Fyne panel launch command

All paths are local. No network service or account is required.
`)
}

type sampleFlags struct {
	dir  string
	seed int64
}

func addSampleFlags(fs *flag.FlagSet) *sampleFlags {
	f := &sampleFlags{}
	fs.StringVar(&f.dir, "samples", "./samples", "directory containing glyphs.json and truth.json")
	fs.Int64Var(&f.seed, "seed", samplegen.DefaultSeed, "deterministic generator seed")
	return f
}

func loadBundle(f *sampleFlags, autoGenerate bool) (*loader.Bundle, error) {
	if autoGenerate {
		if _, err := os.Stat(filepath.Join(f.dir, "glyphs.json")); errors.Is(err, os.ErrNotExist) {
			if _, _, err := samplegen.Write(f.dir, f.seed); err != nil {
				return nil, err
			}
		}
	}
	return loader.Load(f.dir)
}

type renderFlags struct {
	sample       *sampleFlags
	out          string
	checkpoints  string
	glyphID      string
	width        float64
	spacing      float64
	join         string
	tip          string
	curvature    float64
	scale        float64
	grid         int
	maxDimension int
	maxParallel  int
	polygonLimit int
	restore      bool
}

func addRenderFlags(fs *flag.FlagSet) *renderFlags {
	r := &renderFlags{}
	r.sample = addSampleFlags(fs)
	fs.StringVar(&r.out, "out", "./out", "artifact output directory")
	fs.StringVar(&r.checkpoints, "checkpoints", "./out/checkpoints", "persistent checkpoint directory")
	fs.StringVar(&r.glyphID, "glyph", "", "render only this glyph id")
	fs.Float64Var(&r.width, "width", stroke.DefaultParams().Width, "stroke width in glyph units")
	fs.Float64Var(&r.spacing, "spacing", stroke.DefaultParams().Spacing, "spacing as a fraction of stroke width")
	fs.StringVar(&r.join, "join", string(stroke.JoinRound), "round, bevel or miter")
	fs.StringVar(&r.tip, "tip", string(stroke.TipRound), "round, flat or square")
	fs.Float64Var(&r.curvature, "curvature", stroke.DefaultParams().CurvatureFactor, "curvature spacing factor")
	fs.Float64Var(&r.scale, "scale", 1, "raster pixels per glyph unit")
	fs.IntVar(&r.grid, "grid", 1, "pixel quantum used to snap the derived grid")
	fs.IntVar(&r.maxDimension, "max-dimension", 4096, "maximum raster width or height")
	fs.IntVar(&r.maxParallel, "max-parallel", 4, "maximum segment render parallelism")
	fs.IntVar(&r.polygonLimit, "polygon-buffer", 200000, "maximum polygon points per segment")
	fs.BoolVar(&r.restore, "restore", false, "reuse a matching checkpoint instead of forcing warm render")
	return r
}

func (r *renderFlags) params() stroke.Params {
	return stroke.Params{
		Width:           r.width,
		Spacing:         r.spacing,
		Join:            stroke.JoinStyle(r.join),
		Tip:             stroke.TipStyle(r.tip),
		CurvatureFactor: r.curvature,
	}
}

func (r *renderFlags) config(bundle *loader.Bundle) pipeline.Config {
	return pipeline.Config{
		Params:             r.params(),
		Raster:             raster.Config{Scale: r.scale, GridSize: r.grid, MaxDimension: r.maxDimension},
		MaxParallel:        r.maxParallel,
		PolygonBufferLimit: r.polygonLimit,
		Store:              checkpoint.New(r.checkpoints),
		DensityRange:       bundle.Document.DensityRange,
		CoordinateRange:    bundle.Document.CoordinateRange,
	}
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "./samples", "sample output directory")
	seed := fs.Int64("seed", samplegen.DefaultSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	doc, truth, err := samplegen.Write(*out, *seed)
	if err != nil {
		return err
	}
	printJSON(map[string]any{
		"status":     "ok",
		"generator":  doc.Generator,
		"seed":       doc.Seed,
		"glyphs":     len(doc.Glyphs),
		"truthCount": len(truth.Glyphs),
		"directory":  *out,
	})
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	sample := addSampleFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := loadBundle(sample, true)
	if err != nil {
		return err
	}
	type glyphSummary struct {
		ID          string  `json:"id"`
		Contours    int     `json:"contours"`
		Segments    int     `json:"segments"`
		SkeletonPts int     `json:"skeletonPoints"`
		TruthBounds string  `json:"truthBounds"`
		TruthArea   float64 `json:"truthArea"`
		ContentHash string  `json:"contentHash"`
	}
	var out []glyphSummary
	for _, glyph := range bundle.Glyphs {
		segments := 0
		points := 0
		for _, contour := range glyph.Skeletons {
			segments += len(contour.Runs)
			points += len(contour.Points)
		}
		out = append(out, glyphSummary{
			ID:          glyph.Glyph.ID,
			Contours:    len(glyph.Glyph.Contours),
			Segments:    segments,
			SkeletonPts: points,
			TruthBounds: fmt.Sprintf("[%d,%d]-[%d,%d]", glyph.Truth.Bounds.MinX, glyph.Truth.Bounds.MinY, glyph.Truth.Bounds.MaxX, glyph.Truth.Bounds.MaxY),
			TruthArea:   glyph.Truth.ClosedArea,
			ContentHash: glyph.Truth.ContentHash,
		})
	}
	printJSON(map[string]any{
		"status": "ok",
		"schema": bundle.Document.SchemaVersion,
		"seed":   bundle.Document.Seed,
		"glyphs": out,
	})
	return nil
}

func selectGlyphs(bundle *loader.Bundle, id string) ([]loader.Glyph, error) {
	if id == "" {
		return bundle.Glyphs, nil
	}
	glyph, ok := bundle.Find(id)
	if !ok {
		return nil, fmt.Errorf("unknown glyph %q", id)
	}
	return []loader.Glyph{glyph}, nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	r := addRenderFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := loadBundle(r.sample, true)
	if err != nil {
		return err
	}
	glyphs, err := selectGlyphs(bundle, r.glyphID)
	if err != nil {
		return err
	}
	var allErrors []*errs.Error
	var summaries []*export.Summary
	for _, glyph := range glyphs {
		engine, err := pipeline.New(glyph, r.config(bundle))
		if err != nil {
			return err
		}
		var result *pipeline.RunResult
		if r.restore {
			ok, restoreErr := engine.Restore()
			if ok {
				result = engine.Refresh()
			} else if !errors.Is(restoreErr, checkpoint.ErrNoSnapshot) {
				fmt.Fprintf(os.Stderr, "checkpoint invalidated for %s: %v; full redraw required\n", glyph.Glyph.ID, restoreErr)
			}
		}
		if result == nil {
			result, err = engine.Warm(context.Background())
			if err != nil {
				var batch *errs.BatchError
				if errors.As(err, &batch) {
					allErrors = append(allErrors, batch.Errors...)
				} else if e, ok := errs.As(err); ok {
					allErrors = append(allErrors, e)
				} else {
					return err
				}
			}
		}
		if result != nil {
			summary, expErr := export.WriteGlyph(filepath.Join(r.out, glyph.Glyph.ID), glyph, result)
			if expErr != nil {
				return expErr
			}
			summaries = append(summaries, summary)
		}
	}
	status := "ok"
	if len(allErrors) > 0 {
		status = "partial"
	}
	printJSON(map[string]any{"status": status, "glyphs": summaries})
	if len(allErrors) > 0 {
		return &errs.BatchError{Errors: allErrors}
	}
	return nil
}

func cmdIncremental(args []string) error {
	fs := flag.NewFlagSet("incremental", flag.ContinueOnError)
	r := addRenderFlags(fs)
	newWidth := fs.Float64("new-width", 0, "new width (0 keeps current)")
	newSpacing := fs.Float64("new-spacing", 0, "new spacing (0 keeps current)")
	newJoin := fs.String("new-join", "", "new join style")
	newTip := fs.String("new-tip", "", "new tip style")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := loadBundle(r.sample, true)
	if err != nil {
		return err
	}
	glyphs, err := selectGlyphs(bundle, r.glyphID)
	if err != nil {
		return err
	}
	var allErrors []*errs.Error
	var reports []map[string]any
	for _, glyph := range glyphs {
		engine, err := pipeline.New(glyph, r.config(bundle))
		if err != nil {
			return err
		}
		if _, err := engine.Warm(context.Background()); err != nil {
			if e, ok := errs.As(err); ok {
				allErrors = append(allErrors, e)
			}
			continue
		}
		next := engine.Params()
		if *newWidth > 0 {
			next.Width = *newWidth
		}
		if *newSpacing > 0 {
			next.Spacing = *newSpacing
		}
		if *newJoin != "" {
			next.Join = stroke.JoinStyle(*newJoin)
		}
		if *newTip != "" {
			next.Tip = stroke.TipStyle(*newTip)
		}
		incremental, incErr := engine.UpdateParams(context.Background(), next)
		if incErr != nil {
			if e, ok := errs.As(incErr); ok {
				allErrors = append(allErrors, e)
			}
		}
		full, fullErr := engine.FullRender(context.Background())
		if fullErr != nil {
			return fullErr
		}
		diff, cmpErr := pipeline.Compare(incremental, full)
		if cmpErr != nil {
			if e, ok := errs.As(cmpErr); ok {
				allErrors = append(allErrors, e)
			}
			printJSON(map[string]any{"glyphId": glyph.Glyph.ID, "diff": diff, "error": cmpErr.Error()})
		}
		if incremental != nil {
			if _, expErr := export.WriteGlyph(filepath.Join(r.out, glyph.Glyph.ID), glyph, incremental); expErr != nil {
				return expErr
			}
			reports = append(reports, map[string]any{
				"glyphId":          glyph.Glyph.ID,
				"redrawn":          incremental.Redrawn,
				"reused":           incremental.Reused,
				"invalidation":     incremental.Validation,
				"compositeHash":    incremental.CompositeHash,
				"pixelConsistency": cmpErr == nil,
			})
		}
	}
	status := "ok"
	if len(allErrors) > 0 {
		status = "failed"
	}
	printJSON(map[string]any{"status": status, "glyphs": reports})
	if len(allErrors) > 0 {
		sort.SliceStable(allErrors, func(i, j int) bool {
			if allErrors[i].GlyphID == allErrors[j].GlyphID {
				return allErrors[i].SegmentID < allErrors[j].SegmentID
			}
			return allErrors[i].GlyphID < allErrors[j].GlyphID
		})
		return &errs.BatchError{Errors: allErrors}
	}
	return nil
}

func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	r := addRenderFlags(fs)
	kind := fs.String("type", "stage", "self, zlen, dens or stage")
	segment := fs.Int("segment", 2, "target stroke segment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundle, err := loadBundle(r.sample, true)
	if err != nil {
		return err
	}
	glyphs, err := selectGlyphs(bundle, r.glyphID)
	if err != nil {
		return err
	}
	glyph := glyphs[0]
	engine, err := pipeline.New(glyph, r.config(bundle))
	if err != nil {
		return err
	}
	if _, err := engine.Warm(context.Background()); err != nil {
		return err
	}
	if err := engine.SetFaults([]pipeline.Fault{{SegmentID: *segment, Kind: pipeline.FaultKind(*kind)}}); err != nil {
		return err
	}
	result, injected := engine.Retry(context.Background(), *segment)
	out := map[string]any{
		"glyphId":      glyph.Glyph.ID,
		"segmentId":    *segment,
		"fault":        *kind,
		"segmentCount": len(result.Segments),
		"error":        nil,
	}
	if injected != nil {
		out["error"] = injected.Error()
		out["code"] = errorCode(injected)
	}
	if _, err := export.WriteGlyph(filepath.Join(r.out, glyph.Glyph.ID), glyph, result); err != nil {
		return err
	}
	printJSON(out)
	if injected != nil {
		return injected
	}
	return nil
}

func errorCode(err error) string {
	if e, ok := errs.As(err); ok {
		return string(e.Code)
	}
	return "E_STAGE"
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}
