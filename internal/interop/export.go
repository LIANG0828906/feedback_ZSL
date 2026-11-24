package interop

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"saveinterop/internal/checkpoint"
	"saveinterop/internal/core"
	"saveinterop/internal/generator"
)

type ExportVerification struct {
	Export ExportResult
	Loaded *LoadResult
}

type exportOp struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Role        string  `json:"role"`
	TargetPath  string  `json:"targetPath"`
	IntValue    int64   `json:"intValue,omitempty"`
	StringValue string  `json:"stringValue,omitempty"`
	BoolValue   bool    `json:"boolValue,omitempty"`
	Trace       *Trace  `json:"trace,omitempty"`
	Ruling      *Ruling `json:"ruling,omitempty"`
}

type exportPlan struct {
	branch     core.BranchSpec
	source     string
	itemsPath  string
	questsPath string
	ops        []exportOp
}

type exportState struct {
	root    map[string]any
	lists   map[string][]string
	traces  []Trace
	rulings []Ruling
}

type CheckpointExportResult struct {
	ExportResult
	Windows        int
	ResumedWindows int
	Recovery       *core.Error
}

func (m *Middleware) Export(model core.SemanticModel, options ExportOptions) (*ExportResult, error) {
	plan, err := m.prepareExport(model, options)
	if err != nil {
		return nil, err
	}
	state := newExportState()
	for _, op := range plan.ops {
		if err := applyExportOp(state, op); err != nil {
			return nil, err
		}
	}
	return m.assembleExport(plan, state)
}

