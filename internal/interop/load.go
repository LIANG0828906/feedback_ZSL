package interop

import (
	"fmt"
	"sort"

	"saveinterop/internal/core"
)

type Middleware struct {
	table      core.MappingTable
	windowHook func(int) error
}

func New(table core.MappingTable) (*Middleware, error) {
	if table.Version == "" {
		return nil, fmt.Errorf("mapping table version is required")
	}
	for _, branch := range table.Branches {
		if err := branch.Validate(); err != nil {
			return nil, err
		}
	}
	return &Middleware{table: table}, nil
}

func (m *Middleware) MappingTable() core.MappingTable {
	return m.table
}

func (m *Middleware) LoadBytes(data []byte, options LoadOptions) (*LoadResult, error) {
	prepared, err := m.prepareArchive(data, options)
	if err != nil {
		return nil, err
	}
	return m.loadPrepared(prepared)
}

type loadPreparation struct {
	decoded decodedArchive
	branch  core.BranchSpec
	ops     []loadOp
}

func (m *Middleware) prepareArchive(data []byte, options LoadOptions) (*loadPreparation, error) {
	decoded, err := decodeArchive(data)
	if err != nil {
		return nil, err
	}
	branch, ok := m.table.Branch(decoded.Archive.Branch)
	if !ok {
		return nil, &core.Error{
			Code: core.CodeSemantic, Op: "load",
			Message: "archive branch is not declared", Branch: decoded.Archive.Branch,
			FieldPath: "branch", Measured: decoded.Archive.Branch, Window: -1,
		}
	}
	if options.ExpectedBranch != "" && options.ExpectedBranch != branch.ID {
		return nil, &core.Error{
			Code: core.CodeSemantic, Op: "load",
			Message: "archive branch does not match expected branch",
			Branch:  branch.ID, FieldPath: "branch",
			Measured: branch.ID, Expected: options.ExpectedBranch, Window: -1,
		}
	}
	expectedRule := options.RuleVersion
	if expectedRule == "" {
		expectedRule = m.table.Version
	}
	if decoded.Archive.MappingVersion != expectedRule || m.table.Version != expectedRule {
		return nil, branchRuleError("load", decoded.Archive.MappingVersion, expectedRule, branch.ID)
	}
	if decoded.Archive.FormatVersion != 1 {
		return nil, semanticError("load", "formatVersion", branch.ID, "format",
			decoded.Archive.FormatVersion, "unsupported archive format version")
	}
	ops, err := m.prepareLoad(decoded, branch)
	if err != nil {
		return nil, err
	}
	return &loadPreparation{decoded: decoded, branch: branch, ops: ops}, nil
}

type loadOp struct {
	name        string
	kind        string
	role        string
	physical    string
	value       int64
	listIndex   int
	listValue   string
	conditional *core.ConditionalMapping
}

