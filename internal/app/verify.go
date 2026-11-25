package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/gate"
	"maintenanceorchestrator/internal/lease"
	"maintenanceorchestrator/internal/schema"
	"maintenanceorchestrator/internal/solver"
	"maintenanceorchestrator/internal/truth"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type VerifyReport struct {
	OK       bool     `json:"ok"`
	Checks   []Check  `json:"checks"`
	Analysis *Summary `json:"analysis,omitempty"`
}

type Summary struct {
	ProjectID        string `json:"project_id"`
	Nodes            int    `json:"nodes"`
	Instances        int    `json:"instances"`
	Waves            int    `json:"waves"`
	Migrations       int    `json:"migrations"`
	Spillovers       int    `json:"spillovers"`
	LeaseRecalls     int    `json:"lease_recalls"`
	Degradations     int    `json:"degradations"`
	GateNodesReady   int    `json:"gate_nodes_ready"`
	GateNodesBlocked int    `json:"gate_nodes_blocked"`
}

func VerifyDirectory(dir string) (*VerifyReport, error) {
	r := &VerifyReport{OK: true}
	v1, err := schema.LoadFile(filepath.Join(dir, "project.v1.json"))
	if err != nil {
		return failReport(r, "load v1", err), err
	}
	v2, err := schema.LoadFile(filepath.Join(dir, "project.v2.json"))
	if err != nil {
		return failReport(r, "load v2", err), err
	}
	v3, err := schema.LoadFile(filepath.Join(dir, "project.v3.json"))
	if err != nil {
		return failReport(r, "load v3", err), err
	}

	if err := checkCanonicalEquivalence(v1, v2, v3); err != nil {
		r.Checks = append(r.Checks, Check{Name: "three_schema_canonical_equivalence", OK: false, Detail: err.Error()})
		r.OK = false
	} else {
		r.Checks = append(r.Checks, Check{Name: "three_schema_canonical_equivalence", OK: true})
	}

	truthData, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return failReport(r, "read truth", err), err
	}
	var expected truth.Truth
	if err := json.Unmarshal(truthData, &expected); err != nil {
		return failReport(r, "decode truth", err), err
	}

	a1, err := Analyze(*v1)
	if err != nil {
		return failReport(r, "analyze v1", err), err
	}
	a2, err := Analyze(*v2)
	if err != nil {
		return failReport(r, "analyze v2", err), err
	}
	a3, err := Analyze(*v3)
	if err != nil {
		return failReport(r, "analyze v3", err), err
	}
	if err := checkAnalysisEquivalence(a1, a2, a3); err != nil {
		r.Checks = append(r.Checks, Check{Name: "three_schema_analysis_equivalence", OK: false, Detail: err.Error()})
		r.OK = false
	} else {
		r.Checks = append(r.Checks, Check{Name: "three_schema_analysis_equivalence", OK: true})
	}

	checkTruth(r, a1, &expected)
	checkParallelDeterminism(r, a1)
	r.Analysis = summarize(a1)
	return r, nil
}

func failReport(r *VerifyReport, name string, err error) *VerifyReport {
	r.OK = false
	r.Checks = append(r.Checks, Check{Name: name, OK: false, Detail: err.Error()})
	return r
}

func canonicalJSON(p *domain.Project) ([]byte, error) {
	cp := domain.CloneProject(*p)
	cp.SchemaVersion = ""
	return json.Marshal(cp)
}

func checkCanonicalEquivalence(ps ...*domain.Project) error {
	first, err := canonicalJSON(ps[0])
	if err != nil {
		return err
	}
	for i, p := range ps[1:] {
		data, err := canonicalJSON(p)
		if err != nil {
			return err
		}
		if !bytes.Equal(first, data) {
			return fmt.Errorf("schema %d canonical form differs from v1", i+2)
		}
	}
	return nil
}

