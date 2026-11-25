package dressbatch

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func checkProjectJSONPresence(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return newError(CodeSchema, "project", "json", "cannot decode project: %v", err)
	}
	if err := requireKeys(root, "project", "project", "schema_version", "id", "name", "seed", "materials", "mounts", "parts", "mutual_exclusions", "constraints", "truth"); err != nil {
		return err
	}
	if err := checkArrayObjects(root["materials"], "project", "materials", func(item map[string]json.RawMessage, objectID, prefix string) error {
		return requireKeys(item, objectID, prefix, "id", "version")
	}); err != nil {
		return err
	}
	if err := checkArrayObjects(root["mounts"], "project", "mounts", func(item map[string]json.RawMessage, objectID, prefix string) error {
		return requireKeys(item, objectID, prefix, "id")
	}); err != nil {
		return err
	}
	if err := checkArrayObjects(root["parts"], "project", "parts", checkPartPresence); err != nil {
		return err
	}
	if err := checkArrayObjects(root["mutual_exclusions"], "project", "mutual_exclusions", func(item map[string]json.RawMessage, objectID, prefix string) error {
		return requireKeys(item, objectID, prefix, "id", "a", "b")
	}); err != nil {
		return err
	}
	var constraints map[string]json.RawMessage
	if err := unmarshalObject(root["constraints"], "constraints", "constraints", &constraints); err != nil {
		return err
	}
	if err := requireKeys(constraints, "constraints", "constraints", "max_material_slots_per_batch", "max_vertices_per_batch", "max_indices_per_batch", "allow_cross_skin", "policy"); err != nil {
		return err
	}
	var truth map[string]json.RawMessage
	if err := unmarshalObject(root["truth"], "truth", "truth", &truth); err != nil {
		return err
	}
	if err := requireKeys(truth, "truth", "truth", "assemblies"); err != nil {
		return err
	}
	if err := checkArrayObjects(truth["assemblies"], "truth", "truth.assemblies", checkAssemblyPresence); err != nil {
		return err
	}
	return nil
}

func checkPartPresence(item map[string]json.RawMessage, objectID, prefix string) error {
	if err := requireKeys(item, objectID, prefix, "id", "mount_id", "vertex_count", "triangle_count", "material_slots", "submeshes", "skin", "priority"); err != nil {
		return err
	}
	var skin bool
	if raw, exists := item["skin"]; exists {
		_ = json.Unmarshal(raw, &skin)
	}
	if skin {
		if err := requireKeys(item, objectID, prefix, "skin_group"); err != nil {
			return err
		}
	}
	if err := checkArrayObjects(item["submeshes"], objectID, prefix+".submeshes", func(submesh map[string]json.RawMessage, submeshID, submeshPrefix string) error {
		return requireKeys(submesh, submeshID, submeshPrefix, "id", "material_slot", "vertex_start", "vertex_count", "index_start", "index_count")
	}); err != nil {
		return err
	}
	return nil
}

func checkAssemblyPresence(item map[string]json.RawMessage, objectID, prefix string) error {
	if err := requireKeys(item, objectID, prefix, "id", "pose", "candidate_part_ids", "expected_legal", "expected_accepted_part_ids", "expected_conflicts", "expected_batches", "expected_commit_sequence"); err != nil {
		return err
	}
	if err := checkArrayObjects(item["expected_conflicts"], objectID, prefix+".expected_conflicts", func(conflict map[string]json.RawMessage, conflictID, conflictPrefix string) error {
		return requireKeys(conflict, conflictID, conflictPrefix, "category", "part_ids", "detail")
	}); err != nil {
		return err
	}
	return checkArrayObjects(item["expected_batches"], objectID, prefix+".expected_batches", checkBatchTruthPresence)
}

func checkBatchTruthPresence(item map[string]json.RawMessage, objectID, prefix string) error {
	if err := requireKeys(item, objectID, prefix, "batch_id", "part_ids", "material_slots", "skin", "mixed_skin", "skin_groups", "vertex_used", "index_used", "placements"); err != nil {
		return err
	}
	return checkArrayObjects(item["placements"], objectID, prefix+".placements", checkPlacementPresence)
}

func checkPlacementPresence(item map[string]json.RawMessage, objectID, prefix string) error {
	if err := requireKeys(item, objectID, prefix, "part_id", "batch_id", "vertex_offset", "vertex_start", "vertex_end", "index_offset", "index_start", "index_end", "submeshes"); err != nil {
		return err
	}
	return checkArrayObjects(item["submeshes"], objectID, prefix+".submeshes", func(submesh map[string]json.RawMessage, submeshID, submeshPrefix string) error {
		return requireKeys(submesh, submeshID, submeshPrefix,
			"part_id", "submesh_id", "material_slot",
			"vertex_offset", "vertex_start", "vertex_end",
			"index_offset", "index_start", "index_end",
			"source_vertex_start", "source_vertex_end",
			"source_index_start", "source_index_end",
		)
	})
}

func checkArrayObjects(raw json.RawMessage, objectID, prefix string, check func(map[string]json.RawMessage, string, string) error) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return newError(CodeSchema, objectID, prefix, "required field is missing or null")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return newError(CodeSchema, objectID, prefix, "must be an array of objects")
	}
	for i, item := range items {
		itemPrefix := fmt.Sprintf("%s[%d]", prefix, i)
		childID := objectID
		if id, ok := rawString(item["id"]); ok && id != "" {
			childID = objectKind(prefix) + ":" + id
		} else if batchID, ok := rawString(item["batch_id"]); ok && batchID != "" {
			childID = objectID + "/batch:" + batchID
		} else if partID, ok := rawString(item["part_id"]); ok && partID != "" {
			childID = "part:" + partID
		}
		if err := check(item, childID, itemPrefix); err != nil {
			return err
		}
	}
	return nil
}

func unmarshalObject(raw json.RawMessage, objectID, prefix string, target *map[string]json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return newError(CodeSchema, objectID, prefix, "required field is missing or null")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return newError(CodeSchema, objectID, prefix, "must be an object")
	}
	return nil
}

func requireKeys(object map[string]json.RawMessage, objectID, prefix string, keys ...string) error {
	for _, key := range keys {
		raw, exists := object[key]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return newError(CodeSchema, objectID, prefix+"."+key, "required field is missing or null")
		}
	}
	return nil
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func objectKind(prefix string) string {
	switch {
	case len(prefix) >= 6 && prefix[:6] == "parts[":
		return "part"
	case len(prefix) >= 7 && prefix[:7] == "mounts[":
		return "mount"
	case len(prefix) >= 10 && prefix[:10] == "materials[":
		return "material"
	case len(prefix) >= 17 && prefix[:17] == "mutual_exclusion":
		return "exclusion"
	default:
		return "object"
	}
}
