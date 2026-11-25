package engine

import (
	"fmt"
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
	"credrot/internal/project"
)

type GateConditions struct {
	SubscriptionID     string
	AtTick             int64
	Requested          *model.Credential
	Committed          *model.Credential
	RequestedBatch     int64
	LastCommittedBatch int64
	CheckBatchGap      bool
	WatermarkKnown     bool
	RequireWatermark   bool
	Arrived            bool
	Confirmed          bool
	RequestedValid     bool
	CommittedValid     bool
	CommittedRemaining int64
}

type GateDecision struct {
	SubscriptionID      string `json:"subscription_id"`
	Tick                int64  `json:"tick"`
	Decision            string `json:"decision"`
	CredentialID        string `json:"credential_id,omitempty"`
	CredentialVersion   uint64 `json:"credential_version,omitempty"`
	RemainingMillis     int64  `json:"remaining_ms,omitempty"`
	Source              string `json:"source"`
	Code                string `json:"code,omitempty"`
	Reason              string `json:"reason"`
	RequestedCredential string `json:"requested_credential,omitempty"`
	CommittedCredential string `json:"committed_credential,omitempty"`
}

// EvaluateGate applies one deterministic precedence order. It is deliberately
// independent of storage and scheduling so the combined-anomaly behavior can
// be tested directly.
func EvaluateGate(cond GateConditions) GateDecision {
	out := GateDecision{
		SubscriptionID: cond.SubscriptionID,
		Tick:           cond.AtTick,
	}
	if cond.Requested != nil {
		out.RequestedCredential = cond.Requested.ID
	}
	if cond.Committed != nil {
		out.CommittedCredential = cond.Committed.ID
	}
	if cond.Requested == nil {
		out.Decision = "reject"
		out.Code = bizerr.CodeMissingCredential
		out.Source = "state:missing_requested_credential"
		out.Reason = "requested credential is absent"
		return out
	}
	if cond.Committed != nil && cond.Requested.Version < cond.Committed.Version {
		out.Decision = "reject"
		out.Code = bizerr.CodeVersionRegression
		out.Source = "committed:" + cond.Committed.ID
		out.Reason = fmt.Sprintf("credential version regressed from %d to %d", cond.Committed.Version, cond.Requested.Version)
		return out
	}
	if cond.CheckBatchGap && cond.RequestedBatch > cond.LastCommittedBatch+1 {
		out.Decision = "reject"
		out.Code = bizerr.CodeBatchGap
		out.Source = fmt.Sprintf("batch:committed=%d;requested=%d", cond.LastCommittedBatch, cond.RequestedBatch)
		out.Reason = "reload batch sequence skipped one or more committed batches"
		return out
	}
	if cond.RequireWatermark && !cond.WatermarkKnown {
		out.Decision = "reject"
		out.Code = bizerr.CodeWatermarkMissing
		out.Source = "watermark:unavailable"
		out.Reason = "last rotation watermark is required but unavailable"
		return out
	}
	if cond.Confirmed && cond.Arrived && cond.RequestedValid {
		out.Decision = "allow_new"
		out.CredentialID = cond.Requested.ID
		out.CredentialVersion = cond.Requested.Version
		out.Source = "suspended:" + cond.Requested.ID + ";state=arrived_confirmed"
		out.Reason = "new credential reload is confirmed and credential is valid"
		return out
	}

	reason := "new credential reload has not arrived"
	if cond.Arrived {
		reason = "new credential reload has not been confirmed"
	}
	if cond.Arrived && cond.Confirmed && !cond.RequestedValid {
		reason = "new credential reload is confirmed but the credential is outside its validity interval"
	}
	if cond.Committed != nil && cond.CommittedValid {
		out.Decision = "fallback_old"
		out.CredentialID = cond.Committed.ID
		out.CredentialVersion = cond.Committed.Version
		out.RemainingMillis = cond.CommittedRemaining
		out.Source = "committed:" + cond.Committed.ID + ";state=last_valid"
		out.Reason = reason + "; committed credential remains valid"
		return out
	}
	out.Decision = "reject"
	out.Code = bizerr.CodeGateNoFallback
	out.Source = "committed:none_valid"
	out.Reason = reason + "; no committed credential remains valid"
	return out
}

type GateRuntime struct {
	Confirmed          map[string]bool
	Arrived            map[string]bool
	Committed          map[string]string
	RequestedBatch     map[string]int64
	LastCommittedBatch map[string]int64
	CheckBatchGap      bool
	WatermarkKnown     bool
	RequireWatermark   bool
}

