package qos

import (
	"encoding/json"
	"fmt"
)

func exactPackets(bytes, nominal int64, path string) (int64, error) {
	if nominal <= 0 || bytes%nominal != 0 {
		return 0, NewError(EOverflow, path, "byte quantity is not an integer number of nominal packets", map[string]any{"bytes": bytes, "nominal_packet_bytes": nominal})
	}
	return bytes / nominal, nil
}

func ticks(ms, tick int64, path string) (int64, error) {
	if tick <= 0 || ms%tick != 0 {
		return 0, NewError(ETimeAlignment, path, "millisecond quantity is not aligned to a tick", map[string]any{"ms": ms, "tick_ms": tick})
	}
	return ms / tick, nil
}

func EncodeProject(p *Project, version int) ([]byte, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	var doc map[string]any
	var err error
	switch version {
	case 1:
		doc, err = encodeV1(p)
	case 2:
		doc, err = encodeV2(p)
	case 3:
		doc, err = encodeV3(p)
	default:
		return nil, NewError(ESCHEMA, "schema", "unsupported schema version", map[string]any{"version": version})
	}
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "encode", err, nil)
	}
	return append(raw, '\n'), nil
}

func encodeV1(p *Project) (map[string]any, error) {
	budgetPackets, err := exactPackets(p.EgressBudgetBytes, p.NominalPacketBytes, "project.egress_budget_packets")
	if err != nil {
		return nil, err
	}
	horizonTicks, err := ticks(p.HorizonMS, p.TickMS, "project.horizon_ticks")
	if err != nil {
		return nil, err
	}
	shaping := make([]any, 0, len(p.Channels))
	for i, ch := range p.Channels {
		if ch.PriorityBoost != 0 {
			return nil, NewError(EFieldConflict, fmt.Sprintf("project.channels[%d].priority_boost", i),
				"qos.v1 has no priority boost field and cannot represent a non-zero boost",
				map[string]any{"channel": ch.ID, "priority_boost": ch.PriorityBoost})
		}
		rate, err := RateValueForPermyriad(ch.RatePermyriad, RateScalePermille)
		if err != nil {
			return nil, err
		}
		weight, err := RateValueForPermyriad(ch.WeightPermyriad, RateScalePermille)
		if err != nil {
			return nil, err
		}
		burst, err := exactPackets(ch.CapacityBytes, p.NominalPacketBytes, fmt.Sprintf("project.shaping[%d].burst_packets", i))
		if err != nil {
			return nil, err
		}
		watermark, err := exactPackets(ch.InitialTokens, p.NominalPacketBytes, fmt.Sprintf("project.shaping[%d].initial_watermark_packets", i))
		if err != nil {
			return nil, err
		}
		qage, err := ticks(ch.QueueAgeMS, p.TickMS, fmt.Sprintf("project.shaping[%d].queue_age_ticks", i))
		if err != nil {
			return nil, err
		}
		entry := map[string]any{
			"queue":                     ch.ID,
			"order":                     ch.Order,
			"rate_permille":             rate,
			"burst_packets":             burst,
			"initial_watermark_packets": watermark,
			"policy":                    string(ch.Policy),
			"priority":                  ch.Priority,
			"weight_permille":           weight,
			"queue_age_ticks":           qage,
		}
		if ch.MaxDelayMS > 0 {
			md, err := ticks(ch.MaxDelayMS, p.TickMS, fmt.Sprintf("project.shaping[%d].max_delay_ticks", i))
			if err != nil {
				return nil, err
			}
			entry["max_delay_ticks"] = md
		}
		if len(ch.DSCPBoostClasses) > 0 {
			entry["dscp_boost_classes"] = append([]int(nil), ch.DSCPBoostClasses...)
		}
		shaping = append(shaping, entry)
	}
	arrivals := make([]any, 0, len(p.Packets))
	for _, pkt := range p.Packets {
		at, err := ticks(pkt.ArrivalMS, p.TickMS, "project.arrivals.arrival_tick")
		if err != nil {
			return nil, err
		}
		entry := map[string]any{
			"packet":       pkt.ID,
			"queue":        pkt.ChannelID,
			"arrival_tick": at,
			"bytes":        pkt.SizeBytes,
			"priority":     pkt.Priority,
			"seq":          pkt.Seq,
		}
		if pkt.DSCP != nil {
			entry["dscp"] = *pkt.DSCP
		}
		arrivals = append(arrivals, entry)
	}
	return map[string]any{
		"schema": "qos.v1",
		"project": map[string]any{
			"name":                  p.Name,
			"version":               p.Version,
			"tick_ms":               p.TickMS,
			"integer_width":         p.Width,
			"nominal_packet_bytes":  p.NominalPacketBytes,
			"egress_budget_packets": budgetPackets,
			"worker_count":          p.WorkerThreads,
			"queue_depth":           p.QueueDepth,
			"horizon_ticks":         horizonTicks,
			"shaping":               shaping,
			"arrivals":              arrivals,
		},
	}, nil
}

