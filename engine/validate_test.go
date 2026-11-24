package engine

import (
	"testing"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/generator"
)

func sampleProject(t *testing.T) domain.Project {
	t.Helper()
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	return domain.CloneProject(project)
}

func TestValidationErrorCodesAndIdentifiers(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		project := sampleProject(t)
		project.Segments[0].Channels[0].Layers[0].ID = ""
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		appErr, ok := domain.AsAppError(err)
		if !ok || appErr.Code != domain.CodeSchema {
			t.Fatalf("expected E_SCHEMA, got %v", err)
		}
		if appErr.SegmentID != project.Segments[0].ID || appErr.ChannelID != project.Segments[0].Channels[0].ID {
			t.Fatalf("missing segment/channel identifiers: %+v", appErr)
		}
	})
	t.Run("duplicate layer", func(t *testing.T) {
		project := sampleProject(t)
		ch := &project.Segments[0].Channels[0]
		ch.Layers[1].ID = ch.Layers[0].ID
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		appErr, _ := domain.AsAppError(err)
		if appErr == nil || appErr.Code != domain.CodeSchema || appErr.LayerID != ch.Layers[0].ID {
			t.Fatalf("expected duplicate layer E_SCHEMA with layer id, got %v", err)
		}
	})
	t.Run("position", func(t *testing.T) {
		project := sampleProject(t)
		seg := &project.Segments[0]
		ch := &seg.Channels[0]
		ch.Layers[0].Points[0].Beat = seg.EndBeat + 1
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		appErr, _ := domain.AsAppError(err)
		if appErr == nil || appErr.Code != domain.CodePosition || appErr.LayerID != ch.Layers[0].ID {
			t.Fatalf("expected E_POS with layer id, got %v", err)
		}
	})
	t.Run("monotonic", func(t *testing.T) {
		project := sampleProject(t)
		seg := &project.Segments[0]
		points := seg.Channels[0].Layers[0].Points
		points[1].Beat = points[0].Beat
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		if domain.CodeOf(err) != domain.CodePosition {
			t.Fatalf("expected E_POS, got %v", err)
		}
	})
	t.Run("priority coverage", func(t *testing.T) {
		project := sampleProject(t)
		project.Segments[0].Priorities = nil
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		appErr, _ := domain.AsAppError(err)
		if appErr == nil || appErr.Code != domain.CodePriority || appErr.SegmentID != project.Segments[0].ID {
			t.Fatalf("expected E_PRIO with segment id, got %v", err)
		}
		if len(appErr.Channels) == 0 || appErr.ChannelID == "" {
			t.Fatalf("E_PRIO did not identify missing channels: %+v", appErr)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		project := sampleProject(t)
		seg := &project.Segments[0]
		rule := seg.Priorities[0]
		seg.Priorities = append(seg.Priorities, domain.PriorityRule{Higher: rule.Lower, Lower: rule.Higher, Basis: "test-cycle"})
		err := ValidateProject(project, ValidateOptions{RequireTruth: true})
		appErr, _ := domain.AsAppError(err)
		if appErr == nil || appErr.Code != domain.CodeCycle {
			t.Fatalf("expected E_CYCLE, got %v", err)
		}
		if len(appErr.Channels) != 2 {
			t.Fatalf("expected cycle channels, got %+v", appErr.Channels)
		}
	})
}

func TestInterpolationModes(t *testing.T) {
	points := []domain.ControlPoint{{Beat: 0, Value: 0}, {Beat: 1, Value: 1}}
	linear, err := Interpolate(points, 0.5, domain.InterpolationLinear)
	if err != nil || linear != 0.5 {
		t.Fatalf("linear: %v %v", linear, err)
	}
	hold, err := Interpolate(points, 0.9, domain.InterpolationHold)
	if err != nil || hold != 0 {
		t.Fatalf("hold: %v %v", hold, err)
	}
	smooth, err := Interpolate(points, 0.25, domain.InterpolationSmoothstep)
	if err != nil || smooth != 0.15625 {
		t.Fatalf("smoothstep: %v %v", smooth, err)
	}
}
