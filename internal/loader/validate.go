package loader

import (
	"coldkernel/internal/errcode"
	"coldkernel/internal/fixedpoint"
	"coldkernel/internal/model"
)

// validateFunction applies the version-independent declaration rules and
// returns a stable E_xxx code for every injectable bad input. It never
// mutates the source snapshot.
func validateFunction(f *model.Function, width int) error {
	ctx := []any{"function", f.ID}
	if f.ConcurrencyLimit < 0 {
		return errcode.New(errcode.ENegConcurrency, "concurrency limit is negative", ctx...)
	}
	if f.Reserved < 0 {
		return errcode.New(errcode.ENegReserved, "reserved concurrency is negative", ctx...)
	}
	if f.Reserved > f.ConcurrencyLimit {
		return errcode.New(errcode.EReservedExceeds, "reserved exceeds concurrency limit",
			append(ctx, "reserved", f.Reserved, "limit", f.ConcurrencyLimit)...)
	}
	if f.WarmPoolSize < 0 || f.WarmPoolSize > f.ConcurrencyLimit {
		return errcode.New(errcode.EPoolOutOfRange, "warm pool size out of range",
			append(ctx, "pool", f.WarmPoolSize, "limit", f.ConcurrencyLimit)...)
	}
	if f.ColdTimeoutTicks < 0 || f.ColdMinTicks < 0 || f.ColdMaxTicks < 0 {
		return errcode.New(errcode.ENegColdTimeout, "cold duration/timeout is negative", ctx...)
	}
	if f.ColdMaxTicks < f.ColdMinTicks {
		return errcode.New(errcode.ESchemaField, "cold max < cold min", ctx...)
	}
	if f.QueueLimit < 0 || f.WaitLimitTicks < 0 {
		return errcode.New(errcode.ESchemaField, "queue/wait limit negative", ctx...)
	}
	if f.ColdBudgetRatio < 0 || f.ColdBudgetRatio > fixedpoint.ScalePerTenThou {
		return errcode.New(errcode.EBadBudget, "budget ratio out of canonical range", ctx...)
	}
	// Width sanity: limits must fit the declared integer width.
	min, max := fixedpoint.Bounds(width)
	if f.ConcurrencyLimit > max || f.ConcurrencyLimit < min {
		return errcode.New(errcode.EOverflow, "limit outside declared width", ctx...)
	}
	return nil
}

// validateRequest applies arrival/ordering and reference rules.
func validateRequest(r *model.Request, known map[string]*model.Function, prevTick int64) error {
	ctx := []any{"request", r.ID, "function", r.FunctionID}
	if r.ArriveTick < 0 {
		return errcode.New(errcode.ETickOrder, "arrival tick negative", ctx...)
	}
	if r.ArriveTick < prevTick {
		return errcode.New(errcode.ETickOrder, "arrival ticks out of order",
			append(ctx, "tick", r.ArriveTick, "prev", prevTick)...)
	}
	if r.Slots <= 0 {
		return errcode.New(errcode.ESchemaField, "expected slots must be positive", ctx...)
	}
	if _, ok := known[r.FunctionID]; !ok {
		return errcode.New(errcode.EUnknownFunction, "request references unknown function", ctx...)
	}
	if r.Priority != model.PriorityReserved && r.Priority != model.PriorityShared {
		return errcode.New(errcode.EBadEnum, "unknown priority", append(ctx, "priority", string(r.Priority))...)
	}
	return nil
}
