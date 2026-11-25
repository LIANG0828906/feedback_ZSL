package plancache

import (
	"fmt"
	"strings"
	"unicode"
)

func renderV2(p *Project) map[string]any { return renderCamel(p, false, SchemaV2) }

func renderV3(p *Project) map[string]any {
	doc := pascalizeValue(renderCamel(p, true, SchemaV3)).(map[string]any)
	return doc
}

func renderCamel(p *Project, includeFingerprint bool, schema string) map[string]any {
	stmts := make([]any, 0, len(p.Statements))
	for _, st := range p.Statements {
		byID := map[string]Node{}
		for _, sp := range st.Subplans {
			for _, n := range sp.Nodes {
				byID[n.ID] = n
			}
		}
		params := make([]any, 0, len(st.Params))
		for _, param := range st.Params {
			params = append(params, map[string]any{"name": param.Name, "type": param.Type})
		}
		trees := make([]any, 0, len(st.Subplans))
		for _, sp := range st.Subplans {
			var renderNode func(id string) any
			renderNode = func(id string) any {
				n := byID[id]
				children := make([]any, 0, len(n.Children))
				for _, child := range n.Children {
					children = append(children, renderNode(child))
				}
				args := make([]any, 0, len(n.Arguments))
				for _, arg := range n.Arguments {
					args = append(args, arg)
				}
				m := map[string]any{
					"id": n.ID, "op": n.Op, "label": n.Label,
					"relativeArgs": args, "children": children,
					"estimatedCostMs": n.EstimatedCostNS / 1_000_000,
				}
				if n.Ref != nil {
					m["ref"] = map[string]any{
						"statementId": n.Ref.StatementID,
						"subplanId":   n.Ref.SubplanID,
						"planId":      n.Ref.PlanID,
					}
				}
				return m
			}
			trees = append(trees, map[string]any{"subplanId": sp.ID, "root": renderNode(sp.RootID)})
		}
		sm := map[string]any{
			"id": st.ID, "template": st.Template, "params": params,
			"mainSubplanId": st.MainSubplanID, "subplanTrees": trees,
			"statsVersion": st.StatsVersion,
		}
		if includeFingerprint {
			sm["fingerprint"] = st.Fingerprint
		}
		stmts = append(stmts, sm)
	}
	reqs := make([]any, 0, len(p.Requests))
	for _, r := range p.Requests {
		reqs = append(reqs, map[string]any{
			"seq": r.Seq, "requestId": r.RequestID, "sessionId": r.SessionID,
			"statementId": r.StatementID, "settings": r.Settings,
		})
	}
	edits := make([]any, 0, len(p.InvalidationEdits))
	for _, e := range p.InvalidationEdits {
		edits = append(edits, map[string]any{
			"id": e.ID, "kind": e.Kind, "statementId": e.StatementID,
			"parameterIndex": e.ParameterIndex, "oldValue": e.OldValue,
			"newValue": e.NewValue, "source": e.Source,
		})
	}
	return map[string]any{
		"schemaId":           schema,
		"projectId":          p.ProjectID,
		"engineeringVersion": p.EngineeringVersion,
		"generatorSeed":      p.GeneratorSeed,
		"tickDurationNs":     p.TickDurationNS,
		"statements":         stmts,
		"keySettingNames":    p.KeySettingNames,
		"defaultSettings":    p.DefaultSettings,
		"compatibilityRules": renderCamelRules(p.CompatibilityRules),
		"cacheConfig": map[string]any{
			"maxPlansPerStatement": p.Capacity.MaxPlansPerStatement,
			"maxCacheKiB":          p.Capacity.MaxCacheBytes / 1024,
		},
		"evictionPolicy": map[string]any{
			"tickGranularityMs": p.Eviction.TickGranularityMS,
			"perStatementRule":  p.Eviction.PerStatementRule,
			"totalBytesRule":    p.Eviction.TotalBytesRule,
			"tieBreakRule":      p.Eviction.TieBreakRule,
		},
		"runtimeBudget":     map[string]any{"workers": p.Runtime.Workers, "sessionBuffer": p.Runtime.SessionBuffer},
		"propagationDepth":  p.PropagationDepth,
		"requests":          reqs,
		"invalidationEdits": edits,
	}
}

