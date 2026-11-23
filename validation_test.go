package terrainbake

import (
	"encoding/json"
	"testing"
)

func TestGeneratorDeterministic(t *testing.T) {
	a, err := GenerateProject(99)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateProject(99)
	if err != nil {
		t.Fatal(err)
	}
	if ProjectHash(a) != ProjectHash(b) {
		t.Fatal("same seed produced different projects")
	}
}

func TestSchemaMissingFieldObjectAndField(t *testing.T) {
	p, _ := GenerateProject(3)
	data := mustJSON(t, p)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	delete(top, "truth")
	_, err := DecodeProject(mustJSON(t, top))
	assertBiz(t, err, CodeSchema, "project", "truth")
}

func TestSchemaHeightSizeMismatch(t *testing.T) {
	p, _ := GenerateProject(4)
	data := mutateBlockArray(t, p, func(blocks []map[string]json.RawMessage) {
		var heights []float64
		if err := json.Unmarshal(blocks[0]["heights"], &heights); err != nil {
			t.Fatal(err)
		}
		blocks[0]["heights"] = mustJSONRaw(t, heights[:len(heights)-1])
	})
	_, err := DecodeProject(data)
	assertBiz(t, err, CodeSchema, "block:tl", "heights")
}

func TestSchemaWeightOutOfRange(t *testing.T) {
	p, _ := GenerateProject(5)
	data := mutateBlockArray(t, p, func(blocks []map[string]json.RawMessage) {
		var weights []int
		if err := json.Unmarshal(blocks[0]["weights"], &weights); err != nil {
			t.Fatal(err)
		}
		weights[0] = WeightQuantum + 1
		blocks[0]["weights"] = mustJSONRaw(t, weights)
	})
	_, err := DecodeProject(data)
	assertBiz(t, err, CodeSchema, "block:tl", "weights")
}

func TestSchemaFractionalWeight(t *testing.T) {
	p, _ := GenerateProject(51)
	data := mutateBlockArray(t, p, func(blocks []map[string]json.RawMessage) {
		var weights []json.RawMessage
		_ = json.Unmarshal(blocks[0]["weights"], &weights)
		weights[0] = json.RawMessage("1.5")
		blocks[0]["weights"] = mustJSONRaw(t, weights)
	})
	_, err := DecodeProject(data)
	assertBiz(t, err, CodeSchema, "block:tl", "weights")
}

func TestSchemaSeamMissingBlock(t *testing.T) {
	p, _ := GenerateProject(6)
	data := mustJSON(t, p)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(data, &top)
	var seams []map[string]json.RawMessage
	_ = json.Unmarshal(top["seams"], &seams)
	seams[0]["block_b"] = mustJSONRaw(t, "missing")
	top["seams"] = mustJSONRaw(t, seams)
	_, err := DecodeProject(mustJSON(t, top))
	assertBiz(t, err, CodeSchema, "seam:tl-tr", "block_b")
}

func mutateBlockArray(t *testing.T, p *Project, fn func([]map[string]json.RawMessage)) []byte {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, p), &top); err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(top["blocks"], &blocks); err != nil {
		t.Fatal(err)
	}
	fn(blocks)
	top["blocks"] = mustJSONRaw(t, blocks)
	return mustJSON(t, top)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustJSONRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	return json.RawMessage(mustJSON(t, v))
}

func assertBiz(t *testing.T, err error, code, object, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	be, ok := AsError(err)
	if !ok {
		t.Fatalf("not a structured error: %v", err)
	}
	if be.Code != code {
		t.Fatalf("code=%s want %s (%v)", be.Code, code, err)
	}
	if object != "" && be.Object != object {
		t.Fatalf("object=%s want %s (%v)", be.Object, object, err)
	}
	if field != "" && be.Field != field {
		t.Fatalf("field=%s want %s (%v)", be.Field, field, err)
	}
}
