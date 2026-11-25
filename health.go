package hckernel

func (e *Engine) enterHalfOpen(rt *InstanceState, probe Probe, nowMs int64) {
	from := rt.Health
	if from == StateHalfOpen {
		return
	}
	rt.Health = StateHalfOpen
	e.recordObservation(HealthObservation{
		EventID:          probe.ID,
		AtMs:             nowMs,
		InstanceID:       probe.InstanceID,
		ProbeID:          probe.ID,
		Trigger:          "half_open_dispatch",
		FromState:        from,
		ToState:          StateHalfOpen,
		StateChanged:     from != StateHalfOpen,
		SuccessStreak:    rt.SuccessStreak,
		FailureStreak:    rt.FailureStreak,
		SuccessThreshold: rt.Instance.SuccessThreshold,
		FailureThreshold: rt.Instance.FailureThreshold,
		Reason:           "half_open_probe_dispatched",
	})
}

func (e *Engine) applyOutcome(rt *InstanceState, result ProbeResultKind, eventID string, atMs int64, probeID string) {
	inst := rt.Instance
	policy := e.project.CountPolicy
	from := rt.Health
	to := from
	reason := ""

	if result == ResultSuccess {
		rt.SuccessStreak++
		if policy.ResetOppositeStreak {
			rt.FailureStreak = 0
		}
		limit := inst.SuccessThreshold
		if from == StateHalfOpen {
			limit = policy.HalfOpenSuccessLimit
			reason = "half_open_success"
		} else {
			reason = "success_threshold_progress"
		}
		if rt.SuccessStreak >= limit {
			to = StateHealthy
			reason = reason + "_reached"
			if policy.SuccessReachedAction == ThresholdKeep {
				rt.SuccessStreak = limit
			} else {
				rt.SuccessStreak = 0
			}
			rt.HalfOpenAt = 0
			rt.RetryAttempts = 0
			rt.BackoffUntil = 0
		}
	} else {
		rt.FailureStreak++
		if policy.ResetOppositeStreak {
			rt.SuccessStreak = 0
		}
		limit := inst.FailureThreshold
		if from == StateHalfOpen {
			limit = policy.HalfOpenFailureLimit
			reason = "half_open_failure"
		} else {
			reason = "failure_threshold_progress"
		}

		rt.RetryAttempts++
		rt.BackoffUntil = atMs + inst.BackoffMs
		if rt.RetryAttempts <= inst.RetryLimit && inst.BackoffMs > 0 {
			rt.NextPeriodAt = atMs + inst.BackoffMs
		}

		if rt.FailureStreak >= limit {
			to = StateUnhealthy
			reason = reason + "_reached"
			if policy.FailureReachedAction == ThresholdKeep {
				rt.FailureStreak = limit
			} else {
				rt.FailureStreak = 0
			}
			rt.HalfOpenAt = atMs + policy.HalfOpenDelayMs
		}
	}

	rt.Health = to
	e.recordObservation(HealthObservation{
		EventID:          eventID,
		AtMs:             atMs,
		InstanceID:       inst.ID,
		ProbeID:          probeID,
		Result:           result,
		Trigger:          "probe_result",
		FromState:        from,
		ToState:          to,
		StateChanged:     from != to,
		SuccessStreak:    rt.SuccessStreak,
		FailureStreak:    rt.FailureStreak,
		SuccessThreshold: inst.SuccessThreshold,
		FailureThreshold: inst.FailureThreshold,
		Reason:           reason,
	})
}
