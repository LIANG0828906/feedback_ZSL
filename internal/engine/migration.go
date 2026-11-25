package engine

import (
	"fmt"
	"sort"

	"quotaflow/internal/domain"
)

func (e *Engine) commitMigrations(state *State, tick int64) error {
	var due []Migration
	for _, migration := range state.Migrations {
		if migration.Status == "paused" && migration.EndTick <= tick {
			due = append(due, migration)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	for _, migration := range due {
		if state.Nodes[migration.ToNode].Failed {
			return domain.NewBizError(domain.EMigrationConflict, "$.migrations."+migration.ID+".to_node",
				"target node %q failed before migration commit", migration.ToNode)
		}
		if state.actual(migration.TenantID, migration.FromNode) < migration.Amount {
			return domain.NewBizError(domain.EMigrationConflict, "$.migrations."+migration.ID+".from_node",
				"source placement disappeared before migration commit")
		}
		if err := e.removePlacement(state, migration.TenantID, migration.FromNode, migration.Amount, false); err != nil {
			return err
		}
		if err := e.addPlacement(state, migration.TenantID, migration.ToNode, migration.Amount, false); err != nil {
			return err
		}
		migration.Status = "committed"
		migration.EndTick = tick
		state.Migrations[migration.ID] = migration
		delete(state.Pauses, migration.TenantID)
	}
	return nil
}

type targetPlan struct {
	tenantID   string
	amount     int64
	targets    []string
	failedNode string
}

func (e *Engine) failNode(state *State, tick int64, nodeID string) error {
	node := state.Nodes[nodeID]
	if node.Failed {
		return nil
	}
	node.Failed = true
	state.Nodes[nodeID] = node
	var plans []targetPlan
	for _, tenant := range e.project.Tenants {
		amount := state.actual(tenant.ID, nodeID)
		if amount <= 0 {
			continue
		}
		if _, exists := state.Pauses[tenant.ID]; exists {
			return domain.NewBizError(domain.EMigrationConflict, "$.pauses."+tenant.ID,
				"tenant already paused while another migration is pending")
		}
		plans = append(plans, targetPlan{tenantID: tenant.ID, amount: amount, failedNode: nodeID})
	}
	probes := e.parallelTargetProbes(state, plans)
	for index := range plans {
		plan := &plans[index]
		target, constraints, err := e.chooseTarget(state, plan.failedNode, plan.tenantID, plan.amount, probes[index])
		if err != nil {
			return err
		}
		state.NextMigration++
		migration := Migration{
			ID:       fmt.Sprintf("mig-%04d-%s-%02d", tick, plan.tenantID, state.NextMigration),
			TenantID: plan.tenantID, FromNode: plan.failedNode, ToNode: target,
			Amount: plan.amount, StartTick: tick, EndTick: tick + 1, Status: "paused",
			Constraints: constraints,
		}
		migration.EnableBasis = "commit:" + migration.ID
		state.Migrations[migration.ID] = migration
		state.Pauses[plan.tenantID] = Pause{
			TenantID: plan.tenantID, FromNode: plan.failedNode, ToNode: target,
			StartTick: tick, ExpectedEndTick: tick + 1,
			ReserveMilli: state.Tenants[plan.tenantID].Reserve,
			EnableBasis:  "commit:" + migration.ID,
		}
	}
	return nil
}

func (e *Engine) chooseTarget(state *State, failedNode, tenantID string, amount int64, candidates []string) (string, []string, error) {
	failedSpec, ok := e.project.NodeByID(failedNode)
	if !ok {
		return "", nil, domain.NewBizError(domain.ENodeUnknown, "$.nodes."+failedNode, "unknown failed node")
	}
	for _, target := range candidates {
		if state.Nodes[target].Failed {
			continue
		}
		if state.DomainInSubtree(failedSpec.FaultDomain, state.Nodes[target].FaultDomain) {
			continue
		}
		capacity, err := e.admissionCapacity(state, tenantID, target)
		if err != nil {
			return "", nil, err
		}
		if capacity < amount {
			continue
		}
		constraints := []string{
			"fd-avoid:" + failedSpec.FaultDomain,
			"tenant-allowed:" + tenantID,
			"capacity:" + target,
		}
		sort.Strings(constraints)
		return target, constraints, nil
	}
	return "", nil, domain.NewBizError(domain.EMigrationNoTarget, "$.nodes."+failedNode,
		"tenant %q amount %d has no feasible target", tenantID, amount)
}
