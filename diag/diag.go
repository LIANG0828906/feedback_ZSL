package diag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"proxydiag/bizerr"
	"proxydiag/checkpoint"
	"proxydiag/domain"
	"proxydiag/engine"
	"proxydiag/samplegen"
	"proxydiag/schema"
)

func LoadProject(samplesDir, version string) (*domain.Project, error) {
	name := version
	if !strings.HasPrefix(name, "project_") {
		switch strings.ToLower(version) {
		case "v1":
			name = "project_v1.json"
		case "v2":
			name = "project_v2.json"
		case "v3", "":
			name = "project_v3.json"
		default:
			return nil, bizerr.New(bizerr.InvalidArgument, "unknown sample version").
				With("version", version)
		}
	}
	return schema.LoadFile(filepath.Join(samplesDir, name))
}

func LoadTruth(samplesDir string) (*domain.Truth, error) {
	truth, err := samplegen.LoadTruth(filepath.Join(samplesDir, "truth.json"))
	if err != nil {
		return nil, bizerr.Wrap(bizerr.IO, "load truth file", err).With("path", samplesDir)
	}
	return truth, nil
}

type VersionCheck struct {
	Version      string `json:"version"`
	Schema       string `json:"schema"`
	Fingerprint  string `json:"fingerprint"`
	Requests     int    `json:"requests"`
	TruthMatched int    `json:"truth_matched"`
	WithinBounds int    `json:"within_bounds"`
	MaxAbsDelta  int64  `json:"max_abs_delta"`
}

type VerificationReport struct {
	Samples         string         `json:"samples"`
	Versions        []VersionCheck `json:"versions"`
	EquivalentCore  bool           `json:"equivalent_core"`
	AllTruthMatched bool           `json:"all_truth_matched"`
	DecisionParity  bool           `json:"decision_parity"`
	TickMsParity    bool           `json:"tick_ms_parity"`
	Failures        []string       `json:"failures,omitempty"`
}

func Verify(samplesDir string) (*VerificationReport, error) {
	truth, err := LoadTruth(samplesDir)
	if err != nil {
		return nil, err
	}
	report := &VerificationReport{Samples: samplesDir}
	projects := map[string]*domain.Project{}
	results := map[string][]engine.Result{}
	versionNames := []string{"v1", "v2", "v3"}
	for _, v := range versionNames {
		p, err := LoadProject(samplesDir, v)
		if err != nil {
			return nil, err
		}
		projects[v] = p
		rs := engine.EvaluateAll(p, truth)
		results[v] = rs
		check := VersionCheck{
			Version: v, Schema: p.SourceSchema, Fingerprint: p.CoreFingerprint(),
			Requests: len(rs),
		}
		for _, r := range rs {
			if r.TruthMatched {
				check.TruthMatched++
			}
			if r.WithinTolerance {
				check.WithinBounds++
			}
			if r.MaxAbsDelta > check.MaxAbsDelta {
				check.MaxAbsDelta = r.MaxAbsDelta
			}
			if !r.TruthMatched {
				report.Failures = append(report.Failures,
					fmt.Sprintf("%s request %d: %v", v, r.Seq, r.Mismatches))
			}
			if r.ErrorCode != "" {
				report.Failures = append(report.Failures,
					fmt.Sprintf("%s request %d: %s", v, r.Seq, r.ErrorCode))
			}
		}
		if check.TruthMatched != len(rs) {
			report.AllTruthMatched = false
		}
		report.Versions = append(report.Versions, check)
	}
	report.EquivalentCore = projects["v1"].CoreFingerprint() == projects["v2"].CoreFingerprint() &&
		projects["v2"].CoreFingerprint() == projects["v3"].CoreFingerprint()
	report.AllTruthMatched = true
	for _, check := range report.Versions {
		if check.TruthMatched != check.Requests || check.WithinBounds != check.Requests {
			report.AllTruthMatched = false
		}
	}
	report.DecisionParity = reflect.DeepEqual(normalizeResults(results["v1"]), normalizeResults(results["v2"])) &&
		reflect.DeepEqual(normalizeResults(results["v2"]), normalizeResults(results["v3"]))
	report.TickMsParity = reflect.DeepEqual(normalizeResults(results["v2"]), normalizeResults(results["v3"]))
	if !report.EquivalentCore {
		report.Failures = append(report.Failures, "loaded project core fingerprints differ")
	}
	if !report.DecisionParity {
		report.Failures = append(report.Failures, "schema versions do not produce identical decisions")
	}
	return report, nil
}

