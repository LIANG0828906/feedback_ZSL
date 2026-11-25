package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/generator"
	"maintenanceorchestrator/internal/solver"
	"maintenanceorchestrator/internal/store"
)

type RecoveryDemoReport struct {
	DatabasePath      string `json:"database_path"`
	CommittedWave     int    `json:"committed_wave"`
	LoadedInstances   int    `json:"loaded_instances"`
	ResumedMigrations int    `json:"resumed_migrations"`
	FullMigrations    int    `json:"full_migrations"`
	MatchesFullPlan   bool   `json:"matches_full_plan"`
	StateVersion      int    `json:"state_version"`
}

type RecoveryOutcome struct {
	Recomputed bool              `json:"recomputed"`
	Reason     string            `json:"reason,omitempty"`
	Checkpoint *store.Checkpoint `json:"checkpoint,omitempty"`
	Plan       *solver.WavePlan  `json:"plan,omitempty"`
	Analysis   *Analysis         `json:"analysis,omitempty"`
}

func RecoverOrRecompute(p domain.Project, st *store.Store) (*RecoveryOutcome, error) {
	cp, found, err := st.LoadRecovery()
	if domain.IsCode(err, domain.EStateVersionMismatch) {
		a, analyzeErr := Analyze(p)
		if analyzeErr != nil {
			return nil, analyzeErr
		}
		return &RecoveryOutcome{Recomputed: true, Reason: domain.EStateVersionMismatch, Analysis: a}, nil
	}
	if err != nil {
		return nil, err
	}
	if !found {
		a, analyzeErr := Analyze(p)
		if analyzeErr != nil {
			return nil, analyzeErr
		}
		return &RecoveryOutcome{Recomputed: true, Reason: "no_committed_checkpoint", Analysis: a}, nil
	}
	plan, err := ResumePlan(p, cp)
	if err != nil {
		if domain.IsCode(err, domain.EStateVersionMismatch) {
			a, analyzeErr := Analyze(p)
			if analyzeErr != nil {
				return nil, analyzeErr
			}
			return &RecoveryOutcome{Recomputed: true, Reason: domain.EStateVersionMismatch, Analysis: a}, nil
		}
		return nil, err
	}
	return &RecoveryOutcome{Recomputed: false, Checkpoint: &cp, Plan: &plan}, nil
}

func RecoveryDemo(workDir string) (*RecoveryDemoReport, error) {
	if workDir == "" {
		workDir = ".recovery-demo"
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(workDir, "recovery.db")
	p := generator.Generate(generator.DefaultSeed)
	a, err := Analyze(p)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	cp := CheckpointForWave(p, a, 1, 11)
	if err := st.SaveCommit(cp); err != nil {
		st.Close()
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}

	reopened, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer reopened.Close()
	loaded, found, err := reopened.LoadRecovery()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, domain.NewError(domain.ECheckpointTruncated, path, "checkpoint not found after reopen")
	}
	resumed, err := ResumePlan(p, loaded)
	if err != nil {
		return nil, err
	}
	fullJSON, _ := json.Marshal(a.Plan.Migrations)
	resumedJSON, _ := json.Marshal(resumed.Migrations)
	return &RecoveryDemoReport{
		DatabasePath: path, CommittedWave: loaded.CommittedWave,
		LoadedInstances: len(loaded.Instances), ResumedMigrations: len(resumed.Migrations),
		FullMigrations: len(a.Plan.Migrations), MatchesFullPlan: string(fullJSON) == string(resumedJSON),
		StateVersion: loaded.StateVersion,
	}, nil
}

func CheckpointForWave(p domain.Project, a *Analysis, committedWave int, tick domain.Tick) store.Checkpoint {
	c := store.Checkpoint{
		StateVersion:  store.CurrentStateVersion,
		ProjectID:     p.ProjectID,
		Seed:          p.Seed,
		Tick:          tick,
		CommittedWave: committedWave,
	}
	committed := map[string]solver.Migration{}
	placement := map[string]string{}
	for _, m := range a.Plan.Migrations {
		if m.Wave <= committedWave {
			committed[m.InstanceID] = m
			placement[m.InstanceID] = m.TargetNodeID
		}
	}
	used := solver.UsageFromPlacements(p, placement)
	for _, n := range p.Nodes {
		c.Nodes = append(c.Nodes, store.NodeRecord{
			NodeID: n.ID, State: n.State, UsedMilli: used[n.ID],
			Schedulable: !domain.MaintenanceNodeSet(p)[n.ID],
		})
	}
	for _, in := range p.Instances {
		rec := store.InstanceRecord{
			InstanceID: in.ID, OriginalNodeID: in.NodeID, CurrentNodeID: in.NodeID,
			ReturnStatus: "not_started",
		}
		if m, ok := committed[in.ID]; ok {
			rec.CurrentNodeID = m.TargetNodeID
			rec.TargetNodeID = m.TargetNodeID
			rec.Wave = m.Wave
			rec.MigrationStatus = "committed"
			rec.MigrationStart = m.StartTick
			rec.MigrationEnd = m.EndTick
		} else if m, ok := findMigrationValue(a.Plan.Migrations, in.ID); ok {
			rec.TargetNodeID = m.TargetNodeID
			rec.Wave = m.Wave
			rec.MigrationStatus = "planned"
			rec.MigrationStart = m.StartTick
			rec.MigrationEnd = m.EndTick
		} else {
			rec.MigrationStatus = "stable"
		}
		c.Instances = append(c.Instances, rec)
	}
	for _, l := range p.Loans {
		status := "scheduled"
		returned := domain.Tick(0)
		switch {
		case tick >= l.ExpiryTick:
			status = "expired"
			returned = l.ExpiryTick
		case tick >= l.StartTick:
			status = "active"
		}
		c.Leases = append(c.Leases, store.LeaseRecord{
			LeaseID: l.ID, LotID: l.LotID, LenderNodeID: l.LenderNodeID,
			BorrowerNodeID: l.BorrowerNodeID, AmountMilli: l.AmountMilli,
			StartTick: l.StartTick, ExpiryTick: l.ExpiryTick, Status: status, ReturnedTick: returned,
		})
	}
	return c
}

