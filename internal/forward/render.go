package forward

import (
	"encoding/json"
	"fmt"
	"net"
)

func RenderDocument(schema string, p *Project, events []Event) ([]byte, error) {
	var doc map[string]any
	switch schema {
	case SchemaV1:
		doc = renderV1(p, events)
	case SchemaV2:
		doc = renderV2(p, events)
	case SchemaV3:
		doc = renderV3(p, events)
	default:
		return nil, NewBizError(CodeSchemaUnknown, "schema", "cannot render unknown schema %q", schema)
	}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func actionString(a Action) string {
	if a.Type == ActionTunnel {
		return ActionTunnel + ":" + a.TunnelID
	}
	return a.Type
}

func actionMap(a Action) map[string]any {
	if a.Type == ActionTunnel {
		return map[string]any{"type": ActionTunnel, "tunnel_id": a.TunnelID}
	}
	return map[string]any{"type": a.Type}
}

func cidrToV1Network(cidr string) map[string]any {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return map[string]any{"address": cidr}
	}
	ones, _ := network.Mask.Size()
	mask, _ := PrefixToDottedMask(ones)
	return map[string]any{"address": ip.Mask(network.Mask).String(), "netmask": mask}
}

func cidrToV3Prefix(cidr string) map[string]any {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return map[string]any{"address": cidr, "prefix_length": 32}
	}
	ones, _ := network.Mask.Size()
	return map[string]any{"address": ip.Mask(network.Mask).String(), "prefix_length": ones}
}

func renderV1(p *Project, events []Event) map[string]any {
	peers := []any{}
	for _, peer := range p.Peers {
		peers = append(peers, map[string]any{"peer_id": peer.ID, "display_name": peer.Name, "endpoint": peer.Endpoint})
	}
	routes := []any{}
	for _, rt := range p.RouteTargets {
		routes = append(routes, map[string]any{
			"target_id": rt.ID, "peer_ref": rt.PeerID,
			"network": cidrToV1Network(rt.Destination), "next_hop": rt.NextHop, "egress": rt.Egress,
		})
	}
	tunnels := []any{}
	for _, t := range p.Tunnels {
		tunnels = append(tunnels, map[string]any{
			"tunnel_id": t.ID, "peer_ref": t.PeerID,
			"local_address": t.Local.IP, "local_port": t.Local.Port,
			"remote_address": t.Remote.IP, "remote_port": t.Remote.Port,
			"outer_protocol": t.OuterProtocol, "cipher_suite": t.CipherSuite,
			"mtu_bytes": t.MTUBytes, "path_mtu_bytes": t.PathMTUBytes,
			"keepalive_seconds":     t.KeepaliveMS / 1000,
			"handshake_ms":          t.HandshakeMS,
			"rate_bytes_per_second": t.RateLimitBps,
		})
	}
	rules := []any{}
	for _, r := range p.Rules {
		nets := []any{}
		for _, c := range r.CIDRs {
			nets = append(nets, cidrToV1Network(c))
		}
		ports := []any{}
		for _, pr := range r.PortRanges {
			ports = append(ports, map[string]any{"from": pr.Start, "to": pr.End})
		}
		rules = append(rules, map[string]any{
			"rule_id": r.ID, "decl_order": r.DeclOrder, "priority": r.Priority,
			"peer_group": r.GroupPeerID, "networks": nets, "domain_suffixes": r.DomainSuffixes,
			"port_ranges": ports, "action": actionString(r.Action),
		})
	}
	return map[string]any{
		"schema_version": "1.0", "format": "flat-literal-seconds-bytes",
		"project_id": p.ID, "project_name": p.Name,
		"integer_width": p.IntegerBits, "max_fragments": p.MaxFragments, "tolerance_bytes": p.ToleranceBytes,
		"peers": peers, "route_table": routes, "tunnel_table": tunnels, "split_rules": rules,
		"fallback_action": actionString(p.Fallback),
		"events":          renderEventsV1(events),
	}
}

func renderEventsV1(events []Event) []any {
	out := []any{}
	for _, ev := range events {
		m := map[string]any{
			"event_id": ev.ID, "event_type": ev.Kind,
			"at_seconds": float64(ev.AtMs) / 1000.0,
		}
		if ev.TunnelID != "" {
			m["tunnel_id"] = ev.TunnelID
		}
		if ev.ConfigRevision != "" {
			m["config_revision"] = ev.ConfigRevision
		}
		if ev.Packet != nil {
			m["packet"] = renderPacket(ev.Packet)
		}
		out = append(out, m)
	}
	return out
}

func renderPacket(pkt *PacketSpec) map[string]any {
	return map[string]any{
		"id": pkt.ID, "at_ms": pkt.AtMs, "src_ip": pkt.SrcIP, "dst_ip": pkt.DstIP,
		"domain": pkt.Domain, "protocol": pkt.Protocol, "src_port": pkt.SrcPort,
		"dst_port": pkt.DstPort, "payload_bytes": pkt.PayloadBytes,
	}
}