func normalizeResults(in []engine.Result) []engine.Result {
	out := make([]engine.Result, len(in))
	copy(out, in)
	for i := range out {
		out[i].Cache = engine.CacheInfo{}
	}
	return out
}

type IncrementalStep struct {
	Kind       string   `json:"kind"`
	Target     string   `json:"target"`
	AtSeq      int      `json:"at_seq"`
	Source     string   `json:"source"`
	Recomputed []int    `json:"recomputed"`
	Reused     []int    `json:"reused"`
	Closure    []string `json:"closure"`
	MatchFull  bool     `json:"match_full"`
}

type IncrementalReport struct {
	SourceUnchanged bool              `json:"source_unchanged"`
	AllMatchFull    bool              `json:"all_match_full"`
	Steps           []IncrementalStep `json:"steps"`
}

func Incremental(samplesDir string) (*IncrementalReport, error) {
	p, err := LoadProject(samplesDir, "v3")
	if err != nil {
		return nil, err
	}
	sourceFingerprint := p.CoreFingerprint()
	eng := engine.New(p, nil)
	eng.RunAll()
	edits := []engine.Edit{
		{Kind: engine.EditHealth, UpstreamID: "u-blue", Healthy: false, AtSeq: 1},
		{Kind: engine.EditWeight, UpstreamID: "u-green", WeightPPM: 160000, AtSeq: 1},
		{Kind: engine.EditRewrite, RouteID: "R-api-v1", StripPrefix: "/api/v1/", AddPrefix: "/backend/v2/", AtSeq: 1},
		{Kind: engine.EditDelete, Seq: 20, AtSeq: 20},
		{
			Kind: engine.EditAdd, AtSeq: 41,
			Request: &domain.Request{
				Seq: 41, ArrivalMS: 10000, Method: "GET", RawPath: "/api/v1/new/41",
				Headers: []domain.Header{{Name: "X-Request-ID", Value: "req-041"}},
			},
		},
	}
	report := &IncrementalReport{SourceUnchanged: true, AllMatchFull: true}
	for _, edit := range edits {
		inv, err := eng.Apply(edit)
		if err != nil {
			return nil, err
		}
		match := reflect.DeepEqual(normalizeResults(eng.Results()),
			normalizeResults(engine.EvaluateAll(eng.Project(), nil)))
		report.Steps = append(report.Steps, IncrementalStep{
			Kind: string(edit.Kind), Target: inv.Target, AtSeq: inv.AtSeq, Source: inv.Source,
			Recomputed: inv.Recomputed, Reused: inv.Reused, Closure: inv.Closure, MatchFull: match,
		})
		if !match {
			report.AllMatchFull = false
		}
	}
	if p.CoreFingerprint() != sourceFingerprint {
		report.SourceUnchanged = false
		report.AllMatchFull = false
	}
	return report, nil
}

