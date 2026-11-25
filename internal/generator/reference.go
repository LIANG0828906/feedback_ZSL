package generator

import (
	"fmt"
	"sort"

	"quotaflow/internal/domain"
)

// The reference simulator is intentionally independent from internal/engine.
// It consumes only the normalized project and constructs truth snapshots from
// the generated event schedule. No processing output is fed back into truth.
type refTenant struct {
	weight     int64
	quota      int64
	reserve    int64
	used       int64
	encroached int64
}

type refNode struct {
	capacity int64
	failed   bool
	used     int64
}

type refPending struct {
	id        string
	tenantID  string
	nodeID    string
	amount    int64
	reason    string
	blockedBy []string
}

type refMigration struct {
	domain.MigrationObservation
}

type reference struct {
	project    *domain.Project
	tenants    map[string]*refTenant
	nodes      map[string]*refNode
	placement  map[string]map[string]int64
	pending    map[string][]*refPending
	migrations []*refMigration
	pauses     map[string]*domain.PauseObservation
	pauseHist  []domain.PauseObservation
	queueLimit int
	nextMig    int
}

func BuildTruth(project *domain.Project, seed int64, queueLimit int) (*domain.Truth, error) {
	if queueLimit <= 0 {
		return nil, domain.NewBizError(domain.EBackpressureQueue, "$.queue_limit", "queue limit must be positive")
	}
	ref := &reference{
		project: project, tenants: map[string]*refTenant{}, nodes: map[string]*refNode{},
		placement: map[string]map[string]int64{}, pending: map[string][]*refPending{},
		pauses: map[string]*domain.PauseObservation{}, queueLimit: queueLimit,
	}
	for _, tenant := range project.Tenants {
		quota, err := domain.TenantQuota(project.TotalCapacityMilli, tenant, 0, "$.tenants."+tenant.ID)
		if err != nil {
			return nil, err
		}
		reserve, err := domain.TenantReserve(project.TotalCapacityMilli, tenant, nil, "$.tenants."+tenant.ID)
		if err != nil {
			return nil, err
		}
		ref.tenants[tenant.ID] = &refTenant{weight: tenant.Weight, quota: quota, reserve: reserve}
		ref.placement[tenant.ID] = map[string]int64{}
	}
	for _, node := range project.Nodes {
		ref.nodes[node.ID] = &refNode{capacity: node.CapacityMilli}
	}
	truth := &domain.Truth{Seed: seed, ProjectID: project.ID, ProjectFingerprint: project.CanonicalFingerprint()}
	eventsByTick := project.EventsByTick()
	for tick := int64(1); tick <= project.MaxTick; tick++ {
		if err := ref.commitMigrations(tick); err != nil {
			return nil, err
		}
		for _, event := range eventsByTick[tick] {
			if err := ref.applyEvent(event); err != nil {
				return nil, err
			}
		}
		if err := ref.retryPending(); err != nil {
			return nil, err
		}
		snapshot, err := ref.observe(tick)
		if err != nil {
			return nil, err
		}
		truth.Ticks = append(truth.Ticks, *snapshot)
	}
	truth.Migrations = ref.migrationObservations()
	truth.PauseHistory = append([]domain.PauseObservation(nil), ref.pauseHist...)
	sort.SliceStable(truth.PauseHistory, func(i, j int) bool {
		if truth.PauseHistory[i].StartTick != truth.PauseHistory[j].StartTick {
			return truth.PauseHistory[i].StartTick < truth.PauseHistory[j].StartTick
		}
		return truth.PauseHistory[i].TenantID < truth.PauseHistory[j].TenantID
	})
	return truth, nil
}

func (r *reference) tenant(id string) *refTenant {
	return r.tenants[id]
}

func (r *reference) node(id string) *refNode {
	return r.nodes[id]
}

func (r *reference) actual(t, n string) int64 {
	return r.placement[t][n]
}

func (r *reference) pausedIncoming(t, n string) int64 {
	sum := int64(0)
	for _, migration := range r.migrations {
		if migration.Status == "paused" && migration.TenantID == t && migration.ToNode == n {
			sum += migration.AmountMilli
		}
	}
	return sum
}

func (r *reference) virtual(t, n string) int64 {
	return r.actual(t, n) + r.pausedIncoming(t, n)
}

func (r *reference) nodePausedIncoming(n string) int64 {
	sum := int64(0)
	for _, migration := range r.migrations {
		if migration.Status == "paused" && migration.ToNode == n {
			sum += migration.AmountMilli
		}
	}
	return sum
}