func (m *Middleware) prepareLoad(decoded decodedArchive, branch core.BranchSpec) ([]loadOp, error) {
	fieldByRole := make(map[string]string)
	for _, field := range branch.Fields {
		fieldByRole[field.Role] = field.Path
	}
	required := []string{"level", "coins", "experience", "coordinate.x", "coordinate.y"}
	for _, role := range required {
		if _, ok := fieldByRole[role]; !ok {
			return nil, semanticError("prepare-load", "fields."+role, branch.ID, role,
				"<undeclared>", "semantic field mapping is missing")
		}
	}
	listByRole := make(map[string]string)
	for _, list := range branch.Lists {
		listByRole[list.Role] = list.Path
	}
	if _, ok := listByRole["items"]; !ok {
		return nil, semanticError("prepare-load", "lists.items", branch.ID, "items",
			"<undeclared>", "list mapping is missing")
	}
	if _, ok := listByRole["quests"]; !ok {
		return nil, semanticError("prepare-load", "lists.quests", branch.ID, "quests",
			"<undeclared>", "list mapping is missing")
	}

	ops := make([]loadOp, 0, 32)
	roleOrder := []string{"level", "coins", "experience"}
	for _, role := range roleOrder {
		path := fieldByRole[role]
		raw, ok := core.Lookup(decoded.Archive.Save, path)
		if !ok {
			return nil, semanticError("prepare-load", path, branch.ID, role, "<missing>", "")
		}
		value, ok := core.DecodeInt64(raw)
		if !ok {
			return nil, semanticError("prepare-load", path, branch.ID, role, raw,
				"numeric semantic field is not an int64")
		}
		ops = append(ops, loadOp{name: role, kind: "field", role: role, physical: path, value: value})
	}

	itemsPath := listByRole["items"]
	itemsRaw, ok := core.Lookup(decoded.Archive.Save, itemsPath)
	if !ok {
		return nil, semanticError("prepare-load", itemsPath, branch.ID, "items", "<missing>", "")
	}
	items, ok := core.DecodeStringList(itemsRaw)
	if !ok {
		return nil, semanticError("prepare-load", itemsPath, branch.ID, "items", itemsRaw,
			"items field is not a string list")
	}
	for i, item := range items {
		ops = append(ops, loadOp{
			name: fmt.Sprintf("items[%04d]", i), kind: "list-item", role: "items",
			physical: itemsPath, listIndex: i, listValue: item,
		})
	}

	questsPath := listByRole["quests"]
	questsRaw, ok := core.Lookup(decoded.Archive.Save, questsPath)
	if !ok {
		return nil, semanticError("prepare-load", questsPath, branch.ID, "quests", "<missing>", "")
	}
	quests, ok := core.DecodeStringList(questsRaw)
	if !ok {
		return nil, semanticError("prepare-load", questsPath, branch.ID, "quests", questsRaw,
			"quests field is not a string list")
	}
	for i, quest := range quests {
		ops = append(ops, loadOp{
			name: fmt.Sprintf("quests[%04d]", i), kind: "list-item", role: "quests",
			physical: questsPath, listIndex: i, listValue: quest,
		})
	}
	for _, role := range []string{"coordinate.x", "coordinate.y"} {
		path := fieldByRole[role]
		raw, ok := core.Lookup(decoded.Archive.Save, path)
		if !ok {
			return nil, semanticError("prepare-load", path, branch.ID, role, "<missing>", "")
		}
		value, ok := core.DecodeInt64(raw)
		if !ok {
			return nil, semanticError("prepare-load", path, branch.ID, role, raw,
				"coordinate field is not an int64")
		}
		ops = append(ops, loadOp{name: role, kind: "field", role: role, physical: path, value: value})
	}
	rawTime, ok := core.Lookup(decoded.Archive.Save, branch.Time.Path)
	if !ok {
		return nil, semanticError("prepare-load", branch.Time.Path, branch.ID, "time", "<missing>", "")
	}
	ticks, ok := core.DecodeInt64(rawTime)
	if !ok {
		return nil, semanticError("prepare-load", branch.Time.Path, branch.ID, "time", rawTime,
			"time field is not an int64 tick value")
	}
	ops = append(ops, loadOp{name: "timeNanos", kind: "time", role: "time", physical: branch.Time.Path, value: ticks})

	conditionals := append([]core.ConditionalMapping(nil), branch.Conditionals...)
	sort.Slice(conditionals, func(i, j int) bool {
		if conditionals[i].Role != conditionals[j].Role {
			return conditionals[i].Role < conditionals[j].Role
		}
		return conditionals[i].Path < conditionals[j].Path
	})
	for i := range conditionals {
		conditional := conditionals[i]
		ops = append(ops, loadOp{
			name: "conditional:" + conditional.Role + ":" + conditional.Path,
			kind: "conditional", role: conditional.Role, physical: conditional.Path,
			conditional: &conditional,
		})
	}
	return ops, nil
}

func (m *Middleware) loadPrepared(prepared *loadPreparation) (*LoadResult, error) {
	return m.applyLoadOps(prepared.branch, prepared.decoded.Archive.Save, prepared.ops)
}

func (m *Middleware) applyLoadOps(branch core.BranchSpec, root map[string]any, ops []loadOp) (*LoadResult, error) {
	result := m.newLoadResult(branch, root)
	if err := m.applyLoadOpsInto(result, branch, root, ops); err != nil {
		return nil, err
	}
	result.Model.Normalize()
	result.Normalize()
	return result, nil
}

func (m *Middleware) newLoadResult(branch core.BranchSpec, root map[string]any) *LoadResult {
	result := &LoadResult{
		SourceBranch: branch.ID,
		RuleVersion:  m.table.Version,
		Model: core.SemanticModel{
			TimeScaleNanos: core.InternalTimeScaleNS,
			Flags:          make(map[string]bool),
		},
	}
	m.collectFlagValues(branch, root, &result.Model)
	return result
}

