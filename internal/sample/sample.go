package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"tlscheck/internal/engine"
	"tlscheck/internal/model"
	"tlscheck/internal/projectio"
)

const DefaultSeed int64 = 420261126

type Bundle struct {
	Project   model.Project  `json:"project"`
	Truth     *engine.Report `json:"truth"`
	SchemaV1  []byte         `json:"-"`
	SchemaV2  []byte         `json:"-"`
	SchemaV3  []byte         `json:"-"`
	TruthJSON []byte         `json:"-"`
}

func Build(seed int64) (*Bundle, error) {
	p := model.Project{
		Format: model.CanonicalFormat, ProjectID: "deterministic-tls-probe",
		Seed: seed, TickMs: 100, ThreadBudget: 4,
	}
	ciphers := []string{"TLS_AES_256_GCM_SHA384", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}
	rng := rand.New(rand.NewSource(seed))

	p.Chains = []model.Chain{
		chain3("chain-alpha", seed, "alpha", 0, 200, 100),
		chain3("chain-beta", seed, "beta", 0, 200, 100),
		chain2("chain-gamma", seed, "gamma", 0, 200, 100),
		chain3("chain-delta-v1", seed, "delta1", 0, 200, 100),
		chain3("chain-delta-v2", seed, "delta2", 0, 200, 100),
		chain3("chain-epsilon-v1", seed, "epsilon1", 0, 8, 100),
		expiredChain("chain-epsilon-v2", seed, "epsilon2", 0, 3, 100),
		issuerMismatchChain("chain-zeta", seed),
		chain3("chain-eta", seed, "eta", 0, 200, 100),
	}

	p.Endpoints = []model.Endpoint{
		{
			ID: "alpha-ok", Name: "alpha-ok", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(5), MaxParallel: 2,
			BufferCapacity: 8, BudgetBP: 5000, ChainIDs: []string{"chain-alpha"},
			WatermarkSet: true, HandshakeWatermark: 2, Pinned: true,
			PinDigest: p.Chains[0].Certificates[0].Digest,
		},
		{
			ID: "beta-parallel", Name: "beta-parallel", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(10), MaxParallel: 1,
			BufferCapacity: 8, BudgetBP: 5000, ChainIDs: []string{"chain-beta"},
		},
		{
			ID: "gamma-budget", Name: "gamma-budget", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(10), TimeoutMs: sec(5), MaxParallel: 3,
			BufferCapacity: 8, BudgetBP: 500, ChainIDs: []string{"chain-gamma"},
		},
		{
			ID: "delta-freeze", Name: "delta-freeze", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(5), MaxParallel: 2,
			BufferCapacity: 8, BudgetBP: 5000, ChainIDs: []string{"chain-delta-v1", "chain-delta-v2"},
			WatermarkSet: true, HandshakeWatermark: 1,
		},
		{
			ID: "epsilon-fallback", Name: "epsilon-fallback", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(5), MaxParallel: 2,
			BufferCapacity: 8, BudgetBP: 5000, ChainIDs: []string{"chain-epsilon-v1", "chain-epsilon-v2"},
		},
		{
			ID: "zeta-issuer", Name: "zeta-issuer", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(5), MaxParallel: 2,
			BufferCapacity: 8, BudgetBP: 5000, ChainIDs: []string{"chain-zeta"},
		},
		{
			ID: "eta-backpressure", Name: "eta-backpressure", MinProtocol: "TLS1.2", MaxProtocol: "TLS1.3",
			CipherSuites: ciphers, PeriodMs: sec(100), TimeoutMs: sec(5), MaxParallel: 1,
			BufferCapacity: 1, BudgetBP: 5000, ChainIDs: []string{"chain-eta"},
			WatermarkSet: true, HandshakeWatermark: 1,
		},
	}

	req := func(id, ep string, atS, durS int64) model.Event {
		return model.Event{Kind: model.KindHandshake, AtMs: sec(atS), Request: &model.Request{
			ID: id, EndpointID: ep, OfferedProtocol: "TLS1.3",
			OfferedCiphers: []string{ciphers[rng.Intn(len(ciphers))]}, DurationMs: sec(durS),
		}}
	}
	upd := func(id, ep, chain string, atS, commitS int64) model.Event {
		return model.Event{Kind: model.KindChainUpdate, AtMs: sec(atS), Update: &model.ChainUpdate{
			ID: id, EndpointID: ep, ChainID: chain, CommitAtMs: sec(commitS),
		}}
	}

	p.Events = []model.Event{
		req("A-1", "alpha-ok", 1, 1),
		req("A-2", "alpha-ok", 2, 1),

		req("B-1", "beta-parallel", 1, 2),
		req("B-2", "beta-parallel", 1, 2),
		req("B-3", "beta-parallel", 2, 2),

		req("G-1", "gamma-budget", 1, 1),
		req("G-2", "gamma-budget", 1, 1),
		req("G-3", "gamma-budget", 1, 1),
		req("G-4", "gamma-budget", 2, 1),

		upd("D-U1", "delta-freeze", "chain-delta-v2", 2, 4),
		req("D-1", "delta-freeze", 3, 1),
		req("D-2", "delta-freeze", 4, 1),

		upd("E-U1", "epsilon-fallback", "chain-epsilon-v2", 4, 4),
		req("E-1", "epsilon-fallback", 5, 1),
		req("E-2", "epsilon-fallback", 9, 1),

		req("Z-1", "zeta-issuer", 1, 1),

		req("H-1", "eta-backpressure", 1, 1),
		req("H-2", "eta-backpressure", 1, 1),
		req("H-3", "eta-backpressure", 1, 1),
	}
	sort.Slice(p.Endpoints, func(i, j int) bool { return p.Endpoints[i].ID < p.Endpoints[j].ID })
	assignSequences(p.Events)

	truth, err := engine.Run(p)
	if err != nil {
		return nil, err
	}
	truth.Diagnostics = engine.Diagnostics{}
	b := &Bundle{Project: p, Truth: truth}
	b.SchemaV1, err = projectio.Encode(p, model.SchemaV1)
	if err != nil {
		return nil, err
	}
	b.SchemaV2, err = projectio.Encode(p, model.SchemaV2)
	if err != nil {
		return nil, err
	}
	b.SchemaV3, err = projectio.Encode(p, model.SchemaV3)
	if err != nil {
		return nil, err
	}
	b.TruthJSON, err = json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	b.TruthJSON = append(b.TruthJSON, '\n')
	return b, nil
}

