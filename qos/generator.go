package qos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type xorshift64 struct {
	state uint64
}

func newXorShift(seed int64) *xorshift64 {
	state := uint64(seed) ^ 0x9e3779b97f4a7c15
	if state == 0 {
		state = 0xd1b54a32d192ed03
	}
	return &xorshift64{state: state}
}

func (r *xorshift64) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *xorshift64) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func Scenario(seed int64) *Project {
	rng := newXorShift(seed)
	project := &Project{
		Name: "edge-qos-deterministic", Version: fmt.Sprintf("gen-%d-r1", seed),
		TickMS: 10, NominalPacketBytes: 128, EgressBudgetBytes: 512,
		Width: 64, WorkerThreads: 4, QueueDepth: 128, HorizonMS: 800,
		Channels: []Channel{
			{
				ID: "gold", Order: 0, RatePermyriad: 60000,
				CapacityBytes: 1024, InitialTokens: 512, Policy: PolicyDefer,
				Priority: 2, WeightPermyriad: 6000, QueueAgeMS: 20, MaxDelayMS: 300,
			},
			{
				ID: "silver", Order: 1, RatePermyriad: 30000,
				CapacityBytes: 768, InitialTokens: 384, Policy: PolicyDefer,
				Priority: 2, WeightPermyriad: 3000, QueueAgeMS: 20, MaxDelayMS: 300,
			},
			{
				ID: "bronze", Order: 2, RatePermyriad: 10000,
				CapacityBytes: 512, InitialTokens: 256, Policy: PolicyDrop,
				Priority: 1, WeightPermyriad: 1000, QueueAgeMS: 20, MaxDelayMS: 300,
			},
		},
	}
	fixed := []struct {
		channel string
		at      int64
		size    int64
		prio    int
		dscp    *int
	}{
		{channel: "gold", at: 0, size: 320, prio: 1, dscp: intPtr(46)},
		{channel: "silver", at: 0, size: 416, prio: 2, dscp: intPtr(10)},
		{channel: "bronze", at: 0, size: 480, prio: 0},
		{channel: "gold", at: 10, size: 480, prio: 1},
		{channel: "silver", at: 20, size: 448, prio: 1},
	}
	for i, f := range fixed {
		project.Packets = append(project.Packets, Packet{
			ID: fmt.Sprintf("p%03d", i+1), ChannelID: f.channel, ArrivalMS: f.at,
			SizeBytes: f.size, Priority: f.prio, DSCP: f.dscp, Seq: i,
		})
	}
	dcsps := []int{0, 10, 46}
	channels := []string{"gold", "silver", "bronze"}
	for i := 0; i < 24; i++ {
		seq := len(project.Packets)
		at := int64(rng.intn(11) * 10)
		size := int64(96 + 32*rng.intn(13))
		priority := rng.intn(3)
		var dscp *int
		if rng.intn(3) == 0 {
			dscp = intPtr(dcsps[rng.intn(len(dcsps))])
		}
		project.Packets = append(project.Packets, Packet{
			ID: fmt.Sprintf("p%03d", seq+1), ChannelID: channels[rng.intn(len(channels))],
			ArrivalMS: at, SizeBytes: size, Priority: priority,
			DSCP: dscp, Seq: seq,
		})
	}
	return project
}

func intPtr(v int) *int { return &v }

type SampleManifest struct {
	Seed               int64            `json:"seed"`
	ProjectVersion     string           `json:"project_version"`
	ProjectDigest      string           `json:"project_digest"`
	Files              []string         `json:"files"`
	Steps              int              `json:"steps"`
	Emitted            int              `json:"emitted"`
	DelayedEvents      int              `json:"delayed_events"`
	Dropped            int              `json:"dropped"`
	Expired            int              `json:"expired"`
	FinalTokens        map[string]int64 `json:"final_tokens"`
	VersionsNormalized bool             `json:"versions_normalized"`
}

