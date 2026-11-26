package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"warmcache/internal/domain"
)

type decoded map[string]any

func Load(data []byte, version domain.SchemaVersion) (domain.Project, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root decoded
	if err := decoder.Decode(&root); err != nil {
		return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "decode_project", "invalid JSON", map[string]any{"error": err.Error()})
	}
	schema, _ := stringValue(root, "schema", "version")
	if schema != "" {
		parsed, err := domain.ParseSchemaVersion(schema)
		if err != nil {
			return domain.Project{}, err
		}
		if parsed != version {
			return domain.Project{}, domain.NewBizError(domain.CodeUnsupportedSchema, "decode_project", "schema does not match requested loader", map[string]any{
				"schema": schema, "requested": version,
			})
		}
	}
	var (
		project domain.Project
		err     error
	)
	switch version {
	case domain.SchemaV1:
		project, err = loadV1(root)
	case domain.SchemaV2:
		project, err = loadV2(root)
	case domain.SchemaV3:
		project, err = loadV3(root)
	default:
		return domain.Project{}, domain.NewBizError(domain.CodeUnsupportedSchema, "load_project", "unsupported schema version", map[string]any{"schema": version})
	}
	if err != nil {
		return domain.Project{}, err
	}
	project.Schema = version
	project.Normalize()
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func loadV1(root decoded) (domain.Project, error) {
	clock, err := object(root, "clock")
	if err != nil {
		return domain.Project{}, conflict("clock", err)
	}
	tickMS, err := intValue(clock, "tick_ms")
	if err != nil || tickMS <= 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v1", "invalid tick_ms", map[string]any{"field": "clock.tick_ms"})
	}
	globalLeaseTicks, _ := intValue(clock, "lease_ticks")
	globalPreheatTicks, _ := intValueOr(clock, 1, "preheat_ticks")
	watermark, hasWatermark := optionalInt(root, "preheat_watermark", "preheatWatermark", "watermark")
	globalLeaseTicks, err = resolveClockLease(clock, globalLeaseTicks, tickMS, hasWatermark)
	if err != nil {
		return domain.Project{}, err
	}
	budget, err := object(root, "bandwidth")
	if err != nil {
		return domain.Project{}, conflict("bandwidth", err)
	}
	basis := 1000
	if unit, _ := stringValue(budget, "unit", "scale"); unit != "" && unit != "permille" {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v1", "v1 bandwidth unit must be permille", map[string]any{"unit": unit})
	}
	budgetMilli, err := intValue(budget, "total", "budget", "limit")
	if err != nil {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v1", "missing bandwidth total", map[string]any{"field": "bandwidth.total"})
	}
	project := domain.Project{
		ID:          stringOr(root, "", "project_id", "project", "projectId"),
		Seed:        intOr(root, 0, "seed"),
		TickMS:      tickMS,
		Watermark:   watermark,
		BudgetMilli: budgetMilli,
		BudgetBasis: basis,
	}
	if project.Seed == 0 {
		project.Seed = 4242
	}
	nodesRaw, err := array(root, "nodes")
	if err != nil {
		return domain.Project{}, conflict("nodes", err)
	}
	for index, item := range nodesRaw {
		nodeRaw, ok := item.(map[string]any)
		if !ok {
			return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v1", "node must be an object", map[string]any{"path": fmt.Sprintf("nodes[%d]", index)})
		}
		leaseTicks, _ := intValue(nodeRaw, "lease_ticks")
		if leaseTicks == 0 {
			leaseTicks = globalLeaseTicks
		}
		if leaseTicks <= 0 {
			return domain.Project{}, domain.NewBizError(domain.CodeZeroLease, "load_v1", "lease duration must be positive", map[string]any{"path": fmt.Sprintf("nodes[%d].lease_ticks", index)})
		}
		node := domain.Node{
			ID:                 stringOr(nodeRaw, "", "node_id", "id", "node"),
			Group:              stringOr(nodeRaw, "", "replica_group", "group"),
			JoinTick:           intOr(nodeRaw, 1, "join_tick", "joinTick"),
			RetireTick:         intOr(nodeRaw, 0, "retire_tick", "retireTick"),
			ClaimIntervalTicks: intOr(nodeRaw, 1, "claim_interval_ticks", "claim_interval", "claimIntervalTicks"),
			LeaseTicks:         leaseTicks,
			MaxConcurrent:      int(intOr(nodeRaw, 1, "max_concurrent", "maxConcurrent")),
			BufferLimit:        int(intOr(nodeRaw, 1, "buffer_limit", "bufferLimit")),
		}
		project.Nodes = append(project.Nodes, node)
	}
	shardsRaw, err := array(root, "shards")
	if err != nil {
		return domain.Project{}, conflict("shards", err)
	}
	for index, item := range shardsRaw {
		shardRaw, ok := item.(map[string]any)
		if !ok {
			return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v1", "shard must be an object", map[string]any{"path": fmt.Sprintf("shards[%d]", index)})
		}
		shard, err := parseShard(shardRaw, "shards", index, tickMS, basis, globalPreheatTicks)
		if err != nil {
			return domain.Project{}, err
		}
		project.Shards = append(project.Shards, shard)
	}
	if err := validateInitialLease(root, project); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func loadV2(root decoded) (domain.Project, error) {
	clock, err := object(root, "clock")
	if err != nil {
		return domain.Project{}, conflict("clock", err)
	}
	tickMS, err := intValue(clock, "tickMs", "tick_ms", "tickMillis")
	if err != nil || tickMS <= 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v2", "invalid tickMs", map[string]any{"field": "clock.tickMs"})
	}
	watermark, hasWatermark := optionalInt(root, "preheat_watermark", "preheatWatermark", "watermark")
	leaseMS, err := intValue(clock, "leaseMs", "lease_ms")
	if err != nil {
		leaseMS = 0
	}
	if explicitTicks, ok := optionalInt(clock, "leaseTicks", "lease_ticks"); ok {
		expectedMS := explicitTicks * tickMS
		if leaseMS != 0 && expectedMS != leaseMS {
			if !hasWatermark {
				return domain.Project{}, semanticConflict("clock", "leaseMs and leaseTicks disagree")
			}
		} else {
			leaseMS = expectedMS
		}
	}
	if leaseMS <= 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeZeroLease, "load_v2", "lease duration must be positive", map[string]any{"field": "clock.leaseMs"})
	}
	globalPreheatMS, _ := intValueOr(clock, 0, "preheatMs", "preheat_ms")
	budgetRaw, err := object(root, "bandwidth")
	if err != nil {
		return domain.Project{}, conflict("bandwidth", err)
	}
	scale, _ := stringValue(budgetRaw, "scale", "unit")
	basis := 1000
	if scale != "" && scale != "basis_points" && scale != "bps" {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v2", "v2 bandwidth scale must be basis_points", map[string]any{"scale": scale})
	}
	budgetBPS, err := intValue(budgetRaw, "budget", "limit", "total")
	if err != nil {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v2", "missing bandwidth budget", map[string]any{"field": "bandwidth.budget"})
	}
	if budgetBPS%10 != 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v2", "basis-point budget is not representable in per-mille units", map[string]any{"budget_bps": budgetBPS})
	}
	project := domain.Project{
		ID:          stringOr(root, "", "projectId", "project_id", "project"),
		Seed:        intOr(root, 0, "seed"),
		TickMS:      tickMS,
		Watermark:   watermark,
		BudgetMilli: budgetBPS / 10,
		BudgetBasis: basis,
	}
	if project.Seed == 0 {
		project.Seed = 4242
	}
	nodesRaw, err := array(root, "nodes")
	if err != nil {
		return domain.Project{}, conflict("nodes", err)
	}
	for index, item := range nodesRaw {
		nodeRaw, ok := item.(map[string]any)
		if !ok {
			return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v2", "node must be an object", map[string]any{"path": fmt.Sprintf("nodes[%d]", index)})
		}
		leaseMSNode, _ := intValue(nodeRaw, "leaseMs", "lease_ms")
		if leaseMSNode == 0 {
			leaseMSNode = leaseMS
		}
		leaseTicks, err := durationTicks(leaseMSNode, tickMS, "nodes", index, "leaseMs")
		if err != nil {
			return domain.Project{}, err
		}
		if leaseTicks <= 0 {
			return domain.Project{}, domain.NewBizError(domain.CodeZeroLease, "load_v2", "lease duration must be positive", map[string]any{"path": fmt.Sprintf("nodes[%d].leaseMs", index)})
		}
		claimMS, _ := intValue(nodeRaw, "claimIntervalMs", "claim_interval_ms")
		if claimMS == 0 {
			claimMS = tickMS
		}
		claimTicks, err := durationTicks(claimMS, tickMS, "nodes", index, "claimIntervalMs")
		if err != nil {
			return domain.Project{}, err
		}
		project.Nodes = append(project.Nodes, domain.Node{
			ID:                 stringOr(nodeRaw, "", "id", "node", "node_id"),
			Group:              stringOr(nodeRaw, "", "group", "replicaGroup"),
			JoinTick:           intOr(nodeRaw, 1, "joinTick", "join_tick"),
			RetireTick:         intOr(nodeRaw, 0, "retireTick", "retire_tick"),
			ClaimIntervalTicks: claimTicks,
			LeaseTicks:         leaseTicks,
			MaxConcurrent:      int(intOr(nodeRaw, 1, "maxConcurrent", "max_concurrent")),
			BufferLimit:        int(intOr(nodeRaw, 1, "bufferLimit", "buffer_limit")),
		})
	}
	shardsRaw, err := array(root, "replicaTable", "shards")
	if err != nil {
		return domain.Project{}, conflict("replicaTable", err)
	}
	for index, item := range shardsRaw {
		shardRaw, ok := item.(map[string]any)
		if !ok {
			return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v2", "replica must be an object", map[string]any{"path": fmt.Sprintf("replicaTable[%d]", index)})
		}
		shard, err := parseShardWithPreheatMS(shardRaw, "replicaTable", index, tickMS, basis, globalPreheatMS)
		if err != nil {
			return domain.Project{}, err
		}
		project.Shards = append(project.Shards, shard)
	}
	if err := validateInitialLease(root, project); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func loadV3(root decoded) (domain.Project, error) {
	clock, err := object(root, "clock")
	if err != nil {
		return domain.Project{}, conflict("clock", err)
	}
	tickMS, err := intValue(clock, "tick_millis", "tickMillis", "tick_ms")
	if err != nil || tickMS <= 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v3", "invalid tick_millis", map[string]any{"field": "clock.tick_millis"})
	}
	watermark, hasWatermark := optionalInt(root, "preheat_watermark", "preheatWatermark", "watermark")
	leaseSeconds, err := floatValue(clock, "lease_seconds", "leaseSeconds")
	if err != nil {
		leaseSeconds = 0
	}
	if leaseMS, ok := optionalInt(clock, "lease_ms", "leaseMs"); ok && leaseMS > 0 {
		expectedSeconds := float64(leaseMS) / 1000
		if leaseSeconds != 0 && math.Abs(expectedSeconds-leaseSeconds) > 0.000001 && !hasWatermark {
			return domain.Project{}, semanticConflict("clock", "lease_seconds and lease_ms disagree")
		}
		if leaseSeconds == 0 {
			leaseSeconds = expectedSeconds
		}
	}
	if leaseSeconds <= 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeZeroLease, "load_v3", "lease duration must be positive", map[string]any{"field": "clock.lease_seconds"})
	}
	leaseTicksFloat := leaseSeconds * 1000 / float64(tickMS)
	leaseTicks := int64(math.Round(leaseTicksFloat))
	if leaseTicks <= 0 || math.Abs(leaseTicksFloat-float64(leaseTicks)) > 0.000001 {
		return domain.Project{}, domain.NewBizError(domain.CodeZeroLease, "load_v3", "lease duration must map exactly to whole ticks", map[string]any{"lease_seconds": leaseSeconds, "tick_ms": tickMS})
	}
	globalPreheatMS, _ := intValueOr(clock, 0, "preheat_ms", "preheatMs")
	if globalPreheatMS == 0 {
		if seconds, err := floatValue(clock, "preheat_seconds", "preheatSeconds"); err == nil && seconds > 0 {
			globalPreheatMS = int64(math.Round(seconds * 1000))
		}
	}
	budgetRaw, err := object(root, "bandwidth")
	if err != nil {
		return domain.Project{}, conflict("bandwidth", err)
	}
	scale, _ := stringValue(budgetRaw, "unit", "scale")
	basis := 1000
	if scale != "" && scale != "basis_points" && scale != "bps" {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v3", "v3 bandwidth unit must be basis_points", map[string]any{"scale": scale})
	}
	budgetBPS, err := intValue(budgetRaw, "limit", "budget", "total")
	if err != nil || budgetBPS%10 != 0 {
		return domain.Project{}, domain.NewBizError(domain.CodeBadBandwidthScale, "load_v3", "invalid basis-point budget", map[string]any{"budget_bps": budgetBPS})
	}
	project := domain.Project{
		ID:          stringOr(root, "", "project", "project_id", "projectId"),
		Seed:        intOr(root, 0, "seed"),
		TickMS:      tickMS,
		Watermark:   watermark,
		BudgetMilli: budgetBPS / 10,
		BudgetBasis: basis,
	}
	if project.Seed == 0 {
		project.Seed = 4242
	}
	groups, err := array(root, "replica_groups", "replicaGroups", "groups")
	if err != nil {
		return domain.Project{}, conflict("replica_groups", err)
	}
	for groupIndex, item := range groups {
		groupRaw, ok := item.(map[string]any)
		if !ok {
			return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v3", "replica group must be an object", map[string]any{"path": fmt.Sprintf("replica_groups[%d]", groupIndex)})
		}
		group := stringOr(groupRaw, "", "group", "group_id", "name")
		members, _ := array(groupRaw, "members", "nodes")
		for nodeIndex, member := range members {
			nodeRaw, ok := member.(map[string]any)
			if !ok {
				return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v3", "group member must be an object", map[string]any{"path": fmt.Sprintf("replica_groups[%d].members[%d]", groupIndex, nodeIndex)})
			}
			nodeLeaseTicks := leaseTicks
			if leaseMS, ok := optionalInt(nodeRaw, "lease_ms", "leaseMs"); ok && leaseMS > 0 {
				var err error
				nodeLeaseTicks, err = durationTicks(leaseMS, tickMS, "members", nodeIndex, "lease_ms")
				if err != nil {
					return domain.Project{}, err
				}
			}
			claimMS, _ := intValue(nodeRaw, "claim_interval_ms", "claimIntervalMs")
			if claimMS == 0 {
				claimMS = tickMS
			}
			claimTicks, err := durationTicks(claimMS, tickMS, "members", nodeIndex, "claim_interval_ms")
			if err != nil {
				return domain.Project{}, err
			}
			project.Nodes = append(project.Nodes, domain.Node{
				ID:                 stringOr(nodeRaw, "", "node_id", "node", "id"),
				Group:              group,
				JoinTick:           intOr(nodeRaw, 1, "join_tick", "joinTick"),
				RetireTick:         intOr(nodeRaw, 0, "retire_tick", "retireTick"),
				ClaimIntervalTicks: claimTicks,
				LeaseTicks:         nodeLeaseTicks,
				MaxConcurrent:      int(intOr(nodeRaw, 1, "max_concurrent", "maxConcurrent")),
				BufferLimit:        int(intOr(nodeRaw, 1, "buffer_limit", "bufferLimit")),
			})
		}
		replicas, _ := array(groupRaw, "replicas", "shards")
		for replicaIndex, replica := range replicas {
			shardRaw, ok := replica.(map[string]any)
			if !ok {
				return domain.Project{}, domain.NewBizError(domain.CodeBadRequest, "load_v3", "replica must be an object", map[string]any{"path": fmt.Sprintf("replica_groups[%d].replicas[%d]", groupIndex, replicaIndex)})
			}
			shard, err := parseShardWithPreheatMS(shardRaw, "replicas", replicaIndex, tickMS, basis, globalPreheatMS)
			if err != nil {
				return domain.Project{}, err
			}
			if shard.Group == "" {
				shard.Group = group
			}
			project.Shards = append(project.Shards, shard)
		}
	}
	if err := validateInitialLease(root, project); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func parseShard(raw map[string]any, path string, index int, tickMS int64, basis int, defaultPreheatTicks int64) (domain.Shard, error) {
	return parseShardInternal(raw, path, index, tickMS, basis, 0, defaultPreheatTicks, false)
}

