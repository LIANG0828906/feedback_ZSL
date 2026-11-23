//go:build fyne

// Package ui implements the Fyne desktop diagnostics panel.
package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/export"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/stroke"
	"glyphbrush/internal/viz"
)

type Options struct {
	CheckpointDir string
	ExportRoot    string
	MaxParallel   int
	PolygonLimit  int
}

type panel struct {
	app    fyne.App
	win    fyne.Window
	bundle *loader.Bundle
	opts   Options

	engines map[string]*pipeline.Engine
	result  *pipeline.RunResult
	glyph   loader.Glyph

	glyphSelect *widget.Select
	width       *widget.Slider
	spacing     *widget.Slider
	join        *widget.Select
	tip         *widget.Select
	maxParallel *widget.Entry
	segment     *widget.Entry
	fault       *widget.Select

	outlineImg *canvas.Image
	bandImg    *canvas.Image
	maskImg    *canvas.Image
	heatImg    *canvas.Image
	status     *widget.Label
	invalidate *widget.Label
	hash       *widget.Label
}

// Run launches the Fyne panel. Rendering is local and deterministic; the
// engine's worker pool bounds segment parallelism.
func Run(bundle *loader.Bundle, opts Options) {
	p := &panel{
		app:     app.New(),
		bundle:  bundle,
		opts:    opts,
		engines: make(map[string]*pipeline.Engine),
	}
	p.win = p.app.NewWindow("GlyphBrush — glyph stroke diagnostics")
	p.win.Resize(fyne.NewSize(1440, 900))
	p.win.SetContent(p.build())
	p.activate(bundle.Glyphs[0].Glyph.ID)
	p.win.ShowAndRun()
}

