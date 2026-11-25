package plancache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func hashText(domain string, parts ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// StructureSignature renders the plan as traversal-indexed lines. IDs are not
// used for local topology; child positions make the tree shape explicit.
func StructureSignature(stmt Statement) string {
	order := stmt.TraversalOrder()
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	nodes := make(map[string]Node, len(order))
	for _, sp := range stmt.Subplans {
		for _, n := range sp.Nodes {
			nodes[n.ID] = n
		}
	}
	lines := make([]string, 0, len(order))
	for i, id := range order {
		n := nodes[id]
		children := make([]string, 0, len(n.Children))
		for _, child := range n.Children {
			if p, ok := pos[child]; ok {
				children = append(children, fmt.Sprintf("%d", p))
			} else {
				children = append(children, "unresolved:"+child)
			}
		}
		args := make([]string, len(n.Arguments))
		for j, a := range n.Arguments {
			args[j] = fmt.Sprintf("%d", a)
		}
		ref := ""
		if n.Ref != nil {
			ref = n.Ref.StatementID + "/" + n.Ref.SubplanID + "/" + n.Ref.PlanID
		}
		lines = append(lines, fmt.Sprintf(
			"%d|sp=%s|op=%s|args=[%s]|children=[%s]|ref=%s|cost_ns=%d",
			i, n.SubplanID, n.Op, strings.Join(args, ","), strings.Join(children, ","), ref, n.EstimatedCostNS,
		))
	}
	return strings.Join(lines, "\n")
}

func ComputeFingerprint(stmt Statement) string {
	return "fp_" + hashText("plancache.fingerprint/v1", StructureSignature(stmt))[:32]
}

func ValidateRuntimeBudget(r RuntimeBudget) error {
	if r.Workers <= 0 {
		return withContext(newError(CodeBudgetWorkersInvalid, "runtime.workers", "worker budget must be positive"), "workers", r.Workers)
	}
	if r.SessionBuffer <= 0 {
		return withContext(newError(CodeBudgetBufferInvalid, "runtime.session_buffer", "session buffer must be positive"), "session_buffer", r.SessionBuffer)
	}
	return nil
}

func ValidateCapacity(c Capacity) error {
	if c.MaxCacheBytes == 0 {
		return newError(CodeCapacityBytesZero, "capacity.max_cache_bytes", "cache byte capacity must not be zero")
	}
	if c.MaxCacheBytes < 0 {
		return withContext(newError(CodeCapacityBytesZero, "capacity.max_cache_bytes", "cache byte capacity is negative"), "max_cache_bytes", c.MaxCacheBytes)
	}
	if c.MaxPlansPerStatement <= 0 {
		return withContext(newError(CodeSchemaFieldConflict, "capacity.max_plans_per_statement", "per-statement capacity must be positive"), "value", c.MaxPlansPerStatement)
	}
	return nil
}

func ValidateProject(p *Project) error {
	if p == nil {
		return newError(CodeSchemaMissingField, "$", "project is nil")
	}
	if p.EngineeringVersion == "" {
		return newError(CodeSchemaMissingField, "engineering_version", "engineering version is required")
	}
	if p.TickDurationNS <= 0 {
		return withContext(newError(CodeSchemaFieldConflict, "tick_duration_ns", "tick duration must be positive"), "value", p.TickDurationNS)
	}
	if err := ValidateCapacity(p.Capacity); err != nil {
		return err
	}
	if err := ValidateRuntimeBudget(p.Runtime); err != nil {
		return err
	}
	if p.PropagationDepth < 0 {
		return withContext(newError(CodeSchemaFieldConflict, "propagation_depth", "propagation depth must be non-negative"), "value", p.PropagationDepth)
	}
	stmtIDs := map[string]bool{}
	for i := range p.Statements {
		st := &p.Statements[i]
		path := fmt.Sprintf("statements[%d](%s)", i, st.ID)
		if st.ID == "" {
			return newError(CodeSchemaMissingField, path+".id", "statement id is required")
		}
		if stmtIDs[st.ID] {
			return withContext(newError(CodeSchemaFieldConflict, path+".id", "duplicate statement id"), "statement_id", st.ID)
		}
		stmtIDs[st.ID] = true
		if err := validateStatement(st, path); err != nil {
			return err
		}
	}
	for i := range p.Statements {
		st := &p.Statements[i]
		for _, sp := range st.Subplans {
			for _, n := range sp.Nodes {
				if n.Ref != nil && n.Ref.StatementID != "" {
					target, ok := statementByID(p, n.Ref.StatementID)
					if !ok {
						return withContext(newError(CodeReferenceDangling, pathForNode(st.ID, n.ID)+".ref.statement_id", "reference points to absent statement"), "statement_id", n.Ref.StatementID, "node_id", n.ID)
					}
					if n.Ref.SubplanID != "" && !subplanExists(target, n.Ref.SubplanID) {
						return withContext(newError(CodeReferenceDangling, pathForNode(st.ID, n.ID)+".ref.subplan_id", "reference points to absent subplan"), "statement_id", target.ID, "subplan_id", n.Ref.SubplanID, "node_id", n.ID)
					}
				}
			}
		}
	}
	return nil
}

func pathForNode(stmtID, nodeID string) string {
	return fmt.Sprintf("statements[%s].nodes[%s]", stmtID, nodeID)
}

func statementByID(p *Project, id string) (*Statement, bool) {
	for i := range p.Statements {
		if p.Statements[i].ID == id {
			return &p.Statements[i], true
		}
	}
	return nil, false
}

func subplanExists(s *Statement, id string) bool {
	for _, sp := range s.Subplans {
		if sp.ID == id {
			return true
		}
	}
	return false
}

func validateStatement(st *Statement, path string) error {
	if st.Template == "" {
		return newError(CodeSchemaMissingField, path+".template", "statement template is required")
	}
	paramIDs := map[int]bool{}
	for _, param := range st.Params {
		if param.Index < 0 || param.Index >= len(st.Params) {
			return withContext(newError(CodeParamIndexOutOfRange, path+".parameters", "parameter declaration index is out of contiguous range"), "index", param.Index, "param_count", len(st.Params))
		}
		if paramIDs[param.Index] {
			return withContext(newError(CodeSchemaFieldConflict, path+".parameters", "duplicate parameter index"), "index", param.Index)
		}
		paramIDs[param.Index] = true
		if param.Type == "" {
			return withContext(newError(CodeSchemaMissingField, path+".parameters.type", "parameter type is required"), "index", param.Index)
		}
	}
	for i := 0; i < len(st.Params); i++ {
		if !paramIDs[i] {
			return withContext(newError(CodeSchemaMissingField, path+".parameters", "parameter indexes must be contiguous from zero"), "missing_index", i)
		}
	}

	nodeCount := 0
	nodeByID := map[string]Node{}
	parentCount := map[string]int{}
	subplanByID := map[string]Subplan{}
	for _, sp := range st.Subplans {
		if sp.ID == "" || sp.RootID == "" {
			return newError(CodeNodeTreeMalformed, path+".subplans", "subplan id and root id are required")
		}
		if _, exists := subplanByID[sp.ID]; exists {
			return withContext(newError(CodeNodeTreeMalformed, path+".subplans", "duplicate subplan id"), "subplan_id", sp.ID)
		}
		subplanByID[sp.ID] = sp
		for _, n := range sp.Nodes {
			nodeCount++
			if n.ID == "" {
				return newError(CodeNodeTreeMalformed, path+".nodes", "node id is required")
			}
			if _, exists := nodeByID[n.ID]; exists {
				return withContext(newError(CodeNodeTreeMalformed, path+".nodes", "duplicate node id"), "node_id", n.ID)
			}
			nodeByID[n.ID] = n
		}
	}
	if st.MainSubplanID == "" {
		return newError(CodeSchemaMissingField, path+".main_subplan_id", "main subplan id is required")
	}
	main, ok := subplanByID[st.MainSubplanID]
	if !ok {
		return withContext(newError(CodeNodeTreeMalformed, path+".main_subplan_id", "main subplan does not exist"), "main_subplan_id", st.MainSubplanID)
	}
	for _, sp := range st.Subplans {
		if _, ok := nodeByID[sp.RootID]; !ok {
			return withContext(newError(CodeNodeTreeMalformed, path+".root_id", "subplan root is absent"), "subplan_id", sp.ID, "root_id", sp.RootID)
		}
		for _, n := range sp.Nodes {
			if n.SubplanID != "" && n.SubplanID != sp.ID {
				return withContext(newError(CodeNodeTreeMalformed, path+".nodes.subplan_id", "node is assigned to conflicting subplan"), "node_id", n.ID, "expected", sp.ID, "actual", n.SubplanID)
			}
			for _, arg := range n.Arguments {
				if arg < 0 || arg >= len(st.Params) {
					return withContext(newError(CodeParamIndexOutOfRange, path+".nodes.arguments", "placeholder index is outside statement parameter range"), "node_id", n.ID, "argument", arg, "param_count", len(st.Params))
				}
			}
			for _, child := range n.Children {
				childNode, ok := nodeByID[child]
				if !ok {
					return withContext(newError(CodeNodeTreeMalformed, path+".nodes.children", "child node is absent"), "node_id", n.ID, "child_id", child)
				}
				if childNode.SubplanID != sp.ID {
					return withContext(newError(CodeNodeTreeMalformed, path+".nodes.children", "child crosses subplan group"), "parent_id", n.ID, "child_id", child, "parent_subplan", sp.ID, "child_subplan", childNode.SubplanID)
				}
				parentCount[child]++
				if parentCount[child] > 1 {
					return withContext(newError(CodeNodeTreeMalformed, path+".nodes.children", "node has more than one parent"), "child_id", child)
				}
			}
		}
	}
	// Cycle detection and reachability. A valid plan tree has exactly one
	// reachable component per declared subplan root.
	state := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return withContext(newError(CodeNodeTreeMalformed, path+".nodes.children", "cycle in node tree"), "node_id", id)
		case 2:
			return nil
		}
		state[id] = 1
		n := nodeByID[id]
		for _, child := range n.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, sp := range st.Subplans {
		if err := visit(sp.RootID); err != nil {
			return err
		}
	}
	order := st.TraversalOrder()
	if len(order) != nodeCount {
		return withContext(newError(CodeNodeTreeMalformed, path+".nodes", "unreachable or disconnected nodes in plan tree"), "reachable", len(order), "declared", nodeCount)
	}
	// Main root must be first traversal.
	if len(order) == 0 || order[0] != main.RootID {
		return newError(CodeNodeTreeMalformed, path+".main_subplan_id", "main subplan root must be first traversal node")
	}

	expectedFP := ComputeFingerprint(*st)
	if st.Fingerprint != "" && st.Fingerprint != expectedFP {
		return withContext(newError(CodeFingerprintConflict, path+".fingerprint", "declared plan fingerprint does not match canonical structure"), "expected", expectedFP, "actual", st.Fingerprint, "statement_id", st.ID)
	}
	return nil
}

