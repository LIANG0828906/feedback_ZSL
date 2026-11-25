package forward

import "fmt"

func loadV2(doc jsonObject) (*Engineering, error) {
	p := &Project{SourceSchema: SchemaV2}
	meta, _ := asObject(doc["metadata"], "$.metadata")
	if meta != nil {
		p.ID = firstString(meta, "engineering_id", "project_id", "id")
	}
	if p.ID == "" {
		p.ID = firstString(doc, "project_id", "id")
	}
	if p.ID == "" {
		return nil, NewBizError(CodeFieldRequired, "$.metadata.id", "engineering id required")
	}
	parseMeta(doc, p)
	if meta != nil {
		parseMeta(meta, p)
	}
	p.Name = firstString(doc, "name", "project_name")

	peers, biz := parsePeersV12(doc["peer_endpoints"], "$.peer_endpoints")
	if biz != nil {
		return nil, biz
	}
	p.Peers = peers

	routes, biz := parseRoutesCIDR(doc["routes"], "$.routes")
	if biz != nil {
		return nil, biz
	}
	p.RouteTargets = routes

	tunnelsRaw, biz := asArray(doc["forward_tunnels"], "$.forward_tunnels")
	if biz != nil {
		return nil, biz
	}
	for i, raw := range tunnelsRaw {
		path := fmt.Sprintf("$.forward_tunnels[%d]", i)
		m, b := asObject(raw, path)
		if b != nil {
			return nil, b
		}
		t, b := parseTunnelV2(m, path, i)
		if b != nil {
			return nil, b
		}
		p.Tunnels = append(p.Tunnels, t)
	}

	rulesRaw, biz := asArray(doc["shunt_rules"], "$.shunt_rules")
	if biz != nil {
		return nil, biz
	}
	for i, raw := range rulesRaw {
		path := fmt.Sprintf("$.shunt_rules[%d]", i)
		r, b := parseRuleV2(raw, path, i)
		if b != nil {
			return nil, b
		}
		p.Rules = append(p.Rules, r)
	}

	fb, b := parseActionValue(doc["default_action"], "$.default_action")
	if b != nil {
		return nil, b
	}
	p.Fallback = fb
	events, b := parseEvents(doc["timeline"], SchemaV2, "$.timeline")
	if b != nil {
		return nil, b
	}
	return &Engineering{Project: p, Events: events}, nil
}

func parseRoutesCIDR(raw any, path string) ([]RouteTarget, *BizError) {
	arr, biz := asArray(raw, path)
	if biz != nil {
		return nil, biz
	}
	out := []RouteTarget{}
	for i, item := range arr {
		rp := fmt.Sprintf("%s[%d]", path, i)
		m, b := asObject(item, rp)
		if b != nil {
			return nil, b
		}
		cidr := firstString(m, "cidr", "destination_cidr", "network")
		if cidr == "" {
			return nil, NewBizError(CodeFieldRequired, rp+".cidr", "route cidr required")
		}
		if _, err := ParseCIDR(cidr); err != nil {
			return nil, NewBizError(CodeFieldType, rp+".cidr", "invalid cidr: %v", err)
		}
		out = append(out, RouteTarget{
			ID:          firstString(m, "route_id", "id", "target_id"),
			PeerID:      firstString(m, "peer_id", "peer_ref"),
			Destination: cidr,
			NextHop:     firstString(m, "gateway", "next_hop"),
			Egress:      firstString(m, "ifname", "egress", "interface"),
			DeclOrder:   i,
		})
	}
	return out, nil
}

func parseTunnelV2(m jsonObject, path string, order int) (Tunnel, *BizError) {
	t := Tunnel{DeclOrder: order}
	t.ID = firstString(m, "tid", "tunnel_id", "id")
	if t.ID == "" {
		return t, NewBizError(CodeFieldRequired, path+".tid", "tunnel id required")
	}
	t.PeerID = firstString(m, "peer", "peer_ref", "peer_id")
	t.Local = Endpoint{IP: firstString(m, "local_ipv4", "local_ip"), Port: int(firstIntOrZero(m, "lport", "local_port"))}
	t.Remote = Endpoint{IP: firstString(m, "remote_ipv4", "remote_ip"), Port: int(firstIntOrZero(m, "rport", "remote_port"))}
	t.OuterProtocol = firstString(m, "encap", "outer_protocol", "encap_proto")
	t.CipherSuite = firstString(m, "cipher", "cipher_suite")
	t.MTUBytes = firstIntOrZero(m, "mtu", "mtu_bytes")
	t.PathMTUBytes = firstIntOrZero(m, "pmtu", "path_mtu_bytes")
	t.HandshakeMS = firstIntOrZero(m, "handshake_ms")
	if kib, ok, b := optionalFirstInt(m, "rate_kib_s", "rate_kib_per_second"); b != nil {
		return t, b
	} else if ok {
		t.RateLimitBps = kib * 1024
	} else {
		t.RateLimitBps = firstIntOrZero(m, "rate_bps", "rate_bytes_per_second")
	}
	ka, b := parseKeepalive(m, path, "ms")
	if b != nil {
		return t, b
	}
	t.KeepaliveMS = ka
	if wm, ok, b := optionalFirstInt(m, "watermark_bytes", "keepalive_watermark_bytes"); b != nil {
		return t, b
	} else if ok {
		v := wm
		t.KeepaliveWatermarkBytes = &v
		t.WatermarkEnabled = true
	}
	return t, nil
}

