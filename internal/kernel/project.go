package kernel

import "sort"

func (p *Project) Tenant(id ID) (Tenant, bool) {
	for _, t := range p.Tenants {
		if t.ID == id {
			return t, true
		}
	}
	return Tenant{}, false
}

func (p *Project) Node(id ID) (Node, bool) {
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Nodes = append([]Node(nil), p.Nodes...)
	for i := range cp.Nodes {
		cp.Nodes[i].AllowedTenants = append([]ID(nil), p.Nodes[i].AllowedTenants...)
	}
	cp.Tenants = append([]Tenant(nil), p.Tenants...)
	cp.Events = append([]Event(nil), p.Events...)
	for i := range cp.Events {
		cp.Events[i].Refs = append([]string(nil), p.Events[i].Refs...)
	}
	return &cp
}

func (p *Project) SortDefinitions() {
	sort.SliceStable(p.Nodes, func(i, j int) bool { return p.Nodes[i].ID < p.Nodes[j].ID })
	sort.SliceStable(p.Tenants, func(i, j int) bool { return p.Tenants[i].ID < p.Tenants[j].ID })
	sort.SliceStable(p.Events, func(i, j int) bool {
		if p.Events[i].Tick != p.Events[j].Tick {
			return p.Events[i].Tick < p.Events[j].Tick
		}
		return p.Events[i].ID < p.Events[j].ID
	})
	for i := range p.Nodes {
		sort.Slice(p.Nodes[i].AllowedTenants, func(a, b int) bool {
			return p.Nodes[i].AllowedTenants[a] < p.Nodes[i].AllowedTenants[b]
		})
	}
}

// Fingerprint is a deterministic FNV-1a style hash over canonical fields. It
// intentionally includes units/scales only after canonical mapping.
func (p *Project) Fingerprint() string {
	h := uint64(1469598103934665603)
	write := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
	}
	writeInt := func(v int64) {
		var buf [8]byte
		x := uint64(v)
		for i := 7; i >= 0; i-- {
			buf[i] = byte(x)
			x >>= 8
		}
		for _, b := range buf {
			h ^= uint64(b)
			h *= 1099511628211
		}
	}

	write(p.ProjectID)
	writeInt(p.Seed)
	writeInt(p.TickMS)
	for _, n := range p.Nodes {
		write(string(n.ID))
		write(n.Rack)
		writeInt(n.CapacityBytes)
		for _, id := range n.AllowedTenants {
			write(string(id))
		}
	}
	for _, t := range p.Tenants {
		write(string(t.ID))
		writeInt(int64(t.Priority))
		writeInt(t.WeightPermyriad)
		writeInt(t.QuotaBytes)
		writeInt(t.GuaranteeBytes)
		writeInt(t.BurstLimitBytes)
		write(string(t.GuaranteeNodeID))
	}
	for _, e := range p.Events {
		write(e.ID)
		writeInt(e.Tick)
		write(e.Type)
		write(string(e.TenantID))
		write(string(e.NodeID))
		writeInt(e.AmountBytes)
		writeInt(e.WeightPermyriad)
		writeInt(e.BudgetMS)
		for _, r := range e.Refs {
			write(r)
		}
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = hex[h&15]
		h >>= 4
	}
	return "fnv1a-" + string(out)
}