func renderV2(p *Project, events []Event) map[string]any {
	peers := []any{}
	for _, peer := range p.Peers {
		peers = append(peers, map[string]any{"peer_id": peer.ID, "label": peer.Name, "endpoint": peer.Endpoint})
	}
	routes := []any{}
	for _, rt := range p.RouteTargets {
		routes = append(routes, map[string]any{
			"route_id": rt.ID, "peer_id": rt.PeerID, "cidr": rt.Destination,
			"gateway": rt.NextHop, "ifname": rt.Egress,
		})
	}
	tunnels := []any{}
	for _, t := range p.Tunnels {
		tm := map[string]any{
			"tid": t.ID, "peer": t.PeerID,
			"local_ipv4": t.Local.IP, "lport": t.Local.Port,
			"remote_ipv4": t.Remote.IP, "rport": t.Remote.Port,
			"encap": t.OuterProtocol, "cipher": t.CipherSuite,
			"mtu": t.MTUBytes, "pmtu": t.PathMTUBytes,
			"keepalive_ms": t.KeepaliveMS, "handshake_ms": t.HandshakeMS,
			"rate_kib_s": t.RateLimitBps / 1024,
		}
		if t.KeepaliveWatermarkBytes != nil {
			tm["watermark_bytes"] = *t.KeepaliveWatermarkBytes
		}
		tunnels = append(tunnels, tm)
	}
	rules := []any{}
	for _, r := range p.Rules {
		ports := []any{}
		for _, pr := range r.PortRanges {
			ports = append(ports, map[string]any{"lo": pr.Start, "hi": pr.End})
		}
		rules = append(rules, map[string]any{
			"rid": r.ID, "decl_order": r.DeclOrder, "order": r.Priority,
			"peer_group": r.GroupPeerID, "dest_cidrs": r.CIDRs, "suffixes": r.DomainSuffixes,
			"dport_ranges": ports, "verdict": actionMap(r.Action),
		})
	}
	return map[string]any{
		"apiVersion": "2.0", "kind": "FlatIntegerEngineering",
		"metadata": map[string]any{
			"id": p.ID, "counter_bits": p.IntegerBits, "max_pieces": p.MaxFragments, "byte_tolerance": p.ToleranceBytes,
		},
		"name": p.Name, "peer_endpoints": peers, "routes": routes,
		"forward_tunnels": tunnels, "shunt_rules": rules,
		"default_action": actionMap(p.Fallback),
		"timeline":       renderEventsMs(events, "at_ms"),
	}
}

func renderEventsMs(events []Event, timeKey string) []any {
	out := []any{}
	for _, ev := range events {
		m := map[string]any{
			"event_id": ev.ID, "event_type": ev.Kind, timeKey: ev.AtMs,
		}
		if ev.TunnelID != "" {
			m["tid"] = ev.TunnelID
		}
		if ev.ConfigRevision != "" {
			m["revision"] = ev.ConfigRevision
		}
		if ev.Packet != nil {
			m["packet"] = renderPacket(ev.Packet)
		}
		out = append(out, m)
	}
	return out
}

func renderV3(p *Project, events []Event) map[string]any {
	peerTree := []any{}
	for _, peer := range p.Peers {
		tunnels := []any{}
		for _, t := range p.Tunnels {
			if t.PeerID != peer.ID {
				continue
			}
			tm := map[string]any{
				"tunnel_id": t.ID,
				"local":     map[string]any{"address": t.Local.IP, "port": t.Local.Port},
				"remote":    map[string]any{"address": t.Remote.IP, "port": t.Remote.Port},
				"encap":     t.OuterProtocol, "cipher_suite": t.CipherSuite,
				"mtu_bytes": t.MTUBytes, "path_mtu_bytes": t.PathMTUBytes,
				"keepalive_interval_ms": t.KeepaliveMS, "handshake_ms": t.HandshakeMS,
				"rate_limit_kibps": t.RateLimitBps / 1024,
			}
			if t.KeepaliveWatermarkBytes != nil {
				tm["watermark_kib"] = *t.KeepaliveWatermarkBytes / 1024
			}
			tunnels = append(tunnels, tm)
		}
		rules := []any{}
		for _, r := range p.Rules {
			if r.GroupPeerID != peer.ID {
				continue
			}
			prefixes := []any{}
			for _, c := range r.CIDRs {
				prefixes = append(prefixes, cidrToV3Prefix(c))
			}
			ports := []any{}
			for _, pr := range r.PortRanges {
				ports = append(ports, map[string]any{"low": pr.Start, "high": pr.End})
			}
			match := map[string]any{"domain_suffixes": r.DomainSuffixes, "destination_port_ranges": ports}
			if len(prefixes) == 1 {
				match["prefix"] = prefixes[0]
			} else if len(prefixes) > 1 {
				match["prefix_list"] = prefixes
			}
			rules = append(rules, map[string]any{
				"rule_id": r.ID, "decl_order": r.DeclOrder, "precedence": r.Priority,
				"match": match, "verdict": actionMap(r.Action),
			})
		}
		peerTree = append(peerTree, map[string]any{
			"peer_id": peer.ID, "display_name": peer.Name, "endpoint": peer.Endpoint,
			"tunnels": tunnels, "policy_set": map[string]any{"rules": rules},
		})
	}
	routes := []any{}
	for _, rt := range p.RouteTargets {
		routes = append(routes, map[string]any{
			"route_id": rt.ID, "cidr": rt.Destination, "gateway": rt.NextHop, "egress": rt.Egress,
		})
	}
	return map[string]any{
		"schemaVersion": "3.0",
		"engineering": map[string]any{
			"metadata": map[string]any{
				"engineering_id": p.ID, "integer_bits": p.IntegerBits,
				"max_fragment_pieces": p.MaxFragments, "byte_tolerance": p.ToleranceBytes,
			},
			"name": p.Name, "peer_tree": peerTree, "outer_routes": routes,
			"fallback_verdict": actionMap(p.Fallback),
		},
		"timeline": renderEventsMs(events, "logical_time_ms"),
	}
}

func MustLoadSampleSchema(sample *Sample, schema string) *Engineering {
	eng, err := LoadEngineering(schema, sample.Documents[schema])
	if err != nil {
		panic(fmt.Sprintf("generated %s document failed to load: %v", schema, err))
	}
	return eng
}
