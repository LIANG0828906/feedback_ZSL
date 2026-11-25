package lipsync_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"lipsync/domain"
	"lipsync/generate"
	"lipsync/schedule"
	"lipsync/schema"
	"lipsync/truth"
)

func generateSamples(t *testing.T) (*generate.Report, *truth.Manifest) {
	t.Helper()
	report, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	m, err := truth.LoadFile(report.Truth)
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	return report, m
}

func TestGeneratedVersionsMatchTruth(t *testing.T) {
	report, want := generateSamples(t)
	for _, key := range []string{"v1", "v2", "v3"} {
		t.Run(key, func(t *testing.T) {
			p, gotTruth, err := schema.LoadBundle(report.Projects[key])
			if err != nil {
				t.Fatalf("load bundle: %v", err)
			}
			if gotTruth.Fingerprint != want.Fingerprint {
				t.Fatalf("truth fingerprint changed")
			}
			if p.SourceVersion != map[string]string{"v1": "dialogue/v1", "v2": "dialogue/v2", "v3": "dialogue/v3"}[key] {
				t.Fatalf("source version = %s", p.SourceVersion)
			}
			packages, err := schedule.Build(p)
			if err != nil {
				t.Fatalf("build packages: %v", err)
			}
			comparePackages(t, packages, want.Packages)
			result, err := schedule.Schedule(p, packages)
			if err != nil {
				t.Fatalf("schedule: %v", err)
			}
			compareScheduled(t, result, want)
			if p.SourceVersion == "dialogue/v1" && len(p.Defaults) == 0 {
				t.Fatalf("v1 affinity defaults were not recorded")
			}
		})
	}
}

func TestGeneratorIsByteDeterministicForFixedSeed(t *testing.T) {
	first, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{first.Truth}
	for _, key := range []string{"v1", "v2", "v3"} {
		paths = append(paths, first.Projects[key])
	}
	for id := range first.PCMS {
		paths = append(paths, first.PCMS[id])
	}
	for _, p1 := range paths {
		rel, err := filepath.Rel(first.OutDir, p1)
		if err != nil {
			t.Fatal(err)
		}
		p2 := filepath.Join(second.OutDir, rel)
		b1, err := os.ReadFile(p1)
		if err != nil {
			t.Fatal(err)
		}
		b2, err := os.ReadFile(p2)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("seed 42 artifact differs between runs: %s", rel)
		}
	}
}

func comparePackages(t *testing.T, got []domain.Package, want []truth.Package) {
	t.Helper()
	wantMap := make(map[string]truth.Package)
	for _, p := range want {
		wantMap[p.ID] = p
	}
	if len(got) != len(want) {
		t.Fatalf("package count got=%d want=%d", len(got), len(want))
	}
	for _, p := range got {
		w := wantMap[p.ID]
		if w.ID == "" {
			t.Fatalf("unexpected package %s", p.ID)
		}
		if p.StartFrame != w.StartFrame || p.EndFrame != w.EndFrame ||
			!reflect.DeepEqual(p.PhonemeIDs, w.PhonemeIDs) {
			t.Fatalf("package %s got %d..%d %v want %d..%d %v",
				p.ID, p.StartFrame, p.EndFrame, p.PhonemeIDs,
				w.StartFrame, w.EndFrame, w.PhonemeIDs)
		}
	}
}

func compareScheduled(t *testing.T, got *domain.ScheduleResult, want *truth.Manifest) {
	t.Helper()
	wantPkgs := make(map[string]truth.Package)
	for _, p := range want.Packages {
		wantPkgs[p.ID] = p
	}
	for _, p := range got.Packages {
		w := wantPkgs[p.ID]
		if p.Status != w.Status || p.DispatchStartFrame != w.DispatchStartFrame ||
			p.DispatchEndFrame != w.DispatchEndFrame {
			t.Fatalf("dispatch package %s got %s %d..%d want %s %d..%d",
				p.ID, p.Status, p.DispatchStartFrame, p.DispatchEndFrame,
				w.Status, w.DispatchStartFrame, w.DispatchEndFrame)
		}
	}
	gotFrames := append([]domain.FrameRecord(nil), got.Frames...)
	wantFrames := make([]domain.FrameRecord, len(want.Frames))
	for i := range want.Frames {
		wantFrames[i] = domain.FrameRecord{
			Frame: want.Frames[i].Frame, PackageIDs: append([]string(nil), want.Frames[i].PackageIDs...),
		}
	}
	sort.Slice(gotFrames, func(i, j int) bool { return gotFrames[i].Frame < gotFrames[j].Frame })
	sort.Slice(wantFrames, func(i, j int) bool { return wantFrames[i].Frame < wantFrames[j].Frame })
	if !reflect.DeepEqual(gotFrames, wantFrames) {
		t.Fatalf("frame dispatch mismatch\ngot=%v\nwant=%v", gotFrames, wantFrames)
	}
}
