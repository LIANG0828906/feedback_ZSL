package plancache

import (
	"encoding/json"
	"strings"
	"testing"
)

func loadedVersions(t *testing.T) map[string]*Project {
	t.Helper()
	truth := GenerateProject(DefaultSeed)
	out := map[string]*Project{}
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, err := Render(truth, version)
		if err != nil {
			t.Fatalf("render %s: %v", version, err)
		}
		p, err := Load(data)
		if err != nil {
			t.Fatalf("load %s: %v\n%s", version, err, data)
		}
		out[version] = p
	}
	return out
}

func TestThreeSchemaVersionsLoadEquivalent(t *testing.T) {
	truth := GenerateProject(DefaultSeed)
	loaded := loadedVersions(t)

	var first *Project
	for version, p := range loaded {
		if !CanonicalEqualForLoad(truth, p) {
			t.Fatalf("%s canonical project differs from generator truth", version)
		}
		if first == nil {
			first = p
			continue
		}
		if !CanonicalEqualForLoad(first, p) {
			t.Fatalf("schema versions disagree: first=%s current=%s", first.SchemaVersion, version)
		}
	}

	truthKeys, err := DeriveAllKeys(truth, nil)
	if err != nil {
		t.Fatal(err)
	}
	for version, p := range loaded {
		keys, err := DeriveAllKeys(p, nil)
		if err != nil {
			t.Fatalf("%s keys: %v", version, err)
		}
		if strings.Join(KeySet(keys), ",") != strings.Join(KeySet(truthKeys), ",") {
			t.Fatalf("%s key set mismatch", version)
		}
		for i := range truth.Statements {
			wantOrder := truth.Statements[i].TraversalOrder()
			gotOrder := p.Statements[i].TraversalOrder()
			if strings.Join(wantOrder, ",") != strings.Join(gotOrder, ",") {
				t.Fatalf("%s traversal mismatch for %s: %v vs %v", version, truth.Statements[i].ID, gotOrder, wantOrder)
			}
		}
	}
}

func TestV1GlobalTemplateAndArgumentsAreNormalized(t *testing.T) {
	truth := GenerateProject(DefaultSeed)
	v1Bytes, err := Render(truth, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(v1Bytes, &raw); err != nil {
		t.Fatal(err)
	}
	firstStmt := raw["statements"].([]any)[0].(map[string]any)
	if got := firstStmt["template_text"].(string); !strings.Contains(got, "$1") {
		t.Fatalf("first statement should start global numbering at base+1, got %q", got)
	}
	secondStmt := raw["statements"].([]any)[1].(map[string]any)
	if got := secondStmt["template_text"].(string); !strings.Contains(got, "$3") {
		t.Fatalf("second statement should use global placeholder $3 after two params, got %q", got)
	}
	p, err := Load(v1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Statements[1].Template; !strings.Contains(got, "$1") {
		t.Fatalf("global template was not normalized to per-statement numbering: %q", got)
	}
}

func mutateDocument(t *testing.T, version string, fn func(map[string]any)) []byte {
	t.Helper()
	truth := GenerateProject(DefaultSeed)
	data, err := Render(truth, version)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	fn(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSchemaExceptionCodes(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		wantCode string
	}{
		{
			name:     "unknown version",
			data:     []byte(`{"schema_id":"plan-project/v9"}`),
			wantCode: CodeSchemaUnknownVersion,
		},
		{
			name:     "conflicting schema identifiers",
			data:     []byte(`{"schema_id":"plan-project/v1","schemaId":"plan-project/v2"}`),
			wantCode: CodeSchemaFieldConflict,
		},
		{
			name: "v1 flat and tree conflict",
			data: mutateDocument(t, SchemaV1, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				st["subplan_trees"] = []any{}
			}),
			wantCode: CodeSchemaFieldConflict,
		},
		{
			name: "node argument numbering out of range",
			data: mutateDocument(t, SchemaV2, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				tree := st["subplanTrees"].([]any)[0].(map[string]any)
				root := tree["root"].(map[string]any)
				filter := root["children"].([]any)[0].(map[string]any)
				filter["relativeArgs"] = []any{0, 99}
			}),
			wantCode: CodeParamIndexOutOfRange,
		},
		{
			name: "malformed missing tree root",
			data: mutateDocument(t, SchemaV2, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				tree := st["subplanTrees"].([]any)[0].(map[string]any)
				delete(tree, "root")
			}),
			wantCode: CodeNodeTreeMalformed,
		},
		{
			name: "v1 missing child flat node",
			data: mutateDocument(t, SchemaV1, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				nodes := st["flat_nodes"].([]any)
				project := nodes[0].(map[string]any)
				project["child_node_ids"] = []any{"S1-missing"}
			}),
			wantCode: CodeNodeTreeMalformed,
		},
		{
			name: "v1 tick and millisecond fields conflict",
			data: mutateDocument(t, SchemaV1, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				node := st["flat_nodes"].([]any)[1].(map[string]any)
				node["estimated_cost_ms"] = 2
			}),
			wantCode: CodeSchemaFieldConflict,
		},
		{
			name: "v1 absolute and relative arguments conflict",
			data: mutateDocument(t, SchemaV1, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				node := st["flat_nodes"].([]any)[1].(map[string]any)
				node["relative_arguments"] = []any{0}
			}),
			wantCode: CodeSchemaFieldConflict,
		},
		{
			name: "v2 kib and byte capacity conflict",
			data: mutateDocument(t, SchemaV2, func(doc map[string]any) {
				cfg := doc["cacheConfig"].(map[string]any)
				cfg["maxCacheBytes"] = 16384
			}),
			wantCode: CodeSchemaFieldConflict,
		},
		{
			name: "fingerprint conflict",
			data: mutateDocument(t, SchemaV1, func(doc map[string]any) {
				st := doc["statements"].([]any)[0].(map[string]any)
				st["plan_fingerprint"] = "fp_deadbeef"
			}),
			wantCode: CodeFingerprintConflict,
		},
		{
			name: "zero byte capacity",
			data: mutateDocument(t, SchemaV2, func(doc map[string]any) {
				cfg := doc["cacheConfig"].(map[string]any)
				cfg["maxCacheKiB"] = 0
			}),
			wantCode: CodeCapacityBytesZero,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.data)
			assertCode(t, err, tc.wantCode)
		})
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	be, ok := AsBusinessError(err)
	if !ok {
		t.Fatalf("expected business error %s, got %v", code, err)
	}
	if be.Code != code {
		t.Fatalf("expected code %s, got %s (%v) context=%v", code, be.Code, err, be.Context)
	}
	if be.Path == "" {
		t.Fatalf("error %s must include locator path", code)
	}
}
