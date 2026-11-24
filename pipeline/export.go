package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type DegradationTrace struct {
	LayerID string `json:"layer_id,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	Path    string `json:"path"`
}

type ExportReport struct {
	TargetVersion int                `json:"target_version"`
	Format        string             `json:"format"`
	LayerFile     string             `json:"layer_file"`
	ContentHash   string             `json:"content_hash"`
	Warnings      []LossyWarning     `json:"warnings"`
	Traces        []DegradationTrace `json:"traces"`
	LayerOrder    []string           `json:"layer_order"`
}

type ExportBundle struct {
	LayerDocument []byte
	Report        ExportReport
}

func ExportModel(model *Model, targetVersion int) (*ExportBundle, error) {
	if model == nil {
		return nil, paraError(-1, "model is nil")
	}
	if err := model.Validate(); err != nil {
		return nil, err
	}
	if targetVersion < 1 || targetVersion > 3 {
		return nil, versionError("$.target_version", "target_version", "unsupported export version %d", targetVersion)
	}
	cp := model.Clone()
	cp.Normalize()
	warnings := []LossyWarning{}
	traces := []DegradationTrace{}
	var document any
	var format string
	switch targetVersion {
	case 1:
		doc, warns, tr := exportV1(cp)
		document, warnings, traces = doc, warns, tr
		format = V1Format
	case 2:
		doc, warns, tr := exportV2(cp)
		document, warnings, traces = doc, warns, tr
		format = V2Format
	case 3:
		doc, warns, tr := exportV3(cp)
		document, warnings, traces = doc, warns, tr
		format = V3Format
	}
	if warnings == nil {
		warnings = []LossyWarning{}
	}
	if traces == nil {
		traces = []DegradationTrace{}
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errCode(CodeDecode, "encode version %d export: %v", targetVersion, err)
	}
	raw = append(raw, '\n')
	sum := sha256.Sum256(raw)
	report := ExportReport{
		TargetVersion: targetVersion, Format: format,
		LayerFile:   fmt.Sprintf("layers-v%d.json", targetVersion),
		ContentHash: hex.EncodeToString(sum[:]),
		Warnings:    warnings, Traces: traces, LayerOrder: append([]string(nil), cp.Stack...),
	}
	return &ExportBundle{LayerDocument: raw, Report: report}, nil
}

// WriteExport writes a deterministic versioned layer document, report and
// optional frame PNGs. Existing files are atomically replaced.
func WriteExport(model *Model, targetVersion int, dir string, frames []*Frame) (ExportReport, error) {
	bundle, err := ExportModel(model, targetVersion)
	if err != nil {
		return ExportReport{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ExportReport{}, errCode(CodeDecode, "create export directory: %v", err)
	}
	layerPath := filepath.Join(dir, bundle.Report.LayerFile)
	if err := writeAtomic(layerPath, bundle.LayerDocument); err != nil {
		return ExportReport{}, err
	}
	for _, frame := range frames {
		pngData, err := frame.PNG()
		if err != nil {
			return ExportReport{}, err
		}
		frameName := fmt.Sprintf("frame-%06d.png", frame.Frame)
		if err := writeAtomic(filepath.Join(dir, frameName), pngData); err != nil {
			return ExportReport{}, err
		}
	}
	reportRaw, err := json.MarshalIndent(bundle.Report, "", "  ")
	if err != nil {
		return ExportReport{}, errCode(CodeDecode, "encode export report: %v", err)
	}
	if err := writeAtomic(filepath.Join(dir, "export-report.json"), append(reportRaw, '\n')); err != nil {
		return ExportReport{}, err
	}
	return bundle.Report, nil
}

func (e *Engine) Export(ctx context.Context, targetVersion int, dir string) (ExportReport, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	model := e.Model()
	var frames []*Frame
	for i := 0; i < e.windows.Len(); i++ {
		window, _ := e.windows.Get(i)
		if window.State != WindowCommitted {
			continue
		}
		result, err := e.ReadWindow(i)
		if err != nil {
			return ExportReport{}, err
		}
		for _, frame := range result.Frames {
			if err := ctx.Err(); err != nil {
				return ExportReport{}, err
			}
			frames = append(frames, frame)
		}
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Frame < frames[j].Frame })
	return WriteExport(model, targetVersion, dir, frames)
}

type v1Scene struct {
	Name          string `json:"name"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Channels      int    `json:"channels"`
	ColorSpace    string `json:"color_space"`
	FrameRateNum  int64  `json:"frame_rate_num"`
	FrameRateDen  int64  `json:"frame_rate_den"`
	Premultiplied bool   `json:"premultiplied"`
}

