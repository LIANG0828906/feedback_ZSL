package planopt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Generated is the deterministic engineering bundle and the truth produced by
// parse construction.
type Generated struct {
	Project Project
	Truth   Truth
}

// Generate builds the fixed-seed acceptance sample. The seed is retained in the
// envelope and fingerprints; the sample itself is deliberately explicit so
// every semantic edge is easy to audit.
func Generate(seed int64) *Generated {
	tables := sampleTables()
	plans := samplePlans(seed)
	for i := range plans {
		plans[i].Fingerprint = fingerprint(seed, plans[i].ID)
	}
	proj := Project{
		SourceSchemaVersion: "generated-canonical",
		Seed:                seed,
		TickNanoseconds:     100_000,
		Tables:              tables,
		Plans:               plans,
	}
	return &Generated{Project: proj, Truth: buildConstructionTruth(proj)}
}

func fingerprint(seed int64, planID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("planopt|%d|%s", seed, planID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func col(name, typ string, ordinal int, width, ndv, nullPPM, minV, maxV int64, buckets ...Bucket) ColumnStat {
	return ColumnStat{
		Name: name, Ordinal: ordinal, TypeName: typ, WidthBytes: width, NDV: ndv,
		NullPermyriad: nullPPM, MinValue: minV, MaxValue: maxV, HistogramBuckets: buckets,
	}
}

func b(upper, rows, distinct int64) Bucket {
	return Bucket{UpperBound: upper, RowCount: rows, DistinctCount: distinct}
}

func sampleTables() []Table {
	return []Table{
		{
			Name: "regions", Ordinal: 0, RowCount: 8, RowWidthBytes: 1024,
			Columns: []ColumnStat{
				col("region_id", "INT64", 0, 8, 8, 0, 1, 8,
					b(2, 2, 2), b(4, 2, 2), b(6, 2, 2), b(8, 2, 2)),
				col("region_name", "TEXT", 1, 128, 8, 0, 0, 0),
				col("region_notes", "TEXT", 2, 512, 1, 0, 0, 0),
			},
			AccessPaths: []AccessPath{
				{Name: "pk_regions", Kind: "INDEX_SCAN", KeyColumns: []string{"region_id"}, Ordinal: 0},
				{Name: "seq_regions", Kind: "SEQ_SCAN", Ordinal: 1},
			},
		},
		{
			Name: "customers", Ordinal: 1, RowCount: 1200, RowWidthBytes: 1024,
			Columns: []ColumnStat{
				col("customer_id", "INT64", 0, 8, 1200, 0, 1, 1200, uniformBuckets(12, 100, 100, 100)...),
				col("region_id", "INT64", 1, 8, 8, 0, 1, 8,
					b(1, 150, 1), b(2, 150, 1), b(3, 150, 1), b(4, 150, 1),
					b(5, 150, 1), b(6, 150, 1), b(7, 150, 1), b(8, 150, 1)),
				col("segment", "INT64", 2, 4, 4, 0, 1, 4,
					b(1, 300, 1), b(2, 300, 1), b(3, 300, 1), b(4, 300, 1)),
				col("customer_name", "TEXT", 3, 256, 1200, 0, 0, 0),
				col("profile_note", "TEXT", 4, 512, 1200, 0, 0, 0),
			},
			AccessPaths: []AccessPath{
				{Name: "pk_customers", Kind: "INDEX_SCAN", KeyColumns: []string{"customer_id"}, Ordinal: 0},
				{Name: "idx_customer_region", Kind: "INDEX_SCAN", KeyColumns: []string{"region_id"}, Ordinal: 1},
				{Name: "seq_customers", Kind: "SEQ_SCAN", Ordinal: 2},
			},
		},
		{
			Name: "products", Ordinal: 2, RowCount: 300, RowWidthBytes: 1024,
			Columns: []ColumnStat{
				col("product_id", "INT64", 0, 8, 300, 0, 1, 300, uniformBuckets(6, 50, 50, 50)...),
				col("category_id", "INT64", 1, 4, 20, 0, 1, 20, uniformBuckets(4, 5, 75, 5)...),
				col("price", "INT64", 2, 8, 300, 0, 100, 100000,
					b(20080, 60, 60), b(40060, 60, 60), b(60040, 60, 60), b(80020, 60, 60), b(100000, 60, 60)),
				col("product_name", "TEXT", 3, 256, 300, 0, 0, 0),
				col("description", "TEXT", 4, 512, 300, 0, 0, 0),
			},
			AccessPaths: []AccessPath{
				{Name: "pk_products", Kind: "INDEX_SCAN", KeyColumns: []string{"product_id"}, Ordinal: 0},
				{Name: "seq_products", Kind: "SEQ_SCAN", Ordinal: 1},
			},
		},
		{
			Name: "orders", Ordinal: 3, RowCount: 12000, RowWidthBytes: 2048,
			Columns: []ColumnStat{
				col("order_id", "INT64", 0, 8, 12000, 0, 1, 12000, uniformBuckets(12, 1000, 1000, 1000)...),
				// Skewed first customer bucket makes binding sensitivity and
				// index crossover observable.
				col("customer_id", "INT64", 1, 8, 1200, 0, 1, 1200,
					append([]Bucket{b(100, 6000, 10)}, uniformBuckets(11, 100, 545, 100, 200)...)...),
				col("product_id", "INT64", 2, 8, 300, 0, 1, 300, uniformBuckets(6, 50, 2000, 50)...),
				col("amount", "INT64", 3, 8, 12000, 0, 100, 100000,
					b(10000, 1200, 1200), b(20000, 1200, 1200), b(40000, 3200, 3200),
					b(70000, 3600, 3600), b(100000, 2800, 2800)),
				col("status", "INT64", 4, 4, 12, 0, 1, 12, uniformBuckets(12, 1, 1000, 1)...),
				col("order_note", "TEXT", 5, 512, 12000, 0, 0, 0),
			},
			AccessPaths: []AccessPath{
				{Name: "pk_orders", Kind: "INDEX_SCAN", KeyColumns: []string{"order_id"}, Ordinal: 0},
				{Name: "idx_orders_customer", Kind: "INDEX_SCAN", KeyColumns: []string{"customer_id"}, Ordinal: 1},
				{Name: "seq_orders", Kind: "SEQ_SCAN", Ordinal: 2},
			},
		},
	}
}

// uniformBuckets creates count buckets ending at step, 2*step, ... .
func uniformBuckets(count int, step int64, rowsPerBucket, distinctPerBucket int64, firstUpper ...int64) []Bucket {
	start := int64(0)
	if len(firstUpper) > 0 {
		start = firstUpper[0] - step
	}
	out := make([]Bucket, 0, count)
	for i := 1; i <= count; i++ {
		upper := start + int64(i)*step
		out = append(out, b(upper, rowsPerBucket, distinctPerBucket))
	}
	return out
}

func ref(id string, ordinal int, table, column, node, role string) ColumnRef {
	return ColumnRef{ID: id, Ordinal: ordinal, Table: table, Column: column, NodeID: node, UsageRole: role}
}

func param(global, local int, name, typ string, value int64) Param {
	return Param{GlobalIndex: global, LocalIndex: local, Name: name, TypeName: typ, DefaultValue: value}
}

func pred(id string, ordinal int, table, column, op string, global, local int, depth int, compound string, declaredPermille int64) Predicate {
	p := Predicate{
		ID: id, Ordinal: ordinal, Table: table, Column: column, Op: op,
		NestingDepth: depth, CompoundID: compound,
		DeclaredSelPermyriad: declaredPermille * 10,
	}
	if global > 0 {
		p.HasParam = true
		p.GlobalParamIndex = global
		p.LocalParamIndex = local
	}
	return p
}

func betweenPred(id string, ordinal int, table, column, op string, global, localLow, localHigh int, depth int, compound string, declaredPermille int64) Predicate {
	p := pred(id, ordinal, table, column, op, global, localLow, depth, compound, declaredPermille)
	p.SecondGlobalParamIndex = global + 1
	p.SecondLocalParamIndex = localHigh
	p.LocalParamIndex = localLow
	return p
}

func samplePlans(seed int64) []Plan {
	_ = seed
	return []Plan{
		plan1(), plan2(), plan3(), plan4(), plan5(), plan6(),
	}
}

func plan1() Plan {
	return Plan{
		Seq: 1, ID: "plan-0001", StatementID: "stmt-0001", CommittedTick: 10,
		RootNodeID: "p1_project", RequiredColumns: []string{"orders.order_id", "customers.customer_name"},
		PriorCostMilli: 1250, AbsoluteParamBase: 0,
		Nodes: []Node{
			{ID: "p1_scan_orders", SubplanID: "sp1_main", Kind: "SCAN", Table: "orders"},
			{ID: "p1_scan_customers", SubplanID: "sp1_main", Kind: "SCAN", Table: "customers"},
			{ID: "p1_join_oc", SubplanID: "sp1_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "orders", RightTable: "customers", LeftKey: "customer_id", RightKey: "customer_id", Children: []string{"p1_scan_orders", "p1_scan_customers"}},
			{ID: "p1_filter_customer", SubplanID: "sp1_main", Kind: "FILTER", Children: []string{"p1_join_oc"}, PredicateIDs: []string{"p1_pred_customer_eq", "p1_pred_customer_eq_dup"}},
			{ID: "p1_subquery", SubplanID: "sp1_inner", Kind: "SUBQUERY", Children: []string{"p1_filter_customer"}},
			{ID: "p1_project", SubplanID: "sp1_outer", Kind: "PROJECT", Children: []string{"p1_subquery"}, ProjectionColumns: []string{"orders.order_id", "customers.customer_name"}},
		},
		ColumnRefs: []ColumnRef{
			ref("p1_ref_customer_fk", 1, "orders", "customer_id", "p1_filter_customer", "PREDICATE"),
			ref("p1_ref_order_id", 2, "orders", "order_id", "p1_project", "OUTPUT"),
			ref("p1_ref_customer_pk", 3, "customers", "customer_id", "p1_join_oc", "JOIN"),
			ref("p1_ref_customer_name", 4, "customers", "customer_name", "p1_project", "OUTPUT"),
		},
		Predicates: []Predicate{
			pred("p1_pred_customer_eq", 1, "orders", "customer_id", "EQ", 1, 1, 1, "p1_duplicate_group", 1),
			pred("p1_pred_customer_eq_dup", 2, "orders", "customer_id", "EQ", 1, 1, 2, "p1_duplicate_group", 1),
		},
		Params: []Param{param(1, 1, "customer_id", "INT64", 500)},
	}
}

func plan2() Plan {
	p := Plan{
		Seq: 2, ID: "plan-0002", StatementID: "stmt-0002", CommittedTick: 20,
		RootNodeID: "p2_project", RequiredColumns: []string{"orders.order_id", "products.product_name"},
		PriorCostMilli: 4200, AbsoluteParamBase: 1,
		Nodes: []Node{
			{ID: "p2_scan_orders", SubplanID: "sp2_main", Kind: "SCAN", Table: "orders"},
			{ID: "p2_scan_products", SubplanID: "sp2_main", Kind: "SCAN", Table: "products"},
			{ID: "p2_join_op", SubplanID: "sp2_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "orders", RightTable: "products", LeftKey: "product_id", RightKey: "product_id", Children: []string{"p2_scan_orders", "p2_scan_products"}},
			{ID: "p2_filter_amount", SubplanID: "sp2_main", Kind: "FILTER", Children: []string{"p2_join_op"}, PredicateIDs: []string{"p2_pred_amount_between", "p2_pred_amount_between_dup"}},
			{ID: "p2_project", SubplanID: "sp2_outer", Kind: "PROJECT", Children: []string{"p2_filter_amount"}, ProjectionColumns: []string{"orders.order_id", "products.product_name"}},
		},
		ColumnRefs: []ColumnRef{
			ref("p2_ref_amount", 1, "orders", "amount", "p2_filter_amount", "PREDICATE"),
			ref("p2_ref_order_id", 2, "orders", "order_id", "p2_project", "OUTPUT"),
			ref("p2_ref_order_product", 3, "orders", "product_id", "p2_join_op", "JOIN"),
			ref("p2_ref_product_id", 4, "products", "product_id", "p2_join_op", "JOIN"),
			ref("p2_ref_product_name", 5, "products", "product_name", "p2_project", "OUTPUT"),
		},
		Predicates: []Predicate{
			betweenPred("p2_pred_amount_between", 1, "orders", "amount", "BETWEEN", 2, 1, 2, 1, "p2_duplicate_group", 21),
			betweenPred("p2_pred_amount_between_dup", 2, "orders", "amount", "BETWEEN", 2, 1, 2, 2, "p2_duplicate_group", 21),
		},
		Params: []Param{
			param(2, 1, "amount_low", "INT64", 1000),
			param(3, 2, "amount_high", "INT64", 3000),
		},
	}
	return p
}

func plan3() Plan {
	return Plan{
		Seq: 3, ID: "plan-0003", StatementID: "stmt-0003", CommittedTick: 30,
		RootNodeID: "p3_project", RequiredColumns: []string{"customers.customer_name", "customers.customer_id"},
		PriorCostMilli: 3100, AbsoluteParamBase: 3,
		Nodes: []Node{
			{ID: "p3_scan_orders", SubplanID: "sp3_main", Kind: "SCAN", Table: "orders"},
			{ID: "p3_scan_customers", SubplanID: "sp3_main", Kind: "SCAN", Table: "customers"},
			{ID: "p3_join_oc", SubplanID: "sp3_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "orders", RightTable: "customers", LeftKey: "customer_id", RightKey: "customer_id", Children: []string{"p3_scan_orders", "p3_scan_customers"}},
			{ID: "p3_filter_status", SubplanID: "sp3_main", Kind: "FILTER", Children: []string{"p3_join_oc"}, PredicateIDs: []string{"p3_pred_status_eq", "p3_pred_status_eq_dup"}},
			{ID: "p3_aggregate", SubplanID: "sp3_main", Kind: "AGGREGATE", Children: []string{"p3_filter_status"}},
			{ID: "p3_project", SubplanID: "sp3_outer", Kind: "PROJECT", Children: []string{"p3_aggregate"}, ProjectionColumns: []string{"customers.customer_name", "customers.customer_id"}},
		},
		ColumnRefs: []ColumnRef{
			ref("p3_ref_status", 1, "orders", "status", "p3_filter_status", "PREDICATE"),
			ref("p3_ref_customer_name", 2, "customers", "customer_name", "p3_project", "OUTPUT"),
			ref("p3_ref_customer_id_out", 3, "customers", "customer_id", "p3_project", "OUTPUT"),
			ref("p3_ref_order_customer", 4, "orders", "customer_id", "p3_join_oc", "JOIN"),
			ref("p3_ref_customer_id_join", 5, "customers", "customer_id", "p3_join_oc", "JOIN"),
		},
		Predicates: []Predicate{
			pred("p3_pred_status_eq", 1, "orders", "status", "EQ", 4, 1, 1, "p3_status_group", 83),
			pred("p3_pred_status_eq_dup", 2, "orders", "status", "EQ", 4, 1, 2, "p3_status_group", 83),
		},
		Params: []Param{param(4, 1, "status", "INT64", 3)},
	}
}

func plan4() Plan {
	return Plan{
		Seq: 4, ID: "plan-0004", StatementID: "stmt-0004", CommittedTick: 40,
		RootNodeID: "p4_project", RequiredColumns: []string{"orders.order_id", "regions.region_name"},
		PriorCostMilli: 5200, AbsoluteParamBase: 4,
		Nodes: []Node{
			{ID: "p4_scan_orders", SubplanID: "sp4_main", Kind: "SCAN", Table: "orders"},
			{ID: "p4_scan_customers", SubplanID: "sp4_main", Kind: "SCAN", Table: "customers"},
			{ID: "p4_scan_regions", SubplanID: "sp4_main", Kind: "SCAN", Table: "regions"},
			{ID: "p4_join_oc", SubplanID: "sp4_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "orders", RightTable: "customers", LeftKey: "customer_id", RightKey: "customer_id", Children: []string{"p4_scan_orders", "p4_scan_customers"}},
			{ID: "p4_join_cr", SubplanID: "sp4_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "customers", RightTable: "regions", LeftKey: "region_id", RightKey: "region_id", Children: []string{"p4_join_oc", "p4_scan_regions"}},
			{ID: "p4_filter_region", SubplanID: "sp4_main", Kind: "FILTER", Children: []string{"p4_join_cr"}, PredicateIDs: []string{"p4_pred_region_eq", "p4_pred_region_eq_dup"}},
			{ID: "p4_subquery", SubplanID: "sp4_inner", Kind: "SUBQUERY", Children: []string{"p4_filter_region"}},
			{ID: "p4_project", SubplanID: "sp4_outer", Kind: "PROJECT", Children: []string{"p4_subquery"}, ProjectionColumns: []string{"orders.order_id", "regions.region_name"}},
		},
		ColumnRefs: []ColumnRef{
			ref("p4_ref_region_pred", 1, "customers", "region_id", "p4_filter_region", "PREDICATE"),
			ref("p4_ref_order_id", 2, "orders", "order_id", "p4_project", "OUTPUT"),
			ref("p4_ref_order_customer", 3, "orders", "customer_id", "p4_join_oc", "JOIN"),
			ref("p4_ref_customer_id", 4, "customers", "customer_id", "p4_join_oc", "JOIN"),
			ref("p4_ref_customer_region", 5, "customers", "region_id", "p4_join_cr", "JOIN"),
			ref("p4_ref_region_id", 6, "regions", "region_id", "p4_join_cr", "JOIN"),
			ref("p4_ref_region_name", 7, "regions", "region_name", "p4_project", "OUTPUT"),
		},
		Predicates: []Predicate{
			pred("p4_pred_region_eq", 1, "customers", "region_id", "EQ", 5, 1, 1, "p4_region_group", 125),
			pred("p4_pred_region_eq_dup", 2, "customers", "region_id", "EQ", 5, 1, 2, "p4_region_group", 125),
		},
		Params: []Param{param(5, 1, "region_id", "INT64", 7)},
	}
}

func plan5() Plan {
	return Plan{
		Seq: 5, ID: "plan-0005", StatementID: "stmt-0005", CommittedTick: 50,
		RootNodeID: "p5_project", RequiredColumns: []string{"orders.order_id", "customers.customer_name"},
		PriorCostMilli: 2800, AbsoluteParamBase: 5,
		Nodes: []Node{
			{ID: "p5_scan_orders", SubplanID: "sp5_main", Kind: "SCAN", Table: "orders"},
			{ID: "p5_scan_customers", SubplanID: "sp5_main", Kind: "SCAN", Table: "customers"},
			{ID: "p5_join_left", SubplanID: "sp5_main", Kind: "JOIN", JoinType: "LEFT_OUTER", LeftTable: "orders", RightTable: "customers", LeftKey: "customer_id", RightKey: "customer_id", Children: []string{"p5_scan_orders", "p5_scan_customers"}},
			{ID: "p5_filter_right", SubplanID: "sp5_main", Kind: "FILTER", Children: []string{"p5_join_left"}, PredicateIDs: []string{"p5_pred_segment_eq"}},
			{ID: "p5_project", SubplanID: "sp5_outer", Kind: "PROJECT", Children: []string{"p5_filter_right"}, ProjectionColumns: []string{"orders.order_id", "customers.customer_name"}},
		},
		ColumnRefs: []ColumnRef{
			ref("p5_ref_segment", 1, "customers", "segment", "p5_filter_right", "PREDICATE"),
			ref("p5_ref_order_id", 2, "orders", "order_id", "p5_project", "OUTPUT"),
			ref("p5_ref_order_customer", 3, "orders", "customer_id", "p5_join_left", "JOIN"),
			ref("p5_ref_customer_id", 4, "customers", "customer_id", "p5_join_left", "JOIN"),
			ref("p5_ref_customer_name", 5, "customers", "customer_name", "p5_project", "OUTPUT"),
		},
		Predicates: []Predicate{
			pred("p5_pred_segment_eq", 1, "customers", "segment", "EQ", 6, 1, 2, "", 250),
		},
		Params: []Param{param(6, 1, "segment", "INT64", 2)},
	}
}

func plan6() Plan {
	return Plan{
		Seq: 6, ID: "plan-0006", StatementID: "stmt-0006", CommittedTick: 60,
		RootNodeID: "p6_limit", RequiredColumns: []string{"orders.order_id", "products.product_name"},
		PriorCostMilli: 6100, AbsoluteParamBase: 6,
		Nodes: []Node{
			{ID: "p6_scan_orders", SubplanID: "sp6_main", Kind: "SCAN", Table: "orders"},
			{ID: "p6_scan_products", SubplanID: "sp6_main", Kind: "SCAN", Table: "products"},
			{ID: "p6_join_op", SubplanID: "sp6_main", Kind: "JOIN", JoinType: "INNER", LeftTable: "orders", RightTable: "products", LeftKey: "product_id", RightKey: "product_id", Children: []string{"p6_scan_orders", "p6_scan_products"}},
			{ID: "p6_filter_order_id", SubplanID: "sp6_main", Kind: "FILTER", Children: []string{"p6_join_op"}, PredicateIDs: []string{"p6_pred_order_gt"}},
			{ID: "p6_project", SubplanID: "sp6_outer", Kind: "PROJECT", Children: []string{"p6_filter_order_id"}, ProjectionColumns: []string{"orders.order_id", "products.product_name"}},
			{ID: "p6_limit", SubplanID: "sp6_outer", Kind: "LIMIT", Children: []string{"p6_project"}, LimitValue: 100},
		},
		ColumnRefs: []ColumnRef{
			ref("p6_ref_order_id_pred", 1, "orders", "order_id", "p6_filter_order_id", "PREDICATE"),
			ref("p6_ref_order_id_out", 2, "orders", "order_id", "p6_project", "OUTPUT"),
			ref("p6_ref_order_product", 3, "orders", "product_id", "p6_join_op", "JOIN"),
			ref("p6_ref_product_id", 4, "products", "product_id", "p6_join_op", "JOIN"),
			ref("p6_ref_product_name", 5, "products", "product_name", "p6_project", "OUTPUT"),
		},
		Predicates: []Predicate{
			pred("p6_pred_order_gt", 1, "orders", "order_id", "GT", 7, 1, 1, "", 80),
		},
		Params: []Param{param(7, 1, "min_order_id", "INT64", 11000)},
	}
}
