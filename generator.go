package queryopt

import (
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 20261127

type Sample struct {
	Seed     int64
	Project  *Project
	Wire     map[string][]byte
	Truth    *Truth
	Bindings BindingSet
}

func GenerateSample(seed int64) (*Sample, error) {
	rng := rand.New(rand.NewSource(seed))
	auditRows := int64(4_000 + rng.Intn(2_000)) // deterministic for a fixed seed

	p := &Project{
		SchemaVersion: SchemaV3,
		SnapshotKind:  "queryopt-engineering-sample",
		Seed:          seed,
		Weights: CostWeights{
			ScanPermyriad:         5_000,
			IntermediatePermyriad: 3_000,
			WidthPermyriad:        2_000,
		},
		Rules: DecisionRules{TieRelativePermyriad: 100, WorkerBudget: 4, CandidateBuffer: 64},
	}

	addTable := func(id, name string, rows int64, order int) *Table {
		t := Table{
			ID: id, Name: name, Rows: rows, NullRows: 0,
			SampleLatencyMS: int64(4 + order*3), AllocationBytes: 1024 * 1024,
			DeclaredOrder: order,
		}
		p.Tables = append(p.Tables, t)
		return &p.Tables[len(p.Tables)-1]
	}
	addCol := func(id, table, name string, width, ndv int64, order int) *Column {
		c := Column{ID: id, TableID: table, Name: name, WidthBytes: width, NDV: ndv, NullPermyriad: 0, DeclaredOrder: order}
		p.Columns = append(p.Columns, c)
		return &p.Columns[len(p.Columns)-1]
	}
	addBuckets := func(col string, defs ...[5]int64) {
		for i, d := range defs {
			p.Buckets = append(p.Buckets, Bucket{
				ID: fmt.Sprintf("bkt_%s_%02d", col, i+1), ColumnID: col, BucketIndex: i,
				Lower: d[0], Upper: d[1], Rows: d[2], Distinct: d[3],
			})
		}
	}
	addPred := func(pred Predicate) {
		if pred.WeightPermyriad == 0 {
			pred.WeightPermyriad = 1_000
		}
		p.Predicates = append(p.Predicates, pred)
	}
	addEdge := func(e JoinEdge) { p.Edges = append(p.Edges, e) }
	addQuery := func(q Query) { p.Queries = append(p.Queries, q) }

	addTable("tbl_customer", "customer", 10_000, 0)
	addTable("tbl_orders", "orders", 200_000, 1)
	addTable("tbl_lineitem", "lineitem", 800_000, 2)
	addTable("tbl_account", "account", 1_000, 3)
	addTable("tbl_txn", "txn", 500, 4)
	addTable("tbl_audit", "audit_log", auditRows, 5)

	addCol("col_cust_id", "tbl_customer", "c_custkey", 8, 10_000, 0)
	addCol("col_cust_region", "tbl_customer", "c_region_id", 4, 10, 1)
	addCol("col_cust_vip", "tbl_customer", "c_vip", 1, 2, 2)
	addBuckets("col_cust_region",
		[5]int64{0, 5, 5_000, 5},
		[5]int64{5, 10, 5_000, 5},
	)

	addCol("col_order_id", "tbl_orders", "o_orderkey", 8, 200_000, 0)
	addCol("col_order_cust", "tbl_orders", "o_custkey", 8, 10_000, 1)
	addCol("col_order_date", "tbl_orders", "o_orderdate", 4, 30, 2)
	addCol("col_order_total", "tbl_orders", "o_totalprice", 8, 180_000, 3)
	addBuckets("col_order_date",
		[5]int64{1, 10, 60_000, 9},
		[5]int64{10, 20, 70_000, 10},
		[5]int64{20, 31, 70_000, 11},
	)

	addCol("col_line_id", "tbl_lineitem", "l_linenumber", 4, 4, 0)
	addCol("col_line_order", "tbl_lineitem", "l_orderkey", 8, 200_000, 1)
	addCol("col_line_part", "tbl_lineitem", "l_partkey", 8, 120_000, 2)
	addCol("col_line_shipdate", "tbl_lineitem", "l_shipdate", 4, 30, 3)
	addBuckets("col_line_shipdate",
		[5]int64{1, 10, 240_000, 9},
		[5]int64{10, 20, 280_000, 10},
		[5]int64{20, 31, 280_000, 11},
	)

	addCol("col_acct_id", "tbl_account", "a_account_id", 8, 1_000, 0)
	addCol("col_acct_skew", "tbl_account", "a_skew_key", 8, 100, 1)
	addCol("col_acct_payload", "tbl_account", "a_payload", 128, 900, 2)
	addBuckets("col_acct_skew",
		[5]int64{1, 2, 900, 1},
		[5]int64{2, 101, 100, 99},
	)

	addCol("col_txn_id", "tbl_txn", "t_txn_id", 8, 500, 0)
	addCol("col_txn_acct", "tbl_txn", "t_account_id", 8, 500, 1)
	addCol("col_audit_id", "tbl_audit", "au_id", 8, auditRows, 0)
	addCol("col_audit_tag", "tbl_audit", "au_tag", 4, 20, 1)
	addBuckets("col_audit_tag", [5]int64{0, 20, auditRows, 20})

	// Q1: equality + placeholder range + exact bucket boundary, nested deeply.
	addQuery(Query{
		ID: "q1_sales_slice", Ordinal: 1, Name: "sales slice",
		TableIDs:         []string{"tbl_customer", "tbl_orders", "tbl_lineitem"},
		RootPredicateIDs: []string{"pred_q1_deep"},
		RequiredColumnIDs: []string{
			"col_cust_id", "col_cust_region", "col_order_id", "col_order_cust", "col_order_date",
			"col_line_order", "col_line_shipdate",
		},
		PlaceholderBase: 0, PlaceholderCount: 2, IndexThresholdRows: 500,
	})
	addPred(Predicate{ID: "pred_q1_region", QueryID: "q1_sales_slice", Kind: PredEquality,
		TableID: "tbl_customer", ColumnID: "col_cust_region", Op: "=",
		Value: PredicateArg{Literal: int64Ptr(7)}, DeclaredOrder: 10})
	addPred(Predicate{ID: "pred_q1_date_range", QueryID: "q1_sales_slice", Kind: PredRange,
		TableID: "tbl_orders", ColumnID: "col_order_date", Op: "BETWEEN",
		Low: PredicateArg{PlaceholderGlobal: 1}, High: PredicateArg{PlaceholderGlobal: 2},
		InclusiveLow: true, InclusiveHigh: false, DeclaredOrder: 11})
	addPred(Predicate{ID: "pred_q1_boundary", QueryID: "q1_sales_slice", Kind: PredBoundary,
		TableID: "tbl_lineitem", ColumnID: "col_line_shipdate", Op: "AT_BUCKET_BOUNDARY",
		Value: PredicateArg{Literal: int64Ptr(10)}, DeclaredOrder: 12})
	addPred(Predicate{ID: "pred_q1_and_1", QueryID: "q1_sales_slice", Kind: PredAnd, Logic: PredAnd,
		Children: []string{"pred_q1_region", "pred_q1_date_range"}, DeclaredOrder: 13})
	addPred(Predicate{ID: "pred_q1_and_2", QueryID: "q1_sales_slice", Kind: PredAnd, Logic: PredAnd,
		Children: []string{"pred_q1_and_1", "pred_q1_boundary"}, DeclaredOrder: 14})
	addPred(Predicate{ID: "pred_q1_deep", QueryID: "q1_sales_slice", Kind: PredAnd, Logic: PredAnd,
		Children: []string{"pred_q1_and_2"}, DeclaredOrder: 15})
	addPred(Predicate{ID: "pred_join_q1_co", QueryID: "q1_sales_slice", Kind: PredJoin,
		LeftColumnID: "col_cust_id", RightColumnID: "col_order_cust", Op: "=", DeclaredOrder: 1})
	addPred(Predicate{ID: "pred_join_q1_ol", QueryID: "q1_sales_slice", Kind: PredJoin,
		LeftColumnID: "col_order_id", RightColumnID: "col_line_order", Op: "=", DeclaredOrder: 2})
	addEdge(JoinEdge{ID: "edge_q1_co", QueryID: "q1_sales_slice", LeftTableID: "tbl_customer",
		RightTableID: "tbl_orders", PredicateID: "pred_join_q1_co", DeclaredOrder: 0, WeightPermyriad: 10_000})
	addEdge(JoinEdge{ID: "edge_q1_ol", QueryID: "q1_sales_slice", LeftTableID: "tbl_orders",
		RightTableID: "tbl_lineitem", PredicateID: "pred_join_q1_ol", DeclaredOrder: 1, WeightPermyriad: 10_000})

	// Q2: parameter sensitivity is engineered around a skewed key.
	addQuery(Query{
		ID: "q2_account_txn", Ordinal: 2, Name: "account transactions",
		TableIDs:          []string{"tbl_account", "tbl_txn"},
		RootPredicateIDs:  []string{"pred_q2_skew"},
		RequiredColumnIDs: []string{"col_acct_id", "col_acct_skew", "col_txn_acct", "col_txn_id"},
		PlaceholderBase:   2, PlaceholderCount: 1, IndexThresholdRows: 100,
	})
	addPred(Predicate{ID: "pred_q2_skew", QueryID: "q2_account_txn", Kind: PredEquality,
		TableID: "tbl_account", ColumnID: "col_acct_skew", Op: "=",
		Value: PredicateArg{PlaceholderGlobal: 3}, DeclaredOrder: 20})
	addPred(Predicate{ID: "pred_join_q2_at", QueryID: "q2_account_txn", Kind: PredJoin,
		LeftColumnID: "col_acct_id", RightColumnID: "col_txn_acct", Op: "=", DeclaredOrder: 1})
	addEdge(JoinEdge{ID: "edge_q2_at", QueryID: "q2_account_txn", LeftTableID: "tbl_account",
		RightTableID: "tbl_txn", PredicateID: "pred_join_q2_at", DeclaredOrder: 0, WeightPermyriad: 10_000})

	// Q3: genuine cycle; edge_q3_co has the largest break priority.
	addQuery(Query{ID: "q3_cycle", Ordinal: 3, Name: "cyclic customer orders lineitem",
		TableIDs: []string{"tbl_customer", "tbl_orders", "tbl_lineitem"}, IndexThresholdRows: 500,
		RequiredColumnIDs: []string{"col_cust_id", "col_order_cust", "col_order_id", "col_line_order"}})
	for _, e := range []JoinEdge{
		{ID: "edge_q3_co", QueryID: "q3_cycle", LeftTableID: "tbl_customer", RightTableID: "tbl_orders",
			PredicateID: "pred_join_q1_co", BreakPriority: 90, DeclaredOrder: 0, WeightPermyriad: 10_000},
		{ID: "edge_q3_ol", QueryID: "q3_cycle", LeftTableID: "tbl_orders", RightTableID: "tbl_lineitem",
			PredicateID: "pred_join_q1_ol", BreakPriority: 50, DeclaredOrder: 1, WeightPermyriad: 10_000},
		{ID: "edge_q3_lc", QueryID: "q3_cycle", LeftTableID: "tbl_lineitem", RightTableID: "tbl_customer",
			PredicateID: "pred_join_q1_co", BreakPriority: 10, DeclaredOrder: 2, WeightPermyriad: 10_000},
	} {
		addEdge(e)
	}

	// Q4: valid path plus a cartesian shortcut.
	addQuery(Query{ID: "q4_cartesian", Ordinal: 4, Name: "cartesian shortcut",
		TableIDs: []string{"tbl_customer", "tbl_orders", "tbl_lineitem"}, IndexThresholdRows: 500,
		RequiredColumnIDs: []string{"col_cust_id", "col_order_cust", "col_order_id", "col_line_order"}})
	addEdge(JoinEdge{ID: "edge_q4_co", QueryID: "q4_cartesian", LeftTableID: "tbl_customer",
		RightTableID: "tbl_orders", PredicateID: "pred_join_q1_co", DeclaredOrder: 0, WeightPermyriad: 10_000})
	addEdge(JoinEdge{ID: "edge_q4_ol", QueryID: "q4_cartesian", LeftTableID: "tbl_orders",
		RightTableID: "tbl_lineitem", PredicateID: "pred_join_q1_ol", DeclaredOrder: 1, WeightPermyriad: 10_000})
	addEdge(JoinEdge{ID: "edge_q4_cross_cl", QueryID: "q4_cartesian", LeftTableID: "tbl_customer",
		RightTableID: "tbl_lineitem", Cartesian: true, BreakPriority: 100, DeclaredOrder: 2, WeightPermyriad: 0})

	// Q5: connected A/T component plus unreachable audit table/predicate.
	addQuery(Query{ID: "q5_unreachable", Ordinal: 5, Name: "unreachable audit",
		TableIDs:           []string{"tbl_account", "tbl_txn", "tbl_audit"},
		RootPredicateIDs:   []string{"pred_q5_audit_tag"},
		RequiredColumnIDs:  []string{"col_acct_id", "col_txn_acct", "col_audit_tag"},
		IndexThresholdRows: 100})
	addPred(Predicate{ID: "pred_q5_audit_tag", QueryID: "q5_unreachable", Kind: PredEquality,
		TableID: "tbl_audit", ColumnID: "col_audit_tag", Op: "=",
		Value: PredicateArg{Literal: int64Ptr(3)}, DeclaredOrder: 30})
	addEdge(JoinEdge{ID: "edge_q5_at", QueryID: "q5_unreachable", LeftTableID: "tbl_account",
		RightTableID: "tbl_txn", PredicateID: "pred_join_q2_at", DeclaredOrder: 0, WeightPermyriad: 10_000})

	if err := ValidateProject(p); err != nil {
		return nil, err
	}

	wire := map[string][]byte{}
	for _, v := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, err := EncodeProject(p, v)
		if err != nil {
			return nil, err
		}
		wire[v] = data
	}

	baseline := BindingSet{Global: map[int]int64{1: 5, 2: 25, 3: 50}}
	truth, err := BuildTruth(p, baseline)
	if err != nil {
		return nil, err
	}
	return &Sample{Seed: seed, Project: p, Wire: wire, Truth: truth, Bindings: baseline}, nil
}

func int64Ptr(v int64) *int64 { return &v }
