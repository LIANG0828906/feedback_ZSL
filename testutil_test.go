package dressbatch

import (
	"encoding/json"
	"testing"
)

func testProject(t *testing.T) *Project {
	t.Helper()
	project, err := GenerateProject(42, 0)
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	return project
}

func testLegalCandidate(t *testing.T, project *Project) []string {
	t.Helper()
	for _, sample := range project.Truth.Assemblies {
		if sample.ExpectedLegal {
			return append([]string(nil), sample.CandidatePartIDs...)
		}
	}
	t.Fatal("project has no legal candidate")
	return nil
}

func testPack(t *testing.T, project *Project) (*PackResult, []string) {
	t.Helper()
	candidate := testLegalCandidate(t, project)
	result, err := Pack(project, candidate)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return result, candidate
}

func cloneProject(t *testing.T, project *Project) *Project {
	t.Helper()
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var clone Project
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func clonePackResult(t *testing.T, result *PackResult) *PackResult {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var clone PackResult
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func mutateLastBatchLayout(t *testing.T, project *Project, result *PackResult) (*Project, string) {
	t.Helper()
	last := result.Batches[len(result.Batches)-1]
	target := last.PartIDs[len(last.PartIDs)-1]
	clone := cloneProject(t, project)
	for i := range clone.Parts {
		if clone.Parts[i].ID != target {
			continue
		}
		for j := range clone.Parts[i].Submeshes {
			if clone.Parts[i].Submeshes[j].VertexCount > 2 {
				clone.Parts[i].Submeshes[j].VertexCount--
				if clone.Parts[i].Submeshes[j].IndexCount > 3 {
					clone.Parts[i].Submeshes[j].IndexCount -= 3
				}
				return clone, target
			}
		}
	}
	t.Fatalf("could not mutate layout for %s", target)
	return nil, ""
}
