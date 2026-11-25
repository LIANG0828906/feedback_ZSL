package projectio

import (
	"bytes"
	"encoding/json"
	"sort"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

type envelope struct {
	Schema        string `json:"schema"`
	SchemaVersion string `json:"schemaVersion"`
	APIVersion    string `json:"api_version"`
	Version       int    `json:"version"`
}

func Load(data []byte) (*model.Project, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, bizerr.New(bizerr.ESchemaFieldInvalid, "invalid JSON document: "+err.Error())
	}
	switch {
	case env.Schema == model.SchemaV1:
		var doc docV1
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return mapV1(doc)
	case env.SchemaVersion == model.SchemaV2:
		var doc docV2
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return mapV2(doc)
	case env.APIVersion == model.SchemaV3 || (env.APIVersion == "" && env.Version == 3):
		var doc docV3
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return mapV3(doc)
	default:
		return nil, bizerr.Newf(bizerr.ESchemaUnsupported,
			"unrecognized probe schema (schema=%q schemaVersion=%q api_version=%q version=%d)",
			env.Schema, env.SchemaVersion, env.APIVersion, env.Version).
			WithContext("supported", []string{model.SchemaV1, model.SchemaV2, model.SchemaV3})
	}
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return bizerr.New(bizerr.ESchemaFieldInvalid, err.Error())
	}
	return nil
}

type docV1 struct {
	Schema       string    `json:"schema"`
	ProjectID    string    `json:"project_id"`
	Seed         int64     `json:"seed"`
	TickMs       int64     `json:"tick_ms"`
	ThreadBudget int       `json:"thread_budget"`
	BudgetScale  int       `json:"budget_scale"`
	Targets      []epV1    `json:"targets"`
	Chains       []chainV1 `json:"certificate_chains"`
	Events       []eventV1 `json:"logical_events"`
}

type epV1 struct {
	Target          string   `json:"target"`
	TLSMin          string   `json:"tls_min"`
	TLSMax          string   `json:"tls_max"`
	Ciphers         []string `json:"cipher_suites"`
	PeriodTicks     *int64   `json:"probe_every_ticks"`
	PeriodMs        *int64   `json:"probe_period_ms"`
	TimeoutTicks    *int64   `json:"timeout_ticks"`
	TimeoutMs       *int64   `json:"timeout_ms"`
	MaxHandshakes   int      `json:"max_parallel_handshakes"`
	PendingBuffer   int      `json:"pending_buffer"`
	BudgetPermille  int64    `json:"validation_budget_permille"`
	CandidateChains []string `json:"candidate_chains"`
	WaterLevel      *int     `json:"handshake_water_level,omitempty"`
	ChainFixed      *bool    `json:"chain_fixed,omitempty"`
	FixedDigest     string   `json:"fixed_digest,omitempty"`
}

type chainV1 struct {
	ChainID      string   `json:"chain_id"`
	Certificates []certV1 `json:"certificates"`
}

type certV1 struct {
	Digest        string `json:"cert_digest"`
	Subject       string `json:"subject_cn"`
	Issuer        string `json:"issuer_cn"`
	NotBeforeTick *int64 `json:"valid_from_tick"`
	NotAfterTick  *int64 `json:"valid_until_tick"`
	NotBeforeMs   *int64 `json:"valid_from_ms"`
	NotAfterMs    *int64 `json:"valid_until_ms"`
	CostPermille  int64  `json:"check_cost_permille"`
}

type eventV1 struct {
	Sequence int        `json:"sequence"`
	Kind     string     `json:"event"`
	AtTick   *int64     `json:"at_tick"`
	AtMs     *int64     `json:"at_ms"`
	Request  *requestV1 `json:"handshake_request,omitempty"`
	Update   *updateV1  `json:"chain_update,omitempty"`
}

