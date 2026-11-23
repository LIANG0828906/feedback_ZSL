package spatialpick

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleJSON(t *testing.T) ([]byte, []byte, *Scene, *Truth) {
	t.Helper()
	scene, truth, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := json.Marshal(scene)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := json.Marshal(truth)
	if err != nil {
		t.Fatal(err)
	}
	return sb, tb, scene, truth
}

func mutateJSON(t *testing.T, data []byte, fn func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLoaderReportsSchemaCycleAndRefWithoutPartialLoad(t *testing.T) {
	sceneBytes, truthBytes, _, _ := sampleJSON(t)

	t.Run("missing field", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			n := m["nodes"].([]any)[0].(map[string]any)
			delete(n, "local_matrix")
		})
		sc, tr, err := ParseSceneWithTruth(bad, truthBytes)
		if !IsCode(err, CodeSchema) {
			t.Fatalf("got %v, want E_SCHEMA", err)
		}
		var ce *CodedError
		IsCoded(err, &ce)
		if !strings.Contains(ce.Field, "scene.nodes[0].local_matrix") {
			t.Fatalf("field path = %q", ce.Field)
		}
		if ce.NodeID != "n000" {
			t.Fatalf("node id = %q", ce.NodeID)
		}
		if sc != nil || tr != nil {
			t.Fatal("partial load returned documents")
		}
	})

	t.Run("cycle", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			n0 := m["nodes"].([]any)[0].(map[string]any)
			n1 := m["nodes"].([]any)[1].(map[string]any)
			n0["parent_id"] = n1["id"]
		})
		sc, tr, err := ParseSceneWithTruth(bad, truthBytes)
		if !IsCode(err, CodeCycle) {
			t.Fatalf("got %v, want E_CYCLE", err)
		}
		var ce *CodedError
		IsCoded(err, &ce)
		if ce.NodeID == "" || !strings.Contains(ce.Field, "parent_id") {
			t.Fatalf("incomplete cycle error: %+v", ce)
		}
		if sc != nil || tr != nil {
			t.Fatal("partial load returned documents")
		}
	})

	t.Run("geometry ref", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			n := m["nodes"].([]any)[0].(map[string]any)
			n["geometry_index"] = float64(9999)
		})
		sc, tr, err := ParseSceneWithTruth(bad, truthBytes)
		if !IsCode(err, CodeRef) {
			t.Fatalf("got %v, want E_REF", err)
		}
		var ce *CodedError
		IsCoded(err, &ce)
		if !strings.Contains(ce.Field, "geometry_index") || ce.NodeID == "" {
			t.Fatalf("incomplete ref error: %+v", ce)
		}
		if sc != nil || tr != nil {
			t.Fatal("partial load returned documents")
		}
	})

	t.Run("triangle ref", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			for _, ga := range m["geometries"].([]any) {
				g := ga.(map[string]any)
				if g["kind"] == GeometryMesh {
					tri := g["triangles"].([]any)[0].([]any)
					tri[0] = float64(9999)
					return
				}
			}
		})
		_, _, err := ParseSceneWithTruth(bad, truthBytes)
		if !IsCode(err, CodeRef) {
			t.Fatalf("got %v, want E_REF", err)
		}
		var ce *CodedError
		IsCoded(err, &ce)
		if !strings.Contains(ce.Field, ".triangles[0][0]") {
			t.Fatalf("field path = %q", ce.Field)
		}
	})
}

func TestLoaderValidatesTruthManifestAndFields(t *testing.T) {
	sceneBytes, truthBytes, scene, _ := sampleJSON(t)

	t.Run("matrix manifest", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			n := m["nodes"].([]any)[0].(map[string]any)
			mat := n["local_matrix"].([]any)
			mat[3] = mat[3].(float64) + 0.001
		})
		var changed Scene
		if err := json.Unmarshal(bad, &changed); err != nil {
			t.Fatal(err)
		}
		hash, err := ComputeSceneHash(&changed)
		if err != nil {
			t.Fatal(err)
		}
		changed.ContentHash = hash
		bad, _ = json.Marshal(&changed)
		changedTruth := mutateJSON(t, truthBytes, func(m map[string]any) {
			m["content_hash"] = hash
		})
		_, _, err = ParseSceneWithTruth(bad, changedTruth)
		if !IsCode(err, CodeSchema) {
			t.Fatalf("got %v, want E_SCHEMA", err)
		}
		var ce *CodedError
		IsCoded(err, &ce)
		if !strings.Contains(ce.Field, "truth.transform_manifest") {
			t.Fatalf("field path = %q", ce.Field)
		}
	})

	t.Run("missing truth field", func(t *testing.T) {
		bad := mutateJSON(t, truthBytes, func(m map[string]any) {
			delete(m, "ray_results")
		})
		_, _, err := ParseSceneWithTruth(sceneBytes, bad)
		if !IsCode(err, CodeSchema) {
			t.Fatalf("got %v, want E_SCHEMA", err)
		}
	})

	t.Run("content hash", func(t *testing.T) {
		bad := mutateJSON(t, sceneBytes, func(m map[string]any) {
			m["content_hash"] = "not-the-hash"
		})
		_, _, err := ParseSceneWithTruth(bad, truthBytes)
		if !IsCode(err, CodeSchema) {
			t.Fatalf("got %v, want E_SCHEMA", err)
		}
	})

	t.Run("truth node ref", func(t *testing.T) {
		bad := mutateJSON(t, truthBytes, func(m map[string]any) {
			ray := m["ray_results"].([]any)[0].(map[string]any)
			first := ray["first"].(map[string]any)
			first["node_id"] = "n999"
		})
		_, _, err := ParseSceneWithTruth(sceneBytes, bad)
		if !IsCode(err, CodeRef) {
			t.Fatalf("got %v, want E_REF", err)
		}
	})
	_ = scene
}
