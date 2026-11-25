package engine

import (
	"sort"

	"quotaflow/internal/domain"
)

func (e *Engine) observe(state *State) (*domain.TickObservation, error) {
	obs := &domain.TickObservation{Tick: state.Tick}
	for _, tenantSpec := range e.project.Tenants {
		tenant := state.Tenants[tenantSpec.ID]
		reserveRemaining := tenant.Reserve - tenant.Used
		if reserveRemaining < 0 {
			reserveRemaining = 0
			e.stats.FixedStats.Saturations++
		}
		item := domain.TenantObservation{
			TenantID: tenantSpec.ID, Weight: tenant.Weight, QuotaMilli: tenant.Quota,
			UsedMilli: tenant.Used, AvailableMilli: tenant.Quota - tenant.Used,
			ReserveMilli: tenant.Reserve, ReserveRemainingMilli: reserveRemaining,
			EncroachedMilli: tenant.Encroached,
		}
		blocked := map[string]bool{}
		for _, pending := range tenant.Pending {
			waiting, err := domain.CheckedAdd(item.WaitingMilli, pending.Amount, "$.tenants."+tenantSpec.ID+".waiting")
			if err != nil {
				return nil, err
			}
			item.WaitingMilli = waiting
			for _, blocker := range pending.BlockedBy {
				blocked[blocker] = true
			}
		}
		for blocker := range blocked {
			item.BlockedBy = append(item.BlockedBy, blocker)
		}
		sort.Strings(item.BlockedBy)
		encroachedTotal, err := domain.CheckedAdd(obs.EncroachedTotal, item.EncroachedMilli, "$.encroached_total")
		if err != nil {
			return nil, err
		}
		obs.EncroachedTotal = encroachedTotal
		obs.Tenants = append(obs.Tenants, item)
	}
	for _, nodeSpec := range e.project.Nodes {
		node := state.Nodes[nodeSpec.ID]
		nresCap, err := stateNodeNonReservedCapacity(e.project, state, nodeSpec.ID)
		if err != nil {
			return nil, err
		}
		nresUsed, err := stateNodeNonReservedUsed(e.project, state, nodeSpec.ID)
		if err != nil {
			return nil, err
		}
		obs.Nodes = append(obs.Nodes, domain.NodeObservation{
			NodeID: nodeSpec.ID, CapacityMilli: node.Capacity, UsedMilli: node.Used,
			NonReservedUsedMilli: nresUsed, NonReservedFreeMilli: nresCap - nresUsed,
			PausedIncomingMilli: state.nodePausedIncoming(nodeSpec.ID), Failed: node.Failed,
		})
	}
	obs.Migrations = e.migrationObservations(state)
	for _, tenantSpec := range e.project.Tenants {
		if pause, ok := state.Pauses[tenantSpec.ID]; ok {
			obs.Pauses = append(obs.Pauses, domain.PauseObservation{
				TenantID: pause.TenantID, FromNode: pause.FromNode, ToNode: pause.ToNode,
				StartTick: pause.StartTick, ExpectedEndTick: pause.ExpectedEndTick,
				ReserveMilli: pause.ReserveMilli, EnableBasis: pause.EnableBasis,
			})
		}
	}
	for _, tenantSpec := range e.project.Tenants {
		tenant := state.Tenants[tenantSpec.ID]
		for _, pending := range tenant.Pending {
			obs.Pending = append(obs.Pending, domain.PendingObservation{
				ID: pending.ID, TenantID: pending.TenantID, NodeID: pending.NodeID,
				AmountMilli: pending.Amount, Reason: pending.Reason,
				BlockedBy: append([]string(nil), pending.BlockedBy...),
			})
		}
	}
	return obs, nil
}

func (e *Engine) migrationObservations(state *State) []domain.MigrationObservation {
	migrations := state.migrationsSorted()
	out := make([]domain.MigrationObservation, 0, len(migrations))
	for _, migration := range migrations {
		out = append(out, domain.MigrationObservation{
			ID: migration.ID, TenantID: migration.TenantID, FromNode: migration.FromNode,
			ToNode: migration.ToNode, AmountMilli: migration.Amount, StartTick: migration.StartTick,
			EndTick: migration.EndTick, Status: migration.Status,
			Constraints: append([]string(nil), migration.Constraints...),
			EnableBasis: migration.EnableBasis,
		})
	}
	return out
}
