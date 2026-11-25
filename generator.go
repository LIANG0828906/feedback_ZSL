package plancache

const DefaultSeed int64 = 420261126

type splitMix64 struct{ state uint64 }

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// GenerateProject builds the deterministic acceptance fixture. The seed is
// accepted so callers can reproduce the exact engineering identity; the
// default acceptance seed is DefaultSeed.
func GenerateProject(seed int64) *Project {
	rng := &splitMix64{state: uint64(seed)}
	_ = rng.next() // reserved for future deterministic field selection

	p := &Project{
		SchemaVersion:      SchemaV1,
		ProjectID:          "fixed-seed-plan-engineering",
		EngineeringVersion: "ENG-SEED-420261126",
		GeneratorSeed:      seed,
		TickDurationNS:     1_000_000,
		KeySettingNames:    []string{"collation", "optimizer_generation"},
		DefaultSettings: map[string]string{
			"collation":            "en_US.utf8",
			"optimizer_generation": "og-7",
			"isolation_level":      "read_committed",
			"application_name":     "plancache",
			"work_mem_kib":         "1024",
		},
		CompatibilityRules: map[string]SettingRule{
			"isolation_level":  {Mode: "exact"},
			"application_name": {Mode: "ignore"},
			"work_mem_kib":     {Mode: "numeric_at_least_entry"},
		},
		Capacity: Capacity{MaxPlansPerStatement: 2, MaxCacheBytes: 16 * 1024},
		Eviction: EvictionPolicy{
			TickGranularityMS: 10,
			PerStatementRule:  "R-CAP-PER-STMT-RC-THEN-LRU-v1",
			TotalBytesRule:    "R-CAP-TOTAL-BYTES-RC-THEN-LRU-v1",
			TieBreakRule:      "reuse_count:asc,last_used_tick:asc,entry_id:asc",
		},
		Runtime:          RuntimeBudget{Workers: 4, SessionBuffer: 8},
		PropagationDepth: 2,
	}

	p.Statements = []Statement{
		buildUsersStatement(),
		buildOrdersStatement(),
		buildOrderItemsStatement(),
		buildInventoryStatement(),
		buildProductsStatement(),
		buildEventsStatement(),
	}
	for i := range p.Statements {
		p.Statements[i].Fingerprint = ComputeFingerprint(p.Statements[i])
	}

	p.Requests = []SessionRequest{
		{Seq: 1, RequestID: "req-001", SessionID: "session-A", StatementID: "S1-users", Settings: map[string]string{}},
		{Seq: 2, RequestID: "req-002", SessionID: "session-B", StatementID: "S1-users", Settings: map[string]string{"application_name": "bi-dashboard"}},
		{Seq: 3, RequestID: "req-003", SessionID: "session-A", StatementID: "S2-orders", Settings: map[string]string{}},
		{Seq: 4, RequestID: "req-004", SessionID: "session-C", StatementID: "S1-users", Settings: map[string]string{"isolation_level": "serializable"}},
		{Seq: 5, RequestID: "req-005", SessionID: "session-B", StatementID: "S2-orders", Settings: map[string]string{"application_name": "dashboard"}},
		{Seq: 6, RequestID: "req-006", SessionID: "session-D", StatementID: "S3-order-items", Settings: map[string]string{}},
		{Seq: 7, RequestID: "req-007", SessionID: "session-A", StatementID: "S1-users", Settings: map[string]string{"application_name": "admin-console"}},
		{Seq: 8, RequestID: "req-008", SessionID: "session-A", StatementID: "S4-inventory", Settings: map[string]string{}},
		{Seq: 9, RequestID: "req-009", SessionID: "session-B", StatementID: "S5-products", Settings: map[string]string{}},
		{Seq: 10, RequestID: "req-010", SessionID: "session-D", StatementID: "S1-users", Settings: map[string]string{"work_mem_kib": "256"}},
		{Seq: 11, RequestID: "req-011", SessionID: "session-C", StatementID: "S1-users", Settings: map[string]string{"isolation_level": "serializable"}},
		{Seq: 12, RequestID: "req-012", SessionID: "session-D", StatementID: "S1-users", Settings: map[string]string{"work_mem_kib": "256"}},
		{Seq: 13, RequestID: "req-013", SessionID: "session-A", StatementID: "S6-events", Settings: map[string]string{}},
	}

	p.InvalidationEdits = []InvalidationEdit{
		{ID: "inv-001", Kind: "template_edit", StatementID: "S2-orders", OldValue: "SELECT ... old orders", NewValue: "SELECT ... new orders", Source: "template:S2-orders"},
		{ID: "inv-002", Kind: "param_type_edit", StatementID: "S1-users", ParameterIndex: 0, OldValue: "bigint", NewValue: "integer", Source: "param_type:S1-users#0"},
		{ID: "inv-003", Kind: "stats_version_edit", StatementID: "S4-inventory", OldValue: "stats-inventory-v3", NewValue: "stats-inventory-v4", Source: "stats:S4-inventory@v4"},
	}
	return p
}

