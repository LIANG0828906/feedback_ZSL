package slikernel

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestThreeSchemaParity(t *testing.T) {
	data := fixture(t)
	canonical := CloneProject(data.Project)
	canonical.SourceVersion = 0
	encoded := make([][]byte, 4)
	loaded := make([]Project, 4)
	for version := 1; version <= 3; version++ {
		payload, err := EncodeProject(data.Project, version)
		if err != nil {
			t.Fatalf("EncodeProject v%d: %v", version, err)
		}
		encoded[version] = payload
		project, err := LoadProject(payload)
		if err != nil {
			t.Fatalf("LoadProject v%d: %v", version, err)
		}
		loaded[version] = project
	}
	for version := 1; version <= 3; version++ {
		project := CloneProject(loaded[version])
		project.SourceVersion = 0
		if !reflect.DeepEqual(canonical, project) {
			t.Fatalf("canonical project mismatch for schema v%d", version)
		}
		serviceNames := make([]string, 0, len(project.Services))
		for _, service := range project.Services {
			serviceNames = append(serviceNames, service.Name)
		}
		if len(serviceNames) != len(data.Project.Services) {
			t.Fatalf("service count mismatch in v%d", version)
		}
		for serviceIndex := range project.Services {
			if len(project.Services[serviceIndex].SLIs) != len(data.Project.Services[serviceIndex].SLIs) {
				t.Fatalf("SLI count mismatch in v%d", version)
			}
			for sliIndex := range project.Services[serviceIndex].SLIs {
				if project.Services[serviceIndex].SLIs[sliIndex].ID != data.Project.Services[serviceIndex].SLIs[sliIndex].ID {
					t.Fatalf("SLI order mismatch in v%d service %d", version, serviceIndex)
				}
			}
		}
	}
}

func TestTargetBasisPointsAndPercentEquivalence(t *testing.T) {
	data := fixture(t)
	baseReport := compute(t, data.Project, testOptions())
	v2, err := EncodeProject(data.Project, 2)
	if err != nil {
		t.Fatalf("encode v2: %v", err)
	}
	percentJSON := mutateJSON(t, v2, func(document map[string]any) {
		for _, rawService := range serviceMap(t, document) {
			service := rawService.(map[string]any)
			for _, rawSLI := range sliMaps(t, service, "sli_defs") {
				sli := rawSLI.(map[string]any)
				bps := sli["target_bps"].(float64)
				delete(sli, "target_bps")
				sli["target_percent"] = bps / 100
			}
		}
	})
	percentProject, err := LoadProject(percentJSON)
	if err != nil {
		t.Fatalf("load percent targets: %v", err)
	}
	for serviceIndex := range data.Project.Services {
		for sliIndex := range data.Project.Services[serviceIndex].SLIs {
			if data.Project.Services[serviceIndex].SLIs[sliIndex].TargetPPM !=
				percentProject.Services[serviceIndex].SLIs[sliIndex].TargetPPM {
				t.Fatalf("target conversion mismatch at service %d sli %d", serviceIndex, sliIndex)
			}
		}
	}
	percentReport := compute(t, percentProject, testOptions())
	assertReportCoreEqual(t, baseReport, percentReport)
}

func TestOptionalExhaustedMarkerIsIgnored(t *testing.T) {
	data := fixture(t)
	v3, err := EncodeProject(data.Project, 3)
	if err != nil {
		t.Fatalf("encode v3: %v", err)
	}
	withLyingMarkers := mutateJSON(t, v3, func(document map[string]any) {
		for _, rawService := range serviceMap(t, document) {
			service := rawService.(map[string]any)
			for _, rawSLI := range sliMaps(t, service, "slis") {
				rawSLI.(map[string]any)["budget_exhausted"] = true
			}
		}
		for _, rawObservation := range document["observations"].([]any) {
			rawObservation.(map[string]any)["budget_exhausted"] = true
		}
	})
	markedProject, err := LoadProject(withLyingMarkers)
	if err != nil {
		t.Fatalf("load marked v3: %v", err)
	}
	truthReport := compute(t, data.Project, testOptions())
	markedReport := compute(t, markedProject, testOptions())
	assertReportCoreEqual(t, truthReport, markedReport)
	for _, result := range markedReport.Results {
		if result.Budget.Exhausted {
			if result.Budget.Failures < result.Budget.AllowedErrors {
				t.Fatalf("exhaustion was not derived from failures/allowed at %v", result.Key())
			}
		}
	}
}

func TestLoaderDoesNotMutateInputBytes(t *testing.T) {
	data := fixture(t)
	for version := 1; version <= 3; version++ {
		payload, err := EncodeProject(data.Project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		before := append([]byte(nil), payload...)
		if _, err := LoadProject(payload); err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if !reflect.DeepEqual(before, payload) {
			t.Fatalf("source bytes changed for v%d", version)
		}
	}
}

func TestSchemaRoundTripJSONShape(t *testing.T) {
	data := fixture(t)
	v2, err := EncodeProject(data.Project, 2)
	if err != nil {
		t.Fatalf("encode v2: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(v2, &document); err != nil {
		t.Fatalf("unmarshal v2: %v", err)
	}
	if document["schema"] != "2" {
		t.Fatalf("unexpected v2 schema marker: %v", document["schema"])
	}
}
