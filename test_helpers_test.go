package terrainbake

import (
	"encoding/json"
	"errors"
	"testing"
)

func testProject(t *testing.T) *Project {
	t.Helper()
	p := NewGenerator(DefaultSeed).Project()
	if err := p.ValidateTopology(); err != nil {
		t.Fatalf("generated project invalid: %v", err)
	}
	return p
}

func encodeTest(t *testing.T, p *Project, v SchemaVersion) []byte {
	t.Helper()
	data, err := EncodeProject(p, v)
	if err != nil {
		t.Fatalf("encode %s: %v", v, err)
	}
	return data
}

func loadTest(t *testing.T, data []byte) *Project {
	t.Helper()
	p, err := LoadProject(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p
}

func mutateJSON(t *testing.T, data []byte, fn func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	fn(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return out
}

func asTBError(t *testing.T, err error) *Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	return e
}

func mustCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	e := asTBError(t, err)
	if e.Code != code {
		t.Fatalf("code: got %s want %s (%v)", e.Code, code, e)
	}
	return e
}

func cloneMesh(m *Mesh) *Mesh {
	if m == nil {
		return nil
	}
	out := *m
	out.Vertices = append([]Vertex(nil), m.Vertices...)
	out.Triangles = append([]Triangle(nil), m.Triangles...)
	return &out
}

func cloneMeshes(in map[string]*Mesh) map[string]*Mesh {
	out := make(map[string]*Mesh, len(in))
	for k, v := range in {
		out[k] = cloneMesh(v)
	}
	return out
}

func findBlock(t *testing.T, p *Project, id string) BlockSpec {
	t.Helper()
	b, ok := p.BlockByID(id)
	if !ok {
		t.Fatalf("block %s missing", id)
	}
	return *b
}