func (m *Middleware) applyLoadOpsInto(result *LoadResult, branch core.BranchSpec, root map[string]any, ops []loadOp) error {
	for _, op := range ops {
		target := op.name
		semanticRole := op.role
		switch op.kind {
		case "field":
			switch op.role {
			case "level":
				result.Model.Level = op.value
			case "coins":
				result.Model.Coins = op.value
			case "experience":
				result.Model.Experience = op.value
			case "coordinate.x":
				result.Model.Coordinate.X = op.value
			case "coordinate.y":
				result.Model.Coordinate.Y = op.value
			}
			result.Traces = append(result.Traces, m.trace("import", branch.ID, "runtime", op.physical, target, semanticRole, "applied"))
		case "list-item":
			if op.role == "items" {
				result.Model.Items = append(result.Model.Items, op.listValue)
			} else {
				result.Model.Quests = append(result.Model.Quests, op.listValue)
			}
			result.Traces = append(result.Traces, m.trace("import", branch.ID, "runtime",
				fmt.Sprintf("%s[%d]", op.physical, op.listIndex), target, semanticRole, "applied"))
		case "time":
			nanos, err := core.TicksToUnixNanos(op.value, branch.Time.EpochSeconds, branch.Time.TickNanos)
			if err != nil {
				return &core.Error{
					Code: core.CodeTime, Op: "convert-time",
					Message:   "integer time conversion overflow: " + err.Error(),
					FieldPath: op.physical, Branch: branch.ID, Measured: op.value,
					Semantic: "timeNanos", Window: -1,
				}
			}
			result.Model.TimeNanos = nanos
			result.Traces = append(result.Traces, m.trace("import", branch.ID, "runtime", op.physical, target, semanticRole, "applied"))
		case "conditional":
			value, err := m.evaluateCondition(branch, root, op.conditional.ConditionID, nil)
			if err != nil {
				return err
			}
			if !value {
				result.Traces = append(result.Traces, m.trace("import", branch.ID, "runtime",
					op.physical, "conditionals["+op.role+"]", op.role, "skipped"))
				continue
			}
			raw, ok := core.Lookup(root, op.physical)
			if !ok {
				return semanticError("load-conditional", op.physical, branch.ID, op.role,
					"<missing>", "active conditional field is missing")
			}
			contribution, ok := core.DecodeInt64(raw)
			if !ok {
				return semanticError("load-conditional", op.physical, branch.ID, op.role,
					raw, "active conditional field is not an int64")
			}
			switch op.role {
			case "coins":
				updated, err := core.CheckedAdd(result.Model.Coins, contribution)
				if err != nil {
					return semanticError("load-conditional", op.physical, branch.ID, op.role,
						contribution, "conditional contribution overflows semantic total")
				}
				result.Model.Coins = updated
			case "experience":
				updated, err := core.CheckedAdd(result.Model.Experience, contribution)
				if err != nil {
					return semanticError("load-conditional", op.physical, branch.ID, op.role,
						contribution, "conditional contribution overflows semantic total")
				}
				result.Model.Experience = updated
			}
			result.Model.Conditionals = append(result.Model.Conditionals, core.ConditionalContribution{
				Role: op.role, Value: contribution, Physical: op.physical,
				ConditionID: op.conditional.ConditionID, Source: branch.ID,
			})
			result.Traces = append(result.Traces, m.trace("import", branch.ID, "runtime",
				op.physical, "conditionals["+op.role+"]", op.role, "applied"))
		default:
			return semanticError("load", op.physical, branch.ID, op.role, op.kind,
				"unknown load operation")
		}
	}
	m.logEvaluation(branch, root, &result.Evaluations)
	return nil
}

func (m *Middleware) trace(direction, sourceBranch, targetBranch, sourcePath, targetPath, role, status string) Trace {
	return Trace{
		Direction: direction, SourceBranch: sourceBranch, TargetBranch: targetBranch,
		SourcePath: sourcePath, TargetPath: targetPath, SemanticRole: role,
		RuleVersion: m.table.Version, Status: status,
	}
}

func (m *Middleware) LoadFile(path string, options LoadOptions) (*LoadResult, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return m.LoadBytes(data, options)
}
