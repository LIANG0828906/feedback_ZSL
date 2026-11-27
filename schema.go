package planopt

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// LoadEngineeringJSON detects one of the three supported envelope versions and
// maps it to the canonical Project. All business parse failures are *Error.
func LoadEngineeringJSON(data []byte) (Project, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return Project{}, Errorf(CodeFieldCombination, "schema.json", "invalid JSON envelope: %v", err)
	}
	if raw, ok := head["format"]; ok {
		var format string
		_ = json.Unmarshal(raw, &format)
		if format == SchemaV1 {
			return loadV1(data)
		}
	}
	if raw, ok := head["schemaVersion"]; ok {
		var version string
		_ = json.Unmarshal(raw, &version)
		if version == SchemaV2 {
			return loadV2(data)
		}
	}
	if raw, ok := head["schema-version"]; ok {
		var version string
		_ = json.Unmarshal(raw, &version)
		if version == SchemaV3 {
			return loadV3(data)
		}
	}
	return Project{}, Errorf(CodeSchemaUnknown, "schema.detect", "unsupported plan engineering envelope")
}

func unmarshalStrict(data []byte, dst any, code, op string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Errorf(code, op, "%v", err)
	}
	return nil
}

// ValidateProject checks cross-reference and fixed-scale invariants after
// schema mapping. Loaders call it; tests may call it for injected documents.
func ValidateProject(project Project) error {
	for _, table := range project.Tables {
		if table.RowCount < 0 {
			return Errorf(CodeScaleOutOfRange, "validate.table_rows", "table %s has negative rows", table.Name)
		}
		for _, col := range table.Columns {
			if col.NullPermyriad < 0 || col.NullPermyriad > ScalePermyriad {
				return Errorf(CodeScaleOutOfRange, "validate.null_scale", "%s.%s nullPermyriad=%d", table.Name, col.Name, col.NullPermyriad)
			}
		}
	}
	for _, plan := range project.Plans {
		nodes := map[string]Node{}
		for _, node := range plan.Nodes {
			if _, dup := nodes[node.ID]; dup {
				return Errorf(CodeMalformedNodeTree, "validate.node_duplicate", "%s duplicate node %s", plan.ID, node.ID)
			}
			nodes[node.ID] = node
		}
		if plan.RootNodeID != "" {
			if _, ok := nodes[plan.RootNodeID]; !ok {
				return Errorf(CodeMalformedNodeTree, "validate.root_missing", "%s root %s is absent", plan.ID, plan.RootNodeID)
			}
		}
		for _, node := range plan.Nodes {
			if !knownOp(node.Kind) {
				return Errorf(CodeFieldCombination, "validate.op_kind", "%s node %s has unrecognized op %q", plan.ID, node.ID, node.Kind)
			}
			for _, child := range node.Children {
				if _, ok := nodes[child]; !ok {
					return Errorf(CodeMalformedNodeTree, "validate.child_missing", "%s node %s references missing child %s", plan.ID, node.ID, child)
				}
			}
			if err := detectNodeCycle(nodes, node.ID); err != nil {
				return err
			}
		}
		params := map[int]Param{}
		for _, p := range plan.Params {
			if _, dup := params[p.GlobalIndex]; dup {
				return Errorf(CodePlaceholderOutOfRange, "validate.param_duplicate", "%s global parameter %d duplicated", plan.ID, p.GlobalIndex)
			}
			params[p.GlobalIndex] = p
		}
		for _, pred := range plan.Predicates {
			if pred.DeclaredSelPermyriad < 0 || pred.DeclaredSelPermyriad > ScalePermyriad {
				return Errorf(CodeScaleOutOfRange, "validate.predicate_scale", "%s declared selectivity=%d", pred.ID, pred.DeclaredSelPermyriad)
			}
			if pred.HasParam {
				if _, ok := params[pred.GlobalParamIndex]; !ok {
					return Errorf(CodePlaceholderOutOfRange, "validate.predicate_param", "%s references global parameter %d", pred.ID, pred.GlobalParamIndex)
				}
				if pred.Op == "BETWEEN" {
					if _, ok := params[pred.SecondGlobalParamIndex]; !ok {
						return Errorf(CodePlaceholderOutOfRange, "validate.between_param", "%s references second global parameter %d", pred.ID, pred.SecondGlobalParamIndex)
					}
				}
			}
		}
	}
	return nil
}

func knownOp(op string) bool {
	switch op {
	case "SCAN", "JOIN", "FILTER", "PROJECT", "AGGREGATE", "SUBQUERY", "LIMIT":
		return true
	}
	return false
}

