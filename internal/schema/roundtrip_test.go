package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/generate"
	"lip-pipeline-prototype/internal/pipeline"
	"lip-pipeline-prototype/internal/schema"
)

func TestAllThreeVersionsLoadToSameTruth(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		t.Run(string(version), func(t *testing.T) {
			loaded := sample.Loaded[version]
			if loaded == nil || loaded.Project == nil {
				t.Fatal("missing loaded project")
			}
			if loaded.Project.SourceVersion != version {
				t.Fatalf("source version = %s, want %s", loaded.Project.SourceVersion, version)
			}
			for _, dlg := range loaded.Project.Dialogues {
				for _, shard := range dlg.Shards {
					got, err := pipeline.BuildShard(shard, loaded.Project.Params, dlg.ContentHash, version)
					if err != nil {
						t.Fatalf("build %s from %s: %v", shard.ID, version, err)
					}
					want := sample.TruthByShard[shard.ID]
					assertResultEquivalent(t, want, got)
				}
			}
		})
	}
}

func assertResultEquivalent(t *testing.T, want, got *domain.ShardResult) {
	t.Helper()
	if len(want.Packs) != len(got.Packs) {
		t.Fatalf("pack count %d != %d", len(got.Packs), len(want.Packs))
	}
	for i := range want.Packs {
		if strings.Join(want.Packs[i].PhonemeIDs, ",") != strings.Join(got.Packs[i].PhonemeIDs, ",") {
			t.Fatalf("pack %d phonemes %v != %v", i, got.Packs[i].PhonemeIDs, want.Packs[i].PhonemeIDs)
		}
		if want.Packs[i].StartFrame != got.Packs[i].StartFrame || want.Packs[i].EndFrame != got.Packs[i].EndFrame {
			t.Fatalf("pack %d frame range %d-%d != %d-%d", i, got.Packs[i].StartFrame, got.Packs[i].EndFrame, want.Packs[i].StartFrame, want.Packs[i].EndFrame)
		}
	}
	if len(want.Frames) != len(got.Frames) {
		t.Fatalf("frame count %d != %d", len(got.Frames), len(want.Frames))
	}
	for i := range want.Frames {
		wf, gf := want.Frames[i], got.Frames[i]
		if wf.Index != gf.Index || wf.Mouth != gf.Mouth || wf.Kind != gf.Kind || wf.PackIndex != gf.PackIndex {
			t.Fatalf("frame %d mismatch: got idx=%d mouth=%s kind=%s pack=%d; want idx=%d mouth=%s kind=%s pack=%d",
				i, gf.Index, gf.Mouth, gf.Kind, gf.PackIndex, wf.Index, wf.Mouth, wf.Kind, wf.PackIndex)
		}
	}
	if len(want.Transitions) != len(got.Transitions) {
		t.Fatalf("transition count %d != %d", len(got.Transitions), len(want.Transitions))
	}
	for i := range want.Transitions {
		if want.Transitions[i].Frame != got.Transitions[i].Frame || want.Transitions[i].Bridge != got.Transitions[i].Bridge {
			t.Fatalf("transition %d mismatch: got %+v want %+v", i, got.Transitions[i], want.Transitions[i])
		}
	}
}

func TestSchemaErrorsPointAtFieldPath(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(sample.Documents[domain.SchemaV3], &doc); err != nil {
		t.Fatal(err)
	}
	pipelineMap := doc["pipeline"].(map[string]any)
	delete(pipelineMap, "target_fps")
	data, _ := json.Marshal(doc)
	_, err = schema.LoadDocument(data)
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %T %v", err, err)
	}
	if !strings.Contains(de.Path, "target_fps") {
		t.Fatalf("field path should mention target_fps, got %q", de.Path)
	}

	_, err = schema.LoadDocument([]byte(`{"schema":"lip.project/v9"}`))
	de, ok = domain.AsError(err)
	if !ok || de.Code != domain.CodeSchema || de.Path != "$.schema" {
		t.Fatalf("unknown version should be E_SCHEMA at $.schema, got %+v", err)
	}
}

func TestDowngradeLossReportAndShadowRoundTrip(t *testing.T) {
	sample, err := generate.BuildSample()
	if err != nil {
		t.Fatal(err)
	}
	project := sample.Project
	strict, err := schema.Export(project, domain.SchemaV1, schema.ExportOptions{IncludeShadow: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(strict.Losses) == 0 {
		t.Fatal("strict v1 downgrade must report E_LOSSY items")
	}
	loadedStrict, err := schema.LoadDocument(strict.Data)
	if err != nil {
		t.Fatalf("strict v1 export must remain loadable: %v", err)
	}
	if loadedStrict.Project.SourceVersion != domain.SchemaV1 {
		t.Fatal("wrong source version")
	}
	if len(loadedStrict.Report.Fills) == 0 {
		t.Fatal("strict v1 load should record default fill records")
	}

	shadow, err := schema.Export(project, domain.SchemaV1, schema.ExportOptions{IncludeShadow: true})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := schema.LoadDocument(shadow.Data)
	if err != nil {
		t.Fatal(err)
	}
	affinities := 0
	for _, dlg := range loaded.Project.Dialogues {
		for _, shard := range dlg.Shards {
			for i, ph := range shard.Phonemes {
				if ph.EncodingGroup == "" || ph.EncodingGroup == "upgraded-flat" {
					t.Fatalf("shadow group not restored for %s: %q", ph.ID, ph.EncodingGroup)
				}
				if i < len(shard.Phonemes)-1 {
					original := findPhoneme(t, project, ph.ID)
					if ph.AffinityToNext != original.AffinityToNext {
						t.Fatalf("affinity roundtrip %s: %d != %d", ph.ID, ph.AffinityToNext, original.AffinityToNext)
					}
					affinities++
				}
			}
		}
	}
	if affinities == 0 {
		t.Fatal("expected affinity roundtrip checks")
	}
}

func findPhoneme(t *testing.T, p *domain.Project, id string) domain.Phoneme {
	t.Helper()
	for _, d := range p.Dialogues {
		for _, s := range d.Shards {
			for _, ph := range s.Phonemes {
				if ph.ID == id {
					return ph
				}
			}
		}
	}
	t.Fatalf("phoneme %s not found", id)
	return domain.Phoneme{}
}