func parseShardWithPreheatMS(raw map[string]any, path string, index int, tickMS int64, basis int, defaultPreheatMS int64) (domain.Shard, error) {
	defaultTicks := int64(0)
	if defaultPreheatMS > 0 {
		ticks, err := durationTicks(defaultPreheatMS, tickMS, path, index, "preheat_ms")
		if err != nil {
			return domain.Shard{}, err
		}
		defaultTicks = ticks
	}
	return parseShardInternal(raw, path, index, tickMS, basis, defaultPreheatMS, defaultTicks, true)
}

func parseShardInternal(raw map[string]any, path string, index int, tickMS int64, basis int, defaultPreheatMS int64, defaultPreheatTicks int64, hasMS bool) (domain.Shard, error) {
	shard := domain.Shard{
		ID:              stringOr(raw, "", "shard_id", "shard", "id"),
		NodeID:          stringOr(raw, "", "node_id", "node"),
		Group:           stringOr(raw, "", "replica_group", "group"),
		SourceVersion:   intOr(raw, 0, "source_version", "sourceVersion"),
		PreviousVersion: intOr(raw, 0, "previous_version", "previousVersion"),
	}
	digestValue, ok := firstValue(raw, "digest", "fingerprint", "content_digest", "contentFingerprint")
	if !ok {
		return domain.Shard{}, domain.NewBizError(domain.CodeBadRequest, "parse_shard", "missing digest", map[string]any{"path": fmt.Sprintf("%s[%d].digest", path, index)})
	}
	digest, err := parseDigest(digestValue)
	if err != nil {
		return domain.Shard{}, domain.NewBizError(domain.CodeBadRequest, "parse_shard", "invalid digest", map[string]any{"path": fmt.Sprintf("%s[%d].digest", path, index), "value": fmt.Sprint(digestValue)})
	}
	shard.Digest = digest
	bandwidthValue, ok := firstValue(raw, "bandwidth_permille", "bandwidthPermille", "bandwidth_bps", "bandwidthBps", "bandwidth")
	if !ok {
		return domain.Shard{}, domain.NewBizError(domain.CodeBadBandwidthScale, "parse_shard", "missing shard bandwidth", map[string]any{"path": fmt.Sprintf("%s[%d].bandwidth", path, index)})
	}
	bandwidth, err := numberInt64(bandwidthValue)
	if err != nil || bandwidth <= 0 {
		return domain.Shard{}, domain.NewBizError(domain.CodeBadBandwidthScale, "parse_shard", "invalid shard bandwidth", map[string]any{"path": fmt.Sprintf("%s[%d].bandwidth", path, index)})
	}
	if _, usesBPS := firstValue(raw, "bandwidth_bps", "bandwidthBps"); usesBPS {
		if bandwidth%10 != 0 {
			return domain.Shard{}, domain.NewBizError(domain.CodeBadBandwidthScale, "parse_shard", "basis-point shard cost is not representable in per-mille units", map[string]any{"path": fmt.Sprintf("%s[%d].bandwidth_bps", path, index)})
		}
		shard.BandwidthMilli = bandwidth / 10
	} else {
		shard.BandwidthMilli = bandwidth
	}
	if shard.SourceVersion == 0 {
		shard.SourceVersion = 4
	}
	if shard.PreviousVersion == 0 {
		shard.PreviousVersion = shard.SourceVersion - 1
	}
	if preheatMS, ok := optionalInt(raw, "preheat_ms", "preheatMs"); ok {
		ticks, err := durationTicks(preheatMS, tickMS, path, index, "preheat_ms")
		if err != nil {
			return domain.Shard{}, err
		}
		shard.PreheatTicks = ticks
	} else if seconds, err := floatValue(raw, "preheat_seconds", "preheatSeconds"); err == nil && seconds > 0 {
		ms := int64(math.Round(seconds * 1000))
		ticks, err := durationTicks(ms, tickMS, path, index, "preheat_seconds")
		if err != nil {
			return domain.Shard{}, err
		}
		shard.PreheatTicks = ticks
	} else if ticks, ok := optionalInt(raw, "preheat_ticks", "preheatTicks"); ok {
		shard.PreheatTicks = ticks
	} else if defaultPreheatTicks > 0 {
		shard.PreheatTicks = defaultPreheatTicks
	}
	if shard.PreheatTicks <= 0 {
		return domain.Shard{}, domain.NewBizError(domain.CodeBadRequest, "parse_shard", "preheat duration must be positive", map[string]any{"path": fmt.Sprintf("%s[%d].preheat", path, index), "default_ms": defaultPreheatMS})
	}
	return shard, nil
}

