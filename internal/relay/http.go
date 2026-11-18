package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (r *Relay) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "time_unix_millis": time.Now().UnixMilli(),
	})
}

func (r *Relay) handleStatus(w http.ResponseWriter, req *http.Request) {
	gatewayID := req.URL.Query().Get("gateway_id")
	all := r.allMappings()
	byGateway := make(map[string][]StatusMapping)
	for _, m := range all {
		if gatewayID != "" && m.GatewayID != gatewayID {
			continue
		}
		byGateway[m.GatewayID] = append(byGateway[m.GatewayID], m.status(time.Now()))
	}
	if gatewayID != "" {
		if _, ok := byGateway[gatewayID]; !ok {
			byGateway[gatewayID] = []StatusMapping{}
		}
	}
	ids := make([]string, 0, len(byGateway))
	for id := range byGateway {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now()
	out := StatusResponse{NowUnixMillis: now.UnixMilli(), Gateways: make([]GatewayStatus, 0, len(ids))}
	for _, id := range ids {
		mappings := byGateway[id]
		sort.Slice(mappings, func(i, j int) bool { return protocolRank(mappings[i].Protocol) < protocolRank(mappings[j].Protocol) })
		gs := GatewayStatus{GatewayID: id, Mappings: mappings}
		for _, m := range mappings {
			if m.Active {
				gs.ActiveMappings++
			}
			if gs.LastProbe == nil || m.ProbeEvidence.ProbedAt > gs.LastProbe.ProbedAt {
				ev := m.ProbeEvidence
				gs.LastProbe = &ev
			}
		}
		if gs.LastProbe == nil {
			r.regMu.RLock()
			if ev, ok := r.lastProbe[id]; ok {
				copy := ev
				gs.LastProbe = &copy
			}
			r.regMu.RUnlock()
		}
		cs, _ := r.counters(req.Context(), id)
		gs.Counters = GatewayCounters{
			SourceMismatchDrops: cs.SourceMismatchDrops, NATTypeMismatchCount: cs.NATTypeMismatchCount,
			RecycleCount: cs.RecycleCount,
		}
		if rec, ok, err := r.store.LastRecycle(req.Context(), id); err == nil && ok {
			gs.LastRecycle = &RecycleStatus{
				ID: rec.ID, Protocol: rec.Protocol, ExternalPort: rec.ExternalPort, Reason: rec.Reason,
				RecycledAtUnixMillis: rec.RecycledAt.UnixMilli(), CooldownUntilMillis: rec.CooldownUntil.UnixMilli(),
			}
		}
		out.Gateways = append(out.Gateways, gs)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (r *Relay) handleRecycles(w http.ResponseWriter, req *http.Request) {
	gatewayID := req.URL.Query().Get("gateway_id")
	rows, err := r.store.ListRecycles(req.Context(), gatewayID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]RecycleStatus, 0, len(rows))
	for _, r := range rows {
		out = append(out, RecycleStatus{
			ID: r.ID, Protocol: r.Protocol, ExternalPort: r.ExternalPort, Reason: r.Reason,
			RecycledAtUnixMillis: r.RecycledAt.UnixMilli(), CooldownUntilMillis: r.CooldownUntil.UnixMilli(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"gateway_id": gatewayID, "recycles": out})
}

func (r *Relay) handleMetrics(w http.ResponseWriter, req *http.Request) {
	mappings := r.allMappings()
	gateways := make(map[string]bool)
	for _, m := range mappings {
		gateways[m.GatewayID] = true
	}
	if counters, err := r.store.LoadCounters(req.Context()); err == nil {
		for id := range counters {
			gateways[id] = true
		}
	}
	ids := make([]string, 0, len(gateways))
	for id := range gateways {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	writeMetricHeader(&b, "relay_gateway_active_mappings", "Number of currently active protocol mappings for a gateway.", "gauge")
	writeMetricHeader(&b, "relay_mapping_active", "Whether a gateway/protocol mapping is active.", "gauge")
	writeMetricHeader(&b, "relay_mapping_port", "Externally allocated mapping port; zero when absent.", "gauge")
	writeMetricHeader(&b, "relay_mapping_keepalive_remaining_seconds", "Seconds remaining in the mapping keepalive window.", "gauge")
	writeMetricHeader(&b, "relay_mapping_forwarded_total", "Packets accepted from the bound source and forwarded.", "counter")
	writeMetricHeader(&b, "relay_gateway_source_mismatch_drops_total", "Packets/connections dropped because the source did not match the binding.", "counter")
	writeMetricHeader(&b, "relay_gateway_nat_type_mismatch_total", "Detected declared-versus-measured NAT type mismatch events.", "counter")
	writeMetricHeader(&b, "relay_gateway_recycles_total", "Mapping reclamation events.", "counter")
	now := time.Now()
	for _, id := range ids {
		type protoRow struct {
			active int
			port   int
			remain float64
			fwd    uint64
		}
		rows := map[string]protoRow{"udp": {}, "tcp": {}}
		active := 0
		for _, m := range mappings {
			if m.GatewayID != id {
				continue
			}
			m.mu.Lock()
			row := rows[m.Protocol]
			if m.active {
				row.active = 1
				active++
			}
			row.port = m.ExternalPort
			if m.active && m.LeaseExpires.After(now) {
				row.remain = m.LeaseExpires.Sub(now).Seconds()
			}
			row.fwd = m.forwarded.Load()
			m.mu.Unlock()
			rows[m.Protocol] = row
		}
		fmt.Fprintf(&b, "relay_gateway_active_mappings{gateway=%q} %d\n", id, active)
		for _, proto := range []string{"udp", "tcp"} {
			row := rows[proto]
			fmt.Fprintf(&b, "relay_mapping_active{gateway=%q,protocol=%q} %d\n", id, proto, row.active)
			fmt.Fprintf(&b, "relay_mapping_port{gateway=%q,protocol=%q} %d\n", id, proto, row.port)
			fmt.Fprintf(&b, "relay_mapping_keepalive_remaining_seconds{gateway=%q,protocol=%q} %.3f\n", id, proto, row.remain)
			fmt.Fprintf(&b, "relay_mapping_forwarded_total{gateway=%q,protocol=%q} %d\n", id, proto, row.fwd)
		}
		cs, _ := r.store.GetCounters(req.Context(), id)
		fmt.Fprintf(&b, "relay_gateway_source_mismatch_drops_total{gateway=%q} %d\n", id, cs.SourceMismatchDrops)
		fmt.Fprintf(&b, "relay_gateway_nat_type_mismatch_total{gateway=%q} %d\n", id, cs.NATTypeMismatchCount)
		fmt.Fprintf(&b, "relay_gateway_recycles_total{gateway=%q} %d\n", id, cs.RecycleCount)
	}
	_, _ = w.Write([]byte(b.String()))
}

func writeMetricHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}