func detectNodeCycle(nodes map[string]Node, start string) error {
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return Errorf(CodeMalformedNodeTree, "validate.node_cycle", "cycle at node %s", id)
		case 2:
			return nil
		}
		state[id] = 1
		node, ok := nodes[id]
		if !ok {
			return Errorf(CodeMalformedNodeTree, "validate.child_missing", "node %s is absent", id)
		}
		for _, child := range node.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	return visit(start)
}

// EquivalentProject reports whether two canonical projects converge on the
// dimensions required by requirement 1: node set, column-ref order and local
// parameter type sequence for every plan.
func EquivalentProject(a, b Project) error {
	if len(a.Plans) != len(b.Plans) {
		return Errorf(CodeInvariant, "equivalence.plans", "plan count %d != %d", len(a.Plans), len(b.Plans))
	}
	ba := map[int]Plan{}
	bb := map[int]Plan{}
	for _, p := range a.Plans {
		ba[p.Seq] = p
	}
	for _, p := range b.Plans {
		bb[p.Seq] = p
	}
	for seq := range ba {
		pa, ok := bb[seq]
		if !ok {
			return Errorf(CodeInvariant, "equivalence.plan", "plan %d missing in second project", seq)
		}
		pb := ba[seq]
		if err := equivalentPlan(pb, pa); err != nil {
			return err
		}
	}
	return nil
}

func equivalentPlan(want, got Plan) error {
	if want.ID != got.ID {
		return Errorf(CodeInvariant, "equivalence.plan_id", "%s != %s", want.ID, got.ID)
	}
	wantNodes := map[string]Node{}
	for _, n := range want.Nodes {
		wantNodes[n.ID] = canonicalNode(n)
	}
	gotNodes := map[string]Node{}
	for _, n := range got.Nodes {
		gotNodes[n.ID] = canonicalNode(n)
	}
	if len(wantNodes) != len(gotNodes) {
		return Errorf(CodeInvariant, "equivalence.node_count", "%s: %d != %d", want.ID, len(wantNodes), len(gotNodes))
	}
	for id, wn := range wantNodes {
		gn, ok := gotNodes[id]
		if !ok {
			return Errorf(CodeInvariant, "equivalence.node_missing", "%s: node %s missing", want.ID, id)
		}
		if fmt.Sprint(wn) != fmt.Sprint(gn) {
			return Errorf(CodeInvariant, "equivalence.node", "%s node %s: %#v != %#v", want.ID, id, wn, gn)
		}
	}
	if len(want.ColumnRefs) != len(got.ColumnRefs) {
		return Errorf(CodeInvariant, "equivalence.ref_count", want.ID)
	}
	for i := range want.ColumnRefs {
		if want.ColumnRefs[i].Table != got.ColumnRefs[i].Table || want.ColumnRefs[i].Column != got.ColumnRefs[i].Column || want.ColumnRefs[i].Ordinal != got.ColumnRefs[i].Ordinal {
			return Errorf(CodeInvariant, "equivalence.ref_order", "%s ref %d: %s.%s != %s.%s", want.ID, i, want.ColumnRefs[i].Table, want.ColumnRefs[i].Column, got.ColumnRefs[i].Table, got.ColumnRefs[i].Column)
		}
	}
	wantParams := append([]Param(nil), want.Params...)
	gotParams := append([]Param(nil), got.Params...)
	sortParamsLocal(wantParams)
	sortParamsLocal(gotParams)
	if len(wantParams) != len(gotParams) {
		return Errorf(CodeInvariant, "equivalence.param_count", want.ID)
	}
	for i := range wantParams {
		if wantParams[i].TypeName != gotParams[i].TypeName || wantParams[i].LocalIndex != gotParams[i].LocalIndex {
			return Errorf(CodeInvariant, "equivalence.param_types", "%s param %d: %s != %s", want.ID, i, wantParams[i].TypeName, gotParams[i].TypeName)
		}
	}
	return nil
}

func canonicalNode(n Node) Node {
	n.SubplanID = ""
	n.Children = UniqueSorted(n.Children)
	n.PredicateIDs = UniqueSorted(n.PredicateIDs)
	n.ProjectionColumns = UniqueSorted(n.ProjectionColumns)
	return n
}

func sortParamsLocal(params []Param) {
	for i := 0; i < len(params); i++ {
		for j := i + 1; j < len(params); j++ {
			if params[j].LocalIndex < params[i].LocalIndex {
				params[i], params[j] = params[j], params[i]
			}
		}
	}
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstPositive64(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