func validateInitialLease(root decoded, project domain.Project) error {
	holder, ok := stringValue(root, "initial_lease_holder", "lease_holder", "leaseHolder")
	if !ok || holder == "" {
		return nil
	}
	node, exists := project.Node(holder)
	if !exists {
		return domain.NewBizError(domain.CodeRetiredNode, "validate_lease", "initial lease points to an unknown node", map[string]any{"node": holder})
	}
	if node.RetiredAtOrBefore(node.JoinTick) {
		return domain.NewBizError(domain.CodeRetiredNode, "validate_lease", "initial lease points to a retired node", map[string]any{"node": holder, "retire_tick": node.RetireTick})
	}
	return nil
}

func resolveClockLease(clock decoded, ticks int64, tickMS int64, watermark bool) (int64, error) {
	leaseMS, hasMS := optionalInt(clock, "lease_ms", "leaseMs")
	if !hasMS {
		return ticks, nil
	}
	msTicks, err := durationTicks(leaseMS, tickMS, "clock", 0, "lease_ms")
	if err != nil {
		return 0, err
	}
	if ticks != 0 && ticks != msTicks {
		if !watermark {
			return 0, semanticConflict("clock.lease_ticks", "lease_ticks and lease_ms disagree")
		}
		return ticks, nil
	}
	return msTicks, nil
}

