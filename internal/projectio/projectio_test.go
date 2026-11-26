package projectio

import (
	"encoding/json"
	"reflect"
	"testing"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/sample"
)

func TestThreeSchemaRoundTripAndTraces(t *testing.T) {
	p := sample.NewCanonicalProject()
	wantHash := p.CanonicalHash()
	for _, ver := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		exp, err := Export(p, ver)
		if err != nil {
			t.Fatalf("export %s: %v", ver, err)
		}
		loaded, err := Load(exp.Data)
		if err != nil {
			t.Fatalf("load %s: %v", ver, err)
		}
		if got := loaded.Project.CanonicalHash(); got != wantHash {
			t.Fatalf("%s canonical hash mismatch\nwant=%s\ngot =%s", ver, wantHash, got)
		}
		if loaded.Project.SourceSchema != ver {
			t.Fatalf("source version not retained: %s", loaded.Project.SourceSchema)
		}
	}
}

func TestDecodeUnknownVersionAndMissingField(t *testing.T) {
	p := sample.NewCanonicalProject()
	exp, err := Export(p, domain.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(exp.Data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "gameart-ms/v9"
	b, _ := json.Marshal(doc)
	_, err = Load(b)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Decode || ce.Version != "gameart-ms/v9" {
		t.Fatalf("expected unknown E_DECODE with version, got %#v", err)
	}

	exp, _ = Export(p, domain.SchemaV1)
	_ = json.Unmarshal(exp.Data, &doc)
	delete(doc, "seed")
	b, _ = json.Marshal(doc)
	_, err = Load(b)
	ce, ok = e.As(err)
	if !ok || ce.Code != e.Decode || ce.Path != "$.seed" {
		t.Fatalf("expected missing $.seed E_DECODE, got %#v", err)
	}

	exp, _ = Export(p, domain.SchemaV1)
	_ = json.Unmarshal(exp.Data, &doc)
	geom := doc["geometry"].(map[string]any)
	delete(geom, "width")
	doc["geometry"] = geom
	b, _ = json.Marshal(doc)
	_, err = Load(b)
	ce, ok = e.As(err)
	if !ok || ce.Code != e.Decode || ce.Path != "$.geometry.width" {
		t.Fatalf("expected missing geometry.width E_DECODE path, got %#v", err)
	}
}

func TestV1LossReportStableAfterImportExport(t *testing.T) {
	p := sample.NewCanonicalProject()
	first, err := Export(p, domain.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if first.LossReport == nil || first.LossReport.Code != string(e.Lossy) || len(first.LossReport.Items) == 0 {
		t.Fatalf("expected E_LOSSY item list, got %#v", first.LossReport)
	}
	loaded, err := Load(first.Data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Export(loaded.Project, domain.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.LossReport.Items, second.LossReport.Items) {
		t.Fatalf("loss items differ after round trip\nfirst=%#v\nsecond=%#v", first.LossReport.Items, second.LossReport.Items)
	}
	if first.LossReport.Digest != second.LossReport.Digest {
		t.Fatalf("loss digest differs: %s != %s", first.LossReport.Digest, second.LossReport.Digest)
	}
	reloaded, err := Load(second.Data)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Project.CanonicalHash() != p.CanonicalHash() {
		t.Fatal("re-exported v1 does not restore equivalent internal semantics")
	}
}
