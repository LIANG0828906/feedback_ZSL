package acoustic_test

import (
	"encoding/json"
	"errors"
	"testing"

	acoustic "levelacoustic"
	"levelacoustic/internal/samplegen"
)

func sampleProject(t *testing.T) *acoustic.Project {
	t.Helper()
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func cloneProject(t *testing.T, project *acoustic.Project) *acoustic.Project {
	t.Helper()
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var clone acoustic.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func domainError(t *testing.T, err error) *acoustic.Error {
	t.Helper()
	var domain *acoustic.Error
	if !errors.As(err, &domain) {
		t.Fatalf("expected domain error, got %v", err)
	}
	return domain
}

func TestLoadSampleRoundTrip(t *testing.T) {
	project := sampleProject(t)
	data, err := samplegen.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := acoustic.LoadProject(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExampleFileMatchesGeneratedSample(t *testing.T) {
	project, err := acoustic.LoadProjectFile("examples/sample_seed42.json")
	if err != nil {
		t.Fatal(err)
	}
	solutions := solveAllListeners(t, project)
	comparison := acoustic.CompareTruth(project, solutions)
	if !comparison.Passed {
		t.Fatalf("example file truth mismatch: %+v", comparison.Mismatches)
	}
	if comparison.ExpectedPaths != 8 || comparison.ActualPaths != 8 {
		t.Fatalf("unexpected example path totals: %+v", comparison)
	}
}

func TestMissingFieldReturnsSchemaError(t *testing.T) {
	project := sampleProject(t)
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "tolerance")
	modified, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acoustic.LoadProject(modified)
	if err == nil {
		t.Fatal("expected schema error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeSchema || domain.Field != "$.tolerance" {
		t.Fatalf("unexpected schema error: %v", domain)
	}
}

func TestMissingNestedPointFieldReturnsSchemaError(t *testing.T) {
	project := sampleProject(t)
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var listeners []map[string]json.RawMessage
	if err := json.Unmarshal(fields["listeners"], &listeners); err != nil {
		t.Fatal(err)
	}
	delete(listeners[0], "pos")
	fields["listeners"], _ = json.Marshal(listeners)
	modified, _ := json.Marshal(fields)
	_, err = acoustic.LoadProject(modified)
	if err == nil {
		t.Fatal("expected schema error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %v", domain)
	}
}

func TestOverlappingRoomsReturnGeometryError(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Rooms[1].Rect.MinX = 3500
	err := project.Validate()
	if err == nil {
		t.Fatal("expected geometry error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeGeometry || len(domain.RoomIDs) < 2 {
		t.Fatalf("unexpected geometry error: %v", domain)
	}
}

func TestRoomsTouchingOnlyAtCornerReturnGeometryError(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Rooms[1].Rect.MinX = 4000
	project.Rooms[1].Rect.MinY = 3000
	project.Rooms[1].Rect.MaxY = 6000
	err := project.Validate()
	if err == nil {
		t.Fatal("expected corner-touch geometry error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeGeometry || len(domain.RoomIDs) < 2 {
		t.Fatalf("unexpected corner-touch error: %v", domain)
	}
}

func TestOpeningOutsideWallReturnsGeometryError(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Openings[0].Width = 5000
	err := project.Validate()
	if err == nil {
		t.Fatal("expected opening geometry error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeGeometry || domain.WallIDs[0] != 101 {
		t.Fatalf("unexpected opening error: %v", domain)
	}
	if len(domain.RoomIDs) != 2 || len(domain.OpeningIDs) != 1 {
		t.Fatalf("expected room and wall context, got %v", domain)
	}
}

func TestNonUnitNormalReturnsGeometryError(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Reflectors[0].NX = 900
	err := project.Validate()
	if err == nil {
		t.Fatal("expected normal geometry error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeGeometry || domain.ReflectorIDs[0] != 31 {
		t.Fatalf("unexpected normal error: %v", domain)
	}
}

func TestListenerOnSharedBoundaryReturnsGeometryError(t *testing.T) {
	project := cloneProject(t, sampleProject(t))
	project.Listeners[0].Pos = acoustic.Point{X: 4000, Y: 1500}
	err := project.Validate()
	if err == nil {
		t.Fatal("expected listener membership error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeGeometry || domain.ListenerIDs[0] != 21 {
		t.Fatalf("unexpected listener error: %v", domain)
	}
}