type GateResult struct {
	AtTick    int64          `json:"at_tick"`
	Decisions []GateDecision `json:"decisions"`
}

func GateAll(p *model.Project, atTick int64, runtime GateRuntime) (*GateResult, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if err := project.Validate(p); err != nil {
		return nil, err
	}
	if atTick < 0 || atTick > p.HorizonTicks {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "gate tick is outside the declared horizon", map[string]any{
			"tick":          atTick,
			"horizon_ticks": p.HorizonTicks,
		})
	}
	solver, err := NewSolver(p)
	if err != nil {
		return nil, err
	}
	creds := p.CredentialByID()
	rotationTargets := make(map[string]model.Credential)
	for _, rotation := range p.Rotations {
		if c, ok := creds[rotation.CredentialID]; ok {
			rotationTargets[c.ID] = c
		}
	}
	result := &GateResult{AtTick: atTick, Decisions: make([]GateDecision, 0, len(p.Subscriptions))}
	for _, sub := range p.Subscriptions {
		var target *model.Credential
		for _, ref := range sub.Refs {
			c, ok := rotationTargets[ref.CredentialID]
			if !ok {
				continue
			}
			copy := c
			if target == nil || copy.Version > target.Version {
				target = &copy
			}
		}
		if target == nil {
			result.Decisions = append(result.Decisions, GateDecision{
				SubscriptionID: sub.ID,
				Tick:           atTick,
				Decision:       "not_applicable",
				Source:         "no_suspended_rotation_reference",
				Reason:         "subscription has no rotation target reference",
			})
			continue
		}

		committedID := runtime.Committed[sub.ID]
		var committed *model.Credential
		if committedID != "" {
			if c, ok := creds[committedID]; ok {
				copy := c
				committed = &copy
			}
		} else {
			var best *model.Credential
			for _, ref := range sub.Refs {
				c, ok := creds[ref.CredentialID]
				if !ok || c.Version >= target.Version {
					continue
				}
				if valid, _ := solver.CredentialValidAt(c, atTick); !valid {
					continue
				}
				if best == nil || c.Version > best.Version || (c.Version == best.Version && c.ID < best.ID) {
					copy := c
					best = &copy
				}
			}
			committed = best
		}

		requestedValid, requestRemaining := solver.CredentialValidAt(*target, atTick)
		committedValid := false
		var committedRemaining int64
		if committed != nil {
			committedValid, committedRemaining = solver.CredentialValidAt(*committed, atTick)
		}
		requestedBatch := target.ReloadBatch
		if value, ok := runtime.RequestedBatch[sub.ID]; ok {
			requestedBatch = value
		}
		lastCommittedBatch := requestedBatch - 1
		if value, ok := runtime.LastCommittedBatch[sub.ID]; ok {
			lastCommittedBatch = value
		}
		watermarkKnown := true
		if runtime.RequireWatermark || !runtime.WatermarkKnown {
			watermarkKnown = runtime.WatermarkKnown
		}
		decision := EvaluateGate(GateConditions{
			SubscriptionID:     sub.ID,
			AtTick:             atTick,
			Requested:          target,
			Committed:          committed,
			RequestedBatch:     requestedBatch,
			LastCommittedBatch: lastCommittedBatch,
			CheckBatchGap:      runtime.CheckBatchGap,
			WatermarkKnown:     watermarkKnown,
			RequireWatermark:   runtime.RequireWatermark,
			Arrived:            runtime.Arrived[sub.ID],
			Confirmed:          runtime.Confirmed[sub.ID],
			RequestedValid:     requestedValid,
			CommittedValid:     committedValid,
			CommittedRemaining: committedRemaining,
		})
		if decision.Decision == "allow_new" {
			decision.RemainingMillis = requestRemaining
		}
		result.Decisions = append(result.Decisions, decision)
	}
	sort.Slice(result.Decisions, func(i, j int) bool {
		return result.Decisions[i].SubscriptionID < result.Decisions[j].SubscriptionID
	})
	return result, nil
}

func (r GateRuntime) Normalize() GateRuntime {
	if r.Confirmed == nil {
		r.Confirmed = map[string]bool{}
	}
	if r.Arrived == nil {
		r.Arrived = map[string]bool{}
	}
	if r.Committed == nil {
		r.Committed = map[string]string{}
	}
	if r.RequestedBatch == nil {
		r.RequestedBatch = map[string]int64{}
	}
	if r.LastCommittedBatch == nil {
		r.LastCommittedBatch = map[string]int64{}
	}
	return r
}
