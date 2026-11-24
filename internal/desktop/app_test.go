//go:build fyne

package desktop

import (
	"context"
	"path/filepath"
	"testing"

	"fyne.io/fyne/v2/test"

	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/model"
)

type testPlayer struct{}

func (testPlayer) Play(string) error { return nil }
func (testPlayer) Pause() error      { return nil }

func TestHeadlessPanelBuildAndStep(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w := &workbench{app: a, window: a.NewWindow("test"), player: testPlayer{}}
	w.build()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "samples.json")
	manifest := generator.Generate(generator.Options{Seed: 77, Samples: 1, Prefix: "ui"})
	if err := generator.WriteFile(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	w.manifestEntry.SetText(manifestPath)
	w.dbEntry.SetText(filepath.Join(dir, "ui.sqlite"))
	w.loadManifest(manifestPath)
	if w.sess == nil {
		t.Fatal("session was not opened")
	}
	if len(w.snapshots) != model.DefaultParams(w.sample).TotalFrames(len(w.sample.Dry.Samples)) {
		t.Fatalf("unexpected frame count %d", len(w.snapshots))
	}
	if _, err := w.sess.ProcessAndCommitNext(testContext(t)); err != nil {
		t.Fatal(err)
	}
	w.refresh()
	if w.sess.Progress().Committed != 1 {
		t.Fatalf("committed=%d", w.sess.Progress().Committed)
	}
	if w.table == nil || w.direction == nil || w.energyPlot == nil || w.scopePlot == nil {
		t.Fatal("diagnostic panel objects are missing")
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
