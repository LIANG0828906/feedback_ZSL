package dressbatch

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	a, err := GenerateSampleSuite(99, 3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSampleSuite(99, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		aData, _ := MarshalProject(a[i])
		bData, _ := MarshalProject(b[i])
		if !bytes.Equal(aData, bData) {
			t.Fatalf("project %d is not deterministic", i)
		}
	}
	c, err := GenerateSampleSuite(100, 3)
	if err != nil {
		t.Fatal(err)
	}
	aData, _ := MarshalProject(a[0])
	cData, _ := MarshalProject(c[0])
	if bytes.Equal(aData, cData) {
		t.Fatal("different seeds should produce different projects")
	}
}

func TestValidateSchemaErrorsIdentifyObjectAndField(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Project)
		field    string
		objectID string
	}{
		{
			name: "duplicate mount",
			mutate: func(project *Project) {
				project.Mounts = append(project.Mounts, project.Mounts[0])
			},
			field:    "id",
			objectID: "mount:body",
		},
		{
			name: "missing mount reference",
			mutate: func(project *Project) {
				project.Parts[0].MountID = "missing"
			},
			field:    "mount_id",
			objectID: "part:body",
		},
		{
			name: "submesh outside geometry",
			mutate: func(project *Project) {
				project.Parts[0].Submeshes[0].VertexCount = project.Parts[0].VertexCount + 1
			},
			field:    "submeshes[0].vertex_start",
			objectID: "part:body",
		},
		{
			name: "submesh overlap",
			mutate: func(project *Project) {
				for i := range project.Parts {
					if len(project.Parts[i].Submeshes) > 1 {
						project.Parts[i].Submeshes[1].VertexStart = project.Parts[i].Submeshes[0].VertexStart
						return
					}
				}
			},
			field: "submeshes[1].vertex_start",
		},
		{
			name: "exclusion missing part",
			mutate: func(project *Project) {
				project.MutualExclusions[0].B = "missing"
			},
			field:    "b",
			objectID: "exclusion:ex_armor_pack",
		},
		{
			name: "missing truth",
			mutate: func(project *Project) {
				project.Truth.Assemblies = nil
			},
			field:    "assemblies",
			objectID: "truth",
		},
		{
			name: "missing part id",
			mutate: func(project *Project) {
				project.Parts[0].ID = ""
			},
			field:    "parts[0].id",
			objectID: "part:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := cloneProject(t, testProject(t))
			test.mutate(project)
			err := ValidateProject(project)
			if err == nil || ErrorCode(err) != CodeSchema {
				t.Fatalf("expected E_SCHEMA, got %v", err)
			}
			domainErr, _ := AsDomainError(err)
			if domainErr.Field != test.field {
				t.Fatalf("field expected %q got %q (%v)", test.field, domainErr.Field, err)
			}
			if test.objectID != "" && domainErr.ObjectID != test.objectID {
				t.Fatalf("object id expected %q got %q", test.objectID, domainErr.ObjectID)
			}
		})
	}
}

func TestLoadProjectRejectsUnknownAndMissingFields(t *testing.T) {
	project := testProject(t)
	data, err := MarshalProject(project)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(data, []byte(`"schema_version": "1"`), []byte(`"schema_version": "1", "unknown_field": true`), 1)
	if _, err := LoadProject(bytes.NewReader(unknown)); err == nil || ErrorCode(err) != CodeSchema {
		t.Fatalf("expected unknown field E_SCHEMA, got %v", err)
	}
	missing := bytes.Replace(data, []byte(`"schema_version": "1"`), []byte(`"schema_version": ""`), 1)
	if _, err := LoadProject(bytes.NewReader(missing)); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected missing field E_SCHEMA, got %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	constraints := document["constraints"].(map[string]any)
	delete(constraints, "allow_cross_skin")
	missingBool, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(bytes.NewReader(missingBool))
	if err == nil || ErrorCode(err) != CodeSchema {
		t.Fatalf("expected missing bool E_SCHEMA, got %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Field != "constraints.allow_cross_skin" {
		t.Fatalf("unexpected field: %s", domainErr.Field)
	}
}