func durationTicks(milliseconds, tickMS int64, path string, index int, field string) (int64, error) {
	if milliseconds <= 0 {
		return 0, domain.NewBizError(domain.CodeZeroLease, "convert_duration", "duration must be positive", map[string]any{
			"path": fmt.Sprintf("%s[%d].%s", path, index, field), "milliseconds": milliseconds,
		})
	}
	if tickMS <= 0 || milliseconds%tickMS != 0 {
		return 0, domain.NewBizError(domain.CodeBadRequest, "convert_duration", "duration does not map to whole ticks", map[string]any{
			"path": fmt.Sprintf("%s[%d].%s", path, index, field), "milliseconds": milliseconds, "tick_ms": tickMS,
		})
	}
	return milliseconds / tickMS, nil
}

func semanticConflict(path, detail string) error {
	return domain.NewBizError(domain.CodeSchemaConflict, "load_project", detail, map[string]any{"path": path, "resolution": "preheat_watermark required"})
}

func conflict(path string, err error) error {
	return domain.NewBizError(domain.CodeSchemaConflict, "load_project", err.Error(), map[string]any{"path": path})
}

func parseDigest(value any) (uint64, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := strconv.ParseUint(typed.String(), 10, 64); err == nil {
			return integer, nil
		}
		return 0, fmt.Errorf("not an unsigned integer")
	case string:
		trimmed := strings.TrimSpace(typed)
		base := 10
		if strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X") {
			trimmed = trimmed[2:]
			base = 16
		}
		return strconv.ParseUint(trimmed, base, 64)
	case float64:
		if typed < 0 || math.Trunc(typed) != typed {
			return 0, fmt.Errorf("not an unsigned integer")
		}
		return uint64(typed), nil
	default:
		return 0, fmt.Errorf("unsupported digest type %T", value)
	}
}

