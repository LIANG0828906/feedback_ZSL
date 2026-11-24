package formats

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"virtualstudio/compose"
	"virtualstudio/model"
)

type ExportResult struct {
	TargetVersion string             `json:"target_version"`
	Descriptor    []byte             `json:"-"`
	Warnings      []model.Diagnostic `json:"warnings"`
}

type v1Export struct {
	FormatVersion   string          `json:"format_version"`
	Resolution      v1ExportSize    `json:"resolution"`
	FrameCount      int             `json:"frame_count"`
	FrameDurationUS int64           `json:"frame_duration_us"`
	Layers          []v1ExportLayer `json:"layers"`
}

type v1ExportSize struct {
	Width    int `json:"w"`
	Height   int `json:"h"`
	Channels int `json:"channels"`
}

type v1ExportLayer struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Visible   bool    `json:"visible"`
	ZIndex    int     `json:"z_index"`
	Opacity   float64 `json:"opacity"`
	AlphaMode string  `json:"alpha_mode"`
	RGBA      string  `json:"rgba_base64"`
}

type v2Export struct {
	Schema string          `json:"schema"`
	Canvas v2Canvas        `json:"canvas"`
	Timing v2Timing        `json:"timing"`
	Stack  []v2ExportLayer `json:"stack"`
}

type v2ExportLayer struct {
	LayerID       string          `json:"layer_id"`
	DisplayName   string          `json:"display_name"`
	Visibility    string          `json:"visibility"`
	Opacity       float64         `json:"opacity"`
	PixelEncoding v2PixelEncoding `json:"pixel_encoding"`
	RGBA          string          `json:"rgba"`
}

type v3Export struct {
	Spec        string                   `json:"spec"`
	Dimensions  v3Dimensions             `json:"dimensions"`
	Time        v3ExportTime             `json:"time"`
	SampleDepth int                      `json:"sample_depth"`
	Layers      map[string]v3ExportLayer `json:"layers"`
	OrderLinks  []v3Link                 `json:"order_links"`
}

type v3ExportTime struct {
	Frames        int   `json:"frames"`
	TicksPerFrame int64 `json:"ticks_per_frame"`
	TickRateHz    int64 `json:"tick_rate_hz"`
}

type v3ExportLayer struct {
	Name    string  `json:"name"`
	Enabled int     `json:"enabled"`
	Opacity float64 `json:"opacity"`
	PMA     bool    `json:"pma"`
	Pixels  string  `json:"pixels"`
}

func ExportScene(scene *model.Scene, target string) (*ExportResult, error) {
	if scene == nil {
		return nil, model.NewError(model.CodeVersion, "scene", "scene is nil")
	}
	if err := model.ValidateScene(scene); err != nil {
		return nil, err
	}
	switch target {
	case "v1":
		return exportV1(scene)
	case "v2":
		return exportV2(scene)
	case "v3":
		return exportV3(scene)
	default:
		return nil, model.NewError(model.CodeVersion, "target", fmt.Sprintf("unsupported target version %q", target))
	}
}

func exportV1(scene *model.Scene) (*ExportResult, error) {
	doc := v1Export{
		FormatVersion:   "foreground-v1",
		Resolution:      v1ExportSize{Width: scene.Width, Height: scene.Height, Channels: scene.Channels},
		FrameCount:      scene.FrameCount,
		FrameDurationUS: scene.FrameDurationNS / 1_000,
	}
	if scene.FrameDurationNS%1_000 != 0 {
		return nil, model.NewError(model.CodeParam, "frame_duration_ns", "cannot be represented exactly in v1 microseconds")
	}
	warnings := orderWarning(scene, "v1")
	for i, layer := range scene.Layers {
		if layer.SourceAlphaMode != model.AlphaStraight {
			warnings = append(warnings, model.Diagnostic{
				Code:       model.CodeLossy,
				Path:       fmt.Sprintf("layers[%d].source_alpha_mode", i),
				Original:   string(layer.SourceAlphaMode),
				Resolution: "converted to straight alpha; v1 cannot encode premultiplied samples",
			})
		}
		doc.Layers = append(doc.Layers, v1ExportLayer{
			ID:        layer.ID,
			Label:     layer.Name,
			Visible:   layer.Visible,
			ZIndex:    i,
			Opacity:   layer.Opacity,
			AlphaMode: string(model.AlphaStraight),
			RGBA:      base64.StdEncoding.EncodeToString(layer.Pixels),
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "v1.export", "marshal descriptor", err)
	}
	return &ExportResult{TargetVersion: "v1", Descriptor: append(data, '\n'), Warnings: warnings}, nil
}

func exportV2(scene *model.Scene) (*ExportResult, error) {
	if scene.FrameDurationNS%1_000_000 != 0 {
		return nil, model.NewError(model.CodeParam, "frame_duration_ns", "cannot be represented exactly in v2 milliseconds")
	}
	doc := v2Export{
		Schema: "foreground/v2",
		Canvas: v2Canvas{Width: ptr(scene.Width), Height: ptr(scene.Height), Channels: ptr(scene.Channels)},
		Timing: v2Timing{FrameCount: ptr(scene.FrameCount), FrameDurationMS: ptr(scene.FrameDurationNS / 1_000_000)},
	}
	warnings := orderWarning(scene, "v2")
	for i, layer := range scene.Layers {
		if layer.SourceAlphaMode != model.AlphaStraight {
			warnings = append(warnings, model.Diagnostic{
				Code:       model.CodeLossy,
				Path:       fmt.Sprintf("stack[%d].pixel_encoding.alpha", i),
				Original:   string(layer.SourceAlphaMode),
				Resolution: "converted to straight alpha; v2 profile only accepts straight samples",
			})
		}
		visibility := "HIDDEN"
		if layer.Visible {
			visibility = "VISIBLE"
		}
		doc.Stack = append(doc.Stack, v2ExportLayer{
			LayerID:       layer.ID,
			DisplayName:   layer.Name,
			Visibility:    visibility,
			Opacity:       layer.Opacity,
			PixelEncoding: v2PixelEncoding{Alpha: ptr(string(model.AlphaStraight))},
			RGBA:          base64.StdEncoding.EncodeToString(layer.Pixels),
		})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "v2.export", "marshal descriptor", err)
	}
	return &ExportResult{TargetVersion: "v2", Descriptor: append(data, '\n'), Warnings: warnings}, nil
}

