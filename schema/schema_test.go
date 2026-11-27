package schema_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"proxydiag/bizerr"
	"proxydiag/samplegen"
	"proxydiag/schema"
)

func writeSamples(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestThreeSchemasNormalizeEquivalently(t *testing.T) {
	dir := writeSamples(t)
	paths := []string{"project_v1.json", "project_v2.json", "project_v3.json"}
	fingerprints := map[string]string{}
	for _, name := range paths {
		p, err := schema.LoadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fingerprints[name] = p.CoreFingerprint()
		if len(p.Listeners) != 3 || len(p.Upstreams) != 7 || len(p.Requests) != 40 {
			t.Fatalf("%s unexpected shape", name)
		}
		if p.Listeners[0].Routes[0].ID != "R-api-v1-admin" ||
			p.Upstreams[0].ID != "u-blue" {
			t.Fatalf("%s order was not preserved", name)
		}
	}
	if fingerprints[paths[0]] != fingerprints[paths[1]] ||
		fingerprints[paths[1]] != fingerprints[paths[2]] {
		t.Fatalf("core fingerprints differ: %+v", fingerprints)
	}
}

func TestOptionalProbeNormalization(t *testing.T) {
	dir := writeSamples(t)
	v1, err := schema.LoadFile(filepath.Join(dir, "project_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := schema.LoadFile(filepath.Join(dir, "project_v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	v3, err := schema.LoadFile(filepath.Join(dir, "project_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if v1.Upstreams[0].Probe != nil {
		t.Fatal("v1 must not invent an unsupported health probe")
	}
	if v2.Upstreams[0].Probe == nil || v3.Upstreams[0].Probe == nil {
		t.Fatal("v2/v3 optional probe was not loaded")
	}
	if v2.Upstreams[0].Probe.IntervalMS != v3.Upstreams[0].Probe.IntervalMS {
		t.Fatal("probe tick/ms normalization differs")
	}
}

func TestStableSchemaErrors(t *testing.T) {
	dir := writeSamples(t)
	base, err := os.ReadFile(filepath.Join(dir, "project_v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(base, &doc); err != nil {
		t.Fatal(err)
	}
	doc["timeout_ticks"] = 2
	badCombo, _ := json.Marshal(doc)
	_, err = schema.Load(badCombo)
	assertCode(t, err, bizerr.SchemaFieldCombo)

	unknown, _ := json.Marshal(map[string]any{"schema": "proxy.project/v9"})
	_, err = schema.Load(unknown)
	assertCode(t, err, bizerr.SchemaUnknown)
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if got := bizerr.Code(err); got != code {
		t.Fatalf("expected %s, got %s (%v)", code, got, err)
	}
}