func parseRuleV2(raw any, path string, order int) (Rule, *BizError) {
	m, biz := asObject(raw, path)
	if biz != nil {
		return Rule{}, biz
	}
	r := Rule{DeclOrder: int(firstIntOrZero(m, "decl_order"))}
	if r.DeclOrder == 0 && firstString(m, "decl_order") == "" {
		r.DeclOrder = order
	}
	r.ID = firstString(m, "rid", "rule_id", "id")
	if r.ID == "" {
		return r, NewBizError(CodeFieldRequired, path+".rid", "rule id required")
	}
	r.Priority = int(firstIntOrZero(m, "order", "priority", "precedence"))
	r.GroupPeerID = firstString(m, "peer_group", "group_peer_id")
	for i, rawCIDR := range rawSlice(m["dest_cidrs"], m["cidrs"]) {
		cidr, b := parseCIDRString(rawCIDR, fmt.Sprintf("%s.dest_cidrs[%d]", path, i))
		if b != nil {
			return r, b
		}
		r.CIDRs = append(r.CIDRs, cidr)
	}
	r.DomainSuffixes = stringSlice(firstRawSlice(m, "suffixes", "domain_suffixes"))
	r.PortRanges, biz = parsePortRanges(firstRaw(m, "dport_ranges", "port_ranges"), path+".dport_ranges",
		[]string{"lo", "start", "from"}, []string{"hi", "end", "to"})
	if biz != nil {
		return r, biz
	}
	action, b := parseActionValue(m["verdict"], path+".verdict")
	if b != nil {
		action, b = parseActionValue(m["action"], path+".action")
		if b != nil {
			return r, b
		}
	}
	r.Action = action
	return r, nil
}

func rawSlice(values ...any) []any {
	for _, v := range values {
		if a, ok := v.([]any); ok {
			return a
		}
	}
	return nil
}