func exportV3(scene *model.Scene) (*ExportResult, error) {
	doc := v3Export{
		Spec:        "virtualstudio/foreground@3",
		Dimensions:  v3Dimensions{Width: ptr(scene.Width), Height: ptr(scene.Height), Channels: ptr(scene.Channels)},
		Time:        v3ExportTime{Frames: scene.FrameCount, TicksPerFrame: scene.FrameDurationNS, TickRateHz: 1_000_000_000},
		SampleDepth: 16,
		Layers:      make(map[string]v3ExportLayer, len(scene.Layers)),
	}
	for i, layer := range scene.Layers {
		enabled := 0
		if layer.Visible {
			enabled = 1
		}
		var above *string
		if i > 0 {
			id := scene.Layers[i-1].ID
			above = &id
		}
		doc.Layers[layer.ID] = v3ExportLayer{
			Name:    layer.Name,
			Enabled: enabled,
			Opacity: layer.Opacity,
			PMA:     true,
			Pixels:  base64.StdEncoding.EncodeToString(compose.PremultiplyPixels16(layer.Pixels)),
		}
		doc.OrderLinks = append(doc.OrderLinks, v3Link{ID: ptr(layer.ID), Above: above})
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "v3.export", "marshal descriptor", err)
	}
	return &ExportResult{TargetVersion: "v3", Descriptor: append(data, '\n'), Warnings: nil}, nil
}

func orderWarning(scene *model.Scene, target string) []model.Diagnostic {
	if scene.OrderExpr == "z-index" || target == "v3" {
		return nil
	}
	path := "order_expression"
	resolution := "flattened internal bottom-to-top order into z-index values"
	if target == "v2" {
		path = "order_expression"
		resolution = "flattened linked hierarchy into an ordered stack"
	}
	return []model.Diagnostic{{
		Code:       model.CodeLossy,
		Path:       path,
		Original:   scene.OrderExpr,
		Resolution: resolution,
	}}
}

func ptr[T any](v T) *T { return &v }

type BundleManifest struct {
	TargetVersion string             `json:"target_version"`
	Descriptor    string             `json:"descriptor"`
	Frames        []string           `json:"frames"`
	Warnings      []model.Diagnostic `json:"warnings"`
	FrameDigest   string             `json:"frame_digest"`
}

func WriteBundle(dir string, scene *model.Scene, frames []model.Frame, target string) (*BundleManifest, error) {
	result, err := ExportScene(scene, target)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.WrapError(model.CodeIO, dir, "create export directory", err)
	}
	descriptorName := fmt.Sprintf("scene.%s.json", target)
	if err := os.WriteFile(filepath.Join(dir, descriptorName), result.Descriptor, 0o644); err != nil {
		return nil, model.WrapError(model.CodeIO, descriptorName, "write descriptor", err)
	}
	manifest := &BundleManifest{
		TargetVersion: target,
		Descriptor:    descriptorName,
		Warnings:      result.Warnings,
		FrameDigest:   compose.WindowDigest(frames),
	}
	for _, frame := range frames {
		name := fmt.Sprintf("frame_%04d.png", frame.Index)
		pngData, err := compose.FramePNG(frame)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), pngData, 0o644); err != nil {
			return nil, model.WrapError(model.CodeIO, name, "write frame", err)
		}
		manifest.Frames = append(manifest.Frames, name)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "manifest", "marshal manifest", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		return nil, model.WrapError(model.CodeIO, "manifest.json", "write manifest", err)
	}
	return manifest, nil
}