func renderCamelRules(rules map[string]SettingRule) map[string]any {
	out := map[string]any{}
	for k, r := range rules {
		out[k] = map[string]any{"mode": r.Mode, "allowedValues": r.AllowedValues, "minimum": r.Minimum}
	}
	return out
}

func pascalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			nk := pascalizeKey(k)
			switch k {
			case "settings", "defaultSettings":
				// Session setting names are stable data identifiers and must
				// not be PascalCased.
				out[nk] = val
			case "compatibilityRules":
				rules, _ := val.(map[string]any)
				pascalRules := map[string]any{}
				for ruleName, ruleBody := range rules {
					pascalRules[ruleName] = pascalizeValue(ruleBody)
				}
				out[nk] = pascalRules
			default:
				out[nk] = pascalizeValue(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = pascalizeValue(x[i])
		}
		return out
	default:
		return v
	}
}

func pascalizeKey(k string) string {
	if k == "" {
		return k
	}
	if strings.HasSuffix(k, "Id") {
		k = strings.TrimSuffix(k, "Id") + "ID"
	}
	r := []rune(k)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func normalizePascalValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[camelizePascalKey(k)] = normalizePascalValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normalizePascalValue(x[i])
		}
		return out
	default:
		return v
	}
}

func camelizePascalKey(k string) string {
	if strings.HasSuffix(k, "ID") && len(k) > 2 {
		k = k[:len(k)-2] + "Id"
	}
	r := []rune(k)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func loadV2(root map[string]any) (*Project, error) {
	p := &Project{SchemaVersion: SchemaV2}
	var err error
	p.ProjectID, err = requireString(root, "projectId", "$")
	if err != nil {
		return nil, err
	}
	p.EngineeringVersion, err = requireString(root, "engineeringVersion", "$")
	if err != nil {
		return nil, err
	}
	p.GeneratorSeed, _ = int64Value(root["generatorSeed"], "$.generatorSeed")
	if v, ok := root["tickDurationNs"]; ok {
		p.TickDurationNS, err = int64Value(v, "$.tickDurationNs")
		if err != nil {
			return nil, err
		}
	} else {
		p.TickDurationNS = 1_000_000
	}
	p.KeySettingNames = stringSlice(root["keySettingNames"])
	p.DefaultSettings = settingsMap(root["defaultSettings"])
	p.CompatibilityRules, err = loadSettingsRules(root["compatibilityRules"], "$.compatibilityRules")
	if err != nil {
		return nil, err
	}
	if cfg, ok := root["cacheConfig"].(map[string]any); ok {
		p.Capacity.MaxPlansPerStatement, err = intValue(cfg["maxPlansPerStatement"], "$.cacheConfig.maxPlansPerStatement")
		if err != nil {
			return nil, err
		}
		_, hasKiB := cfg["maxCacheKiB"]
		_, hasBytes := cfg["maxCacheBytes"]
		if hasKiB && hasBytes {
			return nil, newError(CodeSchemaFieldConflict, "$.cacheConfig", "maxCacheKiB and maxCacheBytes cannot both be declared")
		}
		if hasKiB {
			kib, kerr := int64Value(cfg["maxCacheKiB"], "$.cacheConfig.maxCacheKiB")
			if kerr != nil {
				return nil, kerr
			}
			p.Capacity.MaxCacheBytes = kib * 1024
		} else if hasBytes {
			p.Capacity.MaxCacheBytes, err = int64Value(cfg["maxCacheBytes"], "$.cacheConfig.maxCacheBytes")
			if err != nil {
				return nil, err
			}
		} else {
			return nil, newError(CodeSchemaMissingField, "$.cacheConfig", "cache capacity is required")
		}
	}
	if ep, ok := root["evictionPolicy"].(map[string]any); ok {
		p.Eviction.TickGranularityMS, _ = int64Value(ep["tickGranularityMs"], "$.evictionPolicy.tickGranularityMs")
		p.Eviction.PerStatementRule = optionalString(ep, "perStatementRule")
		p.Eviction.TotalBytesRule = optionalString(ep, "totalBytesRule")
		p.Eviction.TieBreakRule = optionalString(ep, "tieBreakRule")
	}
	if rb, ok := root["runtimeBudget"].(map[string]any); ok {
		p.Runtime.Workers, err = intValue(rb["workers"], "$.runtimeBudget.workers")
		if err != nil {
			return nil, err
		}
		p.Runtime.SessionBuffer, err = intValue(rb["sessionBuffer"], "$.runtimeBudget.sessionBuffer")
		if err != nil {
			return nil, err
		}
	}
	if v, ok := root["propagationDepth"]; ok {
		p.PropagationDepth, err = intValue(v, "$.propagationDepth")
		if err != nil {
			return nil, err
		}
	}
	stmts, err := asArray(root["statements"], "$.statements")
	if err != nil {
		return nil, err
	}
	for i, item := range stmts {
		sm, err := asMap(item, fmt.Sprintf("$.statements[%d]", i))
		if err != nil {
			return nil, err
		}
		st, err := loadCamelStatement(sm, fmt.Sprintf("$.statements[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Statements = append(p.Statements, st)
	}
	if err := loadCamelRequestsEdits(root, p); err != nil {
		return nil, err
	}
	CanonicalizeProject(p)
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(root map[string]any) (*Project, error) {
	normalized := normalizePascalValue(root).(map[string]any)
	if _, ok := normalized["schemaId"]; ok {
		normalized["schemaId"] = SchemaV3
	}
	p, err := loadV2(normalized)
	if err != nil {
		return nil, err
	}
	p.SchemaVersion = SchemaV3
	return p, nil
}

func loadCamelStatement(sm map[string]any, path string) (Statement, error) {
	st := Statement{}
	var err error
	if st.ID, err = requireString(sm, "id", path); err != nil {
		return st, err
	}
	if st.Template, err = requireString(sm, "template", path); err != nil {
		return st, err
	}
	st.StatsVersion = optionalString(sm, "statsVersion")
	st.MainSubplanID = optionalString(sm, "mainSubplanId")
	st.Fingerprint = optionalString(sm, "fingerprint")
	params, err := anySlice(sm["params"], path+".params", func(pm map[string]any, pp string) (ParamSpec, error) {
		typ, err := requireString(pm, "type", pp)
		if err != nil {
			return ParamSpec{}, err
		}
		return ParamSpec{Name: optionalString(pm, "name"), Type: typ}, nil
	})
	if err != nil {
		return st, err
	}
	for i := range params {
		params[i].Index = i
	}
	st.Params = params
	if _, flat := sm["flatNodes"]; flat {
		if _, trees := sm["subplanTrees"]; trees {
			return st, newError(CodeSchemaFieldConflict, path, "flatNodes and subplanTrees cannot both describe one statement")
		}
		return st, newError(CodeSchemaFieldConflict, path+".flatNodes", "V2/V3 loader requires grouped subplan trees")
	}
	trees, err := asArray(sm["subplanTrees"], path+".subplanTrees")
	if err != nil {
		return st, err
	}
	for i, item := range trees {
		tm, err := asMap(item, fmt.Sprintf("%s.subplanTrees[%d]", path, i))
		if err != nil {
			return st, err
		}
		subID, err := requireString(tm, "subplanId", fmt.Sprintf("%s.subplanTrees[%d]", path, i))
		if err != nil {
			return st, err
		}
		rootRaw, ok := tm["root"]
		if !ok {
			return st, newError(CodeNodeTreeMalformed, fmt.Sprintf("%s.subplanTrees[%d].root", path, i), "tree root is required")
		}
		nodes, rootID, err := flattenCamelTree(rootRaw, subID, fmt.Sprintf("%s.subplanTrees[%d].root", path, i))
		if err != nil {
			return st, err
		}
		st.Subplans = append(st.Subplans, Subplan{ID: subID, RootID: rootID, Nodes: nodes})
	}
	return st, nil
}

func flattenCamelTree(raw any, subID, path string) ([]Node, string, error) {
	order := make([]Node, 0)
	var walk func(any, string) (string, error)
	walk = func(raw any, p string) (string, error) {
		nm, err := asMap(raw, p)
		if err != nil {
			return "", err
		}
		n := Node{SubplanID: subID}
		var nerr error
		if n.ID, nerr = requireString(nm, "id", p); nerr != nil {
			return "", nerr
		}
		n.Op = optionalString(nm, "op")
		n.Label = optionalString(nm, "label")
		_, hasRel := nm["relativeArgs"]
		_, hasAbs := nm["absoluteArgs"]
		if hasRel && hasAbs {
			return "", newError(CodeSchemaFieldConflict, p, "relativeArgs and absoluteArgs cannot both be declared")
		}
		if hasRel {
			n.Arguments, err = intSlice(nm["relativeArgs"], p+".relativeArgs")
			if err != nil {
				return "", err
			}
		} else if hasAbs {
			abs, aerr := intSlice(nm["absoluteArgs"], p+".absoluteArgs")
			if aerr != nil {
				return "", aerr
			}
			// Camel schema has no global base; absoluteArgs is rejected as
			// ambiguous for this version.
			return "", withContext(newError(CodeSchemaFieldConflict, p+".absoluteArgs", "absolute argument numbering is not valid in relative-offset schema"), "values", abs)
		}
		_, hasMS := nm["estimatedCostMs"]
		_, hasTicks := nm["estimatedCostTicks"]
		if hasMS && hasTicks {
			return "", newError(CodeSchemaFieldConflict, p, "estimatedCostMs and estimatedCostTicks cannot both be declared")
		}
		if hasMS {
			ms, merr := int64Value(nm["estimatedCostMs"], p+".estimatedCostMs")
			if merr != nil {
				return "", merr
			}
			n.EstimatedCostNS = ms * 1_000_000
		} else if hasTicks {
			ticks, terr := int64Value(nm["estimatedCostTicks"], p+".estimatedCostTicks")
			if terr != nil {
				return "", terr
			}
			n.EstimatedCostNS = ticks * 1_000_000
		}
		if rm, ok := nm["ref"].(map[string]any); ok {
			n.Ref = &NodeRef{
				StatementID: optionalString(rm, "statementId"),
				SubplanID:   optionalString(rm, "subplanId"),
				PlanID:      optionalString(rm, "planId"),
			}
		}
		childrenRaw, _ := nm["children"].([]any)
		childIDs := make([]string, 0, len(childrenRaw))
		for i, child := range childrenRaw {
			id, err := walk(child, fmt.Sprintf("%s.children[%d]", p, i))
			if err != nil {
				return "", err
			}
			childIDs = append(childIDs, id)
		}
		n.Children = childIDs
		order = append(order, n)
		return n.ID, nil
	}
	rootID, err := walk(raw, path)
	if err != nil {
		return nil, "", err
	}
	return order, rootID, nil
}

func loadCamelRequestsEdits(root map[string]any, p *Project) error {
	reqs, err := anySlice(root["requests"], "$.requests", func(rm map[string]any, rp string) (SessionRequest, error) {
		r := SessionRequest{}
		var err error
		r.Seq, err = intValue(rm["seq"], rp+".seq")
		if err != nil {
			return r, err
		}
		r.RequestID, err = requireString(rm, "requestId", rp)
		if err != nil {
			return r, err
		}
		r.SessionID, err = requireString(rm, "sessionId", rp)
		if err != nil {
			return r, err
		}
		r.StatementID, err = requireString(rm, "statementId", rp)
		if err != nil {
			return r, err
		}
		r.Settings = settingsMap(rm["settings"])
		return r, nil
	})
	if err != nil {
		return err
	}
	p.Requests = reqs
	edits, err := anySlice(root["invalidationEdits"], "$.invalidationEdits", func(em map[string]any, ep string) (InvalidationEdit, error) {
		e := InvalidationEdit{}
		var err error
		if e.ID, err = requireString(em, "id", ep); err != nil {
			return e, err
		}
		if e.Kind, err = requireString(em, "kind", ep); err != nil {
			return e, err
		}
		e.StatementID = optionalString(em, "statementId")
		if v, ok := em["parameterIndex"]; ok {
			e.ParameterIndex, err = intValue(v, ep+".parameterIndex")
			if err != nil {
				return e, err
			}
		}
		e.OldValue = optionalString(em, "oldValue")
		e.NewValue = optionalString(em, "newValue")
		e.Source = optionalString(em, "source")
		if e.Source == "" {
			e.Source = e.Kind + ":" + e.StatementID
		}
		return e, nil
	})
	if err != nil {
		return err
	}
	p.InvalidationEdits = edits
	return nil
}
