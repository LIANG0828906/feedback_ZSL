// Package loader maps the three supported schema dialects onto the single
// internal model. Every numeric conversion goes through fixedpoint so unit
// and semantics differences are explicit and checked.
package loader

import (
	"encoding/json"

	"coldkernel/internal/errcode"
	"coldkernel/internal/fixedpoint"
	"coldkernel/internal/gen"
	"coldkernel/internal/model"
)

const (
	SchemaV1 = "coldkernel/v1"
	SchemaV2 = "coldkernel/v2"
	SchemaV3 = "coldkernel/v3"
)

// Load parses raw JSON and dispatches to the matching schema dialect.
// Unrecognised versions or invalid field combinations return stable codes
// instead of silently degrading.
func Load(raw []byte) (*model.Project, error) {
	head := struct {
		SchemaVersion string `json:"schema_version"`
	}{}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, errcode.New(errcode.ESchemaField, "project is not valid JSON", "err", err.Error())
	}
	switch head.SchemaVersion {
	case SchemaV1:
		var s gen.SnapshotV1
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errcode.New(errcode.ESchemaField, "invalid v1 payload", "err", err.Error())
		}
		return loadV1(&s)
	case SchemaV2:
		var s gen.SnapshotV2
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errcode.New(errcode.ESchemaField, "invalid v2 payload", "err", err.Error())
		}
		return loadV2(&s)
	case SchemaV3:
		var s gen.SnapshotV3
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errcode.New(errcode.ESchemaField, "invalid v3 payload", "err", err.Error())
		}
		return loadV3(&s)
	default:
		return nil, errcode.New(errcode.ESchemaVersion, "unknown schema version",
			"version", head.SchemaVersion)
	}
}

func finalize(p *model.Project) (*model.Project, error) {
	known := make(map[string]*model.Function, len(p.Functions))
	seenFn := map[string]bool{}
	seenReq := map[string]bool{}
	for _, f := range p.Functions {
		if seenFn[f.ID] {
			return nil, errcode.New(errcode.EDupFunction, "duplicate function", "function", f.ID)
		}
		seenFn[f.ID] = true
		if err := validateFunction(f, p.ConversionWidth); err != nil {
			return nil, err
		}
		known[f.ID] = f
	}
	var prevTick int64 = -1
	for i, r := range p.Requests {
		if seenReq[r.ID] {
			return nil, errcode.New(errcode.EDupRequest, "duplicate request", "request", r.ID)
		}
		seenReq[r.ID] = true
		r.Seq = i
		if err := validateRequest(r, known, prevTick); err != nil {
			return nil, err
		}
		prevTick = r.ArriveTick
	}
	p.Index()
	return p, nil
}

func loadV1(s *gen.SnapshotV1) (*model.Project, error) {
	if s.TickUnit != "tick" {
		return nil, errcode.New(errcode.EUnitUnknown, "v1 expects tick unit", "unit", s.TickUnit)
	}
	if s.TickMs <= 0 {
		return nil, errcode.New(errcode.EMissingField, "tick_ms must be positive")
	}
	p := &model.Project{
		SchemaVersion:   SchemaV1,
		SourceVersion:   SchemaV1,
		TickMs:          s.TickMs,
		HorizonTicks:    s.HorizonTicks,
		Seed:            s.Seed,
		ConversionWidth: s.FixedWidth,
		FixedTolerance:  s.Tolerance,
	}
	warm := map[string]int64{}
	for _, w := range s.WarmPool {
		warm[w.FunctionID]++
	}
	for _, sf := range s.Functions {
		ratio := int64(fixedpoint.ScalePerTenThou) // default: all cold allowed
		if sf.ColdBudgetPermille != nil {
			r, err := fixedpoint.BudgetRatio(s.FixedWidth, *sf.ColdBudgetPermille, fixedpoint.ScalePermille)
			if err != nil {
				return nil, err
			}
			ratio = r.Value
		}
		f := &model.Function{
			ID:               sf.FunctionID,
			ConcurrencyLimit: sf.ConcurrencyLimit,
			Reserved:         sf.ReservedSlots,
			WarmPoolSize:     warm[sf.FunctionID],
			ColdMinTicks:     sf.ColdMinTicks,
			ColdMaxTicks:     sf.ColdMaxTicks,
			ColdTimeoutTicks: sf.ColdTimeoutTicks,
			ColdBudgetRatio:  ratio,
			QueueLimit:       sf.QueueLimit,
			WaitLimitTicks:   sf.WaitLimitTicks,
		}
		p.Functions = append(p.Functions, f)
	}
	for _, sr := range s.Requests {
		p.Requests = append(p.Requests, &model.Request{
			ID:         sr.RequestID,
			FunctionID: sr.FunctionID,
			ArriveTick: sr.ArriveTick,
			Slots:      sr.Slots,
			Priority:   model.Priority(sr.Priority),
			Tenant:     sr.Tenant,
			HoldTicks:  sr.HoldTicks,
		})
	}
	resolveAbsoluteBudgets(p, len(s.Requests))
	return finalize(p)
}

