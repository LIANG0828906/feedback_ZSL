package engine

import (
	"fmt"
	"sort"

	"quotaflow/internal/domain"
)

type Config struct {
	ThreadBudget      int    `json:"thread_budget"`
	QueueLimit        int    `json:"queue_limit"`
	CheckpointVersion string `json:"checkpoint_version"`
}

func (c Config) Validate() (Config, error) {
	if c.ThreadBudget < 1 {
		return c, domain.NewBizError(domain.EBackpressureBudget, "$.thread_budget",
			"tenant worker budget must be at least 1, got %d", c.ThreadBudget)
	}
	if c.QueueLimit < 1 {
		return c, domain.NewBizError(domain.EBackpressureQueue, "$.queue_limit",
			"per-tenant waiting queue limit must be at least 1, got %d", c.QueueLimit)
	}
	if c.CheckpointVersion == "" {
		c.CheckpointVersion = "quotaflow-checkpoint-v1"
	}
	return c, nil
}

type EditKind string

const (
	EditTenantWeight    EditKind = "tenant_weight"
	EditNodeCapacity    EditKind = "node_capacity"
	EditNodeFaultDomain EditKind = "node_fault_domain"
	EditDomainParent    EditKind = "fault_domain_parent"
)

type Edit struct {
	ID            string   `json:"id"`
	Kind          EditKind `json:"kind"`
	Target        string   `json:"target"`
	FromTick      int64    `json:"from_tick"`
	Weight        int64    `json:"weight,omitempty"`
	CapacityMilli int64    `json:"capacity_milli,omitempty"`
	FaultDomain   string   `json:"fault_domain,omitempty"`
	Parent        string   `json:"parent,omitempty"`
}

func (e Edit) Validate(project *domain.Project) error {
	if e.ID == "" {
		return domain.NewBizError(domain.EEditInvalid, "$.edit.id", "edit id is required")
	}
	if e.FromTick < 1 {
		return domain.NewBizError(domain.EEditInvalid, "$.edit.from_tick", "from_tick must be >= 1")
	}
	switch e.Kind {
	case EditTenantWeight:
		if _, ok := project.TenantByID(e.Target); !ok {
			return domain.NewBizError(domain.ETenantUnknown, "$.edit.target", "unknown tenant %q", e.Target)
		}
		if e.Weight < 1 || e.Weight > 1000 {
			return domain.NewBizError(domain.EInvalidWeight, "$.edit.weight", "weight %d outside 1..1000", e.Weight)
		}
	case EditNodeCapacity:
		if _, ok := project.NodeByID(e.Target); !ok {
			return domain.NewBizError(domain.ENodeUnknown, "$.edit.target", "unknown node %q", e.Target)
		}
		if e.CapacityMilli <= 0 {
			return domain.NewBizError(domain.EInvalidCapacity, "$.edit.capacity_milli", "capacity must be positive")
		}
	case EditNodeFaultDomain:
		if _, ok := project.NodeByID(e.Target); !ok {
			return domain.NewBizError(domain.ENodeUnknown, "$.edit.target", "unknown node %q", e.Target)
		}
		if _, ok := project.DomainByID(e.FaultDomain); !ok {
			return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.fault_domain", "unknown domain %q", e.FaultDomain)
		}
	case EditDomainParent:
		if _, ok := project.DomainByID(e.Target); !ok {
			return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.target", "unknown domain %q", e.Target)
		}
		if e.Parent != "" {
			if _, ok := project.DomainByID(e.Parent); !ok {
				return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.parent", "unknown parent %q", e.Parent)
			}
		}
	default:
		return domain.NewBizError(domain.EEditInvalid, "$.edit.kind", "unknown edit kind %q", e.Kind)
	}
	return nil
}

type TenantTiming struct {
	TenantID    string `json:"tenant_id"`
	Nanoseconds int64  `json:"nanoseconds"`
}

type RunStats struct {
	ThreadBudget    int               `json:"thread_budget"`
	QueueLimit      int               `json:"queue_limit"`
	WorkerCount     int               `json:"worker_count"`
	PerTenantTiming []TenantTiming    `json:"per_tenant_timing_ns"`
	FixedStats      domain.FixedStats `json:"fixed_point"`
}

func (s *RunStats) addTiming(tenantID string, nanos int64) {
	for i := range s.PerTenantTiming {
		if s.PerTenantTiming[i].TenantID == tenantID {
			s.PerTenantTiming[i].Nanoseconds += nanos
			return
		}
	}
	s.PerTenantTiming = append(s.PerTenantTiming, TenantTiming{TenantID: tenantID, Nanoseconds: nanos})
	sort.SliceStable(s.PerTenantTiming, func(i, j int) bool {
		return s.PerTenantTiming[i].TenantID < s.PerTenantTiming[j].TenantID
	})
}

func (s RunStats) String() string {
	return fmt.Sprintf("workers=%d queue_limit=%d", s.WorkerCount, s.QueueLimit)
}