func WriteSampleDir(dir string) (*Bundle, error) {
	b, err := Build(DefaultSeed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"project.v1.json": b.SchemaV1,
		"project.v2.json": b.SchemaV2,
		"project.v3.json": b.SchemaV3,
		"truth.json":      b.TruthJSON,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func assignSequences(events []model.Event) {
	order := model.SortedEventIndexes(events)
	out := make([]model.Event, len(events))
	for seq, idx := range order {
		ev := events[idx]
		ev.Sequence = seq + 1
		out[seq] = ev
	}
	copy(events, out)
}

func sec(v int64) int64 { return v * 1000 }

func digest(seed int64, label string) string {
	sum := sha256.Sum256([]byte(fmtSeed(seed) + ":" + label))
	return "sha256:" + hex.EncodeToString(sum[:])[:32]
}

func fmtSeed(seed int64) string {
	if seed == 0 {
		return "0"
	}
	neg := seed < 0
	if neg {
		seed = -seed
	}
	var b [20]byte
	i := len(b)
	for seed > 0 {
		i--
		b[i] = byte('0' + seed%10)
		seed /= 10
	}
	s := string(b[i:])
	if neg {
		return "-" + s
	}
	return s
}

func chain3(id string, seed int64, label string, fromS, toS, cost int64) model.Chain {
	rootSubj := "CN=" + label + "-root"
	intSubj := "CN=" + label + "-intermediate"
	leafSubj := "CN=" + label + "-leaf"
	return model.Chain{ID: id, Certificates: []model.Certificate{
		{Digest: digest(seed, label+"-leaf"), Subject: leafSubj, Issuer: intSubj, NotBeforeMs: sec(fromS), NotAfterMs: sec(toS), ValidationCostBP: cost},
		{Digest: digest(seed, label+"-int"), Subject: intSubj, Issuer: rootSubj, NotBeforeMs: sec(fromS), NotAfterMs: sec(toS), ValidationCostBP: cost},
		{Digest: digest(seed, label+"-root"), Subject: rootSubj, Issuer: rootSubj, NotBeforeMs: sec(fromS), NotAfterMs: sec(toS), ValidationCostBP: cost},
	}}
}

func chain2(id string, seed int64, label string, fromS, toS, cost int64) model.Chain {
	rootSubj := "CN=" + label + "-root"
	leafSubj := "CN=" + label + "-leaf"
	return model.Chain{ID: id, Certificates: []model.Certificate{
		{Digest: digest(seed, label+"-leaf"), Subject: leafSubj, Issuer: rootSubj, NotBeforeMs: sec(fromS), NotAfterMs: sec(toS), ValidationCostBP: cost},
		{Digest: digest(seed, label+"-root"), Subject: rootSubj, Issuer: rootSubj, NotBeforeMs: sec(fromS), NotAfterMs: sec(toS), ValidationCostBP: cost},
	}}
}

func expiredChain(id string, seed int64, label string, fromS, toS, cost int64) model.Chain {
	return chain3(id, seed, label, fromS, toS, cost)
}

func issuerMismatchChain(id string, seed int64) model.Chain {
	ch := chain3(id, seed, "zeta-bad", 0, 200, 100)
	ch.Certificates[0].Issuer = "CN=wrong-issuer"
	return ch
}
