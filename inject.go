package planopt

import "encoding/json"

// InjectMalformedNodeTreeV1 returns a syntactically valid v1 document whose
// node graph references a child that does not exist.
func InjectMalformedNodeTreeV1(valid []byte) []byte {
	doc := decodeMap(valid)
	stmts, _ := doc["statements"].([]any)
	stmt := stmts[0].(map[string]any)
	nodes, _ := stmt["flat_nodes"].([]any)
	first := nodes[0].(map[string]any)
	first["children"] = []any{"p_injected_missing_node"}
	stmt["root_node"] = first["node_id"]
	return encodeMap(doc)
}

// InjectPlaceholderOutOfRangeV1 points a predicate at an undeclared parameter.
func InjectPlaceholderOutOfRangeV1(valid []byte) []byte {
	doc := decodeMap(valid)
	stmt := firstStatement(doc)
	preds, _ := stmt["predicate_conditions"].([]any)
	pred := preds[0].(map[string]any)
	pred["absolute_param_index"] = 999
	return encodeMap(doc)
}

// InjectScaleOutOfRangeV1 uses a permille value above the declared 0..1000.
func InjectScaleOutOfRangeV1(valid []byte) []byte {
	doc := decodeMap(valid)
	stmt := firstStatement(doc)
	preds, _ := stmt["predicate_conditions"].([]any)
	pred := preds[0].(map[string]any)
	pred["declared_selectivity_permille"] = 1001
	return encodeMap(doc)
}

// InjectUnknownFieldCombinationV1 adds a field not accepted by the strict v1
// loader, representing an unrecognized producer/version combination.
func InjectUnknownFieldCombinationV1(valid []byte) []byte {
	doc := decodeMap(valid)
	doc["x_unknown_operator_dialect"] = map[string]any{"mixed": true}
	return encodeMap(doc)
}

func firstStatement(doc map[string]any) map[string]any {
	stmts, _ := doc["statements"].([]any)
	return stmts[0].(map[string]any)
}

func decodeMap(data []byte) map[string]any {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		panic(err)
	}
	return doc
}

func encodeMap(doc map[string]any) []byte {
	data, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return data
}
