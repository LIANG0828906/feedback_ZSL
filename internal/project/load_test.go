package project_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"credrot/internal/bizerr"
	"credrot/internal/generator"
	"credrot/internal/model"
	"credrot/internal/project"
)

func TestThreeSchemaVersionsConverge(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 20261126} {
		t.Run(fmt.Sprintf("seed-%s", jsonNumber(seed)), func(t *testing.T) {
			bundle, err := generator.Generate(seed)
			if err != nil {
				t.Fatal(err)
			}
			loaded := make(map[string]*model.Project)
			for _, name := range []string{"project_v1.json", "project_v2.json", "project_v3.json"} {
				value, err := project.Load(bundle.Files[name])
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				loaded[name] = value.Project
			}
			if !reflect.DeepEqual(loaded["project_v1.json"], loaded["project_v2.json"]) {
				t.Fatal("v1 and v2 did not converge")
			}
			if !reflect.DeepEqual(loaded["project_v2.json"], loaded["project_v3.json"]) {
				t.Fatal("v2 and v3 did not converge")
			}
			if !reflect.DeepEqual(loaded["project_v3.json"], bundle.Project) {
				t.Fatal("loaded canonical project differs from generator project")
			}
			if loaded["project_v2.json"].LastRotationWatermarkMillis != bundle.Project.LastRotationWatermarkMillis {
				t.Fatal("optional watermark inference did not converge")
			}
		})
	}
}

func TestAmbiguousFieldCombinationsReturnStableCode(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "time expressed as tick and millis",
			mutate: func(root map[string]any) {
				first(root["credentials"].([]any))["issued_tick"] = float64(1)
			},
		},
		{
			name: "ttl expressed as milliseconds and seconds",
			mutate: func(root map[string]any) {
				first(root["credentials"].([]any))["ttl_sec"] = float64(10)
			},
		},
		{
			name: "conflicting schema identifiers",
			mutate: func(root map[string]any) {
				root["schema_version"] = float64(2)
			},
		},
		{
			name: "v3 logical tick issuance",
			mutate: func(root map[string]any) {
				credential := first(root["credentials"].([]any))
				delete(credential, "issued_at_ms")
				credential["issued_tick"] = float64(1)
			},
		},
		{
			name: "v3 second ttl",
			mutate: func(root map[string]any) {
				credential := first(root["credentials"].([]any))
				delete(credential, "ttl_ms")
				credential["ttl_sec"] = float64(10)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(bundle.Files["project_v3.json"], &root); err != nil {
				t.Fatal(err)
			}
			tc.mutate(root)
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			_, err = project.Load(raw)
			if !bizerr.IsCode(err, bizerr.CodeSchemaAmbiguous) {
				t.Fatalf("got %v, want %s", err, bizerr.CodeSchemaAmbiguous)
			}
		})
	}
}

func TestV2LegacyVersionBatchSwap(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := project.Load(bundle.Files["project_v2.json"])
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range loaded.Project.Credentials {
		canonical := bundle.Project.CredentialByID()[credential.ID]
		if credential.Version != canonical.Version || credential.ReloadBatch != canonical.ReloadBatch {
			t.Fatalf("credential %s swapped fields normalized incorrectly: %+v vs %+v", credential.ID, credential, canonical)
		}
	}
}

func first(values []any) map[string]any {
	return values[0].(map[string]any)
}

func jsonNumber(v int64) string {
	data, _ := json.Marshal(v)
	return string(data)
}
