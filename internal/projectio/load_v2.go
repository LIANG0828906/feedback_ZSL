package projectio

import (
	"tlscheck/internal/bizerr"
	bizerrPkg "tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

type docV2 struct {
	SchemaVersion string    `json:"schemaVersion"`
	ProjectID     string    `json:"projectId"`
	Seed          int64     `json:"deterministicSeed"`
	ThreadBudget  int       `json:"workerThreads"`
	BudgetScale   int       `json:"budgetScale"`
	ProbeTree     []groupV2 `json:"probeTree"`
	Events        []eventV2 `json:"eventLog"`
}

type groupV2 struct {
	Chain   chainV2 `json:"chainGroup"`
	Targets []epV2  `json:"nestedTargets"`
}

type epV2 struct {
	Name              string    `json:"endpointName"`
	ProtocolRange     [2]string `json:"tlsProtocolRange"`
	CipherSuites      []string  `json:"cipherSuiteSet"`
	PeriodMs          int64     `json:"probePeriodMs"`
	TimeoutMs         int64     `json:"handshakeTimeoutMs"`
	MaxParallel       int       `json:"maxParallelHandshakes"`
	PendingBuffer     int       `json:"pendingRequestBuffer"`
	BudgetBP          int64     `json:"validationBudgetBp"`
	CandidateChainIDs []string  `json:"candidateChainIds"`
	Watermark         *int      `json:"handshakeWatermark"`
	Pinned            *bool     `json:"certificatePinned"`
	PinDigest         string    `json:"pinDigest"`
}

type chainV2 struct {
	ChainID      string   `json:"chainId"`
	Certificates []certV2 `json:"certificatePath"`
	Pinned       *bool    `json:"pinned"`
	PinDigest    string   `json:"pinSha256"`
}

type certV2 struct {
	Digest      string `json:"certificateDigest"`
	Subject     string `json:"subjectDistinguishedName"`
	Issuer      string `json:"issuerDistinguishedName"`
	NotBeforeMs int64  `json:"validFromMs"`
	NotAfterMs  int64  `json:"validToMs"`
	CostBP      int64  `json:"validationCostBp"`
}

type eventV2 struct {
	Sequence int        `json:"eventSequence"`
	Type     string     `json:"eventType"`
	AtMs     int64      `json:"logicalTimeMs"`
	Request  *requestV2 `json:"handshakeRequest"`
	Update   *updateV2  `json:"certChainUpdate"`
}

type requestV2 struct {
	RequestID      string   `json:"requestId"`
	EndpointName   string   `json:"endpointName"`
	Protocol       string   `json:"offeredProtocol"`
	Ciphers        []string `json:"offeredCipherSuites"`
	DurationMs     int64    `json:"simulatedDurationMs"`
	InterruptLevel int      `json:"interruptAfterLevel"`
}

type updateV2 struct {
	UpdateID        string `json:"updateId"`
	EndpointName    string `json:"endpointName"`
	ChainID         string `json:"nextChainId"`
	CommitAtMs      int64  `json:"commitTimeMs"`
	InterruptCommit bool   `json:"interruptBeforeCommit"`
}

func mapV2(d docV2) (*model.Project, error) {
	scale := d.BudgetScale
	if scale == 0 {
		scale = 10000
	}
	if _, err := ScaledToBP(1, scale); err != nil {
		return nil, bizerrPkg.WithContext(err, "schema", model.SchemaV2)
	}
	p := &model.Project{
		Format: model.CanonicalFormat, ProjectID: d.ProjectID, Seed: d.Seed,
		ThreadBudget: d.ThreadBudget, TickMs: 1000, SourceSchema: model.SchemaV2,
	}
	for gi, g := range d.ProbeTree {
		ch := model.Chain{ID: g.Chain.ChainID, PinDigest: g.Chain.PinDigest}
		if g.Chain.Pinned != nil {
			ch.Pinned = *g.Chain.Pinned
		}
		for ci, c := range g.Chain.Certificates {
			cost, err := ScaledToBP(c.CostBP, scale)
			if err != nil {
				return nil, bizerr.At(err, path("probeTree", gi, "chainGroup.certificatePath", ci, "validationCostBp"))
			}
			ch.Certificates = append(ch.Certificates, model.Certificate{
				Digest: c.Digest, Subject: c.Subject, Issuer: c.Issuer,
				NotBeforeMs: c.NotBeforeMs, NotAfterMs: c.NotAfterMs, ValidationCostBP: cost,
			})
		}
		p.Chains = append(p.Chains, ch)
		for ei, e := range g.Targets {
			budget, err := ScaledToBP(e.BudgetBP, scale)
			if err != nil {
				return nil, bizerr.At(err, path("probeTree", gi, "nestedTargets", ei, "validationBudgetBp"))
			}
			ep := model.Endpoint{
				ID: e.Name, Name: e.Name,
				MinProtocol:  NormalizeProtocol(e.ProtocolRange[0]),
				MaxProtocol:  NormalizeProtocol(e.ProtocolRange[1]),
				CipherSuites: e.CipherSuites, PeriodMs: e.PeriodMs, TimeoutMs: e.TimeoutMs,
				MaxParallel: e.MaxParallel, BufferCapacity: e.PendingBuffer,
				BudgetBP: budget, ChainIDs: e.CandidateChainIDs,
				HandshakeWatermark: 0, WatermarkSet: e.Watermark != nil,
			}
			if e.Watermark != nil {
				ep.HandshakeWatermark = *e.Watermark
			}
			if e.Pinned != nil {
				ep.Pinned = *e.Pinned
			}
			ep.PinDigest = e.PinDigest
			p.Endpoints = append(p.Endpoints, ep)
		}
	}
	for i, e := range d.Events {
		ev := model.Event{Sequence: e.Sequence, Kind: mapKind(e.Type), AtMs: e.AtMs}
		if e.Request != nil {
			ev.Request = &model.Request{
				ID: e.Request.RequestID, EndpointID: e.Request.EndpointName,
				OfferedProtocol: NormalizeProtocol(e.Request.Protocol), OfferedCiphers: e.Request.Ciphers,
				DurationMs: e.Request.DurationMs, InterruptAfterLevel: e.Request.InterruptLevel,
			}
		}
		if e.Update != nil {
			ev.Update = &model.ChainUpdate{
				ID: e.Update.UpdateID, EndpointID: e.Update.EndpointName, ChainID: e.Update.ChainID,
				CommitAtMs: e.Update.CommitAtMs, InterruptBeforeCommit: e.Update.InterruptCommit,
			}
		}
		p.Events = append(p.Events, ev)
		_ = i
	}
	sortEndpointsAndEvents(p)
	if err := validateCanonical(p); err != nil {
		return nil, err
	}
	return p, nil
}

func path(parts ...any) string {
	out := ""
	for _, part := range parts {
		switch v := part.(type) {
		case string:
			if out == "" {
				out = v
			} else {
				out += "." + v
			}
		case int:
			out += "[" + itoa(v) + "]"
		}
	}
	return out
}
