package samplegen_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"soundlab/audiofp"
	"soundlab/samplegen"
)

func TestGenerateIsByteDeterministic(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	cfg := samplegen.Config{Seed: 12345, Batches: 1, AssetsPerBatch: 2}
	if _, err := samplegen.Generate(first, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := samplegen.Generate(second, cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := treeHashes(t, first), treeHashes(t, second); got != want {
		t.Fatalf("same seed produced different fixture trees\nfirst=%s\nsecond=%s", got, want)
	}
}

func treeHashes(t *testing.T, root string) string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Write([]byte(filepath.ToSlash(rel)))
		_, _ = h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func generateFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := samplegen.Generate(root, samplegen.Config{
		Seed: 77, Batches: 1, AssetsPerBatch: 2, OutputDir: root,
	}); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "batch-000", "project.json")
	return root, manifest
}

func mutateManifest(t *testing.T, path string, mutate func(map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "mutated-*.json")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if _, err := f.Write(append(out, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(name) })
	return name
}

func TestGeneratedTruthMatchesExtraction(t *testing.T) {
	_, manifest := generateFixture(t)
	project, err := audiofp.LoadProject(manifest)
	if err != nil {
		t.Fatal(err)
	}
	final, recipe, _, err := project.ApplyEdits()
	if err != nil {
		t.Fatal(err)
	}
	if err := project.ValidateTruthAgainstFinal(final, recipe); err != nil {
		t.Fatal(err)
	}
	actual, _, err := audiofp.AnalyzeAssets(context.Background(), final, recipe,
		audiofp.ExtractionOptions{Workers: 4, FrameChunk: 7, MaxSamplingBufferSamples: 2048})
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]audiofp.AssetAnalysis, 0, len(project.Manifest.Truth.Assets))
	for _, at := range project.Manifest.Truth.Assets {
		var frames []audiofp.FrameFingerprint
		for _, ft := range at.Frames {
			frames = append(frames, audiofp.TruthToFingerprint(at.AssetID, ft))
		}
		expected = append(expected, audiofp.AssetAnalysis{
			AssetID: at.AssetID, ContentHash: at.ContentHash, RecipeHash: audiofp.RecipeHash(recipe),
			TotalSamples: at.TotalSamples, Frames: frames,
			Index: audiofp.BuildIndexEntries(at.AssetID, at.ContentHash, recipe, frames),
		})
	}
	audiofp.SortAnalyses(expected)
	if err := audiofp.CompareAnalyses(expected, actual); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderValidationCodesAndFields(t *testing.T) {
	_, manifest := generateFixture(t)
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		code    string
		field   string
		assetID string
	}{
		{
			name: "missing schema field",
			mutate: func(root map[string]any) {
				delete(root["recipe"].(map[string]any), "frame_len")
			},
			code: audiofp.CodeSchema, field: "recipe.frame_len",
		},
		{
			name: "parameter contradiction",
			mutate: func(root map[string]any) {
				recipe := root["recipe"].(map[string]any)
				recipe["frame_shift"] = recipe["frame_len"].(float64) + 1
			},
			code: audiofp.CodeParameter, field: "frame_shift",
		},
		{
			name: "truncated PCM declaration",
			mutate: func(root map[string]any) {
				asset := root["assets"].([]any)[0].(map[string]any)
				asset["total_samples"] = asset["total_samples"].(float64) + 1
			},
			code: audiofp.CodeDecode, assetID: "b000-a0",
		},
		{
			name: "unknown edit target",
			mutate: func(root map[string]any) {
				edit := root["edits"].([]any)[0].(map[string]any)
				edit["asset_id"] = "missing"
			},
			code: audiofp.CodeSchema, field: "edits[0].asset_id",
		},
		{
			name: "non-monotonic logical time",
			mutate: func(root map[string]any) {
				edits := root["edits"].([]any)
				edits[1].(map[string]any)["logical_time"] = 1.0
			},
			code: audiofp.CodeOrder, field: "edits[1].logical_time",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := mutateManifest(t, manifest, tc.mutate)
			_, err := audiofp.LoadProject(path)
			if audiofp.CodeOf(err) != tc.code {
				t.Fatalf("got %v (%s), want %s", err, audiofp.CodeOf(err), tc.code)
			}
			typed, ok := audiofp.AsError(err)
			if !ok {
				t.Fatal("error is not typed")
			}
			if tc.field != "" && typed.Field != tc.field {
				t.Fatalf("field=%q, want %q", typed.Field, tc.field)
			}
			if tc.assetID != "" && typed.AssetID != tc.assetID {
				t.Fatalf("asset=%q, want %q", typed.AssetID, tc.assetID)
			}
		})
	}
}

func TestIndependentVerifierUsesPersistedFramesAndTruth(t *testing.T) {
	_, manifest := generateFixture(t)
	project, err := audiofp.LoadProject(manifest)
	if err != nil {
		t.Fatal(err)
	}
	final, recipe, _, err := project.ApplyEdits()
	if err != nil {
		t.Fatal(err)
	}
	store, err := audiofp.OpenStore(filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := audiofp.BuildCheckpoint(context.Background(), store, project.Manifest.BatchID,
		final, recipe, audiofp.SnapshotFresh, audiofp.BuildOptions{
			ShardFrames: 5, Extraction: audiofp.ExtractionOptions{Workers: 3},
		}); err != nil {
		t.Fatal(err)
	}
	report, err := audiofp.VerifyPersistedSnapshot(context.Background(), store, project.Manifest.BatchID,
		recipe, project.Manifest.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.FramesChecked == 0 || report.QueriesChecked == 0 {
		t.Fatalf("unexpected verification report: %+v", report)
	}
}