func FullCheckpoint(p domain.Project, a *Analysis) store.Checkpoint {
	lastMigrationWave := 0
	for _, m := range a.Plan.Migrations {
		if m.Wave > lastMigrationWave {
			lastMigrationWave = m.Wave
		}
	}
	c := CheckpointForWave(p, a, lastMigrationWave, p.Window.End)
	returned := map[string]domain.Tick{}
	for _, step := range a.Gate.Steps {
		if step.Kind == "return_complete" {
			returned[step.InstanceID] = step.Tick
		}
	}
	gateByNode := map[string]struct {
		outstanding []string
		pending     []string
		schedulable bool
	}{}
	for _, g := range a.Gate.Gates {
		gateByNode[g.NodeID] = struct {
			outstanding []string
			pending     []string
			schedulable bool
		}{g.OutstandingLeaseIDs, g.PendingReturnInstanceIDs, g.Schedulable}
	}
	for i := range c.Instances {
		if tick, ok := returned[c.Instances[i].InstanceID]; ok {
			c.Instances[i].ReturnStatus = "completed"
			c.Instances[i].ReturnEnd = tick
			c.Instances[i].CurrentNodeID = c.Instances[i].OriginalNodeID
		}
	}
	for i := range c.Nodes {
		if g, ok := gateByNode[c.Nodes[i].NodeID]; ok {
			c.Nodes[i].Schedulable = g.schedulable
			c.Nodes[i].OutstandingLeases = append([]string(nil), g.outstanding...)
			c.Nodes[i].PendingReturns = append([]string(nil), g.pending...)
		}
	}
	for i, st := range a.LeaseTimeline.Leases {
		for j := range c.Leases {
			if c.Leases[j].LeaseID == st.ID {
				c.Leases[j].Status = st.Status
				c.Leases[j].ReturnedTick = st.ReturnedTick
			}
		}
		_ = i
	}
	sort.SliceStable(c.Instances, func(i, j int) bool { return c.Instances[i].InstanceID < c.Instances[j].InstanceID })
	return c
}

func ResumePlan(p domain.Project, c store.Checkpoint) (solver.WavePlan, error) {
	if c.StateVersion != store.CurrentStateVersion {
		return solver.WavePlan{}, domain.NewError(domain.EStateVersionMismatch, "$.state_version",
			"checkpoint version %d cannot partially resume with code version %d",
			c.StateVersion, store.CurrentStateVersion).
			WithDetailsSafe(map[string]any{"full_recompute_required": true})
	}
	resume := solver.ResumeState{CommittedWave: c.CommittedWave, Tick: c.Tick, Placement: map[string]string{}}
	for _, in := range c.Instances {
		if in.MigrationStatus != "committed" {
			continue
		}
		priority, order := 0, 0
		for _, spec := range p.Instances {
			if spec.ID == in.InstanceID {
				priority, order = spec.Priority, spec.DeclaredOrder
				break
			}
		}
		resume.Placement[in.InstanceID] = in.CurrentNodeID
		resume.Migrations = append(resume.Migrations, solver.Migration{
			InstanceID: in.InstanceID, SourceNodeID: in.OriginalNodeID,
			TargetNodeID: in.TargetNodeID, Wave: in.Wave,
			StartTick: in.MigrationStart, EndTick: in.MigrationEnd,
			DurationTicks: in.MigrationEnd - in.MigrationStart,
			Priority:      priority, DeclaredOrder: order,
		})
	}
	return solver.PlanWavesFrom(p, resume), nil
}

func findMigrationValue(in []solver.Migration, id string) (solver.Migration, bool) {
	for _, m := range in {
		if m.InstanceID == id {
			return m, true
		}
	}
	return solver.Migration{}, false
}