func analysisFingerprint(a *Analysis) ([]byte, error) {
	return json.Marshal(struct {
		Plan  any `json:"plan"`
		Lease any `json:"lease"`
		Gate  any `json:"gate"`
	}{Plan: a.Plan, Lease: a.LeaseTimeline, Gate: a.Gate})
}

func checkAnalysisEquivalence(as ...*Analysis) error {
	first, err := analysisFingerprint(as[0])
	if err != nil {
		return err
	}
	for i, a := range as[1:] {
		data, err := analysisFingerprint(a)
		if err != nil {
			return err
		}
		if !bytes.Equal(first, data) {
			return fmt.Errorf("analysis for schema %d differs", i+2)
		}
	}
	return nil
}

func checkTruth(r *VerifyReport, a *Analysis, want *truth.Truth) {
	for _, tt := range want.Ticks {
		report := findTickReport(a.Feasibility, tt.Tick)
		if report == nil {
			addFail(r, fmt.Sprintf("truth tick %d report", tt.Tick), "missing feasibility report")
			continue
		}
		gotPending := append([]string(nil), report.PendingNodes...)
		sort.Strings(gotPending)
		if !equalStrings(gotPending, tt.MarkedNodes) {
			addFail(r, fmt.Sprintf("tick %d marked nodes", tt.Tick), fmt.Sprintf("got %v want %v", gotPending, tt.MarkedNodes))
		}
		eventPending := EventPendingAt(a.Project, tt.Tick)
		sort.Strings(eventPending)
		if !equalStrings(eventPending, tt.EventPendingNodes) {
			addFail(r, fmt.Sprintf("tick %d event pending", tt.Tick), fmt.Sprintf("got %v want %v", eventPending, tt.EventPendingNodes))
		}
		leaseTick := findLeaseTick(a.LeaseTimeline.Ticks, tt.Tick)
		if leaseTick == nil {
			addFail(r, fmt.Sprintf("tick %d leases", tt.Tick), "missing lease tick report")
		} else {
			var activeIDs []string
			for _, l := range leaseTick.ActiveLeases {
				activeIDs = append(activeIDs, l.ID)
			}
			sort.Strings(activeIDs)
			if !equalStrings(activeIDs, tt.ActiveLeaseIDs) {
				addFail(r, fmt.Sprintf("tick %d active leases", tt.Tick), fmt.Sprintf("got %v want %v", activeIDs, tt.ActiveLeaseIDs))
			}
			if !reflect.DeepEqual(leaseTick.BorrowedByBorrower, tt.BorrowedByBorrower) {
				addFail(r, fmt.Sprintf("tick %d borrowed amounts", tt.Tick),
					fmt.Sprintf("got %v want %v", leaseTick.BorrowedByBorrower, tt.BorrowedByBorrower))
			}
		}
		for _, nt := range tt.Feasibility {
			nf := findNodeReport(report.Nodes, nt.NodeID)
			if nf == nil {
				addFail(r, fmt.Sprintf("tick %d node %s", tt.Tick, nt.NodeID), "missing feasibility node")
				continue
			}
			if nf.EvacuableCount != nt.EvacuableCount {
				addFail(r, fmt.Sprintf("tick %d node %s evacuable", tt.Tick, nt.NodeID),
					fmt.Sprintf("got %d want %d", nf.EvacuableCount, nt.EvacuableCount))
			}
			for _, it := range nt.Instances {
				got := findDecision(nf.Decisions, it.InstanceID)
				if got == nil {
					addFail(r, "decision "+it.InstanceID, "missing")
					continue
				}
				if got.Movable != it.Movable {
					addFail(r, "movable "+it.InstanceID, fmt.Sprintf("got %v want %v", got.Movable, it.Movable))
				}
				if it.TargetNodeID != "" && got.SelectedTargetID != it.TargetNodeID {
					addFail(r, "target "+it.InstanceID, fmt.Sprintf("got %q want %q", got.SelectedTargetID, it.TargetNodeID))
				}
				if it.ReasonCode != "" && got.ReasonCode != it.ReasonCode {
					addFail(r, "reason "+it.InstanceID, fmt.Sprintf("got %q want %q", got.ReasonCode, it.ReasonCode))
				}
				if it.ConstraintID != "" && got.ConstraintID != it.ConstraintID {
					addFail(r, "constraint "+it.InstanceID, fmt.Sprintf("got %q want %q", got.ConstraintID, it.ConstraintID))
				}
			}
		}
	}
	r.Checks = append(r.Checks, Check{Name: "truth_feasibility_ticks", OK: !hasFailureSince(r, "truth_feasibility_ticks")})

	for _, w := range want.Waves {
		wr := findWave(a.Plan.Waves, w.Wave)
		if wr == nil {
			addFail(r, fmt.Sprintf("wave %d", w.Wave), "missing")
			continue
		}
		if wr.StartTick != w.Start || wr.EndTick != w.End {
			addFail(r, fmt.Sprintf("wave %d timing", w.Wave), fmt.Sprintf("got %d-%d want %d-%d", wr.StartTick, wr.EndTick, w.Start, w.End))
		}
		for _, mt := range w.Migrations {
			m := findMigration(a.Plan.Migrations, mt.InstanceID)
			if m == nil {
				addFail(r, "migration "+mt.InstanceID, "missing")
				continue
			}
			if m.TargetNodeID != mt.Target || m.Wave != mt.Wave || m.StartTick != mt.Start || m.EndTick != mt.End {
				addFail(r, "migration "+mt.InstanceID, fmt.Sprintf("got %+v want target=%s wave=%d %d-%d", m, mt.Target, mt.Wave, mt.Start, mt.End))
			}
		}
		for _, st := range w.Spillovers {
			if !hasSpill(a.Plan.Spillovers, st) {
				addFail(r, "spill "+st.InstanceID, "expected spillover not found")
			}
		}
	}
	r.Checks = append(r.Checks, Check{Name: "truth_waves", OK: true})

	checkLeaseTruth(r, a, want)
	checkGateTruth(r, a, want)
}

