package generator

import (
	"encoding/json"
	"fmt"
	"sort"

	"credrot/internal/model"
)

type Bundle struct {
	Seed     int64             `json:"seed"`
	Manifest string            `json:"manifest_version"`
	Project  *model.Project    `json:"canonical_project"`
	Files    map[string][]byte `json:"-"`
	Oracle   *Oracle           `json:"oracle"`
}

type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func Generate(seed int64) (*Bundle, error) {
	r := newRNG(seed)
	suffix := fmt.Sprintf("%08x", uint32(seed))[0:8]
	p := &model.Project{
		ProjectID:                   "cred-project-" + suffix,
		ManifestVersion:             fmt.Sprintf("manifest-%d-%s", seed, suffix),
		TickMillis:                  1000,
		LastRotationWatermarkMillis: 1000,
		Reload: model.ReloadConfig{
			ScheduleStartTick: 0,
			MaxPerBatch:       1,
			RetryLimit:        2 + r.intn(2),
			RetryStepMillis:   1000,
		},
	}

	type topology struct {
		id      string
		parents []string
	}
	topologyRows := []topology{
		{id: "sub-api"},
		{id: "sub-billing", parents: []string{"sub-api"}},
		{id: "sub-search", parents: []string{"sub-api"}},
		{id: "sub-reports", parents: []string{"sub-billing"}},
		{id: "sub-archive", parents: []string{"sub-search"}},
		{id: "sub-audit"},
	}
	rotated := make([]bool, len(topologyRows))
	for i := range rotated {
		if i < 4 {
			rotated[i] = true
		}
	}
	if seed%2 == 0 {
		rotated[5] = true
	}

	var newCredentials []model.Credential
	for index, topo := range topologyRows {
		v1TTL := int64(5+r.intn(8)) * 1000
		if topo.id == "sub-reports" {
			// The reference gate scenario needs one unconfirmed rotation whose
			// previous credential is already expired at tick 6.
			v1TTL = 5000
		}
		v1 := model.Credential{
			ID:             "cred-" + topo.id + "-v1",
			Version:        1,
			IssuedAtMillis: 1000,
			TTLMillis:      v1TTL,
			ReloadBatch:    0,
		}
		sub := model.Subscription{
			ID:        topo.id,
			Active:    true,
			DependsOn: append([]string(nil), topo.parents...),
			Refs: []model.CredentialRef{{
				CredentialID:    v1.ID,
				ExpectedVersion: 1,
				Role:            "primary",
			}},
		}
		p.Credentials = append(p.Credentials, v1)
		if rotated[index] {
			batch := int64(1)
			if topo.id == "sub-audit" {
				batch = 2
			}
			v2TTL := int64(13+r.intn(9)) * 1000
			v2 := model.Credential{
				ID:             "cred-" + topo.id + "-v2",
				Version:        2,
				IssuedAtMillis: int64(3+r.intn(2)) * 1000,
				TTLMillis:      v2TTL,
				ReloadBatch:    batch,
				Supersedes:     v1.ID,
			}
			p.Credentials = append(p.Credentials, v2)
			newCredentials = append(newCredentials, v2)
			sub.Refs = append(sub.Refs, model.CredentialRef{
				CredentialID:    v2.ID,
				ExpectedVersion: 2,
				Role:            "pending",
			})
			p.Rotations = append(p.Rotations, model.Rotation{
				CredentialID: v2.ID,
				FromVersion:  1,
				ToVersion:    2,
			})
		}
		p.Subscriptions = append(p.Subscriptions, sub)
	}

	maxExpiryTick := int64(0)
	for _, c := range p.Credentials {
		expiryTick := (c.IssuedAtMillis + c.TTLMillis + p.TickMillis - 1) / p.TickMillis
		if expiryTick > maxExpiryTick {
			maxExpiryTick = expiryTick
		}
	}
	p.HorizonTicks = maxExpiryTick + 2
	if p.HorizonTicks < 12 {
		p.HorizonTicks = 12
	}
	if p.HorizonTicks > 32 {
		p.HorizonTicks = 32
	}

	if len(newCredentials) > 0 {
		sort.Slice(newCredentials, func(i, j int) bool {
			return newCredentials[i].ID < newCredentials[j].ID
		})
		terminal := newCredentials[r.intn(len(newCredentials))]
		attempts := make([]int, p.Reload.RetryLimit)
		for i := range attempts {
			attempts[i] = i + 1
		}
		p.Reload.FailurePlan = append(p.Reload.FailurePlan, model.FailureInjection{
			SubscriptionID: credentialOwner(terminal.ID),
			Attempts:       attempts,
		})
		if p.Reload.RetryLimit >= 2 && len(newCredentials) > 1 {
			transient := newCredentials[(findCredential(newCredentials, terminal.ID)+1)%len(newCredentials)]
			if transient.ID != terminal.ID {
				p.Reload.FailurePlan = append(p.Reload.FailurePlan, model.FailureInjection{
					SubscriptionID: credentialOwner(transient.ID),
					Attempts:       []int{1},
				})
			}
		}
	}

	p.Sort()
	bundle := &Bundle{
		Seed:     seed,
		Manifest: p.ManifestVersion,
		Project:  p.Clone(),
		Files:    make(map[string][]byte),
	}
	v1, err := encodeV1(p)
	if err != nil {
		return nil, err
	}
	v2, err := encodeV2(p)
	if err != nil {
		return nil, err
	}
	v3, err := encodeV3(p)
	if err != nil {
		return nil, err
	}
	bundle.Files["project_v1.json"] = v1
	bundle.Files["project_v2.json"] = v2
	bundle.Files["project_v3.json"] = v3
	oracle, err := BuildOracle(p, seed)
	if err != nil {
		return nil, err
	}
	bundle.Oracle = oracle
	oracleJSON, err := json.MarshalIndent(oracle, "", "  ")
	if err != nil {
		return nil, err
	}
	bundle.Files["oracle.json"] = append(oracleJSON, '\n')
	manifestJSON, err := json.MarshalIndent(map[string]any{
		"seed":             seed,
		"manifest_version": p.ManifestVersion,
		"files":            []string{"project_v1.json", "project_v2.json", "project_v3.json", "oracle.json"},
		"generator":        "credrot deterministic generator",
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	bundle.Files["manifest.json"] = append(manifestJSON, '\n')
	return bundle, nil
}

func findCredential(creds []model.Credential, id string) int {
	for i, c := range creds {
		if c.ID == id {
			return i
		}
	}
	return 0
}

func credentialOwner(credentialID string) string {
	id := credentialID
	if len(id) > len("cred-") && id[:len("cred-")] == "cred-" {
		id = id[len("cred-"):]
	}
	if len(id) > 3 && id[len(id)-3:] == "-v2" {
		id = id[:len(id)-3]
	}
	return id
}

func encodeV1(p *model.Project) ([]byte, error) {
	type credential struct {
		CredentialID      string `json:"credential_id"`
		CredentialVersion uint64 `json:"credential_version"`
		IssuedTick        int64  `json:"issued_tick"`
		TTL               int64  `json:"ttl"`
		ReloadBatch       int64  `json:"reload_batch"`
		Supersedes        string `json:"supersedes,omitempty"`
	}
	type ref struct {
		CredentialID string `json:"credential_id"`
		Version      uint64 `json:"version"`
		Role         string `json:"role,omitempty"`
	}
	type subscription struct {
		SubscriptionID string   `json:"subscription_id"`
		Active         bool     `json:"active"`
		DependsOn      []string `json:"depends_on"`
		Refs           []ref    `json:"refs"`
	}
	type failure struct {
		SubscriptionID string `json:"subscription_id"`
		Attempts       []int  `json:"attempts"`
	}
	type reload struct {
		ScheduleStartTick int64     `json:"schedule_start_tick"`
		MaxPerBatch       int       `json:"max_per_batch"`
		RetryLimit        int       `json:"retry_limit"`
		RetryStepTicks    int64     `json:"retry_step_ticks"`
		FailurePlan       []failure `json:"failure_plan,omitempty"`
	}
	type doc struct {
		Schema          string           `json:"schema"`
		ProjectID       string           `json:"project_id"`
		ManifestVersion string           `json:"manifest_version"`
		Time            map[string]any   `json:"time"`
		TTLUnit         string           `json:"ttl_unit"`
		HorizonTicks    int64            `json:"horizon_ticks"`
		WatermarkTick   int64            `json:"last_rotation_watermark_tick"`
		Credentials     []credential     `json:"credentials"`
		Subscriptions   []subscription   `json:"subscriptions"`
		Rotations       []model.Rotation `json:"rotations"`
		Reload          reload           `json:"reload"`
	}
	out := doc{
		Schema:          "credential-project/v1",
		ProjectID:       p.ProjectID,
		ManifestVersion: p.ManifestVersion,
		Time: map[string]any{
			"unit":    "logical_tick",
			"tick_ms": p.TickMillis,
		},
		TTLUnit:       "seconds",
		HorizonTicks:  p.HorizonTicks,
		WatermarkTick: p.LastRotationWatermarkMillis / p.TickMillis,
		Rotations:     p.Rotations,
	}
	for _, c := range p.Credentials {
		out.Credentials = append(out.Credentials, credential{
			CredentialID:      c.ID,
			CredentialVersion: c.Version,
			IssuedTick:        c.IssuedAtMillis / p.TickMillis,
			TTL:               c.TTLMillis / 1000,
			ReloadBatch:       c.ReloadBatch,
			Supersedes:        c.Supersedes,
		})
	}
	for _, sub := range p.Subscriptions {
		s := subscription{
			SubscriptionID: sub.ID,
			Active:         sub.Active,
			DependsOn:      append([]string(nil), sub.DependsOn...),
		}
		for _, r := range sub.Refs {
			s.Refs = append(s.Refs, ref{CredentialID: r.CredentialID, Version: r.ExpectedVersion, Role: r.Role})
		}
		out.Subscriptions = append(out.Subscriptions, s)
	}
	out.Reload = reload{
		ScheduleStartTick: p.Reload.ScheduleStartTick,
		MaxPerBatch:       p.Reload.MaxPerBatch,
		RetryLimit:        p.Reload.RetryLimit,
		RetryStepTicks:    p.Reload.RetryStepMillis / p.TickMillis,
	}
	for _, fp := range p.Reload.FailurePlan {
		out.Reload.FailurePlan = append(out.Reload.FailurePlan, failure{SubscriptionID: fp.SubscriptionID, Attempts: fp.Attempts})
	}
	return json.MarshalIndent(out, "", "  ")
}

func encodeV2(p *model.Project) ([]byte, error) {
	type treeCredential struct {
		KeyID      string `json:"key_id"`
		Version    int64  `json:"version"` // legacy swap: reload batch
		Batch      uint64 `json:"batch"`   // legacy swap: credential version
		IssuedTick int64  `json:"issued_tick"`
		TTL        int64  `json:"ttl"`
		Supersedes string `json:"supersedes,omitempty"`
	}
	type keyRef struct {
		KeyID      string `json:"key_id"`
		Generation uint64 `json:"generation"`
		Purpose    string `json:"purpose,omitempty"`
	}
	type face struct {
		Active    bool     `json:"active"`
		DependsOn []string `json:"depends_on_faces"`
		KeyRefs   []keyRef `json:"key_refs"`
	}
	type failure struct {
		SubscriptionID string `json:"subscription_id"`
		Attempts       []int  `json:"attempts"`
	}
	type reload struct {
		ScheduleStartTick int64     `json:"schedule_start_tick"`
		BatchCap          int       `json:"batch_cap"`
		RetryCeiling      int       `json:"retry_ceiling"`
		RetryStepTicks    int64     `json:"retry_step_ticks"`
		FailurePlan       []failure `json:"failure_plan,omitempty"`
	}
	type doc struct {
		Schema           string                      `json:"schema"`
		ProjectID        string                      `json:"project_id"`
		ManifestRevision string                      `json:"manifest_revision"`
		Clock            map[string]any              `json:"clock"`
		TTLUnit          string                      `json:"ttl_unit"`
		HorizonTicks     int64                       `json:"horizon_ticks"`
		CredentialTree   map[string][]treeCredential `json:"credential_tree"`
		Faces            map[string]face             `json:"faces"`
		Rotations        []model.Rotation            `json:"rotations"`
		ReloadLimits     reload                      `json:"reload_limits"`
	}
	out := doc{
		Schema:           "credential-project/v2",
		ProjectID:        p.ProjectID,
		ManifestRevision: p.ManifestVersion,
		Clock: map[string]any{
			"basis":   "logical_tick",
			"tick_ms": p.TickMillis,
		},
		TTLUnit:        "seconds",
		HorizonTicks:   p.HorizonTicks,
		CredentialTree: map[string][]treeCredential{},
		Faces:          map[string]face{},
		Rotations:      p.Rotations,
	}
	creds := p.CredentialByID()
	for _, sub := range p.Subscriptions {
		f := face{Active: sub.Active, DependsOn: append([]string(nil), sub.DependsOn...)}
		for _, r := range sub.Refs {
			f.KeyRefs = append(f.KeyRefs, keyRef{KeyID: r.CredentialID, Generation: r.ExpectedVersion, Purpose: r.Role})
			c := creds[r.CredentialID]
			out.CredentialTree[sub.ID] = append(out.CredentialTree[sub.ID], treeCredential{
				KeyID:      c.ID,
				Version:    c.ReloadBatch,
				Batch:      c.Version,
				IssuedTick: c.IssuedAtMillis / p.TickMillis,
				TTL:        c.TTLMillis / 1000,
				Supersedes: c.Supersedes,
			})
		}
		out.Faces[sub.ID] = f
	}
	out.ReloadLimits = reload{
		ScheduleStartTick: p.Reload.ScheduleStartTick,
		BatchCap:          p.Reload.MaxPerBatch,
		RetryCeiling:      p.Reload.RetryLimit,
		RetryStepTicks:    p.Reload.RetryStepMillis / p.TickMillis,
	}
	for _, fp := range p.Reload.FailurePlan {
		out.ReloadLimits.FailurePlan = append(out.ReloadLimits.FailurePlan, failure{SubscriptionID: fp.SubscriptionID, Attempts: fp.Attempts})
	}
	return json.MarshalIndent(out, "", "  ")
}

func encodeV3(p *model.Project) ([]byte, error) {
	type credential struct {
		ID         string `json:"id"`
		Generation uint64 `json:"generation"`
		IssuedAtMS int64  `json:"issued_at_ms"`
		TTLMS      int64  `json:"ttl_ms"`
		Batch      int64  `json:"batch"`
		Supersedes string `json:"supersedes,omitempty"`
	}
	type ref struct {
		CredentialID string `json:"credential_id"`
		Generation   uint64 `json:"generation"`
		Role         string `json:"role,omitempty"`
	}
	type subscriber struct {
		SubscriberID string   `json:"subscriber_id"`
		Active       bool     `json:"active"`
		DependsOn    []string `json:"depends_on"`
		Refs         []ref    `json:"refs"`
	}
	type failure struct {
		SubscriptionID string `json:"subscription_id"`
		Attempts       []int  `json:"attempts"`
	}
	type reload struct {
		ScheduleStartTick int64     `json:"schedule_start_tick"`
		MaxPerBatch       int       `json:"max_per_batch"`
		RetryLimit        int       `json:"retry_limit"`
		RetryStepMS       int64     `json:"retry_step_ms"`
		FailurePlan       []failure `json:"failure_plan,omitempty"`
	}
	type doc struct {
		Schema          string           `json:"schema"`
		ProjectID       string           `json:"project_id"`
		ManifestVersion string           `json:"manifest_version"`
		Clock           map[string]any   `json:"clock"`
		TTLUnit         string           `json:"ttl_unit"`
		HorizonMS       int64            `json:"horizon_ms"`
		WatermarkMS     int64            `json:"last_rotation_watermark_ms"`
		Credentials     []credential     `json:"credentials"`
		Subscribers     []subscriber     `json:"subscribers"`
		Rotations       []model.Rotation `json:"rotations"`
		Reload          reload           `json:"reload"`
	}
	out := doc{
		Schema:          "credential-project/v3",
		ProjectID:       p.ProjectID,
		ManifestVersion: p.ManifestVersion,
		Clock: map[string]any{
			"basis":   "milliseconds",
			"tick_ms": p.TickMillis,
		},
		TTLUnit:     "milliseconds",
		HorizonMS:   p.HorizonTicks * p.TickMillis,
		WatermarkMS: p.LastRotationWatermarkMillis,
		Rotations:   p.Rotations,
	}
	for _, c := range p.Credentials {
		out.Credentials = append(out.Credentials, credential{
			ID:         c.ID,
			Generation: c.Version,
			IssuedAtMS: c.IssuedAtMillis,
			TTLMS:      c.TTLMillis,
			Batch:      c.ReloadBatch,
			Supersedes: c.Supersedes,
		})
	}
	for _, sub := range p.Subscriptions {
		s := subscriber{
			SubscriberID: sub.ID,
			Active:       sub.Active,
			DependsOn:    append([]string(nil), sub.DependsOn...),
		}
		for _, r := range sub.Refs {
			s.Refs = append(s.Refs, ref{CredentialID: r.CredentialID, Generation: r.ExpectedVersion, Role: r.Role})
		}
		out.Subscribers = append(out.Subscribers, s)
	}
	out.Reload = reload{
		ScheduleStartTick: p.Reload.ScheduleStartTick,
		MaxPerBatch:       p.Reload.MaxPerBatch,
		RetryLimit:        p.Reload.RetryLimit,
		RetryStepMS:       p.Reload.RetryStepMillis,
	}
	for _, fp := range p.Reload.FailurePlan {
		out.Reload.FailurePlan = append(out.Reload.FailurePlan, failure{SubscriptionID: fp.SubscriptionID, Attempts: fp.Attempts})
	}
	return json.MarshalIndent(out, "", "  ")
}
