package service

import (
	"context"
	"encoding/json"
	"testing"

	"volumelab/pkg/model"
)

func TestServiceOpenInvokePrecisionAndRecovery(t *testing.T) {
	dir := t.TempDir()
	svc := New(Options{MaxParallelism: 3, MaxBufferPixels: 53})
	result := svc.Invoke(context.Background(), "generate", mustJSON(t, map[string]any{
		"outputDir":    dir,
		"seed":         42,
		"manifestName": "manifest.json",
	}))
	if !result.OK {
		t.Fatalf("generate failed: %+v", result.Error)
	}
	opened := svc.Invoke(context.Background(), "open", mustJSON(t, map[string]any{
		"manifestPath": dir + "/manifest.json",
		"volumeId":     "vol_a",
	}))
	if !opened.OK {
		t.Fatalf("open failed: %+v", opened.Error)
	}
	view := opened.Data.(SessionView)
	if view.State.Sequence != 1 || len(view.Planes.Images) != 3 {
		t.Fatalf("unexpected initial view: %+v", view)
	}
	added := svc.Invoke(context.Background(), "auto_annotation", mustJSON(t, map[string]any{
		"structureId": "sphere_a",
		"mode":        "sphere_diameter",
	}))
	if !added.OK {
		t.Fatalf("annotation failed: %+v", added.Error)
	}
	addedView := added.Data.(SessionView)
	annotation := addedView.State.Annotations[0]
	if annotation.WorldLengthMM != 16 || annotation.VoxelLength != 16 {
		t.Fatalf("unexpected dual readings: %+v", annotation)
	}
	precision := svc.Invoke(context.Background(), "precision", mustJSON(t, map[string]any{
		"annotationId": annotation.ID,
		"structureId":  "sphere_a",
		"mode":         "sphere_diameter",
		"toleranceMM":  2.5,
	}))
	if !precision.OK {
		t.Fatalf("precision failed: %+v", precision.Error)
	}
	report := precision.Data.(*model.PrecisionReport)
	if !report.Pass || report.DeviationMM != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	paused := svc.Invoke(context.Background(), "pause", "{}")
	if !paused.OK || paused.Data.(map[string]any)["sequence"].(uint64) != addedView.State.Sequence {
		t.Fatalf("pause did not report the committed sequence: %+v", paused)
	}
	badWindow := svc.Invoke(context.Background(), "set_window", `{"width":0,"level":100}`)
	if badWindow.OK || badWindow.Error == nil || badWindow.Error.Code != "E_WIN" {
		t.Fatalf("expected E_WIN, got %+v", badWindow)
	}
	svc.Invoke(context.Background(), "close", "{}")

	reopened := svc.Invoke(context.Background(), "open", mustJSON(t, map[string]any{
		"manifestPath": dir + "/manifest.json",
		"volumeId":     "vol_a",
	}))
	if !reopened.OK {
		t.Fatalf("reopen failed: %+v", reopened.Error)
	}
	reopenedView := reopened.Data.(SessionView)
	if !reopenedView.Recovered || len(reopenedView.State.Annotations) != 1 {
		t.Fatalf("session was not recovered: %+v", reopenedView)
	}
	if reopenedView.State.Annotations[0] != annotation {
		t.Fatalf("annotation values changed after recovery")
	}
	for i := range view.Planes.Images {
		if reopenedView.Planes.Images[i].PixelHash != view.Planes.Images[i].PixelHash {
			t.Fatalf("initial and recovered image hash differ for %s", view.Planes.Images[i].Plane)
		}
	}
}

func TestServiceSerialAndParallelCommitChecksumsMatch(t *testing.T) {
	run := func(workers int) SessionView {
		dir := t.TempDir()
		svc := New(Options{MaxParallelism: workers, MaxBufferPixels: 31})
		generated := svc.Invoke(context.Background(), "generate", mustJSON(t, map[string]any{"outputDir": dir, "seed": 42}))
		if !generated.OK {
			t.Fatal(generated.Error)
		}
		opened := svc.Invoke(context.Background(), "open", mustJSON(t, map[string]any{
			"manifestPath":    dir + "/manifest.json",
			"volumeId":        "vol_a",
			"sessionDir":      dir + "/sessions",
			"maxParallelism":  workers,
			"maxBufferPixels": 31,
		}))
		if !opened.OK {
			t.Fatal(opened.Error)
		}
		actions := []struct {
			action  string
			payload string
		}{
			{"set_crosshair", `{"x":20,"y":30,"z":27}`},
			{"set_window", `{"width":800,"level":200}`},
			{"set_thickness", `{"plane":"axial","thicknessMM":4.5}`},
			{"auto_annotation", `{"structureId":"tube_a","mode":"tube_axis"}`},
		}
		var view SessionView
		for _, action := range actions {
			result := svc.Invoke(context.Background(), action.action, action.payload)
			if !result.OK {
				t.Fatalf("%s failed: %+v", action.action, result.Error)
			}
			view = result.Data.(SessionView)
		}
		return view
	}
	serial := run(1)
	parallel := run(3)
	if serial.Snapshot.Checksum != parallel.Snapshot.Checksum {
		t.Fatalf("commit checksum differs: %s != %s", serial.Snapshot.Checksum, parallel.Snapshot.Checksum)
	}
	if serial.State.Sequence != parallel.State.Sequence {
		t.Fatalf("sequence differs: %d != %d", serial.State.Sequence, parallel.State.Sequence)
	}
	for i := range serial.Planes.Images {
		if serial.Planes.Images[i].PixelHash != parallel.Planes.Images[i].PixelHash {
			t.Fatalf("%s image differs", serial.Planes.Images[i].Plane)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