func GenerateSample(ctx context.Context, seed int64, outDir string) (*SampleManifest, error) {
	project := Scenario(seed)
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, wrapError(EInternal, "generate", err, map[string]any{"dir": outDir})
	}
	eng, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	result, err := eng.Run(ctx)
	if err != nil {
		return nil, err
	}
	digest, err := project.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	manifest := &SampleManifest{
		Seed: seed, ProjectVersion: project.Version, ProjectDigest: digest,
		Steps: result.TotalSteps, Emitted: len(result.Emissions),
		DelayedEvents: len(result.Delays), Dropped: len(result.Drops),
		Expired: len(result.Expired), FinalTokens: result.FinalTokens,
		VersionsNormalized: true,
	}
	for version := 1; version <= 3; version++ {
		raw, err := EncodeProject(project, version)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("sample_v%d.json", version)
		if err := writeJSONFile(filepath.Join(outDir, name), raw); err != nil {
			return nil, err
		}
		manifest.Files = append(manifest.Files, name)
		pv := CloneProject(project)
		pv.SchemaVersion = version
		truth, err := EncodeTruth(pv, result)
		if err != nil {
			return nil, err
		}
		tn := fmt.Sprintf("truth_v%d.json", version)
		if err := writeJSONFile(filepath.Join(outDir, tn), truth); err != nil {
			return nil, err
		}
		manifest.Files = append(manifest.Files, tn)
	}
	stream := map[string]any{
		"project_version": project.Version,
		"tick_ms":         project.TickMS,
		"packets":         project.Packets,
	}
	streamRaw, err := json.MarshalIndent(stream, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "generate.stream", err, nil)
	}
	if err := writeJSONFile(filepath.Join(outDir, "stream.json"), append(streamRaw, '\n')); err != nil {
		return nil, err
	}
	manifest.Files = append(manifest.Files, "stream.json")
	critical, err := CriticalCases()
	if err != nil {
		return nil, err
	}
	critRaw, err := json.MarshalIndent(critical, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "generate.critical", err, nil)
	}
	if err := writeJSONFile(filepath.Join(outDir, "critical_cases.json"), append(critRaw, '\n')); err != nil {
		return nil, err
	}
	manifest.Files = append(manifest.Files, "critical_cases.json")
	manRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "generate.manifest", err, nil)
	}
	if err := writeJSONFile(filepath.Join(outDir, "manifest.json"), append(manRaw, '\n')); err != nil {
		return nil, err
	}
	return manifest, nil
}

func writeJSONFile(path string, raw []byte) error {
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return wrapError(EInternal, "generate.write", err, map[string]any{"path": path})
	}
	return nil
}

type CriticalCase struct {
	Name            string         `json:"name"`
	Inputs          map[string]any `json:"inputs"`
	Computed        map[string]any `json:"computed"`
	WithinTolerance bool           `json:"within_tolerance"`
	Pass            bool           `json:"pass"`
	ErrorCode       Code           `json:"error_code,omitempty"`
}

type CriticalReport struct {
	GeneratedAt string         `json:"generated_at"`
	Cases       []CriticalCase `json:"cases"`
	AllPass     bool           `json:"all_pass"`
}

