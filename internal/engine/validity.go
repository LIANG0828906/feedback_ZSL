package engine

import (
	"fmt"
	"sort"

	"credrot/internal/bizerr"
	"credrot/internal/model"
	"credrot/internal/project"
)

type AvailableCredential struct {
	CredentialID    string `json:"credential_id"`
	Version         uint64 `json:"version"`
	ValidFromMillis int64  `json:"valid_from_ms"`
	ExpiresAtMillis int64  `json:"expires_at_ms"`
	RemainingMillis int64  `json:"remaining_ms"`
	RemainingTicks  int64  `json:"remaining_ticks"`
	Source          string `json:"source"`
}

type CredentialState struct {
	CredentialID string `json:"credential_id"`
	Version      uint64 `json:"version"`
	Status       string `json:"status"`
	Source       string `json:"source"`
}

type SubscriptionValidity struct {
	Tick           int64                 `json:"tick"`
	NowMillis      int64                 `json:"now_ms"`
	SubscriptionID string                `json:"subscription_id"`
	Active         bool                  `json:"active"`
	Available      []AvailableCredential `json:"available"`
	States         []CredentialState     `json:"states"`
}

type Solver struct {
	project *model.Project
	creds   map[string]model.Credential
}

func NewSolver(p *model.Project) (*Solver, error) {
	if p == nil {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "project is nil", nil)
	}
	if err := project.Validate(p); err != nil {
		return nil, err
	}
	return &Solver{project: p.Clone(), creds: p.CredentialByID()}, nil
}

func (s *Solver) Project() *model.Project {
	return s.project
}

func (s *Solver) ValidityAll() ([]SubscriptionValidity, error) {
	out := make([]SubscriptionValidity, 0, s.project.TickCount()*int64(len(s.project.Subscriptions)))
	for tick := int64(0); tick <= s.project.HorizonTicks; tick++ {
		values, err := s.ValidityAt(tick)
		if err != nil {
			return nil, err
		}
		out = append(out, values...)
	}
	return out, nil
}

func (s *Solver) ValidityAt(tick int64) ([]SubscriptionValidity, error) {
	if tick < 0 || tick > s.project.HorizonTicks {
		return nil, bizerr.New(bizerr.CodeSchemaInvalid, "tick is outside the declared horizon", map[string]any{
			"tick":          tick,
			"horizon_ticks": s.project.HorizonTicks,
		})
	}
	out := make([]SubscriptionValidity, 0, len(s.project.Subscriptions))
	for _, sub := range s.project.Subscriptions {
		value, err := s.ValidityForSubscription(tick, sub.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubscriptionID < out[j].SubscriptionID })
	return out, nil
}

func (s *Solver) ValidityForSubscription(tick int64, subscriptionID string) (SubscriptionValidity, error) {
	sub, ok := s.project.SubscriptionByID()[subscriptionID]
	if !ok {
		return SubscriptionValidity{}, bizerr.New(bizerr.CodeMissingSubscription, "subscription is unknown", map[string]any{
			"subscription_id": subscriptionID,
			"tick":            tick,
		})
	}
	return s.validityForSubscriptionObject(tick, sub)
}

func (s *Solver) validityForSubscriptionObject(tick int64, sub model.Subscription) (SubscriptionValidity, error) {
	now := s.project.TickToMillis(tick)
	out := SubscriptionValidity{
		Tick:           tick,
		NowMillis:      now,
		SubscriptionID: sub.ID,
		Active:         sub.Active,
		Available:      []AvailableCredential{},
		States:         make([]CredentialState, 0, len(sub.Refs)),
	}
	for _, ref := range sub.Refs {
		c, ok := s.creds[ref.CredentialID]
		if !ok {
			return SubscriptionValidity{}, bizerr.New(bizerr.CodeMissingCredential, "referenced credential is missing", map[string]any{
				"subscription_id": sub.ID,
				"credential_id":   ref.CredentialID,
				"tick":            tick,
			})
		}
		from := c.ValidFromMillis(s.project.LastRotationWatermarkMillis)
		expires := c.ExpiresAtMillis()
		state := CredentialState{CredentialID: c.ID, Version: c.Version}
		switch {
		case now >= expires:
			state.Status = "expired"
			state.Source = fmt.Sprintf("expiry:%d", expires)
		case now < from:
			state.Status = "not_yet_valid"
			state.Source = fmt.Sprintf("watermark/issued:max=%d", from)
		default:
			state.Status = "valid"
			state.Source = fmt.Sprintf("issued:%d;ttl:%d;watermark:%d", c.IssuedAtMillis, c.TTLMillis, s.project.LastRotationWatermarkMillis)
			remaining := expires - now
			out.Available = append(out.Available, AvailableCredential{
				CredentialID:    c.ID,
				Version:         c.Version,
				ValidFromMillis: from,
				ExpiresAtMillis: expires,
				RemainingMillis: remaining,
				RemainingTicks:  remainingTicks(remaining, s.project.TickMillis),
				Source:          "credential:" + c.ID,
			})
		}
		out.States = append(out.States, state)
	}
	sort.Slice(out.Available, func(i, j int) bool {
		if out.Available[i].Version != out.Available[j].Version {
			return out.Available[i].Version > out.Available[j].Version
		}
		return out.Available[i].CredentialID < out.Available[j].CredentialID
	})
	sort.Slice(out.States, func(i, j int) bool {
		return out.States[i].CredentialID < out.States[j].CredentialID
	})
	return out, nil
}

func (s *Solver) CredentialValidAt(c model.Credential, tick int64) (bool, int64) {
	now := s.project.TickToMillis(tick)
	from := c.ValidFromMillis(s.project.LastRotationWatermarkMillis)
	expires := c.ExpiresAtMillis()
	if now < from || now >= expires {
		return false, 0
	}
	return true, expires - now
}

func remainingTicks(millis, tickMillis int64) int64 {
	if millis <= 0 {
		return 0
	}
	return (millis + tickMillis - 1) / tickMillis
}
