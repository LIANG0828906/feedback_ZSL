package format

import (
	"errors"
	"fmt"

	"economycore/fixed"
	"economycore/model"
)

// ExportOptions controls explicit downgrade behavior.
type ExportOptions struct {
	AllowLossy bool
	RoundMode  fixed.RoundMode
}

// ExportResult carries the encoded document, upgrade traces and any downgrade
// dispositions.
type ExportResult struct {
	Version string                  `json:"version"`
	Data    []byte                  `json:"-"`
	Traces  []model.ConversionTrace `json:"traces,omitempty"`
	Losses  []model.Loss            `json:"losses,omitempty"`
	Lossy   bool                    `json:"lossy"`
}

type exportState struct {
	allowLossy bool
	roundMode  fixed.RoundMode
	losses     []model.Loss
}

func (s *exportState) addLoss(loss model.Loss) {
	for _, existing := range s.losses {
		if existing.Path == loss.Path && existing.NodeID == loss.NodeID && existing.Field == loss.Field {
			return
		}
	}
	s.losses = append(s.losses, loss)
}

// Export converts the unified model back to one of the supported schemas.
// Strict mode returns E_LOSSY with a complete loss list rather than silently
// dropping fields. AllowLossy mode writes a target-version document using the
// recorded dispositions.
func Export(m *model.Model, target string, opts ExportOptions) (*ExportResult, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, model.NewError(model.ESchema, "", "nil model")
	}
	normalized := m.Clone()
	normalized.Normalize()
	if err := normalized.Validate(); err != nil {
		return nil, err
	}
	mode := opts.RoundMode
	if mode == "" {
		mode = fixed.RoundMode(m.RoundMode)
		if mode == "" {
			mode = fixed.RoundFloor
		}
	}
	if _, err := fixed.ParseRoundMode(string(mode)); err != nil {
		return nil, model.NewError(model.ESchema, "/rounding", err.Error())
	}

	work, traces, err := Upgrade(normalized, target)
	if err != nil {
		return nil, err
	}
	if work.ConversionVersion == "" {
		work.ConversionVersion = model.DefaultConversionVersion
	}
	losses := analyzeLosses(work, target, mode)
	result := &ExportResult{Version: target, Traces: traces, Losses: losses, Lossy: len(losses) > 0}
	if len(losses) > 0 && !opts.AllowLossy {
		err := model.NewError(model.ELossy, "", "target schema cannot express all semantic fields")
		err.Losses = losses
		return result, err
	}

	state := exportState{allowLossy: opts.AllowLossy, roundMode: mode}
	var data []byte
	switch target {
	case model.SchemaV1:
		data, err = encodeV1(work, state)
	case model.SchemaV2:
		data, err = encodeV2(work, state)
	case model.SchemaV3:
		data, err = encodeV3(work, state)
	}
	if err != nil {
		return nil, err
	}
	result.Data = data
	result.Losses = mergeLosses(losses, state.losses)
	result.Lossy = len(result.Losses) > 0
	return result, nil
}

func mergeLosses(first, second []model.Loss) []model.Loss {
	out := append([]model.Loss(nil), first...)
	for _, loss := range second {
		found := false
		for _, existing := range out {
			if existing.Path == loss.Path && existing.NodeID == loss.NodeID && existing.Field == loss.Field {
				found = true
				break
			}
		}
		if !found {
			out = append(out, loss)
		}
	}
	return out
}

func analyzeLosses(m *model.Model, target string, mode fixed.RoundMode) []model.Loss {
	if target == model.SchemaV3 {
		return nil
	}
	var losses []model.Loss
	add := func(loss model.Loss) { losses = append(losses, loss) }
	if target == model.SchemaV1 {
		if m.CycleMs%250 != 0 {
			add(model.Loss{Path: "/timing/ticks_per_cycle", Field: "cycleMs", OldValue: m.CycleMs, Disposition: "rounded_" + string(mode), Reason: "v1 uses 250ms ticks"})
		}
		if m.WorkersMaximum != model.DefaultWorkersMaximum && m.WorkersMaximum != m.WorkersTotal {
			add(model.Loss{Path: "/workforce/maximum", Field: "workersMaximum", OldValue: m.WorkersMaximum, Disposition: "dropped", Reason: "v1 has no workforce maximum"})
		}
		for _, node := range m.Nodes {
			if node.RateExpr.HasVariable() {
				add(model.Loss{Path: "/nodes/" + node.ID + "/rate_formula", NodeID: node.ID, Field: "rateFormula", OldValue: node.RateExpr, Disposition: "flattened_at_export_worker_count", Reason: "v1 has no expression tree"})
			}
			rate, ok, err := node.ConstantRate()
			if err != nil {
				add(model.Loss{Path: "/nodes/" + node.ID + "/rate_formula", NodeID: node.ID, Field: "rateFormula", OldValue: node.RateExpr, Disposition: "evaluation_failed", Reason: err.Error()})
			}
			if ok && rate%10 != 0 {
				add(model.Loss{Path: "/nodes/" + node.ID + "/rate_permille", NodeID: node.ID, Field: "rate", OldValue: rate, Disposition: "rounded_" + string(mode), Reason: "v1 uses per-mille rate scale"})
			}
			if node.WorkerEfficiencyFixed%10 != 0 {
				add(model.Loss{Path: "/nodes/" + node.ID + "/worker_efficiency_permille", NodeID: node.ID, Field: "workerEfficiency", OldValue: node.WorkerEfficiencyFixed, Disposition: "rounded_" + string(mode), Reason: "v1 uses per-mille worker scale"})
			}
			if node.Upgrade.RateAddFixed%10 != 0 {
				add(model.Loss{Path: "/nodes/" + node.ID + "/upgrade/rate_add_permille", NodeID: node.ID, Field: "upgradeRateAdd", OldValue: node.Upgrade.RateAddFixed, Disposition: "rounded_" + string(mode), Reason: "v1 uses per-mille rate scale"})
			}
		}
	}
	if m.OverflowPolicy != "" && m.OverflowPolicy != model.DefaultOverflowPolicy {
		add(model.Loss{Path: "/overflowPolicy", Field: "overflowPolicy", OldValue: m.OverflowPolicy, Disposition: "forced_cap_drop", Reason: target + " has no overflow policy"})
	}
	if m.UpgradeDiscountBasisPoints != 0 {
		add(model.Loss{Path: "/upgradeDiscountBasisPoints", Field: "upgradeDiscountBasisPoints", OldValue: m.UpgradeDiscountBasisPoints, Disposition: "flattened_into_upgrade_costs", Reason: target + " has no upgrade discount"})
	}
	return losses
}

func validateTarget(target string) error {
	switch target {
	case model.SchemaV1, model.SchemaV2, model.SchemaV3:
		return nil
	default:
		return model.NewError(model.EVer, "/specVersion", "unsupported target version "+target)
	}
}

func downscale(value, divisor int64, mode fixed.RoundMode) (int64, bool, error) {
	if divisor <= 0 {
		return 0, false, errors.New("non-positive divisor")
	}
	scaled, err := fixed.MulDiv(value, 1, divisor, mode)
	if err != nil {
		return 0, false, err
	}
	return scaled, value%divisor == 0, nil
}

func cloneCost(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for id, value := range in {
		out[id] = value
	}
	return out
}

func flattenDiscount(cost, discountBasisPoints int64) (int64, error) {
	if discountBasisPoints < 0 || discountBasisPoints > 10000 {
		return 0, fmt.Errorf("discount out of range")
	}
	remaining := 10000 - discountBasisPoints
	return fixed.MulDiv(cost, remaining, 10000, fixed.RoundCeil)
}