func checkLeaseTruth(r *VerifyReport, a *Analysis, want *truth.Truth) {
	for _, ev := range want.LeaseEvents {
		if ev.Kind == "recalled" {
			got := findLeaseEvent(a.LeaseTimeline.Recalls, ev.Tick, ev.LeaseID)
			if got == nil || (ev.ReasonCode != "" && got.ReasonCode != ev.ReasonCode) {
				addFail(r, "lease recall "+ev.LeaseID, "expected recall not found")
			}
		} else {
			got := findLeaseEvent(a.LeaseTimeline.Expiries, ev.Tick, ev.LeaseID)
			if got == nil {
				addFail(r, "lease expiry "+ev.LeaseID, "expected expiry not found")
			}
		}
	}
	for _, d := range want.Degradations {
		got := findDegradation(a.LeaseTimeline.Degradations, d.Tick, d.NodeID)
		if got == nil || !equalStrings(got.InstanceIDs, d.InstanceIDs) {
			addFail(r, "degradation "+d.NodeID, fmt.Sprintf("got %+v want %+v", got, d.InstanceIDs))
		}
	}
	r.Checks = append(r.Checks, Check{Name: "truth_leases_and_degradation", OK: true})
}

func checkGateTruth(r *VerifyReport, a *Analysis, want *truth.Truth) {
	for _, gt := range want.Gates {
		g := findGate(a.Gate.Gates, gt.NodeID)
		if g == nil {
			addFail(r, "gate "+gt.NodeID, "missing")
			continue
		}
		if g.Schedulable != gt.Schedulable || len(g.OutstandingLeaseIDs) != len(gt.OutstandingLeaseIDs) ||
			len(g.PendingReturnInstanceIDs) != gt.PendingReturnCount || len(g.NotEvacuatedInstanceIDs) != gt.NotEvacuatedCount {
			addFail(r, "gate "+gt.NodeID, fmt.Sprintf("got schedulable=%v outstanding=%v pending=%d notEvac=%d",
				g.Schedulable, g.OutstandingLeaseIDs, len(g.PendingReturnInstanceIDs), len(g.NotEvacuatedInstanceIDs)))
		}
	}
	r.Checks = append(r.Checks, Check{Name: "truth_return_gates", OK: true})
}

