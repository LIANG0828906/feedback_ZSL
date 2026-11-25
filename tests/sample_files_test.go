package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tlscheck/internal/engine"
	"tlscheck/internal/incremental"
	"tlscheck/internal/projectio"
	"tlscheck/internal/sample"
)

func TestGeneratedSampleFilesReload(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.WriteSampleDir(dir); err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, name := range []string{"project.v1.json", "project.v2.json", "project.v3.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		p, err := projectio.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		h, err := p.CanonicalHash()
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = h
		r, err := engine.Run(*p)
		if err != nil {
			t.Fatal(err)
		}
		truthData, err := os.ReadFile(filepath.Join(dir, "truth.json"))
		if err != nil {
			t.Fatal(err)
		}
		var truth engine.Report
		if err := json.Unmarshal(truthData, &truth); err != nil {
			t.Fatal(err)
		}
		equal, err := incremental.OutcomesEqual(r, &truth)
		if err != nil {
			t.Fatal(err)
		}
		if !equal {
			t.Fatalf("%s outcomes do not match truth.json", name)
		}
	}
	if hashes["project.v1.json"] != hashes["project.v2.json"] || hashes["project.v2.json"] != hashes["project.v3.json"] {
		t.Fatalf("sample file hashes differ: %v", hashes)
	}
}
