package interop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"saveinterop/internal/checkpoint"
	"saveinterop/internal/core"
)

type CheckpointOptions struct {
	DBPath string
	// WindowSize is the fixed number of semantic operations per checkpoint.
	WindowSize int
	// RecomputeOnConflict invalidates the whole run and starts from window zero
	// when any persisted version or hash does not match.
	RecomputeOnConflict bool
}

type CheckpointResult struct {
	LoadResult
	Windows        int
	ResumedWindows int
	Recovery       *core.Error
}

type importWindowPayload struct {
	Model       core.SemanticModel `json:"model"`
	Traces      []Trace            `json:"traces"`
	Rulings     []Ruling           `json:"rulings"`
	Evaluations []Evaluation       `json:"evaluations"`
}

func (m *Middleware) ImportCheckpointed(ctx context.Context, data []byte, loadOptions LoadOptions, options CheckpointOptions) (*CheckpointResult, error) {
	if options.WindowSize <= 0 {
		return nil, &core.Error{
			Code: core.CodeVersion, Op: "checkpoint-import",
			Message:  "checkpoint window size must be positive",
			Measured: options.WindowSize, Window: -1,
		}
	}
	prepared, err := m.prepareArchive(data, loadOptions)
	if err != nil {
		return nil, err
	}
	totalWindows := (len(prepared.ops) + options.WindowSize - 1) / options.WindowSize
	meta := checkpoint.RunMetadata{
		RunID: core.HashString(strings.Join([]string{
			checkpoint.EngineVersion, "import", prepared.decoded.Hash, prepared.branch.ID, "runtime",
			m.table.Version, strconv.Itoa(options.WindowSize),
		}, "|")),
		Mode: "import", SourceBranch: prepared.branch.ID, TargetBranch: "runtime",
		MappingVersion: m.table.Version, WindowSize: options.WindowSize,
		InputHash: core.HashString(checkpoint.EngineVersion + "|" + prepared.decoded.Hash),
	}
	store, err := checkpoint.Open(options.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	records, err := store.ValidateOrCreate(ctx, meta)
	var recovery *core.Error
	if err != nil && core.CodeOf(err) == core.CodeVersion && options.RecomputeOnConflict {
		recovery = asCoreError(err)
		records, err = store.ValidateOrCreate(ctx, meta)
	}
	if err != nil {
		return nil, err
	}
	if len(records) > totalWindows {
		invalidateErr := store.Invalidate(ctx, meta.RunID)
		if invalidateErr != nil {
			return nil, invalidateErr
		}
		return nil, &core.Error{
			Code: core.CodeVersion, Op: "checkpoint-import",
			Message:  "checkpoint has more windows than the archive can produce",
			Expected: fmt.Sprintf("%d", totalWindows), Actual: fmt.Sprintf("%d", len(records)),
			Window: totalWindows,
		}
	}

	var result *LoadResult
	resumed := len(records)
	for i, record := range records {
		var payload importWindowPayload
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			if invalidateErr := store.Invalidate(ctx, meta.RunID); invalidateErr != nil {
				return nil, invalidateErr
			}
			return nil, &core.Error{
				Code: core.CodeVersion, Op: "checkpoint-import",
				Message: "checkpoint payload is invalid: " + err.Error(),
				Window:  i,
			}
		}
		if result == nil {
			result = &LoadResult{
				SourceBranch: prepared.branch.ID,
				RuleVersion:  m.table.Version,
				ArchiveHash:  prepared.decoded.Hash,
				Model:        payload.Model.Clone(),
			}
		} else {
			result.Model = payload.Model.Clone()
		}
		result.Traces = append(result.Traces, payload.Traces...)
		result.Rulings = append(result.Rulings, payload.Rulings...)
		result.Evaluations = append(result.Evaluations, payload.Evaluations...)
	}
	if result == nil {
		result = m.newLoadResult(prepared.branch, prepared.decoded.Archive.Save)
		result.ArchiveHash = prepared.decoded.Hash
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
		start := windowIndex * options.WindowSize
		end := start + options.WindowSize
		if end > len(prepared.ops) {
			end = len(prepared.ops)
		}
		traceOffset := len(result.Traces)
		rulingOffset := len(result.Rulings)
		evaluationOffset := len(result.Evaluations)
		if err := m.applyLoadOpsInto(result, prepared.branch, prepared.decoded.Archive.Save, prepared.ops[start:end]); err != nil {
			return nil, withWindow(err, windowIndex)
		}
		payload := importWindowPayload{
			Model:       result.Model.Clone(),
			Traces:      append([]Trace(nil), result.Traces[traceOffset:]...),
			Rulings:     append([]Ruling(nil), result.Rulings[rulingOffset:]...),
			Evaluations: append([]Evaluation(nil), result.Evaluations[evaluationOffset:]...),
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		delta := hashWindowOps(prepared.ops[start:end], prepared.decoded.Archive.Save, result.Model)
		cumulative := checkpoint.NextCumulative(previousCumulative, delta)
		record := checkpoint.WindowRecord{
			Index: windowIndex, ItemStart: start, ItemEnd: end,
			FingerprintDelta: delta, CumulativeFingerprint: cumulative,
			Payload: payloadBytes,
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
	result.Model.Normalize()
	result.Normalize()
	return &CheckpointResult{
		LoadResult: *result, Windows: totalWindows,
		ResumedWindows: resumed, Recovery: recovery,
	}, nil
}

func hashWindowOps(ops []loadOp, root map[string]any, model core.SemanticModel) string {
	type fingerprintOp struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Role        string `json:"role"`
		Physical    string `json:"physical"`
		Value       int64  `json:"value,omitempty"`
		ListValue   string `json:"listValue,omitempty"`
		Condition   string `json:"condition,omitempty"`
		ListIndex   int    `json:"listIndex,omitempty"`
		Observed    string `json:"observed,omitempty"`
		PhysicalV   int64  `json:"physicalValue,omitempty"`
		HasPhysical bool   `json:"hasPhysicalValue,omitempty"`
	}
	items := make([]fingerprintOp, 0, len(ops))
	for _, op := range ops {
		item := fingerprintOp{
			Name: op.name, Kind: op.kind, Role: op.role, Physical: op.physical,
			Value: op.value, ListValue: op.listValue, ListIndex: op.listIndex,
		}
		if op.conditional != nil {
			item.Condition = op.conditional.ConditionID
		}
		switch op.role {
		case "level":
			item.Observed = core.NewEntry("level", model.Level).Value
		case "coins":
			item.Observed = core.NewEntry("coins", model.Coins).Value
		case "experience":
			item.Observed = core.NewEntry("experience", model.Experience).Value
		case "coordinate.x":
			item.Observed = core.NewEntry("coordinate.x", model.Coordinate.X).Value
		case "coordinate.y":
			item.Observed = core.NewEntry("coordinate.y", model.Coordinate.Y).Value
		case "time":
			item.Observed = core.NewEntry("timeNanos", model.TimeNanos).Value
		}
		if op.conditional != nil {
			if raw, ok := core.Lookup(root, op.physical); ok {
				if value, ok := core.DecodeInt64(raw); ok {
					item.PhysicalV = value
					item.HasPhysical = true
				}
			}
		}
		items = append(items, item)
	}
	payload, _ := json.Marshal(items)
	return core.HashBytes(payload)
}

func withWindow(err error, window int) error {
	var coreErr *core.Error
	if errors.As(err, &coreErr) {
		clone := *coreErr
		if clone.Window < 0 {
			clone.Window = window
		}
		return &clone
	}
	return &core.Error{
		Code: core.CodeVersion, Op: "checkpoint-window",
		Message: err.Error(), Window: window,
	}
}

func asCoreError(err error) *core.Error {
	var coreErr *core.Error
	if errors.As(err, &coreErr) {
		clone := *coreErr
		return &clone
	}
	return &core.Error{
		Code: core.CodeVersion, Op: "checkpoint",
		Message: err.Error(), Window: -1,
	}
}