func checkParallelDeterminism(r *VerifyReport, a *Analysis) {
	seq, _ := json.Marshal(a.Feasibility)
	par, _ := json.Marshal(a.ParallelReports)
	if !bytes.Equal(seq, par) {
		addFail(r, "parallel_feasibility_determinism", "parallel report differs")
		return
	}
	r.Checks = append(r.Checks, Check{Name: "parallel_feasibility_determinism", OK: true})
}

func addFail(r *VerifyReport, name, detail string) {
	r.OK = false
	r.Checks = append(r.Checks, Check{Name: name, OK: false, Detail: detail})
}

func hasFailureSince(r *VerifyReport, _ string) bool { return !r.OK }

func summarize(a *Analysis) *Summary {
	s := &Summary{ProjectID: a.Project.ProjectID, Nodes: len(a.Project.Nodes), Instances: len(a.Project.Instances),
		Waves: len(a.Plan.Waves), Migrations: len(a.Plan.Migrations), Spillovers: len(a.Plan.Spillovers),
		LeaseRecalls: len(a.LeaseTimeline.Recalls), Degradations: len(a.LeaseTimeline.Degradations)}
	for _, g := range a.Gate.Gates {
		if g.Schedulable {
			s.GateNodesReady++
		} else {
			s.GateNodesBlocked++
		}
	}
	return s
}

func findTickReport(in []solver.TickReport, tick domain.Tick) *solver.TickReport {
	for i := range in {
		if in[i].Tick == tick {
			return &in[i]
		}
	}
	return nil
}

func findNodeReport(in []solver.NodeFeasibility, id string) *solver.NodeFeasibility {
	for i := range in {
		if in[i].NodeID == id {
			return &in[i]
		}
	}
	return nil
}

func findDecision(in []solver.InstanceFeasibility, id string) *solver.InstanceFeasibility {
	for i := range in {
		if in[i].InstanceID == id {
			return &in[i]
		}
	}
	return nil
}

func findWave(in []solver.WaveResult, seq int) *solver.WaveResult {
	for i := range in {
		if in[i].Wave == seq {
			return &in[i]
		}
	}
	return nil
}

func findMigration(in []solver.Migration, id string) *solver.Migration {
	for i := range in {
		if in[i].InstanceID == id {
			return &in[i]
		}
	}
	return nil
}

func hasSpill(in []solver.Spillover, want truth.SpillTruth) bool {
	for _, s := range in {
		if s.InstanceID == want.InstanceID && s.FromWave == want.FromWave && s.ToWave == want.ToWave && s.ReasonCode == want.ReasonCode {
			return true
		}
	}
	return false
}

func findLeaseEvent(in []lease.LeaseEvent, tick domain.Tick, id string) *lease.LeaseEvent {
	for i := range in {
		if in[i].Tick == tick && in[i].LeaseID == id {
			return &in[i]
		}
	}
	return nil
}

func findLeaseTick(in []lease.TickReport, tick domain.Tick) *lease.TickReport {
	for i := range in {
		if in[i].Tick == tick {
			return &in[i]
		}
	}
	return nil
}

func findDegradation(in []lease.DegradationEvent, tick domain.Tick, node string) *lease.DegradationEvent {
	for i := range in {
		if in[i].Tick == tick && in[i].NodeID == node {
			return &in[i]
		}
	}
	return nil
}

func findGate(in []gate.NodeGate, id string) *gate.NodeGate {
	for i := range in {
		if in[i].NodeID == id {
			return &in[i]
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
