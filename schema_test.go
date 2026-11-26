package queryopt

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func loadedSample(t *testing.T) (*Sample, map[string]*Project) {
	t.Helper()
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatalf("generate sample: %v", err)
	}
	loaded := map[string]*Project{}
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		p, err := LoadProject(version, sample.Wire[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		loaded[version] = p
	}
	return sample, loaded
}

func idsTable(p *Project) []string {
	var out []string
	for _, x := range p.Tables {
		out = append(out, x.ID)
	}
	return out
}
func idsPred(p *Project) []string {
	var out []string
	for _, x := range p.Predicates {
		out = append(out, x.ID)
	}
	return out
}
func idsEdge(p *Project) []string {
	var out []string
	for _, x := range p.Edges {
		out = append(out, x.ID)
	}
	return out
}

func TestThreeSchemaVersionsLoadSameSetsAndOrders(t *testing.T) {
	sample, loaded := loadedSample(t)
	base := loaded[SchemaV3]
	for _, version := range []string{SchemaV1, SchemaV2} {
		p := loaded[version]
		if !reflect.DeepEqual(idsTable(base), idsTable(p)) {
			t.Fatalf("%s table ids mismatch: %v vs %v", version, idsTable(p), idsTable(base))
		}
		if !reflect.DeepEqual(idsPred(base), idsPred(p)) {
			t.Fatalf("%s predicate ids mismatch: %v vs %v", version, idsPred(p), idsPred(base))
		}
		if !reflect.DeepEqual(idsEdge(base), idsEdge(p)) {
			t.Fatalf("%s edge ids mismatch: %v vs %v", version, idsEdge(p), idsEdge(base))
		}

		_, plans, err := FullRecompute(p, sample.Bindings, OptionsFromProject(p))
		if err != nil {
			t.Fatalf("%s full recompute: %v", version, err)
		}
		for _, tq := range sample.Truth.Queries {
			plan := plans[tq.QueryID]
			if plan == nil {
				t.Fatalf("%s missing plan %s", version, tq.QueryID)
			}
			if tq.ExpectedErrorCode != "" {
				if plan.ErrorCode != tq.ExpectedErrorCode {
					t.Fatalf("%s %s error=%s want %s", version, tq.QueryID, plan.ErrorCode, tq.ExpectedErrorCode)
				}
				continue
			}
			if !reflect.DeepEqual(plan.SelectedOrder, tq.SelectedOrder) {
				t.Fatalf("%s %s order=%v want truth %v", version, tq.QueryID, plan.SelectedOrder, tq.SelectedOrder)
			}
			if plan.RulingBasisID != tq.RulingBasisID {
				t.Fatalf("%s %s basis=%s want %s", version, tq.QueryID, plan.RulingBasisID, tq.RulingBasisID)
			}
			for _, tc := range tq.Candidates {
				var got *CandidateResult
				for i := range plan.Candidates {
					if plan.Candidates[i].CandidateID == tc.CandidateID {
						got = &plan.Candidates[i]
						break
					}
				}
				if got == nil {
					t.Fatalf("%s %s missing candidate %s", version, tq.QueryID, tc.CandidateID)
				}
				if got.Feasible != tc.Feasible || got.RejectCode != tc.RejectCode || got.Cost.TotalCost != tc.TotalCost {
					t.Fatalf("%s %s candidate %s mismatch got=(feas=%v reject=%s cost=%d) want=(feas=%v reject=%s cost=%d)",
						version, tq.QueryID, tc.CandidateID, got.Feasible, got.RejectCode, got.Cost.TotalCost,
						tc.Feasible, tc.RejectCode, tc.TotalCost)
				}
			}
		}
	}
}

