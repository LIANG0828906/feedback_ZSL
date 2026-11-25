package loader_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"graphlab/internal/generator"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

func TestLoaderFaultInjectionStableCodesAndSourceImmutable(t *testing.T) {
	sample, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	original := sample.Documents[model.SchemaV1]
	before := append([]byte(nil), original...)

	t.Run("bad scale", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			vertex := firstMapArray(t, m, "vertices")
			attrs := vertex["attrs"].(map[string]any)
			attrs["score"].(map[string]any)["scale"] = float64(999)
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EBadAttributeScale)
		if !bytes.Equal(original, before) {
			t.Fatal("source document was modified")
		}
	})

	t.Run("missing edge vertex", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			edge := firstMapArray(t, m, "edges")
			edge["target_vertex"] = "v9998"
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EMissingVertex)
	})

	t.Run("overlapping shards", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			ranges := m["vertex_ranges"].([]any)
			ranges[1].(map[string]any)["start_key"] = "v0005"
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EOverlapShard)
	})

	t.Run("unsupported unit", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			m["clock"].(map[string]any)["unit"] = "furlong"
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EUnsupportedUnit)
	})

	t.Run("ambiguous schema markers", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			m["schemaVersion"] = "graph-schema-v3"
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EAmbiguousField)
	})

	t.Run("empty shard keyspace", func(t *testing.T) {
		doc := mutateV1(t, original, func(m map[string]any) {
			ranges := m["vertex_ranges"].([]any)
			ranges[0].(map[string]any)["end_key"] = "v0000"
		})
		_, err := loader.Load(doc)
		assertCode(t, err, model.EEmptyShardKeyspace)
	})
}

func mutateV1(t *testing.T, original []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		t.Fatal(err)
	}
	// Decode into an independent value first; the test's source bytes remain
	// untouched and every mutation works on a fresh copy.
	copyBytes := append([]byte(nil), original...)
	if err := json.Unmarshal(copyBytes, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func firstMapArray(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	items, ok := m[key].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("missing array %s", key)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("array %s first item is not an object", key)
	}
	return first
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if !model.IsCode(err, code) {
		t.Fatalf("expected %s, got %v", code, err)
	}
	var be *model.BizError
	if !asBiz(err, &be) || len(be.Context) == 0 {
		t.Fatalf("error lacks locatable context: %v", err)
	}
}

func asBiz(err error, target **model.BizError) bool {
	for err != nil {
		if be, ok := err.(*model.BizError); ok {
			*target = be
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}
