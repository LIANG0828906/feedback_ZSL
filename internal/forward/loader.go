package forward

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

func NormalizeSchema(s string) (string, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "forward.")
	switch s {
	case "v1", "1", "1.0", "flat-literal", "forward.v1":
		return SchemaV1, true
	case "v2", "2", "2.0", "flat-integer", "forward.v2":
		return SchemaV2, true
	case "v3", "3", "3.0", "peer-tree", "forward.v3":
		return SchemaV3, true
	default:
		return "", false
	}
}

func DetectSchema(data []byte) (string, error) {
	doc, bizErr := decodeObject(data, "$")
	if bizErr != nil {
		return "", bizErr
	}
	for _, key := range []string{"schema", "schema_version", "apiVersion", "schemaVersion"} {
		if raw, ok := doc[key]; ok {
			if s, ok := raw.(string); ok {
				if normalized, ok := NormalizeSchema(s); ok {
					return normalized, nil
				}
				return "", NewBizError(CodeSchemaUnknown, key, "unsupported schema version %q", s).
					WithContext("supported", []string{"1.0", "2.0", "3.0"})
			}
		}
	}
	return "", NewBizError(CodeSchemaUnknown, "schema_version", "document does not declare a recognized schema version")
}

func LoadEngineering(schemaHint string, data []byte) (*Engineering, error) {
	schema := schemaHint
	if schema == "" {
		detected, err := DetectSchema(data)
		if err != nil {
			return nil, err
		}
		schema = detected
	} else {
		normalized, ok := NormalizeSchema(schemaHint)
		if !ok {
			return nil, NewBizError(CodeSchemaUnknown, "schema", "unsupported schema version %q", schemaHint).
				WithContext("supported", []string{SchemaV1, SchemaV2, SchemaV3})
		}
		schema = normalized
	}

	doc, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}

	var eng *Engineering
	switch schema {
	case SchemaV1:
		eng, err = loadV1(doc)
	case SchemaV2:
		eng, err = loadV2(doc)
	case SchemaV3:
		eng, err = loadV3(doc)
	}
	if err != nil {
		return nil, err
	}
	eng.Schema = schema
	if eng.Project == nil {
		return nil, NewBizError(CodeFieldRequired, "project", "normalized project is missing")
	}
	normalizeProjectOrder(eng.Project)
	eng.Project.SourceSchema = schema
	eng.Project.ContentHash = eng.Project.CanonicalHash()
	if schema == SchemaV1 {
		eng.Project.WatermarkDegraded = true
		for i := range eng.Project.Tunnels {
			eng.Project.Tunnels[i].WatermarkEnabled = false
		}
	}
	if err := ValidateProject(eng.Project); err != nil {
		return nil, err
	}
	return eng, nil
}

func normalizeProjectOrder(p *Project) {
	sort.SliceStable(p.Peers, func(i, j int) bool { return p.Peers[i].DeclOrder < p.Peers[j].DeclOrder })
	sort.SliceStable(p.Tunnels, func(i, j int) bool { return p.Tunnels[i].DeclOrder < p.Tunnels[j].DeclOrder })
	sort.SliceStable(p.RouteTargets, func(i, j int) bool { return p.RouteTargets[i].DeclOrder < p.RouteTargets[j].DeclOrder })
	sort.SliceStable(p.Rules, func(i, j int) bool {
		if p.Rules[i].Priority != p.Rules[j].Priority {
			return p.Rules[i].Priority < p.Rules[j].Priority
		}
		return p.Rules[i].DeclOrder < p.Rules[j].DeclOrder
	})
}