func ResultDigest(r engine.Result) string {
	copyResult := r
	copyResult.Cache = engine.CacheInfo{}
	b, _ := json.Marshal(copyResult)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func decisionFromResult(r engine.Result) checkpoint.Decision {
	return checkpoint.Decision{
		Seq: r.Seq, ListenerID: r.ListenerID, RouteID: r.RouteID,
		UpstreamID: r.SelectedUpstreamID, RequestLine: r.RewrittenRequestLine,
		Terminal: r.Terminal, ResultDigest: ResultDigest(r),
	}
}

type CheckpointReport struct {
	Path                      string   `json:"path"`
	Committed                 int      `json:"committed"`
	Recovered                 int      `json:"recovered"`
	LastCommittedSeq          int      `json:"last_committed_seq"`
	ListenerCaches            int      `json:"listener_caches"`
	HealthEntries             int      `json:"health_entries"`
	UncommittedIgnored        bool     `json:"uncommitted_ignored"`
	TruncationCode            string   `json:"truncation_code"`
	PriorUsableAfterCut       bool     `json:"prior_usable_after_cut"`
	VersionMismatchCode       string   `json:"version_mismatch_code"`
	VersionMismatchRecomputed int      `json:"version_mismatch_recomputed"`
	AllDecisionsMatch         bool     `json:"all_decisions_match"`
	Failures                  []string `json:"failures,omitempty"`
}

func CheckpointDemo(samplesDir, dir string) (*CheckpointReport, error) {
	p, err := LoadProject(samplesDir, "v3")
	if err != nil {
		return nil, err
	}
	truth, err := LoadTruth(samplesDir)
	if err != nil {
		return nil, err
	}
	results := engine.EvaluateAll(p, truth)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "forwarding.checkpoint.jsonl")
	_ = os.Remove(path)
	store, err := checkpoint.NewStore(path, p.Version, 0)
	if err != nil {
		return nil, err
	}
	report := &CheckpointReport{Path: path}
	for _, r := range results {
		d := decisionFromResult(r)
		if err := store.Commit(r.Seq, d, p.Upstreams); err != nil {
			store.Close()
			return nil, err
		}
		report.Committed++
	}
	last := report.Committed
	// Leave one prepare-only record to prove that global sequence ordering
	// prevents an unfinished request from becoming visible.
	nextSeq := last + 1
	d := decisionFromResult(results[len(results)-1])
	d.Seq = nextSeq
	if err := store.StageWithoutCommit(nextSeq, d, p.Upstreams); err != nil {
		store.Close()
		return nil, err
	}
	store.Close()

	state, recErr := checkpoint.Recover(path, p.Version)
	if recErr != nil {
		return nil, recErr
	}
	report.Recovered = len(state.Decisions)
	report.LastCommittedSeq = state.LastCommittedSeq
	report.ListenerCaches = len(state.ListenerCaches)
	report.HealthEntries = len(state.UpstreamHealth)
	report.UncommittedIgnored = state.LastCommittedSeq == last
	report.AllDecisionsMatch = true
	for _, r := range results {
		got, ok := state.Decisions[r.Seq]
		want := decisionFromResult(r)
		if !ok || got != want {
			report.AllDecisionsMatch = false
			report.Failures = append(report.Failures, fmt.Sprintf("decision %d differs", r.Seq))
		}
	}

	// Copy the valid journal, truncate only the copy, and prove prior commits
	// remain readable alongside the stable truncation code.
	data, _ := os.ReadFile(path)
	cutPath := filepath.Join(dir, "truncated.checkpoint.jsonl")
	cutAt := len(data) - 17
	if cutAt < 0 {
		cutAt = len(data)
	}
	if err := os.WriteFile(cutPath, data[:cutAt], 0o644); err != nil {
		return nil, err
	}
	cutState, cutErr := checkpoint.Recover(cutPath, p.Version)
	report.TruncationCode = bizerr.Code(cutErr)
	report.PriorUsableAfterCut = cutState.LastCommittedSeq > 0

	_, versionErr := checkpoint.Recover(path, p.Version+"-other")
	report.VersionMismatchCode = bizerr.Code(versionErr)
	if report.VersionMismatchCode == bizerr.CheckpointVersion {
		report.VersionMismatchRecomputed = len(engine.EvaluateAll(p, truth))
	}
	return report, nil
}

type AnomalyCase struct {
	Name            string            `json:"name"`
	Injection       string            `json:"injection"`
	Code            string            `json:"code"`
	Context         map[string]string `json:"context,omitempty"`
	SourceUnchanged bool              `json:"source_unchanged"`
	CacheUsable     bool              `json:"cache_usable"`
	Detail          string            `json:"detail,omitempty"`
}

type AnomalyReport struct {
	Cases     []AnomalyCase `json:"cases"`
	AllStable bool          `json:"all_stable"`
}

