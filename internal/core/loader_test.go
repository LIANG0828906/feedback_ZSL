package core_test

import (
	"encoding/json"
	"testing"

	"economycore/internal/core"
)

func marshalMap(t *testing.T, s *core.Scenario) map[string]any {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeMap(t *testing.T, m map[string]any) (*core.Scenario, error) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return core.DecodeScenario(data)
}

func TestMissingNodeFieldReturnsESchema(t *testing.T) {
	s := baseScenario(t)
	m := marshalMap(t, s)
	node := m["nodes"].([]any)[0].(map[string]any)
	delete(node, "capacity")
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.Path != "nodes[0].capacity" || e.NodeID != "source" {
		t.Fatalf("bad schema locator: path=%q node=%q", e.Path, e.NodeID)
	}
}

func TestCycleReturnsESchemaWithNode(t *testing.T) {
	s := baseScenario(t)
	s.Nodes[0].ParentID = "factory"
	m := marshalMap(t, s)
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.NodeID == "" || e.Field != "parent_id" {
		t.Fatalf("bad cycle locator: %+v", e)
	}
}

func TestIntentMissingTargetReturnsESchema(t *testing.T) {
	s := baseScenario(t)
	m := marshalMap(t, s)
	in := m["intents"].([]any)[0].(map[string]any)
	in["node_id"] = "ghost"
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.Path != "intents[0].node_id" || e.NodeID != "ghost" {
		t.Fatalf("bad intent ref locator: %+v", e)
	}
}

func TestNegativeRateReturnsESchema(t *testing.T) {
	s := baseScenario(t)
	m := marshalMap(t, s)
	node := m["nodes"].([]any)[0].(map[string]any)
	node["outputs"].(map[string]any)["grain"] = float64(-1)
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.Field != "outputs.grain" || e.NodeID != "source" {
		t.Fatalf("bad fixed-point locator: %+v", e)
	}
}

func TestNonMonotonicIntentTimestampReturnsESchema(t *testing.T) {
	s := baseScenario(t)
	m := marshalMap(t, s)
	intents := m["intents"].([]any)
	intents[1].(map[string]any)["timestamp"] = float64(1)
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	if e := core.AsError(err); e.Path != "intents[1].timestamp" {
		t.Fatalf("path=%q", e.Path)
	}
}

func TestMissingIntentTimestampReturnsESchema(t *testing.T) {
	s := baseScenario(t)
	m := marshalMap(t, s)
	delete(m["intents"].([]any)[0].(map[string]any), "timestamp")
	_, err := decodeMap(t, m)
	if core.CodeOf(err) != core.CodeSchema {
		t.Fatalf("code=%s want E_SCHEMA: %v", core.CodeOf(err), err)
	}
	if e := core.AsError(err); e.Path != "intents[0].timestamp" {
		t.Fatalf("path=%q", e.Path)
	}
}
