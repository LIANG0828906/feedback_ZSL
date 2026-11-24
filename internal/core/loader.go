package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, NewError(CodeIO, "failed to read scenario").At(path).Wrap(err)
	}
	return DecodeScenario(data)
}

func DecodeScenario(data []byte) (*Scenario, error) {
	if !json.Valid(data) {
		return nil, NewError(CodeDecode, "sample is not valid JSON")
	}
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, NewError(CodeDecode, "sample cannot be decoded").Wrap(err)
	}
	root, ok := raw.(map[string]any)
	if !ok {
		return nil, NewError(CodeDecode, "sample root must be an object").At("$")
	}
	if err := checkPresence(root); err != nil {
		return nil, err
	}

	var s Scenario
	if err := json.Unmarshal(data, &s); err != nil {
		e := NewError(CodeDecode, "sample contains a value of the wrong type")
		var ute *json.UnmarshalTypeError
		if ok := asTypeError(err, &ute); ok {
			e.Path = ute.Field
		}
		return nil, e.Wrap(err)
	}
	if err := ValidateScenario(&s); err != nil {
		return nil, err
	}
	hash, err := ComputeSceneHash(&s)
	if err != nil {
		return nil, NewError(CodeState, "failed to hash scene").Wrap(err)
	}
	if s.SceneHash != "" && s.SceneHash != hash {
		return nil, NewError(CodeSchema, "scene_hash does not match scene content").
			At("scene_hash").WithValues(s.SceneHash, hash)
	}
	s.SceneHash = hash
	return &s, nil
}

func asTypeError(err error, target **json.UnmarshalTypeError) bool {
	if ute, ok := err.(*json.UnmarshalTypeError); ok {
		*target = ute
		return true
	}
	return false
}

func checkPresence(root map[string]any) *Error {
	required := []string{
		"schema_version", "id", "seed", "tick_unit_ms", "params_version",
		"final_tick", "nodes", "intents", "truth",
	}
	for _, k := range required {
		if _, ok := root[k]; !ok {
			return NewError(CodeSchema, "required field is missing").At(k)
		}
	}
	nodes, ok := root["nodes"].([]any)
	if !ok {
		return NewError(CodeDecode, "nodes must be an array").At("nodes")
	}
	for i, item := range nodes {
		n, ok := item.(map[string]any)
		if !ok {
			return NewError(CodeDecode, "node must be an object").At(fmt.Sprintf("nodes[%d]", i))
		}
		nodeID, _ := n["id"].(string)
		for _, k := range []string{
			"id", "type", "active", "level", "workers", "max_workers",
			"outputs", "inputs", "capacity", "workers_required",
			"initial_stock", "upgrades",
		} {
			if _, ok := n[k]; !ok {
				e := NewError(CodeSchema, "required node field is missing").
					At(fmt.Sprintf("nodes[%d].%s", i, k))
				if nodeID != "" {
					e.ForNode(nodeID)
				}
				return e
			}
		}
		if upgrades, ok := n["upgrades"].([]any); ok {
			for j, raw := range upgrades {
				up, ok := raw.(map[string]any)
				if !ok {
					return NewError(CodeDecode, "upgrade must be an object").
						At(fmt.Sprintf("nodes[%d].upgrades[%d]", i, j))
				}
				for _, k := range []string{"level", "outputs", "inputs", "capacity", "workers_required"} {
					if _, ok := up[k]; !ok {
						e := NewError(CodeSchema, "required upgrade field is missing").
							At(fmt.Sprintf("nodes[%d].upgrades[%d].%s", i, j, k))
						if nodeID != "" {
							e.ForNode(nodeID)
						}
						return e
					}
				}
			}
		}
	}
	intents, ok := root["intents"].([]any)
	if !ok {
		return NewError(CodeDecode, "intents must be an array").At("intents")
	}
	for i, item := range intents {
		in, ok := item.(map[string]any)
		if !ok {
			return NewError(CodeDecode, "intent must be an object").At(fmt.Sprintf("intents[%d]", i))
		}
		for _, k := range []string{"seq", "timestamp", "type", "node_id"} {
			if _, ok := in[k]; !ok {
				return NewError(CodeSchema, "required intent field is missing").
					At(fmt.Sprintf("intents[%d].%s", i, k))
			}
		}
		nodeID, _ := in["node_id"].(string)
		intentType, _ := in["type"].(string)
		intentID := int64(i)
		if seq, ok := in["seq"].(json.Number); ok {
			if parsed, err := seq.Int64(); err == nil {
				intentID = parsed
			}
		}
		requiredPayload := map[string][]string{
			string(IntentUpgrade):      {"level"},
			string(IntentAssignWorker): {"workers"},
			string(IntentRecipe):       {"recipe"},
			string(IntentConsume):      {"consume"},
		}
		for _, k := range requiredPayload[intentType] {
			if _, ok := in[k]; !ok {
				e := NewError(CodeSchema, "required intent payload field is missing").
					At(fmt.Sprintf("intents[%d].%s", i, k)).
					ForIntent(intentID, i)
				if nodeID != "" {
					e.ForNode(nodeID)
				}
				return e
			}
		}
		if intentType == string(IntentRecipe) && in["recipe"] != nil {
			recipe, ok := in["recipe"].(map[string]any)
			if !ok {
				return NewError(CodeDecode, "recipe must be an object").
					At(fmt.Sprintf("intents[%d].recipe", i))
			}
			for _, k := range []string{"outputs", "inputs", "capacity", "workers_required"} {
				if _, ok := recipe[k]; !ok {
					e := NewError(CodeSchema, "required recipe field is missing").
						At(fmt.Sprintf("intents[%d].recipe.%s", i, k)).
						ForIntent(intentID, i)
					if nodeID != "" {
						e.ForNode(nodeID)
					}
					return e
				}
			}
		}
	}
	truth, ok := root["truth"].(map[string]any)
	if !ok {
		return NewError(CodeDecode, "truth must be an object").At("truth")
	}
	for _, k := range []string{"sample_ticks", "snapshots"} {
		if _, ok := truth[k]; !ok {
			return NewError(CodeSchema, "required truth field is missing").At("truth." + k)
		}
	}
	return nil
}
