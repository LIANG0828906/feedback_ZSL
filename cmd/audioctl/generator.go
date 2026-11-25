package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"audiobus/domain"
	"audiobus/internal/packet"
	"audiobus/internal/pipeline"
)

func GenerateSample(out string, seed int64) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	r := rand.New(rand.NewSource(seed))
	m := domain.Manifest{
		Channels: 2, SampleRate: 48000, TotalSamples: 1920, WindowSamples: 960,
		QuantUnit: 64, QuantMode: "nearest", TimingWindowMs: 1000,
		MaxSamplesPerPack: 200, TailRule: "pad", FlagMask: 2, GainDurationK: 48000,
		TemplateCount: 2,
		Templates: [][]domain.Template{
			{{AttackPermille: 128, ReleasePermille: 128}, {AttackPermille: 0, ReleasePermille: 256}},
			{{AttackPermille: 256, ReleasePermille: 0}, {AttackPermille: 64, ReleasePermille: 64}},
		},
		BranchVersion: "generated-v1",
	}
	events := []domain.Event{}
	rawByBranch := map[string][]domain.RawEvent{"alpha": {}, "beta": {}, "gamma": {}}
	id := int64(1)
	for ch := 0; ch < m.Channels; ch++ {
		for i := 0; i < 7; i++ {
			start := int64((2 + i*4 + r.Intn(2)) * 48)
			duration := int64((2 + r.Intn(12)) * 48)
			if start+duration > int64(m.TotalSamples) {
				duration = int64(m.TotalSamples) - start - 48
			}
			if duration < 96 {
				duration = 96
			}
			gain := []int64{800, 1200, 1600, 2000, 2400}[r.Intn(5)]
			priority := 1 + r.Intn(5)
			ni := r.Intn(2)
			cd := r.Intn(2)
			fl := int64(0)
			if cd == 1 && r.Intn(2) == 1 {
				fl = 2
			}
			typeIndex := ch % m.TemplateCount
			ev := domain.Event{
				ID: id, SourceBranch: "alpha", SourceOrdinal: int(id - 1), Channel: ch,
				Start: start, Duration: duration, Gain: gain, Priority: priority,
				NoInterrupt: ni == 1, Conditional: cd == 1, Flag: fl,
				FlagTrue:     (fl & m.FlagMask) == m.FlagMask,
				TypeSequence: []int{typeIndex}, RouteSequence: []int{ch}, ArrivalOrder: int(id - 1),
				EffectiveEnd: start + duration,
			}
			events = append(events, ev)
			pr, nii, cdi := priority, ni, cd
			alpha := domain.RawEvent{ID: id, Ch: &ch, Ts: &start, Ld: &duration, Tg: &gain, Pr: &pr, Ni: &nii, Cd: &cdi, Fl: &fl, Lst: []int{typeIndex}}
			frameStart := start / 48
			frameDuration := duration / 48
			betaLd, betaTg := gain, duration
			beta := domain.RawEvent{ID: id, Ts: &frameStart, Ld: &betaLd, Tg: &betaTg, Pr: &pr, Ni: &nii, Cd: &cdi, Fl: &fl, Lst: []int{ch}}
			gammaLd := frameDuration
			gamma := domain.RawEvent{ID: id, Ch: &ch, Ts: &frameStart, Ld: &gammaLd, Tg: &gain, Pr: &pr, Ni: &nii, Cd: &cdi, Fl: &fl, Lst: []int{typeIndex}}
			rawByBranch["alpha"] = append(rawByBranch["alpha"], alpha)
			rawByBranch["beta"] = append(rawByBranch["beta"], beta)
			rawByBranch["gamma"] = append(rawByBranch["gamma"], gamma)
			id++
		}
	}

	engine := pipeline.NewEngine(m, events, nil)
	result, err := engine.RunRealtime(nil)
	if err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(out, "manifest.json"), m); err != nil {
		return err
	}
	for _, branch := range []string{"alpha", "beta", "gamma"} {
		bf := domain.BranchFile{Branch: branch, BranchVersion: m.BranchVersion, Header: domain.RawHeader{Channels: m.Channels, TotalSamples: m.TotalSamples, SampleRate: m.SampleRate}, Events: rawByBranch[branch]}
		if err := writeJSONFile(filepath.Join(out, "branch_"+branch+".json"), bf); err != nil {
			return err
		}
	}
	truthPackets := []map[string]any{}
	for _, p := range result.Report.Packets {
		truthPackets = append(truthPackets, map[string]any{
			"window": p.Window, "channel": p.Channel, "seq": p.Seq, "start": p.Start,
			"samples": p.Samples, "disposition": p.Disposition, "mark": p.Mark, "sha256": p.ContentHash,
		})
	}
	fingerprints := map[string]string{}
	framesOut := map[string][]int64{}
	for ch, frames := range result.Report.Frames {
		framesOut[fmt.Sprint(ch)] = frames
		fingerprints[fmt.Sprint(ch)] = packet.HashFrames(frames)
	}
	truth := map[string]any{"frames": framesOut, "packets": truthPackets, "channelFingerprints": fingerprints}
	return writeJSONFile(filepath.Join(out, "truth.json"), truth)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