func (m *Middleware) prepareExport(model core.SemanticModel, options ExportOptions) (*exportPlan, error) {
	branch, err := m.validateExportOptions(options)
	if err != nil {
		return nil, err
	}
	model = model.Clone()
	model.Normalize()
	plan := &exportPlan{branch: branch, source: options.SourceBranch}

	fieldByRole := make(map[string]string)
	for _, field := range branch.Fields {
		fieldByRole[field.Role] = field.Path
	}
	listByRole := make(map[string]string)
	for _, list := range branch.Lists {
		listByRole[list.Role] = list.Path
	}
	requiredFields := []string{"level", "coins", "experience", "coordinate.x", "coordinate.y"}
	for _, role := range requiredFields {
		if _, ok := fieldByRole[role]; !ok {
			return nil, semanticError("export", "fields."+role, branch.ID, role,
				"<undeclared>", "target branch is missing semantic field mapping")
		}
	}
	if _, ok := listByRole["items"]; !ok {
		return nil, semanticError("export", "lists.items", branch.ID, "items",
			"<undeclared>", "target branch is missing items mapping")
	}
	if _, ok := listByRole["quests"]; !ok {
		return nil, semanticError("export", "lists.quests", branch.ID, "quests",
			"<undeclared>", "target branch is missing quests mapping")
	}
	plan.itemsPath, plan.questsPath = listByRole["items"], listByRole["quests"]

	conditionalByRole := make(map[string]core.ConditionalMapping)
	for _, conditional := range branch.Conditionals {
		current, exists := conditionalByRole[conditional.Role]
		if !exists || conditional.Path < current.Path {
			conditionalByRole[conditional.Role] = conditional
		}
	}
	conditionalValues := make(map[string]int64)
	for _, conditional := range branch.Conditionals {
		conditionalValues[conditional.Path] = 0
	}

	baseCoins, baseExperience := model.Coins, model.Experience
	if options.PreserveConditionals && len(model.Conditionals) > 0 {
		bonusCoins, bonusExperience, err := contributionTotals(model.Conditionals)
		if err != nil {
			return nil, err
		}
		baseCoins, err = core.CheckedSub(model.Coins, bonusCoins)
		if err != nil {
			return nil, semanticError("export", "coins", branch.ID, "coins", model.Coins,
				"conditional decomposition exceeds coins total")
		}
		baseExperience, err = core.CheckedSub(model.Experience, bonusExperience)
		if err != nil {
			return nil, semanticError("export", "experience", branch.ID, "experience", model.Experience,
				"conditional decomposition exceeds experience total")
		}

		contributions := append([]core.ConditionalContribution(nil), model.Conditionals...)
		sort.Slice(contributions, func(i, j int) bool {
			if contributions[i].Role != contributions[j].Role {
				return contributions[i].Role < contributions[j].Role
			}
			if contributions[i].ConditionID != contributions[j].ConditionID {
				return contributions[i].ConditionID < contributions[j].ConditionID
			}
			return contributions[i].Physical < contributions[j].Physical
		})
		for _, contribution := range contributions {
			targetConditional, hasTarget := conditionalByRole[contribution.Role]
			conditionHolds := false
			if hasTarget {
				var conditionErr error
				conditionHolds, conditionErr = m.conditionHoldsForModelLenient(
					branch, model, targetConditional.ConditionID)
				if conditionErr != nil {
					return nil, conditionErr
				}
			}
			physicalTarget := "conditionals[" + contribution.Role + "]"
			if hasTarget {
				physicalTarget = targetConditional.Path
			}
			if hasTarget && conditionHolds {
				updated, err := core.CheckedAdd(conditionalValues[targetConditional.Path], contribution.Value)
				if err != nil {
					return nil, semanticError("export-conditional", targetConditional.Path, branch.ID,
						contribution.Role, contribution.Value,
						"target conditional contribution overflows")
				}
				conditionalValues[targetConditional.Path] = updated
				trace := m.trace("export", options.SourceBranch, branch.ID,
					"conditionals["+contribution.Role+"]", physicalTarget, contribution.Role, "applied")
				plan.ops = append(plan.ops, exportOp{
					Name: "conditional:" + contribution.Role, Kind: "note",
					Role: contribution.Role, TargetPath: physicalTarget, Trace: &trace,
				})
				continue
			}

			action, priority, err := chooseConflictAction(branch)
			if err != nil {
				return nil, err
			}
			reason := "target condition does not hold"
			if !hasTarget {
				reason = "target branch has no conditional slot for semantic role"
			}
			ruling := Ruling{
				Item: "conditionals[" + contribution.Role + "]", Action: action,
				Policy: action + fmt.Sprintf("@%d", priority), Priority: priority,
				SourceBranch: options.SourceBranch, TargetBranch: branch.ID,
				ConditionID: targetConditional.ConditionID, Original: contribution.Value,
				Reason: reason,
			}
			switch action {
			case "preserve":
				ruling.Result = contribution.Value
				if contribution.Role == "coins" {
					baseCoins, err = core.CheckedAdd(baseCoins, contribution.Value)
				} else {
					baseExperience, err = core.CheckedAdd(baseExperience, contribution.Value)
				}
				if err != nil {
					return nil, semanticError("export-conflict", physicalTarget, branch.ID,
						contribution.Role, contribution.Value,
						"preserved conditional value overflows base semantic total")
				}
			case "default":
				defaultValue := int64(0)
				if hasTarget {
					defaultValue = targetConditional.Default
				}
				ruling.Result = defaultValue
				if contribution.Role == "coins" {
					baseCoins, err = core.CheckedAdd(baseCoins, defaultValue)
				} else {
					baseExperience, err = core.CheckedAdd(baseExperience, defaultValue)
				}
				if err != nil {
					return nil, semanticError("export-conflict", physicalTarget, branch.ID,
						contribution.Role, defaultValue,
						"default conditional value overflows base semantic total")
				}
			case "reject":
				return nil, &core.Error{
					Code: core.CodeConflict, Op: "resolve-export-conflict",
					Message:   "target branch rejected conditional semantic value",
					FieldPath: physicalTarget, Conditional: targetConditional.ConditionID,
					Branch: branch.ID, SourceBranch: options.SourceBranch,
					TargetBranch: branch.ID, Semantic: contribution.Role,
					Measured: contribution.Value, Entry: "conditionals[" + contribution.Role + "]",
					Conflict: reason, Window: -1,
				}
			default:
				return nil, &core.Error{
					Code: core.CodeConflict, Op: "choose-conflict-action",
					Message: "unsupported conflict action", Branch: branch.ID,
					Measured: action, Window: -1,
				}
			}
			trace := m.trace("export", options.SourceBranch, branch.ID,
				"conditionals["+contribution.Role+"]", physicalTarget, contribution.Role,
				action+"-fold")
			plan.ops = append(plan.ops, exportOp{
				Name: "conditional:" + contribution.Role, Kind: "note",
				Role: contribution.Role, TargetPath: physicalTarget,
				Trace: &trace, Ruling: &ruling,
			})
		}
	}

	plan.ops = append(plan.ops,
		exportOp{Name: "level", Kind: "field", Role: "level",
			TargetPath: fieldByRole["level"], IntValue: model.Level,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"semantic.level", fieldByRole["level"], "level", "applied"))},
		exportOp{Name: "coins", Kind: "field", Role: "coins",
			TargetPath: fieldByRole["coins"], IntValue: baseCoins,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"semantic.coins", fieldByRole["coins"], "coins", "applied"))},
		exportOp{Name: "experience", Kind: "field", Role: "experience",
			TargetPath: fieldByRole["experience"], IntValue: baseExperience,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"semantic.experience", fieldByRole["experience"], "experience", "applied"))},
	)
	for i, item := range model.Items {
		physical := fmt.Sprintf("%s[%d]", plan.itemsPath, i)
		plan.ops = append(plan.ops, exportOp{
			Name: fmt.Sprintf("items[%04d]", i), Kind: "list-item", Role: "items",
			TargetPath: plan.itemsPath, StringValue: item,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				fmt.Sprintf("items[%04d]", i), physical, "items", "applied")),
		})
	}
	for i, quest := range model.Quests {
		physical := fmt.Sprintf("%s[%d]", plan.questsPath, i)
		plan.ops = append(plan.ops, exportOp{
			Name: fmt.Sprintf("quests[%04d]", i), Kind: "list-item", Role: "quests",
			TargetPath: plan.questsPath, StringValue: quest,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				fmt.Sprintf("quests[%04d]", i), physical, "quests", "applied")),
		})
	}
	plan.ops = append(plan.ops,
		exportOp{Name: "coordinate.x", Kind: "field", Role: "coordinate.x",
			TargetPath: fieldByRole["coordinate.x"], IntValue: model.Coordinate.X,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"semantic.coordinate.x", fieldByRole["coordinate.x"], "coordinate.x", "applied"))},
		exportOp{Name: "coordinate.y", Kind: "field", Role: "coordinate.y",
			TargetPath: fieldByRole["coordinate.y"], IntValue: model.Coordinate.Y,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"semantic.coordinate.y", fieldByRole["coordinate.y"], "coordinate.y", "applied"))},
	)

	ticks, err := core.UnixNanosToTicks(model.TimeNanos, branch.Time.EpochSeconds,
		branch.Time.TickNanos, branch.Time.Rounding)
	if err != nil {
		return nil, &core.Error{
			Code: core.CodeTime, Op: "export-time",
			Message:   "integer time conversion overflow: " + err.Error(),
			FieldPath: branch.Time.Path, Branch: branch.ID, Measured: model.TimeNanos,
			Semantic: "timeNanos", Window: -1,
		}
	}
	roundTrip, err := core.TicksToUnixNanos(ticks, branch.Time.EpochSeconds, branch.Time.TickNanos)
	if err != nil || roundTrip != model.TimeNanos {
		return nil, &core.Error{
			Code: core.CodeTime, Op: "export-time",
			Message: fmt.Sprintf("time point is not exactly representable using declared %s rounding",
				branch.Time.Rounding),
			FieldPath: branch.Time.Path, Branch: branch.ID, Measured: model.TimeNanos,
			Expected: fmt.Sprintf("%d", roundTrip), Actual: fmt.Sprintf("%d", model.TimeNanos),
			Semantic: "timeNanos", Window: -1,
		}
	}
	plan.ops = append(plan.ops, exportOp{
		Name: "timeNanos", Kind: "time", Role: "time",
		TargetPath: branch.Time.Path, IntValue: ticks,
		Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
			"semantic.timeNanos", branch.Time.Path, "time", "applied")),
	})

	flags := append([]core.FlagMapping(nil), branch.Flags...)
	sort.Slice(flags, func(i, j int) bool { return flags[i].Semantic < flags[j].Semantic })
	for _, flag := range flags {
		semanticValue, ok := model.Flags[flag.Semantic]
		if !ok {
			semanticValue = false
		}
		plan.ops = append(plan.ops, exportOp{
			Name: "flag:" + flag.Semantic, Kind: "flag", Role: "flag:" + flag.Semantic,
			TargetPath: flag.Path, BoolValue: semanticValue != flag.Invert,
			Trace: tracePtr(m.trace("export", options.SourceBranch, branch.ID,
				"flags."+flag.Semantic, flag.Path, "flag:"+flag.Semantic, "applied")),
		})
	}
	conditionals := append([]core.ConditionalMapping(nil), branch.Conditionals...)
	sort.Slice(conditionals, func(i, j int) bool { return conditionals[i].Path < conditionals[j].Path })
	for _, conditional := range conditionals {
		plan.ops = append(plan.ops, exportOp{
			Name: "conditional-value:" + conditional.Role, Kind: "field",
			Role: "conditional:" + conditional.Role, TargetPath: conditional.Path,
			IntValue: conditionalValues[conditional.Path],
		})
	}
	return plan, nil
}

