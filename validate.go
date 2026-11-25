package hckernel

const MaxThreshold = 100

// ValidateProject checks the canonical model after all schema-specific unit
// and semantic conversions have happened. Source documents are never mutated.
func ValidateProject(p *Project) error {
	if p == nil {
		return newError(ErrSchemaPayloadInvalid, "project is nil")
	}
	if p.ProjectID == "" {
		return newError(ErrSchemaPayloadInvalid, "project_id is required", withPath("project_id"))
	}
	if p.StepMs <= 0 {
		return newError(ErrIntervalInvalid, "logical round step must be positive", withPath("step_ms"))
	}
	if p.Rounds < 0 {
		return newError(ErrSchemaPayloadInvalid, "rounds cannot be negative", withPath("rounds"))
	}
	if p.MaxDeferMs < 0 {
		return newError(ErrNegativeDuration, "max_defer_ms cannot be negative", withPath("max_defer_ms"))
	}
	if p.GlobalConcurrency < 0 {
		return newError(ErrBudgetZero, "global concurrency cannot be negative", withPath("global_concurrency"))
	}
	if p.Workers < 0 {
		return newError(ErrWorkerBudgetInvalid, "workers cannot be negative", withPath("workers"))
	}
	if p.BufferCapacity < 0 {
		return newError(ErrBackpressureBufferFull, "buffer capacity cannot be negative", withPath("buffer_capacity"))
	}
	if p.SlotPriority == "" {
		p.SlotPriority = PriorityDeferredFirst
	}
	if p.SlotPriority != PriorityDeferredFirst && p.SlotPriority != PriorityPeriodicFirst {
		return newError(ErrSchemaPayloadInvalid, "unsupported slot priority", withPath("slot_priority"))
	}
	if p.CountPolicy.SuccessReachedAction == "" {
		p.CountPolicy.SuccessReachedAction = ThresholdReset
	}
	if p.CountPolicy.FailureReachedAction == "" {
		p.CountPolicy.FailureReachedAction = ThresholdReset
	}
	if p.CountPolicy.HalfOpenSuccessLimit <= 0 {
		p.CountPolicy.HalfOpenSuccessLimit = 1
	}
	if p.CountPolicy.HalfOpenFailureLimit <= 0 {
		p.CountPolicy.HalfOpenFailureLimit = 1
	}

	racks := p.rackMap()
	for i, rack := range p.Racks {
		if rack.ID == "" {
			return newError(ErrSchemaPayloadInvalid, "rack id is required", withPath("racks["+itoa(i)+"].id"))
		}
		if rack.Concurrency < 0 {
			return newError(ErrBudgetZero, "rack concurrency cannot be negative", withRack(rack.ID), withPath("racks.concurrency"))
		}
	}

	seen := map[string]struct{}{}
	for i := range p.Instances {
		inst := &p.Instances[i]
		if inst.ID == "" {
			return newError(ErrSchemaPayloadInvalid, "instance id is required", withPath("instances["+itoa(i)+"].id"))
		}
		if _, ok := seen[inst.ID]; ok {
			return newError(ErrInstanceDuplicate, "duplicate instance id", withInstance(inst.ID))
		}
		seen[inst.ID] = struct{}{}

		if inst.IntervalMs == 0 {
			return newError(ErrIntervalZero, "probe interval must not be zero", withInstance(inst.ID), withPath("interval_ms"))
		}
		if inst.IntervalMs < 0 {
			return newError(ErrIntervalInvalid, "probe interval must be positive", withInstance(inst.ID), withPath("interval_ms"))
		}
		if inst.TimeoutMs < 0 {
			return newError(ErrNegativeTimeout, "timeout must not be negative", withInstance(inst.ID), withPath("timeout_ms"))
		}
		if inst.BackoffMs < 0 {
			return newError(ErrNegativeDuration, "backoff must not be negative", withInstance(inst.ID), withPath("backoff_ms"))
		}
		if inst.SuccessThreshold <= 0 || inst.SuccessThreshold > MaxThreshold {
			return newError(ErrThresholdOutOfRange, "success threshold out of range [1,100]", withInstance(inst.ID), withPath("success_threshold"))
		}
		if inst.FailureThreshold <= 0 || inst.FailureThreshold > MaxThreshold {
			return newError(ErrThresholdOutOfRange, "failure threshold out of range [1,100]", withInstance(inst.ID), withPath("failure_threshold"))
		}
		if inst.WeightPPM <= 0 {
			return newError(ErrWeightInvalid, "weight must be positive", withInstance(inst.ID), withPath("weight_ppm"))
		}
		if inst.RackID == "" {
			return newError(ErrRackReferenceMissing, "instance has no rack reference", withInstance(inst.ID), withPath("rack_id"))
		}
		if _, ok := racks[inst.RackID]; !ok {
			return newError(ErrRackReferenceMissing, "instance references undeclared rack", withInstance(inst.ID), withRack(inst.RackID), withPath("rack_id"))
		}
		switch inst.InitialState {
		case "", StateInitial, StateHealthy, StateUnhealthy, StateHalfOpen:
			if inst.InitialState == "" {
				inst.InitialState = StateInitial
			}
		default:
			return newError(ErrSchemaPayloadInvalid, "unsupported initial health state", withInstance(inst.ID), withPath("initial_state"))
		}
	}

	if p.Workers == 0 {
		p.Workers = 1
	}
	if p.BufferCapacity == 0 {
		p.BufferCapacity = maxInt(1, p.GlobalConcurrency)
	}
	return nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