func (r *reference) nodeNonReservedCapacity(n string) (int64, error) {
	node := r.node(n)
	reserveSum := int64(0)
	for _, tenant := range r.project.Tenants {
		if !allowedOn(r.project, tenant.ID, n) {
			continue
		}
		var err error
		reserveSum, err = domain.CheckedAdd(reserveSum, r.tenant(tenant.ID).reserve, "$.nodes."+n)
		if err != nil {
			return 0, err
		}
	}
	if reserveSum > node.capacity {
		return 0, domain.NewBizError(domain.EReserveExceedsNode, "$.nodes."+n,
			"reserves %d exceed node capacity %d", reserveSum, node.capacity)
	}
	return node.capacity - reserveSum, nil
}

func (r *reference) nodeNonReservedUsed(n string) (int64, error) {
	used := int64(0)
	for _, tenant := range r.project.Tenants {
		if !allowedOn(r.project, tenant.ID, n) {
			continue
		}
		burst := r.virtual(tenant.ID, n) - r.tenant(tenant.ID).reserve
		if burst > 0 {
			var err error
			used, err = domain.CheckedAdd(used, burst, "$.nodes."+n)
			if err != nil {
				return 0, err
			}
		}
	}
	return used, nil
}

func (r *reference) admissionCapacity(t, n string) (int64, error) {
	nonReservedCap, err := r.nodeNonReservedCapacity(n)
	if err != nil {
		return 0, err
	}
	nonReservedUsed, err := r.nodeNonReservedUsed(n)
	if err != nil {
		return 0, err
	}
	free := nonReservedCap - nonReservedUsed
	if free < 0 {
		return 0, domain.NewBizError(domain.ELedgerInvariant, "$.nodes."+n,
			"non-reserved usage %d exceeds protected capacity %d", nonReservedUsed, nonReservedCap)
	}
	ownGap := r.tenant(t).reserve - r.virtual(t, n)
	if ownGap < 0 {
		ownGap = 0
	}
	return domain.CheckedAdd(free, ownGap, "$.nodes."+n+".admission")
}

func (r *reference) addPlacement(t, n string, amount int64, updateTenant bool) error {
	if amount <= 0 {
		return domain.NewBizError(domain.ELedgerInvariant, "$.placement", "non-positive placement amount %d", amount)
	}
	node := r.node(n)
	if node.failed {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+n, "cannot place on failed node")
	}
	nextCurrent, err := domain.CheckedAdd(r.actual(t, n), amount, "$.placement."+t+"."+n)
	if err != nil {
		return err
	}
	if nextCurrent > node.capacity {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+n,
			"placement would use %d of %d", nextCurrent, node.capacity)
	}
	nextNodeUsed, err := domain.CheckedAdd(node.used, amount, "$.nodes."+n+".used")
	if err != nil {
		return err
	}
	if nextNodeUsed > node.capacity {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+n,
			"node usage %d would exceed capacity %d", nextNodeUsed, node.capacity)
	}
	r.placement[t][n] = nextCurrent
	node.used = nextNodeUsed
	if updateTenant {
		nextUsed, err := domain.CheckedAdd(r.tenant(t).used, amount, "$.tenants."+t+".used")
		if err != nil {
			return err
		}
		r.tenant(t).used = nextUsed
	}
	if r.tenant(t).used > r.tenant(t).quota {
		return domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+t,
			"used %d exceeds quota %d", r.tenant(t).used, r.tenant(t).quota)
	}
	return nil
}

func (r *reference) removePlacement(t, n string, amount int64, updateTenant bool) error {
	if amount <= 0 {
		return domain.NewBizError(domain.ELedgerInvariant, "$.placement", "non-positive removal amount %d", amount)
	}
	if r.actual(t, n) < amount {
		return domain.NewBizError(domain.EReleaseExceedsUsed, "$.placement."+t+"."+n,
			"release %d exceeds placement %d", amount, r.actual(t, n))
	}
	r.placement[t][n] -= amount
	r.node(n).used -= amount
	if updateTenant {
		if r.tenant(t).used < amount {
			return domain.NewBizError(domain.EReleaseExceedsUsed, "$.tenants."+t, "release exceeds used")
		}
		r.tenant(t).used -= amount
	}
	return nil
}