func object(raw decoded, keys ...string) (decoded, error) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return nil, fmt.Errorf("missing object %s", strings.Join(keys, "/"))
	}
	switch typed := value.(type) {
	case map[string]any:
		return decoded(typed), nil
	case decoded:
		return typed, nil
	default:
		return nil, fmt.Errorf("%s is not an object", keys[0])
	}
}

func array(raw decoded, keys ...string) ([]any, error) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return nil, fmt.Errorf("missing array %s", strings.Join(keys, "/"))
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", keys[0])
	}
	return items, nil
}

func firstValue(raw map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := raw[key]; ok && value != nil {
			return value, true
		}
	}
	return nil, false
}

func stringValue(raw map[string]any, keys ...string) (string, bool) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return "", false
	}
	if typed, ok := value.(string); ok {
		return typed, true
	}
	return fmt.Sprint(value), true
}

func stringOr(raw map[string]any, fallback string, keys ...string) string {
	value, ok := stringValue(raw, keys...)
	if !ok {
		return fallback
	}
	return value
}

func intValue(raw map[string]any, keys ...string) (int64, error) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return 0, fmt.Errorf("missing %s", keys[0])
	}
	return numberInt64(value)
}

func intValueOr(raw map[string]any, fallback int64, keys ...string) (int64, error) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return fallback, nil
	}
	return numberInt64(value)
}

func intOr(raw map[string]any, fallback int64, keys ...string) int64 {
	value, err := intValueOr(raw, fallback, keys...)
	if err != nil {
		return fallback
	}
	return value
}

func optionalInt(raw map[string]any, keys ...string) (int64, bool) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return 0, false
	}
	integer, err := numberInt64(value)
	if err != nil {
		return 0, false
	}
	return integer, true
}

func floatValue(raw map[string]any, keys ...string) (float64, error) {
	value, ok := firstValue(raw, keys...)
	if !ok {
		return 0, fmt.Errorf("missing %s", keys[0])
	}
	switch typed := value.(type) {
	case json.Number:
		return typed.Float64()
	case float64:
		return typed, nil
	case int64:
		return float64(typed), nil
	case string:
		return strconv.ParseFloat(typed, 64)
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func numberInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.Int64()
	case float64:
		if math.Trunc(typed) != typed {
			return 0, fmt.Errorf("not an integer")
		}
		return int64(typed), nil
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	default:
		return 0, fmt.Errorf("not an integer")
	}
}
