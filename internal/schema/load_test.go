package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
	"quotaflow/internal/schema"
)

func TestThreeSchemasLoadToSameModel(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := generator.Generate(42, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	projects := make([]*domain.Project, 0, 3)
	for _, version := range []string{"v1", "v2", "v3"} {
		project, err := schema.LoadFile(filepath.Join(dir, "project-"+version+".json"))
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		projects = append(projects, project)
	}
	want := projects[0].CanonicalFingerprint()
	for index, project := range projects[1:] {
		if got := project.CanonicalFingerprint(); got != want {
			t.Fatalf("version v%d fingerprint %s != v1 %s", index+2, got, want)
		}
		if !reflect.DeepEqual(project.Tenants, projects[0].Tenants) {
			t.Fatalf("tenant order/model differs for version v%d", index+2)
		}
		if !reflect.DeepEqual(project.Nodes, projects[0].Nodes) {
			t.Fatalf("node order/model differs for version v%d", index+2)
		}
		if !reflect.DeepEqual(project.Events, projects[0].Events) {
			t.Fatalf("event order/model differs for version v%d", index+2)
		}
	}
	node, ok := projects[0].NodeByID("n-a1")
	if !ok || node.CapacityMilli != 12500 {
		t.Fatalf("exact decimal-core conversion failed: %+v", node)
	}
	tenant, _ := projects[1].TenantByID("t-alpha")
	if tenant.QuotaBasisPoints != 1800 || tenant.ReserveBasisPoints != 2500 {
		t.Fatalf("basis-point conversion failed: %+v", tenant)
	}
}

func TestFlatAndRackTreeYieldSameMigrationAndPauseObservations(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := generator.Generate(42, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var results [][]domain.TickObservation
	for _, version := range []string{"v1", "v2", "v3"} {
		project, err := schema.LoadFile(filepath.Join(dir, "project-"+version+".json"))
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		eng, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 64})
		if err != nil {
			t.Fatalf("new %s: %v", version, err)
		}
		if err := eng.RunAll(); err != nil {
			t.Fatalf("run %s: %v", version, err)
		}
		results = append(results, eng.Observations())
	}
	for index := 1; index < len(results); index++ {
		if !reflect.DeepEqual(results[0], results[index]) {
			t.Fatalf("flat/grouped topology observations differ at schema index %d", index)
		}
	}
}

func TestGeneratorIsByteDeterministic(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if _, _, err := generator.Generate(77, first); err != nil {
		t.Fatalf("generate first: %v", err)
	}
	if _, _, err := generator.Generate(77, second); err != nil {
		t.Fatalf("generate second: %v", err)
	}
	for _, name := range generator.FileNames() {
		a, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatalf("read first %s: %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatalf("read second %s: %v", name, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("file %s differs across deterministic runs", name)
		}
	}
}

func TestLoaderStableBusinessCodes(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		mutate func(map[string]any)
		code   string
	}{
		{
			name: "mixed units",
			base: "v1",
			mutate: func(doc map[string]any) {
				doc["quotaBasisPoints"] = 100
			},
			code: domain.ESchemaMixedUnits,
		},
		{
			name: "mixed version keys",
			base: "v1",
			mutate: func(doc map[string]any) {
				doc["schema"] = "v2"
			},
			code: domain.ESchemaMixedUnits,
		},
		{
			name: "negative quota",
			base: "v1",
			mutate: func(doc map[string]any) {
				doc["tenants"].([]any)[0].(map[string]any)["quota_percent"] = -1
			},
			code: domain.ENegativeQuota,
		},
		{
			name: "reserve over quota",
			base: "v1",
			mutate: func(doc map[string]any) {
				doc["tenants"].([]any)[0].(map[string]any)["reserve_percent"] = 150
			},
			code: domain.EReserveExceedsQuota,
		},
		{
			name: "excess core precision",
			base: "v1",
			mutate: func(doc map[string]any) {
				doc["nodes"].([]any)[0].(map[string]any)["core_capacity"] = "12.5001"
			},
			code: domain.EFixedPointPrecision,
		},
		{
			name: "fault domain cycle",
			base: "v2",
			mutate: func(doc map[string]any) {
				domains := doc["faultDomains"].([]any)
				for _, raw := range domains {
					fd := raw.(map[string]any)
					switch fd["domainId"] {
					case "rack-a":
						fd["parentId"] = "rack-b"
					case "rack-b":
						fd["parentId"] = "rack-a"
					}
				}
			},
			code: domain.EFaultDomainCycle,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := generator.Generate(42, dir); err != nil {
				t.Fatalf("generate: %v", err)
			}
			path := filepath.Join(dir, "project-"+test.base+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			before := append([]byte(nil), raw...)
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("decode: %v", err)
			}
			test.mutate(doc)
			mutated, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := schema.LoadBytes(mutated, "injected"); err == nil {
				t.Fatalf("expected %s", test.code)
			} else if domain.CodeOf(err) != test.code {
				t.Fatalf("got %s (%v), want %s", domain.CodeOf(err), err, test.code)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("loader/injection modified the source project")
			}
		})
	}
}