func (r *reference) commitMigrations(tick int64) error {
	var due []*refMigration
	for _, migration := range r.migrations {
		if migration.Status == "paused" && migration.EndTick <= tick {
			due = append(due, migration)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	for _, migration := range due {
		target := r.node(migration.ToNode)
		if target.failed {
			return domain.NewBizError(domain.EMigrationConflict,
				"$.migrations."+migration.ID+".to_node", "target node failed before commit")
		}
		if r.actual(migration.TenantID, migration.FromNode) < migration.AmountMilli {
			return domain.NewBizError(domain.EMigrationConflict,
				"$.migrations."+migration.ID+".from_node", "source placement disappeared before commit")
		}
		if err := r.removePlacement(migration.TenantID, migration.FromNode, migration.AmountMilli, false); err != nil {
			return err
		}
		if err := r.addPlacement(migration.TenantID, migration.ToNode, migration.AmountMilli, false); err != nil {
			return err
		}
		migration.Status = "committed"
		migration.EndTick = tick
		delete(r.pauses, migration.TenantID)
	}
	return nil
}

func (r *reference) applyEvent(event domain.Event) error {
	switch event.Kind {
	case domain.EventRequest:
		return r.request(event.ID, event.Tenant, event.Node, event.AmountMilli)
	case domain.EventRelease:
		if err := r.removePlacement(event.Tenant, event.Node, event.AmountMilli, true); err != nil {
			return domain.WithDetail(err.(*domain.BizError), "event_id", event.ID)
		}
		return nil
	case domain.EventWeight:
		tenantSpec, ok := r.project.TenantByID(event.Tenant)
		if !ok {
			return domain.NewBizError(domain.ETenantUnknown, "$.events."+event.ID+".tenant", "unknown tenant")
		}
		quota, err := domain.TenantQuota(r.project.TotalCapacityMilli, tenantSpec, event.Weight, "$.events."+event.ID)
		if err != nil {
			return err
		}
		if r.tenant(event.Tenant).used > quota {
			return domain.NewBizError(domain.EWeightBelowUsed, "$.events."+event.ID+".weight",
				"new quota %d would be below used %d", quota, r.tenant(event.Tenant).used)
		}
		r.tenant(event.Tenant).weight = event.Weight
		r.tenant(event.Tenant).quota = quota
		return nil
	case domain.EventNodeFailure:
		return r.failNode(event.Tick, event.Node)
	case domain.EventNodeRecovery:
		r.node(event.Node).failed = false
		return nil
	default:
		return domain.NewBizError(domain.EEventInvalid, "$.events."+event.ID, "unknown kind %q", event.Kind)
	}
}

func (r *reference) request(eventID, tenantID, nodeID string, amount int64) error {
	if _, paused := r.pauses[tenantID]; paused {
		return r.enqueue(&refPending{
			id: eventID, tenantID: tenantID, nodeID: nodeID, amount: amount,
			reason: "migration_pause", blockedBy: []string{tenantID},
		})
	}
	admitted, reason, blocked, err := r.tryAdmit(tenantID, nodeID, amount)
	if err != nil {
		return err
	}
	if admitted > 0 {
		if err := r.addPlacement(tenantID, nodeID, admitted, true); err != nil {
			return err
		}
	}
	if admitted == amount {
		return nil
	}
	remainder := amount - admitted
	encroached, err := domain.CheckedAdd(r.tenant(tenantID).encroached, remainder, "$.tenants."+tenantID+".encroached")
	if err != nil {
		return err
	}
	r.tenant(tenantID).encroached = encroached
	return r.enqueue(&refPending{
		id: eventID, tenantID: tenantID, nodeID: nodeID, amount: remainder,
		reason: reason, blockedBy: blocked,
	})
}

func (r *reference) tryAdmit(tenantID, nodeID string, amount int64) (int64, string, []string, error) {
	node := r.node(nodeID)
	if node.failed {
		return 0, "node_failed", nil, nil
	}
	tenant := r.tenant(tenantID)
	quotaRemaining := tenant.quota - tenant.used
	if quotaRemaining < 0 {
		return 0, "", nil, domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantID, "negative quota remainder")
	}
	nodeCapacity, err := r.admissionCapacity(tenantID, nodeID)
	if err != nil {
		return 0, "", nil, err
	}
	admitted := amount
	if admitted > quotaRemaining {
		admitted = quotaRemaining
	}
	if admitted > nodeCapacity {
		admitted = nodeCapacity
	}
	if admitted < 0 {
		return 0, "", nil, domain.NewBizError(domain.ELedgerInvariant, "$.admission", "negative admission")
	}
	reason := ""
	blocked := []string(nil)
	if admitted < amount {
		switch {
		case quotaRemaining <= nodeCapacity:
			reason = "quota"
			blocked = r.reserveBlockers(tenantID, nodeID, false)
		default:
			reason = "node_capacity"
			blocked = r.reserveBlockers(tenantID, nodeID, true)
		}
		if len(blocked) == 0 {
			blocked = r.nodeUsers(tenantID, nodeID)
		}
		if len(blocked) == 0 && reason == "quota" {
			blocked = []string{tenantID}
		}
	}
	return admitted, reason, blocked, nil
}

func (r *reference) enqueue(pending *refPending) error {
	if len(r.pending[pending.tenantID]) >= r.queueLimit {
		return domain.NewBizError(domain.EBackpressureQueue, "$.pending."+pending.tenantID,
			"tenant queue limit %d reached", r.queueLimit)
	}
	r.pending[pending.tenantID] = append(r.pending[pending.tenantID], pending)
	return nil
}

func (r *reference) retryPending() error {
	for _, tenant := range r.project.Tenants {
		if _, paused := r.pauses[tenant.ID]; paused {
			continue
		}
		queue := r.pending[tenant.ID]
		kept := queue[:0]
		for _, pending := range queue {
			admitted, reason, blocked, err := r.tryAdmit(pending.tenantID, pending.nodeID, pending.amount)
			if err != nil {
				return err
			}
			if admitted > 0 {
				if err := r.addPlacement(pending.tenantID, pending.nodeID, admitted, true); err != nil {
					return err
				}
				pending.amount -= admitted
			}
			if pending.amount == 0 {
				continue
			}
			pending.reason = reason
			pending.blockedBy = blocked
			kept = append(kept, pending)
		}
		r.pending[tenant.ID] = kept
	}
	return nil
}

func (r *reference) failNode(tick int64, nodeID string) error {
	node := r.node(nodeID)
	if node.failed {
		return nil
	}
	node.failed = true
	var affected []string
	for _, tenant := range r.project.Tenants {
		if r.actual(tenant.ID, nodeID) > 0 {
			affected = append(affected, tenant.ID)
		}
	}
	sort.Strings(affected)
	for _, tenantID := range affected {
		if _, exists := r.pauses[tenantID]; exists {
			return domain.NewBizError(domain.EMigrationConflict, "$.pauses."+tenantID,
				"tenant already paused while another migration is pending")
		}
		amount := r.actual(tenantID, nodeID)
		target, constraints, err := r.findTarget(tenantID, nodeID, amount)
		if err != nil {
			return err
		}
		r.nextMig++
		migration := &refMigration{domain.MigrationObservation{
			ID: fmt.Sprintf("mig-%04d-%s-%02d", tick, tenantID, r.nextMig), TenantID: tenantID,
			FromNode: nodeID, ToNode: target, AmountMilli: amount, StartTick: tick,
			EndTick: tick + 1, Status: "paused", Constraints: constraints,
		}}
		migration.EnableBasis = "commit:" + migration.ID
		r.migrations = append(r.migrations, migration)
		pause := domain.PauseObservation{
			TenantID: tenantID, FromNode: nodeID, ToNode: target, StartTick: tick,
			ExpectedEndTick: tick + 1, ReserveMilli: r.tenant(tenantID).reserve,
			EnableBasis: "commit:" + migration.ID,
		}
		r.pauses[tenantID] = &pause
		r.pauseHist = append(r.pauseHist, pause)
	}
	return nil
}

func (r *reference) findTarget(tenantID, failedNode string, amount int64) (string, []string, error) {
	failedSpec, _ := r.project.NodeByID(failedNode)
	var candidates []string
	for _, node := range r.project.Nodes {
		if node.ID == failedNode || r.node(node.ID).failed {
			continue
		}
		if !contains(node.AllowedTenants, tenantID) {
			continue
		}
		if r.project.DomainInSubtree(failedSpec.FaultDomain, node.FaultDomain) {
			continue
		}
		capacity, err := r.admissionCapacity(tenantID, node.ID)
		if err != nil {
			return "", nil, err
		}
		if capacity >= amount {
			candidates = append(candidates, node.ID)
		}
	}
	if len(candidates) == 0 {
		return "", nil, domain.NewBizError(domain.EMigrationNoTarget, "$.nodes."+failedNode,
			"tenant %q amount %d has no feasible target", tenantID, amount)
	}
	sort.Strings(candidates)
	target := candidates[0]
	constraints := []string{
		"fd-avoid:" + failedSpec.FaultDomain,
		"tenant-allowed:" + tenantID,
		"capacity:" + target,
	}
	sort.Strings(constraints)
	return target, constraints, nil
}

func (r *reference) reserveBlockers(requester, nodeID string, nodeScoped bool) []string {
	type item struct {
		id       string
		priority int
	}
	var items []item
	for _, tenant := range r.project.Tenants {
		if tenant.ID == requester {
			continue
		}
		if nodeScoped && !allowedOn(r.project, tenant.ID, nodeID) {
			continue
		}
		unused := r.tenant(tenant.ID).reserve - r.virtual(tenant.ID, nodeID)
		if unused > 0 {
			items = append(items, item{id: tenant.ID, priority: tenant.Priority})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority > items[j].priority
		}
		return items[i].id < items[j].id
	})
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.id)
	}
	return out
}