func newExportState() *exportState {
	return &exportState{root: make(map[string]any), lists: make(map[string][]string)}
}

func applyExportOp(state *exportState, op exportOp) error {
	switch op.Kind {
	case "field", "time":
		core.Set(state.root, op.TargetPath, op.IntValue)
	case "list-item":
		state.lists[op.Role] = append(state.lists[op.Role], op.StringValue)
	case "flag":
		core.Set(state.root, op.TargetPath, op.BoolValue)
	case "note":
	default:
		return semanticError("apply-export-op", op.TargetPath, "", op.Role, op.Kind,
			"unknown export operation")
	}
	if op.Trace != nil {
		state.traces = append(state.traces, *op.Trace)
	}
	if op.Ruling != nil {
		state.rulings = append(state.rulings, *op.Ruling)
	}
	return nil
}

func (m *Middleware) assembleExport(plan *exportPlan, state *exportState) (*ExportResult, error) {
	core.Set(state.root, plan.itemsPath, append([]string(nil), state.lists["items"]...))
	core.Set(state.root, plan.questsPath, append([]string(nil), state.lists["quests"]...))
	archive := generator.Archive{
		FormatVersion: 1, Branch: plan.branch.ID, MappingVersion: m.table.Version, Save: state.root,
	}
	encoded, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	result := &ExportResult{
		Archive: encoded, ArchiveHash: core.HashBytes(encoded),
		TargetBranch: plan.branch.ID, RuleVersion: m.table.Version,
		Traces:  append([]Trace(nil), state.traces...),
		Rulings: append([]Ruling(nil), state.rulings...),
	}
	result.Normalize()
	return result, nil
}