func TestSchemaUnitScaleAndPlaceholderMapping(t *testing.T) {
	sample, loaded := loadedSample(t)
	p1 := loaded[SchemaV1]
	cust := p1.Index().Tables["tbl_customer"]
	if cust.SampleLatencyMS != 4 {
		t.Fatalf("tick mapping latency=%d ms, want 4", cust.SampleLatencyMS)
	}
	if cust.AllocationBytes != 1024*1024 {
		t.Fatalf("v1 bytes allocation=%d", cust.AllocationBytes)
	}
	var w1 wireV1
	if err := json.Unmarshal(sample.Wire[SchemaV1], &w1); err != nil {
		t.Fatal(err)
	}
	if w1.Tables[0].LatencyTicks != 4*TicksPerMillisecond {
		t.Fatalf("v1 latency ticks=%d", w1.Tables[0].LatencyTicks)
	}

	p2 := loaded[SchemaV2]
	if got := p2.Index().Tables["tbl_customer"].AllocationBytes; got != 1024*1024 {
		t.Fatalf("KiB mapping bytes=%d", got)
	}
	var w2 wireV2
	if err := json.Unmarshal(sample.Wire[SchemaV2], &w2); err != nil {
		t.Fatal(err)
	}
	if w2.Tables[0].AllocationKiB != 1024 {
		t.Fatalf("v2 allocation KiB=%d", w2.Tables[0].AllocationKiB)
	}
	if w2.Weights.ScanPermyriad != 5000 || w1.Weights.ScanPermille != 500 {
		t.Fatalf("weight scale mapping v1=%+v v2=%+v", w1.Weights, w2.Weights)
	}

	rel, err := RelativeFromGlobal(sample.Project, sample.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	globalAgain, err := GlobalFromRelative(sample.Project, rel)
	if err != nil {
		t.Fatal(err)
	}
	if !sameBindings(globalAgain, sample.Bindings) {
		t.Fatalf("relative/global binding mismatch: %#v vs %#v", globalAgain, sample.Bindings)
	}
	if rel["q2_account_txn"][0] != 50 {
		t.Fatalf("q2 relative binding=%#v", rel["q2_account_txn"])
	}
}

func TestSelectivityMatchesFixedPointTruth(t *testing.T) {
	sample, _ := loadedSample(t)
	est, err := EstimateProject(sample.Project, sample.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, truth := range sample.Truth.Predicates {
		got := est.Predicates[truth.PredicateID]
		rawDelta := int64(math.Abs(float64(got.SelectivityRaw - truth.SelectivityRaw)))
		if rawDelta > sample.Truth.PredicateToleranceRaw {
			t.Fatalf("%s raw delta=%d > %d (got=%d truth=%d)", truth.PredicateID, rawDelta,
				sample.Truth.PredicateToleranceRaw, got.SelectivityRaw, truth.SelectivityRaw)
		}
		rowDelta := int64(math.Abs(float64(got.EstimatedRows - truth.EstimatedRows)))
		rowTolerance := truth.BaseRows*sample.Truth.PredicateToleranceRaw/FixedScale + sample.Truth.RowTolerance
		if rowDelta > rowTolerance {
			t.Fatalf("%s row delta=%d > %d (got=%d truth=%d)", truth.PredicateID, rowDelta,
				rowTolerance, got.EstimatedRows, truth.EstimatedRows)
		}
	}

	boundary := est.Predicates["pred_q1_boundary"]
	if !boundary.BoundaryHit || boundary.BoundaryBucketID == "" {
		t.Fatalf("boundary estimate did not identify exact bucket: %+v", boundary)
	}
	deep := est.Predicates["pred_q1_deep"]
	if deep.Saturated {
		t.Fatalf("deep nested predicate unexpectedly saturated: %+v", deep)
	}
}

func TestFixedOverflowAndSaturationAreExplicit(t *testing.T) {
	if _, overflow := FixedFromPermille(1001); !overflow {
		t.Fatal("permille overflow not reported")
	}
	v, overflow := Fixed(1 << 62).Mul(Fixed(1 << 40))
	if !overflow || v != FixedMax {
		t.Fatalf("fixed mul overflow v=%d overflow=%v", v, overflow)
	}
	rows, saturated := RoundedRows(math.MaxInt64, FixedOne*2)
	if !saturated || rows != math.MaxInt64 {
		t.Fatalf("row saturation rows=%d sat=%v", rows, saturated)
	}
}