func parseActionValue(v any, path string) (Action, *BizError) {
	switch value := v.(type) {
	case string:
		if strings.Contains(value, ":") {
			parts := strings.SplitN(value, ":", 2)
			if parts[0] != ActionTunnel {
				return Action{}, NewBizError(CodeActionInvalid, path, "unsupported action prefix %q", parts[0])
			}
			return Action{Type: ActionTunnel, TunnelID: parts[1]}, nil
		}
		switch value {
		case ActionDirect, ActionDrop, ActionReject:
			return Action{Type: value}, nil
		default:
			return Action{}, NewBizError(CodeActionInvalid, path, "unsupported action %q", value)
		}
	case map[string]any:
		m := jsonObject(value)
		kind := optString(m, "type")
		if kind == "" {
			kind = optString(m, "action")
		}
		if kind == "route" {
			kind = ActionTunnel
		}
		if kind == ActionTunnel {
			tid := optString(m, "tunnel_id")
			if tid == "" {
				tid = optString(m, "egress")
			}
			if tid == "" {
				tid = optString(m, "tunnel")
			}
			if tid == "" {
				return Action{}, NewBizError(CodeFieldRequired, path+".tunnel_id", "tunnel action requires target")
			}
			return Action{Type: ActionTunnel, TunnelID: tid}, nil
		}
		if kind == ActionDirect || kind == ActionDrop || kind == ActionReject {
			return Action{Type: kind}, nil
		}
		return Action{}, NewBizError(CodeActionInvalid, path+".type", "unsupported action %q", kind)
	case nil:
		return Action{}, NewBizError(CodeFieldRequired, path, "action is required")
	default:
		return Action{}, NewBizError(CodeFieldType, path, "unsupported action value type %T", v)
	}
}

func parsePortRanges(raw any, path string, lowKeys, highKeys []string) ([]PortRange, *BizError) {
	arr, biz := asArray(raw, path)
	if biz != nil {
		return nil, biz
	}
	out := []PortRange{}
	for i, item := range arr {
		m, biz := asObject(item, fmt.Sprintf("%s[%d]", path, i))
		if biz != nil {
			return nil, biz
		}
		low, err1 := firstInt(m, lowKeys...)
		high, err2 := firstInt(m, highKeys...)
		if err1 != nil {
			return nil, err1
		}
		if err2 != nil {
			return nil, err2
		}
		out = append(out, PortRange{Start: int(low), End: int(high)})
	}
	return out, nil
}

func firstInt(m jsonObject, keys ...string) (int64, *BizError) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return toInt64(v, key)
		}
	}
	return 0, NewBizError(CodeFieldRequired, strings.Join(keys, "|"), "one of fields %v is required", keys)
}

func parseCIDRString(raw any, path string) (string, *BizError) {
	s, biz := getString(jsonObject(map[string]any{"v": raw}), path, "v")
	if biz != nil {
		return "", biz
	}
	if _, err := ParseCIDR(s); err != nil {
		return "", NewBizError(CodeFieldType, path, "invalid CIDR %q", s)
	}
	return s, nil
}

func parseV1Network(item any, path string) (string, *BizError) {
	m, biz := asObject(item, path)
	if biz != nil {
		return "", biz
	}
	addr, biz := getString(m, path, "address")
	if biz != nil {
		return "", biz
	}
	mask, biz := getString(m, path, "netmask")
	if biz != nil {
		return "", biz
	}
	prefix, err := DottedMaskToPrefix(mask)
	if err != nil {
		if bizErr, ok := AsBizError(err); ok {
			bizErr.Path = path + ".netmask"
			return "", bizErr
		}
		return "", WrapBiz(CodeFieldType, path+".netmask", err, "invalid dotted mask")
	}
	return fmt.Sprintf("%s/%d", addr, prefix), nil
}

func parseV3Prefix(item any, path string) (string, *BizError) {
	m, biz := asObject(item, path)
	if biz != nil {
		return "", biz
	}
	addr, biz := getString(m, path, "address")
	if biz != nil {
		// Also accept addr for resilience.
		addr = optString(m, "addr")
		if addr == "" {
			return "", biz
		}
	}
	prefix, biz := getInt64(m, path, "prefix_length")
	if biz != nil {
		prefix, biz = getInt64(m, path, "prefix")
		if biz != nil {
			return "", biz
		}
	}
	if prefix < 0 || prefix > 32 {
		return "", NewBizError(CodeFieldOutOfRange, path+".prefix_length", "prefix must be 0..32")
	}
	return fmt.Sprintf("%s/%d", addr, prefix), nil
}