// CanonicalizeProject normalizes loader-owned models to preorder node slices
// and fills omitted fingerprints. It must be called on freshly loaded data,
// not on a caller's source project during request/error processing.
func CanonicalizeProject(p *Project) {
	if p == nil {
		return
	}
	for i := range p.Statements {
		st := &p.Statements[i]
		byID := map[string]Node{}
		for _, sp := range st.Subplans {
			for _, n := range sp.Nodes {
				byID[n.ID] = n
			}
		}
		canonicalizeNodeOrder(st, byID)
		if st.Fingerprint == "" {
			st.Fingerprint = ComputeFingerprint(*st)
		}
	}
}

func canonicalizeNodeOrder(st *Statement, byID map[string]Node) {
	for i := range st.Subplans {
		root := st.Subplans[i].RootID
		orderedIDs := make([]string, 0, len(st.Subplans[i].Nodes))
		seen := map[string]bool{}
		var walk func(string)
		walk = func(id string) {
			if id == "" || seen[id] {
				return
			}
			n, ok := byID[id]
			if !ok {
				return
			}
			seen[id] = true
			orderedIDs = append(orderedIDs, id)
			for _, child := range n.Children {
				walk(child)
			}
		}
		walk(root)
		orderedNodes := make([]Node, 0, len(orderedIDs))
		for _, id := range orderedIDs {
			orderedNodes = append(orderedNodes, byID[id])
		}
		st.Subplans[i].Nodes = orderedNodes
	}
}

// CanonicalEqualForLoad checks all fields that must survive schema conversion.
// Fingerprint is included because absent fingerprints are canonically computed.
func CanonicalEqualForLoad(a, b *Project) bool {
	if a == nil || b == nil {
		return a == b
	}
	ca := *a
	cb := *b
	ca.SchemaVersion = ""
	cb.SchemaVersion = ""
	ca.EngineeringVersion = normalizeEngineeringVersion(ca.EngineeringVersion)
	cb.EngineeringVersion = normalizeEngineeringVersion(cb.EngineeringVersion)
	return canonicalJSON(ca) == canonicalJSON(cb)
}

func normalizeEngineeringVersion(v string) string {
	if i := strings.LastIndex(v, "@"); i >= 0 {
		return v[:i]
	}
	return v
}

func SortedStatementIDs(p *Project) []string {
	out := make([]string, 0, len(p.Statements))
	for _, s := range p.Statements {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}