func firstRaw(m jsonObject, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func firstRawSlice(m jsonObject, keys ...string) any {
	return firstRaw(m, keys...)
}

func loadV3(doc jsonObject) (*Engineering, error) {
	engRaw, biz := asObject(doc["engineering"], "$.engineering")
	if biz != nil {
		return nil, biz
	}
	if engRaw == nil {
		engRaw = doc
	}
	p := &Project{SourceSchema: SchemaV3}
	meta, _ := asObject(engRaw["metadata"], "$.engineering.metadata")
	if meta != nil {
		p.ID = firstString(meta, "engineering_id", "id")
		parseMeta(meta, p)
	} else {
		parseMeta(engRaw, p)
	}
	if p.ID == "" {
		p.ID = firstString(engRaw, "engineering_id", "id")
	}
	if p.ID == "" {
		return nil, NewBizError(CodeFieldRequired, "$.engineering.metadata.engineering_id", "id required")
	}
	p.Name = firstString(engRaw, "name")

	if fbRaw, ok := engRaw["fallback_verdict"]; ok {
		fb, b := parseActionValue(fbRaw, "$.engineering.fallback_verdict")
		if b != nil {
			return nil, b
		}
		p.Fallback = fb
	} else {
		return nil, NewBizError(CodeFieldRequired, "$.engineering.fallback_verdict", "fallback verdict required")
	}

	routes, b := parseRoutesCIDR(engRaw["outer_routes"], "$.engineering.outer_routes")
	if b != nil {
		return nil, b
	}
	p.RouteTargets = routes

	tunnelOrder := 0
	ruleOrder := 0
	peerTree, b := asArray(engRaw["peer_tree"], "$.engineering.peer_tree")
	if b != nil {
		return nil, b
	}
	for pi, peerRaw := range peerTree {
		pp := fmt.Sprintf("$.engineering.peer_tree[%d]", pi)
		pm, b := asObject(peerRaw, pp)
		if b != nil {
			return nil, b
		}
		peerID := firstString(pm, "peer_id", "id")
		if peerID == "" {
			return nil, NewBizError(CodeFieldRequired, pp+".peer_id", "peer id required")
		}
		p.Peers = append(p.Peers, Peer{
			ID:        peerID,
			Name:      firstString(pm, "display_name", "name"),
			Endpoint:  firstString(pm, "endpoint"),
			DeclOrder: pi,
		})

		tunnels, b := asArray(pm["tunnels"], pp+".tunnels")
		if b != nil {
			return nil, b
		}
		for ti, tr := range tunnels {
			tp := fmt.Sprintf("%s.tunnels[%d]", pp, ti)
			tm, b := asObject(tr, tp)
			if b != nil {
				return nil, b
			}
			t, b := parseTunnelV3(tm, tp, tunnelOrder, peerID)
			if b != nil {
				return nil, b
			}
			p.Tunnels = append(p.Tunnels, t)
			tunnelOrder++
		}

		policy, _ := asObject(pm["policy_set"], pp+".policy_set")
		if policy != nil {
			rules, b := asArray(policy["rules"], pp+".policy_set.rules")
			if b != nil {
				return nil, b
			}
			for ri, rr := range rules {
				rp := fmt.Sprintf("%s.policy_set.rules[%d]", pp, ri)
				r, b := parseRuleV3(rr, rp, ruleOrder, peerID)
				if b != nil {
					return nil, b
				}
				p.Rules = append(p.Rules, r)
				ruleOrder++
			}
		}
	}

	events, b := parseEvents(doc["timeline"], SchemaV3, "$.timeline")
	if b != nil {
		return nil, b
	}
	return &Engineering{Project: p, Events: events}, nil
}

func parseTunnelV3(m jsonObject, path string, order int, peerID string) (Tunnel, *BizError) {
	t := Tunnel{DeclOrder: order, PeerID: peerID}
	t.ID = firstString(m, "tunnel_id", "id", "name")
	if t.ID == "" {
		return t, NewBizError(CodeFieldRequired, path+".tunnel_id", "tunnel id required")
	}
	if ref := firstString(m, "peer_ref", "peer_id"); ref != "" {
		t.PeerID = ref
	}
	local, b := asObject(m["local"], path+".local")
	if b != nil {
		return t, b
	}
	remote, b := asObject(m["remote"], path+".remote")
	if b != nil {
		return t, b
	}
	t.Local = Endpoint{IP: firstString(local, "address", "ip"), Port: int(firstIntOrZero(local, "port"))}
	t.Remote = Endpoint{IP: firstString(remote, "address", "ip"), Port: int(firstIntOrZero(remote, "port"))}
	t.OuterProtocol = firstString(m, "encap", "outer_protocol")
	t.CipherSuite = firstString(m, "cipher_suite", "cipher")
	t.MTUBytes = firstIntOrZero(m, "mtu_bytes", "mtu")
	t.PathMTUBytes = firstIntOrZero(m, "path_mtu_bytes", "pmtu")
	t.HandshakeMS = firstIntOrZero(m, "handshake_ms")
	t.KeepaliveMS = firstIntOrZero(m, "keepalive_interval_ms", "keepalive_ms")
	if t.KeepaliveMS == 0 {
		if sec := firstIntOrZero(m, "keepalive_seconds"); sec > 0 {
			t.KeepaliveMS = sec * 1000
		}
	}
	if kib, ok, b := optionalFirstInt(m, "rate_limit_kibps", "rate_kib_s"); b != nil {
		return t, b
	} else if ok {
		t.RateLimitBps = kib * 1024
	}
	if wm, ok, b := optionalFirstInt(m, "watermark_kib"); b != nil {
		return t, b
	} else if ok {
		v := wm * 1024
		t.KeepaliveWatermarkBytes = &v
		t.WatermarkEnabled = true
	} else if wm, ok, b := optionalFirstInt(m, "watermark_bytes"); b != nil {
		return t, b
	} else if ok {
		v := wm
		t.KeepaliveWatermarkBytes = &v
		t.WatermarkEnabled = true
	}
	return t, nil
}

func parseRuleV3(raw any, path string, order int, peerID string) (Rule, *BizError) {
	m, b := asObject(raw, path)
	if b != nil {
		return Rule{}, b
	}
	r := Rule{DeclOrder: int(firstIntOrZero(m, "decl_order")), GroupPeerID: peerID}
	if r.DeclOrder == 0 {
		r.DeclOrder = order
	}
	r.ID = firstString(m, "rule_id", "id", "name")
	if r.ID == "" {
		return r, NewBizError(CodeFieldRequired, path+".name", "rule id required")
	}
	r.Priority = int(firstIntOrZero(m, "precedence", "priority", "order"))
	match, b := asObject(m["match"], path+".match")
	if b != nil {
		return r, b
	}
	prefixes := rawSlice(match["prefix_list"], match["prefixes"])
	if prefixes != nil {
		for i, praw := range prefixes {
			cidr, b := parseV3Prefix(praw, fmt.Sprintf("%s.match.prefix_list[%d]", path, i))
			if b != nil {
				return r, b
			}
			r.CIDRs = append(r.CIDRs, cidr)
		}
	} else if praw, ok := match["prefix"]; ok {
		cidr, b := parseV3Prefix(praw, path+".match.prefix")
		if b != nil {
			return r, b
		}
		r.CIDRs = append(r.CIDRs, cidr)
	}
	r.DomainSuffixes = stringSlice(match["domain_suffixes"])
	r.PortRanges, b = parsePortRanges(match["destination_port_ranges"], path+".match.destination_port_ranges",
		[]string{"low", "start"}, []string{"high", "end"})
	if b != nil {
		return r, b
	}
	actionRaw := firstRaw(m, "verdict", "action")
	action, b := parseActionValue(actionRaw, path+".verdict")
	if b != nil {
		return r, b
	}
	r.Action = action
	return r, nil
}