type v1Layer struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	ZIndex        int        `json:"z_index"`
	Enabled       bool       `json:"enabled"`
	Opacity       float64    `json:"opacity"`
	Color         [3]float64 `json:"color"`
	ColorEncoding string     `json:"color_encoding"`
	Alpha         float64    `json:"alpha"`
	InFrame       int        `json:"in_frame"`
	OutFrame      int        `json:"out_frame"`
	Shape         Shape      `json:"shape"`
}

type exportV1Document struct {
	Format     string    `json:"format"`
	FrameCount int       `json:"frame_count"`
	Scene      v1Scene   `json:"scene"`
	Layers     []v1Layer `json:"layers"`
	LayerOrder []string  `json:"layer_order"`
}

func exportV1(model *Model) (*exportV1Document, []LossyWarning, []DegradationTrace) {
	period := model.TimeBase.Seconds()
	warnings := []LossyWarning{}
	traces := []DegradationTrace{}
	layers := make([]v1Layer, 0, len(model.Layers))
	byIndex := map[string]int{}
	for i, id := range model.Stack {
		byIndex[id] = i
	}
	for _, layer := range model.Layers {
		converted := layer.clone()
		if layer.AlphaMode != AlphaPremultiplied {
			converted.Color[0] *= layer.Alpha
			converted.Color[1] *= layer.Alpha
			converted.Color[2] *= layer.Alpha
			warnings = append(warnings, LossyWarning{
				Code: CodeLossy, LayerID: layer.ID, Property: "alpha_mode",
				From: layer.AlphaMode, To: AlphaPremultiplied,
				Message:   "v1 has one global premultiplication convention; source color was converted to premultiplied alpha",
				TracePath: fmt.Sprintf("layers[%s].color", layer.ID),
			})
			traces = append(traces, DegradationTrace{
				LayerID: layer.ID, From: layer.AlphaMode, To: AlphaPremultiplied,
				Path: fmt.Sprintf("layers[%s].color", layer.ID),
			})
		}
		if layer.ParentID != "" {
			warnings = append(warnings, LossyWarning{
				Code: CodeLossy, LayerID: layer.ID, Property: "hierarchy",
				From: "parent=" + layer.ParentID, To: "flat",
				Message:   "v1 cannot express parent relationships; render order was preserved by flattening",
				TracePath: fmt.Sprintf("layers[%s].parent_id", layer.ID),
			})
			traces = append(traces, DegradationTrace{
				LayerID: layer.ID, From: "parent=" + layer.ParentID, To: "flat",
				Path: fmt.Sprintf("layers[%s].parent_id", layer.ID),
			})
		}
		layers = append(layers, v1Layer{
			ID: layer.ID, Name: layer.Name, Kind: layer.Kind, ZIndex: byIndex[layer.ID],
			Enabled: layer.Visible, Opacity: layer.Opacity, Color: converted.Color,
			ColorEncoding: layer.ColorEncoding, Alpha: layer.Alpha,
			InFrame: layer.InFrame, OutFrame: layer.VisibleUntilFrame, Shape: layer.Shape,
		})
	}
	doc := &exportV1Document{
		Format: V1Format, FrameCount: model.FrameCount,
		Scene: v1Scene{
			Name: model.Name, Width: model.Canvas.Width, Height: model.Canvas.Height,
			Channels: model.Canvas.Channels, ColorSpace: model.Canvas.ColorSpace,
			FrameRateNum: period.Den, FrameRateDen: period.Num, Premultiplied: true,
		},
		Layers: layers, LayerOrder: append([]string(nil), model.Stack...),
	}
	return doc, warnings, traces
}

type v2Canvas struct {
	Width    int `json:"width"`
	Height   int `json:"height"`
	Channels int `json:"channels"`
}

type v2Alpha struct {
	Mode string `json:"mode"`
}

type v2FrameRate struct {
	Numerator   int64 `json:"numerator"`
	Denominator int64 `json:"denominator"`
}

type v2Time struct {
	FrameRate v2FrameRate `json:"frame_rate"`
	Unit      string      `json:"unit"`
}

type v2Visibility struct {
	Visible    bool `json:"visible"`
	StartFrame int  `json:"start_frame"`
	UntilFrame int  `json:"until_frame"`
}

type v2Hierarchy struct {
	Parent *string `json:"parent"`
	Order  int     `json:"order"`
}

type v2Pixel struct {
	Color    [3]float64 `json:"color"`
	Opacity  float64    `json:"opacity"`
	Alpha    float64    `json:"alpha"`
	Encoding string     `json:"encoding"`
}

