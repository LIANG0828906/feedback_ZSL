package gen

import "fmt"

// WarmInstanceID returns the deterministic instance id for a function's
// warm pool slot. All three schemas emit the same member set.
func WarmInstanceID(funcID string, idx int) string {
	return fmt.Sprintf("warm-%s-%02d", funcID, idx)
}

func permille(permyriad int64) *int64 {
	v := permyriad / 10
	return &v
}

// BuildV1 renders the canonical spec in the v1 dialect (ticks, absolute,
// flat pool, ‰ budget).
func BuildV1(s *Spec) *SnapshotV1 {
	out := &SnapshotV1{
		SchemaVersion: "coldkernel/v1",
		TickUnit:      "tick",
		TickMs:        s.TickMs,
		HorizonTicks:  s.HorizonTicks,
		Seed:          s.Seed,
		FixedWidth:    s.ConversionWidth,
		Tolerance:     s.FixedTolerance,
	}
	for _, f := range s.Functions {
		out.Functions = append(out.Functions, V1Function{
			FunctionID:         f.ID,
			ConcurrencyLimit:   f.ConcurrencyLimit,
			ReservedSlots:      f.Reserved,
			ColdMinTicks:       f.ColdMinTicks,
			ColdMaxTicks:       f.ColdMaxTicks,
			ColdTimeoutTicks:   f.ColdTimeoutTicks,
			ColdBudgetPermille: permille(f.BudgetPermyriad),
			QueueLimit:         f.QueueLimit,
			WaitLimitTicks:     f.WaitLimitTicks,
		})
		for i := int64(0); i < f.WarmPoolSize; i++ {
			out.WarmPool = append(out.WarmPool, V1WarmEntry{
				FunctionID: f.ID,
				InstanceID: WarmInstanceID(f.ID, int(i)),
			})
		}
	}
	for _, r := range s.Requests {
		out.Requests = append(out.Requests, V1Request{
			RequestID:  r.ID,
			FunctionID: r.FunctionID,
			ArriveTick: r.ArriveTick,
			Slots:      r.Slots,
			Priority:   string(r.Priority),
			Tenant:     r.Tenant,
			HoldTicks:  r.HoldTicks,
		})
	}
	return out
}

// BuildV2 renders the spec in the v2 dialect (milliseconds, remaining
// concurrency, nested pool, ‱ budget).
func BuildV2(s *Spec) *SnapshotV2 {
	out := &SnapshotV2{
		SchemaVersion: "coldkernel/v2",
		TimeUnit:      "millisecond",
		TickMs:        s.TickMs,
		HorizonMs:     s.HorizonTicks * s.TickMs,
		Seed:          s.Seed,
		FixedWidth:    s.ConversionWidth,
		Tolerance:     s.FixedTolerance,
	}
	for _, f := range s.Functions {
		budget := f.BudgetPermyriad
		out.Functions = append(out.Functions, V2Function{
			FunctionID:          f.ID,
			RemainingAvailable:  f.ConcurrencyLimit, // baseline occupied == 0
			BaselineOccupied:    0,
			ReservedSlots:       f.Reserved,
			ColdMinMs:           f.ColdMinTicks * s.TickMs,
			ColdMaxMs:           f.ColdMaxTicks * s.TickMs,
			ColdTimeoutMs:       f.ColdTimeoutTicks * s.TickMs,
			ColdBudgetPermyriad: &budget,
			QueueLimit:          f.QueueLimit,
			WaitLimitMs:         f.WaitLimitTicks * s.TickMs,
		})
		pool := V2Pool{FunctionID: f.ID}
		for i := int64(0); i < f.WarmPoolSize; i++ {
			pool.Instances = append(pool.Instances, WarmInstanceID(f.ID, int(i)))
		}
		out.WarmPools = append(out.WarmPools, pool)
	}
	for _, r := range s.Requests {
		out.Requests = append(out.Requests, V2Request{
			RequestID:  r.ID,
			FunctionID: r.FunctionID,
			ArriveMs:   r.ArriveTick * s.TickMs,
			Slots:      r.Slots,
			Class:      string(r.Priority),
			Tenant:     r.Tenant,
			HoldMs:     r.HoldTicks * s.TickMs,
		})
	}
	return out
}

// BuildV3 renders the spec in the v3 dialect (scaled decimal seconds,
// absolute concurrency, nested pool, ‱ budget).
func BuildV3(s *Spec) *SnapshotV3 {
	out := &SnapshotV3{
		SchemaVersion:   "coldkernel/v3",
		TimeUnit:        "second_milli",
		TickMs:          s.TickMs,
		HorizonSecMilli: s.HorizonTicks * s.TickMs, // ms magnitude == sec*1000
		Seed:            s.Seed,
		FixedWidth:      s.ConversionWidth,
		Tolerance:       s.FixedTolerance,
}
	for _, f := range s.Functions {
		budget := f.BudgetPermyriad
		out.Functions = append(out.Functions, V3Function{
			FunctionID:          f.ID,
			ConcurrencyLimit:    f.ConcurrencyLimit,
			ReservedSlots:       f.Reserved,
			ColdMinSecMilli:     f.ColdMinTicks * s.TickMs,
			ColdMaxSecMilli:     f.ColdMaxTicks * s.TickMs,
			ColdTimeoutSecMilli: f.ColdTimeoutTicks * s.TickMs,
			ColdBudgetPermyriad: &budget,
			QueueLimit:          f.QueueLimit,
			WaitLimitSecMilli:   f.WaitLimitTicks * s.TickMs,
		})
		pool := V2Pool{FunctionID: f.ID}
		for i := int64(0); i < f.WarmPoolSize; i++ {
			pool.Instances = append(pool.Instances, WarmInstanceID(f.ID, int(i)))
		}
		out.WarmPools = append(out.WarmPools, pool)
	}
	for _, r := range s.Requests {
		out.Requests = append(out.Requests, V3Request{
			RequestID:      r.ID,
			FunctionID:     r.FunctionID,
			ArriveSecMilli: r.ArriveTick * s.TickMs,
			Slots:          r.Slots,
			Priority:       string(r.Priority),
			Tenant:          r.Tenant,
			HoldSecMilli:   r.HoldTicks * s.TickMs,
		})
	}
	return out
}