func (m *Middleware) ExportAndVerify(model core.SemanticModel, options ExportOptions) (*ExportVerification, error) {
	exported, err := m.Export(model, options)
	if err != nil {
		return nil, err
	}
	loaded, err := m.LoadBytes(exported.Archive, LoadOptions{
		ExpectedBranch: options.TargetBranch,
		RuleVersion:    options.RuleVersion,
	})
	if err != nil {
		return nil, err
	}
	if err := core.CompareSemantics(model, loaded.Model, options.SourceBranch, options.TargetBranch); err != nil {
		return nil, err
	}
	return &ExportVerification{Export: *exported, Loaded: loaded}, nil
}

func (m *Middleware) ExportCheckpointed(ctx context.Context, model core.SemanticModel, options ExportOptions, checkpointOptions CheckpointOptions) (*CheckpointExportResult, error) {
	if checkpointOptions.WindowSize <= 0 {
		return nil, &core.Error{
			Code: core.CodeVersion, Op: "checkpoint-export",
			Message:  "checkpoint window size must be positive",
			Measured: checkpointOptions.WindowSize, Window: -1,
		}
	}
	plan, err := m.prepareExport(model, options)
	if err != nil {
		return nil, err
	}
	hashModel := model.Clone()
	hashModel.Normalize()
	modelBytes, err := json.Marshal(hashModel)
	if err != nil {
		return nil, err
	}
	inputHash := core.HashString(fmt.Sprintf("export|%s|%s|%s|%s|%t",
		core.HashBytes(modelBytes), options.SourceBranch, options.TargetBranch,
		m.table.Version, options.PreserveConditionals))
	inputHash = core.HashString(checkpoint.EngineVersion + "|" + inputHash)
	totalWindows := (len(plan.ops) + checkpointOptions.WindowSize - 1) / checkpointOptions.WindowSize
	meta := checkpoint.RunMetadata{
		RunID: core.HashString(strings.Join([]string{
			checkpoint.EngineVersion, "export", inputHash, options.SourceBranch, options.TargetBranch,
			m.table.Version, strconv.Itoa(checkpointOptions.WindowSize),
		}, "|")),
		Mode: "export", SourceBranch: options.SourceBranch, TargetBranch: options.TargetBranch,
		MappingVersion: m.table.Version, WindowSize: checkpointOptions.WindowSize,
		InputHash: inputHash,
	}
	store, err := checkpoint.Open(checkpointOptions.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	records, err := store.ValidateOrCreate(ctx, meta)
	var recovery *core.Error
	if err != nil && core.CodeOf(err) == core.CodeVersion && checkpointOptions.RecomputeOnConflict {
		recovery = asCoreError(err)
		records, err = store.ValidateOrCreate(ctx, meta)
	}
	if err != nil {
		return nil, err
	}
	if len(records) > totalWindows {
		if err := store.Invalidate(ctx, meta.RunID); err != nil {
			return nil, err
		}
		return nil, &core.Error{
			Code: core.CodeVersion, Op: "checkpoint-export",
			Message:  "checkpoint has more windows than the export plan",
			Expected: fmt.Sprintf("%d", totalWindows), Actual: fmt.Sprintf("%d", len(records)),
			Window: totalWindows,
		}
	}

	state := newExportState()
	for i, record := range records {
		var ops []exportOp
		if err := json.Unmarshal(record.Payload, &ops); err != nil {
			if invalidateErr := store.Invalidate(ctx, meta.RunID); invalidateErr != nil {
				return nil, invalidateErr
			}
			return nil, &core.Error{
				Code: core.CodeVersion, Op: "checkpoint-export",
				Message: "checkpoint payload is invalid: " + err.Error(), Window: i,
			}
		}
		for _, op := range ops {
			if err := applyExportOp(state, op); err != nil {
				return nil, withWindow(err, i)
			}
		}
	}
	previousCumulative := ""
	if len(records) > 0 {
		previousCumulative = records[len(records)-1].CumulativeFingerprint
	}
	for windowIndex := len(records); windowIndex < totalWindows; windowIndex++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m.windowHook != nil {
			if err := m.windowHook(windowIndex); err != nil {
				return nil, withWindow(err, windowIndex)
			}
		}
		start := windowIndex * checkpointOptions.WindowSize
		end := start + checkpointOptions.WindowSize
		if end > len(plan.ops) {
			end = len(plan.ops)
		}
		for _, op := range plan.ops[start:end] {
			if err := applyExportOp(state, op); err != nil {
				return nil, withWindow(err, windowIndex)
			}
		}
		payload, err := json.Marshal(plan.ops[start:end])
		if err != nil {
			return nil, err
		}
		delta := hashExportOps(plan.ops[start:end])
		cumulative := checkpoint.NextCumulative(previousCumulative, delta)
		record := checkpoint.WindowRecord{
			Index: windowIndex, ItemStart: start, ItemEnd: end,
			FingerprintDelta: delta, CumulativeFingerprint: cumulative, Payload: payload,
		}
		if err := store.AppendWindow(ctx, meta, record); err != nil {
			return nil, withWindow(err, windowIndex)
		}
		previousCumulative = cumulative
	}
	if err := store.Complete(ctx, meta, totalWindows); err != nil {
		if core.CodeOf(err) == core.CodeVersion {
			_ = store.Invalidate(ctx, meta.RunID)
		}
		return nil, err
	}
	exported, err := m.assembleExport(plan, state)
	if err != nil {
		return nil, err
	}
	return &CheckpointExportResult{
		ExportResult: *exported, Windows: totalWindows,
		ResumedWindows: len(records), Recovery: recovery,
	}, nil
}