type v2Bounds struct {
	Kind string    `json:"kind"`
	Rect []float64 `json:"rect,omitempty"`
}

type v2Layer struct {
	UID         string       `json:"uid"`
	DisplayName string       `json:"display_name"`
	LayerType   string       `json:"layer_type"`
	Visibility  v2Visibility `json:"visibility"`
	Hierarchy   v2Hierarchy  `json:"hierarchy"`
	Pixel       v2Pixel      `json:"pixel"`
	Bounds      v2Bounds     `json:"bounds"`
}

type exportV2Document struct {
	Format            string    `json:"format"`
	SpecVersion       string    `json:"spec_version"`
	FrameCount        int       `json:"frame_count"`
	Name              string    `json:"name"`
	Canvas            v2Canvas  `json:"canvas"`
	WorkingColorSpace string    `json:"working_color_space"`
	Alpha             v2Alpha   `json:"alpha"`
	Time              v2Time    `json:"time"`
	Layers            []v2Layer `json:"layers"`
	Stack             []string  `json:"stack"`
}

func exportV2(model *Model) (*exportV2Document, []LossyWarning, []DegradationTrace) {
	period := model.TimeBase.Seconds()
	targetAlpha := AlphaStraight
	for _, layer := range model.Layers {
		if layer.AlphaMode == AlphaPremultiplied {
			targetAlpha = AlphaPremultiplied
			break
		}
	}
	warnings := []LossyWarning{}
	traces := []DegradationTrace{}
	byOrder := map[string]int{}
	for i, id := range model.Stack {
		byOrder[id] = i
	}
	layers := make([]v2Layer, 0, len(model.Layers))
	for _, layer := range model.Layers {
		converted := layer.clone()
		if layer.AlphaMode != targetAlpha {
			if targetAlpha == AlphaPremultiplied {
				converted.Color[0] *= layer.Alpha
				converted.Color[1] *= layer.Alpha
				converted.Color[2] *= layer.Alpha
			} else if layer.Alpha > 0 {
				converted.Color[0] /= layer.Alpha
				converted.Color[1] /= layer.Alpha
				converted.Color[2] /= layer.Alpha
			}
			warnings = append(warnings, LossyWarning{
				Code: CodeLossy, LayerID: layer.ID, Property: "alpha_mode",
				From: layer.AlphaMode, To: targetAlpha,
				Message:   "v2 has one global alpha convention; source color was normalized",
				TracePath: fmt.Sprintf("layers[%s].pixel.color", layer.ID),
			})
			traces = append(traces, DegradationTrace{
				LayerID: layer.ID, From: layer.AlphaMode, To: targetAlpha,
				Path: fmt.Sprintf("layers[%s].pixel.color", layer.ID),
			})
		}
		var parent *string
		if layer.ParentID != "" {
			p := layer.ParentID
			parent = &p
		}
		bounds := v2Bounds{Kind: layer.Shape.Type}
		if layer.Shape.Type == ShapeRect || layer.Shape.Type == ShapeEllipse {
			bounds.Rect = layer.Shape.Rect[:]
		}
		layers = append(layers, v2Layer{
			UID: layer.ID, DisplayName: layer.Name, LayerType: layer.Kind,
			Visibility: v2Visibility{Visible: layer.Visible, StartFrame: layer.InFrame, UntilFrame: layer.VisibleUntilFrame},
			Hierarchy:  v2Hierarchy{Parent: parent, Order: byOrder[layer.ID]},
			Pixel:      v2Pixel{Color: converted.Color, Opacity: layer.Opacity, Alpha: layer.Alpha, Encoding: layer.ColorEncoding},
			Bounds:     bounds,
		})
	}
	doc := &exportV2Document{
		Format: V2Format, SpecVersion: "2.0", FrameCount: model.FrameCount, Name: model.Name,
		Canvas:            v2Canvas{Width: model.Canvas.Width, Height: model.Canvas.Height, Channels: model.Canvas.Channels},
		WorkingColorSpace: model.Canvas.ColorSpace, Alpha: v2Alpha{Mode: targetAlpha},
		Time:   v2Time{FrameRate: v2FrameRate{Numerator: period.Den, Denominator: period.Num}, Unit: "second"},
		Layers: layers, Stack: append([]string(nil), model.Stack...),
	}
	return doc, warnings, traces
}

type v3Canvas struct {
	Width    int `json:"w"`
	Height   int `json:"h"`
	Channels int `json:"channels"`
}

