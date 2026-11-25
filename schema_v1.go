package plancache

import "fmt"

func renderV1(p *Project) (map[string]any, error) {
	bases := parameterBases(p.Statements)
	stmts := make([]any, 0, len(p.Statements))
	for i, st := range p.Statements {
		params := make([]any, 0, len(st.Params))
		for _, param := range st.Params {
			params = append(params, map[string]any{"parameter_name": param.Name, "type_name": param.Type})
		}
		roots := make([]any, 0, len(st.Subplans))
		nodes := make([]any, 0)
		for _, sp := range st.Subplans {
			roots = append(roots, map[string]any{"subplan_id": sp.ID, "root_node_id": sp.RootID})
			for _, n := range sp.Nodes {
				args := make([]any, 0, len(n.Arguments))
				for _, a := range n.Arguments {
					args = append(args, a+bases[i])
				}
				node := map[string]any{
					"node_id":              n.ID,
					"subplan_id":           n.SubplanID,
					"operation_name":       n.Op,
					"node_label":           n.Label,
					"absolute_arguments":   args,
					"child_node_ids":       n.Children,
					"estimated_cost_ticks": n.EstimatedCostNS / p.TickDurationNS,
				}
				if n.Ref != nil {
					node["reference"] = map[string]any{
						"target_statement_id": n.Ref.StatementID,
						"target_subplan_id":   n.Ref.SubplanID,
						"target_plan_id":      n.Ref.PlanID,
					}
				}
				nodes = append(nodes, node)
			}
		}
		globalTemplate, tErr := renumberTemplate(st.Template, bases[i], len(st.Params), "$.statements.template_text")
		if tErr != nil {
			return nil, tErr
		}
		stmts = append(stmts, map[string]any{
			"statement_id":       st.ID,
			"template_text":      globalTemplate,
			"parameters":         params,
			"parameter_base":     bases[i],
			"main_subplan_id":    st.MainSubplanID,
			"subplan_roots":      roots,
			"flat_nodes":         nodes,
			"statistics_version": st.StatsVersion,
			"plan_fingerprint":   st.Fingerprint,
		})
	}
	reqs := make([]any, 0, len(p.Requests))
	for _, r := range p.Requests {
		reqs = append(reqs, map[string]any{
			"sequence_no": r.Seq, "request_id": r.RequestID, "session_id": r.SessionID,
			"statement_id": r.StatementID, "session_settings": r.Settings,
		})
	}
	edits := make([]any, 0, len(p.InvalidationEdits))
	for _, e := range p.InvalidationEdits {
		edits = append(edits, map[string]any{
			"edit_id": e.ID, "edit_kind": e.Kind, "statement_id": e.StatementID,
			"parameter_index": e.ParameterIndex, "old_value": e.OldValue,
			"new_value": e.NewValue, "trigger_source": e.Source,
		})
	}
	return map[string]any{
		"schema_id":                SchemaV1,
		"format_name":              "flat-absolute-tick-byte",
		"project_id":               p.ProjectID,
		"engineering_version":      p.EngineeringVersion,
		"generator_seed":           p.GeneratorSeed,
		"tick_duration_ns":         p.TickDurationNS,
		"statements":               stmts,
		"key_setting_names":        p.KeySettingNames,
		"default_session_settings": p.DefaultSettings,
		"setting_compatibility":    renderSettingsRules(p.CompatibilityRules, false),
		"cache_config": map[string]any{
			"per_statement_limit": p.Capacity.MaxPlansPerStatement,
			"max_bytes":           p.Capacity.MaxCacheBytes,
		},
		"eviction_policy": map[string]any{
			"eviction_granularity_ticks": p.Eviction.TickGranularityMS * 1_000_000 / p.TickDurationNS,
			"per_statement_rule":         p.Eviction.PerStatementRule,
			"total_bytes_rule":           p.Eviction.TotalBytesRule,
			"tie_break_rule":             p.Eviction.TieBreakRule,
		},
		"runtime_budget":     map[string]any{"worker_count": p.Runtime.Workers, "session_buffer_size": p.Runtime.SessionBuffer},
		"propagation_depth":  p.PropagationDepth,
		"session_requests":   reqs,
		"invalidation_edits": edits,
	}, nil
}

func parameterBases(stmts []Statement) []int {
	bases := make([]int, len(stmts))
	total := 0
	for i, st := range stmts {
		bases[i] = total
		total += len(st.Params)
	}
	return bases
}