type requestV1 struct {
	RequestID       string   `json:"request_id"`
	Target          string   `json:"target"`
	OfferedProtocol string   `json:"offered_tls"`
	OfferedCiphers  []string `json:"offered_ciphers"`
	DurationTicks   *int64   `json:"duration_ticks"`
	DurationMs      *int64   `json:"duration_ms"`
	InterruptLevel  int      `json:"interrupt_after_level,omitempty"`
}

type updateV1 struct {
	UpdateID        string `json:"update_id"`
	Target          string `json:"target"`
	ChainID         string `json:"switch_to_chain"`
	CommitAtTick    *int64 `json:"commit_at_tick"`
	CommitAtMs      *int64 `json:"commit_at_ms"`
	InterruptCommit bool   `json:"interrupt_before_commit,omitempty"`
}

func mapV1(d docV1) (*model.Project, error) {
	if d.TickMs <= 0 {
		return nil, bizerr.New(bizerr.ESchemaFieldInvalid, "tick_ms must be positive").At("tick_ms")
	}
	scale := d.BudgetScale
	if scale == 0 {
		scale = 1000
	}
	if _, err := ScaledToBP(1, scale); err != nil {
		return nil, bizerr.WithContext(err, "schema", model.SchemaV1)
	}
	p := &model.Project{
		Format:       model.CanonicalFormat,
		ProjectID:    d.ProjectID,
		Seed:         d.Seed,
		TickMs:       d.TickMs,
		ThreadBudget: d.ThreadBudget,
		SourceSchema: model.SchemaV1,
	}
	for i, c := range d.Chains {
		cc, err := mapCertListV1(c.Certificates, d.TickMs, scale, "certificate_chains["+itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		p.Chains = append(p.Chains, model.Chain{ID: c.ChainID, Certificates: cc})
	}
	for i, e := range d.Targets {
		period, err := ticksOrMs(e.PeriodTicks, e.PeriodMs, d.TickMs, "targets["+itoa(i)+"].probe_every")
		if err != nil {
			return nil, err
		}
		timeout, err := ticksOrMs(e.TimeoutTicks, e.TimeoutMs, d.TickMs, "targets["+itoa(i)+"].timeout")
		if err != nil {
			return nil, err
		}
		budget, err := ScaledToBP(e.BudgetPermille, scale)
		if err != nil {
			return nil, bizerr.At(err, "targets["+itoa(i)+"].validation_budget_permille")
		}
		p.Endpoints = append(p.Endpoints, model.Endpoint{
			ID: e.Target, Name: e.Target,
			MinProtocol: NormalizeProtocol(e.TLSMin), MaxProtocol: NormalizeProtocol(e.TLSMax),
			CipherSuites: e.Ciphers, PeriodMs: period, TimeoutMs: timeout,
			MaxParallel: e.MaxHandshakes, BufferCapacity: e.PendingBuffer,
			BudgetBP: budget, ChainIDs: e.CandidateChains,
			WatermarkSet: e.WaterLevel != nil, PinDigest: e.FixedDigest,
		})
		ep := &p.Endpoints[len(p.Endpoints)-1]
		if e.WaterLevel != nil {
			ep.HandshakeWatermark = *e.WaterLevel
		}
		if e.ChainFixed != nil {
			ep.Pinned = *e.ChainFixed
		} else if e.FixedDigest != "" {
			ep.Pinned = true
		}
	}
	var err error
	p.Events, err = mapEventsV1(d.Events, d.TickMs)
	if err != nil {
		return nil, err
	}
	sortEndpointsAndEvents(p)
	if err := validateCanonical(p); err != nil {
		return nil, err
	}
	return p, nil
}

func mapCertListV1(in []certV1, tickMs int64, scale int, path string) ([]model.Certificate, error) {
	out := make([]model.Certificate, 0, len(in))
	for i, c := range in {
		from, err := ticksOrMs(c.NotBeforeTick, c.NotBeforeMs, tickMs, path+"["+itoa(i)+"].valid_from")
		if err != nil {
			return nil, err
		}
		to, err := ticksOrMs(c.NotAfterTick, c.NotAfterMs, tickMs, path+"["+itoa(i)+"].valid_until")
		if err != nil {
			return nil, err
		}
		cost, err := ScaledToBP(c.CostPermille, scale)
		if err != nil {
			return nil, bizerr.At(err, path+"["+itoa(i)+"].check_cost")
		}
		out = append(out, model.Certificate{
			Digest: c.Digest, Subject: c.Subject, Issuer: c.Issuer,
			NotBeforeMs: from, NotAfterMs: to, ValidationCostBP: cost,
		})
	}
	return out, nil
}

func ticksOrMs(ticks *int64, ms *int64, tickMs int64, path string) (int64, error) {
	if ticks != nil && ms != nil {
		return 0, bizerr.New(bizerr.ESchemaFieldConflict, "tick and millisecond fields are both set").At(path)
	}
	if ticks != nil {
		return *ticks * tickMs, nil
	}
	if ms != nil {
		return *ms, nil
	}
	return 0, bizerr.New(bizerr.ESchemaFieldInvalid, "either tick or millisecond field is required").At(path)
}

func secondsOrMs(seconds *int64, ms *int64, path string) (int64, error) {
	if seconds != nil && ms != nil {
		return 0, bizerr.New(bizerr.ESchemaFieldConflict, "second and millisecond fields are both set").At(path)
	}
	if seconds != nil {
		return *seconds * 1000, nil
	}
	if ms != nil {
		return *ms, nil
	}
	return 0, bizerr.New(bizerr.ESchemaFieldInvalid, "either second or millisecond field is required").At(path)
}

func mapEventsV1(in []eventV1, tickMs int64) ([]model.Event, error) {
	out := make([]model.Event, 0, len(in))
	for i, e := range in {
		at, err := ticksOrMs(e.AtTick, e.AtMs, tickMs, "logical_events["+itoa(i)+"].at")
		if err != nil {
			return nil, err
		}
		ev := model.Event{Sequence: e.Sequence, Kind: mapKind(e.Kind), AtMs: at}
		if e.Request != nil {
			dur, err := ticksOrMs(e.Request.DurationTicks, e.Request.DurationMs, tickMs, "events.request.duration")
			if err != nil {
				return nil, err
			}
			ev.Request = &model.Request{
				ID: e.Request.RequestID, EndpointID: e.Request.Target,
				OfferedProtocol: NormalizeProtocol(e.Request.OfferedProtocol),
				OfferedCiphers:  e.Request.OfferedCiphers, DurationMs: dur,
				InterruptAfterLevel: e.Request.InterruptLevel,
			}
		}
		if e.Update != nil {
			commit, err := ticksOrMs(e.Update.CommitAtTick, e.Update.CommitAtMs, tickMs, "events.update.commit_at")
			if err != nil {
				return nil, err
			}
			ev.Update = &model.ChainUpdate{
				ID: e.Update.UpdateID, EndpointID: e.Update.Target, ChainID: e.Update.ChainID,
				CommitAtMs: commit, InterruptBeforeCommit: e.Update.InterruptCommit,
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

func mapKind(k string) string {
	switch k {
	case "request", "handshake", "handshake_request", "TLS_HANDSHAKE":
		return model.KindHandshake
	case "update", "chain_update", "CERT_CHAIN_UPDATE":
		return model.KindChainUpdate
	default:
		return k
	}
}

func sortEndpointsAndEvents(p *model.Project) {
	sort.Slice(p.Endpoints, func(i, j int) bool { return p.Endpoints[i].ID < p.Endpoints[j].ID })
	sort.SliceStable(p.Events, func(i, j int) bool { return p.Events[i].Sequence < p.Events[j].Sequence })
	for i := range p.Events {
		if p.Events[i].Sequence == 0 {
			p.Events[i].Sequence = i + 1
		}
	}
}