func resolveAbsoluteBudgets(p *model.Project, reqCount int) {
	for _, f := range p.Functions {
		mul := fixedpoint.Mul(p.ConversionWidth, f.ColdBudgetRatio, int64(reqCount))
		div := fixedpoint.Div(p.ConversionWidth, mul.Value, fixedpoint.ScalePerTenThou)
		f.ColdBudgetAbsolute = div.Value
	}
}

func loadV2(s *gen.SnapshotV2) (*model.Project, error) {
	if s.TimeUnit != "millisecond" {
		return nil, errcode.New(errcode.EUnitUnknown, "v2 expects millisecond unit", "unit", s.TimeUnit)
	}
	if s.TickMs <= 0 {
		return nil, errcode.New(errcode.EMissingField, "tick_ms must be positive")
	}
	p := &model.Project{
		SchemaVersion:   SchemaV2,
		SourceVersion:   SchemaV2,
		TickMs:          s.TickMs,
		Seed:            s.Seed,
		ConversionWidth: s.FixedWidth,
		FixedTolerance:  s.Tolerance,
	}
	// Horizon ms -> ticks, accumulate conversion error.
	hz := fixedpoint.MsToTicks(s.FixedWidth, s.HorizonMs, s.TickMs)
	if hz.Overflowed {
		return nil, errcode.New(errcode.EOverflow, "horizon conversion overflow")
	}
	p.HorizonTicks = hz.Value

	warm := map[string]int64{}
	for _, pool := range s.WarmPools {
		warm[pool.FunctionID] += int64(len(pool.Instances))
	}
	for _, sf := range s.Functions {
		// remaining-available semantics -> absolute limit.
		abs := fixedpoint.AbsoluteFromRemaining(s.FixedWidth, sf.BaselineOccupied, sf.RemainingAvailable)
		if abs.Overflowed {
			return nil, errcode.New(errcode.EOverflow, "remaining->absolute overflow", "function", sf.FunctionID)
		}
		ratio := int64(fixedpoint.ScalePerTenThou)
		if sf.ColdBudgetPermyriad != nil {
			r, err := fixedpoint.BudgetRatio(s.FixedWidth, *sf.ColdBudgetPermyriad, fixedpoint.ScalePerTenThou)
			if err != nil {
				return nil, err
			}
			ratio = r.Value
		}
		f := &model.Function{
			ID:               sf.FunctionID,
			ConcurrencyLimit: abs.Value,
			Reserved:         sf.ReservedSlots,
			WarmPoolSize:     warm[sf.FunctionID],
			ColdMinTicks:     mustTick(s.FixedWidth, sf.ColdMinMs, s.TickMs),
			ColdMaxTicks:     mustTick(s.FixedWidth, sf.ColdMaxMs, s.TickMs),
			ColdTimeoutTicks: mustTick(s.FixedWidth, sf.ColdTimeoutMs, s.TickMs),
			ColdBudgetRatio:  ratio,
			QueueLimit:       sf.QueueLimit,
			WaitLimitTicks:   mustTick(s.FixedWidth, sf.WaitLimitMs, s.TickMs),
		}
		p.Functions = append(p.Functions, f)
	}
	for _, sr := range s.Requests {
		arrive := fixedpoint.MsToTicks(s.FixedWidth, sr.ArriveMs, s.TickMs)
		hold := fixedpoint.MsToTicks(s.FixedWidth, sr.HoldMs, s.TickMs)
		p.Requests = append(p.Requests, &model.Request{
			ID:         sr.RequestID,
			FunctionID: sr.FunctionID,
			ArriveTick: arrive.Value,
			Slots:      sr.Slots,
			Priority:   model.Priority(sr.Class),
			Tenant:     sr.Tenant,
			HoldTicks:  hold.Value,
		})
	}
	resolveAbsoluteBudgets(p, len(s.Requests))
	return finalize(p)
}

