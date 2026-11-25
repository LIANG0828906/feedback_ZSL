package forward

import "fmt"

func loadV1(doc jsonObject) (*Engineering, error) {
	p := &Project{SourceSchema: SchemaV1}
	p.ID = firstString(doc, "project_id", "id")
	p.Name = firstString(doc, "project_name", "name")
	if p.ID == "" {
		return nil, NewBizError(CodeFieldRequired, "project_id", "project_id is required")
	}
	parseMeta(doc, p)

	peers, biz := parsePeersV12(doc["peers"], "$.peers")
	if biz != nil {
		return nil, biz
	}
	p.Peers = peers

	routes, biz := parseRoutesV1(doc["route_table"], "$.route_table")
	if biz != nil {
		return nil, biz
	}
	p.RouteTargets = routes

	tunnelsRaw, biz := asArray(doc["tunnel_table"], "$.tunnel_table")
	if biz != nil {
		return nil, biz
	}
	for i, raw := range tunnelsRaw {
		path := fmt.Sprintf("$.tunnel_table[%d]", i)
		m, b := asObject(raw, path)
		if b != nil {
			return nil, b
		}
		t, b := parseTunnelV1(m, path, i)
		if b != nil {
			return nil, b
		}
		p.Tunnels = append(p.Tunnels, t)
	}

	rulesRaw, biz := asArray(doc["split_rules"], "$.split_rules")
	if biz != nil {
		return nil, biz
	}
	for i, raw := range rulesRaw {
		path := fmt.Sprintf("$.split_rules[%d]", i)
		r, b := parseRuleV1(raw, path, i)
		if b != nil {
			return nil, b
		}
		p.Rules = append(p.Rules, r)
	}

	fb, biz := parseActionValue(doc["fallback_action"], "$.fallback_action")
	if biz != nil {
		return nil, biz
	}
	p.Fallback = fb

	events, biz := parseEvents(doc["events"], SchemaV1, "$.events")
	if biz != nil {
		return nil, biz
	}
	return &Engineering{Project: p, Events: events}, nil
}

func parseMeta(doc jsonObject, p *Project) {
	p.IntegerBits = 32
	p.MaxFragments = 16
	if v, ok, _ := optInt64(doc, "integer_width"); ok {
		p.IntegerBits = int(v)
	}
	if v, ok, _ := optInt64(doc, "integer_bits"); ok {
		p.IntegerBits = int(v)
	}
	if v, ok, _ := optInt64(doc, "max_fragments"); ok {
		p.MaxFragments = int(v)
	}
	if v, ok, _ := optInt64(doc, "max_pieces"); ok {
		p.MaxFragments = int(v)
	}
	if v, ok, _ := optInt64(doc, "max_fragment_pieces"); ok {
		p.MaxFragments = int(v)
	}
	if v, ok, _ := optInt64(doc, "tolerance_bytes"); ok {
		p.ToleranceBytes = v
	}
	if v, ok, _ := optInt64(doc, "byte_tolerance"); ok {
		p.ToleranceBytes = v
	}
}

func parsePeersV12(raw any, path string) ([]Peer, *BizError) {
	arr, biz := asArray(raw, path)
	if biz != nil {
		return nil, biz
	}
	out := []Peer{}
	for i, item := range arr {
		ip := fmt.Sprintf("%s[%d]", path, i)
		m, b := asObject(item, ip)
		if b != nil {
			return nil, b
		}
		id := firstString(m, "peer_id", "id")
		if id == "" {
			return nil, NewBizError(CodeFieldRequired, ip+".peer_id", "peer id required")
		}
		out = append(out, Peer{
			ID:        id,
			Name:      firstString(m, "display_name", "name", "label"),
			Endpoint:  firstString(m, "endpoint", "address"),
			DeclOrder: i,
		})
	}
	return out, nil
}

func parseRoutesV1(raw any, path string) ([]RouteTarget, *BizError) {
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
		dstRaw, ok := m["network"]
		if !ok {
			dstRaw = m["destination"]
		}
		var cidr string
		if _, isMap := dstRaw.(map[string]any); isMap {
			cidr, b = parseV1Network(dstRaw, rp+".network")
		} else if s := optString(jsonObject(map[string]any{"v": dstRaw}), "v"); s != "" {
			cidr = s
		}
		if b != nil {
			return nil, b
		}
		if cidr == "" {
			return nil, NewBizError(CodeFieldRequired, rp+".network", "route destination required")
		}
		out = append(out, RouteTarget{
			ID:          firstString(m, "target_id", "route_id", "id"),
			PeerID:      firstString(m, "peer_ref", "peer_id"),
			Destination: cidr,
			NextHop:     firstString(m, "next_hop", "gateway"),
			Egress:      firstString(m, "egress", "ifname", "interface"),
			DeclOrder:   i,
		})
	}
	return out, nil
}