func CriticalCases() (*CriticalReport, error) {
	report := &CriticalReport{GeneratedAt: "deterministic", AllPass: true}
	r1, err := ConvertRateToPermyriad(2500, 1000, 64)
	if err != nil {
		return nil, err
	}
	report.Cases = append(report.Cases, CriticalCase{
		Name:            "rate_permille_to_permyriad",
		Inputs:          map[string]any{"value": 2500, "scale": 1000},
		Computed:        map[string]any{"rate_permyriad": r1, "rate_bytes_per_ms_milli": r1 * 1000 / 10000},
		WithinTolerance: r1 == 25000, Pass: r1 == 25000,
	})
	r2, err := ConvertRateToPermyriad(25000, 10000, 64)
	if err != nil {
		return nil, err
	}
	report.Cases = append(report.Cases, CriticalCase{
		Name:            "rate_permyriad_identity",
		Inputs:          map[string]any{"value": 25000, "scale": 10000},
		Computed:        map[string]any{"rate_permyriad": r2},
		WithinTolerance: r2 == r1, Pass: r2 == r1,
	})
	capBytes, err := ConvertPacketsToBytes(8, 128, 64)
	if err != nil {
		return nil, err
	}
	report.Cases = append(report.Cases, CriticalCase{
		Name:            "packet_capacity_to_bytes",
		Inputs:          map[string]any{"packets": 8, "nominal_packet_bytes": 128},
		Computed:        map[string]any{"capacity_bytes": capBytes},
		WithinTolerance: capBytes == 1024, Pass: capBytes == 1024,
	})
	delay, exact, errMS, check, err := DelayForDeficit(86, 30000, 20, 64)
	if err != nil {
		return nil, err
	}
	report.Cases = append(report.Cases, CriticalCase{
		Name:   "delay_quantization",
		Inputs: map[string]any{"deficit_bytes": 86, "rate_permyriad": 30000, "quantum_ms": 20},
		Computed: map[string]any{
			"delay_ms": delay, "exact_ms": exact, "quantization_error_ms": errMS,
			"tolerance_ms": check.Tolerance,
		},
		WithinTolerance: check.WithinTol, Pass: check.WithinTol,
	})
	refill, err := Refill(100, 0, 30000, 10, 512, 64)
	if err != nil {
		return nil, err
	}
	combinedDelay, _, _, combinedCheck, err := DelayForDeficit(216-refill.Tokens, 30000, 20, 64)
	if err != nil {
		return nil, err
	}
	report.Cases = append(report.Cases, CriticalCase{
		Name: "refill_watermark_and_delay",
		Inputs: map[string]any{
			"tokens_before": 100, "rate_permyriad": 30000, "elapsed_ms": 10,
			"capacity_bytes": 512, "packet_bytes": 216, "quantum_ms": 20,
		},
		Computed: map[string]any{
			"refill_added_bytes": refill.Added,
			"refill_numerator":   refill.Numerator,
			"refill_remainder":   refill.Remainder,
			"tokens_bytes":       refill.Tokens,
			"deficit_bytes":      216 - refill.Tokens,
			"delay_ms":           combinedDelay,
			"tolerance_ms":       combinedCheck.Tolerance,
		},
		WithinTolerance: combinedCheck.WithinTol,
		Pass:            combinedCheck.WithinTol && refill.Tokens >= 0 && refill.Tokens <= 512,
	})
	_, err = ConvertRateToPermyriad(1<<31-1, 1000, 32)
	overflowCase := CriticalCase{
		Name:            "declared_width_overflow",
		Inputs:          map[string]any{"value": 1<<31 - 1, "scale": 1000, "width": 32},
		Computed:        map[string]any{"detected": err != nil},
		WithinTolerance: err != nil, Pass: err != nil && CodeOf(err) == ERateOverflow,
	}
	if err != nil {
		overflowCase.ErrorCode = CodeOf(err)
	}
	report.Cases = append(report.Cases, overflowCase)

	tokens, remainder := int64(0), int64(0)
	for i := 0; i < 10000; i++ {
		refill, err := Refill(tokens, remainder, 25000, 1, 1<<40, 64)
		if err != nil {
			return nil, err
		}
		tokens, remainder = refill.Tokens, refill.Remainder
	}
	longOK := tokens == 25000 && remainder == 0
	report.Cases = append(report.Cases, CriticalCase{
		Name:            "long_run_exact_refill",
		Inputs:          map[string]any{"rate_permyriad": 25000, "elapsed_ms_each": 1, "iterations": 10000},
		Computed:        map[string]any{"tokens_bytes": tokens, "remainder": remainder},
		WithinTolerance: longOK, Pass: longOK,
	})
	for _, c := range report.Cases {
		if !c.Pass {
			report.AllPass = false
		}
	}
	return report, nil
}