func mustTick(width int, ms, tickMs int64) int64 {
	r := fixedpoint.MsToTicks(width, ms, tickMs)
	return r.Value
}

func loadV3(s *gen.SnapshotV3) (*model.Project, error) {
	if s.TimeUnit != "second_milli" {
		return nil, errcode.New(errcode.EUnitUnknown, "v3 expects second_milli unit", "unit", s.TimeUnit)
	}
	if s.TickMs <= 0 {
		return nil, errcode.New(errcode.EMissingField, "tick_ms must be positive")
	}
	p := &model.Project{
		SchemaVersion:   SchemaV3,
		SourceVersion:   SchemaV3,
		TickMs:          s.TickMs,
		Seed:            s.Seed,
		ConversionWidth: s.FixedWidth,
		FixedTolerance:  s.Tolerance,
	}
	// sec_milli is already an integer ms magnitude; convert directly to ticks.
	hz := fixedpoint.MsToTicks(s.FixedWidth, s.HorizonSecMilli, s.TickMs)
	p.HorizonTicks = hz.Value

	warm := map[string]int64{}
	for _, pool := range s.WarmPools {
		warm[pool.FunctionID] += int64(len(pool.Instances))
	}
	for _, sf := range s.Functions {
		ratio := int64(fixedpoint.ScalePerTenThou)
		if sf.ColdBudgetPermyriad != nil {
			r, err := fixedpoint.BudgetRatio(s.FixedWidth, *sf.ColdBudgetPermyriad, fixedpoint.ScalePerTenThou)
			if err != nil {
				return nil, err
			}
			ratio = r.Value
		}
		f := &model.Function{
			ID:               sf.FunctionID,
			ConcurrencyLimit: sf.ConcurrencyLimit,
			Reserved:         sf.ReservedSlots,
			WarmPoolSize:     warm[sf.FunctionID],
			ColdMinTicks:     mustTick(s.FixedWidth, sf.ColdMinSecMilli, s.TickMs),
			ColdMaxTicks:     mustTick(s.FixedWidth, sf.ColdMaxSecMilli, s.TickMs),
			ColdTimeoutTicks: mustTick(s.FixedWidth, sf.ColdTimeoutSecMilli, s.TickMs),
			ColdBudgetRatio:  ratio,
			QueueLimit:       sf.QueueLimit,
			WaitLimitTicks:   mustTick(s.FixedWidth, sf.WaitLimitSecMilli, s.TickMs),
		}
		p.Functions = append(p.Functions, f)
	}
	for _, sr := range s.Requests {
		arrive := fixedpoint.MsToTicks(s.FixedWidth, sr.ArriveSecMilli, s.TickMs)
		hold := fixedpoint.MsToTicks(s.FixedWidth, sr.HoldSecMilli, s.TickMs)
		p.Requests = append(p.Requests, &model.Request{
			ID:         sr.RequestID,
			FunctionID: sr.FunctionID,
			ArriveTick: arrive.Value,
			Slots:      sr.Slots,
			Priority:   model.Priority(sr.Priority),
			Tenant:     sr.Tenant,
			HoldTicks:  hold.Value,
		})
	}
	resolveAbsoluteBudgets(p, len(s.Requests))
	return finalize(p)
}