type v3Color struct {
	Space         string `json:"space"`
	AlphaMode     string `json:"alpha_mode"`
	PerLayerAlpha bool   `json:"per_layer_alpha"`
}

type v3Timing struct {
	TickRate      int64  `json:"tick_rate"`
	TicksPerFrame int64  `json:"ticks_per_frame"`
	Unit          string `json:"unit"`
}

type v3Lifecycle struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type v3Source struct {
	Color         [3]float64 `json:"color"`
	Encoding      string     `json:"encoding"`
	Alpha         float64    `json:"alpha"`
	Premultiplied string     `json:"premultiplied"`
}

type v3Blend struct {
	Opacity float64 `json:"opacity"`
}

type v3Mask struct {
	Type string    `json:"type"`
	Rect []float64 `json:"rect,omitempty"`
}

type v3Layer struct {
	LayerKey  string      `json:"layer_key"`
	Title     string      `json:"title"`
	Role      string      `json:"role"`
	Visible   bool        `json:"visible"`
	Lifecycle v3Lifecycle `json:"lifecycle"`
	Parent    *string     `json:"parent"`
	Source    v3Source    `json:"source"`
	Blend     v3Blend     `json:"blend"`
	Mask      v3Mask      `json:"mask"`
}

type v3Hierarchy struct {
	Roots       []string            `json:"roots"`
	Children    map[string][]string `json:"children"`
	RenderOrder []string            `json:"render_order"`
}

type exportV3Document struct {
	Format        string      `json:"format"`
	SchemaVersion int         `json:"schema_version"`
	FrameCount    int         `json:"frame_count"`
	Name          string      `json:"name"`
	Canvas        v3Canvas    `json:"canvas"`
	Color         v3Color     `json:"color"`
	Timing        v3Timing    `json:"timing"`
	Layers        []v3Layer   `json:"layers"`
	Hierarchy     v3Hierarchy `json:"hierarchy"`
}

func exportV3(model *Model) (*exportV3Document, []LossyWarning, []DegradationTrace) {
	period := model.TimeBase.Seconds()
	tickRate, ticksPerFrame := exactTicks(period)
	layers := make([]v3Layer, 0, len(model.Layers))
	children := map[string][]string{}
	roots := []string{}
	layerIDs := map[string]bool{}
	for _, layer := range model.Layers {
		layerIDs[layer.ID] = true
	}
	for _, layer := range model.Layers {
		var parent *string
		if layer.ParentID != "" && layerIDs[layer.ParentID] {
			p := layer.ParentID
			parent = &p
			children[p] = append(children[p], layer.ID)
		} else {
			roots = append(roots, layer.ID)
		}
		mask := v3Mask{Type: layer.Shape.Type}
		if layer.Shape.Type == ShapeRect || layer.Shape.Type == ShapeEllipse {
			mask.Rect = layer.Shape.Rect[:]
		}
		layers = append(layers, v3Layer{
			LayerKey: layer.ID, Title: layer.Name, Role: layer.Kind, Visible: layer.Visible,
			Lifecycle: v3Lifecycle{Start: layer.InFrame, End: layer.VisibleUntilFrame},
			Parent:    parent,
			Source: v3Source{
				Color: layer.Color, Encoding: layer.ColorEncoding, Alpha: layer.Alpha,
				Premultiplied: layer.AlphaMode,
			},
			Blend: v3Blend{Opacity: layer.Opacity}, Mask: mask,
		})
	}
	for _, list := range children {
		sort.Strings(list)
	}
	doc := &exportV3Document{
		Format: V3Format, SchemaVersion: 3, FrameCount: model.FrameCount, Name: model.Name,
		Canvas:    v3Canvas{Width: model.Canvas.Width, Height: model.Canvas.Height, Channels: model.Canvas.Channels},
		Color:     v3Color{Space: model.Canvas.ColorSpace, AlphaMode: AlphaStraight, PerLayerAlpha: true},
		Timing:    v3Timing{TickRate: tickRate, TicksPerFrame: ticksPerFrame, Unit: "ticks"},
		Layers:    layers,
		Hierarchy: v3Hierarchy{Roots: roots, Children: children, RenderOrder: append([]string(nil), model.Stack...)},
	}
	return doc, nil, nil
}

func exactTicks(period Rational) (int64, int64) {
	if period.Num <= 0 || period.Den <= 0 {
		return 24000, 1000
	}
	const preferredTickRate int64 = 24000
	if scaled := preferredTickRate * period.Num; scaled%period.Den == 0 {
		return preferredTickRate, scaled / period.Den
	}
	tickRate := period.Den
	ticks := period.Num
	return tickRate, ticks
}
