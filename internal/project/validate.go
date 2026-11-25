package project

import (
	"fmt"
	"math"
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
)

func Validate(p *model.Project) error {
	if p == nil {
		return bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if p.ProjectID == "" {
		return bizerr.New(bizerr.CodeSchemaInvalid, "project_id is required", nil)
	}
	if p.ManifestVersion == "" {
		return bizerr.New(bizerr.CodeSchemaInvalid, "manifest_version is required", nil)
	}
	if p.TickMillis <= 0 {
		return bizerr.New(bizerr.CodeSchemaInvalid, "tick_ms must be positive", map[string]any{"tick_ms": p.TickMillis})
	}
	if p.HorizonTicks < 0 {
		return bizerr.New(bizerr.CodeSchemaInvalid, "horizon must be non-negative", map[string]any{"horizon_ticks": p.HorizonTicks})
	}
	if p.LastRotationWatermarkMillis < 0 {
		return bizerr.New(bizerr.CodeSchemaInvalid, "last rotation watermark must be non-negative", map[string]any{
			"watermark_ms": p.LastRotationWatermarkMillis,
		})
	}
	if p.Reload.MaxPerBatch <= 0 {
		return bizerr.New(bizerr.CodeBatchLimit, "max subscriptions per reload batch must be positive", map[string]any{
			"max_per_batch": p.Reload.MaxPerBatch,
			"constraint":    "REL-CAP-001",
		})
	}
	if p.Reload.RetryLimit <= 0 {
		return bizerr.New(bizerr.CodeSchemaInvalid, "retry_limit must be positive", map[string]any{"retry_limit": p.Reload.RetryLimit})
	}
	if p.Reload.RetryStepMillis <= 0 {
		return bizerr.New(bizerr.CodeSchemaInvalid, "retry step must be positive", map[string]any{
			"retry_step_ms": p.Reload.RetryStepMillis,
		})
	}

	credIDs := make(map[string]model.Credential, len(p.Credentials))
	for i, c := range p.Credentials {
		if c.ID == "" {
			return bizerr.New(bizerr.CodeSchemaInvalid, "credential id is required", map[string]any{"credential_index": i})
		}
		if _, ok := credIDs[c.ID]; ok {
			return bizerr.New(bizerr.CodeDuplicateCredential, "credential id is duplicated", map[string]any{
				"credential_id": c.ID,
				"index":         i,
			})
		}
		if c.Version == 0 {
			return bizerr.New(bizerr.CodeInvalidRotation, "credential version must be positive", map[string]any{
				"credential_id": c.ID,
			})
		}
		if c.IssuedAtMillis < 0 {
			return bizerr.New(bizerr.CodeSchemaInvalid, "credential issuance must be non-negative", map[string]any{
				"credential_id": c.ID,
				"issued_at_ms":  c.IssuedAtMillis,
			})
		}
		if c.TTLMillis < 0 {
			return bizerr.New(bizerr.CodeNegativeTTL, "credential ttl must not be negative", map[string]any{
				"credential_id": c.ID,
				"ttl_ms":        c.TTLMillis,
			})
		}
		if c.TTLMillis == 0 {
			return bizerr.New(bizerr.CodeTTLInvalid, "credential ttl must be positive", map[string]any{
				"credential_id": c.ID,
				"ttl_ms":        c.TTLMillis,
			})
		}
		if c.ReloadBatch < 0 {
			return bizerr.New(bizerr.CodeSchemaInvalid, "reload batch must be non-negative", map[string]any{
				"credential_id": c.ID,
				"reload_batch":  c.ReloadBatch,
			})
		}
		if c.IssuedAtMillis > math.MaxInt64-c.TTLMillis {
			return bizerr.New(bizerr.CodeNumericOverflow, "credential expiry overflows int64", map[string]any{
				"credential_id": c.ID,
			})
		}
		credIDs[c.ID] = c
	}

	subIDs := make(map[string]model.Subscription, len(p.Subscriptions))
	for i, s := range p.Subscriptions {
		if s.ID == "" {
			return bizerr.New(bizerr.CodeSchemaInvalid, "subscription id is required", map[string]any{"subscription_index": i})
		}
		if _, ok := subIDs[s.ID]; ok {
			return bizerr.New(bizerr.CodeDuplicateSub, "subscription id is duplicated", map[string]any{
				"subscription_id": s.ID,
				"index":           i,
			})
		}
		subIDs[s.ID] = s
		seenRefs := make(map[string]int, len(s.Refs))
		for j, ref := range s.Refs {
			if ref.CredentialID == "" {
				return bizerr.New(bizerr.CodeSchemaInvalid, "credential reference id is required", map[string]any{
					"subscription_id": s.ID,
					"ref_index":       j,
				})
			}
			if previous, ok := seenRefs[ref.CredentialID]; ok {
				return bizerr.New(bizerr.CodeDuplicateRef, "subscription contains a duplicate credential reference", map[string]any{
					"subscription_id": s.ID,
					"credential_id":   ref.CredentialID,
					"first_index":     previous,
					"duplicate_index": j,
				})
			}
			seenRefs[ref.CredentialID] = j
			c, ok := credIDs[ref.CredentialID]
			if !ok {
				return bizerr.New(bizerr.CodeMissingCredential, "subscription references an unknown credential", map[string]any{
					"subscription_id": s.ID,
					"credential_id":   ref.CredentialID,
					"ref_index":       j,
				})
			}
			if ref.ExpectedVersion != 0 && ref.ExpectedVersion != c.Version {
				return bizerr.New(bizerr.CodeInvalidRotation, "credential reference version does not match credential", map[string]any{
					"subscription_id": s.ID,
					"credential_id":   ref.CredentialID,
					"expected":        ref.ExpectedVersion,
					"actual":          c.Version,
				})
			}
		}
	}

	for _, s := range p.Subscriptions {
		seenDeps := make(map[string]int, len(s.DependsOn))
		for i, dep := range s.DependsOn {
			if _, ok := subIDs[dep]; !ok {
				return bizerr.New(bizerr.CodeMissingSubscription, "subscription depends on an unknown subscription", map[string]any{
					"subscription_id": s.ID,
					"dependency_id":   dep,
					"index":           i,
				})
			}
			if previous, ok := seenDeps[dep]; ok {
				return bizerr.New(bizerr.CodeSchemaInvalid, "subscription has a duplicate dependency", map[string]any{
					"subscription_id": s.ID,
					"dependency_id":   dep,
					"first_index":     previous,
					"duplicate_index": i,
				})
			}
			seenDeps[dep] = i
		}
	}

	if err := detectCycles(p); err != nil {
		return err
	}

	seenRotations := make(map[string]bool, len(p.Rotations))
	for i, r := range p.Rotations {
		c, ok := credIDs[r.CredentialID]
		if !ok {
			return bizerr.New(bizerr.CodeMissingCredential, "rotation target is unknown", map[string]any{
				"credential_id":  r.CredentialID,
				"rotation_index": i,
			})
		}
		if r.FromVersion == 0 || r.ToVersion == 0 || r.FromVersion >= r.ToVersion {
			return bizerr.New(bizerr.CodeInvalidRotation, "rotation versions must be positive and increasing", map[string]any{
				"credential_id": r.CredentialID,
				"from_version":  r.FromVersion,
				"to_version":    r.ToVersion,
			})
		}
		if c.Version != r.ToVersion {
			return bizerr.New(bizerr.CodeInvalidRotation, "rotation target version does not match credential", map[string]any{
				"credential_id": r.CredentialID,
				"to_version":    r.ToVersion,
				"credential":    c.Version,
			})
		}
		if seenRotations[r.CredentialID] {
			return bizerr.New(bizerr.CodeInvalidRotation, "credential has more than one rotation record", map[string]any{
				"credential_id": r.CredentialID,
			})
		}
		seenRotations[r.CredentialID] = true
	}

	for i, fp := range p.Reload.FailurePlan {
		if _, ok := subIDs[fp.SubscriptionID]; !ok {
			return bizerr.New(bizerr.CodeMissingSubscription, "failure plan references an unknown subscription", map[string]any{
				"subscription_id": fp.SubscriptionID,
				"plan_index":      i,
			})
		}
		if len(fp.Attempts) == 0 {
			return bizerr.New(bizerr.CodeSchemaInvalid, "failure plan attempt list must not be empty", map[string]any{
				"subscription_id": fp.SubscriptionID,
			})
		}
		for _, attempt := range fp.Attempts {
			if attempt <= 0 {
				return bizerr.New(bizerr.CodeSchemaInvalid, "failure plan attempts are one-based", map[string]any{
					"subscription_id": fp.SubscriptionID,
					"attempt":         attempt,
				})
			}
		}
	}

	return nil
}

func detectCycles(p *model.Project) error {
	adj := make(map[string][]string, len(p.Subscriptions))
	for _, s := range p.Subscriptions {
		adj[s.ID] = append([]string(nil), s.DependsOn...)
		sort.Strings(adj[s.ID])
	}
	state := make(map[string]uint8, len(p.Subscriptions))
	stack := make([]string, 0, len(p.Subscriptions))
	position := make(map[string]int, len(p.Subscriptions))

	var visit func(string) error
	visit = func(id string) error {
		state[id] = 1
		position[id] = len(stack)
		stack = append(stack, id)
		for _, next := range adj[id] {
			switch state[next] {
			case 0:
				if err := visit(next); err != nil {
					return err
				}
			case 1:
				start := position[next]
				cycle := append([]string(nil), stack[start:]...)
				cycle = append(cycle, next)
				return bizerr.New(bizerr.CodeSubCycle, "subscription topology contains a cycle", map[string]any{
					"cycle":       cycle,
					"cycle_text":  cycleText(cycle),
					"start_node":  next,
					"path_length": len(cycle) - 1,
				})
			}
		}
		stack = stack[:len(stack)-1]
		delete(position, id)
		state[id] = 2
		return nil
	}

	ids := make([]string, 0, len(adj))
	for id := range adj {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == 0 {
			if err := visit(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func cycleText(cycle []string) string {
	out := ""
	for i, id := range cycle {
		if i > 0 {
			out += " -> "
		}
		out += id
	}
	return out
}

func Describe(p *model.Project) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s credentials=%d subscriptions=%d rotations=%d",
		p.ProjectID, p.ManifestVersion, len(p.Credentials), len(p.Subscriptions), len(p.Rotations))
}