func encodeV2(p *Project) (map[string]any, error) {
	profiles := make([]any, 0, len(p.Channels))
	for _, ch := range p.Channels {
		qosObj := map[string]any{
			"rate":               map[string]any{"permyriad": ch.RatePermyriad},
			"burst":              map[string]any{"bytes": ch.CapacityBytes},
			"watermark":          map[string]any{"bytes": ch.InitialTokens},
			"queue_age_ms":       ch.QueueAgeMS,
			"policy":             string(ch.Policy),
			"priority":           ch.Priority,
			"weight":             map[string]any{"permyriad": ch.WeightPermyriad},
			"priority_boost":     ch.PriorityBoost,
			"dscp_boost_classes": append([]int(nil), ch.DSCPBoostClasses...),
		}
		if ch.MaxDelayMS > 0 {
			qosObj["max_delay_ms"] = ch.MaxDelayMS
		}
		profiles = append(profiles, map[string]any{
			"id":      ch.ID,
			"ordinal": ch.Order,
			"qos":     qosObj,
		})
	}
	arrivals := make([]any, 0, len(p.Packets))
	for _, pkt := range p.Packets {
		entry := map[string]any{
			"id":         pkt.ID,
			"channel_id": pkt.ChannelID,
			"arrival_ms": pkt.ArrivalMS,
			"bytes":      pkt.SizeBytes,
			"priority":   pkt.Priority,
			"seq":        pkt.Seq,
		}
		if pkt.DSCP != nil {
			entry["dscp_class"] = *pkt.DSCP
		}
		arrivals = append(arrivals, entry)
	}
	return map[string]any{
		"schema": "qos.v2",
		"project": map[string]any{
			"name":    p.Name,
			"version": p.Version,
			"runtime": map[string]any{
				"tick_ms":              p.TickMS,
				"integer_width":        p.Width,
				"nominal_packet_bytes": p.NominalPacketBytes,
				"egress_budget_bytes":  p.EgressBudgetBytes,
				"worker_threads":       p.WorkerThreads,
				"queue_depth":          p.QueueDepth,
				"horizon_ms":           p.HorizonMS,
			},
			"channels":       map[string]any{"profiles": profiles},
			"arrival_stream": arrivals,
		},
	}, nil
}

func encodeV3(p *Project) (map[string]any, error) {
	table := make([]any, 0, len(p.Channels))
	for _, ch := range p.Channels {
		burst := map[string]any{}
		if ch.CapacityBytes%p.NominalPacketBytes == 0 {
			burst["packets"] = ch.CapacityBytes / p.NominalPacketBytes
		} else {
			burst["bytes"] = ch.CapacityBytes
		}
		watermark := map[string]any{}
		if ch.InitialTokens%p.NominalPacketBytes == 0 {
			watermark["packets"] = ch.InitialTokens / p.NominalPacketBytes
		} else {
			watermark["bytes"] = ch.InitialTokens
		}
		weight, err := RateValueForPermyriad(ch.WeightPermyriad, RateScalePermille)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{
			"channel":      ch.ID,
			"slot":         ch.Order,
			"rate_value":   ch.RatePermyriad,
			"rate_scale":   RateScalePermyriad,
			"burst":        burst,
			"watermark":    watermark,
			"queue_age":    map[string]any{"remaining_ms": ch.QueueAgeMS},
			"policy":       string(ch.Policy),
			"priority":     ch.Priority,
			"weight_scale": RateScalePermille,
			"weight":       weight,
		}
		if ch.PriorityBoost != 0 {
			entry["priority_boost"] = ch.PriorityBoost
		}
		if ch.MaxDelayMS > 0 {
			entry["max_remaining_ms"] = ch.MaxDelayMS
		}
		if len(ch.DSCPBoostClasses) > 0 {
			entry["dscp_boost_classes"] = append([]int(nil), ch.DSCPBoostClasses...)
		}
		table = append(table, entry)
	}
	packets := make([]any, 0, len(p.Packets))
	for _, pkt := range p.Packets {
		entry := map[string]any{
			"id":         pkt.ID,
			"channel":    pkt.ChannelID,
			"at_ms":      pkt.ArrivalMS,
			"size_bytes": pkt.SizeBytes,
			"priority":   pkt.Priority,
			"seq":        pkt.Seq,
		}
		if pkt.DSCP != nil {
			entry["dscp_class"] = *pkt.DSCP
		}
		packets = append(packets, entry)
	}
	return map[string]any{
		"schema": "qos.v3",
		"project": map[string]any{
			"name":    p.Name,
			"version": p.Version,
			"tick_ms": p.TickMS,
			"defaults": map[string]any{
				"integer_width":        p.Width,
				"nominal_packet_bytes": p.NominalPacketBytes,
				"worker_count":         p.WorkerThreads,
				"queue_depth":          p.QueueDepth,
			},
			"egress_budget_bytes": p.EgressBudgetBytes,
			"horizon_ms":          p.HorizonMS,
			"shaping_table":       table,
			"packets":             packets,
		},
	}, nil
}