func parseTunnelV1(m jsonObject, path string, order int) (Tunnel, *BizError) {
	t := Tunnel{DeclOrder: order}
	t.ID = firstString(m, "tunnel_id", "id")
	if t.ID == "" {
		return t, NewBizError(CodeFieldRequired, path+".tunnel_id", "tunnel id required")
	}
	t.PeerID = firstString(m, "peer_ref", "peer_id")
	t.Local = Endpoint{IP: firstString(m, "local_address", "local_ip"), Port: int(firstIntOrZero(m, "local_port"))}
	t.Remote = Endpoint{IP: firstString(m, "remote_address", "remote_ip"), Port: int(firstIntOrZero(m, "remote_port"))}
	t.OuterProtocol = firstString(m, "outer_protocol", "encap_proto")
	t.CipherSuite = firstString(m, "cipher_suite", "cipher")
	t.MTUBytes = firstIntOrZero(m, "mtu_bytes", "mtu")
	t.PathMTUBytes = firstIntOrZero(m, "path_mtu_bytes", "pmtu")
	t.HandshakeMS = firstIntOrZero(m, "handshake_ms", "handshake_milliseconds")
	t.RateLimitBps = firstIntOrZero(m, "rate_bytes_per_second", "rate_bps")
	ka, biz := parseKeepalive(m, path, "seconds")
	if biz != nil {
		return t, biz
	}
	t.KeepaliveMS = ka
	// V1 intentionally has no watermark field; degradation is applied by LoadEngineering.
	return t, nil
}

func firstIntOrZero(m jsonObject, keys ...string) int64 {
	n, err := firstInt(m, keys...)
	if err != nil {
		return 0
	}
	return n
}

func parseKeepalive(m jsonObject, path, primary string) (int64, *BizError) {
	sec, hasSec, b1 := optionalFirstInt(m, "keepalive_seconds", "ka_seconds")
	if b1 != nil {
		return 0, b1
	}
	ms, hasMS, b2 := optionalFirstInt(m, "keepalive_ms", "keepalive_milliseconds", "ka_ms", "interval_ms", "keepalive_interval_ms")
	if b2 != nil {
		return 0, b2
	}
	if hasSec && hasMS && sec*1000 != ms {
		return 0, NewBizError(CodeFieldConflict, path+".keepalive",
			"conflicting keepalive units: %d seconds normalizes to %d ms but ms field is %d", sec, sec*1000, ms).
			WithContext("keepalive_seconds", sec).WithContext("keepalive_ms", ms)
	}
	if primary == "seconds" {
		if hasSec {
			return sec * 1000, nil
		}
		if hasMS {
			return ms, nil
		}
		return 0, NewBizError(CodeFieldRequired, path+".keepalive_seconds", "keepalive is required")
	}
	if hasMS {
		return ms, nil
	}
	if hasSec {
		return sec * 1000, nil
	}
	return 0, NewBizError(CodeFieldRequired, path+".keepalive_ms", "keepalive is required")
}

func optionalFirstInt(m jsonObject, keys ...string) (int64, bool, *BizError) {
	for _, key := range keys {
		if raw, ok := m[key]; ok && raw != nil {
			n, biz := toInt64(raw, key)
			return n, true, biz
		}
	}
	return 0, false, nil
}

func parseRuleV1(raw any, path string, order int) (Rule, *BizError) {
	m, biz := asObject(raw, path)
	if biz != nil {
		return Rule{}, biz
	}
	r := Rule{DeclOrder: order}
	r.ID = firstString(m, "rule_id", "id")
	if r.ID == "" {
		return r, NewBizError(CodeFieldRequired, path+".rule_id", "rule id required")
	}
	r.Priority = int(firstIntOrZero(m, "priority", "rule_order", "order"))
	r.GroupPeerID = firstString(m, "peer_group", "group_peer_id")

	netArr, b := asArray(m["networks"], path+".networks")
	if b != nil {
		return r, b
	}
	for i, item := range netArr {
		cidr, b := parseV1Network(item, fmt.Sprintf("%s.networks[%d]", path, i))
		if b != nil {
			return r, b
		}
		r.CIDRs = append(r.CIDRs, cidr)
	}
	r.DomainSuffixes = stringSlice(m["domain_suffixes"])
	r.PortRanges, b = parsePortRanges(m["port_ranges"], path+".port_ranges", []string{"from", "low", "start"}, []string{"to", "high", "end"})
	if b != nil {
		return r, b
	}
	actionRaw, ok := m["action"]
	if !ok {
		return r, NewBizError(CodeFieldRequired, path+".action", "action required")
	}
	action, b := parseActionValue(actionRaw, path+".action")
	if b != nil {
		return r, b
	}
	r.Action = action
	return r, nil
}

func stringSlice(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