func planNode(id, op string, costMS int64, args []int, children []string, ref *NodeRef) Node {
	return Node{ID: id, Op: op, Arguments: append([]int(nil), args...), Children: append([]string(nil), children...), Ref: ref, EstimatedCostNS: costMS * 1_000_000}
}

func mainSubplan(stmtID string, nodes []Node) Subplan {
	for i := range nodes {
		nodes[i].SubplanID = "main"
		if nodes[i].ID == "" {
			nodes[i].ID = stmtID + "-node"
		}
	}
	return Subplan{ID: "main", RootID: nodes[0].ID, Nodes: nodes}
}

func buildUsersStatement() Statement {
	nodes := []Node{
		planNode("S1-project", "PROJECT", 1, nil, []string{"S1-filter"}, nil),
		planNode("S1-filter", "FILTER", 2, []int{0, 1}, []string{"S1-index-scan"}, nil),
		planNode("S1-index-scan", "INDEX_SCAN", 3, []int{0}, []string{"S1-table-lookup"}, nil),
		planNode("S1-table-lookup", "TABLE_LOOKUP", 4, []int{0}, nil, nil),
	}
	return Statement{
		ID: "S1-users", Template: "SELECT id, display_name FROM users WHERE tenant_id = $1 AND status_code = $2",
		Params:        []ParamSpec{{Index: 0, Name: "tenant_id", Type: "bigint"}, {Index: 1, Name: "status_code", Type: "text"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S1", nodes)}, StatsVersion: "stats-users-v7",
	}
}

func buildOrdersStatement() Statement {
	nodes := []Node{
		planNode("S2-project", "PROJECT", 1, nil, []string{"S2-filter"}, nil),
		planNode("S2-filter", "FILTER", 2, []int{0, 1, 2}, []string{"S2-nested-loop"}, nil),
		planNode("S2-nested-loop", "NESTED_LOOP", 5, nil, []string{"S2-ref-users", "S2-seq-scan"}, nil),
		planNode("S2-ref-users", "SUBPLAN_REF", 1, nil, nil, &NodeRef{StatementID: "S1-users", SubplanID: "main"}),
		planNode("S2-seq-scan", "SEQ_SCAN", 6, []int{0}, nil, nil),
	}
	return Statement{
		ID: "S2-orders", Template: "SELECT o.id, o.total FROM orders o WHERE o.tenant_id = $1 AND o.state = $2 AND o.created_at >= $3",
		Params:        []ParamSpec{{Index: 0, Name: "tenant_id", Type: "bigint"}, {Index: 1, Name: "state", Type: "text"}, {Index: 2, Name: "since", Type: "timestamptz"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S2", nodes)}, StatsVersion: "stats-orders-v5",
	}
}

func buildOrderItemsStatement() Statement {
	nodes := []Node{
		planNode("S3-project", "PROJECT", 1, nil, []string{"S3-filter"}, nil),
		planNode("S3-filter", "FILTER", 2, []int{0, 1}, []string{"S3-nested-loop"}, nil),
		planNode("S3-nested-loop", "NESTED_LOOP", 4, nil, []string{"S3-ref-orders", "S3-index-scan"}, nil),
		planNode("S3-ref-orders", "SUBPLAN_REF", 1, nil, nil, &NodeRef{StatementID: "S2-orders", SubplanID: "main"}),
		planNode("S3-index-scan", "INDEX_SCAN", 3, []int{0}, nil, nil),
	}
	return Statement{
		ID: "S3-order-items", Template: "SELECT i.id, i.sku FROM order_items i WHERE i.order_id = $1 AND i.warehouse_id = $2",
		Params:        []ParamSpec{{Index: 0, Name: "order_id", Type: "bigint"}, {Index: 1, Name: "warehouse_id", Type: "integer"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S3", nodes)}, StatsVersion: "stats-items-v2",
	}
}

func buildInventoryStatement() Statement {
	nodes := []Node{
		planNode("S4-project", "PROJECT", 1, nil, []string{"S4-filter"}, nil),
		planNode("S4-filter", "FILTER", 2, []int{0, 1}, []string{"S4-hash-join"}, nil),
		planNode("S4-hash-join", "HASH_JOIN", 7, nil, []string{"S4-ref-items", "S4-scan"}, nil),
		planNode("S4-ref-items", "SUBPLAN_REF", 1, nil, nil, &NodeRef{StatementID: "S3-order-items", SubplanID: "main"}),
		planNode("S4-scan", "SEQ_SCAN", 5, []int{1}, nil, nil),
	}
	return Statement{
		ID: "S4-inventory", Template: "SELECT q.available FROM inventory q WHERE q.sku = $1 AND q.warehouse_id = $2",
		Params:        []ParamSpec{{Index: 0, Name: "sku", Type: "text"}, {Index: 1, Name: "warehouse_id", Type: "integer"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S4", nodes)}, StatsVersion: "stats-inventory-v3",
	}
}

func buildProductsStatement() Statement {
	nodes := []Node{
		planNode("S5-project", "PROJECT", 1, nil, []string{"S5-filter"}, nil),
		planNode("S5-filter", "FILTER", 2, []int{0}, []string{"S5-index-scan"}, nil),
		planNode("S5-index-scan", "INDEX_SCAN", 4, []int{0}, nil, nil),
	}
	return Statement{
		ID: "S5-products", Template: "SELECT p.name, p.price FROM products p WHERE p.category_id = $1",
		Params:        []ParamSpec{{Index: 0, Name: "category_id", Type: "integer"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S5", nodes)}, StatsVersion: "stats-products-v9",
	}
}

func buildEventsStatement() Statement {
	nodes := []Node{
		planNode("S6-project", "PROJECT", 1, nil, []string{"S6-filter"}, nil),
		planNode("S6-filter", "FILTER", 2, []int{0, 1}, []string{"S6-nested-loop"}, nil),
		planNode("S6-nested-loop", "NESTED_LOOP", 6, nil, []string{"S6-ref-inventory", "S6-index-scan"}, nil),
		planNode("S6-ref-inventory", "SUBPLAN_REF", 1, nil, nil, &NodeRef{StatementID: "S4-inventory", SubplanID: "main"}),
		planNode("S6-index-scan", "INDEX_SCAN", 3, []int{0}, nil, nil),
	}
	return Statement{
		ID: "S6-events", Template: "SELECT e.id FROM domain_events e WHERE e.aggregate_id = $1 AND e.event_type = $2",
		Params:        []ParamSpec{{Index: 0, Name: "aggregate_id", Type: "uuid"}, {Index: 1, Name: "event_type", Type: "text"}},
		MainSubplanID: "main", Subplans: []Subplan{mainSubplan("S6", nodes)}, StatsVersion: "stats-events-v1",
	}
}