func (r *reference) nodeUsers(requester, nodeID string) []string {
	type item struct {
		id   string
		used int64
	}
	var items []item
	for _, tenant := range r.project.Tenants {
		if tenant.ID == requester {
			continue
		}
		if used := r.virtual(tenant.ID, nodeID); used > 0 {
			items = append(items, item{id: tenant.ID, used: used})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].used != items[j].used {
			return items[i].used > items[j].used
		}
		return items[i].id < items[j].id
	})
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.id)
	}
	return out
}

func (r *reference) observe(tick int64) (*domain.TickObservation, error) {
	obs := &domain.TickObservation{Tick: tick}
	for _, tenantSpec := range r.project.Tenants {
		tenant := r.tenant(tenantSpec.ID)
		remaining := tenant.reserve - tenant.used
		if remaining < 0 {
			remaining = 0
		}
		item := domain.TenantObservation{
			TenantID: tenantSpec.ID, Weight: tenant.weight, QuotaMilli: tenant.quota,
			UsedMilli: tenant.used, AvailableMilli: tenant.quota - tenant.used,
			ReserveMilli: tenant.reserve, ReserveRemainingMilli: remaining,
			EncroachedMilli: tenant.encroached,
		}
		blocked := map[string]bool{}
		for _, pending := range r.pending[tenantSpec.ID] {
			waiting, err := domain.CheckedAdd(item.WaitingMilli, pending.amount, "$.tenants."+tenantSpec.ID+".waiting")
			if err != nil {
				return nil, err
			}
			item.WaitingMilli = waiting
			for _, blocker := range pending.blockedBy {
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
	for _, nodeSpec := range r.project.Nodes {
		node := r.node(nodeSpec.ID)
		nresCap, err := r.nodeNonReservedCapacity(nodeSpec.ID)
		if err != nil {
			return nil, err
		}
		nresUsed, err := r.nodeNonReservedUsed(nodeSpec.ID)
		if err != nil {
			return nil, err
		}
		obs.Nodes = append(obs.Nodes, domain.NodeObservation{
			NodeID: nodeSpec.ID, CapacityMilli: node.capacity, UsedMilli: node.used,
			NonReservedUsedMilli: nresUsed, NonReservedFreeMilli: nresCap - nresUsed,
			PausedIncomingMilli: r.nodePausedIncoming(nodeSpec.ID), Failed: node.failed,
		})
	}
	obs.Migrations = r.migrationObservations()
	for _, tenantSpec := range r.project.Tenants {
		if pause, ok := r.pauses[tenantSpec.ID]; ok {
			obs.Pauses = append(obs.Pauses, *pause)
		}
	}
	for _, tenantSpec := range r.project.Tenants {
		for _, pending := range r.pending[tenantSpec.ID] {
			obs.Pending = append(obs.Pending, domain.PendingObservation{
				ID: pending.id, TenantID: pending.tenantID, NodeID: pending.nodeID,
				AmountMilli: pending.amount, Reason: pending.reason,
				BlockedBy: append([]string(nil), pending.blockedBy...),
			})
		}
	}
	return obs, nil
}

func (r *reference) migrationObservations() []domain.MigrationObservation {
	out := make([]domain.MigrationObservation, 0, len(r.migrations))
	for _, migration := range r.migrations {
		item := migration.MigrationObservation
		item.Constraints = append([]string(nil), migration.Constraints...)
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartTick != out[j].StartTick {
			return out[i].StartTick < out[j].StartTick
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func allowedOn(project *domain.Project, tenantID, nodeID string) bool {
	node, ok := project.NodeByID(nodeID)
	if !ok {
		return false
	}
	return contains(node.AllowedTenants, tenantID)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
