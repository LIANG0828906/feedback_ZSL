package thermalcache

import (
	"encoding/json"
	"testing"
)

func TestInjectedAnomaliesReturnStableCodes(t *testing.T) {
	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	before, _ := json.Marshal(p)

	tests := []struct {
		name   string
		mutate func(*Project)
		code   string
		run    bool
	}{
		{
			name: "duplicate object",
			mutate: func(p *Project) {
				dup := p.Objects[0]
				p.Objects = append(p.Objects, dup)
			},
			code: CodeDuplicateObject,
		},
		{
			name: "negative capacity",
			mutate: func(p *Project) {
				p.Nodes[0].CapacityBytes = -1
			},
			code: CodeCapacityNegative,
		},
		{
			name: "zero batch object limit",
			mutate: func(p *Project) {
				p.Limits.MaxBatchObjects = 0
			},
			code: CodeBatchLimitZero,
		},
		{
			name: "zero batch byte limit",
			mutate: func(p *Project) {
				p.Limits.MaxBatchBytes = 0
			},
			code: CodeBatchLimitZero,
		},
		{
			name: "thread budget too high",
			mutate: func(p *Project) {
				p.Limits.WorkerBudget = p.Limits.MaxWorkers + 1
			},
			code: CodeThreadBudget,
		},
		{
			name: "thread budget negative",
			mutate: func(p *Project) {
				p.Limits.WorkerBudget = -1
			},
			code: CodeThreadBudget,
		},
		{
			name: "zero layer buffer",
			mutate: func(p *Project) {
				p.Limits.PerLayerBuffer = 0
			},
			code: CodeBufferBudget,
		},
		{
			name: "migration to deleted capacity",
			mutate: func(p *Project) {
				zero := int64(0)
				p.CapacityEvents = append(p.CapacityEvents,
					CapacityEvent{AtMs: 450, NodeID: "hot-a", CapacityBytes: &zero, Delete: true})
			},
			code: CodeMigrationTarget,
			run:  true,
		},
		{
			name: "migration target node cannot carry object",
			mutate: func(p *Project) {
				p.Migrations[0].TargetNodeID = "hot-b"
			},
			code: CodeMigrationTarget,
			run:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := p.Clone()
			tc.mutate(cp)
			eng, err := NewEngine(cp)
			if err == nil && tc.run {
				_, err = eng.Run()
			}
			assertCode(t, err, tc.code)
		})
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("anomaly handling modified the source project")
	}
}

func TestUnsupportedSchemaAndObjectReferenceCodes(t *testing.T) {
	bad := []byte(`{"schema":"future/v9","projectId":"x","tickMs":1,"score":{},"limits":{},"nodes":[],"objects":[]}`)
	_, err := LoadProject(bad)
	assertCode(t, err, CodeSchemaUnsupported)

	f := fixture42(t)
	p := loadVersion(t, f, SchemaV1)
	p.AccessEvents = append(p.AccessEvents, AccessEvent{AtMs: 0, ObjectID: "missing", Count: 1})
	_, err = NewEngine(p)
	assertCode(t, err, CodeObjectNotFound)
}
