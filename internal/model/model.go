package model

import (
	"fmt"
	"sort"
	"strings"
)

// Project is the lossless, schema-neutral representation of a credential
// project. All instants and durations are normalized to integer milliseconds.
type Project struct {
	ProjectID                   string         `json:"project_id"`
	ManifestVersion             string         `json:"manifest_version"`
	TickMillis                  int64          `json:"tick_ms"`
	HorizonTicks                int64          `json:"horizon_ticks"`
	LastRotationWatermarkMillis int64          `json:"last_rotation_watermark_ms"`
	Credentials                 []Credential   `json:"credentials"`
	Subscriptions               []Subscription `json:"subscriptions"`
	Rotations                   []Rotation     `json:"rotations"`
	Reload                      ReloadConfig   `json:"reload"`
}

type Credential struct {
	ID             string `json:"id"`
	Version        uint64 `json:"version"`
	IssuedAtMillis int64  `json:"issued_at_ms"`
	TTLMillis      int64  `json:"ttl_ms"`
	ReloadBatch    int64  `json:"reload_batch"`
	Supersedes     string `json:"supersedes,omitempty"`
}

func (c Credential) ExpiresAtMillis() int64 {
	return c.IssuedAtMillis + c.TTLMillis
}

func (c Credential) ValidFromMillis(watermarkMillis int64) int64 {
	if watermarkMillis > c.IssuedAtMillis {
		return watermarkMillis
	}
	return c.IssuedAtMillis
}

type Subscription struct {
	ID        string          `json:"id"`
	Active    bool            `json:"active"`
	DependsOn []string        `json:"depends_on"`
	Refs      []CredentialRef `json:"refs"`
}

type CredentialRef struct {
	CredentialID    string `json:"credential_id"`
	ExpectedVersion uint64 `json:"expected_version"`
	Role            string `json:"role,omitempty"`
}

type Rotation struct {
	CredentialID string `json:"credential_id"`
	FromVersion  uint64 `json:"from_version"`
	ToVersion    uint64 `json:"to_version"`
}

type ReloadConfig struct {
	ScheduleStartTick int64              `json:"schedule_start_tick"`
	MaxPerBatch       int                `json:"max_per_batch"`
	RetryLimit        int                `json:"retry_limit"`
	RetryStepMillis   int64              `json:"retry_step_ms"`
	FailurePlan       []FailureInjection `json:"failure_plan,omitempty"`
}

// FailureInjection makes the listed attempts fail. Attempt numbers are one
// based. A subscription that fails every allowed attempt becomes terminal.
type FailureInjection struct {
	SubscriptionID string `json:"subscription_id"`
	Attempts       []int  `json:"attempts"`
}

type Loaded struct {
	Project      *Project `json:"project"`
	SourceSchema string   `json:"source_schema"`
	RawSHA256    string   `json:"raw_sha256"`
	Notes        []string `json:"notes,omitempty"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Credentials = append([]Credential(nil), p.Credentials...)
	out.Subscriptions = make([]Subscription, len(p.Subscriptions))
	for i, sub := range p.Subscriptions {
		out.Subscriptions[i] = sub
		out.Subscriptions[i].DependsOn = append([]string(nil), sub.DependsOn...)
		out.Subscriptions[i].Refs = append([]CredentialRef(nil), sub.Refs...)
	}
	out.Rotations = append([]Rotation(nil), p.Rotations...)
	out.Reload.FailurePlan = make([]FailureInjection, len(p.Reload.FailurePlan))
	for i, fp := range p.Reload.FailurePlan {
		out.Reload.FailurePlan[i] = FailureInjection{
			SubscriptionID: fp.SubscriptionID,
			Attempts:       append([]int(nil), fp.Attempts...),
		}
	}
	return &out
}

func (p *Project) Sort() {
	sort.Slice(p.Credentials, func(i, j int) bool {
		return p.Credentials[i].ID < p.Credentials[j].ID
	})
	for i := range p.Subscriptions {
		sort.Strings(p.Subscriptions[i].DependsOn)
		sort.Slice(p.Subscriptions[i].Refs, func(a, b int) bool {
			return p.Subscriptions[i].Refs[a].CredentialID < p.Subscriptions[i].Refs[b].CredentialID
		})
	}
	sort.Slice(p.Subscriptions, func(i, j int) bool {
		return p.Subscriptions[i].ID < p.Subscriptions[j].ID
	})
	sort.Slice(p.Rotations, func(i, j int) bool {
		if p.Rotations[i].CredentialID != p.Rotations[j].CredentialID {
			return p.Rotations[i].CredentialID < p.Rotations[j].CredentialID
		}
		return p.Rotations[i].FromVersion < p.Rotations[j].FromVersion
	})
	sort.Slice(p.Reload.FailurePlan, func(i, j int) bool {
		return p.Reload.FailurePlan[i].SubscriptionID < p.Reload.FailurePlan[j].SubscriptionID
	})
	for i := range p.Reload.FailurePlan {
		sort.Ints(p.Reload.FailurePlan[i].Attempts)
	}
}

func (p *Project) CredentialByID() map[string]Credential {
	out := make(map[string]Credential, len(p.Credentials))
	for _, c := range p.Credentials {
		out[c.ID] = c
	}
	return out
}

func (p *Project) SubscriptionByID() map[string]Subscription {
	out := make(map[string]Subscription, len(p.Subscriptions))
	for _, s := range p.Subscriptions {
		out[s.ID] = s
	}
	return out
}

func (p *Project) ActiveSubscriptions() []Subscription {
	out := make([]Subscription, 0, len(p.Subscriptions))
	for _, s := range p.Subscriptions {
		if s.Active {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Project) TickToMillis(tick int64) int64 {
	return tick * p.TickMillis
}

func (p *Project) MillisToTickCeil(millis int64) int64 {
	if millis <= 0 {
		return 0
	}
	return (millis + p.TickMillis - 1) / p.TickMillis
}

func (p *Project) TickCount() int64 {
	if p.HorizonTicks < 0 {
		return 0
	}
	return p.HorizonTicks + 1
}

func (p *Project) SemanticDigestFields() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%d|%d|%d|%d|%d|%d", p.ProjectID, p.ManifestVersion,
		p.TickMillis, p.HorizonTicks, p.LastRotationWatermarkMillis,
		p.Reload.ScheduleStartTick, p.Reload.MaxPerBatch, p.Reload.RetryLimit)
	for _, c := range p.Credentials {
		fmt.Fprintf(&b, "|C:%s:%d:%d:%d:%d:%s", c.ID, c.Version, c.IssuedAtMillis, c.TTLMillis, c.ReloadBatch, c.Supersedes)
	}
	for _, s := range p.Subscriptions {
		fmt.Fprintf(&b, "|S:%s:%t", s.ID, s.Active)
		for _, d := range s.DependsOn {
			fmt.Fprintf(&b, ":%s", d)
		}
		for _, r := range s.Refs {
			fmt.Fprintf(&b, ":%s@%d:%s", r.CredentialID, r.ExpectedVersion, r.Role)
		}
	}
	for _, r := range p.Rotations {
		fmt.Fprintf(&b, "|R:%s:%d:%d", r.CredentialID, r.FromVersion, r.ToVersion)
	}
	return b.String()
}