func hashExportOps(ops []exportOp) string {
	payload, _ := json.Marshal(ops)
	return core.HashBytes(payload)
}

func tracePtr(trace Trace) *Trace {
	return &trace
}

func (m *Middleware) validateExportOptions(options ExportOptions) (core.BranchSpec, error) {
	if options.SourceBranch == "" {
		return core.BranchSpec{}, &core.Error{
			Code: core.CodeMapping, Op: "export", Message: "source branch is required",
			Entry: "sourceBranch", TargetBranch: options.TargetBranch, Window: -1,
		}
	}
	if _, ok := m.table.Branch(options.SourceBranch); !ok {
		return core.BranchSpec{}, &core.Error{
			Code: core.CodeMapping, Op: "export", Message: "source branch is not declared",
			Entry: "sourceBranch", SourceBranch: options.SourceBranch,
			TargetBranch: options.TargetBranch, Measured: options.SourceBranch, Window: -1,
		}
	}
	branch, ok := m.table.Branch(options.TargetBranch)
	if !ok {
		return core.BranchSpec{}, &core.Error{
			Code: core.CodeMapping, Op: "export", Message: "target branch is not declared",
			Entry: "targetBranch", SourceBranch: options.SourceBranch,
			TargetBranch: options.TargetBranch, Measured: options.TargetBranch, Window: -1,
		}
	}
	requested := options.RuleVersion
	if requested == "" {
		requested = m.table.Version
	}
	if requested != m.table.Version {
		return core.BranchSpec{}, &core.Error{
			Code: core.CodeMapping, Op: "export", Message: "mapping rule version mismatch",
			Entry: "mapping.coins", SourceBranch: options.SourceBranch,
			TargetBranch: options.TargetBranch, Expected: m.table.Version,
			Actual: requested, Window: -1,
		}
	}
	return branch, nil
}

func contributionTotals(contributions []core.ConditionalContribution) (int64, int64, error) {
	var coins, experience int64
	for _, contribution := range contributions {
		current := experience
		if contribution.Role == "coins" {
			current = coins
		}
		updated, err := core.CheckedAdd(current, contribution.Value)
		if err != nil {
			return 0, 0, &core.Error{
				Code: core.CodeSemantic, Op: "sum-conditionals",
				Message:  "conditional contribution overflow",
				Semantic: contribution.Role, Measured: contribution.Value, Window: -1,
			}
		}
		if contribution.Role == "coins" {
			coins = updated
		} else {
			experience = updated
		}
	}
	return coins, experience, nil
}

func chooseConflictAction(branch core.BranchSpec) (string, int, error) {
	rules := branch.SortedConflictRules()
	if len(rules) == 0 {
		return "", 0, &core.Error{
			Code: core.CodeConflict, Op: "choose-conflict-action",
			Message: "target branch declares no conflict policy",
			Branch:  branch.ID, Window: -1,
		}
	}
	return rules[0].Action, rules[0].Priority, nil
}