func loadV1(root map[string]any) (*Project, error) {
	p := &Project{SchemaVersion: SchemaV1}
	var err error
	if p.ProjectID, err = requireString(root, "project_id", "$"); err != nil {
		return nil, err
	}
	if p.EngineeringVersion, err = requireString(root, "engineering_version", "$"); err != nil {
		return nil, err
	}
	p.GeneratorSeed, _ = int64Value(root["generator_seed"], "$.generator_seed")
	p.TickDurationNS, err = int64Value(root["tick_duration_ns"], "$.tick_duration_ns")
	if err != nil {
		return nil, err
	}
	p.KeySettingNames = stringSlice(root["key_setting_names"])
	p.DefaultSettings = settingsMap(root["default_session_settings"])
	p.CompatibilityRules, err = loadSettingsRules(root["setting_compatibility"], "$.setting_compatibility")
	if err != nil {
		return nil, err
	}
	if cfg, ok := root["cache_config"].(map[string]any); ok {
		p.Capacity.MaxPlansPerStatement, err = intValue(cfg["per_statement_limit"], "$.cache_config.per_statement_limit")
		if err != nil {
			return nil, err
		}
		_, hasBytes := cfg["max_bytes"]
		_, hasKiB := cfg["max_kib"]
		if hasBytes && hasKiB {
			return nil, newError(CodeSchemaFieldConflict, "$.cache_config", "max_bytes and max_kib cannot both be declared")
		}
		if hasBytes {
			p.Capacity.MaxCacheBytes, err = int64Value(cfg["max_bytes"], "$.cache_config.max_bytes")
		} else {
			kib, kerr := int64Value(cfg["max_kib"], "$.cache_config.max_kib")
			if kerr != nil {
				return nil, kerr
			}
			p.Capacity.MaxCacheBytes = kib * 1024
		}
		if err != nil {
			return nil, err
		}
	}
	if ep, ok := root["eviction_policy"].(map[string]any); ok {
		p.Eviction.PerStatementRule = optionalString(ep, "per_statement_rule")
		p.Eviction.TotalBytesRule = optionalString(ep, "total_bytes_rule")
		p.Eviction.TieBreakRule = optionalString(ep, "tie_break_rule")
		ticks, terr := int64Value(ep["eviction_granularity_ticks"], "$.eviction_policy.eviction_granularity_ticks")
		if terr != nil {
			return nil, terr
		}
		p.Eviction.TickGranularityMS = ticks * p.TickDurationNS / 1_000_000
	}
	if rb, ok := root["runtime_budget"].(map[string]any); ok {
		p.Runtime.Workers, err = intValue(rb["worker_count"], "$.runtime_budget.worker_count")
		if err != nil {
			return nil, err
		}
		p.Runtime.SessionBuffer, err = intValue(rb["session_buffer_size"], "$.runtime_budget.session_buffer_size")
		if err != nil {
			return nil, err
		}
	}
	if d, ok := root["propagation_depth"]; ok {
		p.PropagationDepth, err = intValue(d, "$.propagation_depth")
		if err != nil {
			return nil, err
		}
	}

	stmtArr, err := asArray(root["statements"], "$.statements")
	if err != nil {
		return nil, err
	}
	for i, item := range stmtArr {
		sm, err := asMap(item, fmt.Sprintf("$.statements[%d]", i))
		if err != nil {
			return nil, err
		}
		st, err := loadV1Statement(sm, p.TickDurationNS, fmt.Sprintf("$.statements[%d]", i))
		if err != nil {
			return nil, err
		}
		p.Statements = append(p.Statements, st)
	}
	if err := loadRequestsEditsV1(root, p); err != nil {
		return nil, err
	}
	CanonicalizeProject(p)
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV1Statement(sm map[string]any, tickDurationNS int64, path string) (Statement, error) {
	st := Statement{}
	var err error
	if st.ID, err = requireString(sm, "statement_id", path); err != nil {
		return st, err
	}
	if st.Template, err = requireString(sm, "template_text", path); err != nil {
		return st, err
	}
	st.StatsVersion = optionalString(sm, "statistics_version")
	st.MainSubplanID = optionalString(sm, "main_subplan_id")
	st.Fingerprint = optionalString(sm, "plan_fingerprint")
	base := 0
	if b, ok := sm["parameter_base"]; ok {
		base, err = intValue(b, path+".parameter_base")
		if err != nil {
			return st, err
		}
	}
	params, err := anySlice(sm["parameters"], path+".parameters", func(pm map[string]any, pp string) (ParamSpec, error) {
		name := optionalString(pm, "parameter_name")
		if name == "" {
			name = optionalString(pm, "name")
		}
		typ, perr := requireString(pm, "type_name", pp)
		if perr != nil {
			typ, perr = requireString(pm, "type", pp)
			if perr != nil {
				return ParamSpec{}, perr
			}
		}
		return ParamSpec{Name: name, Type: typ}, nil
	})
	if err != nil {
		return st, err
	}
	for i := range params {
		params[i].Index = i
	}
	st.Params = params
	st.Template, err = normalizeGlobalTemplate(st.Template, base, len(st.Params), path+".template_text")
	if err != nil {
		return st, err
	}

	_, hasFlat := sm["flat_nodes"]
	_, hasTrees := sm["subplan_trees"]
	if hasFlat && hasTrees {
		return st, newError(CodeSchemaFieldConflict, path, "flat_nodes and subplan_trees cannot both describe one statement")
	}
	rootPairs, err := anySlice(sm["subplan_roots"], path+".subplan_roots", func(rm map[string]any, rp string) (Subplan, error) {
		id, ierr := requireString(rm, "subplan_id", rp)
		if ierr != nil {
			return Subplan{}, ierr
		}
		root, rerr := requireString(rm, "root_node_id", rp)
		if rerr != nil {
			return Subplan{}, rerr
		}
		return Subplan{ID: id, RootID: root}, nil
	})
	if err != nil {
		return st, err
	}

	nodes, err := anySlice(sm["flat_nodes"], path+".flat_nodes", func(nm map[string]any, np string) (Node, error) {
		n := Node{}
		var nerr error
		if n.ID, nerr = requireString(nm, "node_id", np); nerr != nil {
			return n, nerr
		}
		n.SubplanID = optionalString(nm, "subplan_id")
		n.Op = optionalString(nm, "operation_name")
		if n.Op == "" {
			n.Op = optionalString(nm, "op")
		}
		n.Label = optionalString(nm, "node_label")
		if _, both := nm["absolute_arguments"]; both {
			if _, rel := nm["relative_arguments"]; rel {
				return n, newError(CodeSchemaFieldConflict, np, "absolute_arguments and relative_arguments cannot both be declared")
			}
		}
		if av, ok := nm["absolute_arguments"]; ok {
			absArgs, aerr := intSlice(av, np+".absolute_arguments")
			if aerr != nil {
				return n, aerr
			}
			n.Arguments = make([]int, len(absArgs))
			for j, a := range absArgs {
				n.Arguments[j] = a - base
			}
		} else if rv, ok := nm["relative_arguments"]; ok {
			n.Arguments, err = intSlice(rv, np+".relative_arguments")
			if err != nil {
				return n, err
			}
		}
		n.Children = stringSlice(nm["child_node_ids"])
		if n.Children == nil {
			n.Children = stringSlice(nm["children"])
		}
		_, hasTicks := nm["estimated_cost_ticks"]
		_, hasMS := nm["estimated_cost_ms"]
		if hasTicks && hasMS {
			return n, newError(CodeSchemaFieldConflict, np, "estimated_cost_ticks and estimated_cost_ms cannot both be declared")
		}
		if hasTicks {
			ticks, terr := int64Value(nm["estimated_cost_ticks"], np+".estimated_cost_ticks")
			if terr != nil {
				return n, terr
			}
			if tickDurationNS <= 0 {
				return n, newError(CodeSchemaFieldConflict, path+".tick_duration_ns", "tick duration must be positive for tick-valued costs")
			}
			n.EstimatedCostNS = ticks * tickDurationNS
		} else if hasMS {
			ms, merr := int64Value(nm["estimated_cost_ms"], np+".estimated_cost_ms")
			if merr != nil {
				return n, merr
			}
			n.EstimatedCostNS = ms * 1_000_000
		}
		if rm, ok := nm["reference"].(map[string]any); ok {
			n.Ref = &NodeRef{
				StatementID: optionalString(rm, "target_statement_id"),
				SubplanID:   optionalString(rm, "target_subplan_id"),
				PlanID:      optionalString(rm, "target_plan_id"),
			}
		}
		return n, nil
	})
	if err != nil {
		return st, err
	}
	// Attach nodes to root group order.
	bySubplan := map[string][]Node{}
	for _, n := range nodes {
		bySubplan[n.SubplanID] = append(bySubplan[n.SubplanID], n)
	}
	for _, root := range rootPairs {
		root.Nodes = bySubplan[root.ID]
		st.Subplans = append(st.Subplans, root)
	}
	return st, nil
}

func loadRequestsEditsV1(root map[string]any, p *Project) error {
	reqs, err := anySlice(root["session_requests"], "$.session_requests", func(rm map[string]any, rp string) (SessionRequest, error) {
		r := SessionRequest{}
		var err error
		r.Seq, err = intValue(rm["sequence_no"], rp+".sequence_no")
		if err != nil {
			return r, err
		}
		r.RequestID, err = requireString(rm, "request_id", rp)
		if err != nil {
			return r, err
		}
		r.SessionID, err = requireString(rm, "session_id", rp)
		if err != nil {
			return r, err
		}
		r.StatementID, err = requireString(rm, "statement_id", rp)
		if err != nil {
			return r, err
		}
		r.Settings = settingsMap(rm["session_settings"])
		return r, nil
	})
	if err != nil {
		return err
	}
	p.Requests = reqs
	edits, err := anySlice(root["invalidation_edits"], "$.invalidation_edits", func(em map[string]any, ep string) (InvalidationEdit, error) {
		e := InvalidationEdit{}
		var err error
		if e.ID, err = requireString(em, "edit_id", ep); err != nil {
			return e, err
		}
		if e.Kind, err = requireString(em, "edit_kind", ep); err != nil {
			return e, err
		}
		e.StatementID = optionalString(em, "statement_id")
		if v, ok := em["parameter_index"]; ok {
			e.ParameterIndex, err = intValue(v, ep+".parameter_index")
			if err != nil {
				return e, err
			}
		}
		e.OldValue = optionalString(em, "old_value")
		e.NewValue = optionalString(em, "new_value")
		e.Source = optionalString(em, "trigger_source")
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