func InjectAnomalies(samplesDir string) (*AnomalyReport, error) {
	p, err := LoadProject(samplesDir, "v3")
	if err != nil {
		return nil, err
	}
	truth, err := LoadTruth(samplesDir)
	if err != nil {
		return nil, err
	}
	report := &AnomalyReport{AllStable: true}

	addDomainCase := func(name, injection string, mutate func(*domain.Project) error) error {
		source := p.CoreFingerprint()
		cp := p.Clone()
		err := mutate(cp)
		c := AnomalyCase{Name: name, Injection: injection, Code: bizerr.Code(err), Context: bizerr.Context(err)}
		c.SourceUnchanged = p.CoreFingerprint() == source
		c.CacheUsable = c.SourceUnchanged
		if c.Code == "" {
			c.Detail = "injection did not produce an error"
			report.AllStable = false
		}
		report.Cases = append(report.Cases, c)
		return nil
	}

	_ = addDomainCase("overlapping listeners", "move edge-ops under /api/", func(cp *domain.Project) error {
		cp.Listeners[1].Prefix = "/api/ops/"
		return cp.Validate()
	})
	_ = addDomainCase("missing upstream reference", "append ghost upstream id to a route", func(cp *domain.Project) error {
		cp.Listeners[0].Routes[1].UpstreamIDs = append(cp.Listeners[0].Routes[1].UpstreamIDs, "u-ghost")
		return cp.Validate()
	})
	_ = addDomainCase("out-of-range weight", "set u-blue weight above 1e6 ppm", func(cp *domain.Project) error {
		cp.Upstreams[0].WeightPPM = 2_000_000
		return cp.Validate()
	})
	_ = addDomainCase("out-of-range in-flight budget", "set edge-api in-flight limit to zero", func(cp *domain.Project) error {
		cp.Listeners[0].InflightLimit = 0
		return cp.Validate()
	})
	_ = addDomainCase("timeout inversion", "set retry tick beyond listener timeout", func(cp *domain.Project) error {
		cp.RetryTickMS = cp.Listeners[0].TimeoutMS
		return cp.Validate()
	})
	_ = addDomainCase("empty rewritten request line", "strip the entire path and add no prefix", func(cp *domain.Project) error {
		route, _, _ := cp.RouteByID("R-api-v1")
		route.Rewrite.AddPrefix = ""
		cp.Requests = append(cp.Requests, domain.Request{
			Seq: 41, ArrivalMS: 10000, Method: "GET", RawPath: "/api/v1/",
			Headers: []domain.Header{{Name: "X-Request-ID", Value: "empty"}},
		})
		rs := engine.EvaluateAll(cp, nil)
		last := rs[len(rs)-1]
		return last.Err()
	})

	eng := engine.New(p, truth)
	eng.RunAll()
	beforeCount := len(eng.Results())
	sameVersion := eng.Project()
	sameVersion.Upstreams[1].WeightPPM = 177000
	_, snapshotErr := eng.ReplaceSnapshot(sameVersion, truth)
	report.Cases = append(report.Cases, AnomalyCase{
		Name: "version-mismatched snapshot", Injection: "change content without changing project version",
		Code: bizerr.Code(snapshotErr), Context: bizerr.Context(snapshotErr),
		SourceUnchanged: true, CacheUsable: len(eng.Results()) == beforeCount,
	})

	badSchema := []byte(`{"schema":"proxy.project/v2","version":"x","clock":{"tickMs":10},"timeout_ms":200}`)
	_, schemaErr := schema.Load(badSchema)
	report.Cases = append(report.Cases, AnomalyCase{
		Name: "version-incompatible field combo", Injection: "v2 document contains v1 timeout_ms",
		Code: bizerr.Code(schemaErr), Context: bizerr.Context(schemaErr),
		SourceUnchanged: true, CacheUsable: true,
	})

	dir := tTempDir()
	defer os.RemoveAll(dir)
	journal := filepath.Join(dir, "cut.jsonl")
	store, _ := checkpoint.NewStore(journal, p.Version, 0)
	d1 := decisionFromResult(resultsFirst(engine.EvaluateAll(p, truth)))
	_ = store.Commit(1, d1, p.Upstreams)
	d2 := d1
	d2.Seq = 2
	_ = store.Commit(2, d2, p.Upstreams)
	store.Close()
	data, _ := os.ReadFile(journal)
	_ = os.WriteFile(journal, append(data, []byte(`{"type":"commit"`)...), 0o644)
	cutState, cutErr := checkpoint.Recover(journal, p.Version)
	report.Cases = append(report.Cases, AnomalyCase{
		Name: "truncated checkpoint", Injection: "partial final journal record",
		Code: bizerr.Code(cutErr), Context: bizerr.Context(cutErr),
		SourceUnchanged: true, CacheUsable: cutState.LastCommittedSeq == 2,
	})

	for _, c := range report.Cases {
		if c.Code == "" || !c.SourceUnchanged || !c.CacheUsable {
			report.AllStable = false
		}
	}
	sort.Slice(report.Cases, func(i, j int) bool { return report.Cases[i].Name < report.Cases[j].Name })
	return report, nil
}

func resultsFirst(results []engine.Result) engine.Result {
	if len(results) == 0 {
		return engine.Result{}
	}
	return results[0]
}

func tTempDir() string {
	d, err := os.MkdirTemp("", "proxydiag-inject-*")
	if err != nil {
		return "."
	}
	return d
}

func MarshalIndented(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
