package projectio

import (
	"encoding/json"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/model"
)

func Encode(p model.Project, schema string) ([]byte, error) {
	var doc any
	var err error
	switch schema {
	case model.SchemaV1:
		doc, err = encodeV1(p)
	case model.SchemaV2:
		doc, err = encodeV2(p)
	case model.SchemaV3:
		doc, err = encodeV3(p)
	default:
		return nil, bizerr.Newf(bizerr.ESchemaUnsupported, "cannot encode unknown schema %q", schema)
	}
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func mustTicks(ms, tickMs int64, path string) (*int64, error) {
	if tickMs <= 0 || ms%tickMs != 0 {
		return nil, bizerr.Newf(bizerr.ESchemaFieldConflict, "%s=%d is not representable with tick_ms=%d", path, ms, tickMs)
	}
	v := ms / tickMs
	return &v, nil
}

func mustSeconds(ms int64, path string) (*int64, error) {
	if ms%1000 != 0 {
		return nil, bizerr.Newf(bizerr.ESchemaFieldConflict, "%s=%d is not whole seconds", path, ms)
	}
	v := ms / 1000
	return &v, nil
}

func encodeV1(p model.Project) (docV1, error) {
	tick := p.TickMs
	if tick == 0 {
		tick = 100
	}
	doc := docV1{
		Schema: model.SchemaV1, ProjectID: p.ProjectID, Seed: p.Seed, TickMs: tick,
		ThreadBudget: p.ThreadBudget, BudgetScale: 1000,
	}
	for _, ep := range p.Endpoints {
		period, err := mustTicks(ep.PeriodMs, tick, "period")
		if err != nil {
			return docV1{}, err
		}
		timeout, err := mustTicks(ep.TimeoutMs, tick, "timeout")
		if err != nil {
			return docV1{}, err
		}
		budget, err := BPToScaled(ep.BudgetBP, 1000)
		if err != nil {
			return docV1{}, err
		}
		dto := epV1{
			Target: ep.ID, TLSMin: ep.MinProtocol, TLSMax: ep.MaxProtocol, Ciphers: ep.CipherSuites,
			PeriodTicks: period, TimeoutTicks: timeout, MaxHandshakes: ep.MaxParallel,
			PendingBuffer: ep.BufferCapacity, BudgetPermille: budget, CandidateChains: ep.ChainIDs,
			FixedDigest: ep.PinDigest,
		}
		if ep.WatermarkSet {
			w := ep.HandshakeWatermark
			dto.WaterLevel = &w
		}
		if ep.Pinned {
			b := true
			dto.ChainFixed = &b
		}
		doc.Targets = append(doc.Targets, dto)
	}
	for _, ch := range p.Chains {
		dc := chainV1{ChainID: ch.ID}
		for _, c := range ch.Certificates {
			from, err := mustTicks(c.NotBeforeMs, tick, "not_before")
			if err != nil {
				return docV1{}, err
			}
			to, err := mustTicks(c.NotAfterMs, tick, "not_after")
			if err != nil {
				return docV1{}, err
			}
			cost, err := BPToScaled(c.ValidationCostBP, 1000)
			if err != nil {
				return docV1{}, err
			}
			dc.Certificates = append(dc.Certificates, certV1{
				Digest: c.Digest, Subject: c.Subject, Issuer: c.Issuer,
				NotBeforeTick: from, NotAfterTick: to, CostPermille: cost,
			})
		}
		doc.Chains = append(doc.Chains, dc)
	}
	for _, ev := range p.Events {
		at, err := mustTicks(ev.AtMs, tick, "event.at")
		if err != nil {
			return docV1{}, err
		}
		de := eventV1{Sequence: ev.Sequence, Kind: ev.Kind, AtTick: at}
		if ev.Request != nil {
			dur, err := mustTicks(ev.Request.DurationMs, tick, "request.duration")
			if err != nil {
				return docV1{}, err
			}
			de.Request = &requestV1{
				RequestID: ev.Request.ID, Target: ev.Request.EndpointID,
				OfferedProtocol: ev.Request.OfferedProtocol, OfferedCiphers: ev.Request.OfferedCiphers,
				DurationTicks: dur, InterruptLevel: ev.Request.InterruptAfterLevel,
			}
		}
		if ev.Update != nil {
			commit, err := mustTicks(ev.Update.CommitAtMs, tick, "update.commit")
			if err != nil {
				return docV1{}, err
			}
			de.Update = &updateV1{
				UpdateID: ev.Update.ID, Target: ev.Update.EndpointID, ChainID: ev.Update.ChainID,
				CommitAtTick: commit, InterruptCommit: ev.Update.InterruptBeforeCommit,
			}
		}
		doc.Events = append(doc.Events, de)
	}
	return doc, nil
}

func encodeV2(p model.Project) (docV2, error) {
	doc := docV2{
		SchemaVersion: model.SchemaV2, ProjectID: p.ProjectID, Seed: p.Seed,
		ThreadBudget: p.ThreadBudget, BudgetScale: 10000,
	}
	for _, ch := range p.Chains {
		g := groupV2{Chain: chainV2{ChainID: ch.ID, PinDigest: ch.PinDigest}}
		pinned := ch.Pinned
		g.Chain.Pinned = &pinned
		for _, c := range ch.Certificates {
			cost, err := BPToScaled(c.ValidationCostBP, 10000)
			if err != nil {
				return docV2{}, err
			}
			g.Chain.Certificates = append(g.Chain.Certificates, certV2{
				Digest: c.Digest, Subject: c.Subject, Issuer: c.Issuer,
				NotBeforeMs: c.NotBeforeMs, NotAfterMs: c.NotAfterMs, CostBP: cost,
			})
		}
		for _, ep := range p.Endpoints {
			if len(ep.ChainIDs) == 0 || ep.ChainIDs[0] != ch.ID {
				continue
			}
			budget, err := BPToScaled(ep.BudgetBP, 10000)
			if err != nil {
				return docV2{}, err
			}
			target := epV2{
				Name: ep.ID, ProtocolRange: [2]string{ep.MinProtocol, ep.MaxProtocol},
				CipherSuites: ep.CipherSuites, PeriodMs: ep.PeriodMs, TimeoutMs: ep.TimeoutMs,
				MaxParallel: ep.MaxParallel, PendingBuffer: ep.BufferCapacity, BudgetBP: budget,
				CandidateChainIDs: ep.ChainIDs, PinDigest: ep.PinDigest,
			}
			if ep.WatermarkSet {
				w := ep.HandshakeWatermark
				target.Watermark = &w
			}
			pinnedEP := ep.Pinned
			target.Pinned = &pinnedEP
			g.Targets = append(g.Targets, target)
		}
		doc.ProbeTree = append(doc.ProbeTree, g)
	}
	for _, ev := range p.Events {
		de := eventV2{Sequence: ev.Sequence, Type: ev.Kind, AtMs: ev.AtMs}
		if ev.Request != nil {
			de.Request = &requestV2{
				RequestID: ev.Request.ID, EndpointName: ev.Request.EndpointID,
				Protocol: ev.Request.OfferedProtocol, Ciphers: ev.Request.OfferedCiphers,
				DurationMs: ev.Request.DurationMs, InterruptLevel: ev.Request.InterruptAfterLevel,
			}
		}
		if ev.Update != nil {
			de.Update = &updateV2{
				UpdateID: ev.Update.ID, EndpointName: ev.Update.EndpointID, ChainID: ev.Update.ChainID,
				CommitAtMs: ev.Update.CommitAtMs, InterruptCommit: ev.Update.InterruptBeforeCommit,
			}
		}
		doc.Events = append(doc.Events, de)
	}
	return doc, nil
}

func encodeV3(p model.Project) (docV3, error) {
	doc := docV3{
		APIVersion: model.SchemaV3, ProjectUUID: p.ProjectID, RandomSeed: p.Seed,
		Concurrency: p.ThreadBudget, BudgetScale: 10000,
	}
	for _, ep := range p.Endpoints {
		period, err := mustSeconds(ep.PeriodMs, "period")
		if err != nil {
			return docV3{}, err
		}
		timeout, err := mustSeconds(ep.TimeoutMs, "timeout")
		if err != nil {
			return docV3{}, err
		}
		budget, err := BPToScaled(ep.BudgetBP, 10000)
		if err != nil {
			return docV3{}, err
		}
		h := epV3{
			Host: ep.ID, TLSVersions: []string{ep.MinProtocol, ep.MaxProtocol}, CipherSet: ep.CipherSuites,
			IntervalS: period, TimeoutS: timeout, ParallelLimit: ep.MaxParallel, BufferLimit: ep.BufferCapacity,
			BudgetMyriad: budget, ChainRefs: ep.ChainIDs, PinFingerprint: ep.PinDigest,
		}
		if ep.WatermarkSet {
			w := ep.HandshakeWatermark
			h.WaterLevel = &w
		}
		doc.Hosts = append(doc.Hosts, h)
	}
	for _, ch := range p.Chains {
		tc := chainV3{ID: ch.ID, DepthNote: len(ch.Certificates), Pin: ch.PinDigest}
		for _, c := range ch.Certificates {
			from, err := mustSeconds(c.NotBeforeMs, "not_before")
			if err != nil {
				return docV3{}, err
			}
			to, err := mustSeconds(c.NotAfterMs, "not_after")
			if err != nil {
				return docV3{}, err
			}
			cost, err := BPToScaled(c.ValidationCostBP, 10000)
			if err != nil {
				return docV3{}, err
			}
			tc.Certificates = append(tc.Certificates, certV3{
				Fingerprint: c.Digest, SubjectDN: c.Subject, IssuerDN: c.Issuer,
				NotBeforeS: from, NotAfterS: to, CostMyriad: cost,
			})
		}
		doc.TrustChains = append(doc.TrustChains, tc)
	}
	for _, ev := range p.Events {
		at, err := mustSeconds(ev.AtMs, "event.at")
		if err != nil {
			return docV3{}, err
		}
		de := eventV3{ID: "EVT-" + itoa(ev.Sequence), Sequence: ev.Sequence, Kind: ev.Kind, AtS: at}
		if ev.Request != nil {
			dur, err := mustSeconds(ev.Request.DurationMs, "duration")
			if err != nil {
				return docV3{}, err
			}
			de.Probe = &probeV3{
				RequestID: ev.Request.ID, Host: ev.Request.EndpointID, TLS: ev.Request.OfferedProtocol,
				Ciphers: ev.Request.OfferedCiphers, DurationS: dur, InterruptLevel: ev.Request.InterruptAfterLevel,
			}
		}
		if ev.Update != nil {
			commit, err := mustSeconds(ev.Update.CommitAtMs, "commit")
			if err != nil {
				return docV3{}, err
			}
			de.Switch = &switchV3{
				UpdateID: ev.Update.ID, Host: ev.Update.EndpointID, ChainRef: ev.Update.ChainID,
				CommitS: commit, Interrupt: ev.Update.InterruptBeforeCommit,
			}
		}
		doc.Timeline = append(doc.Timeline, de)
	}
	return doc, nil
}
