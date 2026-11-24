package ui

import (
	"path/filepath"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/generator"
)

func TestTimelineCreatesCanvasObjects(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	result, err := engine.EvaluateProject(project, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewTimeline(func(string, string, string, int, float64, float64) error { return nil }, func(string) {})
	timeline.Resize(fyne.NewSize(900, 420))
	timeline.SetData(TimelineData{
		Project:       project,
		Result:        result,
		SegmentID:     project.Segments[0].ID,
		ChannelID:     "velocity.main",
		LayerID:       "base",
		PlayheadBeat:  1,
		Interpolation: domain.InterpolationLinear,
	})
	renderer := test.WidgetRenderer(timeline)
	if renderer == nil || len(renderer.Objects()) == 0 {
		t.Fatal("timeline renderer produced no canvas objects")
	}
	renderer.Layout(fyne.NewSize(900, 420))
	renderer.Refresh()
}

func TestWorkspaceBuildsWithHeadlessFyneDriver(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	workspace, err := NewWorkspace(a, project, domain.DefaultParameters(), filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if workspace.Window() == nil || workspace.Window().Content() == nil {
		t.Fatal("workspace window was not built")
	}
}