func parsePacket(raw any, path string) (*PacketSpec, *BizError) {
	if raw == nil {
		return nil, nil
	}
	m, biz := asObject(raw, path)
	if biz != nil {
		return nil, biz
	}
	pkt := &PacketSpec{}
	if pkt.ID = optString(m, "id"); pkt.ID == "" {
		pkt.ID = optString(m, "packet_id")
	}
	if pkt.ID == "" {
		return nil, NewBizError(CodeFieldRequired, path+".id", "packet id is required")
	}
	if v, ok, biz := optInt64(m, "at_ms"); biz != nil {
		return nil, biz
	} else if ok {
		pkt.AtMs = v
	}
	pkt.SrcIP = optString(m, "src_ip")
	pkt.DstIP = optString(m, "dst_ip")
	pkt.Domain = optString(m, "domain")
	pkt.Protocol = optString(m, "protocol")
	if pkt.Protocol == "" {
		pkt.Protocol = "udp"
	}
	if v, ok, biz := optInt64(m, "src_port"); biz != nil {
		return nil, biz
	} else if ok {
		pkt.SrcPort = int(v)
	}
	if v, ok, biz := optInt64(m, "dst_port"); biz != nil {
		return nil, biz
	} else if ok {
		pkt.DstPort = int(v)
	}
	if v, ok, biz := optInt64(m, "payload_bytes"); biz != nil {
		return nil, biz
	} else if ok {
		pkt.PayloadBytes = v
	}
	return pkt, nil
}

func normalizeEventTime(m jsonObject, schema string, path string) (int64, *BizError) {
	switch schema {
	case SchemaV1:
		if raw, ok := m["at_seconds"]; ok {
			f, biz := toFloat64(raw, path+".at_seconds")
			if biz != nil {
				return 0, biz
			}
			return int64(math.Round(f * 1000)), nil
		}
		if raw, ok := m["at_ms"]; ok {
			return toInt64(raw, path+".at_ms")
		}
		return 0, NewBizError(CodeFieldRequired, path+".at_seconds", "event time is required")
	default:
		for _, key := range []string{"at_ms", "logical_time_ms", "timestamp_ms"} {
			if raw, ok := m[key]; ok {
				return toInt64(raw, path+"."+key)
			}
		}
		if raw, ok := m["at_seconds"]; ok {
			f, biz := toFloat64(raw, path+".at_seconds")
			if biz != nil {
				return 0, biz
			}
			return int64(math.Round(f * 1000)), nil
		}
		return 0, NewBizError(CodeFieldRequired, path+".at_ms", "event time is required")
	}
}

func parseEvents(raw any, schema string, path string) ([]Event, *BizError) {
	arr, biz := asArray(raw, path)
	if biz != nil {
		return nil, biz
	}
	events := []Event{}
	for i, item := range arr {
		ep := fmt.Sprintf("%s[%d]", path, i)
		m, biz := asObject(item, ep)
		if biz != nil {
			return nil, biz
		}
		id := optString(m, "event_id")
		if id == "" {
			id = optString(m, "id")
		}
		if id == "" {
			id = fmt.Sprintf("E%03d", i+1)
		}
		kind := optString(m, "kind")
		if kind == "" {
			kind = optString(m, "event_type")
		}
		if kind == "" {
			return nil, NewBizError(CodeFieldRequired, ep+".kind", "event kind is required")
		}
		at, biz := normalizeEventTime(m, schema, ep)
		if biz != nil {
			return nil, biz
		}
		ev := Event{
			ID:             id,
			Seq:            i + 1,
			Kind:           normalizeEventKind(kind),
			AtMs:           at,
			TunnelID:       firstString(m, "tunnel_id", "tid", "tunnel"),
			ConfigRevision: firstString(m, "config_revision", "revision"),
		}
		if packetRaw, ok := m["packet"]; ok {
			pkt, biz := parsePacket(packetRaw, ep+".packet")
			if biz != nil {
				return nil, biz
			}
			if pkt != nil {
				if pkt.AtMs == 0 {
					pkt.AtMs = at
				}
				ev.Packet = pkt
			}
		}
		events = append(events, ev)
	}
	return events, nil
}

func firstString(m jsonObject, keys ...string) string {
	for _, key := range keys {
		if s := optString(m, key); s != "" {
			return s
		}
	}
	return ""
}

func normalizeEventKind(kind string) string {
	switch strings.ToLower(kind) {
	case "up", "start", "connect", "link_up", "tunnel_connect", "create":
		return "connect"
	case "handshake", "handshake_complete", "established":
		return "handshake_complete"
	case "down", "disconnect", "link_down", "teardown":
		return "disconnect"
	case "keepalive_timeout", "ka_timeout", "timeout":
		return "keepalive_timeout"
	case "config", "config_apply", "configuration_delivered":
		return "config_apply"
	case "packet", "traffic", "inner_packet":
		return "packet"
	default:
		return kind
	}
}
