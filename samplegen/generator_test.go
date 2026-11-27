package samplegen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"proxydiag/domain"
	"proxydiag/engine"
	"proxydiag/samplegen"
	"proxydiag/schema"
)

func TestCanonicalProjectIsValid(t *testing.T) {
	p := samplegen.BuildCanonical(samplegen.DefaultSeed)
	if err := p.Validate(); err != nil {
		t.Fatalf("canonical project invalid: %v", err)
	}
	if len(p.Listeners) != 3 || len(p.Upstreams) != 7 || len(p.Requests) != 40 {
		t.Fatalf("unexpected shape: listeners=%d upstreams=%d requests=%d",
			len(p.Listeners), len(p.Upstreams), len(p.Requests))
	}
}

func TestTruthMatchesEngineForAllSchemaVersions(t *testing.T) {
	dir := t.TempDir()
	if _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	truth, err := samplegen.LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project_v1.json", "project_v2.json", "project_v3.json"} {
		t.Run(name, func(t *testing.T) {
			p, err := schema.LoadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			results := engine.EvaluateAll(p, truth)
			if len(results) != len(truth.Requests) {
				t.Fatalf("result count %d != truth %d", len(results), len(truth.Requests))
			}
			for _, r := range results {
				if !r.TruthMatched || !r.WithinTolerance {
					t.Fatalf("request %d mismatch=%v max_delta=%d terminal=%s err=%s",
						r.Seq, r.Mismatches, r.MaxAbsDelta, r.Terminal, r.ErrorCode)
				}
			}
		})
	}
}

func TestTruthFileRecordedShape(t *testing.T) {
	dir := t.TempDir()
	if _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var truth domain.Truth
	if err := jsonUnmarshal(b, &truth); err != nil {
		t.Fatal(err)
	}
	if truth.Seed != samplegen.DefaultSeed || truth.Version != samplegen.ProjectVer {
		t.Fatalf("unexpected truth metadata: %+v", truth)
	}
}

func TestGenerationIsDeterministic(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if _, err := samplegen.Write(a, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	if _, err := samplegen.Write(b, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project_v1.json", "project_v2.json", "project_v3.json", "truth.json", "manifest.json"} {
		left, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s differs across generation runs", name)
		}
	}
}

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
