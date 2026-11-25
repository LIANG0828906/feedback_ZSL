package projectio

import (
	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

type docV3 struct {
	APIVersion  string    `json:"api_version"`
	ProjectUUID string    `json:"project_uuid"`
	RandomSeed  int64     `json:"random_seed"`
	Concurrency int       `json:"concurrency_budget"`
	BudgetScale int       `json:"budget_scale"`
	Hosts       []epV3    `json:"hosts"`
	TrustChains []chainV3 `json:"trust_chains"`
	Timeline    []eventV3 `json:"timeline"`
}

type epV3 struct {
	Host           string   `json:"host"`
	TLSVersions    []string `json:"tls_version_interval"`
	CipherSet      []string `json:"cipher_set"`
	IntervalS      *int64   `json:"interval_seconds"`
	IntervalMs     *int64   `json:"interval_ms"`
	TimeoutS       *int64   `json:"timeout_seconds"`
	TimeoutMs      *int64   `json:"timeout_ms"`
	ParallelLimit  int      `json:"parallel_limit"`
	BufferLimit    int      `json:"buffer_limit"`
	BudgetMyriad   int64    `json:"chain_budget_per_10000"`
	ChainRefs      []string `json:"chain_refs"`
	WaterLevel     *int     `json:"handshake_water_level"`
	PinFingerprint string   `json:"pin_fingerprint_sha256"`
}

type chainV3 struct {
	ID           string   `json:"chain_ref"`
	DepthNote    int      `json:"declared_depth"`
	Certificates []certV3 `json:"path"`
	Pin          string   `json:"fixed_pin_sha256"`
}

type certV3 struct {
	Fingerprint string `json:"fingerprint"`
	SubjectDN   string `json:"subject"`
	IssuerDN    string `json:"issuer"`
	NotBeforeS  *int64 `json:"not_before_seconds"`
	NotBeforeMs *int64 `json:"not_before_ms"`
	NotAfterS   *int64 `json:"not_after_seconds"`
	NotAfterMs  *int64 `json:"not_after_ms"`
	CostMyriad  int64  `json:"level_budget_per_10000"`
}

type eventV3 struct {
	ID       string    `json:"event_id"`
	Sequence int       `json:"logical_sequence"`
	Kind     string    `json:"kind"`
	AtS      *int64    `json:"at_second"`
	AtMs     *int64    `json:"at_ms"`
	Probe    *probeV3  `json:"probe,omitempty"`
	Switch   *switchV3 `json:"switch,omitempty"`
}

type probeV3 struct {
	RequestID      string   `json:"probe_id"`
	Host           string   `json:"host"`
	TLS            string   `json:"tls_version"`
	Ciphers        []string `json:"ciphers"`
	DurationS      *int64   `json:"duration_seconds"`
	DurationMs     *int64   `json:"duration_ms"`
	InterruptLevel int      `json:"interrupt_after_level"`
}

type switchV3 struct {
	UpdateID  string `json:"switch_id"`
	Host      string `json:"host"`
	ChainRef  string `json:"to_chain_ref"`
	CommitS   *int64 `json:"commit_second"`
	CommitMs  *int64 `json:"commit_ms"`
	Interrupt bool   `json:"interrupt_before_commit"`
}

func mapV3(d docV3) (*model.Project, error) {
	scale := d.BudgetScale
	if scale == 0 {
		scale = 10000
	}
	if _, err := ScaledToBP(1, scale); err != nil {
		return nil, bizerr.WithContext(err, "schema", model.SchemaV3)
	}
	p := &model.Project{
		Format: model.CanonicalFormat, ProjectID: d.ProjectUUID, Seed: d.RandomSeed,
		ThreadBudget: d.Concurrency, TickMs: 1000, SourceSchema: model.SchemaV3,
	}
	for i, c := range d.TrustChains {
		ch := model.Chain{ID: c.ID, PinDigest: c.Pin, Pinned: c.Pin != ""}
		if c.DepthNote != 0 && c.DepthNote != len(c.Certificates) {
			return nil, bizerr.Newf(bizerr.ESchemaFieldConflict,
				"declared_depth=%d but path has %d certificates", c.DepthNote, len(c.Certificates)).
				At(path("trust_chains", i, "declared_depth"))
		}
		for j, cert := range c.Certificates {
			from, err := secondsOrMs(cert.NotBeforeS, cert.NotBeforeMs, path("trust_chains", i, "path", j, "not_before"))
			if err != nil {
				return nil, err
			}
			to, err := secondsOrMs(cert.NotAfterS, cert.NotAfterMs, path("trust_chains", i, "path", j, "not_after"))
			if err != nil {
				return nil, err
			}
			cost, err := ScaledToBP(cert.CostMyriad, scale)
			if err != nil {
				return nil, bizerr.At(err, path("trust_chains", i, "path", j, "level_budget"))
			}
			ch.Certificates = append(ch.Certificates, model.Certificate{
				Digest: cert.Fingerprint, Subject: cert.SubjectDN, Issuer: cert.IssuerDN,
				NotBeforeMs: from, NotAfterMs: to, ValidationCostBP: cost,
			})
		}
		p.Chains = append(p.Chains, ch)
	}
	for i, h := range d.Hosts {
		period, err := secondsOrMs(h.IntervalS, h.IntervalMs, path("hosts", i, "interval"))
		if err != nil {
			return nil, err
		}
		timeout, err := secondsOrMs(h.TimeoutS, h.TimeoutMs, path("hosts", i, "timeout"))
		if err != nil {
			return nil, err
		}
		if len(h.TLSVersions) != 2 {
			return nil, bizerr.New(bizerr.ESchemaFieldInvalid, "tls_version_interval must contain exactly [min,max]").
				At(path("hosts", i, "tls_version_interval"))
		}
		budget, err := ScaledToBP(h.BudgetMyriad, scale)
		if err != nil {
			return nil, bizerr.At(err, path("hosts", i, "chain_budget"))
		}
		ep := model.Endpoint{
			ID: h.Host, Name: h.Host, MinProtocol: NormalizeProtocol(h.TLSVersions[0]),
			MaxProtocol: NormalizeProtocol(h.TLSVersions[1]), CipherSuites: h.CipherSet,
			PeriodMs: period, TimeoutMs: timeout, MaxParallel: h.ParallelLimit,
			BufferCapacity: h.BufferLimit, BudgetBP: budget, ChainIDs: h.ChainRefs,
			WatermarkSet: h.WaterLevel != nil, PinDigest: h.PinFingerprint, Pinned: h.PinFingerprint != "",
		}
		if h.WaterLevel != nil {
			ep.HandshakeWatermark = *h.WaterLevel
		}
		p.Endpoints = append(p.Endpoints, ep)
	}
	for i, e := range d.Timeline {
		at, err := secondsOrMs(e.AtS, e.AtMs, path("timeline", i, "at"))
		if err != nil {
			return nil, err
		}
		seq := e.Sequence
		if seq == 0 {
			seq = i + 1
		}
		ev := model.Event{Sequence: seq, Kind: mapKind(e.Kind), AtMs: at}
		if e.Probe != nil {
			dur, err := secondsOrMs(e.Probe.DurationS, e.Probe.DurationMs, path("timeline", i, "probe.duration"))
			if err != nil {
				return nil, err
			}
			ev.Request = &model.Request{
				ID: e.Probe.RequestID, EndpointID: e.Probe.Host,
				OfferedProtocol: NormalizeProtocol(e.Probe.TLS), OfferedCiphers: e.Probe.Ciphers,
				DurationMs: dur, InterruptAfterLevel: e.Probe.InterruptLevel,
			}
		}
		if e.Switch != nil {
			commit, err := secondsOrMs(e.Switch.CommitS, e.Switch.CommitMs, path("timeline", i, "switch.commit"))
			if err != nil {
				return nil, err
			}
			ev.Update = &model.ChainUpdate{
				ID: e.Switch.UpdateID, EndpointID: e.Switch.Host, ChainID: e.Switch.ChainRef,
				CommitAtMs: commit, InterruptBeforeCommit: e.Switch.Interrupt,
			}
		}
		p.Events = append(p.Events, ev)
	}
	sortEndpointsAndEvents(p)
	if err := validateCanonical(p); err != nil {
		return nil, err
	}
	return p, nil
}
