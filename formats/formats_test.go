package formats_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"virtualstudio/compose"
	"virtualstudio/formats"
	"virtualstudio/model"
	"virtualstudio/sample"
)

func TestThreeVersionsLoadToSameEffectiveStack(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 42); err != nil {
		t.Fatal(err)
	}
	var scenes []*model.Scene
	for _, version := range formats.SupportedVersions {
		scene, err := formats.LoadScene(dir + "/" + version + "/scene.json")
		if err != nil {
			t.Fatal(err)
		}
		scenes = append(scenes, scene)
	}
	base := semanticScene(scenes[0])
	for i := 1; i < len(scenes); i++ {
		if got := semanticScene(scenes[i]); got != base {
			t.Fatalf("version %s effective stack differs\nbase=%s\ngot=%s", formats.SupportedVersions[i], base, got)
		}
	}
	if scenes[0].OrderExpr != "z-index" || scenes[1].OrderExpr != "ordered-stack" || scenes[2].OrderExpr != "linked-above" {
		t.Fatalf("order expression difference missing: %s/%s/%s", scenes[0].OrderExpr, scenes[1].OrderExpr, scenes[2].OrderExpr)
	}
	if scenes[2].Layers[0].SourceAlphaMode != model.AlphaPremul {
		t.Fatalf("v3 premultiplied source convention not retained")
	}
}

func TestLoaderErrors(t *testing.T) {
	tests := []struct {
		name  string
		doc   string
		code  model.ErrorCode
		path  string
		layer string
	}{
		{"missing discriminator", `{}`, model.CodeVersion, "format_version", ""},
		{"unknown version", `{"format_version":"v9"}`, model.CodeVersion, "format_version", ""},
		{"missing required", `{"format_version":"foreground-v1"}`, model.CodeVersion, "resolution", ""},
		{"cycle", cycleDoc(), model.CodeCycle, "order_links", "a"},
		{"decode", corruptBase64Doc(), model.CodeDecode, "rgba_base64", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := formats.ParseScene([]byte(tc.doc))
			if err == nil {
				t.Fatal("expected an error")
			}
			e, ok := model.AsError(err)
			if !ok {
				t.Fatalf("not a model error: %v", err)
			}
			if e.Code != tc.code || !strings.Contains(e.Path, tc.path) || (tc.layer != "" && e.LayerID != tc.layer) {
				t.Fatalf("got code=%s path=%s layer=%s", e.Code, e.Path, e.LayerID)
			}
		})
	}
}

func TestExportRoundTripAndLossyReport(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 42); err != nil {
		t.Fatal(err)
	}
	v3, err := formats.LoadScene(dir + "/v3/scene.json")
	if err != nil {
		t.Fatal(err)
	}
	first, err := formats.ExportScene(v3, "v1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := formats.ExportScene(v3, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Descriptor) != string(second.Descriptor) {
		t.Fatal("repeat export is not byte-identical")
	}
	if len(first.Warnings) == 0 {
		t.Fatal("expected E_LOSSY warnings")
	}
	seenAlpha, seenOrder := false, false
	for _, warning := range first.Warnings {
		if warning.Code != model.CodeLossy {
			t.Fatalf("warning code=%s", warning.Code)
		}
		if warning.Original == "" || warning.Resolution == "" {
			t.Fatalf("incomplete downgrade record: %+v", warning)
		}
		seenAlpha = seenAlpha || strings.Contains(warning.Path, "alpha")
		seenOrder = seenOrder || strings.Contains(warning.Path, "order")
	}
	if !seenAlpha || !seenOrder {
		t.Fatalf("lossy report does not cover alpha and order: %+v", first.Warnings)
	}
	loadedV1, err := formats.ParseScene(first.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if semanticScene(loadedV1) != semanticScene(v3) {
		t.Fatal("v3 -> v1 reload changed the effective stack")
	}
	v1, err := formats.LoadScene(dir + "/v1/scene.json")
	if err != nil {
		t.Fatal(err)
	}
	up, err := formats.ExportScene(v1, "v3")
	if err != nil {
		t.Fatal(err)
	}
	loadedV3, err := formats.ParseScene(up.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if semanticScene(loadedV3) != semanticScene(v1) {
		t.Fatal("v1 -> v3 reload changed the effective stack")
	}

	frames, err := compose.RenderAll(v1, mustCommands(t, dir), compose.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	if _, err := formats.WriteBundle(dirA, v1, frames, "v3"); err != nil {
		t.Fatal(err)
	}
	if _, err := formats.WriteBundle(dirB, v1, frames, "v3"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scene.v3.json", "manifest.json", "frame_0000.png", "frame_0023.png"} {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("bundle file %s is not byte-deterministic", name)
		}
	}
}

func mustCommands(t *testing.T, dir string) *model.CommandSet {
	t.Helper()
	scene, err := formats.LoadScene(dir + "/v1/scene.json")
	if err != nil {
		t.Fatal(err)
	}
	commands, err := formats.LoadCommands(dir+"/commands.json", scene)
	if err != nil {
		t.Fatal(err)
	}
	return commands
}

func semanticScene(scene *model.Scene) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d/%d/%d/%d", scene.Width, scene.Height, scene.Channels,
		scene.FrameCount, scene.FrameDurationNS)
	for _, layer := range scene.Layers {
		fmt.Fprintf(&b, "|%s:%s:%t:%.6f:%s", layer.ID, layer.Name, layer.Visible,
			layer.Opacity, model.HashBytes(layer.Pixels))
	}
	return b.String()
}

func cycleDoc() string {
	pixel := "AAAAAAAAAAA="
	return fmt.Sprintf(`{"spec":"virtualstudio/foreground@3",
	"dimensions":{"width":1,"height":1,"channels":4},
	"time":{"frames":1,"ticks_per_frame":1,"tick_rate_hz":1000000000},
	"layers":{"a":{"name":"A","enabled":1,"opacity":1,"pma":false,"pixels":%q},
	"b":{"name":"B","enabled":1,"opacity":1,"pma":false,"pixels":%q},
	"c":{"name":"C","enabled":1,"opacity":1,"pma":false,"pixels":%q}},
	"order_links":[{"id":"a","above":"c"},{"id":"b","above":"a"},{"id":"c","above":"b"}]}`,
		pixel, pixel, pixel)
}

func corruptBase64Doc() string {
	return `{"format_version":"foreground-v1","resolution":{"w":1,"h":1,"channels":4},
	"frame_count":1,"frame_duration_us":1000,
	"layers":[{"id":"a","visible":true,"z_index":0,"opacity":1,"alpha_mode":"straight","rgba_base64":"!!bad!!"}]}`
}