func (p *panel) build() fyne.CanvasObject {
	p.glyphSelect = widget.NewSelect(glyphIDs(p.bundle), func(id string) {
		if id != "" {
			p.activate(id)
		}
	})
	p.glyphSelect.PlaceHolder = "glyph"

	p.width = widget.NewSlider(2, 120)
	p.width.SetValue(stroke.DefaultParams().Width)
	p.spacing = widget.NewSlider(0.1, 2)
	p.spacing.SetValue(stroke.DefaultParams().Spacing)
	p.join = widget.NewSelect([]string{"round", "bevel", "miter"}, nil)
	p.join.SetSelected("round")
	p.tip = widget.NewSelect([]string{"round", "flat", "square"}, nil)
	p.tip.SetSelected("round")
	p.maxParallel = widget.NewEntry()
	parallel := p.opts.MaxParallel
	if parallel <= 0 {
		parallel = 4
	}
	p.maxParallel.SetText(strconv.Itoa(parallel))
	p.segment = widget.NewEntry()
	p.segment.SetText("1")
	p.fault = widget.NewSelect([]string{"self", "zlen", "dens", "stage"}, nil)
	p.fault.SetSelected("stage")

	apply := widget.NewButton("Apply parameters", p.applyParams)
	retry := widget.NewButton("Retry segment", p.retry)
	clear := widget.NewButton("Clear segment fault", p.clearFault)
	inject := widget.NewButton("Inject fault", p.inject)
	restore := widget.NewButton("Restore checkpoint", p.restore)
	exportBtn := widget.NewButton("Export PNG + JSON", p.export)

	controls := container.NewVBox(
		widget.NewLabel("Glyph"),
		p.glyphSelect,
		widget.NewLabel("Width"),
		p.width,
		widget.NewLabel("Spacing (× width)"),
		p.spacing,
		container.NewGridWithColumns(2, p.join, p.tip),
		widget.NewLabel("Max parallel"),
		p.maxParallel,
		container.NewGridWithColumns(2, apply, restore),
		separator(),
		widget.NewLabel("Segment"),
		p.segment,
		container.NewGridWithColumns(2, retry, clear),
		widget.NewLabel("Fault"),
		p.fault,
		inject,
		separator(),
		exportBtn,
	)

	p.outlineImg = canvas.NewImageFromImage(nil)
	p.outlineImg.FillMode = canvas.ImageFillContain
	p.outlineImg.SetMinSize(fyne.NewSize(620, 620))
	p.bandImg = canvas.NewImageFromImage(nil)
	p.bandImg.FillMode = canvas.ImageFillContain
	p.bandImg.SetMinSize(fyne.NewSize(620, 620))
	p.maskImg = canvas.NewImageFromImage(nil)
	p.maskImg.FillMode = canvas.ImageFillContain
	p.maskImg.SetMinSize(fyne.NewSize(620, 620))
	p.heatImg = canvas.NewImageFromImage(nil)
	p.heatImg.FillMode = canvas.ImageFillContain
	p.heatImg.SetMinSize(fyne.NewSize(620, 620))
	tabs := container.NewAppTabs(
		container.NewTabItem("Contour + skeleton", p.outlineImg),
		container.NewTabItem("Stroke bands", p.bandImg),
		container.NewTabItem("Gray mask", p.maskImg),
		container.NewTabItem("Density heatmap", p.heatImg),
	)

	p.status = widget.NewLabel("")
	p.status.TextStyle = fyne.TextStyle{Monospace: true}
	p.invalidate = widget.NewLabel("")
	p.invalidate.Wrapping = fyne.TextWrapWord
	p.hash = widget.NewLabel("")
	p.hash.Wrapping = fyne.TextWrapWord
	inspector := container.NewVBox(
		widget.NewLabelWithStyle("Segment state matrix", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		p.status,
		widget.NewLabelWithStyle("Invalidation propagation", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		p.invalidate,
		widget.NewLabelWithStyle("Version / hash", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		p.hash,
	)
	left := container.NewScroll(controls)
	left.SetMinSize(fyne.NewSize(280, 800))
	right := container.NewVSplit(tabs, container.NewScroll(inspector))
	right.SetOffset(0.68)
	body := container.NewBorder(nil, nil, left, nil, right)
	return body
}

func (p *panel) activate(id string) {
	glyph, ok := p.bundle.Find(id)
	if !ok {
		return
	}
	p.glyph = glyph
	engine := p.engines[id]
	if engine == nil {
		cfg := p.config()
		var err error
		engine, err = pipeline.New(glyph, cfg)
		if err != nil {
			dialog.ShowError(err, p.win)
			return
		}
		result, err := engine.Warm(context.Background())
		if err != nil {
			p.showError(err)
		}
		p.engines[id] = engine
		p.result = result
	} else {
		p.result = engine.Refresh()
	}
	p.refresh()
}

func (p *panel) config() pipeline.Config {
	parallel := 4
	if value, err := strconv.Atoi(p.maxParallel.Text); err == nil && value > 0 {
		parallel = value
	}
	return pipeline.Config{
		Params: stroke.Params{
			Width:           p.width.Value,
			Spacing:         p.spacing.Value,
			Join:            stroke.JoinStyle(p.join.Selected),
			Tip:             stroke.TipStyle(p.tip.Selected),
			CurvatureFactor: stroke.DefaultParams().CurvatureFactor,
		},
		Raster:             raster.DefaultConfig(),
		MaxParallel:        parallel,
		PolygonBufferLimit: p.opts.PolygonLimit,
		Store:              checkpoint.New(p.opts.CheckpointDir),
		DensityRange:       p.bundle.Document.DensityRange,
		CoordinateRange:    p.bundle.Document.CoordinateRange,
	}
}

func (p *panel) applyParams() {
	engine := p.engines[p.glyph.Glyph.ID]
	if engine == nil {
		p.activate(p.glyph.Glyph.ID)
		engine = p.engines[p.glyph.Glyph.ID]
	}
	params := stroke.Params{
		Width:           p.width.Value,
		Spacing:         p.spacing.Value,
		Join:            stroke.JoinStyle(p.join.Selected),
		Tip:             stroke.TipStyle(p.tip.Selected),
		CurvatureFactor: stroke.DefaultParams().CurvatureFactor,
	}
	result, err := engine.UpdateParams(context.Background(), params)
	if result != nil {
		p.result = result
	}
	p.refresh()
	if err != nil {
		p.showError(err)
	}
}

func (p *panel) retry() {
	engine := p.engines[p.glyph.Glyph.ID]
	if engine == nil {
		return
	}
	id, _ := strconv.Atoi(p.segment.Text)
	result, err := engine.Retry(context.Background(), id)
	if result != nil {
		p.result = result
	}
	p.refresh()
	if err != nil {
		p.showError(err)
	}
}

func (p *panel) clearFault() {
	engine := p.engines[p.glyph.Glyph.ID]
	if engine == nil {
		return
	}
	id, _ := strconv.Atoi(p.segment.Text)
	engine.ClearFault(id)
	p.invalidate.SetText(fmt.Sprintf("fault cleared for segment %d; use Retry segment", id))
}

func (p *panel) inject() {
	engine := p.engines[p.glyph.Glyph.ID]
	if engine == nil {
		return
	}
	id, _ := strconv.Atoi(p.segment.Text)
	if err := engine.SetFaults([]pipeline.Fault{{SegmentID: id, Kind: pipeline.FaultKind(p.fault.Selected)}}); err != nil {
		p.showError(err)
		return
	}
	result, err := engine.Retry(context.Background(), id)
	if result != nil {
		p.result = result
	}
	p.refresh()
	if err != nil {
		p.showError(err)
	}
}

func (p *panel) restore() {
	cfg := p.config()
	engine, err := pipeline.New(p.glyph, cfg)
	if err != nil {
		p.showError(err)
		return
	}
	ok, restoreErr := engine.Restore()
	if ok {
		p.engines[p.glyph.Glyph.ID] = engine
		p.result = engine.Refresh()
		p.invalidate.SetText("checkpoint restored; all segments reused")
	} else {
		result, warmErr := engine.Warm(context.Background())
		p.engines[p.glyph.Glyph.ID] = engine
		p.result = result
		if warmErr != nil {
			p.showError(warmErr)
		}
		if restoreErr != nil {
			p.invalidate.SetText("checkpoint discarded (" + restoreErr.Error() + "); full redraw completed")
		}
	}
	p.refresh()
}

func (p *panel) export() {
	if p.result == nil {
		return
	}
	dir := filepath.Join(p.opts.ExportRoot, p.glyph.Glyph.ID)
	if _, err := export.WriteGlyph(dir, p.glyph, p.result); err != nil {
		p.showError(err)
		return
	}
	p.invalidate.SetText("exported to " + dir)
}

func (p *panel) refresh() {
	if p.result == nil {
		return
	}
	p.outlineImg.Image = viz.Outline(p.glyph, p.result)
	p.outlineImg.Refresh()
	p.bandImg.Image = viz.Bands(p.glyph, p.result)
	p.bandImg.Refresh()
	p.maskImg.Image = viz.MaskImage(p.result.Mask)
	p.maskImg.Refresh()
	p.heatImg.Image = viz.Heatmap(p.result)
	p.heatImg.Refresh()
	p.status.SetText(statusMatrix(p.result))
	p.invalidate.SetText(invalidationText(p.result))
	p.hash.SetText(fmt.Sprintf("glyph=%s\nparam=%s\nalgorithm=%s\ncomposite=%s\ngrid=%dx%d",
		p.result.GlyphHash, p.result.ParamVersion, p.result.Algorithm, p.result.CompositeHash,
		p.result.Grid.Width, p.result.Grid.Height))
}

func (p *panel) showError(err error) {
	if e, ok := errs.As(err); ok {
		message := e.Error()
		if e.PixelX != nil && e.PixelY != nil {
			message += fmt.Sprintf("\nfirst diff pixel: (%d,%d)", *e.PixelX, *e.PixelY)
		}
		if e.SegmentID != 0 {
			message += fmt.Sprintf("\nsegment: %d", e.SegmentID)
		}
		dialog.ShowError(fmt.Errorf("%s", message), p.win)
		return
	}
	dialog.ShowError(err, p.win)
}

func statusMatrix(result *pipeline.RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-4s %-3s %-4s %-10s %-10s %-9s %-7s %s\n", "SEG", "C", "K", "ARC-START", "ARC-END", "STATE", "AREA", "DENS")
	for _, segment := range result.Segments {
		area := 0.0
		if segment.Band != nil {
			area = segment.Band.Area
		}
		state := string(segment.Status)
		if segment.Error != nil {
			state = string(segment.Error.Code)
		}
		fmt.Fprintf(&b, "%-4d %-3d %-4d %-10.1f %-10.1f %-9s %-7.1f %d\n",
			segment.SegmentID, segment.ContourIndex, segment.CurveIndex,
			segment.ArcStart, segment.ArcEnd, state, area, len(segment.Densities))
	}
	return b.String()
}

func invalidationText(result *pipeline.RunResult) string {
	if result.Validation == nil {
		return "no invalidation report (full render)"
	}
	var b strings.Builder
	if result.Validation.Empty {
		b.WriteString("no segments invalidated\n")
	}
	for _, entry := range result.Validation.Entries {
		fmt.Fprintf(&b, "segment %d [c%d/k%d] ← %s (%s)\n",
			entry.SegmentID, entry.ContourIndex, entry.CurveIndex, entry.Param, entry.Reason)
	}
	if len(result.Validation.TopoOrder) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(result.Validation.TopoOrder, "\n  ↓\n"))
	}
	b.WriteString("\n\nredrawn: ")
	b.WriteString(intsText(result.Redrawn))
	b.WriteString("\nreused:  ")
	b.WriteString(intsText(result.Reused))
	return b.String()
}

func intsText(values []int) string {
	if len(values) == 0 {
		return "(none)"
	}
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ", ")
}

func glyphIDs(bundle *loader.Bundle) []string {
	ids := make([]string, len(bundle.Glyphs))
	for i, glyph := range bundle.Glyphs {
		ids[i] = glyph.Glyph.ID
	}
	return ids
}

func separator() fyne.CanvasObject {
	return widget.NewSeparator()
}
