package locklab

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestThreeSchemaRoundTrip(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	want := project.CanonicalDigest()
	var first *Project
	for version := 1; version <= 3; version++ {
		data, err := EncodeProject(project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if got := loaded.CanonicalDigest(); got != want {
			t.Fatalf("v%d digest mismatch: got %s want %s", version, got, want)
		}
		if first == nil {
			first = loaded
		} else if !reflect.DeepEqual(first, loaded) {
			t.Fatalf("v%d loaded model is not deeply equal to v1", version)
		}
	}
	if !reflect.DeepEqual(project, first) {
		t.Fatalf("generator canonical project is not deeply equal to loaded v1")
	}
}

func TestLoaderRejectsAmbiguity(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	v1, err := EncodeProject(project, 1)
	if err != nil {
		t.Fatal(err)
	}
	duplicateKey := strings.Replace(string(v1), `"seed": 20261127,`, `"seed": 20261127, "seed": 1,`, 1)
	if _, err := LoadProject([]byte(duplicateKey)); ErrorCodeOf(err) != EJSONDuplicateKey {
		t.Fatalf("duplicate key: got %v", err)
	}
	ambiguousVersion := strings.Replace(string(v1), `"schema_version": 1,`, `"schema_version": 1, "SchemaVersion": 3,`, 1)
	if _, err := LoadProject([]byte(ambiguousVersion)); ErrorCodeOf(err) != ESchemaAmbiguous {
		t.Fatalf("ambiguous version: got %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(v1, &document); err != nil {
		t.Fatal(err)
	}
	transactions := document["transactions"].([]any)
	operations := transactions[0].(map[string]any)["operations"].([]any)
	operations[0].(map[string]any)["wait_deadline_ms"] = 1234
	ambiguousTimeout, _ := json.Marshal(document)
	if _, err := LoadProject(ambiguousTimeout); ErrorCodeOf(err) != ESchemaAmbiguous {
		t.Fatalf("ambiguous timeout: got %v", err)
	}
	delete(operations[0].(map[string]any), "wait_timeout_ms")
	delete(operations[0].(map[string]any), "wait_deadline_ms")
	missingTimeout, _ := json.Marshal(document)
	if _, err := LoadProject(missingTimeout); ErrorCodeOf(err) != EFieldRequired {
		t.Fatalf("missing timeout: got %v", err)
	}

	v2, _ := EncodeProject(project, 2)
	document = nil
	if err := json.Unmarshal(v2, &document); err != nil {
		t.Fatal(err)
	}
	transactions = document["transactions"].([]any)
	operations = transactions[0].(map[string]any)["operations"].([]any)
	operations[0].(map[string]any)["atSeconds"] = 0.0000001
	imprecise, _ := json.Marshal(document)
	if _, err := LoadProject(imprecise); ErrorCodeOf(err) != EUnitAmbiguous {
		t.Fatalf("imprecise seconds: got %v", err)
	}
}

func TestV3CompatibilityTreeMustMatchCanonicalRelation(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	v3, err := EncodeProject(project, 3)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(v3), `"Compatible": true`, `"Compatible": false`, 1)
	if _, err := LoadProject([]byte(mutated)); ErrorCodeOf(err) != ELockModeCompatMismatch {
		t.Fatalf("want compatibility mismatch, got %v", err)
	}
}

func TestTimeoutRepresentationsNormalizeEqually(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	v1, _ := EncodeProject(project, 1)
	v2, _ := EncodeProject(project, 2)
	p1, err := LoadProject(v1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := LoadProject(v2)
	if err != nil {
		t.Fatal(err)
	}
	for tx := range p1.Transactions {
		for op := range p1.Transactions[tx].Ops {
			a, b := p1.Transactions[tx].Ops[op], p2.Transactions[tx].Ops[op]
			if a.WaitTimeoutMs != b.WaitTimeoutMs || a.DeadlineMs() != b.DeadlineMs() {
				t.Fatalf("timeout mismatch at %s/%d: %d/%d vs %d/%d",
					p1.Transactions[tx].ID, op, a.WaitTimeoutMs, a.DeadlineMs(),
					b.WaitTimeoutMs, b.DeadlineMs())
			}
		}
	}
}

func TestLockCompatibilityTableAndRowHierarchy(t *testing.T) {
	base := HeldLock{TxID: "T1", Table: "t", Resource: "row:t/a", ItemID: "t/a", Granularity: RowGranularity}
	left := base
	left.Mode = ModeS
	right := HeldLock{TxID: "T2", Table: "t", Resource: "table:t", Granularity: TableGranularity, Mode: ModeIX}
	if HeldLocksConflict(left, right) {
		t.Fatalf("table IX must not directly conflict with descendant row S")
	}
	right.Mode = ModeX
	if !HeldLocksConflict(left, right) {
		t.Fatalf("table X must conflict with row S")
	}
	right.Mode = ModeS
	right.Resource = "row:t/a"
	right.ItemID = "t/a"
	right.Granularity = RowGranularity
	right.TxID = "T2"
	right.Mode = ModeX
	if !HeldLocksConflict(left, right) {
		t.Fatalf("row S/X must conflict on the same item")
	}
	otherRow := right
	otherRow.ItemID = "t/b"
	otherRow.Resource = "row:t/b"
	if HeldLocksConflict(left, otherRow) {
		t.Fatalf("row locks on different items must not conflict")
	}
}

func TestOptionalDeadlockFlagAbsenceMeansFalse(t *testing.T) {
	options := DefaultGenerateOptions()
	options.DeadlockDetection = false
	project := GenerateProject(11, options)
	for _, version := range []int{2, 3} {
		data, err := EncodeProject(project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if loaded.DeadlockDetection {
			t.Fatalf("v%d absent optional deadlock flag loaded as true", version)
		}
	}
}
